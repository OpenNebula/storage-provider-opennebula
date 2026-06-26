package driver

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	datastoreSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/datastore"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/stretchr/testify/assert"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	testingexec "k8s.io/utils/exec/testing"
)

func TestParseSecretRefsCSV(t *testing.T) {
	refs, err := ParseSecretRefsCSV("storage/cephfs-node,omni/cephfs-admin")
	assert.NoError(t, err)
	assert.Equal(t, []SecretRef{
		{Namespace: "storage", Name: "cephfs-node"},
		{Namespace: "omni", Name: "cephfs-admin"},
	}, refs)
}

func TestParseSecretRefsCSVRejectsInvalidValue(t *testing.T) {
	_, err := ParseSecretRefsCSV("missing-namespace")
	assert.Error(t, err)
}

func TestRunPreflightCommandOutputsJSONFailure(t *testing.T) {
	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.OpenNebulaRPCEndpointVar, "")
	cfg.OverrideVal(config.OpenNebulaCredentialsVar, "")

	var buf bytes.Buffer
	err := RunPreflightCommand(context.Background(), cfg, nil, PreflightOptions{}, "json", &buf)
	assert.Error(t, err)
	assert.Contains(t, buf.String(), "\"passed\": false")
	assert.Contains(t, buf.String(), "\"check\": \"opennebula_auth\"")
}

func TestRunPreflightCommandOutputsText(t *testing.T) {
	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.OpenNebulaRPCEndpointVar, "")
	cfg.OverrideVal(config.OpenNebulaCredentialsVar, "")

	var buf bytes.Buffer
	err := RunPreflightCommand(context.Background(), cfg, nil, PreflightOptions{}, "text", &buf)
	assert.Error(t, err)
	assert.True(t, strings.Contains(buf.String(), "[FAIL] opennebula_auth"))
}

func TestCheckLocalImmediateBindingWarnsForLocalImmediate(t *testing.T) {
	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.PreflightLocalImmediateBindingPolicyVar, "warn")
	client := fake.NewSimpleClientset(&storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "local-immediate"},
		Provisioner: DefaultDriverName,
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
	})

	runner := NewPreflightRunner(cfg, &testingexec.FakeExec{
		LookPathFunc: func(path string) (string, error) {
			return path, nil
		},
	}, PreflightOptions{})
	outcome, message := runner.checkLocalImmediateBinding(context.Background(), client, []datastoreSchema.Datastore{
		{ID: 100, Name: "local-ds", DSMad: "fs", TMMad: "local", StateRaw: 0, Type: "IMAGE"},
	})

	assert.Equal(t, "warn", outcome)
	assert.Contains(t, message, "local-immediate")
}

func TestCheckLocalImmediateBindingFailsWhenPolicyRequiresIt(t *testing.T) {
	mode := storagev1.VolumeBindingImmediate
	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.PreflightLocalImmediateBindingPolicyVar, "fail")
	client := fake.NewSimpleClientset(&storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "local-immediate"},
		Provisioner: DefaultDriverName,
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
		VolumeBindingMode: &mode,
	})

	runner := NewPreflightRunner(cfg, nil, PreflightOptions{})
	outcome, message := runner.checkLocalImmediateBinding(context.Background(), client, []datastoreSchema.Datastore{
		{ID: 100, Name: "local-ds", DSMad: "fs", TMMad: "local", StateRaw: 0, Type: "IMAGE"},
	})

	assert.Equal(t, "fail", outcome)
	assert.Contains(t, message, "Immediate")
}

func TestCheckLocalImmediateBindingPassesForWaitForFirstConsumer(t *testing.T) {
	mode := storagev1.VolumeBindingWaitForFirstConsumer
	cfg := config.LoadConfiguration()
	client := fake.NewSimpleClientset(&storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "local-wffc"},
		Provisioner: DefaultDriverName,
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
		VolumeBindingMode: &mode,
	})

	runner := NewPreflightRunner(cfg, nil, PreflightOptions{})
	outcome, message := runner.checkLocalImmediateBinding(context.Background(), client, []datastoreSchema.Datastore{
		{ID: 100, Name: "local-ds", DSMad: "fs", TMMad: "local", StateRaw: 0, Type: "IMAGE"},
	})

	assert.Equal(t, "pass", outcome)
	assert.Contains(t, message, "no local-backed StorageClasses")
}

func TestInspectStorageClassesIncludesWarnings(t *testing.T) {
	cfg := config.LoadConfiguration()
	immediate := storagev1.VolumeBindingImmediate
	client := fake.NewSimpleClientset(&storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: "local-immediate"},
		Provisioner:          DefaultDriverName,
		Parameters:           map[string]string{storageClassParamDatastoreIDs: "100"},
		VolumeBindingMode:    &immediate,
		AllowVolumeExpansion: boolPtrPreflight(false),
	})

	runner := NewPreflightRunner(cfg, nil, PreflightOptions{})
	summaries := runner.inspectStorageClasses(context.Background(), client, []datastoreSchema.Datastore{
		{ID: 100, Name: "local-ds", DSMad: "fs", TMMad: "local", StateRaw: 0, Type: "IMAGE"},
	})

	assert.Len(t, summaries, 1)
	assert.Equal(t, "local", summaries[0].Backend)
	assert.NotEmpty(t, summaries[0].Warnings)
}

func TestInspectStorageClassesWarnsOnMixedBackends(t *testing.T) {
	cfg := config.LoadConfiguration()
	client := fake.NewSimpleClientset(&storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "mixed"},
		Provisioner: DefaultDriverName,
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100,300",
		},
	})

	runner := NewPreflightRunner(cfg, nil, PreflightOptions{})
	summaries := runner.inspectStorageClasses(context.Background(), client, []datastoreSchema.Datastore{
		newPreflightDatastore(100, "local-a", "fs", "local", "IMAGE", nil),
		newPreflightDatastore(300, "cephfs-a", "fs", "shared", "FILE", map[string]string{
			"SPARKAI_CSI_SHARE_BACKEND":          "cephfs",
			"SPARKAI_CSI_CEPHFS_FS_NAME":         "cephfs-prod",
			"SPARKAI_CSI_CEPHFS_ROOT_PATH":       "/kubernetes",
			"SPARKAI_CSI_CEPHFS_SUBVOLUME_GROUP": "csi",
			"CEPH_HOST":                          "mon1,mon2",
		}),
	})

	assert.Len(t, summaries, 1)
	assert.Equal(t, "mixed", summaries[0].Backend)
	assert.Contains(t, summaries[0].Warnings, "storage class mixes CephFS and disk datastores; use separate StorageClasses per backend")
}

func TestInspectStorageClassesUsesStandardCephFSSecretRefs(t *testing.T) {
	cfg := config.LoadConfiguration()
	allowExpansion := true
	client := fake.NewSimpleClientset(&storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "cephfs"},
		Provisioner: DefaultDriverName,
		Parameters: map[string]string{
			storageClassParamDatastoreIDs:                           "300",
			"sharedFilesystemSubvolumeGroup":                        "csi",
			"csi.storage.k8s.io/provisioner-secret-name":            "cephfs-provisioner",
			"csi.storage.k8s.io/provisioner-secret-namespace":       "kube-system",
			"csi.storage.k8s.io/node-stage-secret-name":             "cephfs-node-stage",
			"csi.storage.k8s.io/node-stage-secret-namespace":        "kube-system",
			"csi.storage.k8s.io/controller-expand-secret-name":      "cephfs-provisioner",
			"csi.storage.k8s.io/controller-expand-secret-namespace": "kube-system",
		},
		AllowVolumeExpansion: &allowExpansion,
	})

	runner := NewPreflightRunner(cfg, nil, PreflightOptions{})
	summaries := runner.inspectStorageClasses(context.Background(), client, []datastoreSchema.Datastore{
		newPreflightDatastore(300, "cephfs-a", "fs", "shared", "FILE", map[string]string{
			"SPARKAI_CSI_SHARE_BACKEND":          "cephfs",
			"SPARKAI_CSI_CEPHFS_FS_NAME":         "cephfs-prod",
			"SPARKAI_CSI_CEPHFS_ROOT_PATH":       "/kubernetes",
			"SPARKAI_CSI_CEPHFS_SUBVOLUME_GROUP": "csi",
			"CEPH_HOST":                          "mon1,mon2",
		}),
	})

	assert.Len(t, summaries, 1)
	assert.Equal(t, "cephfs", summaries[0].Backend)
	assert.NotContains(t, summaries[0].Warnings, "cephfs provisioner secret refs incomplete")
	assert.NotContains(t, summaries[0].Warnings, "cephfs node-stage secret refs incomplete")
	assert.NotContains(t, summaries[0].Warnings, "cephfs controller-expand secret refs incomplete")
}

func TestInspectStorageClassesWarnsWhenKernelCephFSMounterIsUnsupported(t *testing.T) {
	cfg := config.LoadConfiguration()
	client := fake.NewSimpleClientset(&storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "cephfs-kernel"},
		Provisioner: DefaultDriverName,
		Parameters: map[string]string{
			storageClassParamDatastoreIDs:                     "300",
			storageClassParamCephFSMounter:                    "kernel",
			"sharedFilesystemSubvolumeGroup":                  "csi",
			"csi.storage.k8s.io/provisioner-secret-name":      "cephfs-provisioner",
			"csi.storage.k8s.io/provisioner-secret-namespace": "kube-system",
			"csi.storage.k8s.io/node-stage-secret-name":       "cephfs-node-stage",
			"csi.storage.k8s.io/node-stage-secret-namespace":  "kube-system",
		},
	})

	originalProcFilesystems := sharedFilesystemProcFilesystemsPath
	tempProc := filepath.Join(t.TempDir(), "filesystems")
	assert.NoError(t, os.WriteFile(tempProc, []byte("nodev\text4\n"), 0o644))
	sharedFilesystemProcFilesystemsPath = tempProc
	t.Cleanup(func() {
		sharedFilesystemProcFilesystemsPath = originalProcFilesystems
	})

	runner := NewPreflightRunner(cfg, nil, PreflightOptions{})
	summaries := runner.inspectStorageClasses(context.Background(), client, []datastoreSchema.Datastore{
		newPreflightDatastore(300, "cephfs-a", "fs", "shared", "FILE", map[string]string{
			"SPARKAI_CSI_SHARE_BACKEND":          "cephfs",
			"SPARKAI_CSI_CEPHFS_FS_NAME":         "cephfs-prod",
			"SPARKAI_CSI_CEPHFS_ROOT_PATH":       "/kubernetes",
			"SPARKAI_CSI_CEPHFS_SUBVOLUME_GROUP": "csi",
			"CEPH_HOST":                          "mon1,mon2",
		}),
	})

	assert.Len(t, summaries, 1)
	assert.Equal(t, "kernel", summaries[0].Mounter)
	assert.Contains(t, summaries[0].Warnings, "cephfs kernel mounts requested but feature gate cephfsKernelMounts is disabled")
	assert.NotEmpty(t, summaries[0].Warnings)
}

func boolPtrPreflight(value bool) *bool {
	return &value
}

func newPreflightDatastore(id int, name, dsMad, tmMad, datastoreType string, attrs map[string]string) datastoreSchema.Datastore {
	tpl := datastoreSchema.NewTemplate()
	for key, value := range attrs {
		tpl.AddPair(key, value)
	}

	return datastoreSchema.Datastore{
		ID:       id,
		Name:     name,
		DSMad:    dsMad,
		TMMad:    tmMad,
		Type:     datastoreType,
		StateRaw: 0,
		Template: *tpl,
	}
}
