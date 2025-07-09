package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"
	"k8s.io/utils/exec/testing"
)

const (
	stagingTargetPath = "/mnt"        // Example staging target path
	targetPath        = "/mnt/target" // Example target path for publishing
)

func getTestNodeServer(mountPoints []string) *NodeServer {
	driver := &Driver{
		name:               DefaultDriverName,
		version:            driverVersion,
		grpcServerEndpoint: DefaultGRPCServerEndpoint,
		nodeID:             "test-node-id",
		maxVolumesPerNode:  30,
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
	mountPointList := []mount.MountPoint{}
	for _, mountPoint := range mountPoints {
		mountPointList = append(mountPointList, mount.MountPoint{
			Path: mountPoint,
		})
	}

	mounter := mount.NewSafeFormatAndMount(
		mount.NewFakeMounter(mountPointList), // using fake mounter implementation
		&testingexec.FakeExec{
			CommandScript: commandScriptArray,
		}, // using fake exec implementation
	)
	return NewNodeServer(driver, mounter)
}

func TestStageVolume(t *testing.T) {

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
				StagingTargetPath: stagingTargetPath,
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType: "ext4",
						},
					},
				},
				VolumeContext: map[string]string{
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

func TestUnstageVolume(t *testing.T) {

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
				StagingTargetPath: stagingTargetPath,
			},
			expectResponse: &csi.NodeUnstageVolumeResponse{},
			expectError:    false,
		},
		{
			name: "[SUCCESS] Test unmounted staging target path",
			request: &csi.NodeUnstageVolumeRequest{
				VolumeId:          "test-volume-id",
				StagingTargetPath: "/mnt/nonexistent",
			},
			expectResponse: &csi.NodeUnstageVolumeResponse{},
			expectError:    false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{stagingTargetPath})
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
				StagingTargetPath: stagingTargetPath,
				TargetPath:        targetPath,
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType:     "ext4",
							MountFlags: []string{"ro"},
						},
					},
				},
				VolumeContext: map[string]string{
					"volumeName": "zero",
				},
			},
			expectResponse: &csi.NodePublishVolumeResponse{},
			expectError:    false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{stagingTargetPath})
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

func TestUnpublishVolume(t *testing.T) {

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
			ns := getTestNodeServer([]string{stagingTargetPath})
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

	tcs := []struct {
		name           string
		request        *csi.NodeGetVolumeStatsRequest
		expectResponse *csi.NodeGetVolumeStatsResponse
		expectError    bool
	}{
		{
			name: "[ERROR] Test unimplemented",
			request: &csi.NodeGetVolumeStatsRequest{
				VolumeId: "test-volume-id",
			},
			expectResponse: &csi.NodeGetVolumeStatsResponse{},
			expectError:    true,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{stagingTargetPath})
			response, err := ns.NodeGetVolumeStats(context.Background(), tc.request)
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

func TestNodeExpandVolume(t *testing.T) {
	tcs := []struct {
		name           string
		request        *csi.NodeExpandVolumeRequest
		expectResponse *csi.NodeExpandVolumeResponse
		expectError    bool
	}{
		{
			name: "[ERROR] Test unimplemented",
			request: &csi.NodeExpandVolumeRequest{
				VolumeId: "test-volume-id",
			},
			expectResponse: &csi.NodeExpandVolumeResponse{},
			expectError:    true,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{stagingTargetPath})
			response, err := ns.NodeExpandVolume(context.Background(), tc.request)
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

func TestNodeGetCapabilities(t *testing.T) {

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
				},
			},
			expectError: false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ns := getTestNodeServer([]string{stagingTargetPath})
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
			ns := getTestNodeServer([]string{stagingTargetPath})
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
