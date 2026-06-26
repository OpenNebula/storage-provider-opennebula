package controller

import (
	"strings"
	"testing"
	"time"

	datastoreSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/datastore"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	inventoryv1alpha1 "github.com/SparkAIUR/storage-provider-opennebula/pkg/inventory/apis/storageprovider/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildDatastoreStatusEnabled(t *testing.T) {
	s := &Syncer{}
	item := inventoryv1alpha1.OpenNebulaDatastore{
		ObjectMeta: metav1.ObjectMeta{Name: "ds-111", Generation: 3},
		Spec: inventoryv1alpha1.OpenNebulaDatastoreSpec{
			DisplayName: "lvm_local_image",
			Enabled:     true,
			Discovery: inventoryv1alpha1.OpenNebulaDatastoreDiscoverySpec{
				OpenNebulaDatastoreID: 111,
				ExpectedBackend:       "local",
				Allowed:               true,
			},
		},
	}
	ds := datastoreSchema.Datastore{
		ID:      111,
		Name:    "lvm_local_image",
		Type:    "IMAGE",
		TMMad:   "fs_lvm_ssh",
		DSMad:   "fs",
		TotalMB: 20 * 1024 * 1024,
		FreeMB:  12 * 1024 * 1024,
	}
	scs := []storagev1.StorageClass{{
		ObjectMeta: metav1.ObjectMeta{Name: "opennebula-default-rwo"},
		Parameters: map[string]string{"datastoreIDs": "111"},
	}}

	status := s.buildDatastoreStatus(item, ds, nil, nil, scs, nil, nil)
	if status.Phase != inventoryv1alpha1.DatastorePhaseEnabled {
		t.Fatalf("expected phase %q, got %q", inventoryv1alpha1.DatastorePhaseEnabled, status.Phase)
	}
	if status.Health != inventoryv1alpha1.DatastoreHealthHealthy {
		t.Fatalf("expected health %q, got %q", inventoryv1alpha1.DatastoreHealthHealthy, status.Health)
	}
	if status.Name != "lvm_local_image" {
		t.Fatalf("expected name to be datastore name, got %q", status.Name)
	}
	if status.Type != "IMAGE" {
		t.Fatalf("expected type IMAGE, got %q", status.Type)
	}
	if status.Backend != "local" {
		t.Fatalf("expected backend local, got %q", status.Backend)
	}
	if status.StorageClassesDisplay != "opennebula-default-rwo" {
		t.Fatalf("unexpected storageClassesDisplay: %q", status.StorageClassesDisplay)
	}
	if status.CapacityDisplay != "12 TB / 20 TB (40%)" {
		t.Fatalf("unexpected capacityDisplay: %q", status.CapacityDisplay)
	}
}

func TestBuildDatastoreStatusAvailableDisabledAndUnavailable(t *testing.T) {
	s := &Syncer{}
	base := inventoryv1alpha1.OpenNebulaDatastore{
		ObjectMeta: metav1.ObjectMeta{Name: "ds-100", Generation: 1},
		Spec: inventoryv1alpha1.OpenNebulaDatastoreSpec{
			Enabled: true,
			Discovery: inventoryv1alpha1.OpenNebulaDatastoreDiscoverySpec{
				OpenNebulaDatastoreID: 100,
				ExpectedBackend:       "local",
				Allowed:               true,
			},
		},
	}
	ds := datastoreSchema.Datastore{
		ID:      100,
		Name:    "default",
		Type:    "IMAGE",
		TMMad:   "ssh",
		TotalMB: 3 * 1024 * 1024,
		FreeMB:  3 * 1024 * 1024,
	}

	available := s.buildDatastoreStatus(base, ds, nil, nil, nil, nil, nil)
	if available.Phase != inventoryv1alpha1.DatastorePhaseAvailable {
		t.Fatalf("expected available phase, got %q", available.Phase)
	}

	disabledItem := base.DeepCopy()
	disabledItem.Spec.Enabled = false
	disabled := s.buildDatastoreStatus(*disabledItem, ds, nil, nil, []storagev1.StorageClass{{
		ObjectMeta: metav1.ObjectMeta{Name: "sc"},
		Parameters: map[string]string{"datastoreIDs": "100"},
	}}, nil, nil)
	if disabled.Phase != inventoryv1alpha1.DatastorePhaseDisabled {
		t.Fatalf("expected disabled phase, got %q", disabled.Phase)
	}

	maintenanceItem := base.DeepCopy()
	maintenanceItem.Spec.MaintenanceMode = true
	maintenanceItem.Spec.MaintenanceMessage = "operator drain"
	maintenance := s.buildDatastoreStatus(*maintenanceItem, ds, nil, nil, []storagev1.StorageClass{{
		ObjectMeta: metav1.ObjectMeta{Name: "sc"},
		Parameters: map[string]string{"datastoreIDs": "100"},
	}}, nil, nil)
	if maintenance.Phase != inventoryv1alpha1.DatastorePhaseDisabled {
		t.Fatalf("expected maintenance datastore to be disabled, got %q", maintenance.Phase)
	}
	if !maintenance.ReferencedByStorageClass || maintenance.ReferenceCount != 1 {
		t.Fatalf("expected maintenance datastore reference count to be tracked, got referenced=%t count=%d", maintenance.ReferencedByStorageClass, maintenance.ReferenceCount)
	}

	unavailableItem := base.DeepCopy()
	unavailableItem.Spec.Discovery.ExpectedBackend = "ceph-rbd"
	unavailable := s.buildDatastoreStatus(*unavailableItem, ds, nil, nil, nil, nil, nil)
	if unavailable.Phase != inventoryv1alpha1.DatastorePhaseUnavailable {
		t.Fatalf("expected unavailable phase, got %q", unavailable.Phase)
	}
	if unavailable.Health != inventoryv1alpha1.DatastoreHealthBackendMismatch {
		t.Fatalf("expected backend mismatch health, got %q", unavailable.Health)
	}
}

func TestBuildDatastoreStatusStorageClassDetails(t *testing.T) {
	s := &Syncer{}
	item := inventoryv1alpha1.OpenNebulaDatastore{
		ObjectMeta: metav1.ObjectMeta{Name: "ds-111", Generation: 1},
		Spec: inventoryv1alpha1.OpenNebulaDatastoreSpec{
			Enabled: true,
			Validation: inventoryv1alpha1.OpenNebulaDatastoreValidationSpec{
				Mode: inventoryv1alpha1.DatastoreValidationModeManual,
			},
			Discovery: inventoryv1alpha1.OpenNebulaDatastoreDiscoverySpec{
				OpenNebulaDatastoreID: 111,
				ExpectedBackend:       "local",
				Allowed:               true,
			},
		},
	}
	ds := datastoreSchema.Datastore{
		ID:      111,
		Name:    "lvm_local_image",
		Type:    "IMAGE",
		TMMad:   "fs_lvm_ssh",
		DSMad:   "fs",
		TotalMB: 1024,
		FreeMB:  512,
	}
	immediate := storagev1.VolumeBindingImmediate
	status := s.buildDatastoreStatus(item, ds, nil, nil, []storagev1.StorageClass{{
		ObjectMeta:           metav1.ObjectMeta{Name: "opennebula-local"},
		Parameters:           map[string]string{"datastoreIDs": "111"},
		VolumeBindingMode:    &immediate,
		AllowVolumeExpansion: boolPtr(false),
	}}, nil, nil)
	if !status.ValidationEligible {
		t.Fatal("expected validation to be eligible for local datastore")
	}
	if len(status.StorageClassDetails) != 1 {
		t.Fatalf("expected one storage class detail, got %d", len(status.StorageClassDetails))
	}
	if len(status.StorageClassDetails[0].Warnings) < 2 {
		t.Fatalf("expected storage class warnings to be populated, got %#v", status.StorageClassDetails[0].Warnings)
	}
}

func TestBuildStorageClassDetailsDetectsBackendMismatch(t *testing.T) {
	ds := opennebula.Datastore{ID: 111, Name: "local-a", Backend: "local"}
	details := buildStorageClassDetails([]storagev1.StorageClass{{
		ObjectMeta: metav1.ObjectMeta{Name: "cephfs-on-local"},
		Parameters: map[string]string{
			"datastoreIDs":                   "111",
			"sharedFilesystemSubvolumeGroup": "csi",
		},
	}}, ds)

	if len(details) != 1 {
		t.Fatalf("expected one storage class detail, got %d", len(details))
	}
	if details[0].BackendCompatible {
		t.Fatalf("expected backend compatibility to be false, got %#v", details[0])
	}
	if len(details[0].Warnings) == 0 || details[0].Warnings[len(details[0].Warnings)-1] != "storage class backend does not match datastore backend" {
		t.Fatalf("expected backend mismatch warning, got %#v", details[0].Warnings)
	}
}

func TestBuildStorageClassDetailsCephFSUsesStandardSecretRefs(t *testing.T) {
	ds := opennebula.Datastore{ID: 300, Name: "cephfs-a", Backend: "cephfs"}
	allowExpansion := true
	details := buildStorageClassDetails([]storagev1.StorageClass{{
		ObjectMeta: metav1.ObjectMeta{Name: "cephfs"},
		Parameters: map[string]string{
			"datastoreIDs":                                          "300",
			"sharedFilesystemSubvolumeGroup":                        "csi",
			"csi.storage.k8s.io/provisioner-secret-name":            "cephfs-provisioner",
			"csi.storage.k8s.io/provisioner-secret-namespace":       "kube-system",
			"csi.storage.k8s.io/node-stage-secret-name":             "cephfs-node-stage",
			"csi.storage.k8s.io/node-stage-secret-namespace":        "kube-system",
			"csi.storage.k8s.io/controller-expand-secret-name":      "cephfs-provisioner",
			"csi.storage.k8s.io/controller-expand-secret-namespace": "kube-system",
		},
		AllowVolumeExpansion: &allowExpansion,
	}}, ds)

	if len(details) != 1 {
		t.Fatalf("expected one storage class detail, got %d", len(details))
	}
	for _, warning := range details[0].Warnings {
		if strings.Contains(warning, "secret references are incomplete") {
			t.Fatalf("expected complete standard secret refs to suppress cephfs warnings, got %#v", details[0].Warnings)
		}
	}
}

func TestNormalizeDatastoreTypeAndBackend(t *testing.T) {
	ds := datastoreSchema.Datastore{
		ID:      104,
		Name:    "one-csi-cephfs",
		Type:    "2",
		TMMad:   "ssh",
		DSMad:   "fs",
		TotalMB: 1024,
		FreeMB:  512,
	}
	ds.Template.AddPair(sharedBackendAttr, "cephfs")

	status := (&Syncer{}).buildDatastoreStatus(
		inventoryv1alpha1.OpenNebulaDatastore{
			ObjectMeta: metav1.ObjectMeta{Name: "ds-104", Generation: 1},
			Spec: inventoryv1alpha1.OpenNebulaDatastoreSpec{
				Enabled: true,
				Discovery: inventoryv1alpha1.OpenNebulaDatastoreDiscoverySpec{
					OpenNebulaDatastoreID: 104,
					ExpectedBackend:       "cephfs",
					Allowed:               true,
				},
			},
		},
		ds,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	if status.Type != string(datastoreSchema.File) {
		t.Fatalf("expected datastore type %q, got %q", datastoreSchema.File, status.Type)
	}
	if status.Backend != "cephfs" {
		t.Fatalf("expected datastore backend cephfs, got %q", status.Backend)
	}
}

func TestValidationMetricsDisplayAndCapacityFallback(t *testing.T) {
	read := int64(12000)
	write := int64(8000)
	p99 := int64(4000)
	status := inventoryv1alpha1.OpenNebulaDatastoreValidationStatus{
		Phase: inventoryv1alpha1.ValidationPhaseSucceeded,
		Result: inventoryv1alpha1.ValidationResult{
			ReadIops:         &read,
			WriteIops:        &write,
			LatencyP99Micros: &p99,
		},
	}
	if got := validationMetricsDisplay(status); got != "R:12k W:8k p99:4ms" {
		t.Fatalf("unexpected metrics display: %q", got)
	}
	if got := validationMetricsDisplay(inventoryv1alpha1.OpenNebulaDatastoreValidationStatus{}); got != "-" {
		t.Fatalf("expected empty validation display to be '-', got %q", got)
	}
	if got := formatCapacityDisplay(0, 0); got != unknownCapacityDisplay {
		t.Fatalf("expected unknown capacity display, got %q", got)
	}
	if got := formatCapacityDisplay(93*1024*1024*1024, 3*1024*1024*1024*1024); got != "93 GB / 3 TB (97%)" {
		t.Fatalf("unexpected capacity display for 93GB/3TB: %q", got)
	}
}

func TestBuildDatastoreStatusPrefersLatestBenchmarkMetrics(t *testing.T) {
	s := &Syncer{}
	item := inventoryv1alpha1.OpenNebulaDatastore{
		ObjectMeta: metav1.ObjectMeta{Name: "ds-111", Generation: 1},
		Spec: inventoryv1alpha1.OpenNebulaDatastoreSpec{
			Enabled: true,
			Discovery: inventoryv1alpha1.OpenNebulaDatastoreDiscoverySpec{
				OpenNebulaDatastoreID: 111,
				ExpectedBackend:       "local",
				Allowed:               true,
			},
			Validation: inventoryv1alpha1.OpenNebulaDatastoreValidationSpec{
				Mode: inventoryv1alpha1.DatastoreValidationModeManual,
			},
		},
		Status: inventoryv1alpha1.OpenNebulaDatastoreStatus{
			Validation: inventoryv1alpha1.OpenNebulaDatastoreValidationStatus{
				Phase: inventoryv1alpha1.ValidationPhaseSucceeded,
				Result: inventoryv1alpha1.ValidationResult{
					ReadIops: int64PtrTest(1000),
				},
			},
		},
	}
	ds := datastoreSchema.Datastore{ID: 111, Name: "default", Type: "IMAGE", TMMad: "ssh", TotalMB: 1024, FreeMB: 512}
	benchmark := &latestBenchmarkResult{
		RunName: "ds-111-20260326",
		Status: inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunStatus{
			Phase: inventoryv1alpha1.ValidationPhaseSucceeded,
			Result: inventoryv1alpha1.ValidationResult{
				ReadIops:         int64PtrTest(12000),
				WriteIops:        int64PtrTest(8000),
				LatencyP99Micros: int64PtrTest(4000),
			},
		},
	}
	status := s.buildDatastoreStatus(item, ds, nil, nil, nil, nil, benchmark)
	if status.MetricsDisplay != "R:12k W:8k p99:4ms" {
		t.Fatalf("expected benchmark metrics display to win, got %q", status.MetricsDisplay)
	}
}

func TestBenchmarkHelpers(t *testing.T) {
	scs := []storagev1.StorageClass{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Parameters: map[string]string{"datastoreIDs": "111"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Parameters: map[string]string{"datastoreIDs": "111"}},
	}
	if got := defaultBenchmarkStorageClassName(scs, 111); got != "a" {
		t.Fatalf("expected sorted default storage class, got %q", got)
	}
	if got := benchmarkRunObjectName(111, time.Date(2026, time.March, 26, 23, 30, 45, 0, time.UTC)); got != "ds-111-20260326-233045" {
		t.Fatalf("unexpected benchmark object name: %q", got)
	}
	run := inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun{
		ObjectMeta: metav1.ObjectMeta{Name: "ds-111-20260326", Generation: 7},
		Spec:       inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunSpec{DatastoreID: 111},
	}
	if got := benchmarkResourceName(benchmarkJobPrefix, run); got == benchmarkResourceName(benchmarkJobPrefix, inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun{
		ObjectMeta: metav1.ObjectMeta{Name: "ds-111-20260326", Generation: 8},
		Spec:       inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunSpec{DatastoreID: 111},
	}) {
		t.Fatalf("expected benchmark resource names to vary by generation")
	}
}

func TestResolveBenchmarkRunDefaultsCephFSAccessModesAndDeadline(t *testing.T) {
	s := &Syncer{defaultValidationImage: "ghcr.io/sparkaiur/fio:latest"}
	item := inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun{
		ObjectMeta: metav1.ObjectMeta{Name: "bench", Generation: 2},
		Spec: inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunSpec{
			DatastoreID: 300,
		},
	}
	ds := datastoreSchema.Datastore{ID: 300, Name: "cephfs-a", Type: "FILE", TMMad: "shared"}
	ds.Template.AddPair(sharedBackendAttr, "cephfs")

	resolved := s.resolveBenchmarkRun(item, ds, nil)
	if len(resolved.Spec.AccessModes) != 1 || resolved.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Fatalf("expected cephfs default benchmark access mode RWX, got %#v", resolved.Spec.AccessModes)
	}
	if resolved.Spec.ActiveDeadlineSeconds == nil || *resolved.Spec.ActiveDeadlineSeconds != defaultValidationActiveDeadlineSeconds {
		t.Fatalf("expected default benchmark activeDeadlineSeconds=%d, got %#v", defaultValidationActiveDeadlineSeconds, resolved.Spec.ActiveDeadlineSeconds)
	}
	if resolved.Spec.Image != "ghcr.io/sparkaiur/fio:latest" {
		t.Fatalf("expected default validation image to be injected, got %q", resolved.Spec.Image)
	}
}

func TestResolveBenchmarkRunAppliesRWXContentionProfile(t *testing.T) {
	s := &Syncer{}
	item := inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun{
		ObjectMeta: metav1.ObjectMeta{Name: "bench", Generation: 2},
		Spec: inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunSpec{
			DatastoreID: 300,
			Profile:     "rwx-contention",
		},
	}
	ds := datastoreSchema.Datastore{ID: 300, Name: "cephfs-a", Type: "FILE", TMMad: "shared"}
	ds.Template.AddPair(sharedBackendAttr, "cephfs")

	resolved := s.resolveBenchmarkRun(item, ds, nil)
	if len(resolved.Spec.AccessModes) != 1 || resolved.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Fatalf("expected RWX profile access mode, got %#v", resolved.Spec.AccessModes)
	}
	if strings.TrimSpace(resolved.Spec.Size) == "" || len(resolved.Spec.FioArgs) == 0 {
		t.Fatalf("expected profile defaults to populate size and fio args, got size=%q args=%#v", resolved.Spec.Size, resolved.Spec.FioArgs)
	}
}

func TestBenchmarkThresholdFailure(t *testing.T) {
	readIops := int64(10)
	minReadIops := int64(20)
	message := benchmarkThresholdFailure(inventoryv1alpha1.ValidationResult{ReadIops: &readIops}, inventoryv1alpha1.BenchmarkThresholds{MinReadIops: &minReadIops})
	if !strings.Contains(message, "read IOPS below threshold") {
		t.Fatalf("expected read threshold failure, got %q", message)
	}
}

func TestBuildBenchmarkPVCRejectsUnsupportedAccessMode(t *testing.T) {
	s := &Syncer{}
	run := &inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun{
		Spec: inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{"ReadWriteOncePod"},
		},
	}
	ds := datastoreSchema.Datastore{ID: 111, Name: "local-a", Type: "IMAGE", TMMad: "ssh"}

	if _, err := s.buildBenchmarkPVC(run, ds, "pvc"); err == nil {
		t.Fatal("expected unsupported access mode to fail")
	}
}

func TestBuildValidationCommandMergesPartialOverrides(t *testing.T) {
	command := buildValidationCommand([]string{
		"--name=smoke",
		"--iodepth=8",
		"--runtime=15",
		"--time_based=1",
	})

	for _, expected := range []string{
		"fio",
		"--name=smoke",
		"--filename=/data/validation.bin",
		"--rw=randrw",
		"--bs=4k",
		"--size=256Mi",
		"--iodepth=8",
		"--runtime=15",
		"--time_based=1",
		"--output-format=json",
	} {
		if !strings.Contains(command, expected) {
			t.Fatalf("expected command %q to contain %q", command, expected)
		}
	}
	for _, unexpected := range []string{
		"--name=validate",
		"--iodepth=16",
		"--runtime=30",
		" --time_based ",
	} {
		if strings.Contains(command, unexpected) {
			t.Fatalf("expected command %q not to contain %q", command, unexpected)
		}
	}
}

func TestBuildValidationCommandSupportsSeparateValueOverrides(t *testing.T) {
	command := buildValidationCommand([]string{
		"--size", "1Gi",
		"--filename", "/data/custom.bin",
		"--name", "bench",
	})

	for _, expected := range []string{
		"--size 1Gi",
		"--filename /data/custom.bin",
		"--name bench",
	} {
		if !strings.Contains(command, expected) {
			t.Fatalf("expected command %q to contain %q", command, expected)
		}
	}
	for _, unexpected := range []string{
		"--size=256Mi",
		"--filename=/data/validation.bin",
		"--name=validate",
	} {
		if strings.Contains(command, unexpected) {
			t.Fatalf("expected command %q not to contain %q", command, unexpected)
		}
	}
}

func TestParseFIOResultSkipsLeadingNoise(t *testing.T) {
	payload := []byte("note: queue depth will be capped at 1\n{\"jobs\":[{\"read\":{\"iops\":12000,\"bw_bytes\":4096,\"clat_ns\":{\"percentile\":{\"50.000000\":4000000,\"99.000000\":5000000}}},\"write\":{\"iops\":8000,\"bw_bytes\":2048,\"clat_ns\":{\"percentile\":{\"99.000000\":6000000}}}}]}")
	result, err := parseFIOResult(payload)
	if err != nil {
		t.Fatalf("expected fio payload with warning prefix to parse, got %v", err)
	}
	if result.ReadIops == nil || *result.ReadIops != 12000 {
		t.Fatalf("unexpected read iops: %#v", result.ReadIops)
	}
	if result.WriteIops == nil || *result.WriteIops != 8000 {
		t.Fatalf("unexpected write iops: %#v", result.WriteIops)
	}
	if result.LatencyP99Micros == nil || *result.LatencyP99Micros != 5000 {
		t.Fatalf("unexpected p99 latency: %#v", result.LatencyP99Micros)
	}
}

func TestBuildDatastoreStatusCountsPVCsAndClaims(t *testing.T) {
	s := &Syncer{}
	item := inventoryv1alpha1.OpenNebulaDatastore{
		ObjectMeta: metav1.ObjectMeta{Name: "ds-111", Generation: 1},
		Spec: inventoryv1alpha1.OpenNebulaDatastoreSpec{
			Enabled: true,
			Discovery: inventoryv1alpha1.OpenNebulaDatastoreDiscoverySpec{
				OpenNebulaDatastoreID: 111,
				Allowed:               true,
			},
		},
	}
	ds := datastoreSchema.Datastore{
		ID:      111,
		Name:    "lvm_local_image",
		Type:    "IMAGE",
		TMMad:   "ssh",
		TotalMB: 1024,
		FreeMB:  512,
	}
	pv := corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pv-a",
			Annotations: map[string]string{annotationDatastoreID: "111"},
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: "sc-a",
			Capacity:         corev1.ResourceList{corev1.ResourceStorage: resourceMustParse("2Gi")},
			ClaimRef: &corev1.ObjectReference{
				Namespace: "db",
				Name:      "claim-a",
				UID:       "uid-a",
			},
		},
	}
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "claim-a"},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resourceMustParse("1Gi")},
			},
		},
	}

	status := s.buildDatastoreStatus(item, ds, []corev1.PersistentVolume{pv}, map[string]corev1.PersistentVolumeClaim{"db/claim-a": pvc}, nil, nil, nil)
	if status.Usage.BoundPVCCount != 1 {
		t.Fatalf("expected 1 bound PVC, got %d", status.Usage.BoundPVCCount)
	}
	if status.Capacity.ProvisionedPVBytes == 0 || status.Capacity.RequestedPVCBytes == 0 {
		t.Fatalf("expected non-zero provisioned/requested bytes, got pv=%d pvc=%d", status.Capacity.ProvisionedPVBytes, status.Capacity.RequestedPVCBytes)
	}
	if len(status.Usage.BoundClaims) != 1 || status.Usage.BoundClaims[0].Name != "claim-a" {
		t.Fatalf("unexpected bound claims: %#v", status.Usage.BoundClaims)
	}
}

func TestNodeDisplayState(t *testing.T) {
	if got := nodeDisplayState(true, false, true); got != "Ready" {
		t.Fatalf("expected Ready, got %q", got)
	}
	if got := nodeDisplayState(true, true, false); got != "HotplugCooldown" {
		t.Fatalf("expected HotplugCooldown, got %q", got)
	}
	if got := nodeDisplayState(true, false, false); got != "VMNotReady" {
		t.Fatalf("expected VMNotReady, got %q", got)
	}
}

func TestDatastoreNameMapHandlesNilPool(t *testing.T) {
	if got := datastoreNameMap(nil); len(got) != 0 {
		t.Fatalf("expected empty map for nil pool, got %#v", got)
	}

	pool := &datastoreSchema.Pool{
		Datastores: []datastoreSchema.Datastore{
			{ID: 1, Name: "default"},
			{ID: 100, Name: "lvm_local_image"},
		},
	}
	got := datastoreNameMap(pool)
	if got[1] != "default" || got[100] != "lvm_local_image" {
		t.Fatalf("unexpected datastore name map: %#v", got)
	}
}

func boolPtr(value bool) *bool {
	return &value
}

func int64PtrTest(value int64) *int64 {
	return &value
}
