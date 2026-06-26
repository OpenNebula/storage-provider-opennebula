package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

const (
	maintenanceModeKey        = "maintainenceMode"
	maintenanceModeAliasKey   = "maintenanceMode"
	maintenanceReadyKey       = "maintainenceReady"
	maintenanceReadyAliasKey  = "maintenanceReady"
	maintenanceStickyReason   = "maintenance_mode"
	maintenanceReleaseReason  = "maintenance_release"
	maintenanceWatchResync    = 30 * time.Second
	maintenanceWatchTimeout   = 10 * time.Minute
	maintenanceWatchRetryWait = 5 * time.Second
)

type MaintenanceModeManager struct {
	driver    *Driver
	runtime   *KubeRuntime
	namespace string

	mu         sync.RWMutex
	active     bool
	ready      bool
	aliasReady bool
}

type maintenancePrepareReport struct {
	Protected int
	Patched   int
	Skipped   int
	Conflicts int
}

func NewMaintenanceModeManager(driver *Driver, namespace string) *MaintenanceModeManager {
	if driver == nil || driver.kubeRuntime == nil || !driver.kubeRuntime.enabled {
		return nil
	}
	if strings.TrimSpace(namespace) == "" {
		namespace = namespaceFromServiceAccount()
	}
	return &MaintenanceModeManager{
		driver:    driver,
		runtime:   driver.kubeRuntime,
		namespace: namespace,
	}
}

func (m *MaintenanceModeManager) Run(ctx context.Context) {
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return
	}
	_ = m.Reconcile(ctx)
	for {
		if err := m.watchOnce(ctx); err != nil && ctx.Err() == nil {
			klog.V(2).InfoS("Maintenance mode watch failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(maintenanceWatchRetryWait):
		}
	}
}

func (m *MaintenanceModeManager) watchOnce(ctx context.Context) error {
	timeout := int64(maintenanceWatchTimeout.Seconds())
	watcher, err := m.runtime.client.CoreV1().ConfigMaps(m.namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:  fields.OneTermEqualSelector("metadata.name", hotplugStateConfigMapName).String(),
		TimeoutSeconds: &timeout,
	})
	if err != nil {
		return err
	}
	defer watcher.Stop()

	ticker := time.NewTicker(maintenanceWatchResync)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-watcher.ResultChan():
			if !ok {
				return nil
			}
			_ = m.Reconcile(ctx)
		case <-ticker.C:
			_ = m.Reconcile(ctx)
		}
	}
}

func (m *MaintenanceModeManager) Reconcile(ctx context.Context) error {
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return nil
	}
	cm, err := m.runtime.GetConfigMap(ctx, m.namespace, hotplugStateConfigMapName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			m.setState(false, false, false)
			return nil
		}
		return err
	}

	active, aliasPresent := maintenanceModeActive(cm.Data)
	if !active {
		m.setState(false, false, aliasPresent)
		if m.driver != nil && m.driver.metrics != nil {
			m.driver.metrics.SetMaintenanceActive(false)
			m.driver.metrics.SetMaintenanceProtectedVolumes(0)
		}
		if err := m.clearReadyKeys(ctx); err != nil {
			return err
		}
		m.releaseMaintenanceHolds(ctx)
		return nil
	}

	m.setState(true, false, aliasPresent)
	if m.driver != nil && m.driver.metrics != nil {
		m.driver.metrics.SetMaintenanceActive(true)
		m.driver.metrics.RecordMaintenance("mode", "active")
	}

	report := m.prepare(ctx)
	if err := m.publishReady(ctx, aliasPresent); err != nil {
		if m.driver != nil && m.driver.metrics != nil {
			m.driver.metrics.RecordMaintenance("ready", "error")
		}
		return err
	}
	m.setState(true, true, aliasPresent)
	if m.driver != nil && m.driver.metrics != nil {
		m.driver.metrics.SetMaintenanceProtectedVolumes(report.Protected)
		m.driver.metrics.RecordMaintenance("ready", "published")
	}
	klog.V(1).InfoS("Maintenance mode prepared",
		"protected", report.Protected,
		"patched", report.Patched,
		"skipped", report.Skipped,
		"conflicts", report.Conflicts)
	return nil
}

func maintenanceModeActive(data map[string]string) (bool, bool) {
	if data == nil {
		return false, false
	}
	aliasPresent := false
	active := false
	if raw, ok := data[maintenanceModeKey]; ok {
		active = parseMaintenanceBool(raw)
	}
	if raw, ok := data[maintenanceModeAliasKey]; ok {
		aliasPresent = true
		active = active || parseMaintenanceBool(raw)
	}
	return active, aliasPresent
}

func parseMaintenanceBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func isMaintenanceConfigMapKey(key string) bool {
	switch strings.TrimSpace(key) {
	case maintenanceModeKey, maintenanceModeAliasKey, maintenanceReadyKey, maintenanceReadyAliasKey:
		return true
	default:
		return false
	}
}

func (m *MaintenanceModeManager) Active() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

func (m *MaintenanceModeManager) Ready() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ready
}

func (m *MaintenanceModeManager) setState(active, ready, aliasReady bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = active
	m.ready = ready
	m.aliasReady = aliasReady
}

func (m *MaintenanceModeManager) publishReady(ctx context.Context, aliasReady bool) error {
	if current, err := m.runtime.GetConfigMap(ctx, m.namespace, hotplugStateConfigMapName); err == nil {
		readySet := current.Data != nil && current.Data[maintenanceReadyKey] == "true"
		aliasSet := !aliasReady || (current.Data != nil && current.Data[maintenanceReadyAliasKey] == "true")
		if readySet && aliasSet {
			if !aliasReady {
				return m.runtime.DeleteConfigMapKey(ctx, m.namespace, hotplugStateConfigMapName, maintenanceReadyAliasKey)
			}
			return nil
		}
	}
	data := map[string]string{maintenanceReadyKey: "true"}
	if aliasReady {
		data[maintenanceReadyAliasKey] = "true"
	}
	return m.runtime.UpsertConfigMapData(ctx, m.namespace, hotplugStateConfigMapName, data)
}

func (m *MaintenanceModeManager) clearReadyKeys(ctx context.Context) error {
	if err := m.runtime.DeleteConfigMapKey(ctx, m.namespace, hotplugStateConfigMapName, maintenanceReadyKey); err != nil {
		return err
	}
	return m.runtime.DeleteConfigMapKey(ctx, m.namespace, hotplugStateConfigMapName, maintenanceReadyAliasKey)
}

func (m *MaintenanceModeManager) prepare(ctx context.Context) maintenancePrepareReport {
	report := maintenancePrepareReport{}
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return report
	}
	pvs, err := m.runtime.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.V(2).InfoS("Maintenance mode failed to list persistent volumes", "err", err)
		return report
	}
	vas, err := m.runtime.client.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.V(2).InfoS("Maintenance mode failed to list volume attachments", "err", err)
		return report
	}
	attachedByPV, conflicts := maintenanceAttachedNodesByPV(vas.Items)

	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if !eligibleForLastNodePreference(pv) {
			continue
		}
		volumeHandle := strings.TrimSpace(pv.Spec.CSI.VolumeHandle)
		node, conflict := m.preferredMaintenanceNode(pv, attachedByPV, conflicts)
		if conflict {
			report.Conflicts++
			m.recordMaintenanceOutcome("prepare", "conflict")
			continue
		}
		if strings.TrimSpace(node) == "" {
			report.Skipped++
			m.recordMaintenanceOutcome("prepare", "missing_node")
			continue
		}
		report.Protected++
		if pv.Annotations == nil || strings.TrimSpace(pv.Annotations[annotationLastAttachedNode]) != node {
			if err := m.patchPVLastAttachedNode(ctx, pv.Name, node); err != nil {
				report.Skipped++
				m.recordMaintenanceOutcome("prepare", "patch_error")
				klog.V(2).InfoS("Maintenance mode failed to patch PV last attached node", "pv", pv.Name, "volume", volumeHandle, "node", node, "err", err)
				continue
			}
			report.Patched++
			m.recordMaintenanceOutcome("prepare", "patched")
			if pv.Spec.ClaimRef != nil {
				m.runtime.EmitPVCEvent(ctx, pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name, eventReasonMaintenanceModePrepared, fmt.Sprintf("prepared local RWO volume %s to remain on node %s during maintenance mode", volumeHandle, node))
			}
		} else {
			m.recordMaintenanceOutcome("prepare", "observed")
		}
	}
	return report
}

func (m *MaintenanceModeManager) preferredMaintenanceNode(pv *corev1.PersistentVolume, attachedByPV map[string]string, conflicts map[string]struct{}) (string, bool) {
	if pv == nil {
		return "", false
	}
	if _, ok := conflicts[pv.Name]; ok {
		return "", true
	}
	if pv.Annotations != nil {
		if preferred := strings.TrimSpace(pv.Annotations[annotationPreferredLastNode]); preferred != "" {
			return preferred, false
		}
	}
	if pv.Spec.CSI != nil && m != nil && m.driver != nil && m.driver.stickyAttachments != nil {
		if state, ok := m.driver.stickyAttachments.Get(pv.Spec.CSI.VolumeHandle); ok && strings.TrimSpace(state.NodeID) != "" {
			return strings.TrimSpace(state.NodeID), false
		}
	}
	if attached := strings.TrimSpace(attachedByPV[pv.Name]); attached != "" {
		return attached, false
	}
	if pv.Annotations != nil {
		return strings.TrimSpace(pv.Annotations[annotationLastAttachedNode]), false
	}
	return "", false
}

func maintenanceAttachedNodesByPV(vas []storagev1.VolumeAttachment) (map[string]string, map[string]struct{}) {
	attached := map[string]string{}
	conflicts := map[string]struct{}{}
	for _, va := range vas {
		if va.Spec.Source.PersistentVolumeName == nil || strings.TrimSpace(*va.Spec.Source.PersistentVolumeName) == "" {
			continue
		}
		if !va.Status.Attached {
			continue
		}
		if !va.DeletionTimestamp.IsZero() {
			continue
		}
		pvName := strings.TrimSpace(*va.Spec.Source.PersistentVolumeName)
		node := strings.TrimSpace(va.Spec.NodeName)
		if node == "" {
			continue
		}
		if previous := attached[pvName]; previous != "" && previous != node {
			conflicts[pvName] = struct{}{}
			continue
		}
		attached[pvName] = node
	}
	return attached, conflicts
}

func (m *MaintenanceModeManager) patchPVLastAttachedNode(ctx context.Context, pvName, node string) error {
	payload, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				annotationLastAttachedNode: node,
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = m.runtime.client.CoreV1().PersistentVolumes().Patch(ctx, pvName, types.MergePatchType, payload, metav1.PatchOptions{})
	return err
}

func (m *MaintenanceModeManager) recordMaintenanceOutcome(operation, outcome string) {
	if m == nil || m.driver == nil || m.driver.metrics == nil {
		return
	}
	m.driver.metrics.RecordMaintenance(operation, outcome)
}

func (m *MaintenanceModeManager) releaseMaintenanceHolds(ctx context.Context) {
	if m == nil || m.driver == nil || m.driver.stickyAttachments == nil {
		return
	}
	states := m.driver.stickyAttachments.ListByReason(maintenanceStickyReason)
	if len(states) == 0 {
		return
	}
	sort.Slice(states, func(i, j int) bool {
		return states[i].VolumeID < states[j].VolumeID
	})
	minDelay, maxDelay := maintenanceReleaseBounds(m.driver.PluginConfig)
	now := time.Now().UTC()
	for i, state := range states {
		delay := staggeredMaintenanceDelay(i, len(states), minDelay, maxDelay)
		state.Reason = maintenanceReleaseReason
		state.StartedAt = now
		state.ExpiresAt = now.Add(delay)
		state.GraceSeconds = int(delay.Seconds())
		if err := m.driver.stickyAttachments.Update(state); err != nil {
			klog.V(2).InfoS("Failed to release maintenance sticky attachment", "volumeID", state.VolumeID, "err", err)
			m.recordMaintenanceOutcome("release", "error")
			continue
		}
		m.recordMaintenanceOutcome("release", "scheduled")
		if m.runtime != nil {
			m.runtime.EmitPVCEvent(ctx, state.PVCNamespace, state.PVCName, eventReasonMaintenanceModeReleased, fmt.Sprintf("scheduled maintenance attachment release for volume %s on node %s after %s", state.VolumeID, state.NodeID, delay))
		}
	}
}

func maintenanceReleaseBounds(cfg config.CSIPluginConfig) (time.Duration, time.Duration) {
	minSeconds, ok := cfg.GetInt(config.MaintenanceReleaseMinSecondsVar)
	if !ok || minSeconds < 0 {
		minSeconds = 300
	}
	maxSeconds, ok := cfg.GetInt(config.MaintenanceReleaseMaxSecondsVar)
	if !ok || maxSeconds < minSeconds {
		maxSeconds = minSeconds
	}
	return time.Duration(minSeconds) * time.Second, time.Duration(maxSeconds) * time.Second
}

func staggeredMaintenanceDelay(index, total int, minDelay, maxDelay time.Duration) time.Duration {
	if total <= 1 || maxDelay <= minDelay {
		return minDelay
	}
	span := maxDelay - minDelay
	return minDelay + time.Duration(int64(span)*int64(index)/int64(total-1))
}

func maintenanceStickyGraceSeconds(cfg config.CSIPluginConfig) int {
	_, maxDelay := maintenanceReleaseBounds(cfg)
	seconds := int(maxDelay.Seconds())
	if seconds <= 0 {
		return 1800
	}
	return seconds
}

func maintenanceLastNodeForVolume(driver *Driver, runtimeCtx *VolumeRuntimeContext, volumeID string) string {
	if driver != nil && driver.stickyAttachments != nil {
		if state, ok := driver.stickyAttachments.Get(volumeID); ok && strings.TrimSpace(state.NodeID) != "" {
			return strings.TrimSpace(state.NodeID)
		}
	}
	if runtimeCtx != nil && runtimeCtx.PVAnnotations != nil {
		return strings.TrimSpace(runtimeCtx.PVAnnotations[annotationLastAttachedNode])
	}
	return ""
}

func runtimeContextEligibleForMaintenance(runtimeCtx *VolumeRuntimeContext, volumeID string) bool {
	if runtimeCtx == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(runtimeCtx.Backend), "local") {
		return false
	}
	if opennebula.IsSharedFilesystemVolumeID(volumeID) {
		return false
	}
	return hasSingleWriterAccessMode(runtimeCtx.AccessModes)
}

func maintenanceModeError(volumeID, requestedNode, lastNode string) error {
	return fmt.Errorf("maintenance mode is active for local RWO volume %s: refusing cross-node attach to %s while last attached node is %s", volumeID, requestedNode, lastNode)
}

func maintenanceDataDebug(data map[string]string) string {
	if len(data) == 0 {
		return ""
	}
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+strconv.Quote(data[key]))
	}
	return strings.Join(parts, " ")
}
