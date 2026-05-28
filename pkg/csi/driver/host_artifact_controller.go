package driver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

func (s *ControllerServer) rejectIfHostArtifactQuarantineActive(ctx context.Context, req *csi.ControllerPublishVolumeRequest) error {
	if s == nil || s.driver == nil || req == nil || !s.hostArtifactQuarantineEnabled() {
		return nil
	}
	if len(s.driver.hostArtifactQuarantine.Snapshot()) == 0 {
		return nil
	}
	inspector, ok := s.volumeProvider.(opennebula.HostArtifactAttachmentTargetInspector)
	if !ok {
		return nil
	}
	target, err := inspector.InspectHostArtifactAttachmentTarget(ctx, req.VolumeId, req.NodeId, req.GetVolumeContext())
	if err != nil {
		klog.V(2).InfoS("Failed to inspect host artifact attachment target", "volumeID", req.VolumeId, "nodeID", req.NodeId, "err", err)
		return nil
	}
	if target == nil {
		return nil
	}
	state, active := s.driver.hostArtifactQuarantine.GetActive(target.Key(), time.Now().UTC())
	if !active {
		return nil
	}
	message := hostArtifactQuarantineMessage(state)
	s.driver.metrics.RecordHostArtifactQuarantine(state.Classification, "active")
	s.recordHostArtifactWarning(ctx, req.VolumeId, req.GetVolumeContext(), eventReasonHostArtifactQuarantined, message)
	return status.Error(codes.FailedPrecondition, message)
}

func (s *ControllerServer) handleHostArtifactConflict(ctx context.Context, req *csi.ControllerPublishVolumeRequest, conflict *opennebula.HostArtifactConflictError) error {
	if s == nil || s.driver == nil || req == nil || conflict == nil {
		return nil
	}
	if !s.hostArtifactQuarantineEnabled() {
		message := conflict.Error()
		s.recordHostArtifactWarning(ctx, req.VolumeId, req.GetVolumeContext(), eventReasonHostArtifactQuarantined, message)
		return status.Error(codes.FailedPrecondition, message)
	}
	state := hostArtifactStateFromConflict(req.VolumeId, conflict)
	state, active, err := s.driver.hostArtifactQuarantine.MarkFailure(
		ctx,
		state,
		s.hostArtifactQuarantineFailureThreshold(),
		s.hostArtifactQuarantineTTL(),
	)
	if err != nil {
		klog.V(2).InfoS("Failed to persist host artifact quarantine state", "volumeID", req.VolumeId, "key", state.Key, "err", err)
	}
	outcome := "observed"
	message := state.Message
	if active {
		outcome = "active"
		message = hostArtifactQuarantineMessage(state)
	}
	s.driver.metrics.RecordHostArtifactQuarantine(state.Classification, outcome)
	s.recordHostArtifactWarning(ctx, req.VolumeId, req.GetVolumeContext(), eventReasonHostArtifactQuarantined, message)
	return status.Error(codes.FailedPrecondition, message)
}

func hostArtifactStateFromConflict(volumeID string, conflict *opennebula.HostArtifactConflictError) HostArtifactQuarantineState {
	target := conflict.Target
	classification := strings.TrimSpace(conflict.Classification)
	if classification == "" {
		classification = opennebula.HostArtifactConflictClassification
	}
	key := target.Key()
	if key == "" {
		key = strings.TrimSpace(volumeID)
	}
	state := HostArtifactQuarantineState{
		Key:                 key,
		Classification:      classification,
		Reason:              hostArtifactReason,
		VolumeID:            strings.TrimSpace(volumeID),
		ImageID:             target.ImageID,
		NodeName:            target.NodeName,
		VMID:                target.VMID,
		HostID:              target.HostID,
		HostName:            target.HostName,
		SystemDatastoreID:   target.SystemDatastoreID,
		SystemDatastoreName: target.SystemDatastoreName,
		SystemDatastoreTM:   target.SystemDatastoreTM,
		DiskID:              target.DiskID,
		Target:              target.Target,
		LVName:              target.LVName,
		Message:             conflict.Error(),
	}
	if state.LVName != "" {
		state.RepairHint = fmt.Sprintf("read-only quarantine: inspect the OpenNebula VM log and host-local LV %s; after an operator confirms it is stale and not attached to the VM, repair the host artifact externally, then delete ConfigMap %s key %s or wait for the TTL before retrying", state.LVName, hostArtifactStateConfigMapName, state.Key)
	} else {
		state.RepairHint = fmt.Sprintf("read-only quarantine: inspect the OpenNebula VM log and host-local LVM artifacts; after an operator confirms the artifact is stale, repair it externally, then delete ConfigMap %s key %s or wait for the TTL before retrying", hostArtifactStateConfigMapName, state.Key)
	}
	return state
}

func (s *ControllerServer) clearHostArtifactQuarantineForVolume(ctx context.Context, volumeID string) {
	if s == nil || s.driver == nil || s.driver.hostArtifactQuarantine == nil || strings.TrimSpace(volumeID) == "" {
		return
	}
	for key, state := range s.driver.hostArtifactQuarantine.Snapshot() {
		if strings.TrimSpace(state.VolumeID) != strings.TrimSpace(volumeID) {
			continue
		}
		if err := s.driver.hostArtifactQuarantine.Clear(ctx, key); err != nil {
			klog.V(2).InfoS("Failed to clear host artifact quarantine after successful attach", "volumeID", volumeID, "key", key, "err", err)
			continue
		}
		s.driver.metrics.RecordHostArtifactQuarantine(state.Classification, "cleared")
	}
}

func (s *ControllerServer) recordHostArtifactWarning(ctx context.Context, volumeID string, params map[string]string, reason, message string) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled {
		s.recordPVCWarningFromParams(ctx, params, reason, message)
		return
	}
	runtimeCtx, err := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, volumeID)
	if err != nil || runtimeCtx == nil {
		s.recordPVCWarningFromParams(ctx, params, reason, message)
		return
	}
	s.recordPVCWarningFromRuntimeContext(ctx, runtimeCtx, reason, message)
}

func (s *ControllerServer) hostArtifactQuarantineEnabled() bool {
	if s == nil || s.driver == nil || s.driver.hostArtifactQuarantine == nil {
		return false
	}
	enabled, ok := s.driver.PluginConfig.GetBool(config.HostArtifactQuarantineEnabledVar)
	return !ok || enabled
}

func (s *ControllerServer) hostArtifactQuarantineFailureThreshold() int {
	if s == nil || s.driver == nil {
		return 1
	}
	threshold, ok := s.driver.PluginConfig.GetInt(config.HostArtifactQuarantineFailureThresholdVar)
	if !ok || threshold <= 0 {
		return 1
	}
	return threshold
}

func (s *ControllerServer) hostArtifactQuarantineTTL() time.Duration {
	if s == nil || s.driver == nil {
		return time.Hour
	}
	seconds, ok := s.driver.PluginConfig.GetInt(config.HostArtifactQuarantineTTLSecondsVar)
	if !ok || seconds <= 0 {
		seconds = 3600
	}
	return time.Duration(seconds) * time.Second
}
