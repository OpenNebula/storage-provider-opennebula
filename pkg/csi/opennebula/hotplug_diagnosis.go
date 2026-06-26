package opennebula

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/shared"
	vmSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
)

const (
	HotplugClassificationReady       = "Ready"
	HotplugClassificationProgressing = "Progressing"
	HotplugClassificationStuck       = "Stuck"
	HotplugClassificationCooldown    = "Cooldown"
	HotplugClassificationUnknown     = "Unknown"
)

type HotplugDiagnosisConfig struct {
	StuckAfter     time.Duration
	ProgressWindow time.Duration
	Now            func() time.Time
}

type HotplugObservation struct {
	Node                 string    `json:"node,omitempty"`
	VMID                 int       `json:"vmID,omitempty"`
	VMName               string    `json:"vmName,omitempty"`
	State                string    `json:"state,omitempty"`
	LCMState             string    `json:"lcmState,omitempty"`
	LCMStateRaw          int       `json:"lcmStateRaw,omitempty"`
	LastPoll             int       `json:"lastPoll,omitempty"`
	Signature            string    `json:"signature,omitempty"`
	FirstObservedAt      time.Time `json:"firstObservedAt,omitempty"`
	LastObservedAt       time.Time `json:"lastObservedAt,omitempty"`
	LastChangedAt        time.Time `json:"lastChangedAt,omitempty"`
	Operation            string    `json:"operation,omitempty"`
	VolumeHandle         string    `json:"volumeHandle,omitempty"`
	PersistentDiskID     int       `json:"persistentDiskID,omitempty"`
	VMDiskID             int       `json:"vmDiskID,omitempty"`
	DiskTarget           string    `json:"diskTarget,omitempty"`
	DiskAttachFlag       string    `json:"diskAttachFlag,omitempty"`
	PendingDiskSignature string    `json:"pendingDiskSignature,omitempty"`
}

type HotplugDiagnosis struct {
	Node                 string    `json:"node,omitempty"`
	VMID                 int       `json:"vmID,omitempty"`
	VMName               string    `json:"vmName,omitempty"`
	State                string    `json:"state,omitempty"`
	LCMState             string    `json:"lcmState,omitempty"`
	Classification       string    `json:"classification,omitempty"`
	Operation            string    `json:"operation,omitempty"`
	VolumeHandle         string    `json:"volumeHandle,omitempty"`
	PersistentDiskID     int       `json:"persistentDiskID,omitempty"`
	VMDiskID             int       `json:"vmDiskID,omitempty"`
	DiskTarget           string    `json:"diskTarget,omitempty"`
	DiskAttachFlag       string    `json:"diskAttachFlag,omitempty"`
	FirstObservedAt      time.Time `json:"firstObservedAt,omitempty"`
	LastObservedAt       time.Time `json:"lastObservedAt,omitempty"`
	LastChangedAt        time.Time `json:"lastChangedAt,omitempty"`
	AgeSeconds           int64     `json:"ageSeconds,omitempty"`
	StuckAfterSeconds    int64     `json:"stuckAfterSeconds,omitempty"`
	Reason               string    `json:"reason,omitempty"`
	Message              string    `json:"message,omitempty"`
	RecommendedAction    string    `json:"recommendedAction,omitempty"`
	ObservationSignature string    `json:"observationSignature,omitempty"`
}

func (p *PersistentDiskVolumeProvider) DiagnoseNodeHotplug(ctx context.Context, node string, previous *HotplugObservation, cfg HotplugDiagnosisConfig) (HotplugDiagnosis, HotplugObservation, error) {
	vmID, err := p.NodeExists(ctx, node)
	if err != nil {
		return HotplugDiagnosis{}, HotplugObservation{}, err
	}
	vmInfo, err := p.ctrl.VM(vmID).InfoContext(ctx, true)
	if err != nil {
		return HotplugDiagnosis{}, HotplugObservation{}, fmt.Errorf("failed to inspect VM %d for hotplug diagnosis: %w", vmID, err)
	}
	current := BuildHotplugObservation(node, vmInfo, cfg.Now)
	diagnosis, updated := ClassifyHotplugObservation(current, previous, cfg)
	return diagnosis, updated, nil
}

func BuildHotplugObservation(node string, vmInfo *vmSchema.VM, nowFunc func() time.Time) HotplugObservation {
	now := time.Now().UTC()
	if nowFunc != nil {
		now = nowFunc().UTC()
	}
	obs := HotplugObservation{
		Node:            strings.TrimSpace(node),
		FirstObservedAt: now,
		LastObservedAt:  now,
		LastChangedAt:   now,
		Operation:       "unknown",
	}
	if vmInfo == nil {
		obs.LCMState = "UNKNOWN"
		return obs
	}
	vmState, vmLCM, _ := vmInfo.State()
	obs.VMID = vmInfo.ID
	obs.VMName = vmInfo.Name
	obs.State = vmState.String()
	obs.LCMState = vmLCM.String()
	obs.LCMStateRaw = vmInfo.LCMStateRaw
	obs.LastPoll = vmInfo.LastPoll

	diskSignatures := make([]string, 0)
	var fallback *HotplugObservation
	for _, disk := range vmInfo.Template.GetDisks() {
		diskObs := observationFromDisk(obs.Node, disk)
		signature := diskObservationSignature(diskObs)
		if signature != "" {
			diskSignatures = append(diskSignatures, signature)
		}
		if strings.EqualFold(diskObs.DiskAttachFlag, "YES") {
			obs.Operation = "attach"
			obs.VolumeHandle = diskObs.VolumeHandle
			obs.PersistentDiskID = diskObs.PersistentDiskID
			obs.VMDiskID = diskObs.VMDiskID
			obs.DiskTarget = diskObs.DiskTarget
			obs.DiskAttachFlag = diskObs.DiskAttachFlag
			obs.PendingDiskSignature = signature
			continue
		}
		if fallback == nil && strings.HasPrefix(diskObs.VolumeHandle, "pvc-") {
			copy := diskObs
			fallback = &copy
		}
	}
	if obs.VolumeHandle == "" && fallback != nil {
		obs.VolumeHandle = fallback.VolumeHandle
		obs.PersistentDiskID = fallback.PersistentDiskID
		obs.VMDiskID = fallback.VMDiskID
		obs.DiskTarget = fallback.DiskTarget
		obs.DiskAttachFlag = fallback.DiskAttachFlag
		obs.PendingDiskSignature = diskObservationSignature(*fallback)
	}
	if obs.Operation == "unknown" && obs.LCMStateRaw == int(vmSchema.Hotplug) && obs.VolumeHandle != "" {
		obs.Operation = "attach"
	}
	sort.Strings(diskSignatures)
	obs.Signature = strings.Join(diskSignatures, "|")
	return obs
}

func ClassifyHotplugObservation(current HotplugObservation, previous *HotplugObservation, cfg HotplugDiagnosisConfig) (HotplugDiagnosis, HotplugObservation) {
	if cfg.StuckAfter <= 0 {
		cfg.StuckAfter = 5 * time.Minute
	}
	if cfg.ProgressWindow <= 0 {
		cfg.ProgressWindow = time.Minute
	}
	now := current.LastObservedAt
	if now.IsZero() {
		now = time.Now().UTC()
		current.LastObservedAt = now
	}

	updated := current
	if previous != nil && previous.Signature == current.Signature && previous.LCMStateRaw == current.LCMStateRaw {
		updated.FirstObservedAt = previous.FirstObservedAt
		updated.LastChangedAt = previous.LastChangedAt
	} else if previous != nil && !previous.FirstObservedAt.IsZero() {
		updated.FirstObservedAt = previous.FirstObservedAt
		updated.LastChangedAt = now
	}
	if updated.FirstObservedAt.IsZero() {
		updated.FirstObservedAt = now
	}
	if updated.LastChangedAt.IsZero() {
		updated.LastChangedAt = now
	}

	diagnosis := HotplugDiagnosis{
		Node:                 updated.Node,
		VMID:                 updated.VMID,
		VMName:               updated.VMName,
		State:                updated.State,
		LCMState:             updated.LCMState,
		Operation:            updated.Operation,
		VolumeHandle:         updated.VolumeHandle,
		PersistentDiskID:     updated.PersistentDiskID,
		VMDiskID:             updated.VMDiskID,
		DiskTarget:           updated.DiskTarget,
		DiskAttachFlag:       updated.DiskAttachFlag,
		FirstObservedAt:      updated.FirstObservedAt,
		LastObservedAt:       updated.LastObservedAt,
		LastChangedAt:        updated.LastChangedAt,
		AgeSeconds:           int64(now.Sub(updated.FirstObservedAt).Seconds()),
		StuckAfterSeconds:    int64(cfg.StuckAfter.Seconds()),
		ObservationSignature: updated.Signature,
	}

	switch {
	case updated.LCMStateRaw == int(vmSchema.Running):
		diagnosis.Classification = HotplugClassificationReady
		diagnosis.Reason = "vm_running"
		diagnosis.Message = "OpenNebula VM is running and available for hotplug operations"
	case updated.LCMStateRaw == int(vmSchema.Hotplug):
		unchangedFor := now.Sub(updated.LastChangedAt)
		age := now.Sub(updated.FirstObservedAt)
		if age >= cfg.StuckAfter && unchangedFor >= cfg.ProgressWindow {
			diagnosis.Classification = HotplugClassificationStuck
			diagnosis.Reason = "hotplug_signature_unchanged"
			diagnosis.Message = fmt.Sprintf("OpenNebula VM has remained in HOTPLUG for %s without observable disk-state progress", age.Round(time.Second))
			diagnosis.RecommendedAction = "Inspect the OpenNebula VM and complete or recover the stuck hotplug operation before retrying CSI attachment work."
		} else {
			diagnosis.Classification = HotplugClassificationProgressing
			diagnosis.Reason = "hotplug_recent_or_changing"
			diagnosis.Message = fmt.Sprintf("OpenNebula VM is in HOTPLUG for %s and is still within the configured progress window", age.Round(time.Second))
		}
	default:
		diagnosis.Classification = HotplugClassificationUnknown
		diagnosis.Reason = "vm_lcm_not_running"
		diagnosis.Message = fmt.Sprintf("OpenNebula VM LCM state is %s", firstNonEmptyString(updated.LCMState, "UNKNOWN"))
	}

	return diagnosis, updated
}

func observationFromDisk(node string, disk shared.Disk) HotplugObservation {
	obs := HotplugObservation{Node: node, Operation: "unknown"}
	if value, err := disk.Get("IMAGE"); err == nil {
		obs.VolumeHandle = strings.TrimSpace(value)
	}
	if value, err := disk.GetI(shared.ImageID); err == nil {
		obs.PersistentDiskID = value
	}
	if value, err := disk.GetI(shared.DiskID); err == nil {
		obs.VMDiskID = value
	} else if raw, rawErr := disk.Get(shared.DiskID); rawErr == nil {
		if parsed, convErr := strconv.Atoi(raw); convErr == nil {
			obs.VMDiskID = parsed
		}
	}
	if value, err := disk.Get("TARGET"); err == nil {
		obs.DiskTarget = strings.TrimSpace(value)
	}
	if value, err := disk.Get("ATTACH"); err == nil {
		obs.DiskAttachFlag = strings.TrimSpace(value)
	}
	return obs
}

func diskObservationSignature(obs HotplugObservation) string {
	parts := []string{
		strconv.Itoa(obs.PersistentDiskID),
		strconv.Itoa(obs.VMDiskID),
		obs.VolumeHandle,
		obs.DiskTarget,
		obs.DiskAttachFlag,
	}
	signature := strings.Join(parts, ":")
	if strings.Trim(signature, ":0") == "" {
		return ""
	}
	return signature
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
