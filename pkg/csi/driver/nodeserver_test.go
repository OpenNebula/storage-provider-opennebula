package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"
)

const (
	targetPath = "/tmp/target" // Example target path for publishing
)

type failingMountInterface struct {
	*mount.FakeMounter
	err error
}

func (f *failingMountInterface) Mount(source, target, fstype string, options []string) error {
	return f.MountSensitive(source, target, fstype, options, nil)
}

func (f *failingMountInterface) MountSensitive(string, string, string, []string, []string) error {
	return f.err
}

func (f *failingMountInterface) MountSensitiveWithoutSystemd(source, target, fstype string, options []string, sensitiveOptions []string) error {
	return f.MountSensitive(source, target, fstype, options, sensitiveOptions)
}

func (f *failingMountInterface) MountSensitiveWithoutSystemdWithMountFlags(source, target, fstype string, options []string, sensitiveOptions []string, _ []string) error {
	return f.MountSensitive(source, target, fstype, options, sensitiveOptions)
}

func getTestNodeServer(mountPoints []string) *NodeServer {
	mountPointList := make([]mount.MountPoint, 0, len(mountPoints))
	for _, mountPoint := range mountPoints {
		mountPointList = append(mountPointList, mount.MountPoint{
			Path: mountPoint,
		})
	}
	return getTestNodeServerWithMountPoints(mountPointList)
}

func getTestNodeServerWithMountPoints(mountPointList []mount.MountPoint) *NodeServer {
	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.NodeDeviceRescanOnMissEnabledVar, false)
	pluginConfig.OverrideVal(config.NodeDeviceUdevSettleTimeoutSecondsVar, 0)
	driver := &Driver{
		name:               DefaultDriverName,
		version:            driverVersion,
		grpcServerEndpoint: DefaultGRPCServerEndpoint,
		nodeID:             "test-node-id",
		maxVolumesPerNode:  30,
		PluginConfig:       pluginConfig,
		featureGates:       defaultFeatureGates(),
		metrics:            NewDriverMetrics(driverVersion, "test"),
	}
	commandScriptArray := []testingexec.FakeCommandAction{}
	//TODO: Simulate real commands
	for i := 0; i < 10; i++ {
		commandScriptArray = append(commandScriptArray, func(cmd string, args ...string) exec.Cmd {
			return &testingexec.FakeCmd{
				Argv:           append([]string{cmd}, args...),
				Stdout:         nil,
				Stderr:         nil,
				DisableScripts: true, // Disable script checking for simplicity
			}
		})
	}

	mounter := mount.NewSafeFormatAndMount(
		mount.NewFakeMounter(mountPointList), // using fake mounter implementation
		&testingexec.FakeExec{
			CommandScript: commandScriptArray,
			LookPathFunc: func(path string) (string, error) {
				return path, nil
			},
		}, // using fake exec implementation
	)
	return NewNodeServer(driver, mounter)
}

func withTestDiskPath(t *testing.T) string {
	t.Helper()

	originalDiskPath := defaultDiskPath
	originalStat := nodeVolumePathStat
	originalSleep := nodeDeviceSleep
	diskPath := t.TempDir()

	defaultDiskPath = diskPath
	nodeVolumePathStat = os.Stat
	nodeDeviceSleep = func(time.Duration) {}

	t.Cleanup(func() {
		defaultDiskPath = originalDiskPath
		nodeVolumePathStat = originalStat
		nodeDeviceSleep = originalSleep
	})

	return diskPath
}

func TestStageVolume(t *testing.T) {
	tempDir := t.TempDir()
	diskPath := withTestDiskPath(t)
	err := os.WriteFile(filepath.Join(diskPath, "zero"), []byte("test"), 0o644)
	assert.NoError(t, err)

	tcs := []struct {
		name           string
		request        *csi.NodeStageVolumeRequest
		expectResponse *csi.NodeStageVolumeResponse
		expectError    bool
	}{
		{
			name: "[SUCCESS] Test basic volume mount",
			request: &csi.NodeStageVolumeRequest{
				VolumeId:          "test-volume-id",
				StagingTargetPath: tempDir,
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType: "ext4",
						},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
				PublishContext: map[string]string{
					"volumeName": "zero",
				},
			},
			expectResponse: &csi.NodeStageVolumeResponse{},
			expectError:    false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{})
			response, err := ns.NodeStageVolume(context.Background(), tc.request)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse, response)
			} else {
				assert.NotNil(t, response)
			}
		})
	}
}

func TestStageVolumeKeepsLocalDeviceReportUntilMountSucceeds(t *testing.T) {
	tempDir := t.TempDir()
	diskPath := withTestDiskPath(t)
	err := os.WriteFile(filepath.Join(diskPath, "sdd"), []byte("test"), 0o644)
	assert.NoError(t, err)

	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.LocalDeviceRecoveryEnabledVar, true)
	pluginConfig.OverrideVal(config.NodeDeviceRescanOnMissEnabledVar, false)
	pluginConfig.OverrideVal(config.NodeDeviceUdevSettleTimeoutSecondsVar, 0)
	driver := &Driver{
		name:         DefaultDriverName,
		version:      driverVersion,
		nodeID:       "node-a",
		PluginConfig: pluginConfig,
		featureGates: defaultFeatureGates(),
		metrics:      NewDriverMetrics(driverVersion, "test"),
		kubeRuntime:  &KubeRuntime{client: fake.NewSimpleClientset(), enabled: true},
	}
	commandScriptArray := []testingexec.FakeCommandAction{}
	for i := 0; i < 10; i++ {
		commandScriptArray = append(commandScriptArray, func(cmd string, args ...string) exec.Cmd {
			return &testingexec.FakeCmd{
				Argv:           append([]string{cmd}, args...),
				Stdout:         nil,
				Stderr:         nil,
				DisableScripts: true,
			}
		})
	}
	mountErr := errors.New("can't read superblock")
	ns := NewNodeServer(driver, mount.NewSafeFormatAndMount(
		&failingMountInterface{FakeMounter: mount.NewFakeMounter(nil), err: mountErr},
		&testingexec.FakeExec{
			CommandScript: commandScriptArray,
			LookPathFunc: func(path string) (string, error) {
				return path, nil
			},
		},
	))
	publishContext := map[string]string{
		"volumeName":                    "sdd",
		annotationBackend:               "local",
		publishContextDeviceSerial:      "onecsi-439",
		publishContextOpenNebulaImageID: "439",
	}
	ns.recordLocalDeviceMissing(context.Background(), "vol-1", "sdd", tempDir, publishContext, errors.New("device not found"))

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-1",
		StagingTargetPath: tempDir,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
		PublishContext: publishContext,
	}

	_, err = ns.NodeStageVolume(context.Background(), req)
	assert.Error(t, err)
	_, err = ns.NodeStageVolume(context.Background(), req)
	assert.Error(t, err)

	cm, err := driver.kubeRuntime.client.CoreV1().ConfigMaps(namespaceFromServiceAccount()).Get(context.Background(), localDeviceStateConfigMapName, metav1.GetOptions{})
	assert.NoError(t, err)
	raw := cm.Data[localDeviceReportKey("node-a", "vol-1")]
	var report LocalDeviceMissingReport
	assert.NoError(t, json.Unmarshal([]byte(raw), &report))
	assert.Equal(t, localDeviceFailureClassMountFailed, report.FailureClass)
	assert.Equal(t, 3, report.Attempts)
	assert.Equal(t, filepath.Join(diskPath, "sdd"), report.DevicePath)
}

func TestRecordLocalDiskStageSessionClearsPreviousRecoveryFailure(t *testing.T) {
	ns := getTestNodeServer(nil)
	ns.localDiskSessions = newLocalDiskSessionStore(t.TempDir())
	recoveredAt := time.Now().Add(-time.Minute)
	ns.recordLocalDiskSession(localDiskSession{
		VolumeID: "vol-1",
		PublishedTargets: []localDiskPublishedTarget{
			{TargetPath: "/pods/pod-a/vol", MountOptions: []string{"rw"}},
		},
		LastRecoveredAt:   &recoveredAt,
		LastRecoveryError: "local RWO recovery attempts exhausted",
		RecoveryAttempts:  3,
	})

	ns.recordLocalDiskStageSession(&csi.NodeStageVolumeRequest{
		VolumeId:          "vol-1",
		StagingTargetPath: "/stage/vol-1",
		PublishContext: map[string]string{
			"volumeName": "sdg",
		},
	}, "/dev/sdg", "xfs", []string{"rw"})

	session, exists, err := ns.loadLocalDiskSession("vol-1")
	assert.NoError(t, err)
	assert.True(t, exists)
	assert.Len(t, session.PublishedTargets, 1)
	assert.Nil(t, session.LastRecoveredAt)
	assert.Empty(t, session.LastRecoveryError)
	assert.Equal(t, 0, session.RecoveryAttempts)
}

func TestResolveDevicePath(t *testing.T) {
	t.Run("returns exact device path when present", func(t *testing.T) {
		diskPath := withTestDiskPath(t)
		err := os.WriteFile(filepath.Join(diskPath, "sde"), []byte("test"), 0o644)
		assert.NoError(t, err)

		ns := getTestNodeServer(nil)
		devicePath, resolution, resolveErr := ns.resolveDevicePath("sde", time.Second)

		assert.NoError(t, resolveErr)
		assert.Equal(t, filepath.Join(diskPath, "sde"), devicePath)
		assert.Equal(t, "exact", resolution.ResolvedBy)
	})

	t.Run("falls back to virtio alias when present", func(t *testing.T) {
		diskPath := withTestDiskPath(t)
		err := os.WriteFile(filepath.Join(diskPath, "vde"), []byte("test"), 0o644)
		assert.NoError(t, err)

		ns := getTestNodeServer(nil)
		devicePath, resolution, resolveErr := ns.resolveDevicePath("sde", time.Second)

		assert.NoError(t, resolveErr)
		assert.Equal(t, filepath.Join(diskPath, "vde"), devicePath)
		assert.Equal(t, "alias", resolution.ResolvedBy)
	})

	t.Run("times out when no candidate device appears", func(t *testing.T) {
		withTestDiskPath(t)
		ns := getTestNodeServer(nil)
		ns.Driver.PluginConfig.OverrideVal(config.NodeDeviceDiscoveryTimeoutVar, 1)

		devicePath, _, resolveErr := ns.resolveDevicePath("sdf", ns.deviceDiscoveryTimeout(nil))

		assert.Empty(t, devicePath)
		assert.Error(t, resolveErr)
		assert.Contains(t, resolveErr.Error(), "timed out after 1s")
		assert.Contains(t, resolveErr.Error(), "sdf")
	})

	t.Run("uses dedicated publish context timeout override when present", func(t *testing.T) {
		ns := getTestNodeServer(nil)
		ns.Driver.PluginConfig.OverrideVal(config.NodeDeviceDiscoveryTimeoutVar, 30)

		timeout := ns.deviceDiscoveryTimeout(map[string]string{
			publishContextDeviceDiscoveryTimeoutSeconds: "30",
			publishContextHotplugTimeoutSeconds:         "300",
		})

		assert.Equal(t, 30*time.Second, timeout)
	})

	t.Run("falls back to configured device discovery timeout", func(t *testing.T) {
		ns := getTestNodeServer(nil)
		ns.Driver.PluginConfig.OverrideVal(config.NodeDeviceDiscoveryTimeoutVar, 45)

		timeout := ns.deviceDiscoveryTimeout(nil)

		assert.Equal(t, 45*time.Second, timeout)
	})
}

func TestStageSharedFilesystemVolume(t *testing.T) {
	tempDir := t.TempDir()
	ns := getTestNodeServer([]string{})

	resp, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "cephfs:eyJiYWNrZW5kIjoiY2VwaGZzIiwiZGF0YXN0b3JlSUQiOjMwMCwiZnNOYW1lIjoiY2VwaGZzLXByb2QiLCJtb2RlIjoic3RhdGljIiwic3VicGF0aCI6Ii9rdWJlcm5ldGVzL3N0YXRpYy9tb2RlbC1jYWNoZSJ9",
		StagingTargetPath: tempDir,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
		PublishContext: map[string]string{
			"shareBackend":   "cephfs",
			"cephfsMonitors": "mon1,mon2",
			"cephfsFSName":   "cephfs-prod",
			"cephfsSubpath":  "/kubernetes/static/model-cache",
			"cephfsReadonly": "false",
		},
		Secrets: map[string]string{
			"userID":  "csi-node",
			"userKey": "super-secret",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, &csi.NodeStageVolumeResponse{}, resp)
}

func TestUnstageVolume(t *testing.T) {
	tempDir := t.TempDir()
	tcs := []struct {
		name           string
		request        *csi.NodeUnstageVolumeRequest
		expectResponse *csi.NodeUnstageVolumeResponse
		expectError    bool
	}{
		{
			name: "[SUCCESS] Test correct staging target path",
			request: &csi.NodeUnstageVolumeRequest{
				VolumeId:          "test-volume-id",
				StagingTargetPath: tempDir,
			},
			expectResponse: &csi.NodeUnstageVolumeResponse{},
			expectError:    false,
		},
		{
			name: "[SUCCESS] Test unmounted staging target path",
			request: &csi.NodeUnstageVolumeRequest{
				VolumeId:          "test-volume-id",
				StagingTargetPath: "/tmp/nonexistent",
			},
			expectResponse: &csi.NodeUnstageVolumeResponse{},
			expectError:    false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{tempDir})
			response, err := ns.NodeUnstageVolume(context.Background(), tc.request)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse, response)
			} else {
				assert.NotNil(t, response)
			}
		})
	}
}

func TestPublishVolume(t *testing.T) {
	tempDir := t.TempDir()
	tcs := []struct {
		name           string
		request        *csi.NodePublishVolumeRequest
		expectResponse *csi.NodePublishVolumeResponse
		expectError    bool
	}{
		{
			name: "[SUCCESS] Test correct staging target path",
			request: &csi.NodePublishVolumeRequest{
				VolumeId:          "test-volume-id",
				StagingTargetPath: tempDir,
				TargetPath:        targetPath,
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType:     "ext4",
							MountFlags: []string{"ro"},
						},
					},
				},
				PublishContext: map[string]string{
					"volumeName": "zero",
				},
			},
			expectResponse: &csi.NodePublishVolumeResponse{},
			expectError:    false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{tempDir})
			response, err := ns.NodePublishVolume(context.Background(), tc.request)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse, response)
			} else {
				assert.NotNil(t, response)
			}
		})
	}
}

func TestPublishSharedFilesystemVolume(t *testing.T) {
	tempDir := t.TempDir()
	ns := getTestNodeServer([]string{tempDir})

	resp, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "cephfs:eyJiYWNrZW5kIjoiY2VwaGZzIiwiZGF0YXN0b3JlSUQiOjMwMCwiZnNOYW1lIjoiY2VwaGZzLXByb2QiLCJtb2RlIjoiZHluYW1pYyIsInN1YnBhdGgiOiIva3ViZXJuZXRlcy9keW5hbWljL29uZS1jc2ktZGVtbyJ9",
		StagingTargetPath: tempDir,
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
			"shareBackend":   "cephfs",
			"cephfsMonitors": "mon1,mon2",
			"cephfsFSName":   "cephfs-prod",
			"cephfsSubpath":  "/kubernetes/dynamic/one-csi-demo",
			"cephfsReadonly": "false",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, &csi.NodePublishVolumeResponse{}, resp)
}

func TestUnpublishVolume(t *testing.T) {
	tempDir := t.TempDir()
	tcs := []struct {
		name           string
		request        *csi.NodeUnpublishVolumeRequest
		expectResponse *csi.NodeUnpublishVolumeResponse
		expectError    bool
	}{
		{
			name: "[SUCCESS] Test correct staging target path",
			request: &csi.NodeUnpublishVolumeRequest{
				VolumeId:   "test-volume-id",
				TargetPath: targetPath,
			},
			expectResponse: &csi.NodeUnpublishVolumeResponse{},
			expectError:    false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{tempDir})
			response, err := ns.NodeUnpublishVolume(context.Background(), tc.request)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse, response)
			} else {
				assert.NotNil(t, response)
			}
		})
	}
}

func TestNodeGetVolumeStats(t *testing.T) {
	t.Run("returns filesystem stats for mounted directory", func(t *testing.T) {
		tempDir := t.TempDir()
		ns := getTestNodeServer([]string{tempDir})

		response, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
			VolumeId:   "test-volume-id",
			VolumePath: tempDir,
		})

		assert.NoError(t, err)
		if assert.Len(t, response.GetUsage(), 2) {
			assert.Equal(t, csi.VolumeUsage_BYTES, response.GetUsage()[0].GetUnit())
			assert.Greater(t, response.GetUsage()[0].GetTotal(), int64(0))
			assert.Equal(t, csi.VolumeUsage_INODES, response.GetUsage()[1].GetUnit())
			assert.Greater(t, response.GetUsage()[1].GetTotal(), int64(0))
		}
	})

	t.Run("returns byte stats for block-like file", func(t *testing.T) {
		tempDir := t.TempDir()
		filePath := tempDir + "/volume.img"
		payload := make([]byte, 4096)
		err := os.WriteFile(filePath, payload, 0o644)
		assert.NoError(t, err)

		ns := getTestNodeServer([]string{tempDir})
		response, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
			VolumeId:   "test-volume-id",
			VolumePath: filePath,
		})

		assert.NoError(t, err)
		if assert.Len(t, response.GetUsage(), 1) {
			assert.Equal(t, csi.VolumeUsage_BYTES, response.GetUsage()[0].GetUnit())
			assert.Equal(t, int64(len(payload)), response.GetUsage()[0].GetTotal())
			assert.Equal(t, int64(len(payload)), response.GetUsage()[0].GetUsed())
		}
	})

	t.Run("rejects empty volume path", func(t *testing.T) {
		ns := getTestNodeServer(nil)
		response, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
			VolumeId: "test-volume-id",
		})

		assert.Error(t, err)
		assert.Nil(t, response)
	})

	t.Run("maps disconnected cephfs statfs to failed precondition", func(t *testing.T) {
		tempDir := t.TempDir()
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

		response, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
			VolumeId:   "cephfs:test-volume-id",
			VolumePath: tempDir,
		})

		assert.Nil(t, response)
		assert.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, err.Error(), "stale CephFS mount detected")
	})

	t.Run("maps stale local disk statfs to failed precondition", func(t *testing.T) {
		tempDir := t.TempDir()
		ns := getTestNodeServer([]string{tempDir})
		ns.localDiskSessions = newLocalDiskSessionStore(t.TempDir())
		ns.recordLocalDiskSession(localDiskSession{
			VolumeID:          "test-volume-id",
			StagingTargetPath: tempDir,
			PVCNamespace:      "default",
			PVCName:           "claim",
		})

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
			return unix.EIO
		}

		response, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
			VolumeId:   "test-volume-id",
			VolumePath: tempDir,
		})

		assert.Nil(t, response)
		assert.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, err.Error(), "stale local disk mount detected")
	})
}

func TestNodeExpandVolume(t *testing.T) {
	const requiredBytes = int64(2 * 1024 * 1024 * 1024)

	setFilesystemBytesSequence := func(sequence []int64) {
		index := 0
		nodeVolumePathFS = func(_ string, buf *unix.Statfs_t) error {
			sizeBytes := sequence[len(sequence)-1]
			if index < len(sequence) {
				sizeBytes = sequence[index]
				index++
			}
			buf.Bsize = 4096
			buf.Blocks = uint64(sizeBytes / int64(buf.Bsize))
			buf.Bavail = buf.Blocks
			buf.Bfree = buf.Blocks
			buf.Files = 1024
			buf.Ffree = 1024
			return nil
		}
	}

	makeFilesystemRequest := func(volumePath string) *csi.NodeExpandVolumeRequest {
		return &csi.NodeExpandVolumeRequest{
			VolumeId:          "test-volume-id",
			VolumePath:        volumePath,
			StagingTargetPath: volumePath,
			CapacityRange: &csi.CapacityRange{
				RequiredBytes: requiredBytes,
			},
			VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
				},
			},
		}
	}

	t.Run("returns immediately for block volume", func(t *testing.T) {
		tempDir := t.TempDir()
		ns := getTestNodeServer([]string{tempDir})
		response, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
			VolumeId:   "test-volume-id",
			VolumePath: tempDir,
			CapacityRange: &csi.CapacityRange{
				RequiredBytes: requiredBytes,
			},
			VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Block{
					Block: &csi.VolumeCapability_BlockVolume{},
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, &csi.NodeExpandVolumeResponse{CapacityBytes: requiredBytes}, response)
	})

	t.Run("retries until filesystem reaches target", func(t *testing.T) {
		originalStatfs := nodeVolumePathFS
		originalSleep := nodeDeviceSleep
		originalNow := nodeNow
		originalResizeFS := nodeResizeFS
		originalGOOS := nodeRuntimeGOOS
		t.Cleanup(func() {
			nodeVolumePathFS = originalStatfs
			nodeDeviceSleep = originalSleep
			nodeNow = originalNow
			nodeResizeFS = originalResizeFS
			nodeRuntimeGOOS = originalGOOS
		})

		volumePath := t.TempDir()
		devicePath := filepath.Join(t.TempDir(), "device")
		assert.NoError(t, os.WriteFile(devicePath, []byte("x"), 0o644))
		assert.NoError(t, os.Truncate(devicePath, requiredBytes))

		ns := getTestNodeServerWithMountPoints([]mount.MountPoint{
			{Path: volumePath, Device: devicePath},
		})
		ns.Driver.PluginConfig.OverrideVal(config.NodeExpandVerifyTimeoutSecondsVar, 30)
		ns.Driver.PluginConfig.OverrideVal(config.NodeExpandRetryIntervalSecondsVar, 2)
		ns.Driver.PluginConfig.OverrideVal(config.NodeExpandSizeToleranceBytesVar, 0)

		nodeRuntimeGOOS = "linux"
		now := time.Unix(0, 0)
		nodeNow = func() time.Time { return now }
		nodeDeviceSleep = func(d time.Duration) { now = now.Add(d) }

		resizeCalls := 0
		nodeResizeFS = func(_ exec.Interface, _, _ string) (bool, error) {
			resizeCalls++
			return true, nil
		}
		setFilesystemBytesSequence([]int64{
			requiredBytes / 2,
			requiredBytes / 2,
			requiredBytes,
		})

		response, err := ns.NodeExpandVolume(context.Background(), makeFilesystemRequest(volumePath))
		assert.NoError(t, err)
		assert.Equal(t, &csi.NodeExpandVolumeResponse{CapacityBytes: requiredBytes}, response)
		assert.Equal(t, 3, resizeCalls)
	})

	t.Run("returns deadline exceeded when filesystem never converges", func(t *testing.T) {
		originalStatfs := nodeVolumePathFS
		originalSleep := nodeDeviceSleep
		originalNow := nodeNow
		originalResizeFS := nodeResizeFS
		originalGOOS := nodeRuntimeGOOS
		t.Cleanup(func() {
			nodeVolumePathFS = originalStatfs
			nodeDeviceSleep = originalSleep
			nodeNow = originalNow
			nodeResizeFS = originalResizeFS
			nodeRuntimeGOOS = originalGOOS
		})

		volumePath := t.TempDir()
		devicePath := filepath.Join(t.TempDir(), "device")
		assert.NoError(t, os.WriteFile(devicePath, []byte("x"), 0o644))
		assert.NoError(t, os.Truncate(devicePath, requiredBytes))

		ns := getTestNodeServerWithMountPoints([]mount.MountPoint{
			{Path: volumePath, Device: devicePath},
		})
		ns.Driver.PluginConfig.OverrideVal(config.NodeExpandVerifyTimeoutSecondsVar, 10)
		ns.Driver.PluginConfig.OverrideVal(config.NodeExpandRetryIntervalSecondsVar, 5)
		ns.Driver.PluginConfig.OverrideVal(config.NodeExpandSizeToleranceBytesVar, 0)

		nodeRuntimeGOOS = "linux"
		now := time.Unix(0, 0)
		nodeNow = func() time.Time { return now }
		nodeDeviceSleep = func(d time.Duration) { now = now.Add(d) }

		resizeCalls := 0
		nodeResizeFS = func(_ exec.Interface, _, _ string) (bool, error) {
			resizeCalls++
			return true, nil
		}
		setFilesystemBytesSequence([]int64{requiredBytes / 2})

		response, err := ns.NodeExpandVolume(context.Background(), makeFilesystemRequest(volumePath))
		assert.Nil(t, response)
		assert.Error(t, err)
		assert.Equal(t, codes.DeadlineExceeded, status.Code(err))
		assert.Contains(t, err.Error(), "requested_bytes")
		assert.GreaterOrEqual(t, resizeCalls, 2)
	})

	t.Run("waits for device expansion before resizing filesystem", func(t *testing.T) {
		originalStatfs := nodeVolumePathFS
		originalSleep := nodeDeviceSleep
		originalNow := nodeNow
		originalResizeFS := nodeResizeFS
		originalGOOS := nodeRuntimeGOOS
		t.Cleanup(func() {
			nodeVolumePathFS = originalStatfs
			nodeDeviceSleep = originalSleep
			nodeNow = originalNow
			nodeResizeFS = originalResizeFS
			nodeRuntimeGOOS = originalGOOS
		})

		volumePath := t.TempDir()
		devicePath := filepath.Join(t.TempDir(), "device")
		assert.NoError(t, os.WriteFile(devicePath, []byte("x"), 0o644))
		assert.NoError(t, os.Truncate(devicePath, requiredBytes/2))

		ns := getTestNodeServerWithMountPoints([]mount.MountPoint{
			{Path: volumePath, Device: devicePath},
		})
		ns.Driver.PluginConfig.OverrideVal(config.NodeExpandVerifyTimeoutSecondsVar, 20)
		ns.Driver.PluginConfig.OverrideVal(config.NodeExpandRetryIntervalSecondsVar, 2)
		ns.Driver.PluginConfig.OverrideVal(config.NodeExpandSizeToleranceBytesVar, 0)

		nodeRuntimeGOOS = "linux"
		now := time.Unix(0, 0)
		nodeNow = func() time.Time { return now }

		sleepCalls := 0
		nodeDeviceSleep = func(d time.Duration) {
			sleepCalls++
			now = now.Add(d)
			if sleepCalls == 1 {
				_ = os.Truncate(devicePath, requiredBytes)
			}
		}

		resizeCalls := 0
		nodeResizeFS = func(_ exec.Interface, _, _ string) (bool, error) {
			resizeCalls++
			return true, nil
		}
		setFilesystemBytesSequence([]int64{requiredBytes})

		response, err := ns.NodeExpandVolume(context.Background(), makeFilesystemRequest(volumePath))
		assert.NoError(t, err)
		assert.Equal(t, &csi.NodeExpandVolumeResponse{CapacityBytes: requiredBytes}, response)
		assert.Equal(t, 1, sleepCalls)
		assert.Equal(t, 1, resizeCalls)
	})
}

func TestNodeGetCapabilities(t *testing.T) {
	tempDir := t.TempDir()
	tcs := []struct {
		name           string
		request        *csi.NodeGetCapabilitiesRequest
		expectResponse *csi.NodeGetCapabilitiesResponse
		expectError    bool
	}{
		{
			name:    "[Success] Test capabilities",
			request: &csi.NodeGetCapabilitiesRequest{},
			expectResponse: &csi.NodeGetCapabilitiesResponse{
				Capabilities: []*csi.NodeServiceCapability{
					{
						Type: &csi.NodeServiceCapability_Rpc{
							Rpc: &csi.NodeServiceCapability_RPC{
								Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
							},
						},
					},
					{
						Type: &csi.NodeServiceCapability_Rpc{
							Rpc: &csi.NodeServiceCapability_RPC{
								Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
							},
						},
					},
					{
						Type: &csi.NodeServiceCapability_Rpc{
							Rpc: &csi.NodeServiceCapability_RPC{
								Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
							},
						},
					},
				},
			},
			expectError: false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{tempDir})
			response, err := ns.NodeGetCapabilities(context.Background(), tc.request)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse, response)
			} else {
				assert.NotNil(t, response)
			}
		})
	}
}

func TestNodeGetInfo(t *testing.T) {
	tempDir := t.TempDir()
	tcs := []struct {
		name           string
		request        *csi.NodeGetInfoRequest
		expectResponse *csi.NodeGetInfoResponse
		expectError    bool
	}{
		{
			name:    "[Success] Test retrieved node info",
			request: &csi.NodeGetInfoRequest{},
			expectResponse: &csi.NodeGetInfoResponse{
				NodeId:            "test-node-id",
				MaxVolumesPerNode: 30,
			},
			expectError: false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{tempDir})
			response, err := ns.NodeGetInfo(context.Background(), tc.request)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse, response)
			} else {
				assert.NotNil(t, response)
			}
		})
	}
}

func TestNodeGetInfoIncludesAccessibleTopologyWhenEnabled(t *testing.T) {
	tempDir := t.TempDir()
	ns := getTestNodeServer([]string{tempDir})
	ns.Driver.featureGates.TopologyAccessibility = true
	ns.Driver.PluginConfig.OverrideVal(config.NodeTopologySystemDSVar, "111")

	response, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	assert.NoError(t, err)
	if assert.NotNil(t, response.GetAccessibleTopology()) {
		assert.Equal(t, "111", response.GetAccessibleTopology().Segments[topologySystemDSLabel])
	}
}
