package opennebula

import (
	"testing"

	datastoreSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/datastore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveDatastoresSupportsDefaultAliasAndIDLookup(t *testing.T) {
	pool := []datastoreSchema.Datastore{
		{ID: 42, Name: "default", DSMad: "fs", TMMad: "local", StateRaw: 0, FreeMB: 512},
		{ID: 100, Name: "fast-local", DSMad: "fs", TMMad: "local", StateRaw: 0, FreeMB: 1024},
	}

	resolved, err := ResolveDatastores(pool, DatastoreSelectionConfig{
		Identifiers:  []string{"default", "100"},
		AllowedTypes: []string{"local"},
	})
	require.NoError(t, err)
	require.Len(t, resolved, 2)
	assert.Equal(t, 42, resolved[0].ID)
	assert.Equal(t, 100, resolved[1].ID)
	assert.Equal(t, "local", resolved[0].Type)
}

func TestResolveDatastoresTreatsFSLVMBackendsAsLocal(t *testing.T) {
	pool := []datastoreSchema.Datastore{
		{ID: 110, Name: "lvm-local", DSMad: "fs_lvm", TMMad: "fs_lvm", Type: "IMAGE", StateRaw: 0, FreeMB: 512},
		{ID: 111, Name: "lvm-local-ssh", DSMad: "fs_lvm_ssh", TMMad: "fs_lvm_ssh", Type: "IMAGE", StateRaw: 0, FreeMB: 1024},
	}

	resolved, err := ResolveDatastores(pool, DatastoreSelectionConfig{
		Identifiers:  []string{"110", "111"},
		AllowedTypes: []string{"local"},
	})
	require.NoError(t, err)
	require.Len(t, resolved, 2)
	assert.Equal(t, "local", resolved[0].Type)
	assert.Equal(t, "local", resolved[1].Type)
	assert.Equal(t, "local", resolved[0].Backend)
	assert.Equal(t, "local", resolved[1].Backend)
}

func TestResolveDatastoresRejectsSystemDatastoreForProvisioning(t *testing.T) {
	_, err := ResolveDatastores([]datastoreSchema.Datastore{
		{ID: 110, Name: "lvm-system", DSMad: "fs_lvm", TMMad: "fs_lvm", Type: "SYSTEM", StateRaw: 0, FreeMB: 512},
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"110"},
		AllowedTypes: []string{"local"},
	})
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
	assert.Contains(t, err.Error(), "IMAGE or FILE datastore")
}

func TestResolveDatastoresAcceptsNumericImageDatastoreCategoryFromOpenNebula(t *testing.T) {
	resolved, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTypedTestDatastore(111, "lvm-local-image", "fs", "fs_lvm_ssh", "0", map[string]string{
			"TYPE":      "IMAGE_DS",
			"DISK_TYPE": "BLOCK",
			"DRIVER":    "raw",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"111"},
		AllowedTypes: []string{"local"},
	})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, "IMAGE", resolved[0].Category)
	assert.Equal(t, "local", resolved[0].Type)
}

func TestResolveDatastoresTreatsFSWithSSHTMModeAsLocal(t *testing.T) {
	resolved, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTypedTestDatastore(100, "one-csi-local", "fs", "ssh", "0", map[string]string{
			"TYPE":      "IMAGE_DS",
			"DISK_TYPE": "FILE",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"100"},
		AllowedTypes: []string{"local"},
	})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, "local", resolved[0].Type)
	assert.Equal(t, "local", resolved[0].Backend)
	assert.Equal(t, "IMAGE", resolved[0].Category)
}

func TestResolveDatastoresRejectsNumericSystemDatastoreCategoryFromOpenNebula(t *testing.T) {
	_, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTypedTestDatastore(112, "lvm-local-system", "fs", "fs_lvm_ssh", "1", map[string]string{
			"TYPE":      "SYSTEM_DS",
			"DISK_TYPE": "BLOCK",
			"DRIVER":    "raw",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"112"},
		AllowedTypes: []string{"local"},
	})
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
	assert.Contains(t, err.Error(), `OpenNebula type "1"`)
	assert.Contains(t, err.Error(), `template TYPE="SYSTEM_DS"`)
}

func TestResolveDatastoresRejectsUnknownIdentifier(t *testing.T) {
	_, err := ResolveDatastores([]datastoreSchema.Datastore{
		{ID: 100, Name: "fast-local", DSMad: "fs", TMMad: "local", StateRaw: 0},
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"missing"},
		AllowedTypes: []string{"local"},
	})
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
}

func TestResolveDatastoresRejectsDisallowedType(t *testing.T) {
	_, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTestDatastore(200, "ceph-a", "ceph", "ceph", map[string]string{
			"DISK_TYPE":   "RBD",
			"POOL_NAME":   "one",
			"CEPH_HOST":   "mon1 mon2",
			"CEPH_USER":   "libvirt",
			"CEPH_SECRET": "secret-id",
			"BRIDGE_LIST": "frontend",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"200"},
		AllowedTypes: []string{"local"},
	})
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
}

func TestResolveDatastoresAcceptsValidCephImageDatastore(t *testing.T) {
	resolved, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTestDatastore(200, "ceph-a", "ceph", "ceph", map[string]string{
			"DISK_TYPE":   "RBD",
			"POOL_NAME":   "one",
			"CEPH_HOST":   "mon1 mon2",
			"CEPH_USER":   "libvirt",
			"CEPH_SECRET": "secret-id",
			"BRIDGE_LIST": "frontend",
			"RBD_FORMAT":  "2",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"200"},
		AllowedTypes: []string{"ceph"},
	})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, "ceph", resolved[0].Type)
	require.NotNil(t, resolved[0].Ceph)
	assert.Equal(t, "one", resolved[0].Ceph.PoolName)
	assert.Equal(t, "RBD", resolved[0].DiskType)
}

func TestResolveDatastoresAcceptsCephImageDatastoreWithNumericSchemaDiskType(t *testing.T) {
	ds := newTestDatastore(200, "ceph-a", "ceph", "ceph", map[string]string{
		"DISK_TYPE":   "RBD",
		"POOL_NAME":   "one",
		"CEPH_HOST":   "mon1 mon2",
		"CEPH_USER":   "libvirt",
		"CEPH_SECRET": "secret-id",
		"BRIDGE_LIST": "frontend",
		"RBD_FORMAT":  "2",
	})
	ds.DiskType = "3"

	resolved, err := ResolveDatastores([]datastoreSchema.Datastore{ds}, DatastoreSelectionConfig{
		Identifiers:  []string{"200"},
		AllowedTypes: []string{"ceph"},
	})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, "RBD", resolved[0].DiskType)
}

func TestResolveDatastoresAcceptsValidCephFSFileDatastore(t *testing.T) {
	resolved, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTypedTestDatastore(300, "cephfs-a", "fs", "shared", "FILE", map[string]string{
			"SPARKAI_CSI_SHARE_BACKEND":          "cephfs",
			"SPARKAI_CSI_CEPHFS_FS_NAME":         "cephfs-prod",
			"SPARKAI_CSI_CEPHFS_ROOT_PATH":       "/kubernetes",
			"SPARKAI_CSI_CEPHFS_SUBVOLUME_GROUP": "csi",
			"CEPH_HOST":                          "mon1,mon2",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"300"},
		AllowedTypes: []string{"cephfs"},
	})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, "cephfs", resolved[0].Type)
	require.NotNil(t, resolved[0].CephFS)
	assert.Equal(t, "cephfs-prod", resolved[0].CephFS.FSName)
	assert.Equal(t, "/kubernetes", resolved[0].CephFS.RootPath)
	assert.Equal(t, []string{"mon1", "mon2"}, resolved[0].CephFS.Monitors)
}

func TestResolveDatastoresAcceptsNumericCephFSFileCategoryFromOpenNebula(t *testing.T) {
	resolved, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTypedTestDatastore(300, "cephfs-a", "fs", "shared", "2", map[string]string{
			"TYPE":                               "FILE_DS",
			"SPARKAI_CSI_SHARE_BACKEND":          "cephfs",
			"SPARKAI_CSI_CEPHFS_FS_NAME":         "cephfs-prod",
			"SPARKAI_CSI_CEPHFS_ROOT_PATH":       "/kubernetes",
			"SPARKAI_CSI_CEPHFS_SUBVOLUME_GROUP": "csi",
			"CEPH_HOST":                          "mon1,mon2",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"300"},
		AllowedTypes: []string{"cephfs"},
	})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, "FILE", resolved[0].Category)
	assert.Equal(t, "cephfs", resolved[0].Type)
}

func TestResolveDatastoresRejectsCephFSWithoutFileCategory(t *testing.T) {
	_, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTypedTestDatastore(300, "cephfs-a", "fs", "shared", "IMAGE", map[string]string{
			"SPARKAI_CSI_SHARE_BACKEND":          "cephfs",
			"SPARKAI_CSI_CEPHFS_FS_NAME":         "cephfs-prod",
			"SPARKAI_CSI_CEPHFS_ROOT_PATH":       "/kubernetes",
			"SPARKAI_CSI_CEPHFS_SUBVOLUME_GROUP": "csi",
			"CEPH_HOST":                          "mon1 mon2",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"300"},
		AllowedTypes: []string{"cephfs"},
	})
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
	assert.Contains(t, err.Error(), "FILE datastore")
}

func TestResolveDatastoresRejectsCephDatastoreMissingRequiredAttr(t *testing.T) {
	_, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTestDatastore(200, "ceph-a", "ceph", "ceph", map[string]string{
			"DISK_TYPE":   "RBD",
			"POOL_NAME":   "one",
			"CEPH_USER":   "libvirt",
			"CEPH_SECRET": "secret-id",
			"BRIDGE_LIST": "frontend",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"200"},
		AllowedTypes: []string{"ceph"},
	})
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
	assert.Contains(t, err.Error(), "CEPH_HOST")
}

func TestResolveDatastoresRejectsCephDatastoreWithoutRBDDiskType(t *testing.T) {
	_, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTestDatastore(200, "ceph-a", "ceph", "ceph", map[string]string{
			"DISK_TYPE":   "FILE",
			"POOL_NAME":   "one",
			"CEPH_HOST":   "mon1 mon2",
			"CEPH_USER":   "libvirt",
			"CEPH_SECRET": "secret-id",
			"BRIDGE_LIST": "frontend",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"200"},
		AllowedTypes: []string{"ceph"},
	})
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
	assert.Contains(t, err.Error(), "DISK_TYPE=RBD")
}

func TestResolveDatastoresRejectsCephDatastoreWithWrongMADs(t *testing.T) {
	_, err := ResolveDatastores([]datastoreSchema.Datastore{
		newTestDatastore(200, "ceph-a", "ceph", "ssh", map[string]string{
			"DISK_TYPE":   "RBD",
			"POOL_NAME":   "one",
			"CEPH_HOST":   "mon1 mon2",
			"CEPH_USER":   "libvirt",
			"CEPH_SECRET": "secret-id",
			"BRIDGE_LIST": "frontend",
		}),
	}, DatastoreSelectionConfig{
		Identifiers:  []string{"200"},
		AllowedTypes: []string{"ceph"},
	})
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
	assert.Contains(t, err.Error(), "TM_MAD=ceph")
}

func TestCompareCephConnectionIdentityRejectsMismatch(t *testing.T) {
	imageDS := newTestDatastore(200, "ceph-image", "ceph", "ceph", map[string]string{
		"DISK_TYPE":   "RBD",
		"POOL_NAME":   "one",
		"CEPH_HOST":   "mon1 mon2",
		"CEPH_USER":   "libvirt",
		"CEPH_SECRET": "secret-a",
		"BRIDGE_LIST": "frontend",
	})
	systemDS := newTestDatastore(201, "ceph-system", "", "ceph", map[string]string{
		"DISK_TYPE":   "RBD",
		"POOL_NAME":   "one",
		"CEPH_HOST":   "mon1 mon2",
		"CEPH_USER":   "libvirt",
		"CEPH_SECRET": "secret-b",
		"BRIDGE_LIST": "host-a",
	})

	err := compareCephConnectionIdentity(imageDS, systemDS)
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
	assert.Contains(t, err.Error(), "CEPH_SECRET")
}

func TestResolveDeploymentMode(t *testing.T) {
	assert.Equal(t, DeploymentModeCeph, resolveDeploymentMode(newTestDatastore(200, "ceph-system", "", "ceph", nil)))
	assert.Equal(t, DeploymentModeSSH, resolveDeploymentMode(newTestDatastore(201, "ssh-system", "", "ssh", nil)))
	assert.Equal(t, DeploymentModeSSH, resolveDeploymentMode(newTestDatastore(202, "lvm-ssh-system", "", "fs_lvm_ssh", nil)))
	assert.Equal(t, DeploymentModeUnknown, resolveDeploymentMode(newTestDatastore(203, "other", "", "shared", nil)))
}

func TestGetBackendCapabilityProfileReportsNoFilesystemRWXForCurrentBackends(t *testing.T) {
	localProfile := GetBackendCapabilityProfile("local")
	cephProfile := GetBackendCapabilityProfile("ceph")
	cephFSProfile := GetBackendCapabilityProfile("cephfs")

	assert.Equal(t, "local", localProfile.Backend)
	assert.True(t, localProfile.SupportsFilesystemRWO)
	assert.True(t, localProfile.SupportsFilesystemROX)
	assert.False(t, localProfile.SupportsFilesystemRWX)
	assert.True(t, localProfile.SupportsBlockRWO)
	assert.True(t, localProfile.SupportsBlockROX)
	assert.False(t, localProfile.SupportsBlockRWX)

	assert.Equal(t, "ceph", cephProfile.Backend)
	assert.True(t, cephProfile.SupportsFilesystemRWO)
	assert.True(t, cephProfile.SupportsFilesystemROX)
	assert.False(t, cephProfile.SupportsFilesystemRWX)
	assert.True(t, cephProfile.SupportsBlockRWO)
	assert.True(t, cephProfile.SupportsBlockROX)
	assert.False(t, cephProfile.SupportsBlockRWX)

	assert.Equal(t, "cephfs", cephFSProfile.Backend)
	assert.True(t, cephFSProfile.SupportsFilesystemRWO)
	assert.True(t, cephFSProfile.SupportsFilesystemROX)
	assert.True(t, cephFSProfile.SupportsFilesystemRWX)
	assert.False(t, cephFSProfile.SupportsBlockRWO)
	assert.False(t, cephFSProfile.SupportsBlockROX)
	assert.False(t, cephFSProfile.SupportsBlockRWX)
}

func TestSharedVolumeIDRoundTrip(t *testing.T) {
	encoded, err := EncodeSharedVolumeID(SharedVolumeMetadata{
		DatastoreID:    300,
		Mode:           SharedVolumeModeDynamic,
		FSName:         "cephfs-prod",
		SubvolumeGroup: "csi",
		Subpath:        "/volumes/csi/test",
		Backend:        "cephfs",
		SubvolumeName:  "one-csi-test",
	})
	require.NoError(t, err)

	decoded, err := DecodeSharedVolumeID(encoded)
	require.NoError(t, err)
	assert.Equal(t, 300, decoded.DatastoreID)
	assert.Equal(t, SharedVolumeModeDynamic, decoded.Mode)
	assert.Equal(t, "/volumes/csi/test", decoded.Subpath)
	assert.Equal(t, "one-csi-test", decoded.SubvolumeName)
}

func TestOrderDatastoresLeastUsedSortsByFreeCapacity(t *testing.T) {
	ordered, err := OrderDatastores([]Datastore{
		{ID: 100, FreeBytes: 10},
		{ID: 101, FreeBytes: 30},
		{ID: 102, FreeBytes: 20},
	}, DatastoreSelectionPolicyLeastUsed)
	require.NoError(t, err)
	assert.Equal(t, []int{101, 102, 100}, []int{ordered[0].ID, ordered[1].ID, ordered[2].ID})
}

func TestOrderDatastoresOrderedPreservesConfiguredOrder(t *testing.T) {
	ordered, err := OrderDatastores([]Datastore{
		{ID: 100, FreeBytes: 10},
		{ID: 101, FreeBytes: 30},
		{ID: 102, FreeBytes: 20},
	}, DatastoreSelectionPolicyOrdered)
	require.NoError(t, err)
	assert.Equal(t, []int{100, 101, 102}, []int{ordered[0].ID, ordered[1].ID, ordered[2].ID})
}

func TestOrderDatastoresAutopilotPrefersHealthierDatastore(t *testing.T) {
	resetDatastoreSelectionRuntime()
	recordDatastoreProvisioningResult(100, false)
	recordDatastoreProvisioningResult(100, false)
	finish := beginDatastoreAttempt(101)
	defer finish()

	ordered, err := OrderDatastores([]Datastore{
		{ID: 100, FreeBytes: 90, TotalBytes: 100},
		{ID: 101, FreeBytes: 80, TotalBytes: 100},
		{ID: 102, FreeBytes: 70, TotalBytes: 100},
	}, DatastoreSelectionPolicyAutopilot)
	require.NoError(t, err)
	assert.Equal(t, []int{102, 101, 100}, []int{ordered[0].ID, ordered[1].ID, ordered[2].ID})
}

func TestValidateCompatibleSystemDatastore(t *testing.T) {
	imageDS := newTestDatastore(200, "fast-local", "fs", "local", map[string]string{
		"COMPATIBLE_SYS_DS": "100,101",
	})

	require.NoError(t, validateCompatibleSystemDatastore(imageDS, 100))

	err := validateCompatibleSystemDatastore(imageDS, 102)
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
	assert.Contains(t, err.Error(), "COMPATIBLE_SYS_DS")
}

func TestSumDatastoreCapacityAggregatesConfiguredPool(t *testing.T) {
	total := SumDatastoreCapacity([]Datastore{
		{ID: 100, FreeBytes: 10},
		{ID: 101, FreeBytes: 20},
		{ID: 102, FreeBytes: 30},
	})

	assert.Equal(t, int64(60), total)
}

func newTestDatastore(id int, name, dsMad, tmMad string, attrs map[string]string) datastoreSchema.Datastore {
	return newTypedTestDatastore(id, name, dsMad, tmMad, "", attrs)
}

func newTypedTestDatastore(id int, name, dsMad, tmMad, datastoreType string, attrs map[string]string) datastoreSchema.Datastore {
	tpl := datastoreSchema.NewTemplate()
	for key, value := range attrs {
		tpl.AddPair(key, value)
	}

	ds := datastoreSchema.Datastore{
		ID:       id,
		Name:     name,
		DSMad:    dsMad,
		TMMad:    tmMad,
		Type:     datastoreType,
		StateRaw: 0,
		Template: *tpl,
	}
	if value, ok := attrs["DISK_TYPE"]; ok {
		ds.DiskType = value
	}

	return ds
}
