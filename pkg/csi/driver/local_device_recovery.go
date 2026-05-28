package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const localDeviceStateConfigMapName = "opennebula-csi-node-device-state"

const (
	localDeviceFailureClassMissingDevice = "device_missing"
	localDeviceFailureClassMountFailed   = "mount_failed"
)

type LocalDeviceMissingReport struct {
	Node                  string     `json:"node"`
	VolumeID              string     `json:"volumeID"`
	VolumeName            string     `json:"volumeName,omitempty"`
	FailureClass          string     `json:"failureClass,omitempty"`
	DevicePath            string     `json:"devicePath,omitempty"`
	DeviceSerial          string     `json:"deviceSerial,omitempty"`
	OpenNebulaImageID     string     `json:"openNebulaImageID,omitempty"`
	FsType                string     `json:"fsType,omitempty"`
	ResolvedBy            string     `json:"resolvedBy,omitempty"`
	PVCNamespace          string     `json:"pvcNamespace,omitempty"`
	PVCName               string     `json:"pvcName,omitempty"`
	PVName                string     `json:"pvName,omitempty"`
	StagingTargetPath     string     `json:"stagingTargetPath,omitempty"`
	FirstObservedAt       time.Time  `json:"firstObservedAt"`
	LastObservedAt        time.Time  `json:"lastObservedAt"`
	Attempts              int        `json:"attempts"`
	LastError             string     `json:"lastError,omitempty"`
	RecoveryAttempts      int        `json:"recoveryAttempts,omitempty"`
	LastRecoveryAt        *time.Time `json:"lastRecoveryAt,omitempty"`
	LastRecoveryError     string     `json:"lastRecoveryError,omitempty"`
	LastRecoveryOutcome   string     `json:"lastRecoveryOutcome,omitempty"`
	LastRecoverySignature string     `json:"lastRecoverySignature,omitempty"`
}

type localDeviceReportIdentity struct {
	PVCNamespace string
	PVCName      string
	PVName       string
}

func (ns *NodeServer) recordLocalDeviceMissing(ctx context.Context, volumeID, volumeName, stagingTargetPath string, publishContext map[string]string, cause error) {
	if ns == nil || ns.Driver == nil || ns.Driver.kubeRuntime == nil || !ns.Driver.kubeRuntime.enabled {
		return
	}
	if enabled, ok := ns.Driver.PluginConfig.GetBool(config.LocalDeviceRecoveryEnabledVar); ok && !enabled {
		return
	}
	volumeID = strings.TrimSpace(volumeID)
	node := strings.TrimSpace(ns.Driver.nodeID)
	if volumeID == "" || node == "" || opennebula.IsSharedFilesystemVolumeID(volumeID) {
		return
	}
	if backend := strings.TrimSpace(publishContext[annotationBackend]); backend != "" && !strings.EqualFold(backend, "local") {
		return
	}

	now := time.Now().UTC()
	key := localDeviceReportKey(node, volumeID)
	namespace := namespaceFromServiceAccount()
	identity := ns.localDeviceReportIdentity(ctx, volumeID, publishContext)
	var attempts int
	err := updateLocalDeviceReport(ctx, ns.Driver.kubeRuntime, namespace, key, func(report *LocalDeviceMissingReport) {
		if report.FirstObservedAt.IsZero() {
			report.FirstObservedAt = now
		}
		report.Node = node
		report.VolumeID = volumeID
		report.VolumeName = strings.TrimSpace(volumeName)
		report.FailureClass = localDeviceFailureClassMissingDevice
		report.DevicePath = ""
		report.DeviceSerial = strings.TrimSpace(publishContext[publishContextDeviceSerial])
		report.OpenNebulaImageID = strings.TrimSpace(publishContext[publishContextOpenNebulaImageID])
		report.FsType = ""
		report.ResolvedBy = ""
		report.PVCNamespace = identity.PVCNamespace
		report.PVCName = identity.PVCName
		report.PVName = identity.PVName
		report.StagingTargetPath = strings.TrimSpace(stagingTargetPath)
		report.LastObservedAt = now
		report.Attempts++
		attempts = report.Attempts
		if cause != nil {
			report.LastError = cause.Error()
		}
	})
	if err != nil {
		klog.V(2).InfoS("Failed to persist local device missing report",
			"node", node, "volumeID", volumeID, "configMap", localDeviceStateConfigMapName, "err", err)
		return
	}
	if ns.Driver.metrics != nil {
		ns.Driver.metrics.RecordLocalVolumeHealth("device_missing", "reported")
	}
	minAttempts := 3
	if configured, ok := ns.Driver.PluginConfig.GetInt(config.LocalDeviceRecoveryMinAttemptsVar); ok && configured > 0 {
		minAttempts = configured
	}
	if attempts == 1 || attempts%minAttempts == 0 {
		message := fmt.Sprintf("node %s reported missing local device for volume %s after %d attempt(s): %v", node, volumeID, attempts, cause)
		ns.recordPVCWarningFromPublishContext(ctx, publishContext, eventReasonLocalDeviceMissingReported, message)
	}
}

func (ns *NodeServer) recordLocalDeviceMountFailure(ctx context.Context, volumeID, volumeName, devicePath, stagingTargetPath, fsType string, resolution deviceResolutionResult, publishContext map[string]string, cause error) {
	if ns == nil || ns.Driver == nil || ns.Driver.kubeRuntime == nil || !ns.Driver.kubeRuntime.enabled {
		return
	}
	if enabled, ok := ns.Driver.PluginConfig.GetBool(config.LocalDeviceRecoveryEnabledVar); ok && !enabled {
		return
	}
	volumeID = strings.TrimSpace(volumeID)
	node := strings.TrimSpace(ns.Driver.nodeID)
	if volumeID == "" || node == "" || opennebula.IsSharedFilesystemVolumeID(volumeID) {
		return
	}
	if backend := strings.TrimSpace(publishContext[annotationBackend]); backend != "" && !strings.EqualFold(backend, "local") {
		return
	}

	now := time.Now().UTC()
	key := localDeviceReportKey(node, volumeID)
	namespace := namespaceFromServiceAccount()
	identity := ns.localDeviceReportIdentity(ctx, volumeID, publishContext)
	var attempts int
	err := updateLocalDeviceReport(ctx, ns.Driver.kubeRuntime, namespace, key, func(report *LocalDeviceMissingReport) {
		if report.FirstObservedAt.IsZero() {
			report.FirstObservedAt = now
		}
		report.Node = node
		report.VolumeID = volumeID
		report.VolumeName = strings.TrimSpace(volumeName)
		report.FailureClass = localDeviceFailureClassMountFailed
		report.DevicePath = strings.TrimSpace(devicePath)
		report.DeviceSerial = strings.TrimSpace(publishContext[publishContextDeviceSerial])
		report.OpenNebulaImageID = strings.TrimSpace(publishContext[publishContextOpenNebulaImageID])
		report.FsType = strings.TrimSpace(fsType)
		report.ResolvedBy = strings.TrimSpace(resolution.ResolvedBy)
		report.PVCNamespace = identity.PVCNamespace
		report.PVCName = identity.PVCName
		report.PVName = identity.PVName
		report.StagingTargetPath = strings.TrimSpace(stagingTargetPath)
		report.LastObservedAt = now
		report.Attempts++
		attempts = report.Attempts
		if cause != nil {
			report.LastError = cause.Error()
		}
	})
	if err != nil {
		klog.V(2).InfoS("Failed to persist local device mount failure report",
			"node", node, "volumeID", volumeID, "devicePath", devicePath, "configMap", localDeviceStateConfigMapName, "err", err)
		return
	}
	if ns.Driver.metrics != nil {
		ns.Driver.metrics.RecordLocalVolumeHealth("device_mount_failed", "reported")
	}
	minAttempts := 3
	if configured, ok := ns.Driver.PluginConfig.GetInt(config.LocalDeviceRecoveryMinAttemptsVar); ok && configured > 0 {
		minAttempts = configured
	}
	if attempts == 1 || attempts%minAttempts == 0 {
		message := fmt.Sprintf("node %s failed to mount local device %s for volume %s after %d attempt(s) (fsType=%s resolvedBy=%s): %v", node, devicePath, volumeID, attempts, fsType, resolution.ResolvedBy, cause)
		ns.recordPVCWarningFromPublishContext(ctx, publishContext, eventReasonLocalDeviceMountFailed, message)
	}
}

func (ns *NodeServer) clearLocalDeviceMissing(ctx context.Context, volumeID string) {
	if ns == nil || ns.Driver == nil || ns.Driver.kubeRuntime == nil || !ns.Driver.kubeRuntime.enabled {
		return
	}
	node := strings.TrimSpace(ns.Driver.nodeID)
	volumeID = strings.TrimSpace(volumeID)
	if node == "" || volumeID == "" {
		return
	}
	if err := ns.Driver.kubeRuntime.DeleteConfigMapKey(ctx, namespaceFromServiceAccount(), localDeviceStateConfigMapName, localDeviceReportKey(node, volumeID)); err != nil {
		klog.V(3).InfoS("Failed to clear local device missing report", "node", node, "volumeID", volumeID, "err", err)
	}
}

func (ns *NodeServer) localDeviceReportIdentity(ctx context.Context, volumeID string, publishContext map[string]string) localDeviceReportIdentity {
	identity := localDeviceReportIdentity{
		PVCNamespace: strings.TrimSpace(publishContext[paramPVCNamespace]),
		PVCName:      strings.TrimSpace(publishContext[paramPVCName]),
		PVName:       strings.TrimSpace(publishContext[paramPVName]),
	}
	if identity.PVCNamespace != "" && identity.PVCName != "" && identity.PVName != "" {
		return identity
	}
	if ns == nil || ns.Driver == nil || ns.Driver.kubeRuntime == nil || !ns.Driver.kubeRuntime.enabled {
		return identity
	}
	runtimeCtx, err := ns.Driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, volumeID)
	if err != nil || runtimeCtx == nil {
		return identity
	}
	if identity.PVCNamespace == "" {
		identity.PVCNamespace = strings.TrimSpace(runtimeCtx.PVCNamespace)
	}
	if identity.PVCName == "" {
		identity.PVCName = strings.TrimSpace(runtimeCtx.PVCName)
	}
	if identity.PVName == "" {
		identity.PVName = strings.TrimSpace(runtimeCtx.PVName)
	}
	return identity
}

func (s *ControllerServer) runLocalDeviceRecovery(ctx context.Context) {
	interval := s.localDeviceRecoveryInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.reconcileLocalDeviceRecovery(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileLocalDeviceRecovery(ctx)
		}
	}
}

func (s *ControllerServer) reconcileLocalDeviceRecovery(ctx context.Context) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled || s.volumeProvider == nil {
		return
	}
	if s.driver.controllerLeadership != nil && !s.driver.controllerLeadership.IsLeader() {
		return
	}
	cm, err := s.driver.kubeRuntime.GetConfigMap(ctx, namespaceFromServiceAccount(), localDeviceStateConfigMapName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.V(2).InfoS("Failed to read local device recovery reports", "configMap", localDeviceStateConfigMapName, "err", err)
		}
		return
	}
	if len(cm.Data) == 0 {
		return
	}

	keys := make([]string, 0, len(cm.Data))
	for key := range cm.Data {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	now := time.Now().UTC()
	for _, key := range keys {
		raw := strings.TrimSpace(cm.Data[key])
		if raw == "" {
			continue
		}
		var report LocalDeviceMissingReport
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			klog.V(2).InfoS("Skipping malformed local device recovery report", "key", key, "err", err)
			continue
		}
		if ready, reason := s.localDeviceReportReady(report, now); !ready {
			klog.V(5).InfoS("Local device recovery report is not ready", "key", key, "reason", reason)
			continue
		}
		if err := s.recoverLocalDeviceReport(ctx, key, report); err != nil {
			klog.V(2).InfoS("Local device recovery failed", "key", key, "node", report.Node, "volumeID", report.VolumeID, "err", err)
		}
	}
}

func (s *ControllerServer) recoverLocalDeviceReport(ctx context.Context, key string, report LocalDeviceMissingReport) error {
	node := strings.TrimSpace(report.Node)
	volumeID := strings.TrimSpace(report.VolumeID)
	if node == "" || volumeID == "" {
		_ = s.clearLocalDeviceReport(ctx, key)
		return nil
	}

	runtimeCtx, err := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, volumeID)
	if err != nil {
		if strings.Contains(err.Error(), "persistent volume for handle") {
			return s.clearLocalDeviceReportWithEvent(ctx, key, report, nil, "persistent volume no longer exists")
		}
		return s.markLocalDeviceRecoverySkipped(ctx, key, report, "runtime_context_unavailable", err)
	}
	if !runtimeContextEligibleForMaintenance(runtimeCtx, volumeID) {
		return s.clearLocalDeviceReportWithEvent(ctx, key, report, runtimeCtx, "volume is not an eligible local single-writer disk")
	}
	desired, desiredReason, err := s.driver.kubeRuntime.VolumeDesiredOnNode(ctx, volumeID, node)
	if err != nil {
		return s.markLocalDeviceRecoverySkipped(ctx, key, report, "desired_state_unavailable", err)
	}
	if !desired {
		return s.clearLocalDeviceReportWithEvent(ctx, key, report, runtimeCtx, fmt.Sprintf("volume is no longer desired on node %s", node))
	}
	kubeReady, err := s.driver.kubeRuntime.IsNodeReady(ctx, node)
	if err != nil {
		return s.markLocalDeviceRecoverySkipped(ctx, key, report, "kubernetes_node_unavailable", err)
	}
	if !kubeReady {
		return s.markLocalDeviceRecoverySkipped(ctx, key, report, "kubernetes_node_not_ready", nil)
	}
	vmReady, err := s.volumeProvider.NodeReady(ctx, node)
	if err != nil {
		return s.markLocalDeviceRecoverySkipped(ctx, key, report, "opennebula_node_unavailable", err)
	}
	if !vmReady {
		return s.markLocalDeviceRecoverySkipped(ctx, key, report, "opennebula_node_not_ready", nil)
	}
	volumeNumericID, _, err := s.volumeProvider.VolumeExists(ctx, volumeID)
	if err != nil {
		return s.markLocalDeviceRecoverySkipped(ctx, key, report, "volume_lookup_failed", err)
	}
	if volumeNumericID == -1 {
		return s.clearLocalDeviceReportWithEvent(ctx, key, report, runtimeCtx, "OpenNebula volume no longer exists")
	}
	nodeNumericID, err := s.volumeProvider.NodeExists(ctx, node)
	if err != nil {
		return s.markLocalDeviceRecoverySkipped(ctx, key, report, "node_lookup_failed", err)
	}
	if nodeNumericID == -1 {
		return s.clearLocalDeviceReportWithEvent(ctx, key, report, runtimeCtx, "OpenNebula node no longer exists")
	}

	message := fmt.Sprintf("attempting same-node device recovery for local RWO volume %s on node %s after %d missing-device report(s) (%s)", volumeID, node, report.Attempts, desiredReason)
	if localDeviceFailureClass(report) == localDeviceFailureClassMountFailed {
		message = fmt.Sprintf("attempting same-node device recovery for local RWO volume %s on node %s after %d mount failure report(s) using %s (%s)", volumeID, node, report.Attempts, strings.TrimSpace(report.DevicePath), desiredReason)
	}
	s.recordLocalDeviceRecoveryEvent(ctx, report, runtimeCtx, eventReasonLocalDeviceRecoveryAttempted, message)
	if s.driver.metrics != nil {
		s.driver.metrics.RecordLocalVolumeHealth("device_recovery", "attempted")
	}

	if err := s.executeLocalDeviceRecovery(ctx, report, runtimeCtx, volumeNumericID, nodeNumericID); err != nil {
		if s.driver.metrics != nil {
			s.driver.metrics.RecordLocalVolumeHealth("device_recovery", "failed")
		}
		s.recordLocalDeviceRecoveryEvent(ctx, report, runtimeCtx, eventReasonLocalDeviceRecoveryFailed, fmt.Sprintf("same-node device recovery for volume %s on node %s failed: %v", volumeID, node, err))
		return s.updateLocalDeviceRecoveryFailure(ctx, key, report, err)
	}

	if s.driver.metrics != nil {
		s.driver.metrics.RecordLocalVolumeHealth("device_recovery", "succeeded")
	}
	s.recordLocalDeviceRecoveryEvent(ctx, report, runtimeCtx, eventReasonLocalDeviceRecoverySucceeded, fmt.Sprintf("same-node device recovery for volume %s on node %s completed; node staging can retry device discovery", volumeID, node))
	return s.clearLocalDeviceReport(ctx, key)
}

func (s *ControllerServer) executeLocalDeviceRecovery(ctx context.Context, report LocalDeviceMissingReport, runtimeCtx *VolumeRuntimeContext, volumeNumericID, nodeNumericID int) error {
	node := strings.TrimSpace(report.Node)
	volumeID := strings.TrimSpace(report.VolumeID)
	params := localDeviceRecoveryParams(report, runtimeCtx)
	return s.withQueuedHotplug(ctx, node, "reattach", volumeID, hotplugQueuePriorityCritical, func(queueCtx context.Context) error {
		nodeRelease := s.driver.operationLocks.Acquire(controllerNodeLockKey(node))
		defer nodeRelease()

		volumeRelease := s.driver.operationLocks.Acquire(controllerVolumeLockKey(volumeID))
		defer volumeRelease()

		if desired, reason, err := s.driver.kubeRuntime.VolumeDesiredOnNode(queueCtx, volumeID, node); err != nil {
			return fmt.Errorf("failed to confirm same-node desired state: %w", err)
		} else if !desired {
			return fmt.Errorf("volume is no longer desired on node %s", node)
		} else {
			klog.V(3).InfoS("Confirmed local device recovery desired state", "volumeID", volumeID, "node", node, "reason", reason)
		}

		target, attachedErr := s.volumeProvider.GetVolumeInNode(queueCtx, volumeNumericID, nodeNumericID)
		sizeBytes, sizeErr := s.volumeProvider.ResolveVolumeSizeBytes(queueCtx, volumeID)
		if attachedErr == nil {
			detachStarted := time.Now()
			if err := s.volumeProvider.DetachVolume(queueCtx, volumeID, node); err != nil {
				s.handleHotplugTimeout(queueCtx, node, volumeID, params, "detach", "disk", err)
				return fmt.Errorf("failed to detach existing same-node attachment target %s: %w", target, err)
			}
			if sizeErr == nil {
				s.driver.observeAdaptiveTimeout(queueCtx, "detach", "disk", sizeBytes, time.Since(detachStarted))
			}
		} else {
			klog.V(2).InfoS("Local device recovery found no existing OpenNebula attachment; attaching directly",
				"volumeID", volumeID, "node", node, "err", attachedErr)
			if s.driver.metrics != nil {
				s.driver.metrics.RecordLocalVolumeHealth("device_recovery", "attachment_absent")
			}
		}

		attachStarted := time.Now()
		if err := s.volumeProvider.AttachVolume(queueCtx, volumeID, node, false, params); err != nil {
			s.handleHotplugTimeout(queueCtx, node, volumeID, params, "attach", "disk", err)
			return fmt.Errorf("failed to attach volume back to same node: %w", err)
		}
		target, err := s.volumeProvider.GetVolumeInNode(queueCtx, volumeNumericID, nodeNumericID)
		if err != nil {
			return fmt.Errorf("failed to confirm recovered same-node attachment: %w", err)
		}
		if sizeErr == nil {
			s.driver.observeAdaptiveTimeout(queueCtx, "attach", "disk", sizeBytes, time.Since(attachStarted))
		}
		s.clearHotplugGuardState(queueCtx, node)
		s.clearStickyReuseState(queueCtx, volumeID, node, "disk")
		s.annotateRestartOptimization(queueCtx, runtimeCtx, node, s.effectiveDetachGraceSeconds(runtimeCtx))
		klog.V(1).InfoS("Local device recovery reattached volume",
			"volumeID", volumeID, "node", node, "target", target)
		return nil
	})
}

func (s *ControllerServer) localDeviceReportReady(report LocalDeviceMissingReport, now time.Time) (bool, string) {
	if strings.TrimSpace(report.Node) == "" || strings.TrimSpace(report.VolumeID) == "" {
		return false, "missing_identity"
	}
	if report.Attempts < s.localDeviceRecoveryMinAttempts() {
		return false, "min_attempts"
	}
	firstObserved := report.FirstObservedAt
	if firstObserved.IsZero() {
		firstObserved = report.LastObservedAt
	}
	if !firstObserved.IsZero() && now.Sub(firstObserved) < s.localDeviceRecoveryMinAge() {
		return false, "min_age"
	}
	if report.LastRecoveryAt != nil && s.localDeviceRecoveryCooldown() > 0 && now.Sub(*report.LastRecoveryAt) < s.localDeviceRecoveryCooldown() {
		return false, "cooldown"
	}
	if maxAttempts := s.localDeviceRecoveryMaxAttempts(); maxAttempts > 0 && report.RecoveryAttempts >= maxAttempts {
		if report.LastRecoveryAt == nil || !report.LastObservedAt.After(*report.LastRecoveryAt) {
			return false, "max_recovery_attempts"
		}
		currentSignature := localDeviceRecoverySignature(report)
		if strings.TrimSpace(report.LastRecoverySignature) == "" || currentSignature == report.LastRecoverySignature {
			return false, "max_recovery_attempts_same_failure"
		}
	}
	return true, ""
}

func (s *ControllerServer) updateLocalDeviceRecoveryFailure(ctx context.Context, key string, report LocalDeviceMissingReport, cause error) error {
	now := time.Now().UTC()
	message := "unknown error"
	if cause != nil {
		message = cause.Error()
	}
	return updateLocalDeviceReport(ctx, s.driver.kubeRuntime, namespaceFromServiceAccount(), key, func(current *LocalDeviceMissingReport) {
		if strings.TrimSpace(current.Node) == "" {
			*current = report
		}
		if current.FirstObservedAt.IsZero() {
			current.FirstObservedAt = report.FirstObservedAt
		}
		if current.LastObservedAt.Before(report.LastObservedAt) {
			current.LastObservedAt = report.LastObservedAt
		}
		if current.Attempts < report.Attempts {
			current.Attempts = report.Attempts
		}
		current.Node = report.Node
		current.VolumeID = report.VolumeID
		current.VolumeName = report.VolumeName
		current.FailureClass = localDeviceFailureClass(report)
		current.DevicePath = report.DevicePath
		current.DeviceSerial = report.DeviceSerial
		current.OpenNebulaImageID = report.OpenNebulaImageID
		current.FsType = report.FsType
		current.ResolvedBy = report.ResolvedBy
		current.PVCNamespace = report.PVCNamespace
		current.PVCName = report.PVCName
		current.PVName = report.PVName
		current.StagingTargetPath = report.StagingTargetPath
		current.RecoveryAttempts++
		current.LastRecoveryAt = &now
		current.LastRecoveryError = message
		current.LastRecoveryOutcome = "failed"
		current.LastRecoverySignature = localDeviceRecoverySignature(*current)
	})
}

func (s *ControllerServer) markLocalDeviceRecoverySkipped(ctx context.Context, key string, report LocalDeviceMissingReport, reason string, cause error) error {
	message := reason
	if cause != nil {
		message = fmt.Sprintf("%s: %v", reason, cause)
	}
	klog.V(3).InfoS("Skipping local device recovery", "node", report.Node, "volumeID", report.VolumeID, "reason", message)
	if s.driver != nil && s.driver.metrics != nil {
		s.driver.metrics.RecordLocalVolumeHealth("device_recovery", "skipped_"+reason)
	}
	return updateLocalDeviceReport(ctx, s.driver.kubeRuntime, namespaceFromServiceAccount(), key, func(current *LocalDeviceMissingReport) {
		if strings.TrimSpace(current.Node) == "" {
			*current = report
		}
		current.LastRecoveryError = message
		current.LastRecoveryOutcome = "skipped"
	})
}

func (s *ControllerServer) clearLocalDeviceReportWithEvent(ctx context.Context, key string, report LocalDeviceMissingReport, runtimeCtx *VolumeRuntimeContext, reason string) error {
	if s.driver != nil && s.driver.metrics != nil {
		s.driver.metrics.RecordLocalVolumeHealth("device_recovery", "cleared")
	}
	s.recordLocalDeviceRecoveryEvent(ctx, report, runtimeCtx, eventReasonLocalDeviceRecoverySkipped, fmt.Sprintf("cleared missing-device report for volume %s: %s", report.VolumeID, reason))
	return s.clearLocalDeviceReport(ctx, key)
}

func (s *ControllerServer) clearLocalDeviceReport(ctx context.Context, key string) error {
	return s.driver.kubeRuntime.DeleteConfigMapKey(ctx, namespaceFromServiceAccount(), localDeviceStateConfigMapName, key)
}

func (s *ControllerServer) recordLocalDeviceRecoveryEvent(ctx context.Context, report LocalDeviceMissingReport, runtimeCtx *VolumeRuntimeContext, reason, message string) {
	namespace := strings.TrimSpace(report.PVCNamespace)
	name := strings.TrimSpace(report.PVCName)
	if runtimeCtx != nil {
		if namespace == "" {
			namespace = runtimeCtx.PVCNamespace
		}
		if name == "" {
			name = runtimeCtx.PVCName
		}
	}
	if namespace == "" || name == "" || s == nil || s.driver == nil || s.driver.kubeRuntime == nil {
		return
	}
	if strings.HasSuffix(reason, "Failed") || strings.HasSuffix(reason, "Skipped") {
		s.driver.kubeRuntime.EmitWarningEventOnPVC(ctx, namespace, name, reason, message)
		return
	}
	s.driver.kubeRuntime.EmitPVCEvent(ctx, namespace, name, reason, message)
}

func updateLocalDeviceReport(ctx context.Context, runtime *KubeRuntime, namespace, key string, mutate func(*LocalDeviceMissingReport)) error {
	if runtime == nil || !runtime.enabled {
		return fmt.Errorf("kubernetes runtime is not enabled")
	}
	if namespace == "" {
		namespace = namespaceFromServiceAccount()
	}
	cmClient := runtime.client.CoreV1().ConfigMaps(namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := cmClient.Get(ctx, localDeviceStateConfigMapName, metav1.GetOptions{})
		report := LocalDeviceMissingReport{}
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			mutate(&report)
			payload, marshalErr := json.Marshal(report)
			if marshalErr != nil {
				return marshalErr
			}
			_, err = cmClient.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: localDeviceStateConfigMapName, Namespace: namespace},
				Data:       map[string]string{key: string(payload)},
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(corev1.Resource("configmaps"), localDeviceStateConfigMapName, err)
			}
			return err
		}
		if current.Data == nil {
			current.Data = map[string]string{}
		}
		if raw := strings.TrimSpace(current.Data[key]); raw != "" {
			if err := json.Unmarshal([]byte(raw), &report); err != nil {
				klog.V(3).InfoS("Replacing malformed local device report", "key", key, "err", err)
				report = LocalDeviceMissingReport{}
			}
		}
		mutate(&report)
		payload, err := json.Marshal(report)
		if err != nil {
			return err
		}
		current.Data[key] = string(payload)
		_, err = cmClient.Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
}

func localDeviceRecoveryParams(report LocalDeviceMissingReport, runtimeCtx *VolumeRuntimeContext) map[string]string {
	params := map[string]string{
		annotationBackend: "local",
	}
	if value := strings.TrimSpace(report.PVCNamespace); value != "" {
		params[paramPVCNamespace] = value
	} else if runtimeCtx != nil && runtimeCtx.PVCNamespace != "" {
		params[paramPVCNamespace] = runtimeCtx.PVCNamespace
	}
	if value := strings.TrimSpace(report.PVCName); value != "" {
		params[paramPVCName] = value
	} else if runtimeCtx != nil && runtimeCtx.PVCName != "" {
		params[paramPVCName] = runtimeCtx.PVCName
	}
	if value := strings.TrimSpace(report.PVName); value != "" {
		params[paramPVName] = value
	} else if runtimeCtx != nil && runtimeCtx.PVName != "" {
		params[paramPVName] = runtimeCtx.PVName
	}
	if value := strings.TrimSpace(report.DeviceSerial); value != "" {
		params[publishContextDeviceSerial] = value
	}
	if value := strings.TrimSpace(report.OpenNebulaImageID); value != "" {
		params[publishContextOpenNebulaImageID] = value
	}
	return params
}

func localDeviceFailureClass(report LocalDeviceMissingReport) string {
	switch strings.TrimSpace(report.FailureClass) {
	case localDeviceFailureClassMountFailed:
		return localDeviceFailureClassMountFailed
	default:
		return localDeviceFailureClassMissingDevice
	}
}

func localDeviceRecoverySignature(report LocalDeviceMissingReport) string {
	parts := []string{
		localDeviceFailureClass(report),
		strings.TrimSpace(report.VolumeName),
		strings.TrimSpace(report.DevicePath),
		strings.TrimSpace(report.DeviceSerial),
		strings.TrimSpace(report.OpenNebulaImageID),
		strings.TrimSpace(report.FsType),
		strings.TrimSpace(report.ResolvedBy),
		strings.TrimSpace(report.StagingTargetPath),
	}
	return strings.Join(parts, "|")
}

func localDeviceReportKey(node, volumeID string) string {
	return sanitizeLocalDeviceReportKey(node) + "." + sanitizeLocalDeviceReportKey(volumeID)
}

func sanitizeLocalDeviceReportKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return replacer.Replace(value)
}

func (s *ControllerServer) localDeviceRecoveryEnabled() bool {
	if s == nil || s.driver == nil {
		return false
	}
	enabled, ok := s.driver.PluginConfig.GetBool(config.LocalDeviceRecoveryEnabledVar)
	return !ok || enabled
}

func (s *ControllerServer) localDeviceRecoveryMinAttempts() int {
	attempts, ok := s.driver.PluginConfig.GetInt(config.LocalDeviceRecoveryMinAttemptsVar)
	if !ok || attempts <= 0 {
		return 3
	}
	return attempts
}

func (s *ControllerServer) localDeviceRecoveryMinAge() time.Duration {
	seconds, ok := s.driver.PluginConfig.GetInt(config.LocalDeviceRecoveryMinAgeSecondsVar)
	if !ok || seconds < 0 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func (s *ControllerServer) localDeviceRecoveryInterval() time.Duration {
	seconds, ok := s.driver.PluginConfig.GetInt(config.LocalDeviceRecoveryIntervalSecondsVar)
	if !ok || seconds <= 0 {
		seconds = 15
	}
	return time.Duration(seconds) * time.Second
}

func (s *ControllerServer) localDeviceRecoveryCooldown() time.Duration {
	seconds, ok := s.driver.PluginConfig.GetInt(config.LocalDeviceRecoveryCooldownSecondsVar)
	if !ok || seconds < 0 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func (s *ControllerServer) localDeviceRecoveryMaxAttempts() int {
	attempts, ok := s.driver.PluginConfig.GetInt(config.LocalDeviceRecoveryMaxAttemptsVar)
	if !ok || attempts <= 0 {
		return 2
	}
	return attempts
}
