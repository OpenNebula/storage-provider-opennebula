package opennebula

import (
	"testing"
	"time"

	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/shared"
	vmSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
)

func TestClassifyHotplugObservationReady(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	vm := &vmSchema.VM{
		ID:          127,
		Name:        "node-a",
		StateRaw:    int(vmSchema.Active),
		LCMStateRaw: int(vmSchema.Running),
	}

	diagnosis, _ := ClassifyHotplugObservation(BuildHotplugObservation("node-a", vm, func() time.Time {
		return now
	}), nil, HotplugDiagnosisConfig{StuckAfter: 5 * time.Minute, ProgressWindow: time.Minute})

	if diagnosis.Classification != HotplugClassificationReady {
		t.Fatalf("expected ready classification, got %q", diagnosis.Classification)
	}
	if diagnosis.Reason != "vm_running" {
		t.Fatalf("unexpected reason: %q", diagnosis.Reason)
	}
}

func TestClassifyHotplugObservationProgressing(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	current := hotplugObservation("node-a", now, "sig-a")

	diagnosis, _ := ClassifyHotplugObservation(current, nil, HotplugDiagnosisConfig{StuckAfter: 5 * time.Minute, ProgressWindow: time.Minute})

	if diagnosis.Classification != HotplugClassificationProgressing {
		t.Fatalf("expected progressing classification, got %q", diagnosis.Classification)
	}
}

func TestClassifyHotplugObservationStuck(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 10, 0, 0, time.UTC)
	previous := hotplugObservation("node-a", now.Add(-10*time.Minute), "sig-a")
	current := hotplugObservation("node-a", now, "sig-a")

	diagnosis, _ := ClassifyHotplugObservation(current, &previous, HotplugDiagnosisConfig{StuckAfter: 5 * time.Minute, ProgressWindow: time.Minute})

	if diagnosis.Classification != HotplugClassificationStuck {
		t.Fatalf("expected stuck classification, got %q", diagnosis.Classification)
	}
	if diagnosis.RecommendedAction == "" {
		t.Fatal("expected stuck diagnosis to include recommended action")
	}
}

func TestBuildHotplugObservationExtractsAttachDisk(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	tmpl := vmSchema.Template{}
	disk := tmpl.AddVector(shared.DiskVec)
	_ = disk.AddPair(string(shared.Image), "pvc-123")
	_ = disk.AddPair(string(shared.ImageID), 123)
	_ = disk.AddPair(string(shared.DiskID), 4)
	_ = disk.AddPair(string(shared.TargetDisk), "vdb")
	_ = disk.AddPair("ATTACH", "YES")
	vm := &vmSchema.VM{
		ID:          127,
		Name:        "node-a",
		StateRaw:    int(vmSchema.Active),
		LCMStateRaw: int(vmSchema.Hotplug),
		Template:    tmpl,
	}

	observation := BuildHotplugObservation("node-a", vm, func() time.Time {
		return now
	})

	if observation.Operation != "attach" {
		t.Fatalf("expected attach operation, got %q", observation.Operation)
	}
	if observation.VolumeHandle != "pvc-123" || observation.PersistentDiskID != 123 || observation.VMDiskID != 4 {
		t.Fatalf("unexpected disk observation: %#v", observation)
	}
	if observation.PendingDiskSignature == "" || observation.Signature == "" {
		t.Fatalf("expected disk signatures to be populated: %#v", observation)
	}
}

func hotplugObservation(node string, observedAt time.Time, signature string) HotplugObservation {
	return HotplugObservation{
		Node:            node,
		State:           "ACTIVE",
		LCMState:        "HOTPLUG",
		LCMStateRaw:     int(vmSchema.Hotplug),
		Signature:       signature,
		FirstObservedAt: observedAt,
		LastObservedAt:  observedAt,
		LastChangedAt:   observedAt,
		Operation:       "attach",
		VolumeHandle:    "pvc-123",
	}
}
