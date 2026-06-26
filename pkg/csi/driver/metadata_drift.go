package driver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const (
	metadataDriftClassification = "opennebula_metadata_drift"
	metadataDriftReason         = "external_repair_required"
)

func (s *ControllerServer) rejectIfOpenNebulaMetadataDrift(ctx context.Context, req *csi.ControllerPublishVolumeRequest) error {
	if s == nil || s.driver == nil || req == nil || !s.metadataDriftQuarantineEnabled() {
		return nil
	}
	runtimeCtx, eligible := s.metadataDriftGuardEligible(ctx, req.VolumeId)
	if !eligible {
		return nil
	}
	inspector, ok := s.volumeProvider.(opennebula.VolumeAttachmentInspector)
	if !ok {
		return nil
	}

	activeState, quarantined := s.driver.volumeQuarantine.GetActive(req.VolumeId, time.Now().UTC())
	metadata, inspectErr := inspector.InspectVolumeAttachment(ctx, req.VolumeId, req.NodeId)
	if inspectErr != nil {
		if quarantined {
			message := volumeQuarantineMessage(activeState)
			s.driver.metrics.RecordVolumeQuarantine(activeState.Reason, "active_inspection_failed")
			s.recordPVCWarningFromRuntimeContext(ctx, runtimeCtx, eventReasonVolumeQuarantined, message)
			return status.Error(codes.FailedPrecondition, message)
		}
		klog.V(2).InfoS("Failed to inspect OpenNebula attachment metadata", "volumeID", req.VolumeId, "nodeID", req.NodeId, "err", inspectErr)
		return nil
	}

	if !attachmentMetadataDriftDetected(metadata) {
		if quarantined {
			if err := s.driver.volumeQuarantine.Clear(ctx, req.VolumeId); err != nil {
				klog.V(2).InfoS("Failed to clear volume quarantine after healthy metadata observation", "volumeID", req.VolumeId, "err", err)
			} else {
				s.driver.metrics.RecordVolumeQuarantine(activeState.Reason, "cleared")
			}
		}
		return nil
	}

	message := metadataDriftMessage(metadata, req.VolumeId, req.NodeId)
	state := VolumeQuarantineState{
		VolumeID:       req.VolumeId,
		Reason:         metadataDriftReason,
		Classification: metadataDriftClassification,
		Message:        message,
		RequestedNode:  req.NodeId,
		OwnerVMIDs:     metadata.ConflictingOwnerVMIDs(),
	}
	state, active, err := s.driver.volumeQuarantine.MarkFailure(
		ctx,
		state,
		s.metadataDriftQuarantineFailureThreshold(),
		s.metadataDriftQuarantineTTL(),
	)
	if err != nil {
		klog.V(2).InfoS("Failed to persist volume quarantine state", "volumeID", req.VolumeId, "err", err)
	}

	s.driver.metrics.RecordMetadataDrift(metadataDriftClassification, "detected")
	if active {
		s.driver.metrics.RecordVolumeQuarantine(metadataDriftReason, "active")
		s.recordPVCWarningFromRuntimeContext(ctx, runtimeCtx, eventReasonVolumeQuarantined, volumeQuarantineMessage(state))
	} else {
		s.driver.metrics.RecordVolumeQuarantine(metadataDriftReason, "observed")
		s.recordPVCWarningFromRuntimeContext(ctx, runtimeCtx, eventReasonOpenNebulaMetadataDrift, message)
	}
	return status.Error(codes.FailedPrecondition, message)
}

func (s *ControllerServer) detachMetadataDriftError(ctx context.Context, volumeID, nodeID string, cause error) error {
	if s == nil || s.driver == nil || !s.metadataDriftQuarantineEnabled() {
		return nil
	}
	runtimeCtx, eligible := s.metadataDriftGuardEligible(ctx, volumeID)
	if !eligible {
		return nil
	}
	inspector, ok := s.volumeProvider.(opennebula.VolumeAttachmentInspector)
	if !ok {
		return nil
	}
	metadata, inspectErr := inspector.InspectVolumeAttachment(ctx, volumeID, nodeID)
	if inspectErr != nil || !attachmentMetadataStillOwned(metadata) {
		return nil
	}
	message := fmt.Sprintf("%s; OpenNebula detach failed while metadata still reports an owner for local RWO volume %s on node %s: %v", metadataDriftMessage(metadata, volumeID, nodeID), volumeID, nodeID, cause)
	state := VolumeQuarantineState{
		VolumeID:       volumeID,
		Reason:         "detach_failed_metadata_drift",
		Classification: metadataDriftClassification,
		Message:        message,
		RequestedNode:  nodeID,
		OwnerVMIDs:     metadata.ConflictingOwnerVMIDs(),
	}
	state, active, err := s.driver.volumeQuarantine.MarkFailure(
		ctx,
		state,
		s.metadataDriftQuarantineFailureThreshold(),
		s.metadataDriftQuarantineTTL(),
	)
	if err != nil {
		klog.V(2).InfoS("Failed to persist detach metadata drift quarantine state", "volumeID", volumeID, "err", err)
	}
	s.driver.metrics.RecordMetadataDrift(metadataDriftClassification, "detach_failed")
	if active {
		s.driver.metrics.RecordVolumeQuarantine(state.Reason, "active")
		s.recordPVCWarningFromRuntimeContext(ctx, runtimeCtx, eventReasonVolumeQuarantined, volumeQuarantineMessage(state))
	} else {
		s.driver.metrics.RecordVolumeQuarantine(state.Reason, "observed")
		s.recordPVCWarningFromRuntimeContext(ctx, runtimeCtx, eventReasonOpenNebulaMetadataDrift, message)
	}
	return status.Error(codes.FailedPrecondition, message)
}

func (s *ControllerServer) metadataDriftGuardEligible(ctx context.Context, volumeID string) (*VolumeRuntimeContext, bool) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled {
		return nil, false
	}
	runtimeCtx, err := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, volumeID)
	if err != nil {
		return nil, false
	}
	return runtimeCtx, runtimeContextEligibleForMaintenance(runtimeCtx, volumeID)
}

func attachmentMetadataDriftDetected(metadata *opennebula.VolumeAttachmentMetadata) bool {
	if metadata == nil {
		return false
	}
	if len(metadata.ConflictingOwnerVMIDs()) > 0 {
		return true
	}
	if metadata.AttachedToRequestedNode {
		return false
	}
	if metadata.ImageRunningVMs > 0 && imageStateInUse(metadata.ImageState) {
		return true
	}
	return false
}

func attachmentMetadataStillOwned(metadata *opennebula.VolumeAttachmentMetadata) bool {
	if metadata == nil {
		return false
	}
	return metadata.AttachedToRequestedNode ||
		metadata.ImageRunningVMs > 0 ||
		len(metadata.ImageVMIDs) > 0 ||
		len(metadata.DiskRecords) > 0
}

func imageStateInUse(state string) bool {
	state = strings.ToUpper(strings.TrimSpace(state))
	return state == "USED" || state == "USED_PERS" || state == "LOCKED_USED" || state == "LOCKED_USED_PERS"
}

func metadataDriftMessage(metadata *opennebula.VolumeAttachmentMetadata, volumeID, requestedNode string) string {
	if metadata == nil {
		return fmt.Sprintf("OpenNebula metadata drift detected for local RWO volume %s while publishing to node %s", volumeID, requestedNode)
	}
	owners := intList(metadata.ConflictingOwnerVMIDs())
	if owners == "" && metadata.ImageRunningVMs > 0 {
		owners = fmt.Sprintf("unknown image owner count %d", metadata.ImageRunningVMs)
	}
	records := diskRecordSummary(metadata.DiskRecords)
	if records == "" {
		records = "no matching VM disk records found"
	}
	return fmt.Sprintf(
		"OpenNebula metadata drift detected for local RWO volume %s/image %d while publishing to node %s: image state=%s runningVMs=%d ownerVMs=%s diskRecords=%s; external OpenNebula repair is required before the driver will attempt another cross-node attach",
		firstNonEmpty(metadata.VolumeHandle, volumeID),
		metadata.ImageID,
		firstNonEmpty(metadata.RequestedNode, requestedNode),
		firstNonEmpty(metadata.ImageState, "unknown"),
		metadata.ImageRunningVMs,
		firstNonEmpty(owners, "none"),
		records,
	)
}

func volumeQuarantineMessage(state VolumeQuarantineState) string {
	if strings.TrimSpace(state.Message) != "" {
		return fmt.Sprintf("%s; volume quarantine active until %s after %d matching failure(s)", state.Message, state.ExpiresAt.Format(time.RFC3339), state.FailureCount)
	}
	return fmt.Sprintf("volume %s is quarantined for %s until %s after %d matching failure(s)", state.VolumeID, state.Reason, state.ExpiresAt.Format(time.RFC3339), state.FailureCount)
}

func diskRecordSummary(records []opennebula.VolumeDiskRecord) string {
	if len(records) == 0 {
		return ""
	}
	parts := make([]string, 0, len(records))
	for _, record := range records {
		fields := []string{}
		if record.NodeID > 0 {
			fields = append(fields, "vm="+strconv.Itoa(record.NodeID))
		}
		if strings.TrimSpace(record.NodeName) != "" {
			fields = append(fields, "node="+record.NodeName)
		}
		if record.DiskID > 0 {
			fields = append(fields, "disk="+strconv.Itoa(record.DiskID))
		}
		if strings.TrimSpace(record.Target) != "" {
			fields = append(fields, "target="+record.Target)
		}
		if strings.TrimSpace(record.Serial) != "" {
			fields = append(fields, "serial="+record.Serial)
		}
		parts = append(parts, strings.Join(fields, "/"))
	}
	return strings.Join(parts, ",")
}

func intList(values []int) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func (s *ControllerServer) metadataDriftQuarantineEnabled() bool {
	if s == nil || s.driver == nil || s.driver.volumeQuarantine == nil {
		return false
	}
	enabled, ok := s.driver.PluginConfig.GetBool(config.MetadataDriftQuarantineEnabledVar)
	return !ok || enabled
}

func (s *ControllerServer) metadataDriftQuarantineFailureThreshold() int {
	if s == nil || s.driver == nil {
		return 2
	}
	threshold, ok := s.driver.PluginConfig.GetInt(config.MetadataDriftQuarantineFailureThresholdVar)
	if !ok || threshold <= 0 {
		return 2
	}
	return threshold
}

func (s *ControllerServer) metadataDriftQuarantineTTL() time.Duration {
	if s == nil || s.driver == nil {
		return 30 * time.Minute
	}
	seconds, ok := s.driver.PluginConfig.GetInt(config.MetadataDriftQuarantineTTLSecondsVar)
	if !ok || seconds <= 0 {
		seconds = 1800
	}
	return time.Duration(seconds) * time.Second
}
