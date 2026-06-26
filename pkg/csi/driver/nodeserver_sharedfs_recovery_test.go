package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	mount "k8s.io/mount-utils"
)

func TestLoadFeatureGatesIncludesCephFSRecoveryDefaults(t *testing.T) {
	gates := defaultFeatureGates()
	assert.True(t, gates.CephFSPersistentRecovery)
	assert.False(t, gates.CephFSKernelMounts)

	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.FeatureGatesVar, "cephfsPersistentRecovery=false,cephfsKernelMounts=true")
	loaded := loadFeatureGates(cfg)
	assert.False(t, loaded.CephFSPersistentRecovery)
	assert.True(t, loaded.CephFSKernelMounts)
}

func TestStageSharedFilesystemPersistsSessionAndCredentials(t *testing.T) {
	withSharedFilesystemTestPaths(t)

	stagePath := filepath.Join(t.TempDir(), "globalmount")
	volumeID := "cephfs:test-stage-session"
	ns := getTestNodeServer(nil)

	resp, err := ns.NodeStageVolume(context.Background(), newSharedFilesystemStageRequest(volumeID, stagePath, "fuse"))
	require.NoError(t, err)
	assert.Equal(t, &csi.NodeStageVolumeResponse{}, resp)

	session, exists, err := ns.sharedFilesystemRecovery.store.Load(volumeID)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, sharedFilesystemMounterFuse, session.Mounter)
	assert.Equal(t, "csi-node", session.UserID)
	assert.Equal(t, []string{"mon1", "mon2"}, session.Monitors)
	assert.Equal(t, "/kubernetes/dynamic/test", session.Subpath)

	_, err = os.Stat(sharedCephFSKeyringPath(stagePath))
	assert.NoError(t, err)
	_, err = os.Stat(sharedCephFSSecretPath(stagePath))
	assert.NoError(t, err)
}

func TestNodeGetVolumeStatsQueuesRecoveryForStaleCephFSMount(t *testing.T) {
	withSharedFilesystemTestPaths(t)

	tempDir := t.TempDir()
	volumeID := "cephfs:test-stale-stats"
	ns := getTestNodeServer([]string{tempDir})

	originalStat := nodeVolumePathStat
	originalStatfs := nodeVolumePathFS
	t.Cleanup(func() {
		nodeVolumePathStat = originalStat
		nodeVolumePathFS = originalStatfs
	})

	nodeVolumePathStat = func(name string) (os.FileInfo, error) {
		return originalStat(name)
	}
	nodeVolumePathFS = func(path string, buf *unix.Statfs_t) error {
		return errors.New("transport endpoint is not connected")
	}

	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   volumeID,
		VolumePath: tempDir,
	})

	assert.Nil(t, resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale CephFS mount detected")

	ns.sharedFilesystemRecovery.mu.Lock()
	_, queued := ns.sharedFilesystemRecovery.queued[volumeID]
	ns.sharedFilesystemRecovery.mu.Unlock()
	assert.True(t, queued)
}

func TestSharedFilesystemRecoveryRebindsMissingTarget(t *testing.T) {
	withSharedFilesystemTestPaths(t)

	baseDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	stagePath := filepath.Join(baseDir, "globalmount")
	targetPath := filepath.Join(baseDir, "target")
	volumeID := "cephfs:test-rebind"
	ns := getTestNodeServer(nil)

	_, err = ns.NodeStageVolume(context.Background(), newSharedFilesystemStageRequest(volumeID, stagePath, "fuse"))
	require.NoError(t, err)

	_, err = ns.NodePublishVolume(context.Background(), newSharedFilesystemPublishRequest(volumeID, stagePath, targetPath))
	require.NoError(t, err)
	session, exists, err := ns.sharedFilesystemRecovery.store.Load(volumeID)
	require.NoError(t, err)
	require.True(t, exists)
	require.Len(t, session.PublishedTargets, 1)

	fakeMounter, ok := ns.mounter.Interface.(*mount.FakeMounter)
	require.True(t, ok)
	filtered := make([]mount.MountPoint, 0, len(fakeMounter.MountPoints))
	for _, mountPoint := range fakeMounter.MountPoints {
		if mountPoint.Path == targetPath {
			continue
		}
		filtered = append(filtered, mountPoint)
	}
	fakeMounter.MountPoints = filtered

	err = ns.sharedFilesystemRecovery.recoverVolume(context.Background(), volumeID)
	require.NoError(t, err)

	check, err := ns.checkMountPoint(stagePath, targetPath, nil)
	require.NoError(t, err)
	assert.True(t, check.targetIsMountPoint)
}

func TestMountSharedFilesystemSessionRecreatesMissingStagePath(t *testing.T) {
	withSharedFilesystemTestPaths(t)

	stagePath := filepath.Join(t.TempDir(), "globalmount")
	volumeID := "cephfs:test-missing-stage-path"
	ns := getTestNodeServer(nil)

	_, err := ns.NodeStageVolume(context.Background(), newSharedFilesystemStageRequest(volumeID, stagePath, "fuse"))
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(stagePath))
	_, err = os.Stat(stagePath)
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))

	session, exists, err := ns.sharedFilesystemRecovery.store.Load(volumeID)
	require.NoError(t, err)
	require.True(t, exists)

	require.NoError(t, ns.mountSharedFilesystemSession(session))

	info, err := os.Stat(stagePath)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestStageSharedFilesystemKernelMounterRequiresFeatureGate(t *testing.T) {
	withSharedFilesystemTestPaths(t)

	stagePath := filepath.Join(t.TempDir(), "globalmount")
	ns := getTestNodeServer(nil)

	_, err := ns.NodeStageVolume(context.Background(), newSharedFilesystemStageRequest("cephfs:test-kernel-gate", stagePath, "kernel"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cephfsKernelMounts=true")
}

func TestStageSharedFilesystemKernelMounterSucceedsWhenEnabled(t *testing.T) {
	withSharedFilesystemTestPaths(t)

	procFile := filepath.Join(t.TempDir(), "filesystems")
	require.NoError(t, os.WriteFile(procFile, []byte("nodev\tceph\n"), 0o644))
	sharedFilesystemProcFilesystemsPath = procFile

	stagePath := filepath.Join(t.TempDir(), "globalmount")
	volumeID := "cephfs:test-kernel-success"
	ns := getTestNodeServer(nil)
	ns.Driver.featureGates.CephFSKernelMounts = true

	resp, err := ns.NodeStageVolume(context.Background(), newSharedFilesystemStageRequest(volumeID, stagePath, "kernel"))
	require.NoError(t, err)
	assert.Equal(t, &csi.NodeStageVolumeResponse{}, resp)

	session, exists, err := ns.sharedFilesystemRecovery.store.Load(volumeID)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, sharedFilesystemMounterKernel, session.Mounter)

	_, err = os.Stat(sharedCephFSSecretPath(stagePath))
	assert.NoError(t, err)
}

func TestHostSupportsCephKernelClient(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "filesystems")

	require.NoError(t, os.WriteFile(tempFile, []byte("nodev\text4\n"), 0o644))
	supported, err := hostSupportsCephKernelClient(tempFile)
	require.NoError(t, err)
	assert.False(t, supported)

	require.NoError(t, os.WriteFile(tempFile, []byte("nodev\tceph\n"), 0o644))
	supported, err = hostSupportsCephKernelClient(tempFile)
	require.NoError(t, err)
	assert.True(t, supported)
}

func withSharedFilesystemTestPaths(t *testing.T) {
	t.Helper()

	originalSessionRoot := sharedFilesystemSessionRootPath
	originalProcFilesystems := sharedFilesystemProcFilesystemsPath
	sharedFilesystemSessionRootPath = filepath.Join(t.TempDir(), "cephfs-sessions")
	sharedFilesystemProcFilesystemsPath = "/proc/filesystems"

	t.Cleanup(func() {
		sharedFilesystemSessionRootPath = originalSessionRoot
		sharedFilesystemProcFilesystemsPath = originalProcFilesystems
	})
}

func newSharedFilesystemStageRequest(volumeID, stagePath, mounter string) *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
		VolumeContext: map[string]string{
			storageClassParamCephFSMounter: mounter,
		},
		PublishContext: map[string]string{
			sharedPublishContextShareBackend:   "cephfs",
			sharedPublishContextCephFSMonitors: "mon1,mon2",
			sharedPublishContextCephFSFSName:   "cephfs-prod",
			sharedPublishContextCephFSSubpath:  "/kubernetes/dynamic/test",
			sharedPublishContextCephFSReadonly: "false",
		},
		Secrets: map[string]string{
			sharedNodeStageSecretUserIDKey:  "csi-node",
			sharedNodeStageSecretUserKeyKey: "super-secret",
		},
	}
}

func newSharedFilesystemPublishRequest(volumeID, stagePath, targetPath string) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
		PublishContext: map[string]string{
			sharedPublishContextShareBackend: "cephfs",
		},
	}
}
