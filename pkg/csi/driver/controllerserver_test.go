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
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	inventoryv1alpha1 "github.com/SparkAIUR/storage-provider-opennebula/pkg/inventory/apis/storageprovider/v1alpha1"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	volumeSize    = 10 * 1024 * 1024
	datastoreSize = 100 * 1024 * 1024 * 1024
)

func getTestControllerServer(mockProvider *MockOpenNebulaVolumeProviderTestify) *ControllerServer {
	return getTestControllerServerWithAllowedTypes(mockProvider, &MockSharedFilesystemProviderTestify{}, "local")
}

func getTestControllerServerWithAllowedTypes(mockProvider *MockOpenNebulaVolumeProviderTestify, sharedProvider *MockSharedFilesystemProviderTestify, allowedTypes string) *ControllerServer {
	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.DefaultDatastoresVar, "100,101")
	pluginConfig.OverrideVal(config.AllowedDatastoreTypesVar, allowedTypes)

	driver := &Driver{
		name:               DefaultDriverName,
		version:            driverVersion,
		grpcServerEndpoint: DefaultGRPCServerEndpoint,
		nodeID:             "test-controller-id",
		PluginConfig:       pluginConfig,
		metrics:            NewDriverMetrics(driverVersion, "test"),
		hotplugGuard:       NewHotplugGuard(5 * time.Minute),
		operationLocks:     NewOperationLocks(),
	}

	return NewControllerServer(driver, mockProvider, sharedProvider)
}

func getTestControllerServerWithDriver(mockProvider *MockOpenNebulaVolumeProviderTestify, sharedProvider *MockSharedFilesystemProviderTestify, driver *Driver) *ControllerServer {
	return NewControllerServer(driver, mockProvider, sharedProvider)
}

func newTestDriver() *Driver {
	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.DefaultDatastoresVar, "100,101")
	pluginConfig.OverrideVal(config.AllowedDatastoreTypesVar, "local")

	return &Driver{
		name:               DefaultDriverName,
		version:            driverVersion,
		grpcServerEndpoint: DefaultGRPCServerEndpoint,
		nodeID:             "test-controller-id",
		PluginConfig:       pluginConfig,
		metrics:            NewDriverMetrics(driverVersion, "test"),
		featureGates:       FeatureGates{DetachedDiskExpansion: true},
		hotplugGuard:       NewHotplugGuard(5 * time.Minute),
		operationLocks:     NewOperationLocks(),
	}
}

func newStickyTestDriver(t *testing.T, objects ...runtime.Object) *Driver {
	t.Helper()
	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.DefaultDatastoresVar, "100,101")
	pluginConfig.OverrideVal(config.AllowedDatastoreTypesVar, "local")
	pluginConfig.OverrideVal(config.LocalRestartOptimizationEnabledVar, true)
	pluginConfig.OverrideVal(config.LocalRestartDetachGraceSecondsVar, 90)
	pluginConfig.OverrideVal(config.LocalRestartDetachGraceMaxSecondsVar, 300)
	pluginConfig.OverrideVal(config.LocalRestartRequireNodeReadyVar, false)

	client := fake.NewSimpleClientset(objects...)
	runtime := &KubeRuntime{client: client, enabled: true}

	driver := &Driver{
		name:               DefaultDriverName,
		version:            driverVersion,
		grpcServerEndpoint: DefaultGRPCServerEndpoint,
		nodeID:             "",
		PluginConfig:       pluginConfig,
		metrics:            NewDriverMetrics(driverVersion, "test"),
		hotplugGuard:       NewHotplugGuard(5 * time.Minute),
		operationLocks:     NewOperationLocks(),
		kubeRuntime:        runtime,
	}
	driver.stickyAttachments = NewStickyAttachmentManager(runtime, "default")
	driver.volumeQuarantine = NewVolumeQuarantineManager(runtime, "default")
	driver.hostArtifactQuarantine = NewHostArtifactQuarantineManager(runtime, "default")
	require.NoError(t, driver.stickyAttachments.LoadFromConfigMap(context.Background()))
	return driver
}

func newInventoryFakeClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, inventoryv1alpha1.AddToScheme(scheme))
	return ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func newLocalPVAndPVC(volumeHandle string, accessModes []corev1.PersistentVolumeAccessMode, pvcAnnotations map[string]string) (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim) {
	pvAnnotations := map[string]string{
		annotationBackend:         "local",
		annotationDatastoreID:     "1",
		annotationDatastoreName:   "default",
		annotationSelectionPolicy: "least-used",
	}
	for key, value := range pvcAnnotations {
		if key == annotationBackend || key == annotationDatastoreID || key == annotationDatastoreName || key == annotationSelectionPolicy {
			pvAnnotations[key] = value
		}
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pv-" + volumeHandle,
			Annotations: pvAnnotations,
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: accessModes,
			ClaimRef: &corev1.ObjectReference{
				Namespace: "default",
				Name:      "pvc-" + volumeHandle,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       DefaultDriverName,
					VolumeHandle: volumeHandle,
				},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "pvc-" + volumeHandle,
			Annotations: pvcAnnotations,
		},
	}
	return pv, pvc
}

func newReadyNode(name string, ready bool) *corev1.Node {
	conditionStatus := corev1.ConditionFalse
	if ready {
		conditionStatus = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: conditionStatus,
			}},
		},
	}
}

func newPublishReq(volumeID, nodeID string) *csi.ControllerPublishVolumeRequest {
	return &csi.ControllerPublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   nodeID,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
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
					storageClassParamDatastoreIDs: "100",
					"type":                        "BLOCK",
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
				m.On(
					"CreateVolume",
					mock.Anything,
					"test-volume",
					volumeSize,
					mock.Anything,
					false,
					"",
					map[string]string{"type": "BLOCK"},
					opennebula.DatastoreSelectionConfig{
						Identifiers:  []string{"100"},
						Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
						AllowedTypes: []string{"local"},
					},
				).Return(&opennebula.VolumeCreateResult{Datastore: opennebula.Datastore{ID: 100, Name: "ds-100"}}, nil)
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

func TestCreateVolumeAddsUUIDToVolumeContext(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	cs := getTestControllerServer(mockProvider)

	mockProvider.On("VolumeExists", mock.Anything, "test-volume").Return(-1, -1, nil)
	mockProvider.On(
		"CreateVolume",
		mock.Anything,
		"test-volume",
		int64(1024*1024*1024),
		mock.Anything,
		false,
		"",
		map[string]string{"type": "BLOCK"},
		opennebula.DatastoreSelectionConfig{
			Identifiers:  []string{"100"},
			Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
			AllowedTypes: []string{"local"},
		},
	).Return(&opennebula.VolumeCreateResult{Datastore: opennebula.Datastore{ID: 100, Name: "ds-100", Backend: "local"}}, nil)

	resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "test-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(1024 * 1024 * 1024),
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
			"type":                        "BLOCK",
			paramPVCUID:                   "20e586e1-da88-448a-a6ed-1c855822bc75",
			paramPVCName:                  "data-pvc",
			paramPVCNamespace:             "vela",
			paramPVName:                   "pvc-20e586e1-da88-448a-a6ed-1c855822bc75",
		},
	})

	require.NoError(t, err)
	require.NotNil(t, resp.GetVolume())
	assert.Equal(t, "20e586e1-da88-448a-a6ed-1c855822bc75", resp.GetVolume().GetVolumeContext()[volumeContextUUID])
	assert.Equal(t, "20e586e1-da88-448a-a6ed-1c855822bc75", resp.GetVolume().GetVolumeContext()[paramPVCUID])
	assert.Equal(t, "data-pvc", resp.GetVolume().GetVolumeContext()[paramPVCName])
	assert.Equal(t, "vela", resp.GetVolume().GetVolumeContext()[paramPVCNamespace])
	assert.Equal(t, "pvc-20e586e1-da88-448a-a6ed-1c855822bc75", resp.GetVolume().GetVolumeContext()[paramPVName])
	assert.Equal(t, "local", resp.GetVolume().GetVolumeContext()[annotationBackend])
	assert.Equal(t, "100", resp.GetVolume().GetVolumeContext()[annotationDatastoreID])
	assert.Equal(t, "ds-100", resp.GetVolume().GetVolumeContext()[annotationDatastoreName])
	assert.Equal(t, "least-used", resp.GetVolume().GetVolumeContext()[annotationSelectionPolicy])
	assert.Equal(t, "BLOCK", resp.GetVolume().GetVolumeContext()["type"])
	mockProvider.AssertExpectations(t)
}

func TestPublishContextForVolumeIncludesDedicatedDeviceDiscoveryTimeout(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	cs := getTestControllerServer(mockProvider)

	publishContext := cs.publishContextForVolume(context.Background(), "pvc-1", "sdd", 30*time.Second, map[string]string{
		paramPVCName:      "data-pvc",
		paramPVCNamespace: "default",
		paramPVName:       "pv-data-pvc",
	})

	assert.Equal(t, "sdd", publishContext["volumeName"])
	assert.Equal(t, "180", publishContext[publishContextHotplugTimeoutSeconds])
	assert.Equal(t, "30", publishContext[publishContextDeviceDiscoveryTimeoutSeconds])
	assert.Equal(t, "1", publishContext[publishContextOpenNebulaImageID])
	assert.Equal(t, "onecsi-1", publishContext[publishContextDeviceSerial])
	assert.Equal(t, "data-pvc", publishContext[paramPVCName])
	assert.Equal(t, "default", publishContext[paramPVCNamespace])
	assert.Equal(t, "pv-data-pvc", publishContext[paramPVName])
}

func TestNodeDeviceDiscoveryTimeoutCapsToHotplugTimeout(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	cs := getTestControllerServer(mockProvider)
	cs.driver.PluginConfig.OverrideVal(config.NodeDeviceDiscoveryTimeoutVar, 30)

	timeout := cs.nodeDeviceDiscoveryTimeout("disk", 1, 15*time.Second)

	assert.Equal(t, 15*time.Second, timeout)
}

func TestCreateVolumeCreatesSharedFilesystemVolumeForRWX(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")
	mockProvider.On("ResolveProvisioningDatastores", mock.Anything, opennebula.DatastoreSelectionConfig{
		Identifiers:  []string{"100"},
		Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
		AllowedTypes: []string{"local", "cephfs"},
	}).Return([]opennebula.Datastore{{
		ID:      100,
		Name:    "cephfs-file",
		Backend: "cephfs",
		Type:    "cephfs",
	}}, nil)

	sharedProvider.On("CreateSharedVolume", mock.Anything, opennebula.SharedVolumeRequest{
		Name:      "rwx-volume",
		SizeBytes: int64(1024 * 1024 * 1024),
		Selection: opennebula.DatastoreSelectionConfig{
			Identifiers:  []string{"100"},
			Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
			AllowedTypes: []string{"local", "cephfs"},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
		Secrets: map[string]string{
			"adminID":  "csi-admin",
			"adminKey": "super-secret",
		},
	}).Return(&opennebula.SharedVolumeCreateResult{
		VolumeID:      "cephfs:eyJiYWNrZW5kIjoiY2VwaGZzIiwiZGF0YXN0b3JlSUQiOjEwMCwiZnNOYW1lIjoiY2VwaGZzIiwibW9kZSI6ImR5bmFtaWMiLCJzdWJ2b2x1bWVHcm91cCI6ImNzaSIsInN1YnBhdGgiOiIvdm9sdW1lcy9jc2ktcHZjIn0",
		CapacityBytes: int64(1024 * 1024 * 1024),
		Datastore:     opennebula.Datastore{ID: 100, Name: "cephfs-file"},
		Metadata: opennebula.SharedVolumeMetadata{
			Backend: "cephfs",
			Mode:    opennebula.SharedVolumeModeDynamic,
		},
	}, nil)

	resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "rwx-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(1024 * 1024 * 1024),
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
		Secrets: map[string]string{
			"adminID":  "csi-admin",
			"adminKey": "super-secret",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, "cephfs:eyJiYWNrZW5kIjoiY2VwaGZzIiwiZGF0YXN0b3JlSUQiOjEwMCwiZnNOYW1lIjoiY2VwaGZzIiwibW9kZSI6ImR5bmFtaWMiLCJzdWJ2b2x1bWVHcm91cCI6ImNzaSIsInN1YnBhdGgiOiIvdm9sdW1lcy9jc2ktcHZjIn0", resp.GetVolume().GetVolumeId())
	assert.Equal(t, resp.GetVolume().GetVolumeId(), resp.GetVolume().GetVolumeContext()[volumeContextUUID])
	assert.Equal(t, "cephfs", resp.GetVolume().GetVolumeContext()[annotationBackend])
	assert.Equal(t, "100", resp.GetVolume().GetVolumeContext()[annotationDatastoreID])
	assert.Equal(t, "cephfs-file", resp.GetVolume().GetVolumeContext()[annotationDatastoreName])
	assert.Equal(t, "least-used", resp.GetVolume().GetVolumeContext()[annotationSelectionPolicy])
	mockProvider.AssertExpectations(t)
	sharedProvider.AssertExpectations(t)
}

func TestCreateVolumeCreatesSharedFilesystemVolumeForCephFSRWO(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")
	mockProvider.On("ResolveProvisioningDatastores", mock.Anything, opennebula.DatastoreSelectionConfig{
		Identifiers:  []string{"100"},
		Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
		AllowedTypes: []string{"local", "cephfs"},
	}).Return([]opennebula.Datastore{{
		ID:      100,
		Name:    "cephfs-file",
		Backend: "cephfs",
		Type:    "cephfs",
	}}, nil)

	sharedProvider.On("CreateSharedVolume", mock.Anything, opennebula.SharedVolumeRequest{
		Name:      "rwo-cephfs",
		SizeBytes: int64(1024 * 1024 * 1024),
		Selection: opennebula.DatastoreSelectionConfig{
			Identifiers:  []string{"100"},
			Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
			AllowedTypes: []string{"local", "cephfs"},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs:          "100",
			storageClassParamSharedFilesystemGroup: "csi",
		},
		Secrets: map[string]string{
			"adminID":  "csi-admin",
			"adminKey": "super-secret",
		},
	}).Return(&opennebula.SharedVolumeCreateResult{
		VolumeID:      "cephfs:rwo",
		CapacityBytes: int64(1024 * 1024 * 1024),
		Datastore:     opennebula.Datastore{ID: 100, Name: "cephfs-file"},
		Metadata: opennebula.SharedVolumeMetadata{
			Backend: "cephfs",
			Mode:    opennebula.SharedVolumeModeDynamic,
		},
	}, nil)

	resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "rwo-cephfs",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(1024 * 1024 * 1024),
		},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		}},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs:          "100",
			storageClassParamSharedFilesystemGroup: "csi",
		},
		Secrets: map[string]string{
			"adminID":  "csi-admin",
			"adminKey": "super-secret",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, "cephfs:rwo", resp.GetVolume().GetVolumeId())
	mockProvider.AssertExpectations(t)
	sharedProvider.AssertExpectations(t)
}

func TestCreateVolumeRejectsMixedCephFSAndDiskDatastores(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")
	mockProvider.On("ResolveProvisioningDatastores", mock.Anything, opennebula.DatastoreSelectionConfig{
		Identifiers:  []string{"100"},
		Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
		AllowedTypes: []string{"local", "cephfs"},
	}).Return([]opennebula.Datastore{
		{ID: 100, Name: "cephfs-file", Backend: "cephfs", Type: "cephfs"},
		{ID: 101, Name: "lvm-local", Backend: "local", Type: "local"},
	}, nil)

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "mixed-cephfs",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(1024 * 1024 * 1024),
		},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		}},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs:          "100",
			storageClassParamSharedFilesystemGroup: "csi",
		},
	})

	assert.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "mixes CephFS and disk datastores")
	mockProvider.AssertExpectations(t)
	sharedProvider.AssertExpectations(t)
}

func TestControllerGetCapabilitiesIncludesCloneVolume(t *testing.T) {
	cs := getTestControllerServer(&MockOpenNebulaVolumeProviderTestify{})

	resp, err := cs.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	assert.NoError(t, err)

	var cloneCap bool
	for _, capability := range resp.GetCapabilities() {
		rpc := capability.GetRpc()
		if rpc == nil {
			continue
		}
		if rpc.GetType() == csi.ControllerServiceCapability_RPC_CLONE_VOLUME {
			cloneCap = true
			break
		}
	}

	assert.True(t, cloneCap, "controller must advertise CLONE_VOLUME capability")
}

func TestCreateVolumeIncludesAccessibleTopologyWhenEnabled(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.DefaultDatastoresVar, "100")
	pluginConfig.OverrideVal(config.AllowedDatastoreTypesVar, "local")

	driver := &Driver{
		name:               DefaultDriverName,
		version:            driverVersion,
		grpcServerEndpoint: DefaultGRPCServerEndpoint,
		nodeID:             "test-controller-id",
		PluginConfig:       pluginConfig,
		featureGates: FeatureGates{
			TopologyAccessibility: true,
		},
	}

	mockProvider.On("VolumeExists", mock.Anything, "topology-volume").Return(-1, -1, nil)
	mockProvider.On(
		"CreateVolume",
		mock.Anything,
		"topology-volume",
		int64(1024*1024*1024),
		mock.Anything,
		false,
		"",
		map[string]string{},
		opennebula.DatastoreSelectionConfig{
			Identifiers:  []string{"100"},
			Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
			AllowedTypes: []string{"local"},
		},
	).Return(&opennebula.VolumeCreateResult{
		Datastore: opennebula.Datastore{
			ID:                         100,
			Name:                       "image-ds",
			CompatibleSystemDatastores: []int{111, 112},
		},
		CapacityBytes: int64(1024 * 1024 * 1024),
	}, nil)

	cs := getTestControllerServerWithDriver(mockProvider, sharedProvider, driver)
	resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "topology-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(1024 * 1024 * 1024),
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
	})

	assert.NoError(t, err)
	if assert.Len(t, resp.GetVolume().GetAccessibleTopology(), 2) {
		assert.Equal(t, "111", resp.GetVolume().GetAccessibleTopology()[0].Segments[topologySystemDSLabel])
		assert.Equal(t, "112", resp.GetVolume().GetAccessibleTopology()[1].Segments[topologySystemDSLabel])
	}
	mockProvider.AssertExpectations(t)
}

func TestCreateVolumeRejectsReadWriteManyBlockVolume(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "rwx-block-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(1024 * 1024 * 1024),
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Block{
					Block: &csi.VolumeCapability_BlockVolume{},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
	})

	assert.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "filesystem volume capability")
	mockProvider.AssertExpectations(t)
	sharedProvider.AssertExpectations(t)
}

func TestCreateVolumeClonesFromSourceVolume(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local")

	mockProvider.On("CloneVolume", mock.Anything, "clone-volume", "source-volume", opennebula.DatastoreSelectionConfig{
		Identifiers:  []string{"100"},
		Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
		AllowedTypes: []string{"local"},
	}).Return(&opennebula.VolumeCreateResult{
		Datastore:     opennebula.Datastore{ID: 100, Name: "fast-local", Backend: "local"},
		CapacityBytes: int64(1024 * 1024 * 1024),
	}, nil)

	resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "clone-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(1024 * 1024 * 1024),
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: "source-volume"},
			},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, "clone-volume", resp.GetVolume().GetVolumeId())
	assert.Equal(t, int64(1024*1024*1024), resp.GetVolume().GetCapacityBytes())
	if assert.NotNil(t, resp.GetVolume().GetContentSource()) {
		assert.Equal(t, "source-volume", resp.GetVolume().GetContentSource().GetVolume().GetVolumeId())
	}
	mockProvider.AssertExpectations(t)
}

func TestCreateVolumeRejectsSnapshotRestoreForDiskPath(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local")

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "restore-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(1024 * 1024 * 1024),
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "image-snapshot:10:1"},
			},
		},
	})

	assert.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
	assert.Contains(t, err.Error(), "snapshot")
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
				m.On("GetVolumeInNode", mock.Anything, 1, 1).Twice().Return("", errors.New("volume not attached to node"))
				m.On("AttachVolume", mock.Anything, "1234", "test-node-id", false, mock.Anything).Return(nil)
				m.On("GetVolumeInNode", mock.Anything, 1, 1).Once().Return("attached-volume", nil)
			},
			expectResponse: &csi.ControllerPublishVolumeResponse{
				PublishContext: map[string]string{
					"volumeName":                                "attached-volume",
					publishContextDeviceSerial:                  "onecsi-1",
					publishContextOpenNebulaImageID:             "1",
					publishContextHotplugTimeoutSeconds:         "180",
					publishContextDeviceDiscoveryTimeoutSeconds: "30",
				},
			},
			expectError: false,
		},
		{
			name: "TestCephAttachValidationFailureMapsToFailedPrecondition",
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
				m.On("GetVolumeInNode", mock.Anything, 1, 1).Twice().Return("", errors.New("volume not attached to node"))
				m.On("AttachVolume", mock.Anything, "1234", "test-node-id", false, mock.Anything).
					Return(opennebula.NewDatastoreConfigError("ceph datastore mismatch"))
			},
			expectResponse: nil,
			expectError:    true,
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
				if tc.name == "TestCephAttachValidationFailureMapsToFailedPrecondition" {
					assert.Equal(t, codes.FailedPrecondition, status.Code(err))
				}
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

func TestControllerPublishVolumeReturnsSharedFilesystemPublishContext(t *testing.T) {
	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	sharedProvider := new(MockSharedFilesystemProviderTestify)
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")

	mockProvider.On("NodeExists", mock.Anything, "test-node-id").Return(1, nil)
	sharedProvider.On("PublishSharedVolume", mock.Anything, mock.Anything, false).Return(map[string]string{
		"shareBackend":   "cephfs",
		"cephfsMonitors": "mon1,mon2",
		"cephfsFSName":   "cephfs-prod",
		"cephfsSubpath":  "/kubernetes/dynamic/one-csi-demo",
		"cephfsReadonly": "false",
	}, nil)

	resp, err := cs.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: "cephfs:eyJiYWNrZW5kIjoiY2VwaGZzIiwiZGF0YXN0b3JlSUQiOjMwMCwiZnNOYW1lIjoiY2VwaGZzLXByb2QiLCJtb2RlIjoiZHluYW1pYyIsInN1YnZvbHVtZUdyb3VwIjoiY3NpIiwic3VicGF0aCI6Ii9rdWJlcm5ldGVzL2R5bmFtaWMvb25lLWNzaS1kZW1vIiwic3Vidm9sdW1lTmFtZSI6Im9uZS1jc2ktZGVtbyJ9",
		NodeId:   "test-node-id",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, "cephfs", resp.GetPublishContext()["shareBackend"])
	mockProvider.AssertExpectations(t)
	sharedProvider.AssertExpectations(t)
}

func TestDeleteVolumeRoutesSharedFilesystemVolumes(t *testing.T) {
	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	sharedProvider := new(MockSharedFilesystemProviderTestify)
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")

	sharedProvider.On("DeleteSharedVolume", mock.Anything, mock.Anything, map[string]string{
		"adminID":  "csi-admin",
		"adminKey": "super-secret",
	}).Return(nil)

	resp, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "cephfs:eyJiYWNrZW5kIjoiY2VwaGZzIiwiZGF0YXN0b3JlSUQiOjMwMCwiZnNOYW1lIjoiY2VwaGZzLXByb2QiLCJtb2RlIjoiZHluYW1pYyIsInN1YnZvbHVtZUdyb3VwIjoiY3NpIiwic3VicGF0aCI6Ii9rdWJlcm5ldGVzL2R5bmFtaWMvb25lLWNzaS1kZW1vIiwic3Vidm9sdW1lTmFtZSI6Im9uZS1jc2ktZGVtbyJ9",
		Secrets: map[string]string{
			"adminID":  "csi-admin",
			"adminKey": "super-secret",
		},
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	mockProvider.AssertExpectations(t)
	sharedProvider.AssertExpectations(t)
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
				m.On(
					"GetCapacity",
					mock.Anything,
					opennebula.DatastoreSelectionConfig{
						Identifiers:  []string{"100", "101"},
						Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
						AllowedTypes: []string{"local"},
					},
				).Return(int64(datastoreSize), nil)
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

func TestControllerExpandVolume(t *testing.T) {
	const expandedSize = int64(2 * 1024 * 1024 * 1024)

	tcs := []struct {
		name             string
		request          *csi.ControllerExpandVolumeRequest
		setupMock        func(m *MockOpenNebulaVolumeProviderTestify)
		expectError      bool
		expectNodeExpand bool
	}{
		{
			name: "FilesystemVolumeRequiresNodeExpansion",
			request: &csi.ControllerExpandVolumeRequest{
				VolumeId: "test-volume",
				CapacityRange: &csi.CapacityRange{
					RequiredBytes: expandedSize,
				},
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType: "ext4",
						},
					},
				},
			},
			setupMock: func(m *MockOpenNebulaVolumeProviderTestify) {
				m.On("ExpandVolume", mock.Anything, "test-volume", expandedSize, false).Return(expandedSize, nil)
			},
			expectNodeExpand: true,
		},
		{
			name: "BlockVolumeSkipsNodeExpansion",
			request: &csi.ControllerExpandVolumeRequest{
				VolumeId: "test-volume",
				CapacityRange: &csi.CapacityRange{
					RequiredBytes: expandedSize,
				},
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Block{
						Block: &csi.VolumeCapability_BlockVolume{},
					},
				},
			},
			setupMock: func(m *MockOpenNebulaVolumeProviderTestify) {
				m.On("ExpandVolume", mock.Anything, "test-volume", expandedSize, false).Return(expandedSize, nil)
			},
			expectNodeExpand: false,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			mockProvider := new(MockOpenNebulaVolumeProviderTestify)
			if tc.setupMock != nil {
				tc.setupMock(mockProvider)
			}

			cs := getTestControllerServer(mockProvider)
			response, err := cs.ControllerExpandVolume(context.Background(), tc.request)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, expandedSize, response.CapacityBytes)
				assert.Equal(t, tc.expectNodeExpand, response.NodeExpansionRequired)
			}

			mockProvider.AssertExpectations(t)
		})
	}
}

func TestCreateSnapshot(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	cs := getTestControllerServer(mockProvider)
	now := time.Now().UTC()

	mockProvider.On("CreateSnapshot", mock.Anything, "test-volume", "snap-1").Return(&opennebula.VolumeSnapshot{
		SnapshotID:     "image-snapshot:10:1",
		SourceVolumeID: "test-volume",
		CreationTime:   now,
		SizeBytes:      int64(1024 * 1024 * 1024),
		ReadyToUse:     true,
	}, nil)

	resp, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snap-1",
		SourceVolumeId: "test-volume",
	})

	assert.NoError(t, err)
	assert.Equal(t, "image-snapshot:10:1", resp.GetSnapshot().GetSnapshotId())
	assert.Equal(t, "test-volume", resp.GetSnapshot().GetSourceVolumeId())
	mockProvider.AssertExpectations(t)
}

func TestCreateSnapshotCreatesSharedFilesystemSnapshotWhenEnabled(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")
	cs.driver.featureGates.CephFSSnapshots = true
	now := time.Now().UTC()

	sharedProvider.On("CreateSharedSnapshot", mock.Anything, "cephfs:source", "snap-1", map[string]string{
		"adminID":  "csi-admin",
		"adminKey": "super-secret",
	}).Return(&opennebula.VolumeSnapshot{
		SnapshotID:     "cephfs-snapshot:test",
		SourceVolumeID: "cephfs:source",
		CreationTime:   now,
		SizeBytes:      int64(1024 * 1024 * 1024),
		ReadyToUse:     true,
	}, nil)

	resp, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snap-1",
		SourceVolumeId: "cephfs:source",
		Secrets: map[string]string{
			"adminID":  "csi-admin",
			"adminKey": "super-secret",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, "cephfs-snapshot:test", resp.GetSnapshot().GetSnapshotId())
	sharedProvider.AssertExpectations(t)
}

func TestDeleteSnapshot(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	cs := getTestControllerServer(mockProvider)

	mockProvider.On("DeleteSnapshot", mock.Anything, "image-snapshot:10:1").Return(nil)

	resp, err := cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{
		SnapshotId: "image-snapshot:10:1",
	})

	assert.NoError(t, err)
	assert.Equal(t, &csi.DeleteSnapshotResponse{}, resp)
	mockProvider.AssertExpectations(t)
}

func TestDeleteSnapshotDeletesSharedFilesystemSnapshotWhenEnabled(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")
	cs.driver.featureGates.CephFSSnapshots = true

	sharedProvider.On("DeleteSharedSnapshot", mock.Anything, "cephfs-snapshot:test", mock.Anything).Return(nil)

	resp, err := cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{
		SnapshotId: "cephfs-snapshot:test",
	})

	assert.NoError(t, err)
	assert.Equal(t, &csi.DeleteSnapshotResponse{}, resp)
	sharedProvider.AssertExpectations(t)
}

func TestListSnapshots(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	cs := getTestControllerServer(mockProvider)
	now := time.Now().UTC()

	mockProvider.On("ListSnapshots", mock.Anything, "", "test-volume", int32(10), "").Return([]opennebula.VolumeSnapshot{
		{
			SnapshotID:     "image-snapshot:10:1",
			SourceVolumeID: "test-volume",
			CreationTime:   now,
			SizeBytes:      int64(1024 * 1024 * 1024),
			ReadyToUse:     true,
		},
	}, "", nil)

	resp, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SourceVolumeId: "test-volume",
		MaxEntries:     10,
	})

	assert.NoError(t, err)
	if assert.Len(t, resp.GetEntries(), 1) {
		assert.Equal(t, "image-snapshot:10:1", resp.GetEntries()[0].GetSnapshot().GetSnapshotId())
	}
	mockProvider.AssertExpectations(t)
}

func TestCreateVolumeClonesSharedFilesystemVolumeWhenEnabled(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")
	cs.driver.featureGates.CephFSClones = true
	mockProvider.On("ResolveProvisioningDatastores", mock.Anything, opennebula.DatastoreSelectionConfig{
		Identifiers:  []string{"100"},
		Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
		AllowedTypes: []string{"local", "cephfs"},
	}).Return([]opennebula.Datastore{{
		ID:      100,
		Name:    "cephfs-file",
		Backend: "cephfs",
		Type:    "cephfs",
	}}, nil)

	sharedProvider.On("CloneSharedVolume", mock.Anything, opennebula.SharedVolumeCloneRequest{
		Name:      "rwx-clone",
		SizeBytes: int64(1024 * 1024 * 1024),
		Selection: opennebula.DatastoreSelectionConfig{
			Identifiers:  []string{"100"},
			Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
			AllowedTypes: []string{"local", "cephfs"},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
		Secrets: map[string]string{
			"adminID":  "csi-admin",
			"adminKey": "super-secret",
		},
		SourceVolumeID: "cephfs:source",
	}).Return(&opennebula.SharedVolumeCreateResult{
		VolumeID:      "cephfs:clone",
		CapacityBytes: int64(1024 * 1024 * 1024),
		Datastore:     opennebula.Datastore{ID: 100, Name: "cephfs-file"},
		Metadata:      opennebula.SharedVolumeMetadata{Backend: "cephfs", Mode: opennebula.SharedVolumeModeDynamic},
	}, nil)

	resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "rwx-clone",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(1024 * 1024 * 1024),
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
		Secrets: map[string]string{
			"adminID":  "csi-admin",
			"adminKey": "super-secret",
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: "cephfs:source"},
			},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, "cephfs:clone", resp.GetVolume().GetVolumeId())
	sharedProvider.AssertExpectations(t)
}

func TestCreateVolumeRestoresSharedFilesystemSnapshotWhenEnabled(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")
	cs.driver.featureGates.CephFSClones = true
	cs.driver.featureGates.CephFSSnapshots = true
	mockProvider.On("ResolveProvisioningDatastores", mock.Anything, opennebula.DatastoreSelectionConfig{
		Identifiers:  []string{"100"},
		Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
		AllowedTypes: []string{"local", "cephfs"},
	}).Return([]opennebula.Datastore{{
		ID:      100,
		Name:    "cephfs-file",
		Backend: "cephfs",
		Type:    "cephfs",
	}}, nil)

	sharedProvider.On("CloneSharedVolume", mock.Anything, opennebula.SharedVolumeCloneRequest{
		Name:      "rwx-restore",
		SizeBytes: int64(1024 * 1024 * 1024),
		Selection: opennebula.DatastoreSelectionConfig{
			Identifiers:  []string{"100"},
			Policy:       opennebula.DatastoreSelectionPolicyLeastUsed,
			AllowedTypes: []string{"local", "cephfs"},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
		Secrets: map[string]string{
			"adminID":  "csi-admin",
			"adminKey": "super-secret",
		},
		SourceSnapshotID: "cephfs-snapshot:test",
	}).Return(&opennebula.SharedVolumeCreateResult{
		VolumeID:      "cephfs:restore",
		CapacityBytes: int64(1024 * 1024 * 1024),
		Datastore:     opennebula.Datastore{ID: 100, Name: "cephfs-file"},
		Metadata:      opennebula.SharedVolumeMetadata{Backend: "cephfs", Mode: opennebula.SharedVolumeModeDynamic},
	}, nil)

	resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "rwx-restore",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(1024 * 1024 * 1024),
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			},
		},
		Parameters: map[string]string{
			storageClassParamDatastoreIDs: "100",
		},
		Secrets: map[string]string{
			"adminID":  "csi-admin",
			"adminKey": "super-secret",
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "cephfs-snapshot:test"},
			},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, "cephfs:restore", resp.GetVolume().GetVolumeId())
	sharedProvider.AssertExpectations(t)
}

func TestControllerExpandVolumeExpandsSharedFilesystemWhenEnabled(t *testing.T) {
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	sharedProvider := &MockSharedFilesystemProviderTestify{}
	cs := getTestControllerServerWithAllowedTypes(mockProvider, sharedProvider, "local,cephfs")
	cs.driver.featureGates.CephFSExpansion = true

	sharedProvider.On("ExpandSharedVolume", mock.Anything, "cephfs:volume", int64(2*1024*1024*1024), mock.Anything).Return(int64(2*1024*1024*1024), nil)

	resp, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "cephfs:volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(2 * 1024 * 1024 * 1024),
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
			},
		},
	})

	assert.NoError(t, err)
	assert.False(t, resp.GetNodeExpansionRequired())
	assert.Equal(t, int64(2*1024*1024*1024), resp.GetCapacityBytes())
	sharedProvider.AssertExpectations(t)
}

func TestControllerExpandVolumePassesDetachedExpansionGate(t *testing.T) {
	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	cs := getTestControllerServer(mockProvider)
	cs.driver.featureGates.DetachedDiskExpansion = true

	mockProvider.On("ExpandVolume", mock.Anything, "test-volume", int64(2*1024*1024*1024), true).Return(int64(2*1024*1024*1024), nil)

	resp, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "test-volume",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: int64(2 * 1024 * 1024 * 1024),
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
			},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, int64(2*1024*1024*1024), resp.GetCapacityBytes())
	mockProvider.AssertExpectations(t)
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
				m.On("GetVolumeInNode", mock.Anything, 1, 42).Return("attached-target", nil).Twice()
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

func TestControllerUnpublishVolumeStartsStickyDetachGraceForOptedInLocalPVC(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-sticky", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, map[string]string{
		annotationRestartOpt:  restartOptimizationAnnotationValue,
		annotationDetachGrace: "120",
	})
	driver := newStickyTestDriver(t, pv, pvc)
	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("VolumeExists", mock.Anything, "vol-sticky").Return(1, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-1").Return(41, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("vdb", nil).Twice()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	resp, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "vol-sticky",
		NodeId:   "node-1",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	state, ok := driver.stickyAttachments.Get("vol-sticky")
	require.True(t, ok)
	assert.Equal(t, "node-1", state.NodeID)
	assert.Equal(t, 120, state.GraceSeconds)
	mockProvider.AssertNotCalled(t, "DetachVolume", mock.Anything, "vol-sticky", "node-1")
	mockProvider.AssertExpectations(t)
}

func TestControllerUnpublishVolumeStartsMaintenanceHold(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-maint-hold", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	driver := newStickyTestDriver(t, pv, pvc)
	driver.PluginConfig.OverrideVal(config.MaintenanceReleaseMaxSecondsVar, 1200)
	driver.maintenanceMode = NewMaintenanceModeManager(driver, "default")
	driver.maintenanceMode.setState(true, true, false)

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("VolumeExists", mock.Anything, "vol-maint-hold").Return(1, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-1").Return(41, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("vdb", nil).Once()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	resp, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "vol-maint-hold",
		NodeId:   "node-1",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	state, ok := driver.stickyAttachments.Get("vol-maint-hold")
	require.True(t, ok)
	assert.Equal(t, maintenanceStickyReason, state.Reason)
	assert.Equal(t, "node-1", state.NodeID)
	mockProvider.AssertNotCalled(t, "DetachVolume", mock.Anything, "vol-maint-hold", "node-1")
	mockProvider.AssertExpectations(t)
}

func TestControllerPublishVolumeBlocksCrossNodeDuringMaintenance(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-maint-block", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pv.Annotations[annotationLastAttachedNode] = "node-old"
	driver := newStickyTestDriver(t, pv, pvc)
	driver.maintenanceMode = NewMaintenanceModeManager(driver, "default")
	driver.maintenanceMode.setState(true, true, false)

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("VolumeExists", mock.Anything, "vol-maint-block").Return(1, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-new").Return(42, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 42).Return("", fmt.Errorf("not attached")).Once()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	resp, err := cs.ControllerPublishVolume(context.Background(), newPublishReq("vol-maint-block", "node-new"))
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	mockProvider.AssertNotCalled(t, "AttachVolume", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockProvider.AssertExpectations(t)
}

func TestControllerPublishVolumeRejectsOpenNebulaMetadataDriftAndQuarantines(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-drift", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pvc.Spec.VolumeName = pv.Name
	driver := newStickyTestDriver(t, pv, pvc)
	driver.PluginConfig.OverrideVal(config.MetadataDriftQuarantineFailureThresholdVar, 2)
	driver.PluginConfig.OverrideVal(config.MetadataDriftQuarantineTTLSecondsVar, 1800)

	provider := new(MockOpenNebulaVolumeProviderTestify)
	provider.On("VolumeExists", mock.Anything, "vol-drift").Return(488, volumeSize, nil).Twice()
	provider.On("NodeExists", mock.Anything, "node-new").Return(181, nil).Twice()
	provider.On("GetVolumeInNode", mock.Anything, 488, 181).Return("", errors.New("not attached")).Times(4)
	provider.On("InspectVolumeAttachment", mock.Anything, "vol-drift", "node-new").Return(&opennebula.VolumeAttachmentMetadata{
		VolumeHandle:    "vol-drift",
		ImageID:         488,
		ImageState:      "USED_PERS",
		ImageRunningVMs: 1,
		ImageVMIDs:      []int{160},
		RequestedNode:   "node-new",
		RequestedNodeID: 181,
		DiskRecords: []opennebula.VolumeDiskRecord{{
			NodeName: "node-old",
			NodeID:   160,
			DiskID:   12,
			Target:   "sdd",
		}},
	}, nil).Twice()

	cs := NewControllerServer(driver, provider, &MockSharedFilesystemProviderTestify{})
	req := newPublishReq("vol-drift", "node-new")
	_, err := cs.ControllerPublishVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "OpenNebula metadata drift")

	_, err = cs.ControllerPublishVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	cm, err := driver.kubeRuntime.client.CoreV1().ConfigMaps("default").Get(context.Background(), volumeQuarantineStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	var state VolumeQuarantineState
	require.NoError(t, json.Unmarshal([]byte(cm.Data["vol-drift"]), &state))
	assert.Equal(t, 2, state.FailureCount)
	assert.True(t, state.ExpiresAt.After(time.Now()))
	assert.Equal(t, metadataDriftReason, state.Reason)
	provider.AssertNotCalled(t, "AttachVolume", mock.Anything, "vol-drift", "node-new", false, mock.Anything)
	provider.AssertExpectations(t)
}

func TestControllerPublishVolumeClassifiesHostArtifactConflictAndQuarantines(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-host-artifact", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pvc.Spec.VolumeName = pv.Name
	driver := newStickyTestDriver(t, pv, pvc)
	driver.PluginConfig.OverrideVal(config.HostArtifactQuarantineFailureThresholdVar, 1)
	driver.PluginConfig.OverrideVal(config.HostArtifactQuarantineTTLSecondsVar, 3600)

	conflict := &opennebula.HostArtifactConflictError{
		Classification: opennebula.HostArtifactConflictClassification,
		Target: opennebula.HostArtifactAttachmentTarget{
			VolumeHandle:        "vol-host-artifact",
			ImageID:             233,
			NodeName:            "node-161",
			VMID:                161,
			DiskID:              2,
			Target:              "sdb",
			LVName:              "lv-one-161-2",
			SystemDatastoreID:   104,
			SystemDatastoreName: "DEVONeb1",
			SystemDatastoreTM:   "fs_lvm_ssh",
		},
		RawMessage: `ATTACHDISK: Logical volume "lv-one-161-2" already exists`,
		Cause:      errors.New("attach failed"),
	}

	provider := new(MockOpenNebulaVolumeProviderTestify)
	provider.On("VolumeExists", mock.Anything, "vol-host-artifact").Return(233, volumeSize, nil).Once()
	provider.On("NodeExists", mock.Anything, "node-161").Return(161, nil).Once()
	provider.On("GetVolumeInNode", mock.Anything, 233, 161).Return("", errors.New("not attached")).Twice()
	provider.On("AttachVolume", mock.Anything, "vol-host-artifact", "node-161", false, mock.Anything).Return(conflict).Once()

	cs := NewControllerServer(driver, provider, &MockSharedFilesystemProviderTestify{})
	_, err := cs.ControllerPublishVolume(context.Background(), newPublishReq("vol-host-artifact", "node-161"))
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "lv-one-161-2")

	cm, err := driver.kubeRuntime.client.CoreV1().ConfigMaps("default").Get(context.Background(), hostArtifactStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	var state HostArtifactQuarantineState
	require.NoError(t, json.Unmarshal([]byte(cm.Data["vm-161.disk-2"]), &state))
	assert.Equal(t, 1, state.FailureCount)
	assert.True(t, state.ExpiresAt.After(time.Now()))
	assert.Equal(t, "vol-host-artifact", state.VolumeID)
	assert.Equal(t, "lv-one-161-2", state.LVName)
	assert.Contains(t, state.RepairHint, "read-only quarantine")
	provider.AssertExpectations(t)
}

func TestControllerPublishVolumeBlocksActiveHostArtifactQuarantine(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-host-artifact-active", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pvc.Spec.VolumeName = pv.Name
	driver := newStickyTestDriver(t, pv, pvc)
	_, active, err := driver.hostArtifactQuarantine.MarkFailure(context.Background(), HostArtifactQuarantineState{
		Key:            "vm-161.disk-2",
		Classification: opennebula.HostArtifactConflictClassification,
		Reason:         hostArtifactReason,
		VolumeID:       "vol-host-artifact-active",
		NodeName:       "node-161",
		VMID:           161,
		DiskID:         2,
		LVName:         "lv-one-161-2",
		Message:        "stale host LV lv-one-161-2 blocks attach",
	}, 1, time.Hour)
	require.NoError(t, err)
	require.True(t, active)

	provider := new(MockOpenNebulaVolumeProviderTestify)
	provider.On("VolumeExists", mock.Anything, "vol-host-artifact-active").Return(233, volumeSize, nil).Once()
	provider.On("NodeExists", mock.Anything, "node-161").Return(161, nil).Once()
	provider.On("GetVolumeInNode", mock.Anything, 233, 161).Return("", errors.New("not attached")).Twice()
	provider.On("InspectHostArtifactAttachmentTarget", mock.Anything, "vol-host-artifact-active", "node-161", mock.Anything).Return(&opennebula.HostArtifactAttachmentTarget{
		VolumeHandle: "vol-host-artifact-active",
		ImageID:      233,
		NodeName:     "node-161",
		VMID:         161,
		DiskID:       2,
		LVName:       "lv-one-161-2",
	}, nil).Once()

	cs := NewControllerServer(driver, provider, &MockSharedFilesystemProviderTestify{})
	_, err = cs.ControllerPublishVolume(context.Background(), newPublishReq("vol-host-artifact-active", "node-161"))
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "host artifact quarantine active")
	provider.AssertNotCalled(t, "AttachVolume", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	provider.AssertExpectations(t)
}

func TestControllerUnpublishVolumeClassifiesDetachMetadataDrift(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-detach-drift", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pvc.Spec.VolumeName = pv.Name
	driver := newStickyTestDriver(t, pv, pvc)
	driver.PluginConfig.OverrideVal(config.MetadataDriftQuarantineFailureThresholdVar, 1)

	provider := new(MockOpenNebulaVolumeProviderTestify)
	provider.On("VolumeExists", mock.Anything, "vol-detach-drift").Return(489, volumeSize, nil).Once()
	provider.On("NodeExists", mock.Anything, "node-old").Return(160, nil).Once()
	provider.On("GetVolumeInNode", mock.Anything, 489, 160).Return("sdd", nil).Twice()
	provider.On("DetachVolume", mock.Anything, "vol-detach-drift", "node-old").Return(errors.New("DETACHDISK: Could not detach sdd")).Once()
	provider.On("InspectVolumeAttachment", mock.Anything, "vol-detach-drift", "node-old").Return(&opennebula.VolumeAttachmentMetadata{
		VolumeHandle:            "vol-detach-drift",
		ImageID:                 489,
		ImageState:              "USED_PERS",
		ImageRunningVMs:         1,
		ImageVMIDs:              []int{160},
		RequestedNode:           "node-old",
		RequestedNodeID:         160,
		AttachedToRequestedNode: true,
		DiskRecords: []opennebula.VolumeDiskRecord{{
			NodeName: "node-old",
			NodeID:   160,
			DiskID:   12,
			Target:   "sdd",
		}},
	}, nil).Once()

	cs := NewControllerServer(driver, provider, &MockSharedFilesystemProviderTestify{})
	_, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "vol-detach-drift",
		NodeId:   "node-old",
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "OpenNebula detach failed")

	cm, err := driver.kubeRuntime.client.CoreV1().ConfigMaps("default").Get(context.Background(), volumeQuarantineStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, cm.Data, "vol-detach-drift")
	provider.AssertExpectations(t)
}

func TestControllerPublishVolumeReusesStickyAttachmentOnSameNode(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-reuse", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, map[string]string{
		annotationRestartOpt: restartOptimizationAnnotationValue,
	})
	driver := newStickyTestDriver(t, pv, pvc)
	require.NoError(t, driver.stickyAttachments.StartGrace(StickyAttachmentState{
		VolumeID:     "vol-reuse",
		NodeID:       "node-1",
		Backend:      "local",
		PVCNamespace: "default",
		PVCName:      "pvc-vol-reuse",
		StartedAt:    time.Now().Add(-10 * time.Second),
		ExpiresAt:    time.Now().Add(90 * time.Second),
		GraceSeconds: 90,
		Reason:       "stateful_restart",
	}))

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("VolumeExists", mock.Anything, "vol-reuse").Return(1, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-1").Return(41, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("", fmt.Errorf("not attached")).Once()
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("vdc", nil).Once()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	resp, err := cs.ControllerPublishVolume(context.Background(), newPublishReq("vol-reuse", "node-1"))
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "vdc", resp.GetPublishContext()["volumeName"])
	_, ok := driver.stickyAttachments.Get("vol-reuse")
	assert.False(t, ok)
	mockProvider.AssertNotCalled(t, "AttachVolume", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockProvider.AssertNotCalled(t, "DetachVolume", mock.Anything, mock.Anything, mock.Anything)
	mockProvider.AssertExpectations(t)
}

func TestControllerPublishVolumeCancelsStickyAttachmentOnDifferentNode(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-move", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, map[string]string{
		annotationRestartOpt: restartOptimizationAnnotationValue,
	})
	driver := newStickyTestDriver(t, pv, pvc)
	require.NoError(t, driver.stickyAttachments.StartGrace(StickyAttachmentState{
		VolumeID:     "vol-move",
		NodeID:       "node-old",
		Backend:      "local",
		PVCNamespace: "default",
		PVCName:      "pvc-vol-move",
		StartedAt:    time.Now().Add(-10 * time.Second),
		ExpiresAt:    time.Now().Add(90 * time.Second),
		GraceSeconds: 90,
		Reason:       "stateful_restart",
	}))

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("VolumeExists", mock.Anything, "vol-move").Return(1, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-new").Return(42, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-old").Return(41, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 42).Return("", fmt.Errorf("not attached")).Twice()
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("vdc", nil).Once()
	mockProvider.On("DetachVolume", mock.Anything, "vol-move", "node-old").Return(nil).Once()
	mockProvider.On("AttachVolume", mock.Anything, "vol-move", "node-new", false, mock.Anything).Return(nil).Once()
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 42).Return("vdd", nil).Once()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	resp, err := cs.ControllerPublishVolume(context.Background(), newPublishReq("vol-move", "node-new"))
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "vdd", resp.GetPublishContext()["volumeName"])
	_, ok := driver.stickyAttachments.Get("vol-move")
	assert.False(t, ok)
	mockProvider.AssertExpectations(t)
}

func TestControllerPublishVolumeClearsStickyWhenOldAttachmentAlreadyAbsent(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-absent", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, map[string]string{
		annotationRestartOpt: restartOptimizationAnnotationValue,
	})
	driver := newStickyTestDriver(t, pv, pvc)
	require.NoError(t, driver.stickyAttachments.StartGrace(StickyAttachmentState{
		VolumeID:     "vol-absent",
		NodeID:       "node-old",
		Backend:      "local",
		PVCNamespace: "default",
		PVCName:      "pvc-vol-absent",
		StartedAt:    time.Now().Add(-10 * time.Second),
		ExpiresAt:    time.Now().Add(90 * time.Second),
		GraceSeconds: 90,
		Reason:       "stateful_restart",
	}))

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("NodeExists", mock.Anything, "node-old").Return(41, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("", fmt.Errorf("already absent")).Once()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	handled, resp, err := cs.reuseStickyAttachment(context.Background(), newPublishReq("vol-absent", "node-new"), 1, 42)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Nil(t, resp)
	_, ok := driver.stickyAttachments.Get("vol-absent")
	assert.False(t, ok)
	mockProvider.AssertNotCalled(t, "DetachVolume", mock.Anything, mock.Anything, mock.Anything)
	mockProvider.AssertExpectations(t)
}

func TestControllerPublishVolumeBlocksCrossNodeWhenOldNodeHotplugStuck(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-stuck", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, map[string]string{
		annotationRestartOpt: restartOptimizationAnnotationValue,
	})
	driver := newStickyTestDriver(t, pv, pvc)
	driver.kubeRuntime.inventoryClient = newInventoryFakeClient(t, &inventoryv1alpha1.OpenNebulaNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-old"},
		Status: inventoryv1alpha1.OpenNebulaNodeStatus{
			DisplayState: "HotplugStuck",
			Hotplug: inventoryv1alpha1.OpenNebulaNodeHotplugStatus{
				Diagnosis: inventoryv1alpha1.OpenNebulaNodeHotplugDiagnosisStatus{
					Classification: opennebula.HotplugClassificationStuck,
					Operation:      "attach",
					VolumeHandle:   "vol-stuck",
					DiskAttachFlag: "YES",
				},
			},
		},
	})
	require.NoError(t, driver.stickyAttachments.StartGrace(StickyAttachmentState{
		VolumeID:     "vol-stuck",
		NodeID:       "node-old",
		Backend:      "local",
		PVCNamespace: "default",
		PVCName:      "pvc-vol-stuck",
		StartedAt:    time.Now().Add(-10 * time.Second),
		ExpiresAt:    time.Now().Add(90 * time.Second),
		GraceSeconds: 90,
		Reason:       "stateful_restart",
	}))

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("VolumeExists", mock.Anything, "vol-stuck").Return(1, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-new").Return(42, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 42).Return("", fmt.Errorf("not attached")).Once()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	resp, err := cs.ControllerPublishVolume(context.Background(), newPublishReq("vol-stuck", "node-new"))
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	mockProvider.AssertNotCalled(t, "DetachVolume", mock.Anything, mock.Anything, mock.Anything)
	mockProvider.AssertExpectations(t)
}

func TestControllerUnpublishVolumeRescuesSameNodeDesiredVolume(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-rescue", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-rescue", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	driver := newStickyTestDriver(t, pv, pvc, pod)

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("VolumeExists", mock.Anything, "vol-rescue").Return(1, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-1").Return(41, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("vdb", nil).Twice()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	resp, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "vol-rescue",
		NodeId:   "node-1",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	mockProvider.AssertNotCalled(t, "DetachVolume", mock.Anything, mock.Anything, mock.Anything)
	mockProvider.AssertExpectations(t)
}

func TestExpireStickyDetachClearsAlreadyAbsentAttachment(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-expired-absent", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	driver := newStickyTestDriver(t, pv, pvc)
	state := StickyAttachmentState{
		VolumeID:     "vol-expired-absent",
		NodeID:       "node-1",
		Backend:      "local",
		PVCNamespace: "default",
		PVCName:      "pvc-vol-expired-absent",
		StartedAt:    time.Now().Add(-2 * time.Minute),
		ExpiresAt:    time.Now().Add(-time.Minute),
		GraceSeconds: 60,
		Reason:       "stateful_restart",
	}
	require.NoError(t, driver.stickyAttachments.StartGrace(state))

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("VolumeExists", mock.Anything, "vol-expired-absent").Return(1, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-1").Return(41, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("", fmt.Errorf("already absent")).Once()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	cs.expireStickyDetach(context.Background(), state)

	_, ok := driver.stickyAttachments.Get("vol-expired-absent")
	assert.False(t, ok)
	mockProvider.AssertNotCalled(t, "DetachVolume", mock.Anything, mock.Anything, mock.Anything)
	mockProvider.AssertExpectations(t)
}

func TestMaintenanceAttachedNodesIgnoresDeletingVolumeAttachments(t *testing.T) {
	now := metav1.Now()
	pvName := "pv-vol"
	attached, conflicts := maintenanceAttachedNodesByPV([]storagev1.VolumeAttachment{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "va-deleting", DeletionTimestamp: &now},
			Spec: storagev1.VolumeAttachmentSpec{
				NodeName: "node-a",
				Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pvName},
			},
			Status: storagev1.VolumeAttachmentStatus{Attached: true},
		},
	})
	assert.Empty(t, attached)
	assert.Empty(t, conflicts)
}

func TestControllerUnpublishVolumeBypassesStickyDetachWhenNodeNotReady(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-bypass", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, map[string]string{
		annotationRestartOpt: restartOptimizationAnnotationValue,
	})
	node := newReadyNode("node-1", false)
	driver := newStickyTestDriver(t, pv, pvc, node)
	driver.PluginConfig.OverrideVal(config.LocalRestartRequireNodeReadyVar, true)

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("VolumeExists", mock.Anything, "vol-bypass").Return(1, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-1").Return(41, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("vdb", nil).Twice()
	mockProvider.On("DetachVolume", mock.Anything, "vol-bypass", "node-1").Return(nil).Once()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	resp, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "vol-bypass",
		NodeId:   "node-1",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	_, ok := driver.stickyAttachments.Get("vol-bypass")
	assert.False(t, ok)
	mockProvider.AssertExpectations(t)
}

func TestControllerUnpublishVolumePausesHotplugAfterUnhealthyNodeFailures(t *testing.T) {
	node := newReadyNode("node-1", false)
	driver := newStickyTestDriver(t, node)
	driver.PluginConfig.OverrideVal(config.NodeHotplugGuardEnabledVar, true)
	driver.PluginConfig.OverrideVal(config.NodeHotplugGuardFailureThresholdVar, 2)
	driver.PluginConfig.OverrideVal(config.NodeHotplugGuardRequireKubernetesReadyVar, true)
	driver.PluginConfig.OverrideVal(config.NodeHotplugGuardRequireOpenNebulaReadyVar, true)

	timeoutErr := &opennebula.HotplugTimeoutError{
		Operation:            "detach",
		Volume:               "vol-pause",
		Node:                 "node-1",
		Timeout:              time.Second,
		LastObservedAttached: true,
		LastObservedReady:    false,
		Cause:                errors.New("wrong state HOTPLUG"),
	}
	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("VolumeExists", mock.Anything, "vol-pause").Return(1, 1, nil).Times(3)
	mockProvider.On("NodeExists", mock.Anything, "node-1").Return(41, nil).Times(3)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("vdb", nil).Times(5)
	mockProvider.On("NodeReady", mock.Anything, "node-1").Return(false, nil).Times(3)
	mockProvider.On("DetachVolume", mock.Anything, "vol-pause", "node-1").Return(timeoutErr).Twice()

	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)
	req := &csi.ControllerUnpublishVolumeRequest{VolumeId: "vol-pause", NodeId: "node-1"}

	_, err := cs.ControllerUnpublishVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))

	_, err = cs.ControllerUnpublishVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))

	_, err = cs.ControllerUnpublishVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err))

	state, ok := driver.hotplugGuard.Get("node-1")
	require.True(t, ok)
	assert.True(t, state.PauseUntilReady)
	assert.Equal(t, 2, state.FailureCount)
	mockProvider.AssertExpectations(t)
}

func TestHotplugGuardTreatsUnschedulableAsUnhealthyOnlyDuringMaintenance(t *testing.T) {
	node := newReadyNode("node-1", true)
	node.Spec.Unschedulable = true
	driver := newStickyTestDriver(t, node)
	driver.PluginConfig.OverrideVal(config.NodeHotplugGuardRequireKubernetesReadyVar, true)
	driver.PluginConfig.OverrideVal(config.NodeHotplugGuardRequireOpenNebulaReadyVar, true)

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("NodeReady", mock.Anything, "node-1").Return(true, nil).Twice()
	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)

	health := cs.nodeHotplugHealth(context.Background(), "node-1", false)
	require.True(t, health.KubernetesReady)
	require.True(t, health.OpenNebulaReady)
	require.True(t, health.Unschedulable)
	assert.False(t, cs.hotplugNodeUnhealthy(health))

	driver.maintenanceMode = NewMaintenanceModeManager(driver, "default")
	driver.maintenanceMode.setState(true, false, false)
	health = cs.nodeHotplugHealth(context.Background(), "node-1", false)
	assert.True(t, cs.hotplugNodeUnhealthy(health))
	assert.Equal(t, "kubernetes_node_unschedulable", cs.hotplugUnhealthyReason(health))
	mockProvider.AssertExpectations(t)
}

func TestHotplugGuardCleanupClearsReadyCordonedNodeOutsideMaintenance(t *testing.T) {
	node := newReadyNode("node-1", true)
	node.Spec.Unschedulable = true
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: hotplugStateConfigMapName, Namespace: namespaceFromServiceAccount()},
		Data:       map[string]string{"node-1": "stale"},
	}
	driver := newStickyTestDriver(t, node, cm)
	driver.PluginConfig.OverrideVal(config.NodeHotplugGuardEnabledVar, true)
	driver.PluginConfig.OverrideVal(config.NodeHotplugGuardRequireKubernetesReadyVar, true)
	driver.PluginConfig.OverrideVal(config.NodeHotplugGuardRequireOpenNebulaReadyVar, true)
	driver.maintenanceMode = NewMaintenanceModeManager(driver, "default")
	driver.maintenanceMode.setState(true, false, false)
	driver.hotplugGuard.MarkPaused(opennebula.HotplugCooldownState{
		Node:            "node-1",
		Operation:       "detach",
		Volume:          "vol-1",
		KubernetesReady: false,
		OpenNebulaReady: false,
		Unschedulable:   true,
	})

	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	mockProvider.On("NodeReady", mock.Anything, "node-1").Return(true, nil).Twice()
	cs := getTestControllerServerWithDriver(mockProvider, &MockSharedFilesystemProviderTestify{}, driver)

	cs.reconcileHotplugGuardCleanup(context.Background())
	_, ok := driver.hotplugGuard.Get("node-1")
	assert.True(t, ok)
	updated, err := driver.kubeRuntime.client.CoreV1().ConfigMaps(namespaceFromServiceAccount()).Get(context.Background(), hotplugStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, updated.Data, "node-1")

	driver.maintenanceMode.setState(false, false, false)
	cs.reconcileHotplugGuardCleanup(context.Background())
	_, ok = driver.hotplugGuard.Get("node-1")
	assert.False(t, ok)
	updated, err = driver.kubeRuntime.client.CoreV1().ConfigMaps(namespaceFromServiceAccount()).Get(context.Background(), hotplugStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updated.Data, "node-1")
	mockProvider.AssertExpectations(t)
}

func TestLoadHotplugGuardStateGarbageCollectsExpiredSnapshots(t *testing.T) {
	expiredState := opennebula.HotplugCooldownState{
		Node:      "node-expired",
		Operation: "detach",
		Volume:    "vol-expired",
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	activeState := opennebula.HotplugCooldownState{
		Node:      "node-active",
		Operation: "attach",
		Volume:    "vol-active",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	expiredPayload, err := json.Marshal(expiredState)
	require.NoError(t, err)
	activePayload, err := json.Marshal(activeState)
	require.NoError(t, err)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: hotplugStateConfigMapName, Namespace: namespaceFromServiceAccount()},
		Data: map[string]string{
			"node-expired":          string(expiredPayload),
			"node-active":           string(activePayload),
			maintenanceModeKey:      "false",
			maintenanceReadyKey:     "true",
			maintenanceModeAliasKey: "false",
		},
	}
	driver := newStickyTestDriver(t, cm)

	require.NoError(t, driver.loadHotplugGuardState(context.Background()))

	_, expiredLoaded := driver.hotplugGuard.Get("node-expired")
	assert.False(t, expiredLoaded)
	_, activeLoaded := driver.hotplugGuard.Get("node-active")
	assert.True(t, activeLoaded)
	updated, err := driver.kubeRuntime.client.CoreV1().ConfigMaps(namespaceFromServiceAccount()).Get(context.Background(), hotplugStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updated.Data, "node-expired")
	assert.Contains(t, updated.Data, "node-active")
	assert.Contains(t, updated.Data, maintenanceModeKey)
	assert.Contains(t, updated.Data, maintenanceReadyKey)
}

func TestControllerPublishVolumeSerializesSameNodeHotplug(t *testing.T) {
	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	provider := &serializingVolumeProvider{MockOpenNebulaVolumeProviderTestify: mockProvider}
	provider.attachGate = make(chan struct{})
	provider.attachStarted = make(chan struct{}, 2)
	provider.attachFinished = make(chan struct{}, 2)

	cs := NewControllerServer(newTestDriver(), provider, &MockSharedFilesystemProviderTestify{})

	mockProvider.On("VolumeExists", mock.Anything, "vol-1").Return(1, 1, nil)
	mockProvider.On("VolumeExists", mock.Anything, "vol-2").Return(2, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-1").Return(41, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("", errors.New("not attached")).Twice()
	mockProvider.On("GetVolumeInNode", mock.Anything, 2, 41).Return("", errors.New("not attached")).Twice()
	mockProvider.On("ResolveVolumeSizeBytes", mock.Anything, "vol-1").Return(int64(300*1024*1024*1024), nil).Maybe()
	mockProvider.On("ResolveVolumeSizeBytes", mock.Anything, "vol-2").Return(int64(300*1024*1024*1024), nil).Maybe()
	mockProvider.On("ComputeHotplugTimeout", int64(300*1024*1024*1024)).Return(5 * time.Minute).Maybe()
	mockProvider.On("AttachVolume", mock.Anything, "vol-1", "node-1", false, mock.Anything).Return(nil).Once()
	mockProvider.On("AttachVolume", mock.Anything, "vol-2", "node-1", false, mock.Anything).Return(nil).Once()
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("sdb", nil).Once()
	mockProvider.On("GetVolumeInNode", mock.Anything, 2, 41).Return("sdc", nil).Once()

	errs := make(chan error, 2)
	go func() {
		_, err := cs.ControllerPublishVolume(context.Background(), newPublishReq("vol-1", "node-1"))
		errs <- err
	}()
	<-provider.attachStarted

	go func() {
		_, err := cs.ControllerPublishVolume(context.Background(), newPublishReq("vol-2", "node-1"))
		errs <- err
	}()

	select {
	case <-provider.attachStarted:
		t.Fatal("expected second same-node publish to remain queued until the first attach completed")
	case err := <-errs:
		t.Fatalf("expected second same-node publish to remain queued, got early completion: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(provider.attachGate)
	<-provider.attachFinished
	assert.NoError(t, <-errs)
	<-provider.attachStarted
	<-provider.attachFinished

	assert.NoError(t, <-errs)
	assert.Equal(t, int32(1), atomic.LoadInt32(&provider.maxConcurrentAttach))
	mockProvider.AssertExpectations(t)
}

func TestControllerExpandVolumeSerializesWithPublishOnSameVolume(t *testing.T) {
	mockProvider := new(MockOpenNebulaVolumeProviderTestify)
	provider := &serializingVolumeProvider{MockOpenNebulaVolumeProviderTestify: mockProvider}
	provider.attachGate = make(chan struct{})
	provider.attachStarted = make(chan struct{}, 1)
	provider.attachFinished = make(chan struct{}, 1)
	provider.expandGate = make(chan struct{})
	provider.expandStarted = make(chan struct{}, 1)
	provider.expandFinished = make(chan struct{}, 1)

	cs := NewControllerServer(newTestDriver(), provider, &MockSharedFilesystemProviderTestify{})

	mockProvider.On("ExpandVolume", mock.Anything, "vol-1", int64(2*1024*1024*1024), true).Return(int64(2*1024*1024*1024), nil).Once()
	mockProvider.On("VolumeExists", mock.Anything, "vol-1").Return(1, 1, nil)
	mockProvider.On("NodeExists", mock.Anything, "node-1").Return(41, nil)
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("", errors.New("not attached")).Twice()
	mockProvider.On("ResolveVolumeSizeBytes", mock.Anything, "vol-1").Return(int64(2*1024*1024*1024), nil).Maybe()
	mockProvider.On("ComputeHotplugTimeout", int64(2*1024*1024*1024)).Return(3 * time.Minute).Maybe()
	mockProvider.On("AttachVolume", mock.Anything, "vol-1", "node-1", false, mock.Anything).Return(nil).Once()
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 41).Return("sdb", nil).Once()

	errs := make(chan error, 2)
	go func() {
		_, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
			VolumeId: "vol-1",
			CapacityRange: &csi.CapacityRange{
				RequiredBytes: int64(2 * 1024 * 1024 * 1024),
			},
		})
		errs <- err
	}()
	<-provider.expandStarted

	go func() {
		_, err := cs.ControllerPublishVolume(context.Background(), newPublishReq("vol-1", "node-1"))
		errs <- err
	}()

	select {
	case <-provider.attachStarted:
		t.Fatal("expected expand to hold the volume lock until completion")
	case <-time.After(20 * time.Millisecond):
	}

	close(provider.expandGate)
	<-provider.expandFinished
	close(provider.attachGate)
	<-provider.attachFinished

	assert.NoError(t, <-errs)
	assert.NoError(t, <-errs)
	mockProvider.AssertExpectations(t)
}

type MockOpenNebulaVolumeProviderTestify struct {
	mock.Mock
}

type serializingVolumeProvider struct {
	*MockOpenNebulaVolumeProviderTestify
	attachGate          chan struct{}
	attachStarted       chan struct{}
	attachFinished      chan struct{}
	expandGate          chan struct{}
	expandStarted       chan struct{}
	expandFinished      chan struct{}
	concurrentAttach    int32
	maxConcurrentAttach int32
}

type MockSharedFilesystemProviderTestify struct {
	mock.Mock
}

func (m *MockOpenNebulaVolumeProviderTestify) hasExpectation(method string) bool {
	for _, call := range m.ExpectedCalls {
		if call.Method == method {
			return true
		}
	}
	return false
}

func (m *MockOpenNebulaVolumeProviderTestify) CreateVolume(ctx context.Context, name string, size int64, owner string, immutable bool, fsType string, params map[string]string, selection opennebula.DatastoreSelectionConfig) (*opennebula.VolumeCreateResult, error) {
	args := m.Called(ctx, name, size, owner, immutable, fsType, params, selection)
	if result := args.Get(0); result != nil {
		return result.(*opennebula.VolumeCreateResult), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) ResolveProvisioningDatastores(ctx context.Context, selection opennebula.DatastoreSelectionConfig) ([]opennebula.Datastore, error) {
	if !m.hasExpectation("ResolveProvisioningDatastores") {
		return []opennebula.Datastore{{
			ID:      100,
			Name:    "ds-100",
			Backend: "local",
			Type:    "local",
		}}, nil
	}
	args := m.Called(ctx, selection)
	return args.Get(0).([]opennebula.Datastore), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) CloneVolume(ctx context.Context, name string, sourceVolume string, selection opennebula.DatastoreSelectionConfig) (*opennebula.VolumeCreateResult, error) {
	args := m.Called(ctx, name, sourceVolume, selection)
	if result := args.Get(0); result != nil {
		return result.(*opennebula.VolumeCreateResult), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) DeleteVolume(ctx context.Context, volume string) error {
	args := m.Called(ctx, volume)
	return args.Error(0)
}

func (m *MockOpenNebulaVolumeProviderTestify) ExpandVolume(ctx context.Context, volume string, size int64, allowDetached bool) (int64, error) {
	args := m.Called(ctx, volume, size, allowDetached)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) AttachVolume(ctx context.Context, volume string, node string, immutable bool, params map[string]string) error {
	args := m.Called(ctx, volume, node, immutable, params)
	return args.Error(0)
}

func (p *serializingVolumeProvider) AttachVolume(ctx context.Context, volume string, node string, immutable bool, params map[string]string) error {
	current := atomic.AddInt32(&p.concurrentAttach, 1)
	for {
		max := atomic.LoadInt32(&p.maxConcurrentAttach)
		if current <= max || atomic.CompareAndSwapInt32(&p.maxConcurrentAttach, max, current) {
			break
		}
	}
	if p.attachStarted != nil {
		p.attachStarted <- struct{}{}
	}
	if p.attachGate != nil {
		<-p.attachGate
	}
	if p.attachFinished != nil {
		p.attachFinished <- struct{}{}
	}
	atomic.AddInt32(&p.concurrentAttach, -1)
	return p.MockOpenNebulaVolumeProviderTestify.AttachVolume(ctx, volume, node, immutable, params)
}

func (m *MockOpenNebulaVolumeProviderTestify) DetachVolume(ctx context.Context, volume string, node string) error {
	args := m.Called(ctx, volume, node)
	return args.Error(0)
}

func (m *MockOpenNebulaVolumeProviderTestify) ListVolumes(ctx context.Context, volume string, maxEntries int32, startingToken string) ([]string, error) {
	args := m.Called(ctx, volume, maxEntries, startingToken)
	return args.Get(0).([]string), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) GetCapacity(ctx context.Context, selection opennebula.DatastoreSelectionConfig) (int64, error) {
	args := m.Called(ctx, selection)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) VolumeExists(ctx context.Context, volume string) (int, int, error) {
	if !m.hasExpectation("VolumeExists") {
		return 1, volumeSize, nil
	}
	args := m.Called(ctx, volume)
	return args.Get(0).(int), args.Get(1).(int), args.Error(2)
}

func (m *MockOpenNebulaVolumeProviderTestify) CreateSnapshot(ctx context.Context, sourceVolume string, snapshotName string) (*opennebula.VolumeSnapshot, error) {
	args := m.Called(ctx, sourceVolume, snapshotName)
	if result := args.Get(0); result != nil {
		return result.(*opennebula.VolumeSnapshot), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	args := m.Called(ctx, snapshotID)
	return args.Error(0)
}

func (m *MockOpenNebulaVolumeProviderTestify) ListSnapshots(ctx context.Context, snapshotID string, sourceVolumeID string, maxEntries int32, startingToken string) ([]opennebula.VolumeSnapshot, string, error) {
	args := m.Called(ctx, snapshotID, sourceVolumeID, maxEntries, startingToken)
	return args.Get(0).([]opennebula.VolumeSnapshot), args.String(1), args.Error(2)
}

func (m *MockOpenNebulaVolumeProviderTestify) NodeExists(ctx context.Context, node string) (int, error) {
	args := m.Called(ctx, node)
	return args.Get(0).(int), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) GetVolumeInNode(ctx context.Context, volumeID int, nodeID int) (string, error) {
	args := m.Called(ctx, volumeID, nodeID)
	return args.Get(0).(string), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) ResolveVolumeSizeBytes(ctx context.Context, volume string) (int64, error) {
	if !m.hasExpectation("ResolveVolumeSizeBytes") {
		return int64(10 * 1024 * 1024 * 1024), nil
	}
	args := m.Called(ctx, volume)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) ComputeHotplugTimeout(sizeBytes int64) time.Duration {
	if !m.hasExpectation("ComputeHotplugTimeout") {
		return 3 * time.Minute
	}
	args := m.Called(sizeBytes)
	return args.Get(0).(time.Duration)
}

func (m *MockOpenNebulaVolumeProviderTestify) HotplugPolicy() opennebula.HotplugTimeoutPolicy {
	if !m.hasExpectation("HotplugPolicy") {
		return opennebula.HotplugTimeoutPolicy{
			BaseTimeout:  time.Minute,
			Per100GiB:    time.Minute,
			MaxTimeout:   15 * time.Minute,
			PollInterval: time.Second,
		}
	}
	args := m.Called()
	if policy := args.Get(0); policy != nil {
		return policy.(opennebula.HotplugTimeoutPolicy)
	}
	return opennebula.HotplugTimeoutPolicy{}
}

func (m *MockOpenNebulaVolumeProviderTestify) NodeReady(ctx context.Context, node string) (bool, error) {
	if !m.hasExpectation("NodeReady") {
		return false, nil
	}
	args := m.Called(ctx, node)
	return args.Bool(0), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) VolumeReadyWithTimeout(volumeID int) (bool, error) {
	args := m.Called(volumeID)
	return args.Bool(0), args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) ListCurrentAttachments(ctx context.Context) ([]opennebula.ObservedAttachment, error) {
	if !m.hasExpectation("ListCurrentAttachments") {
		return nil, nil
	}
	args := m.Called(ctx)
	if attachments := args.Get(0); attachments != nil {
		return attachments.([]opennebula.ObservedAttachment), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) InspectVolumeAttachment(ctx context.Context, volume string, node string) (*opennebula.VolumeAttachmentMetadata, error) {
	if !m.hasExpectation("InspectVolumeAttachment") {
		return nil, nil
	}
	args := m.Called(ctx, volume, node)
	if metadata := args.Get(0); metadata != nil {
		return metadata.(*opennebula.VolumeAttachmentMetadata), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockOpenNebulaVolumeProviderTestify) InspectHostArtifactAttachmentTarget(ctx context.Context, volume string, node string, params map[string]string) (*opennebula.HostArtifactAttachmentTarget, error) {
	if !m.hasExpectation("InspectHostArtifactAttachmentTarget") {
		return nil, nil
	}
	args := m.Called(ctx, volume, node, params)
	if target := args.Get(0); target != nil {
		return target.(*opennebula.HostArtifactAttachmentTarget), args.Error(1)
	}
	return nil, args.Error(1)
}

func (p *serializingVolumeProvider) ExpandVolume(ctx context.Context, volume string, size int64, allowDetached bool) (int64, error) {
	if p.expandStarted != nil {
		p.expandStarted <- struct{}{}
	}
	if p.expandGate != nil {
		<-p.expandGate
	}
	if p.expandFinished != nil {
		p.expandFinished <- struct{}{}
	}
	return p.MockOpenNebulaVolumeProviderTestify.ExpandVolume(ctx, volume, size, allowDetached)
}

func (m *MockSharedFilesystemProviderTestify) CreateSharedVolume(ctx context.Context, req opennebula.SharedVolumeRequest) (*opennebula.SharedVolumeCreateResult, error) {
	args := m.Called(ctx, req)
	if result := args.Get(0); result != nil {
		return result.(*opennebula.SharedVolumeCreateResult), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockSharedFilesystemProviderTestify) CloneSharedVolume(ctx context.Context, req opennebula.SharedVolumeCloneRequest) (*opennebula.SharedVolumeCreateResult, error) {
	args := m.Called(ctx, req)
	if result := args.Get(0); result != nil {
		return result.(*opennebula.SharedVolumeCreateResult), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockSharedFilesystemProviderTestify) DeleteSharedVolume(ctx context.Context, volumeID string, secrets map[string]string) error {
	args := m.Called(ctx, volumeID, secrets)
	return args.Error(0)
}

func (m *MockSharedFilesystemProviderTestify) ExpandSharedVolume(ctx context.Context, volumeID string, sizeBytes int64, secrets map[string]string) (int64, error) {
	args := m.Called(ctx, volumeID, sizeBytes, secrets)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockSharedFilesystemProviderTestify) CreateSharedSnapshot(ctx context.Context, sourceVolumeID string, snapshotName string, secrets map[string]string) (*opennebula.VolumeSnapshot, error) {
	args := m.Called(ctx, sourceVolumeID, snapshotName, secrets)
	if result := args.Get(0); result != nil {
		return result.(*opennebula.VolumeSnapshot), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockSharedFilesystemProviderTestify) DeleteSharedSnapshot(ctx context.Context, snapshotID string, secrets map[string]string) error {
	args := m.Called(ctx, snapshotID, secrets)
	return args.Error(0)
}

func (m *MockSharedFilesystemProviderTestify) ListSharedSnapshots(ctx context.Context, snapshotID string, sourceVolumeID string, maxEntries int32, startingToken string, secrets map[string]string) ([]opennebula.VolumeSnapshot, string, error) {
	args := m.Called(ctx, snapshotID, sourceVolumeID, maxEntries, startingToken, secrets)
	return args.Get(0).([]opennebula.VolumeSnapshot), args.String(1), args.Error(2)
}

func (m *MockSharedFilesystemProviderTestify) PublishSharedVolume(ctx context.Context, volumeID string, readonly bool) (map[string]string, error) {
	args := m.Called(ctx, volumeID, readonly)
	if result := args.Get(0); result != nil {
		return result.(map[string]string), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockSharedFilesystemProviderTestify) ValidateSharedVolume(ctx context.Context, volumeID string) (*opennebula.SharedVolumeMetadata, error) {
	args := m.Called(ctx, volumeID)
	if result := args.Get(0); result != nil {
		return result.(*opennebula.SharedVolumeMetadata), args.Error(1)
	}
	return nil, args.Error(1)
}
