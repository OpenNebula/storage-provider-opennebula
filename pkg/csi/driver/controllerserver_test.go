/*
Copyright 2025, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

const (
	volumeSize    = 10 * 1024 * 1024
	datastoreSize = 100 * 1024 * 1024 * 1024
)

func getTestControllerServer(mockProvider *MockOpenNebulaVolumeProviderTestify) *ControllerServer {
	driver := &Driver{
		name:               DefaultDriverName,
		version:            driverVersion,
		grpcServerEndpoint: DefaultGRPCServerEndpoint,
		nodeID:             "test-controller-id",
	}

	return NewControllerServer(driver, mockProvider)
}

func TestCreateVolume(t *testing.T) {
	const volumeSize = int64(1024 * 1024 * 1024) // 1GiB

	tcs := []struct {
		name                string
		createVolumeRequest *csi.CreateVolumeRequest
		expectResponse      *csi.CreateVolumeResponse
		expectError         bool
		setupMock           func(m *MockOpenNebulaVolumeProviderTestify)
	}{
		{
			name: "TestBasicVolumeCreation",
			createVolumeRequest: &csi.CreateVolumeRequest{
				Name: "test-volume",
				CapacityRange: &csi.CapacityRange{
					RequiredBytes: volumeSize,
				},
				VolumeCapabilities: []*csi.VolumeCapability{
					{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								FsType: "",
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
						},
					},
				},
				Parameters: map[string]string{
					"datastore": "default",
					"type":      "BLOCK",
				},
			},
			expectResponse: &csi.CreateVolumeResponse{
				Volume: &csi.Volume{
					VolumeId:      "test-volume",
					CapacityBytes: volumeSize,
				},
			},
			expectError: false,
			setupMock: func(m *MockOpenNebulaVolumeProviderTestify) {
				m.On("VolumeExists", mock.Anything, "test-volume").
					Return(-1, -1, nil)
				m.On("CreateVolume", mock.Anything, "test-volume", volumeSize, mock.Anything, false, "", map[string]string{"datastore": "default", "type": "BLOCK"}).
					Return(nil)
			},
		},
		{
			name: "TestVolumeCreationWithoutName",
			createVolumeRequest: &csi.CreateVolumeRequest{
				Name: "",
				CapacityRange: &csi.CapacityRange{
					RequiredBytes: volumeSize,
				},
				VolumeCapabilities: []*csi.VolumeCapability{
					{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								FsType: "ext4",
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
						},
					},
				},
			},
			expectResponse: nil,
			expectError:    true,
			setupMock:      func(m *MockOpenNebulaVolumeProviderTestify) {},
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			mockProvider := &MockOpenNebulaVolumeProviderTestify{}
			if tc.setupMock != nil {
				tc.setupMock(mockProvider)
			}

			cs := getTestControllerServer(mockProvider)
			response, err := cs.CreateVolume(context.Background(), tc.createVolumeRequest)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse.Volume.CapacityBytes, response.Volume.CapacityBytes)
				assert.Equal(t, tc.expectResponse.Volume.VolumeId, response.Volume.VolumeId)
			} else if !tc.expectError {
				assert.NotNil(t, response)
			}

			mockProvider.AssertExpectations(t)
		})
	}
}

func TestDeleteVolume(t *testing.T) {
	tcs := []struct {
		name                string
		deleteVolumeRequest *csi.DeleteVolumeRequest
		setupMock           func(m *MockOpenNebulaVolumeProviderTestify)
		expectResponse      *csi.DeleteVolumeResponse
		expectError         bool
	}{
		{
			name: "TestBasicVolumeDeletion",
			deleteVolumeRequest: &csi.DeleteVolumeRequest{
				VolumeId: "test-volume-id",
			},
			setupMock: func(m *MockOpenNebulaVolumeProviderTestify) {
				m.On("DeleteVolume", mock.Anything, "test-volume-id").Return(nil)
			},
			expectResponse: &csi.DeleteVolumeResponse{},
			expectError:    false,
		},
		{
			name: "TestDeleteNonExistentVolume",
			deleteVolumeRequest: &csi.DeleteVolumeRequest{
				VolumeId: "non-existent-volume",
			},
			setupMock: func(m *MockOpenNebulaVolumeProviderTestify) {
				m.On("DeleteVolume", mock.Anything, "non-existent-volume").Return(nil)
			},
			expectResponse: &csi.DeleteVolumeResponse{},
			expectError:    false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			mockProvider := new(MockOpenNebulaVolumeProviderTestify)
			if tc.setupMock != nil {
				tc.setupMock(mockProvider)
			}

			cs := getTestControllerServer(mockProvider)
			response, err := cs.DeleteVolume(context.Background(), tc.deleteVolumeRequest)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse, response)
			} else if !tc.expectError {
				assert.NotNil(t, response)
			}

			mockProvider.AssertExpectations(t)
		})
	}
}

func TestControllerPublishVolume(t *testing.T) {
	tcs := []struct {
		name                           string
		controllerPublishVolumeRequest *csi.ControllerPublishVolumeRequest
		setupMock                      func(m *MockOpenNebulaVolumeProviderTestify)
		expectResponse                 *csi.ControllerPublishVolumeResponse
		expectError                    bool
	}{
		{
			name: "TestBasicVolumeAttach",
			controllerPublishVolumeRequest: &csi.ControllerPublishVolumeRequest{
				VolumeId: "1234",
				NodeId:   "test-node-id",
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
			},
			setupMock: func(m *MockOpenNebulaVolumeProviderTestify) {
				m.On("VolumeExists", mock.Anything, "1234").Return(1, 1, nil)
				m.On("NodeExists", mock.Anything, "test-node-id").Return(1, nil)
				m.On("GetVolumeInNode", mock.Anything, 1, 1).Once().Return("", errors.New("volume not attached to node"))
				m.On("AttachVolume", mock.Anything, "1234", "test-node-id", false, mock.Anything).Return(nil)
				m.On("GetVolumeInNode", mock.Anything, 1, 1).Once().Return("attached-volume", nil)
			},
			expectResponse: &csi.ControllerPublishVolumeResponse{
				PublishContext: map[string]string{
					"volumeName": "attached-volume",
				},
			},
			expectError: false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			mockProvider := new(MockOpenNebulaVolumeProviderTestify)
			if tc.setupMock != nil {
				tc.setupMock(mockProvider)
			}

			cs := getTestControllerServer(mockProvider)
			response, err := cs.ControllerPublishVolume(context.Background(), tc.controllerPublishVolumeRequest)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse, response)
			} else if !tc.expectError {
				assert.NotNil(t, response)
			}

			mockProvider.AssertExpectations(t)
		})
	}
}

func TestListVolumes(t *testing.T) {
	tcs := []struct {
		name               string
		listVolumesRequest *csi.ListVolumesRequest
		setupMock          func(m *MockOpenNebulaVolumeProviderTestify)
		expectError        bool
		expectResponse     *csi.ListVolumesResponse
		expectVolumeCount  int
	}{
		{
			name: "TestListAllVolumes",
			listVolumesRequest: &csi.ListVolumesRequest{
				MaxEntries: 10,
			},
			setupMock: func(m *MockOpenNebulaVolumeProviderTestify) {
				m.On("ListVolumes", mock.Anything, "csi.opennebula.io", int32(10), "").Return([]string{"1"}, nil)
			},
			expectError: false,
			expectResponse: &csi.ListVolumesResponse{
				Entries: []*csi.ListVolumesResponse_Entry{
					{
						Volume: &csi.Volume{
							VolumeId: "1",
						},
					},
				},
			},
			expectVolumeCount: 1,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			mockProvider := new(MockOpenNebulaVolumeProviderTestify)
			if tc.setupMock != nil {
				tc.setupMock(mockProvider)
			}

			cs := getTestControllerServer(mockProvider)
			response, err := cs.ListVolumes(context.Background(), tc.listVolumesRequest)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, response)
				assert.Equal(t, tc.expectResponse, response)
				assert.Equal(t, tc.expectVolumeCount, len(response.Entries))
			}

			mockProvider.AssertExpectations(t)
		})
	}
}

func TestGetCapacity(t *testing.T) {
	tcs := []struct {
		name               string
		getCapacityRequest *csi.GetCapacityRequest
		setupMock          func(m *MockOpenNebulaVolumeProviderTestify)
		expectError        bool
		expectResponse     *csi.GetCapacityResponse
		expectCapacity     int64
	}{
		{
			name: "TestGetCapacity",
			getCapacityRequest: &csi.GetCapacityRequest{
				VolumeCapabilities: []*csi.VolumeCapability{
					{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								FsType: "ext4",
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
						},
					},
				},
			},
			setupMock: func(m *MockOpenNebulaVolumeProviderTestify) {
				m.On("GetCapacity", mock.Anything).Return(int64(datastoreSize), nil)
			},
			expectError: false,
			expectResponse: &csi.GetCapacityResponse{
				AvailableCapacity: datastoreSize,
			},
			expectCapacity: datastoreSize,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			mockProvider := new(MockOpenNebulaVolumeProviderTestify)
			if tc.setupMock != nil {
				tc.setupMock(mockProvider)
			}

			cs := getTestControllerServer(mockProvider)
			response, err := cs.GetCapacity(context.Background(), tc.getCapacityRequest)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, response)
				assert.Equal(t, tc.expectResponse, response)
				assert.Equal(t, tc.expectCapacity, response.AvailableCapacity)
			}

			mockProvider.AssertExpectations(t)
		})
	}
}

func TestControllerUnpublishVolume(t *testing.T) {
	tcs := []struct {
		name                             string
		controllerUnpublishVolumeRequest *csi.ControllerUnpublishVolumeRequest
		setupMock                        func(m *MockOpenNebulaVolumeProviderTestify)
		expectResponse                   *csi.ControllerUnpublishVolumeResponse
		expectError                      bool
	}{
		{
			name: "TestBasicVolumeDetach",
			controllerUnpublishVolumeRequest: &csi.ControllerUnpublishVolumeRequest{
				VolumeId: "test-volume-id",
				NodeId:   "test-node-id",
			},
			setupMock: func(m *MockOpenNebulaVolumeProviderTestify) {
				m.On("VolumeExists", mock.Anything, "test-volume-id").Return(1, 1, nil)
				m.On("NodeExists", mock.Anything, "test-node-id").Return(42, nil)
				m.On("GetVolumeInNode", mock.Anything, 1, 42).Return("attached-target", nil)
				m.On("DetachVolume", mock.Anything, "test-volume-id", "test-node-id").Return(nil)
			},
			expectResponse: &csi.ControllerUnpublishVolumeResponse{},
			expectError:    false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			mockProvider := new(MockOpenNebulaVolumeProviderTestify)
			if tc.setupMock != nil {
				tc.setupMock(mockProvider)
			}

			cs := getTestControllerServer(mockProvider)
			response, err := cs.ControllerUnpublishVolume(context.Background(), tc.controllerUnpublishVolumeRequest)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse, response)
			} else if !tc.expectError {
				assert.NotNil(t, response)
			}

			mockProvider.AssertExpectations(t)
		})
	}
}

type MockOpenNebulaVolumeProviderTestify struct {
	mock.Mock
}

func (m *MockOpenNebulaVolumeProviderTestify) CreateVolume(ctx context.Context, name string, size int64, owner string, immutable bool, fsType string, params map[string]string) error {
	args := m.Called(ctx, name, size, owner, immutable, fsType, params)
	return args.Error(0)
}

func (m *MockOpenNebulaVolumeProviderTestify) DeleteVolume(ctx context.Context, volume string) error {
	args := m.Called(ctx, volume)
	return args.Error(0)
}

func (m *MockOpenNebulaVolumeProviderTestify) AttachVolume(ctx context.Context, volume string, node string, immutable bool, params map[string]string) error {
	args := m.Called(ctx, volume, node, immutable, params)
	return args.Error(0)
}

func (m *MockOpenNebulaVolumeProviderTestify) DetachVolume(ctx context.Context, volume string, node string) error {
	args := m.Called(ctx, volume, node)
	return args.Error(0)
}

func (m *MockOpenNebulaVolumeProviderTestify) ListVolumes(ctx context.Context, volume string, maxEntries int32, startingToken string) ([]string, error) {
	args := m.Called(ctx, volume, maxEntries, startingToken)
	return args.Get(0).([]string), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) GetCapacity(ctx context.Context) (int64, error) {
	args := m.Called(ctx)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) VolumeExists(ctx context.Context, volume string) (int, int, error) {
	args := m.Called(ctx, volume)
	return args.Get(0).(int), args.Get(1).(int), args.Error(2)
}

func (m *MockOpenNebulaVolumeProviderTestify) NodeExists(ctx context.Context, node string) (int, error) {
	args := m.Called(ctx, node)
	return args.Get(0).(int), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) GetVolumeInNode(ctx context.Context, volumeID int, nodeID int) (string, error) {
	args := m.Called(ctx, volumeID, nodeID)
	return args.Get(0).(string), args.Error(1)
}
