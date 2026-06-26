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
	"strconv"
	"strings"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

var (
	controllerCapabilityTypes = []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
	}

	supportedAccessModes = []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
	}
)

const publishContextHotplugTimeoutSeconds = "hotplugTimeoutSeconds"
const publishContextDeviceDiscoveryTimeoutSeconds = "deviceDiscoveryTimeoutSeconds"
const volumeContextUUID = "uuid"
const stickyDetachReaperInterval = 5 * time.Second
const hotplugGuardCleanupInterval = 30 * time.Second

type ControllerServer struct {
	driver                   *Driver
	volumeProvider           opennebula.OpenNebulaVolumeProvider
	sharedFilesystemProvider opennebula.SharedFilesystemProvider
	csi.UnimplementedControllerServer
}

func NewControllerServer(d *Driver, vp opennebula.OpenNebulaVolumeProvider, sharedProvider opennebula.SharedFilesystemProvider) *ControllerServer {
	if d.operationLocks == nil {
		d.operationLocks = NewOperationLocks()
	}
	return &ControllerServer{
		driver:                   d,
		volumeProvider:           vp,
		sharedFilesystemProvider: sharedProvider,
	}
}

func (s *ControllerServer) StartBackgroundWorkers(ctx context.Context) {
	if s == nil || s.driver == nil || strings.TrimSpace(s.driver.nodeID) != "" || s.driver.stickyAttachments == nil {
		return
	}
	if s.driver.maintenanceMode != nil {
		go s.driver.maintenanceMode.Run(ctx)
	}
	go s.driver.stickyAttachments.RunConfigMapSync(ctx)
	go s.runStickyDetachReaper(ctx)
	if enabled, ok := s.driver.PluginConfig.GetBool(config.StuckAttachmentReconcilerEnabledVar); !ok || enabled {
		go NewAttachmentReconciler(s).Run(ctx)
	}
	if s.localDeviceRecoveryEnabled() {
		go s.runLocalDeviceRecovery(ctx)
	}
	if s.nodeHotplugGuardEnabled() && s.driver.hotplugGuard != nil {
		go s.runHotplugGuardCleanup(ctx)
	}
}

func (s *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	started := time.Now()
	backend := "unknown"
	klog.V(1).InfoS("CreateVolume called", "req", protosanitizer.StripSecrets(req))

	name := req.GetName()
	if name == "" {
		s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
		klog.V(0).ErrorS(nil, "method", "CreateVolume", "CreateVolume called with empty volume name")
		return nil, status.Error(codes.InvalidArgument, "missing volume name")
	}

	volumeCapabilities := req.GetVolumeCapabilities()
	if len(volumeCapabilities) == 0 {
		s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
		klog.V(0).ErrorS(nil, "method", "CreateVolume", "CreateVolume called with empty or nil volume capabilities")
		return nil, status.Error(codes.InvalidArgument, "missing volume capabilities")
	}

	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	if requiredBytes == 0 {
		requiredBytes = DefaultVolumeSizeBytes
	}

	rawParams := req.GetParameters()
	requestedPVCUUID := strings.TrimSpace(rawParams[paramPVCUID])
	selection, err := s.driver.GetDatastoreSelectionConfig(rawParams)
	if err != nil {
		s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
		s.recordPVCWarningFromParams(ctx, rawParams, eventReasonDatastoreRejected, err.Error())
		klog.V(0).ErrorS(err, "Invalid datastore configuration", "method", "CreateVolume", "params", rawParams)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if err := validateAccessMode(volumeCapabilities, selection.AllowedTypes); err != nil {
		s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	candidates, err := s.volumeProvider.ResolveProvisioningDatastores(ctx, selection)
	if err != nil {
		s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
		s.recordPVCWarningFromParams(ctx, rawParams, eventReasonDatastoreRejected, err.Error())
		klog.V(0).ErrorS(err, "Invalid datastore candidates", "method", "CreateVolume", "params", rawParams)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	accessMode := volumeCapabilities[0].GetAccessMode()
	if accessMode == nil {
		s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
		klog.V(0).ErrorS(nil, "method", "CreateVolume", "CreateVolume called with empty access mode")
		return nil, status.Error(codes.InvalidArgument, "missing access mode")
	}

	volumeAccessModel, err := resolveVolumeAccessModel(volumeCapabilities[0], selection.AllowedTypes, rawParams, candidates)
	if err != nil {
		s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	response := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      name,
			CapacityBytes: requiredBytes,
		},
	}

	volumeContext := buildVolumeContext(rawParams)
	params := filterProvisioningParams(rawParams)
	fsType := getRequestedFSType(rawParams, volumeCapabilities[0])
	immutableVolume := accessMode.Mode == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
	contentSource := req.GetVolumeContentSource()

	response.Volume.VolumeContext = volumeContext
	if contentSource != nil {
		response.Volume.ContentSource = contentSource
	}

	if volumeAccessModel == opennebula.VolumeAccessModelSharedFS {
		if s.sharedFilesystemProvider == nil {
			s.driver.metrics.RecordOperation("create_volume", "cephfs", "failed_precondition", time.Since(started))
			return nil, status.Error(codes.FailedPrecondition, "shared filesystem provider is not configured")
		}
		backend = "cephfs"

		if contentSource != nil {
			var (
				result   *opennebula.SharedVolumeCreateResult
				cloneErr error
			)
			switch source := contentSource.Type.(type) {
			case *csi.VolumeContentSource_Volume:
				if !s.driver.featureGates.CephFSClones {
					s.driver.metrics.RecordOperation("create_volume", backend, "unimplemented", time.Since(started))
					return nil, status.Error(codes.Unimplemented, "CephFS volume cloning is disabled by feature gate")
				}
				result, cloneErr = s.sharedFilesystemProvider.CloneSharedVolume(ctx, opennebula.SharedVolumeCloneRequest{
					Name:           name,
					SizeBytes:      requiredBytes,
					Selection:      selection,
					Parameters:     rawParams,
					Secrets:        req.GetSecrets(),
					SourceVolumeID: source.Volume.GetVolumeId(),
				})
			case *csi.VolumeContentSource_Snapshot:
				if !opennebula.IsSharedFilesystemSnapshotID(source.Snapshot.GetSnapshotId()) {
					s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
					return nil, status.Error(codes.InvalidArgument, "shared filesystem restore requires a CephFS snapshot source")
				}
				if !s.driver.featureGates.CephFSSnapshots || !s.driver.featureGates.CephFSClones {
					s.driver.metrics.RecordOperation("create_volume", backend, "unimplemented", time.Since(started))
					return nil, status.Error(codes.Unimplemented, "CephFS snapshot restore is disabled by feature gate")
				}
				result, cloneErr = s.sharedFilesystemProvider.CloneSharedVolume(ctx, opennebula.SharedVolumeCloneRequest{
					Name:             name,
					SizeBytes:        requiredBytes,
					Selection:        selection,
					Parameters:       rawParams,
					Secrets:          req.GetSecrets(),
					SourceSnapshotID: source.Snapshot.GetSnapshotId(),
				})
			default:
				s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
				return nil, status.Error(codes.InvalidArgument, "unsupported shared filesystem content source")
			}

			if cloneErr != nil {
				switch {
				case opennebula.IsDatastoreConfigError(cloneErr):
					s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
					return nil, status.Error(codes.InvalidArgument, cloneErr.Error())
				case opennebula.IsDatastoreCapacityError(cloneErr):
					s.driver.metrics.RecordOperation("create_volume", backend, "resource_exhausted", time.Since(started))
					return nil, status.Error(codes.ResourceExhausted, cloneErr.Error())
				}
				s.driver.metrics.RecordOperation("create_volume", backend, "internal", time.Since(started))
				return nil, status.Error(codes.Internal, "failed to clone shared filesystem volume")
			}

			response.Volume.VolumeId = result.VolumeID
			response.Volume.CapacityBytes = result.CapacityBytes
			if s.driver.featureGates.TopologyAccessibility {
				response.Volume.AccessibleTopology = accessibleTopologyForDatastore(result.Datastore)
			}
			s.driver.metrics.RecordOperation("create_volume", backend, "success", time.Since(started))
			s.driver.metrics.RecordDatastoreSelection(string(selection.Policy), result.Datastore.Backend, result.Datastore.ID, "selected")
			s.driver.metrics.SetDatastoreCapacity(result.Datastore.Backend, result.Datastore.ID, result.Datastore.FreeBytes, result.Datastore.TotalBytes)
			s.recordPVCEventFromParams(ctx, rawParams, eventReasonCloneCreated, fmt.Sprintf("created CephFS clone in datastore %d from content source", result.Datastore.ID))
			if result.FallbackUsed {
				s.recordPVCEventFromParams(ctx, rawParams, eventReasonDatastoreFallback, fmt.Sprintf("fallback selected datastore %d after attempts %v", result.Datastore.ID, result.AttemptedDatastoreIDs))
			}
			s.recordPVCEventFromParams(ctx, rawParams, eventReasonDatastoreSelected, fmt.Sprintf("selected %s datastore %d (%s) with policy %s", result.Datastore.Backend, result.Datastore.ID, result.Datastore.Name, selection.Policy))
			s.annotatePlacementFromParams(ctx, rawParams, PlacementReport{
				Backend:                     result.Metadata.Backend,
				DatastoreID:                 result.Datastore.ID,
				DatastoreName:               result.Datastore.Name,
				SelectionPolicy:             string(selection.Policy),
				FallbackUsed:                result.FallbackUsed,
				CompatibilityAwareSelection: s.driver.featureGates.CompatibilityAwareSelection,
			})
			setVolumeContextPlacement(response.Volume, result.Metadata.Backend, result.Datastore.ID, result.Datastore.Name, string(selection.Policy))
			ensureVolumeContextUUID(response.Volume, requestedPVCUUID)
			return response, nil
		}

		result, sharedErr := s.sharedFilesystemProvider.CreateSharedVolume(ctx, opennebula.SharedVolumeRequest{
			Name:       name,
			SizeBytes:  requiredBytes,
			Selection:  selection,
			Parameters: rawParams,
			Secrets:    req.GetSecrets(),
		})
		if sharedErr != nil {
			switch {
			case opennebula.IsDatastoreConfigError(sharedErr):
				s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
				s.recordPVCWarningFromParams(ctx, rawParams, eventReasonDatastoreRejected, sharedErr.Error())
				return nil, status.Error(codes.InvalidArgument, sharedErr.Error())
			case opennebula.IsDatastoreCapacityError(sharedErr):
				s.driver.metrics.RecordOperation("create_volume", backend, "resource_exhausted", time.Since(started))
				return nil, status.Error(codes.ResourceExhausted, sharedErr.Error())
			}
			s.driver.metrics.RecordOperation("create_volume", backend, "internal", time.Since(started))
			klog.V(0).ErrorS(sharedErr, "Failed to create shared filesystem volume",
				"method", "CreateVolume", "volumeName", name, "requiredBytes", requiredBytes,
				"policy", selection.Policy, "datastores", selection.Identifiers)
			return nil, status.Error(codes.Internal, "failed to create volume")
		}

		response.Volume.VolumeId = result.VolumeID
		response.Volume.CapacityBytes = result.CapacityBytes
		if s.driver.featureGates.TopologyAccessibility {
			response.Volume.AccessibleTopology = accessibleTopologyForDatastore(result.Datastore)
		}
		s.driver.metrics.RecordOperation("create_volume", backend, "success", time.Since(started))
		s.driver.metrics.RecordDatastoreSelection(string(selection.Policy), result.Datastore.Backend, result.Datastore.ID, "selected")
		s.driver.metrics.SetDatastoreCapacity(result.Datastore.Backend, result.Datastore.ID, result.Datastore.FreeBytes, result.Datastore.TotalBytes)
		if result.FallbackUsed {
			s.recordPVCEventFromParams(ctx, rawParams, eventReasonDatastoreFallback, fmt.Sprintf("fallback selected datastore %d after attempts %v", result.Datastore.ID, result.AttemptedDatastoreIDs))
		}
		s.recordPVCEventFromParams(ctx, rawParams, eventReasonDatastoreSelected, fmt.Sprintf("selected %s datastore %d (%s) with policy %s", result.Datastore.Backend, result.Datastore.ID, result.Datastore.Name, selection.Policy))
		s.annotatePlacementFromParams(ctx, rawParams, PlacementReport{
			Backend:                     result.Metadata.Backend,
			DatastoreID:                 result.Datastore.ID,
			DatastoreName:               result.Datastore.Name,
			SelectionPolicy:             string(selection.Policy),
			FallbackUsed:                result.FallbackUsed,
			CompatibilityAwareSelection: s.driver.featureGates.CompatibilityAwareSelection,
		})
		klog.V(1).InfoS("Shared filesystem volume created successfully",
			"method", "CreateVolume", "volumeName", name, "requiredBytes", requiredBytes,
			"policy", selection.Policy, "datastores", selection.Identifiers,
			"selectedDatastoreID", result.Datastore.ID, "selectedDatastoreName", result.Datastore.Name,
			"backend", result.Metadata.Backend, "mode", result.Metadata.Mode)

		setVolumeContextPlacement(response.Volume, result.Metadata.Backend, result.Datastore.ID, result.Datastore.Name, string(selection.Policy))
		ensureVolumeContextUUID(response.Volume, requestedPVCUUID)
		return response, nil
	}
	backend = "disk"

	if contentSource != nil {
		switch source := contentSource.Type.(type) {
		case *csi.VolumeContentSource_Volume:
			result, err := s.volumeProvider.CloneVolume(ctx, name, source.Volume.GetVolumeId(), selection)
			if err != nil {
				switch {
				case opennebula.IsDatastoreConfigError(err):
					s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
					return nil, status.Error(codes.InvalidArgument, err.Error())
				case opennebula.IsDatastoreCapacityError(err):
					s.driver.metrics.RecordOperation("create_volume", backend, "resource_exhausted", time.Since(started))
					return nil, status.Error(codes.ResourceExhausted, err.Error())
				}
				s.driver.metrics.RecordOperation("create_volume", backend, "internal", time.Since(started))
				return nil, status.Error(codes.Internal, "failed to clone volume")
			}

			if result.CapacityBytes > 0 {
				response.Volume.CapacityBytes = result.CapacityBytes
			}
			if s.driver.featureGates.TopologyAccessibility {
				response.Volume.AccessibleTopology = accessibleTopologyForDatastore(result.Datastore)
			}
			s.driver.metrics.RecordOperation("create_volume", result.Datastore.Backend, "success", time.Since(started))
			s.recordPVCEventFromParams(ctx, rawParams, eventReasonCloneCreated, fmt.Sprintf("cloned source volume %s into datastore %d", source.Volume.GetVolumeId(), result.Datastore.ID))
			if result.FallbackUsed {
				s.recordPVCEventFromParams(ctx, rawParams, eventReasonDatastoreFallback, fmt.Sprintf("fallback selected datastore %d after attempts %v", result.Datastore.ID, result.AttemptedDatastoreIDs))
			}
			s.recordPVCEventFromParams(ctx, rawParams, eventReasonDatastoreSelected, fmt.Sprintf("selected %s datastore %d (%s) with policy %s", result.Datastore.Backend, result.Datastore.ID, result.Datastore.Name, selection.Policy))
			s.driver.metrics.RecordDatastoreSelection(string(selection.Policy), result.Datastore.Backend, result.Datastore.ID, "selected")
			s.driver.metrics.SetDatastoreCapacity(result.Datastore.Backend, result.Datastore.ID, result.Datastore.FreeBytes, result.Datastore.TotalBytes)
			s.annotatePlacementFromParams(ctx, rawParams, PlacementReport{
				Backend:                     result.Datastore.Backend,
				DatastoreID:                 result.Datastore.ID,
				DatastoreName:               result.Datastore.Name,
				SelectionPolicy:             string(selection.Policy),
				FallbackUsed:                result.FallbackUsed,
				CompatibilityAwareSelection: s.driver.featureGates.CompatibilityAwareSelection,
			})
			setVolumeContextPlacement(response.Volume, result.Datastore.Backend, result.Datastore.ID, result.Datastore.Name, string(selection.Policy))
			ensureVolumeContextUUID(response.Volume, requestedPVCUUID)
			return response, nil
		case *csi.VolumeContentSource_Snapshot:
			s.driver.metrics.RecordOperation("create_volume", backend, "unimplemented", time.Since(started))
			return nil, status.Error(codes.Unimplemented, "restoring a volume from an OpenNebula image snapshot is not supported by the current backend")
		default:
			s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
			return nil, status.Error(codes.InvalidArgument, "unsupported volume content source")
		}
	}

	volumeID, volumeSize, _ := s.volumeProvider.VolumeExists(ctx, name)
	if volumeID != -1 {
		if int64(volumeSize) == requiredBytes {
			s.driver.metrics.RecordOperation("create_volume", backend, "success", time.Since(started))
			klog.V(3).InfoS("Volume already exists with the same size",
				"method", "CreateVolume", "volumeID", volumeID, "requiredSize", requiredBytes)
			ensureVolumeContextUUID(response.Volume, requestedPVCUUID)
			return response, nil
		}
		s.driver.metrics.RecordOperation("create_volume", backend, "already_exists", time.Since(started))
		klog.V(0).ErrorS(nil, "Volume with the same name already exists with different size",
			"method", "CreateVolume", "volumeID", volumeID, "existingSize", volumeSize, "requiredSize", requiredBytes)
		return nil, status.Error(codes.AlreadyExists,
			"volume with the same name already exists with different size")

	}

	result, err := s.volumeProvider.CreateVolume(ctx, name, requiredBytes, DefaultDriverName, immutableVolume, fsType, params, selection)
	if err != nil {
		switch {
		case opennebula.IsDatastoreConfigError(err):
			s.driver.metrics.RecordOperation("create_volume", backend, "invalid_argument", time.Since(started))
			s.recordPVCWarningFromParams(ctx, rawParams, eventReasonDatastoreRejected, err.Error())
			return nil, status.Error(codes.InvalidArgument, err.Error())
		case opennebula.IsDatastoreCapacityError(err):
			s.driver.metrics.RecordOperation("create_volume", backend, "resource_exhausted", time.Since(started))
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		s.driver.metrics.RecordOperation("create_volume", backend, "internal", time.Since(started))
		klog.V(0).ErrorS(err, "Failed to create volume",
			"method", "CreateVolume", "volumeName", name, "requiredBytes", requiredBytes,
			"defaultDriverName", DefaultDriverName, "immutableVolume", immutableVolume,
			"policy", selection.Policy, "datastores", selection.Identifiers, "params", params)
		return nil, status.Error(codes.Internal, "failed to create volume")
	}
	s.driver.metrics.RecordOperation("create_volume", result.Datastore.Backend, "success", time.Since(started))
	if result.CapacityBytes > 0 {
		response.Volume.CapacityBytes = result.CapacityBytes
	}
	if s.driver.featureGates.TopologyAccessibility {
		response.Volume.AccessibleTopology = accessibleTopologyForDatastore(result.Datastore)
	}
	s.driver.metrics.RecordDatastoreSelection(string(selection.Policy), result.Datastore.Backend, result.Datastore.ID, "selected")
	s.driver.metrics.SetDatastoreCapacity(result.Datastore.Backend, result.Datastore.ID, result.Datastore.FreeBytes, result.Datastore.TotalBytes)
	if result.FallbackUsed {
		s.recordPVCEventFromParams(ctx, rawParams, eventReasonDatastoreFallback, fmt.Sprintf("fallback selected datastore %d after attempts %v", result.Datastore.ID, result.AttemptedDatastoreIDs))
	}
	s.recordPVCEventFromParams(ctx, rawParams, eventReasonDatastoreSelected, fmt.Sprintf("selected %s datastore %d (%s) with policy %s", result.Datastore.Backend, result.Datastore.ID, result.Datastore.Name, selection.Policy))
	s.annotatePlacementFromParams(ctx, rawParams, PlacementReport{
		Backend:                     result.Datastore.Backend,
		DatastoreID:                 result.Datastore.ID,
		DatastoreName:               result.Datastore.Name,
		SelectionPolicy:             string(selection.Policy),
		FallbackUsed:                result.FallbackUsed,
		CompatibilityAwareSelection: s.driver.featureGates.CompatibilityAwareSelection,
	})

	klog.V(1).InfoS("Volume created successfully",
		"method", "CreateVolume", "volumeName", name, "requiredBytes",
		requiredBytes, "defaultDriverName", DefaultDriverName, "immutableVolume", immutableVolume,
		"policy", selection.Policy, "datastores", selection.Identifiers,
		"selectedDatastoreID", result.Datastore.ID, "selectedDatastoreName", result.Datastore.Name,
		"params", params)

	setVolumeContextPlacement(response.Volume, result.Datastore.Backend, result.Datastore.ID, result.Datastore.Name, string(selection.Policy))
	ensureVolumeContextUUID(response.Volume, requestedPVCUUID)
	return response, nil
}

func buildVolumeContext(params map[string]string) map[string]string {
	volumeContext := filterProvisioningParams(params)
	for _, key := range []string{paramPVCUID, paramPVCName, paramPVCNamespace, paramPVName} {
		if value := strings.TrimSpace(params[key]); value != "" {
			volumeContext[key] = value
		}
	}
	if value := strings.TrimSpace(params[storageClassParamCephFSMounter]); value != "" {
		volumeContext[storageClassParamCephFSMounter] = value
	}
	return volumeContext
}

func setVolumeContextPlacement(volume *csi.Volume, backend string, datastoreID int, datastoreName, selectionPolicy string) {
	if volume == nil {
		return
	}
	if volume.VolumeContext == nil {
		volume.VolumeContext = make(map[string]string)
	}
	if normalizedBackend := strings.TrimSpace(backend); normalizedBackend != "" {
		volume.VolumeContext[annotationBackend] = normalizedBackend
	}
	if datastoreID > 0 {
		volume.VolumeContext[annotationDatastoreID] = strconv.Itoa(datastoreID)
	}
	if normalizedDatastoreName := strings.TrimSpace(datastoreName); normalizedDatastoreName != "" {
		volume.VolumeContext[annotationDatastoreName] = normalizedDatastoreName
	}
	if normalizedSelectionPolicy := strings.TrimSpace(selectionPolicy); normalizedSelectionPolicy != "" {
		volume.VolumeContext[annotationSelectionPolicy] = normalizedSelectionPolicy
	}
}

func ensureVolumeContextUUID(volume *csi.Volume, requestedUUID string) {
	if volume == nil {
		return
	}
	if volume.VolumeContext == nil {
		volume.VolumeContext = make(map[string]string)
	}
	if existing := strings.TrimSpace(volume.VolumeContext[volumeContextUUID]); existing != "" {
		return
	}
	if requested := strings.TrimSpace(requestedUUID); requested != "" {
		volume.VolumeContext[volumeContextUUID] = requested
		return
	}
	if volumeID := strings.TrimSpace(volume.GetVolumeId()); volumeID != "" {
		volume.VolumeContext[volumeContextUUID] = volumeID
	}
}

func (s *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	started := time.Now()
	klog.V(1).InfoS("DeleteVolume called", "req", protosanitizer.StripSecrets(req))

	if req.VolumeId == "" {
		s.driver.metrics.RecordOperation("delete_volume", "unknown", "invalid_argument", time.Since(started))
		klog.V(0).ErrorS(nil, "method", "DeleteVolume", "DeleteVolume called with empty volume ID")
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}

	klog.V(3).InfoS("Deleting volume", "method", "DeleteVolume", "volumeID", req.VolumeId)

	if opennebula.IsSharedFilesystemVolumeID(req.VolumeId) {
		if s.sharedFilesystemProvider == nil {
			return nil, status.Error(codes.FailedPrecondition, "shared filesystem provider is not configured")
		}

		err := s.sharedFilesystemProvider.DeleteSharedVolume(ctx, req.VolumeId, req.GetSecrets())
		if err != nil {
			switch {
			case opennebula.IsDatastoreConfigError(err):
				s.driver.metrics.RecordOperation("delete_volume", "cephfs", "failed_precondition", time.Since(started))
				return nil, status.Error(codes.FailedPrecondition, err.Error())
			case opennebula.IsDatastoreCapacityError(err):
				s.driver.metrics.RecordOperation("delete_volume", "cephfs", "resource_exhausted", time.Since(started))
				return nil, status.Error(codes.ResourceExhausted, err.Error())
			}
			s.driver.metrics.RecordOperation("delete_volume", "cephfs", "failed_precondition", time.Since(started))
			klog.V(0).ErrorS(err, "Failed to delete shared filesystem volume", "method", "DeleteVolume", "volumeID", req.VolumeId)
			return nil, status.Error(codes.FailedPrecondition, "failed to delete volume")
		}

		s.driver.metrics.RecordOperation("delete_volume", "cephfs", "success", time.Since(started))
		klog.V(1).InfoS("Shared filesystem volume deleted successfully", "method", "DeleteVolume", "volumeID", req.VolumeId)
		return &csi.DeleteVolumeResponse{}, nil
	}

	err := s.volumeProvider.DeleteVolume(ctx, req.VolumeId)
	if err != nil {
		s.driver.metrics.RecordOperation("delete_volume", "disk", "failed_precondition", time.Since(started))
		klog.V(0).ErrorS(err, "Failed to delete volume", "method", "DeleteVolume", "volumeID", req.VolumeId)
		return nil, status.Error(codes.FailedPrecondition, "failed to delete volume")
	}

	s.driver.metrics.RecordOperation("delete_volume", "disk", "success", time.Since(started))
	klog.V(1).InfoS("Volume deleted successfully", "method", "DeleteVolume", "volumeID", req.VolumeId)
	return &csi.DeleteVolumeResponse{}, nil
}

func (s *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	started := time.Now()
	klog.V(1).InfoS("CreateSnapshot called", "req", protosanitizer.StripSecrets(req))

	if strings.TrimSpace(req.GetName()) == "" {
		s.driver.metrics.RecordSnapshot("disk", "create", "invalid_argument")
		return nil, status.Error(codes.InvalidArgument, "snapshot name is required")
	}
	if strings.TrimSpace(req.GetSourceVolumeId()) == "" {
		s.driver.metrics.RecordSnapshot("disk", "create", "invalid_argument")
		return nil, status.Error(codes.InvalidArgument, "source volume ID is required")
	}
	if opennebula.IsSharedFilesystemVolumeID(req.GetSourceVolumeId()) {
		if s.sharedFilesystemProvider == nil {
			return nil, status.Error(codes.FailedPrecondition, "shared filesystem provider is not configured")
		}
		if !s.driver.featureGates.CephFSSnapshots {
			s.driver.metrics.RecordSnapshot("cephfs", "create", "unimplemented")
			return nil, status.Error(codes.Unimplemented, "CephFS snapshots are disabled by feature gate")
		}

		snapshot, err := s.sharedFilesystemProvider.CreateSharedSnapshot(ctx, req.GetSourceVolumeId(), req.GetName(), req.GetSecrets())
		if err != nil {
			s.driver.metrics.RecordSnapshot("cephfs", "create", "internal")
			return nil, status.Error(codes.Internal, err.Error())
		}

		s.driver.metrics.RecordSnapshot("cephfs", "create", "success")
		s.driver.metrics.RecordOperation("create_snapshot", "cephfs", "success", time.Since(started))
		return &csi.CreateSnapshotResponse{
			Snapshot: &csi.Snapshot{
				SizeBytes:      snapshot.SizeBytes,
				SnapshotId:     snapshot.SnapshotID,
				SourceVolumeId: snapshot.SourceVolumeID,
				CreationTime:   timestamppb.New(snapshot.CreationTime),
				ReadyToUse:     snapshot.ReadyToUse,
			},
		}, nil
	}

	snapshot, err := s.volumeProvider.CreateSnapshot(ctx, req.GetSourceVolumeId(), req.GetName())
	if err != nil {
		s.driver.metrics.RecordSnapshot("disk", "create", "internal")
		return nil, status.Error(codes.Internal, err.Error())
	}

	s.driver.metrics.RecordSnapshot("disk", "create", "success")
	s.driver.metrics.RecordOperation("create_snapshot", "disk", "success", time.Since(started))

	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SizeBytes:      snapshot.SizeBytes,
			SnapshotId:     snapshot.SnapshotID,
			SourceVolumeId: snapshot.SourceVolumeID,
			CreationTime:   timestamppb.New(snapshot.CreationTime),
			ReadyToUse:     snapshot.ReadyToUse,
		},
	}, nil
}

func (s *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	started := time.Now()
	klog.V(1).InfoS("DeleteSnapshot called", "req", protosanitizer.StripSecrets(req))

	if strings.TrimSpace(req.GetSnapshotId()) == "" {
		s.driver.metrics.RecordSnapshot("disk", "delete", "invalid_argument")
		return nil, status.Error(codes.InvalidArgument, "snapshot ID is required")
	}
	if opennebula.IsSharedFilesystemSnapshotID(req.GetSnapshotId()) {
		if s.sharedFilesystemProvider == nil {
			return nil, status.Error(codes.FailedPrecondition, "shared filesystem provider is not configured")
		}
		if !s.driver.featureGates.CephFSSnapshots {
			s.driver.metrics.RecordSnapshot("cephfs", "delete", "unimplemented")
			return nil, status.Error(codes.Unimplemented, "CephFS snapshots are disabled by feature gate")
		}
		if err := s.sharedFilesystemProvider.DeleteSharedSnapshot(ctx, req.GetSnapshotId(), req.GetSecrets()); err != nil {
			s.driver.metrics.RecordSnapshot("cephfs", "delete", "internal")
			return nil, status.Error(codes.Internal, err.Error())
		}
		s.driver.metrics.RecordSnapshot("cephfs", "delete", "success")
		s.driver.metrics.RecordOperation("delete_snapshot", "cephfs", "success", time.Since(started))
		return &csi.DeleteSnapshotResponse{}, nil
	}

	if err := s.volumeProvider.DeleteSnapshot(ctx, req.GetSnapshotId()); err != nil {
		s.driver.metrics.RecordSnapshot("disk", "delete", "internal")
		return nil, status.Error(codes.Internal, err.Error())
	}

	s.driver.metrics.RecordSnapshot("disk", "delete", "success")
	s.driver.metrics.RecordOperation("delete_snapshot", "disk", "success", time.Since(started))
	return &csi.DeleteSnapshotResponse{}, nil
}

func (s *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	klog.V(1).InfoS("ListSnapshots called", "req", protosanitizer.StripSecrets(req))

	if opennebula.IsSharedFilesystemSnapshotID(req.GetSnapshotId()) || opennebula.IsSharedFilesystemVolumeID(req.GetSourceVolumeId()) {
		if s.sharedFilesystemProvider == nil {
			return nil, status.Error(codes.FailedPrecondition, "shared filesystem provider is not configured")
		}
		if !s.driver.featureGates.CephFSSnapshots {
			return nil, status.Error(codes.Unimplemented, "CephFS snapshots are disabled by feature gate")
		}

		snapshots, nextToken, err := s.sharedFilesystemProvider.ListSharedSnapshots(ctx, req.GetSnapshotId(), req.GetSourceVolumeId(), req.GetMaxEntries(), req.GetStartingToken(), nil)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}

		entries := make([]*csi.ListSnapshotsResponse_Entry, 0, len(snapshots))
		for _, snapshot := range snapshots {
			entries = append(entries, &csi.ListSnapshotsResponse_Entry{
				Snapshot: &csi.Snapshot{
					SizeBytes:      snapshot.SizeBytes,
					SnapshotId:     snapshot.SnapshotID,
					SourceVolumeId: snapshot.SourceVolumeID,
					CreationTime:   timestamppb.New(snapshot.CreationTime),
					ReadyToUse:     snapshot.ReadyToUse,
				},
			})
		}

		return &csi.ListSnapshotsResponse{
			Entries:   entries,
			NextToken: nextToken,
		}, nil
	}

	snapshots, nextToken, err := s.volumeProvider.ListSnapshots(ctx, req.GetSnapshotId(), req.GetSourceVolumeId(), req.GetMaxEntries(), req.GetStartingToken())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	entries := make([]*csi.ListSnapshotsResponse_Entry, 0, len(snapshots))
	for _, snapshot := range snapshots {
		entries = append(entries, &csi.ListSnapshotsResponse_Entry{
			Snapshot: &csi.Snapshot{
				SizeBytes:      snapshot.SizeBytes,
				SnapshotId:     snapshot.SnapshotID,
				SourceVolumeId: snapshot.SourceVolumeID,
				CreationTime:   timestamppb.New(snapshot.CreationTime),
				ReadyToUse:     snapshot.ReadyToUse,
			},
		})
	}

	return &csi.ListSnapshotsResponse{
		Entries:   entries,
		NextToken: nextToken,
	}, nil
}

// TODO: Process VolumeCapability, readonly
func (s *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	started := time.Now()
	klog.V(1).InfoS("ControllerPublishVolume called", "req", protosanitizer.StripSecrets(req))
	if req.VolumeId == "" {
		s.driver.metrics.RecordOperation("controller_publish_volume", "unknown", "invalid_argument", time.Since(started))
		klog.V(0).ErrorS(nil, "method", "ControllerPublishVolume", "ControllerPublishVolume called with empty volume ID")
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}

	if req.NodeId == "" {
		s.driver.metrics.RecordOperation("controller_publish_volume", "unknown", "invalid_argument", time.Since(started))
		klog.V(0).ErrorS(nil, "method", "ControllerPublishVolume", "ControllerPublishVolume called with empty node ID")
		return nil, status.Error(codes.InvalidArgument, "missing node ID")
	}

	if req.VolumeCapability == nil {
		s.driver.metrics.RecordOperation("controller_publish_volume", "unknown", "invalid_argument", time.Since(started))
		klog.V(0).ErrorS(nil, "method", "ControllerPublishVolume", "ControllerPublishVolume called with empty volume capability")
		return nil, status.Error(codes.InvalidArgument, "missing volume capability")
	}

	if req.VolumeCapability.AccessMode == nil {
		s.driver.metrics.RecordOperation("controller_publish_volume", "unknown", "invalid_argument", time.Since(started))
		klog.V(0).ErrorS(nil, "method", "ControllerPublishVolume", "ControllerPublishVolume called with empty access mode")
		return nil, status.Error(codes.InvalidArgument, "missing access mode")
	}

	immutableVolume := req.VolumeCapability.AccessMode.Mode == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY

	if opennebula.IsSharedFilesystemVolumeID(req.VolumeId) {
		if s.sharedFilesystemProvider == nil {
			return nil, status.Error(codes.FailedPrecondition, "shared filesystem provider is not configured")
		}

		nodeID, err := s.volumeProvider.NodeExists(ctx, req.NodeId)
		if err != nil || nodeID == -1 {
			s.driver.metrics.RecordOperation("controller_publish_volume", "cephfs", "not_found", time.Since(started))
			klog.V(0).ErrorS(err, "Node does not exist", "method", "ControllerPublishVolume", "nodeID", req.NodeId)
			return nil, status.Error(codes.NotFound, "node not found")
		}

		publishContext, err := s.sharedFilesystemProvider.PublishSharedVolume(ctx, req.VolumeId, req.GetReadonly() || immutableVolume)
		if err != nil {
			switch {
			case opennebula.IsDatastoreConfigError(err):
				s.driver.metrics.RecordOperation("controller_publish_volume", "cephfs", "failed_precondition", time.Since(started))
				return nil, status.Error(codes.FailedPrecondition, err.Error())
			case opennebula.IsDatastoreCapacityError(err):
				s.driver.metrics.RecordOperation("controller_publish_volume", "cephfs", "resource_exhausted", time.Since(started))
				return nil, status.Error(codes.ResourceExhausted, err.Error())
			}
			s.driver.metrics.RecordOperation("controller_publish_volume", "cephfs", "internal", time.Since(started))
			klog.V(0).ErrorS(err, "Failed to publish shared filesystem volume",
				"method", "ControllerPublishVolume", "volumeID", req.VolumeId, "nodeID", req.NodeId)
			return nil, status.Error(codes.Internal, "failed to publish volume")
		}

		s.driver.metrics.RecordOperation("controller_publish_volume", "cephfs", "success", time.Since(started))
		_ = nodeID
		if requestedMounter := strings.TrimSpace(req.GetVolumeContext()[storageClassParamCephFSMounter]); requestedMounter != "" {
			publishContext[sharedPublishContextCephFSMounter] = requestedMounter
		}
		return &csi.ControllerPublishVolumeResponse{PublishContext: publishContext}, nil
	}

	volumeID, _, err := s.volumeProvider.VolumeExists(ctx, req.VolumeId)
	if err != nil || volumeID == -1 {
		s.driver.metrics.RecordOperation("controller_publish_volume", "disk", "not_found", time.Since(started))
		klog.V(0).ErrorS(err, "Volume does not exist", "method", "ControllerPublishVolume", "volumeID", req.VolumeId)
		return nil, status.Error(codes.NotFound, "volume not found")
	}

	nodeID, err := s.volumeProvider.NodeExists(ctx, req.NodeId)
	if err != nil || nodeID == -1 {
		s.driver.metrics.RecordOperation("controller_publish_volume", "disk", "not_found", time.Since(started))
		klog.V(0).ErrorS(err, "Node does not exist", "method", "ControllerPublishVolume", "nodeID", req.NodeId)
		return nil, status.Error(codes.NotFound, "node not found")
	}

	target, err := s.volumeProvider.GetVolumeInNode(ctx, volumeID, nodeID)
	if err == nil {
		if s.driver.volumeQuarantine != nil {
			_ = s.driver.volumeQuarantine.Clear(ctx, req.VolumeId)
		}
		s.clearHostArtifactQuarantineForVolume(ctx, req.VolumeId)
		s.clearStickyReuseState(ctx, req.VolumeId, req.NodeId, "disk")
		s.annotateRestartOptimizationForVolume(ctx, req.VolumeId, req.NodeId)
		publishContext := s.publishContextForVolume(ctx, req.VolumeId, target, 0, req.GetVolumeContext())
		s.driver.metrics.RecordOperation("controller_publish_volume", "disk", "success", time.Since(started))
		s.driver.metrics.RecordControllerPublishDuration("disk", "success", time.Since(started))
		klog.V(1).InfoS("Volume already attached to node",
			"method", "ControllerPublishVolume", "volumeID", volumeID, "nodeID", nodeID, "volumeName", target)
		return &csi.ControllerPublishVolumeResponse{
			PublishContext: publishContext,
		}, nil
	}

	if blocked, message := s.maintenanceBlocksPublish(ctx, req); blocked {
		s.driver.metrics.RecordMaintenanceBlock("publish", "cross_node_attach")
		s.recordPVCWarningFromParams(ctx, req.GetVolumeContext(), eventReasonMaintenanceModeBlocked, message)
		return nil, status.Error(codes.Unavailable, message)
	}

	if cooldown, ok := s.hotplugCooldownState(ctx, req.NodeId); ok {
		s.driver.metrics.RecordHotplugGuard("attach", "vm_cooldown", "rejected")
		s.driver.metrics.RecordHotplugRecovery("attach", "vm_cooldown", "rejected")
		message := s.formatHotplugGuardMessage(cooldown)
		s.recordPVCWarningFromParams(ctx, req.GetVolumeContext(), eventReasonHotplugCooldown, message)
		return nil, status.Error(codes.Unavailable, message)
	}

	priority := hotplugQueuePriorityNormal
	if _, ok := s.driver.stickyAttachments.Get(req.VolumeId); ok {
		priority = hotplugQueuePriorityCritical
	}
	var response *csi.ControllerPublishVolumeResponse
	queueErr := s.withQueuedHotplug(ctx, req.NodeId, "attach", req.VolumeId, priority, func(queueCtx context.Context) error {
		nodeRelease := s.driver.operationLocks.Acquire(controllerNodeLockKey(req.NodeId))
		defer nodeRelease()

		release := s.driver.operationLocks.Acquire(controllerVolumeLockKey(req.VolumeId))
		defer release()

		if reused, reusedResponse, reuseErr := s.reuseStickyAttachment(queueCtx, req, volumeID, nodeID); reused {
			if reuseErr != nil {
				return reuseErr
			}
			s.annotateRestartOptimizationForVolume(queueCtx, req.VolumeId, req.NodeId)
			response = reusedResponse
			return nil
		}

		target, err = s.volumeProvider.GetVolumeInNode(queueCtx, volumeID, nodeID)
		if err == nil {
			if s.driver.volumeQuarantine != nil {
				_ = s.driver.volumeQuarantine.Clear(queueCtx, req.VolumeId)
			}
			s.clearHostArtifactQuarantineForVolume(queueCtx, req.VolumeId)
			s.clearStickyReuseState(queueCtx, req.VolumeId, req.NodeId, "disk")
			s.annotateRestartOptimizationForVolume(queueCtx, req.VolumeId, req.NodeId)
			response = &csi.ControllerPublishVolumeResponse{
				PublishContext: s.publishContextForVolume(queueCtx, req.VolumeId, target, 0, req.GetVolumeContext()),
			}
			return nil
		}

		if driftErr := s.rejectIfOpenNebulaMetadataDrift(queueCtx, req); driftErr != nil {
			return driftErr
		}
		if hostArtifactErr := s.rejectIfHostArtifactQuarantineActive(queueCtx, req); hostArtifactErr != nil {
			return hostArtifactErr
		}

		params := req.GetVolumeContext()
		sizeBytes, err := s.volumeProvider.ResolveVolumeSizeBytes(queueCtx, req.VolumeId)
		if err != nil {
			return status.Error(codes.Internal, "failed to resolve volume size")
		}
		hotplugTimeout := s.effectiveHotplugTimeout("attach", "disk", sizeBytes, s.volumeProvider.ComputeHotplugTimeout(sizeBytes))
		deviceDiscoveryTimeout := s.nodeDeviceDiscoveryTimeout("disk", sizeBytes, hotplugTimeout)
		s.recordPVCEventFromParams(queueCtx, params, eventReasonHotplugQueued, fmt.Sprintf("queued attach for volume %s on node %s", req.GetVolumeId(), req.GetNodeId()))
		s.recordPVCEventFromParams(queueCtx, params, eventReasonHotplugTimeoutScaled, fmt.Sprintf("computed hotplug timeout %s for volume size %d bytes", hotplugTimeout, sizeBytes))
		klog.V(3).InfoS("Attaching volume to node",
			"method", "ControllerPublishVolume", "volumeID", volumeID, "nodeID", nodeID, "sizeBytes", sizeBytes, "hotplugTimeout", hotplugTimeout, "deviceDiscoveryTimeout", deviceDiscoveryTimeout)
		attachStarted := time.Now()
		err = s.volumeProvider.AttachVolume(queueCtx, req.VolumeId, req.NodeId, immutableVolume, params)
		if err != nil {
			if conflict, ok := opennebula.AsHostArtifactConflictError(err); ok {
				return s.handleHostArtifactConflict(queueCtx, req, conflict)
			}
			s.handleHotplugTimeout(queueCtx, req.NodeId, req.VolumeId, params, "attach", "disk", err)
			switch {
			case opennebula.IsDatastoreConfigError(err):
				return status.Error(codes.FailedPrecondition, err.Error())
			case opennebula.IsDatastoreCapacityError(err):
				return status.Error(codes.ResourceExhausted, err.Error())
			}
			return status.Error(codes.Internal, "failed to attach volume")
		}
		s.clearHotplugGuardState(queueCtx, req.NodeId)
		if s.driver.volumeQuarantine != nil {
			_ = s.driver.volumeQuarantine.Clear(queueCtx, req.VolumeId)
		}
		s.clearHostArtifactQuarantineForVolume(queueCtx, req.VolumeId)

		klog.V(3).InfoS("Checking if volume is attached",
			"method", "ControllerPublishVolume", "volumeID", volumeID, "nodeID", nodeID)
		target, err = s.volumeProvider.GetVolumeInNode(queueCtx, volumeID, nodeID)
		if err != nil {
			return status.Error(codes.Internal, "failed to get volume in node")
		}
		s.driver.observeAdaptiveTimeout(queueCtx, "attach", "disk", sizeBytes, time.Since(attachStarted))

		s.driver.metrics.RecordAttachValidation("disk", "attach", "success")
		s.recordPVCEventFromParams(queueCtx, req.GetVolumeContext(), eventReasonAttachValidated, fmt.Sprintf("validated and attached volume %s to node %s", req.GetVolumeId(), req.GetNodeId()))
		klog.V(1).InfoS("Volume attached successfully",
			"method", "ControllerPublishVolume", "volumeID", volumeID, "nodeID", nodeID, "volumeName", target)
		s.annotateRestartOptimizationForVolume(queueCtx, req.VolumeId, req.NodeId)
		response = &csi.ControllerPublishVolumeResponse{
			PublishContext: s.publishContextForVolume(queueCtx, req.VolumeId, target, deviceDiscoveryTimeout, req.GetVolumeContext()),
		}
		return nil
	})
	if queueErr != nil {
		s.driver.metrics.RecordOperation("controller_publish_volume", "disk", "internal", time.Since(started))
		s.driver.metrics.RecordControllerPublishDuration("disk", "internal", time.Since(started))
		var pausedQueueErr *HotplugQueuePausedError
		if errors.As(queueErr, &pausedQueueErr) {
			s.recordPVCWarningFromParams(ctx, req.GetVolumeContext(), eventReasonHotplugCooldown, pausedQueueErr.Error())
			return nil, status.Error(codes.Unavailable, pausedQueueErr.Error())
		}
		var queueTimeoutErr *HotplugQueueTimeoutError
		if errors.As(queueErr, &queueTimeoutErr) {
			s.recordPVCWarningFromParams(ctx, req.GetVolumeContext(), eventReasonHotplugQueueTimeout, queueTimeoutErr.Error())
			return nil, status.Error(codes.DeadlineExceeded, queueTimeoutErr.Error())
		}
		var activeTimeoutErr *HotplugQueueActiveTimeoutError
		if errors.As(queueErr, &activeTimeoutErr) {
			if diagnosis, ok := s.diagnoseAndPersistHotplug(ctx, req.NodeId); ok {
				s.driver.metrics.RecordHotplugRecovery("attach", strings.ToLower(diagnosis.Classification), "queue_active_timeout")
			}
			s.recordPVCWarningFromParams(ctx, req.GetVolumeContext(), eventReasonHotplugQueueTimeout, activeTimeoutErr.Error())
			return nil, status.Error(codes.DeadlineExceeded, activeTimeoutErr.Error())
		}
		var staleQueueErr *HotplugQueueStaleRequestError
		if errors.As(queueErr, &staleQueueErr) {
			s.recordPVCWarningFromParams(ctx, req.GetVolumeContext(), eventReasonHotplugQueueStale, staleQueueErr.Error())
			return nil, status.Error(codes.Aborted, staleQueueErr.Error())
		}
		if status.Code(queueErr) == codes.Unknown {
			return nil, status.Error(codes.Aborted, queueErr.Error())
		}
		return nil, queueErr
	}

	s.driver.metrics.RecordOperation("controller_publish_volume", "disk", "success", time.Since(started))
	s.driver.metrics.RecordControllerPublishDuration("disk", "success", time.Since(started))
	return response, nil
}

func (s *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	klog.V(1).InfoS("ControllerUnpublishVolume called", "req", protosanitizer.StripSecrets(req))
	if req.VolumeId == "" {
		klog.V(0).ErrorS(nil, "method", "ControllerUnpublishVolume", "ControllerUnpublishVolume called with empty volume ID")
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}
	if req.NodeId == "" {
		klog.V(0).ErrorS(nil, "method", "ControllerUnpublishVolume", "ControllerUnpublishVolume called with empty node ID")
		return nil, status.Error(codes.InvalidArgument, "missing node ID")
	}

	if opennebula.IsSharedFilesystemVolumeID(req.VolumeId) {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	volumeID, _, err := s.volumeProvider.VolumeExists(ctx, req.VolumeId)
	if err != nil || volumeID == -1 {
		klog.V(1).InfoS("Volume not found, skipping volume unpublish",
			"method", "ControllerUnpublishVolume", "volumeID", req.VolumeId)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	nodeID, err := s.volumeProvider.NodeExists(ctx, req.NodeId)
	if err != nil || nodeID == -1 {
		volumeReady, err := s.volumeProvider.VolumeReadyWithTimeout(volumeID)
		if err != nil {
			klog.V(0).ErrorS(err, "Failed to check if volume is used",
				"method", "ControllerUnpublishVolume", "volumeID", req.VolumeId)
			return nil, status.Error(codes.Internal, "failed to check if volume is used")
		}

		if !volumeReady {
			klog.V(0).ErrorS(err, "Node not found and volume is still in use",
				"method", "ControllerUnpublishVolume", "nodeID", req.NodeId)
			return nil, status.Error(codes.NotFound, "node not found and volume is still in use")
		}
		klog.V(1).InfoS("Node not found and volume not in use, skipping volume unpublish",
			"method", "ControllerUnpublishVolume", "nodeID", req.NodeId)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	_, err = s.volumeProvider.GetVolumeInNode(ctx, volumeID, nodeID)
	if err != nil {
		klog.V(1).InfoS("Volume does not exist in node, skipping unpublish",
			"method", "ControllerUnpublishVolume", "volumeID", req.VolumeId, "nodeID", req.NodeId)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	if rescued, response, rescueErr := s.rescueSameNodeUnpublish(ctx, req, volumeID, nodeID); rescued {
		return response, rescueErr
	}

	if held, response, holdErr := s.startMaintenanceDetachHold(ctx, req); held {
		return response, holdErr
	}

	if cooldown, ok := s.hotplugCooldownState(ctx, req.NodeId); ok {
		s.driver.metrics.RecordHotplugGuard("detach", "vm_cooldown", "rejected")
		message := s.formatHotplugGuardMessage(cooldown)
		return nil, status.Error(codes.Unavailable, message)
	}

	queueErr := s.withQueuedHotplug(ctx, req.NodeId, "detach", req.VolumeId, hotplugQueuePriorityNormal, func(queueCtx context.Context) error {
		nodeRelease := s.driver.operationLocks.Acquire(controllerNodeLockKey(req.NodeId))
		defer nodeRelease()

		release := s.driver.operationLocks.Acquire(controllerVolumeLockKey(req.VolumeId))
		defer release()

		_, err = s.volumeProvider.GetVolumeInNode(queueCtx, volumeID, nodeID)
		if err != nil {
			klog.V(1).InfoS("Volume does not exist in node, skipping unpublish",
				"method", "ControllerUnpublishVolume", "volumeID", req.VolumeId, "nodeID", req.NodeId)
			return nil
		}

		runtimeCtx, stickyState, stickyEligible, stickyReason := s.shouldDelayDetach(queueCtx, req)
		if stickyEligible {
			if err := s.startDetachGrace(queueCtx, runtimeCtx, stickyState); err != nil {
				klog.V(2).InfoS("Sticky detach grace start failed, falling back to immediate detach",
					"method", "ControllerUnpublishVolume", "volumeID", req.VolumeId, "nodeID", req.NodeId, "err", err)
				s.recordPVCWarningFromRuntimeContext(queueCtx, runtimeCtx, eventReasonDetachGraceBypassed, fmt.Sprintf("failed to persist detach grace for volume %s: %v", req.VolumeId, err))
				s.driver.metrics.RecordStickyDetach("bypassed", "persist_failed")
			} else {
				return nil
			}
		} else if stickyReason != "" {
			s.driver.metrics.RecordStickyDetach("bypassed", stickyReason)
			s.recordPVCEventFromRuntimeContext(queueCtx, runtimeCtx, eventReasonDetachGraceBypassed, fmt.Sprintf("detach grace bypassed for volume %s: %s", req.VolumeId, stickyReason))
		}

		if runtimeCtx != nil {
			s.recordPVCEventFromParams(queueCtx, map[string]string{
				paramPVCName:      runtimeCtx.PVCName,
				paramPVCNamespace: runtimeCtx.PVCNamespace,
			}, eventReasonHotplugQueued, fmt.Sprintf("queued detach for volume %s on node %s", req.GetVolumeId(), req.GetNodeId()))
		}
		klog.V(3).InfoS("Detaching volume from node",
			"method", "ControllerUnpublishVolume", "volumeID", volumeID, "nodeID", nodeID)

		detachStarted := time.Now()
		err = s.volumeProvider.DetachVolume(queueCtx, req.VolumeId, req.NodeId)
		if err != nil {
			s.handleHotplugTimeout(queueCtx, req.NodeId, req.VolumeId, nil, "detach", "disk", err)
			if driftErr := s.detachMetadataDriftError(queueCtx, req.VolumeId, req.NodeId, err); driftErr != nil {
				return driftErr
			}
			klog.V(0).ErrorS(err, "Failed to detach volume",
				"method", "ControllerUnpublishVolume", "volumeID", req.VolumeId, "nodeID", req.NodeId)
			return status.Error(codes.Internal, "failed to detach volume")
		}
		s.clearHotplugGuardState(queueCtx, req.NodeId)
		sizeBytes, sizeErr := s.volumeProvider.ResolveVolumeSizeBytes(queueCtx, req.VolumeId)
		if sizeErr == nil {
			s.driver.observeAdaptiveTimeout(queueCtx, "detach", "disk", sizeBytes, time.Since(detachStarted))
		}

		klog.V(1).InfoS("Volume detached successfully",
			"method", "ControllerUnpublishVolume", "volumeID", volumeID, "nodeID", nodeID)
		return nil
	})
	if queueErr != nil {
		var pausedQueueErr *HotplugQueuePausedError
		if errors.As(queueErr, &pausedQueueErr) {
			s.recordPVCEventForVolumeHandle(ctx, req.VolumeId, eventReasonHotplugCooldown, pausedQueueErr.Error())
			return nil, status.Error(codes.Unavailable, pausedQueueErr.Error())
		}
		var queueTimeoutErr *HotplugQueueTimeoutError
		if errors.As(queueErr, &queueTimeoutErr) {
			s.recordPVCEventForVolumeHandle(ctx, req.VolumeId, eventReasonHotplugQueueTimeout, queueTimeoutErr.Error())
			return nil, status.Error(codes.DeadlineExceeded, queueTimeoutErr.Error())
		}
		var activeTimeoutErr *HotplugQueueActiveTimeoutError
		if errors.As(queueErr, &activeTimeoutErr) {
			if diagnosis, ok := s.diagnoseAndPersistHotplug(ctx, req.NodeId); ok {
				s.driver.metrics.RecordHotplugRecovery("detach", strings.ToLower(diagnosis.Classification), "queue_active_timeout")
			}
			s.recordPVCEventForVolumeHandle(ctx, req.VolumeId, eventReasonHotplugQueueTimeout, activeTimeoutErr.Error())
			return nil, status.Error(codes.DeadlineExceeded, activeTimeoutErr.Error())
		}
		var staleQueueErr *HotplugQueueStaleRequestError
		if errors.As(queueErr, &staleQueueErr) {
			s.recordPVCEventForVolumeHandle(ctx, req.VolumeId, eventReasonHotplugQueueStale, staleQueueErr.Error())
			return nil, status.Error(codes.Aborted, staleQueueErr.Error())
		}
		if status.Code(queueErr) == codes.Unknown {
			return nil, status.Error(codes.Aborted, queueErr.Error())
		}
		return nil, queueErr
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (s *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	klog.V(1).InfoS("ValidateVolumeCapabilities called", "req", protosanitizer.StripSecrets(req))
	if req.VolumeId == "" {
		klog.V(0).ErrorS(nil, "method", "ValidateVolumeCapabilities", "ValidateVolumeCapabilities called with empty volume ID")
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}

	if len(req.VolumeCapabilities) == 0 {
		klog.V(0).ErrorS(nil, "method", "ValidateVolumeCapabilities", "ValidateVolumeCapabilities called with empty volume capabilities")
		return nil, status.Error(codes.InvalidArgument, "missing volume capabilities")
	}

	if opennebula.IsSharedFilesystemVolumeID(req.VolumeId) {
		if s.sharedFilesystemProvider == nil {
			return nil, status.Error(codes.FailedPrecondition, "shared filesystem provider is not configured")
		}
		if _, err := s.sharedFilesystemProvider.ValidateSharedVolume(ctx, req.VolumeId); err != nil {
			klog.V(0).ErrorS(err, "Shared volume not found",
				"method", "ValidateVolumeCapabilities", "volumeID", req.VolumeId)
			return nil, status.Error(codes.NotFound, "volume not found")
		}
	} else {
		volumeID, _, err := s.volumeProvider.VolumeExists(ctx, req.VolumeId)
		if err != nil || volumeID == -1 {
			klog.V(0).ErrorS(err, "Volume not found",
				"method", "ValidateVolumeCapabilities", "volumeID", req.VolumeId)
			return nil, status.Error(codes.NotFound, "volume not found")
		}
	}

	if err := validateAccessMode(req.VolumeCapabilities, s.driver.getAllowedDatastoreTypes()); err != nil {
		klog.V(0).ErrorS(err, "Unsupported access mode",
			"method", "ValidateVolumeCapabilities", "volumeID", req.VolumeId, "volumeCapabilities", req.VolumeCapabilities)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	klog.V(1).InfoS("Volume capabilities validated successfully",
		"method", "ValidateVolumeCapabilities", "volumeID", req.VolumeId, "volumeCapabilities", req.VolumeCapabilities)

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

func (s *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	klog.V(1).InfoS("ListVolumes called", "req", protosanitizer.StripSecrets(req))

	maxEntries := req.GetMaxEntries()
	if maxEntries < 0 {
		klog.V(0).ErrorS(nil, "Invalid max entries",
			"method", "ListVolumes", "maxEntries", maxEntries)
		return nil, status.Error(codes.Aborted, "invalid max_entries")
	}

	startingToken := req.GetStartingToken()
	if startingToken != "" {
		startIndex, err := strconv.Atoi(startingToken)
		if err != nil || startIndex < 0 {
			klog.V(0).ErrorS(err, "Invalid starting token",
				"method", "ListVolumes", "startingToken", startingToken)
			return nil, status.Error(codes.Aborted, "invalid starting_token")
		}
	}

	volumes, err := s.volumeProvider.ListVolumes(ctx, DefaultDriverName, maxEntries, startingToken)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to list volumes",
			"method", "ListVolumes", "error", err.Error())
		return nil, status.Error(codes.Internal, "failed to list volumes")
	}

	entries := make([]*csi.ListVolumesResponse_Entry, 0, len(volumes))

	for _, volumeId := range volumes {
		volume := &csi.Volume{
			VolumeId: volumeId,
		}

		entry := &csi.ListVolumesResponse_Entry{
			Volume: volume,
		}

		entries = append(entries, entry)
	}

	klog.V(1).InfoS("Volumes listed successfully",
		"method", "ListVolumes", "volumeCount", len(entries), "startingToken", req.GetStartingToken())

	return &csi.ListVolumesResponse{
		Entries: entries,
	}, nil
}

func (s *ControllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	started := time.Now()
	klog.V(1).InfoS("GetCapacity called", "req", protosanitizer.StripSecrets(req))

	selection, err := s.driver.GetDatastoreSelectionConfig(req.GetParameters())
	if err != nil {
		s.driver.metrics.RecordOperation("get_capacity", "unknown", "invalid_argument", time.Since(started))
		klog.V(0).ErrorS(err, "Invalid datastore configuration", "method", "GetCapacity")
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	availableCapacity, err := s.volumeProvider.GetCapacity(ctx, selection)
	if err != nil {
		switch {
		case opennebula.IsDatastoreConfigError(err):
			s.driver.metrics.RecordOperation("get_capacity", "disk", "invalid_argument", time.Since(started))
			return nil, status.Error(codes.InvalidArgument, err.Error())
		case opennebula.IsDatastoreCapacityError(err):
			s.driver.metrics.RecordOperation("get_capacity", "disk", "resource_exhausted", time.Since(started))
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		s.driver.metrics.RecordOperation("get_capacity", "disk", "internal", time.Since(started))
		klog.V(0).ErrorS(err, "Failed to get available capacity", "method", "GetCapacity")
		return nil, status.Error(codes.Internal, "failed to get capacity")
	}

	s.driver.metrics.RecordOperation("get_capacity", "disk", "success", time.Since(started))
	klog.V(1).InfoS("Available capacity retrieved successfully",
		"method", "GetCapacity", "availableCapacity", availableCapacity)
	return &csi.GetCapacityResponse{
		AvailableCapacity: availableCapacity,
	}, nil
}

func (s *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	started := time.Now()
	klog.V(1).InfoS("ControllerExpandVolume called", "req", protosanitizer.StripSecrets(req))

	if req.GetVolumeId() == "" {
		s.driver.metrics.RecordOperation("controller_expand_volume", "unknown", "invalid_argument", time.Since(started))
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}

	capacityRange := req.GetCapacityRange()
	if capacityRange == nil || capacityRange.GetRequiredBytes() <= 0 {
		s.driver.metrics.RecordOperation("controller_expand_volume", "unknown", "invalid_argument", time.Since(started))
		return nil, status.Error(codes.InvalidArgument, "missing required capacity range")
	}
	requestedBytes := capacityRange.GetRequiredBytes()

	if opennebula.IsSharedFilesystemVolumeID(req.GetVolumeId()) {
		if s.sharedFilesystemProvider == nil {
			return nil, status.Error(codes.FailedPrecondition, "shared filesystem provider is not configured")
		}
		if !s.driver.featureGates.CephFSExpansion {
			s.driver.metrics.RecordOperation("controller_expand_volume", "cephfs", "unimplemented", time.Since(started))
			return nil, status.Error(codes.Unimplemented, "CephFS shared filesystem expansion is disabled by feature gate")
		}

		newSize, err := s.sharedFilesystemProvider.ExpandSharedVolume(ctx, req.GetVolumeId(), requestedBytes, req.GetSecrets())
		if err != nil {
			switch {
			case opennebula.IsDatastoreConfigError(err):
				s.driver.metrics.RecordOperation("controller_expand_volume", "cephfs", "failed_precondition", time.Since(started))
				return nil, status.Error(codes.FailedPrecondition, err.Error())
			case opennebula.IsDatastoreCapacityError(err):
				s.driver.metrics.RecordOperation("controller_expand_volume", "cephfs", "resource_exhausted", time.Since(started))
				return nil, status.Error(codes.ResourceExhausted, err.Error())
			}

			s.driver.metrics.RecordOperation("controller_expand_volume", "cephfs", "internal", time.Since(started))
			klog.V(0).ErrorS(err, "Failed to expand CephFS volume",
				"method", "ControllerExpandVolume",
				"volumeID", req.GetVolumeId(),
				"requestedBytes", requestedBytes,
				"returnedBytes", newSize,
				"nodeExpansionRequired", false)
			return nil, status.Error(codes.Internal, "failed to expand CephFS volume")
		}
		klog.V(1).InfoS("ControllerExpandVolume completed",
			"method", "ControllerExpandVolume",
			"volumeID", req.GetVolumeId(),
			"requestedBytes", requestedBytes,
			"returnedBytes", newSize,
			"nodeExpansionRequired", false)

		return &csi.ControllerExpandVolumeResponse{
			CapacityBytes:         newSize,
			NodeExpansionRequired: false,
		}, nil
	}

	release := s.driver.operationLocks.Acquire(controllerVolumeLockKey(req.GetVolumeId()))
	defer release()

	newSize, err := s.volumeProvider.ExpandVolume(ctx, req.GetVolumeId(), requestedBytes, s.driver.featureGates.DetachedDiskExpansion)
	if err != nil {
		switch {
		case opennebula.IsDatastoreConfigError(err):
			s.driver.metrics.RecordOperation("controller_expand_volume", "disk", "failed_precondition", time.Since(started))
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		case opennebula.IsDatastoreCapacityError(err):
			s.driver.metrics.RecordOperation("controller_expand_volume", "disk", "resource_exhausted", time.Since(started))
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}

		s.driver.metrics.RecordOperation("controller_expand_volume", "disk", "internal", time.Since(started))
		klog.V(0).ErrorS(err, "Failed to expand volume",
			"method", "ControllerExpandVolume",
			"volumeID", req.GetVolumeId(),
			"requestedBytes", requestedBytes,
			"returnedBytes", newSize)
		return nil, status.Error(codes.Internal, "failed to expand volume")
	}

	nodeExpansionRequired := true
	if capability := req.GetVolumeCapability(); capability != nil {
		if _, ok := capability.GetAccessType().(*csi.VolumeCapability_Block); ok {
			nodeExpansionRequired = false
		}
	}
	klog.V(1).InfoS("ControllerExpandVolume completed",
		"method", "ControllerExpandVolume",
		"volumeID", req.GetVolumeId(),
		"requestedBytes", requestedBytes,
		"returnedBytes", newSize,
		"nodeExpansionRequired", nodeExpansionRequired)

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         newSize,
		NodeExpansionRequired: nodeExpansionRequired,
	}, nil
}

func controllerNodeLockKey(nodeID string) string {
	return "node:" + nodeID
}

func controllerVolumeLockKey(volumeID string) string {
	return "volume:" + volumeID
}

type hotplugNodeHealth struct {
	KubernetesReady bool
	KubernetesKnown bool
	OpenNebulaReady bool
	OpenNebulaKnown bool
	Unschedulable   bool
	KubernetesErr   error
	OpenNebulaErr   error
}

func (s *ControllerServer) hotplugCooldownState(ctx context.Context, node string) (opennebula.HotplugCooldownState, bool) {
	if !s.nodeHotplugGuardEnabled() {
		return opennebula.HotplugCooldownState{}, false
	}
	if state, ok := s.openNebulaNodeHotplugPauseState(ctx, node); ok {
		state = s.driver.hotplugGuard.MarkPaused(state)
		s.persistHotplugStateSnapshot(ctx, state)
		s.driver.metrics.RecordHotplugGuard(strings.TrimSpace(state.Operation), state.Reason, "paused")
		return state, true
	}
	cooldown, ok := s.driver.hotplugGuard.Get(node)
	if !ok {
		return opennebula.HotplugCooldownState{}, false
	}
	health := s.nodeHotplugHealth(ctx, node, cooldown.LastObservedReady)
	if s.hotplugGuardStateCleared(cooldown, health) {
		s.driver.hotplugGuard.Clear(node)
		s.clearHotplugStateSnapshot(ctx, node)
		return opennebula.HotplugCooldownState{}, false
	}
	cooldown.KubernetesReady = health.KubernetesReady
	cooldown.OpenNebulaReady = health.OpenNebulaReady
	cooldown.Unschedulable = health.Unschedulable
	return cooldown, true
}

func (s *ControllerServer) openNebulaNodeHotplugPauseState(ctx context.Context, node string) (opennebula.HotplugCooldownState, bool) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil || s.driver.hotplugGuard == nil || strings.TrimSpace(node) == "" {
		return opennebula.HotplugCooldownState{}, false
	}
	openNebulaNode, err := s.driver.kubeRuntime.OpenNebulaNode(ctx, node)
	if err != nil || openNebulaNode == nil {
		return opennebula.HotplugCooldownState{}, false
	}
	diagnosis := openNebulaNode.Status.Hotplug.Diagnosis
	classification := strings.TrimSpace(diagnosis.Classification)
	if !strings.EqualFold(classification, opennebula.HotplugClassificationStuck) &&
		!strings.EqualFold(strings.TrimSpace(openNebulaNode.Status.DisplayState), "HotplugStuck") {
		return opennebula.HotplugCooldownState{}, false
	}
	health := s.nodeHotplugHealth(ctx, node, false)
	state := opennebula.HotplugCooldownState{
		Node:                 node,
		Operation:            strings.TrimSpace(diagnosis.Operation),
		Volume:               strings.TrimSpace(diagnosis.VolumeHandle),
		Timeout:              loadHotplugDiagnosisConfig(s.driver.PluginConfig).StuckAfter,
		LastObservedAttached: strings.EqualFold(strings.TrimSpace(diagnosis.DiskAttachFlag), "YES"),
		LastObservedReady:    false,
		FailureCount:         1,
		Reason:               "opennebula_hotplug_stuck",
		PauseUntilReady:      true,
		KubernetesReady:      health.KubernetesReady,
		OpenNebulaReady:      false,
		Unschedulable:        health.Unschedulable,
		FirstFailureAt:       time.Now().UTC(),
		LastFailureAt:        time.Now().UTC(),
	}
	if diagnosis.FirstObservedAt != nil {
		state.FirstFailureAt = diagnosis.FirstObservedAt.Time
	}
	if diagnosis.LastObservedAt != nil {
		state.LastFailureAt = diagnosis.LastObservedAt.Time
	}
	if state.Operation == "" {
		state.Operation = "hotplug"
	}
	if state.Volume == "" {
		state.Volume = "unknown"
	}
	return state, true
}

func (s *ControllerServer) hotplugGuardStateCleared(state opennebula.HotplugCooldownState, health hotplugNodeHealth) bool {
	if state.PauseUntilReady {
		return (!s.nodeHotplugGuardRequireKubernetesReady() || health.KubernetesReady) &&
			(!s.nodeHotplugGuardRequireOpenNebulaReady() || health.OpenNebulaReady) &&
			(!s.nodeHotplugGuardTreatUnschedulableAsUnhealthy() || !health.Unschedulable)
	}
	if !state.ExpiresAt.IsZero() && time.Now().UTC().After(state.ExpiresAt) {
		return true
	}
	return health.OpenNebulaReady
}

func (s *ControllerServer) nodeHotplugHealth(ctx context.Context, node string, fallbackOpenNebulaReady bool) hotplugNodeHealth {
	health := hotplugNodeHealth{
		KubernetesReady: !s.nodeHotplugGuardRequireKubernetesReady(),
		OpenNebulaReady: fallbackOpenNebulaReady || !s.nodeHotplugGuardRequireOpenNebulaReady(),
	}
	if s.nodeHotplugGuardRequireKubernetesReady() && s != nil && s.driver != nil && s.driver.kubeRuntime != nil && s.driver.kubeRuntime.enabled {
		kubeHealth, err := s.driver.kubeRuntime.NodeHealth(ctx, node)
		if err != nil {
			health.KubernetesErr = err
		} else {
			health.KubernetesReady = kubeHealth.Ready
			health.Unschedulable = kubeHealth.Unschedulable
			health.KubernetesKnown = true
		}
	}
	if s.nodeHotplugGuardRequireOpenNebulaReady() && s != nil && s.volumeProvider != nil {
		ready, err := s.volumeProvider.NodeReady(ctx, node)
		if err != nil {
			health.OpenNebulaErr = err
		} else {
			health.OpenNebulaReady = ready
			health.OpenNebulaKnown = true
		}
	}
	return health
}

func (s *ControllerServer) hotplugUnhealthyReason(health hotplugNodeHealth) string {
	reasons := make([]string, 0, 2)
	if s.nodeHotplugGuardRequireKubernetesReady() && health.KubernetesKnown && !health.KubernetesReady {
		reasons = append(reasons, "kubernetes_node_not_ready")
	}
	if s.nodeHotplugGuardRequireOpenNebulaReady() && !health.OpenNebulaReady {
		reasons = append(reasons, "opennebula_vm_not_running")
	}
	if s.nodeHotplugGuardTreatUnschedulableAsUnhealthy() && health.Unschedulable {
		reasons = append(reasons, "kubernetes_node_unschedulable")
	}
	if len(reasons) == 0 {
		return "node_unhealthy"
	}
	return strings.Join(reasons, ",")
}

func (s *ControllerServer) hotplugNodeUnhealthy(health hotplugNodeHealth) bool {
	if s.nodeHotplugGuardRequireKubernetesReady() && health.KubernetesKnown && !health.KubernetesReady {
		return true
	}
	if s.nodeHotplugGuardRequireOpenNebulaReady() && !health.OpenNebulaReady {
		return true
	}
	if s.nodeHotplugGuardTreatUnschedulableAsUnhealthy() && health.Unschedulable {
		return true
	}
	return false
}

func (s *ControllerServer) nodeHotplugGuardTreatUnschedulableAsUnhealthy() bool {
	return s != nil &&
		s.driver != nil &&
		s.driver.maintenanceMode != nil &&
		s.driver.maintenanceMode.Active()
}

func (s *ControllerServer) runHotplugGuardCleanup(ctx context.Context) {
	ticker := time.NewTicker(hotplugGuardCleanupInterval)
	defer ticker.Stop()

	s.reconcileHotplugGuardCleanup(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileHotplugGuardCleanup(ctx)
		}
	}
}

func (s *ControllerServer) reconcileHotplugGuardCleanup(ctx context.Context) {
	if s == nil || s.driver == nil || s.driver.hotplugGuard == nil || !s.nodeHotplugGuardEnabled() {
		return
	}
	if s.driver.controllerLeadership != nil && !s.driver.controllerLeadership.IsLeader() {
		return
	}
	for node, state := range s.driver.hotplugGuard.Snapshot() {
		node = strings.TrimSpace(node)
		if node == "" {
			continue
		}
		health := s.nodeHotplugHealth(ctx, node, state.LastObservedReady)
		if health.KubernetesErr != nil || health.OpenNebulaErr != nil {
			klog.V(4).InfoS("Skipping hotplug guard cleanup while node health is unavailable",
				"node", node, "kubernetesErr", health.KubernetesErr, "openNebulaErr", health.OpenNebulaErr)
			continue
		}
		if s.hotplugGuardStateCleared(state, health) {
			s.driver.hotplugGuard.Clear(node)
			s.clearHotplugStateSnapshot(ctx, node)
			klog.V(2).InfoS("Cleared stale hotplug guard state",
				"node", node, "operation", state.Operation, "volumeID", state.Volume,
				"kubernetesReady", health.KubernetesReady, "openNebulaReady", health.OpenNebulaReady,
				"unschedulable", health.Unschedulable)
			continue
		}
		state.KubernetesReady = health.KubernetesReady
		state.OpenNebulaReady = health.OpenNebulaReady
		state.Unschedulable = health.Unschedulable
		s.persistHotplugStateSnapshot(ctx, state)
	}
}

func (s *ControllerServer) formatHotplugGuardMessage(state opennebula.HotplugCooldownState) string {
	if state.PauseUntilReady {
		return fmt.Sprintf(
			"node %s hotplug operations are paused after %d unhealthy-node failure(s) during %s of volume %s; waiting for required readiness gates (kubernetesReady=%t openNebulaReady=%t unschedulable=%t lastObservedAttached=%t)",
			state.Node,
			state.FailureCount,
			state.Operation,
			state.Volume,
			state.KubernetesReady,
			state.OpenNebulaReady,
			state.Unschedulable,
			state.LastObservedAttached,
		)
	}
	return fmt.Sprintf(
		"node %s is in hotplug recovery cooldown until %s after timed out %s (last observed attached=%t ready=%t)",
		state.Node,
		state.ExpiresAt.Format(time.RFC3339),
		state.Operation,
		state.LastObservedAttached,
		state.LastObservedReady,
	)
}

func (s *ControllerServer) publishContextForVolume(ctx context.Context, volumeID, target string, deviceDiscoveryTimeout time.Duration, sourceContext map[string]string) map[string]string {
	publishContext := map[string]string{
		"volumeName": target,
	}
	for _, key := range []string{paramPVCName, paramPVCNamespace, paramPVName} {
		if value := strings.TrimSpace(sourceContext[key]); value != "" {
			publishContext[key] = value
		}
	}

	opennebulaImageID, _, imageIDErr := s.volumeProvider.VolumeExists(ctx, volumeID)
	if imageIDErr == nil && opennebulaImageID > 0 {
		publishContext[publishContextOpenNebulaImageID] = strconv.Itoa(opennebulaImageID)
		publishContext[publishContextDeviceSerial] = fmt.Sprintf("onecsi-%d", opennebulaImageID)
	}
	if s.driver != nil && s.driver.kubeRuntime != nil && s.driver.kubeRuntime.enabled {
		if runtimeCtx, runtimeErr := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, volumeID); runtimeErr == nil {
			if publishContext[paramPVCNamespace] == "" && runtimeCtx.PVCNamespace != "" {
				publishContext[paramPVCNamespace] = runtimeCtx.PVCNamespace
			}
			if publishContext[paramPVCName] == "" && runtimeCtx.PVCName != "" {
				publishContext[paramPVCName] = runtimeCtx.PVCName
			}
			if publishContext[paramPVName] == "" && runtimeCtx.PVName != "" {
				publishContext[paramPVName] = runtimeCtx.PVName
			}
			if backend := strings.TrimSpace(runtimeCtx.Backend); backend != "" {
				publishContext[annotationBackend] = backend
			}
		}
	}

	sizeBytes, err := s.volumeProvider.ResolveVolumeSizeBytes(ctx, volumeID)
	if err != nil {
		klog.V(2).InfoS("Skipping dynamic hotplug timeout in publish context after size resolution failure",
			"method", "publishContextForVolume", "volumeID", volumeID, "err", err)
		return publishContext
	}

	hotplugTimeout := s.volumeProvider.ComputeHotplugTimeout(sizeBytes)
	publishContext[publishContextHotplugTimeoutSeconds] = strconv.Itoa(int(hotplugTimeout.Seconds()))
	if deviceDiscoveryTimeout <= 0 {
		deviceDiscoveryTimeout = s.nodeDeviceDiscoveryTimeout("disk", sizeBytes, hotplugTimeout)
	}
	publishContext[publishContextDeviceDiscoveryTimeoutSeconds] = strconv.Itoa(int(deviceDiscoveryTimeout.Seconds()))
	return publishContext
}

func (s *ControllerServer) nodeDeviceDiscoveryTimeout(backend string, sizeBytes int64, hotplugTimeout time.Duration) time.Duration {
	timeoutSeconds, ok := s.driver.PluginConfig.GetInt(config.NodeDeviceDiscoveryTimeoutVar)
	if !ok || timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}

	timeout := time.Duration(timeoutSeconds) * time.Second
	if s.driver != nil && s.driver.adaptiveTimeouts != nil {
		recommendation := s.driver.adaptiveTimeouts.Recommend(opennebula.NormalizeObservationKey("device_resolution", backend, sizeBytes), timeout)
		s.driver.metrics.SetHotplugRecommendation("device_resolution", backend, recommendation.Key.SizeBucket, recommendation.Recommended, recommendation.SampleCount)
		timeout = recommendation.Recommended
	}
	if hotplugTimeout > 0 && hotplugTimeout < timeout {
		return hotplugTimeout
	}
	return timeout
}

func (s *ControllerServer) effectiveHotplugTimeout(operation, backend string, sizeBytes int64, staticFloor time.Duration) time.Duration {
	if s == nil || s.driver == nil || s.driver.adaptiveTimeouts == nil {
		return staticFloor
	}
	recommendation := s.driver.adaptiveTimeouts.Recommend(opennebula.NormalizeObservationKey(operation, backend, sizeBytes), staticFloor)
	s.driver.metrics.SetHotplugRecommendation(operation, backend, recommendation.Key.SizeBucket, recommendation.Recommended, recommendation.SampleCount)
	return recommendation.Recommended
}

func (s *ControllerServer) hotplugQueueEnabled() bool {
	if s == nil || s.driver == nil || s.driver.hotplugQueue == nil {
		return false
	}
	enabled, ok := s.driver.PluginConfig.GetBool(config.HotplugQueueEnabledVar)
	if !ok {
		return true
	}
	return enabled
}

func (s *ControllerServer) withQueuedHotplug(ctx context.Context, node, operation, volume string, priority HotplugQueuePriority, fn func(context.Context) error) error {
	if !s.hotplugQueueEnabled() {
		return fn(ctx)
	}
	return s.driver.hotplugQueue.Run(ctx, node, operation, volume, priority, fn)
}

func (s *ControllerServer) validateHotplugQueueRequest(ctx context.Context, node, operation, volume string) HotplugQueueValidation {
	if s == nil || s.volumeProvider == nil || strings.TrimSpace(node) == "" || strings.TrimSpace(volume) == "" {
		return HotplugQueueValidation{Decision: HotplugQueueValidationValid}
	}

	volumeID, _, err := s.volumeProvider.VolumeExists(ctx, volume)
	if err != nil {
		return HotplugQueueValidation{Decision: HotplugQueueValidationValid}
	}
	if volumeID == -1 {
		return HotplugQueueValidation{
			Decision: HotplugQueueValidationStale,
			Reason:   "volume_not_found",
			Message:  fmt.Sprintf("volume %s no longer exists; dropping queued %s for node %s", volume, operation, node),
		}
	}
	nodeID, err := s.volumeProvider.NodeExists(ctx, node)
	if err != nil {
		return HotplugQueueValidation{Decision: HotplugQueueValidationValid}
	}
	if nodeID == -1 {
		return HotplugQueueValidation{
			Decision: HotplugQueueValidationStale,
			Reason:   "node_not_found",
			Message:  fmt.Sprintf("node %s no longer exists; dropping queued %s for volume %s", node, operation, volume),
		}
	}
	if state, ok := s.hotplugCooldownState(ctx, node); ok {
		return HotplugQueueValidation{
			Decision: HotplugQueueValidationPaused,
			Reason:   state.Reason,
			Message:  s.formatHotplugGuardMessage(state),
		}
	}

	_, attachedErr := s.volumeProvider.GetVolumeInNode(ctx, volumeID, nodeID)
	attached := attachedErr == nil
	switch strings.ToLower(strings.TrimSpace(operation)) {
	case "attach":
		if attached {
			return HotplugQueueValidation{
				Decision: HotplugQueueValidationCompleted,
				Reason:   "already_attached",
				Message:  fmt.Sprintf("volume %s is already attached to node %s", volume, node),
			}
		}
		if stale, reason := s.attachRequestStale(ctx, node, volume); stale {
			return HotplugQueueValidation{
				Decision: HotplugQueueValidationStale,
				Reason:   reason,
				Message:  fmt.Sprintf("queued attach for volume %s on node %s is stale: %s", volume, node, reason),
			}
		}
	case "detach":
		if !attached {
			return HotplugQueueValidation{
				Decision: HotplugQueueValidationCompleted,
				Reason:   "already_detached",
				Message:  fmt.Sprintf("volume %s is already detached from node %s", volume, node),
			}
		}
	}
	return HotplugQueueValidation{Decision: HotplugQueueValidationValid}
}

func (s *ControllerServer) attachRequestStale(ctx context.Context, node, volume string) (bool, string) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled {
		return false, ""
	}
	runtimeCtx, err := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, volume)
	if err != nil {
		if strings.Contains(err.Error(), "persistent volume for handle") {
			return true, "persistent_volume_not_found"
		}
		return false, ""
	}
	pv, err := s.driver.kubeRuntime.client.CoreV1().PersistentVolumes().Get(ctx, runtimeCtx.PVName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, "persistent_volume_not_found"
		}
		return false, ""
	}
	if !pv.DeletionTimestamp.IsZero() {
		return true, "persistent_volume_deleting"
	}
	if pv.Status.Phase == corev1.VolumeReleased || pv.Status.Phase == corev1.VolumeFailed {
		return true, "persistent_volume_" + strings.ToLower(string(pv.Status.Phase))
	}

	vas, err := s.driver.kubeRuntime.client.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, ""
	}
	for _, va := range vas.Items {
		if va.Spec.Source.PersistentVolumeName == nil || *va.Spec.Source.PersistentVolumeName != runtimeCtx.PVName {
			continue
		}
		if va.Spec.NodeName == node && va.DeletionTimestamp.IsZero() {
			return false, ""
		}
	}
	return true, "volume_attachment_not_found"
}

func (s *ControllerServer) handleHotplugTimeout(ctx context.Context, node, volume string, params map[string]string, operation, backend string, err error) {
	var timeoutErr *opennebula.HotplugTimeoutError
	if !errors.As(err, &timeoutErr) {
		return
	}

	s.driver.metrics.RecordHotplugTimeout(operation, backend, "timeout_exhausted")
	s.driver.metrics.RecordHotplugRecovery(operation, "timeout_exhausted", "observed")
	if diagnosis, ok := s.diagnoseAndPersistHotplug(ctx, node); ok {
		s.driver.metrics.RecordHotplugRecovery(operation, strings.ToLower(diagnosis.Classification), "observed")
		if diagnosis.Classification == opennebula.HotplugClassificationStuck {
			s.recordPVCWarningFromParams(ctx, params, eventReasonHotplugCooldown, diagnosis.Message)
		}
	}
	if !s.nodeHotplugGuardEnabled() {
		return
	}

	health := s.nodeHotplugHealth(ctx, node, timeoutErr.LastObservedReady)
	if !s.hotplugNodeUnhealthy(health) {
		return
	}

	state, paused := s.driver.hotplugGuard.MarkUnhealthyFailure(
		node,
		operation,
		volume,
		timeoutErr.Timeout,
		timeoutErr.LastObservedAttached,
		health.OpenNebulaReady,
		health.KubernetesReady,
		health.Unschedulable,
		s.nodeHotplugGuardFailureThreshold(),
		s.hotplugUnhealthyReason(health),
	)
	s.persistHotplugStateSnapshot(ctx, state)
	outcome := "observed"
	if paused {
		outcome = "paused"
	}
	s.driver.metrics.RecordHotplugGuard(operation, state.Reason, outcome)
	s.driver.metrics.RecordHotplugRecovery(operation, state.Reason, outcome)
	message := fmt.Sprintf(
		"node %s recorded unhealthy hotplug failure %d/%d after %s timeout %s (kubernetesReady=%t openNebulaReady=%t unschedulable=%t attached=%t)",
		node,
		state.FailureCount,
		s.nodeHotplugGuardFailureThreshold(),
		operation,
		timeoutErr.Timeout,
		state.KubernetesReady,
		state.OpenNebulaReady,
		state.Unschedulable,
		timeoutErr.LastObservedAttached,
	)
	if paused {
		message = s.formatHotplugGuardMessage(state)
	}
	s.recordPVCWarningFromParams(ctx, params, eventReasonHotplugCooldown, message)
}

func (s *ControllerServer) persistHotplugStateSnapshot(ctx context.Context, state opennebula.HotplugCooldownState) {
	if s.driver == nil || s.driver.kubeRuntime == nil {
		return
	}
	payload, err := json.Marshal(state)
	if err != nil {
		klog.V(2).InfoS("Failed to marshal hotplug cooldown snapshot", "node", state.Node, "err", err)
		return
	}
	if err := s.driver.kubeRuntime.UpsertConfigMapData(ctx, namespaceFromServiceAccount(), hotplugStateConfigMapName, map[string]string{
		state.Node: string(payload),
	}); err != nil {
		klog.V(2).InfoS("Failed to persist hotplug cooldown snapshot", "node", state.Node, "err", err)
	}
}

func (s *ControllerServer) clearHotplugStateSnapshot(ctx context.Context, node string) {
	if s.driver == nil || s.driver.kubeRuntime == nil {
		return
	}
	if err := s.driver.kubeRuntime.DeleteConfigMapKey(ctx, namespaceFromServiceAccount(), hotplugStateConfigMapName, node); err != nil {
		klog.V(2).InfoS("Failed to clear hotplug cooldown snapshot", "node", node, "err", err)
	}
}

func (s *ControllerServer) clearHotplugGuardState(ctx context.Context, node string) {
	if s == nil || s.driver == nil || s.driver.hotplugGuard == nil || strings.TrimSpace(node) == "" {
		return
	}
	if state, ok := s.driver.hotplugGuard.Snapshot()[node]; ok && state.PauseUntilReady {
		return
	}
	s.driver.hotplugGuard.Clear(node)
	s.clearHotplugStateSnapshot(ctx, node)
}

// TODO: Implement methods specified in https://github.com/container-storage-interface/spec/blob/98819c45a37a67e0cd466bd02b813faf91af4e45/spec.md#controller-service-rpc
func (s *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(1).InfoS("ControllerGetCapabilities called", "req", protosanitizer.StripSecrets(req))

	capabilities := make([]*csi.ControllerServiceCapability, 0, len(controllerCapabilityTypes))
	for _, cap := range controllerCapabilityTypes {
		capabilities = append(capabilities, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: cap},
			},
		})
	}

	klog.V(1).InfoS("Controller capabilities retrieved successfully",
		"method", "ControllerGetCapabilities", "capabilitiesCount", len(capabilities))

	return &csi.ControllerGetCapabilitiesResponse{Capabilities: capabilities}, nil
}

func (s *ControllerServer) testConnectivity() {
	endpoint, _ := s.driver.PluginConfig.GetString(config.OpenNebulaRPCEndpointVar)
	credentials, ok := s.driver.PluginConfig.GetString(config.OpenNebulaCredentialsVar)
	if !ok {
		klog.V(0).ErrorS(nil, "Missing OpenNebula credentials", "method", "testConnectivity")
	}
	oneConfig := opennebula.OpenNebulaConfig{
		Endpoint:    endpoint,
		Credentials: credentials,
	}
	client := opennebula.NewClient(oneConfig)
	if err := client.Probe(context.TODO()); err != nil {
		klog.V(0).ErrorS(err, "Failed to connect to OpenNebula",
			"method", "testConnectivity", "endpoint", endpoint)
	}
	klog.V(3).InfoS("Successfully connected to OpenNebula",
		"method", "testConnectivity", "endpoint", endpoint)
}

func unsupportedRWXError(mode csi.VolumeCapability_AccessMode_Mode, profiles []opennebula.BackendCapabilityProfile) error {
	backends := unsupportedFilesystemRWXBackends(profiles)
	return fmt.Errorf(
		"access mode %q is not supported: this driver provisions VM-attached block disks and does not provide a shared-filesystem publish path; true ReadWriteMany requires a shared-filesystem backend such as NFS or CephFS. Current backends without filesystem RWX support: %s",
		mode.String(),
		strings.Join(backends, ", "),
	)
}

func backendCapabilityProfiles(backends []string) []opennebula.BackendCapabilityProfile {
	if len(backends) == 0 {
		backends = []string{"local", "ceph", "cephfs"}
	}

	profiles := make([]opennebula.BackendCapabilityProfile, 0, len(backends))
	seen := make(map[string]struct{}, len(backends))
	for _, backend := range backends {
		profile := opennebula.GetBackendCapabilityProfile(backend)
		if _, ok := seen[profile.Backend]; ok {
			continue
		}
		seen[profile.Backend] = struct{}{}
		profiles = append(profiles, profile)
	}

	return profiles
}

func unsupportedFilesystemRWXBackends(profiles []opennebula.BackendCapabilityProfile) []string {
	backends := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if !profile.SupportsFilesystemRWX {
			backends = append(backends, profile.Backend)
		}
	}
	if len(backends) == 0 {
		return []string{"configured backends"}
	}

	return backends
}

func supportedAccessModeNames() []string {
	names := make([]string, 0, len(supportedAccessModes))
	for _, mode := range supportedAccessModes {
		names = append(names, mode.String())
	}

	return names
}

func (s *ControllerServer) recordPVCEventFromParams(ctx context.Context, params map[string]string, reason, message string) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil {
		return
	}
	s.driver.kubeRuntime.EmitPVCEvent(ctx, params[paramPVCNamespace], params[paramPVCName], reason, message)
}

func (s *ControllerServer) recordPVCWarningFromParams(ctx context.Context, params map[string]string, reason, message string) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil {
		return
	}
	s.driver.kubeRuntime.EmitWarningEventOnPVC(ctx, params[paramPVCNamespace], params[paramPVCName], reason, message)
}

func (s *ControllerServer) annotatePlacementFromParams(ctx context.Context, params map[string]string, report PlacementReport) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil {
		return
	}
	s.driver.kubeRuntime.AnnotatePVAsync(ctx, params[paramPVName], report)
}

func (s *ControllerServer) runStickyDetachReaper(ctx context.Context) {
	ticker := time.NewTicker(stickyDetachReaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.driver != nil && s.driver.controllerLeadership != nil && !s.driver.controllerLeadership.IsLeader() {
				continue
			}
			for _, state := range s.driver.stickyAttachments.ListExpired(time.Now()) {
				if state.Reason == maintenanceStickyReason && s.driver.maintenanceMode != nil && s.driver.maintenanceMode.Active() {
					continue
				}
				s.expireStickyDetach(ctx, state)
			}
		}
	}
}

func (s *ControllerServer) maintenanceBlocksPublish(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (bool, string) {
	if s == nil || s.driver == nil || s.driver.maintenanceMode == nil || !s.driver.maintenanceMode.Active() {
		return false, ""
	}
	if req == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled {
		return false, ""
	}
	runtimeCtx, err := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, req.VolumeId)
	if err != nil || !runtimeContextEligibleForMaintenance(runtimeCtx, req.VolumeId) {
		return false, ""
	}
	lastNode := maintenanceLastNodeForVolume(s.driver, runtimeCtx, req.VolumeId)
	if strings.TrimSpace(lastNode) == "" || strings.TrimSpace(lastNode) == strings.TrimSpace(req.NodeId) {
		return false, ""
	}
	return true, maintenanceModeError(req.VolumeId, req.NodeId, lastNode).Error()
}

func (s *ControllerServer) startMaintenanceDetachHold(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (bool, *csi.ControllerUnpublishVolumeResponse, error) {
	if s == nil || s.driver == nil || s.driver.maintenanceMode == nil || !s.driver.maintenanceMode.Active() {
		return false, nil, nil
	}
	if req == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled || s.driver.stickyAttachments == nil {
		return false, nil, nil
	}
	runtimeCtx, err := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, req.VolumeId)
	if err != nil || !runtimeContextEligibleForMaintenance(runtimeCtx, req.VolumeId) {
		return false, nil, nil
	}
	graceSeconds := maintenanceStickyGraceSeconds(s.driver.PluginConfig)
	now := time.Now().UTC()
	state := StickyAttachmentState{
		VolumeID:           req.VolumeId,
		NodeID:             req.NodeId,
		Backend:            "local",
		DatastoreID:        runtimeCtx.DatastoreID,
		PVCNamespace:       runtimeCtx.PVCNamespace,
		PVCName:            runtimeCtx.PVCName,
		StartedAt:          now,
		ExpiresAt:          now.Add(time.Duration(graceSeconds) * time.Second),
		GraceSeconds:       graceSeconds,
		Reason:             maintenanceStickyReason,
		LastKnownNodeReady: true,
	}
	if err := s.driver.stickyAttachments.StartGrace(state); err != nil {
		s.driver.metrics.RecordMaintenanceBlock("unpublish", "persist_failed")
		s.recordPVCWarningFromRuntimeContext(ctx, runtimeCtx, eventReasonMaintenanceModeBlocked, fmt.Sprintf("failed to persist maintenance hold for volume %s on node %s: %v", req.VolumeId, req.NodeId, err))
		return true, nil, status.Errorf(codes.Internal, "failed to persist maintenance hold: %v", err)
	}
	s.driver.metrics.RecordStickyDetach("started", state.Reason)
	s.driver.metrics.RecordMaintenance("hold", "started")
	s.recordPVCEventFromRuntimeContext(ctx, runtimeCtx, eventReasonMaintenanceModePrepared, fmt.Sprintf("maintenance mode is holding local RWO volume %s on node %s without detaching", req.VolumeId, req.NodeId))
	s.annotateRestartOptimization(ctx, runtimeCtx, req.NodeId, graceSeconds)
	return true, &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (s *ControllerServer) rescueSameNodeUnpublish(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest, volumeID, nodeID int) (bool, *csi.ControllerUnpublishVolumeResponse, error) {
	if s == nil || s.driver == nil || req == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled {
		return false, nil, nil
	}
	runtimeCtx, err := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, req.VolumeId)
	if err != nil || !runtimeContextEligibleForMaintenance(runtimeCtx, req.VolumeId) {
		return false, nil, nil
	}
	desired, reason, err := s.driver.kubeRuntime.VolumeDesiredOnNode(ctx, req.VolumeId, req.NodeId)
	if err != nil || !desired {
		return false, nil, nil
	}
	target, err := s.volumeProvider.GetVolumeInNode(ctx, volumeID, nodeID)
	if err != nil {
		return false, nil, nil
	}
	if s.driver.stickyAttachments != nil {
		if state, ok := s.driver.stickyAttachments.Get(req.VolumeId); ok && strings.TrimSpace(state.NodeID) == strings.TrimSpace(req.NodeId) {
			if clearErr := s.driver.stickyAttachments.Clear(req.VolumeId); clearErr != nil {
				return true, nil, status.Errorf(codes.Internal, "failed to clear sticky attachment state after same-node detach rescue: %v", clearErr)
			}
			s.driver.metrics.RecordStickyDetach("reused", state.Reason)
			s.driver.metrics.RecordStickyDetachResidency("reused", time.Since(state.StartedAt))
		}
	}
	s.driver.metrics.RecordSameNodeRestartReuse("disk")
	s.driver.metrics.RecordStickyDetach("reused", "same_node_detach_rescue")
	s.recordPVCEventFromRuntimeContext(ctx, runtimeCtx, eventReasonSameNodeDetachRescued, fmt.Sprintf("kept local RWO volume %s attached to node %s because Kubernetes still desires it there (%s, target %s)", req.VolumeId, req.NodeId, reason, target))
	return true, &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (s *ControllerServer) shouldDelayDetach(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*VolumeRuntimeContext, StickyAttachmentState, bool, string) {
	if s == nil || s.driver == nil {
		return nil, StickyAttachmentState{}, false, "driver_unavailable"
	}
	if !s.localRestartOptimizationEnabled() {
		return nil, StickyAttachmentState{}, false, "feature_disabled"
	}
	if s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled {
		return nil, StickyAttachmentState{}, false, "kubernetes_runtime_disabled"
	}

	runtimeCtx, err := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, req.VolumeId)
	if err != nil {
		return nil, StickyAttachmentState{}, false, "volume_runtime_context_unavailable"
	}
	if runtimeCtx.RestartMode != restartOptimizationAnnotationValue {
		return runtimeCtx, StickyAttachmentState{}, false, "not_opted_in"
	}
	if !strings.EqualFold(strings.TrimSpace(runtimeCtx.Backend), "local") {
		return runtimeCtx, StickyAttachmentState{}, false, "backend_not_local"
	}
	if opennebula.IsSharedFilesystemVolumeID(req.VolumeId) {
		return runtimeCtx, StickyAttachmentState{}, false, "shared_filesystem"
	}
	if !hasSingleWriterAccessMode(runtimeCtx.AccessModes) {
		return runtimeCtx, StickyAttachmentState{}, false, "access_mode_not_rwo"
	}
	if _, ok := s.hotplugCooldownState(ctx, req.NodeId); ok {
		return runtimeCtx, StickyAttachmentState{}, false, "hotplug_cooldown"
	}
	if s.localRestartRequireNodeReady() {
		kubeReady, err := s.driver.kubeRuntime.IsNodeReady(ctx, req.NodeId)
		if err != nil || !kubeReady {
			return runtimeCtx, StickyAttachmentState{}, false, "kubernetes_node_not_ready"
		}
		vmReady, err := s.volumeProvider.NodeReady(ctx, req.NodeId)
		if err != nil || !vmReady {
			return runtimeCtx, StickyAttachmentState{}, false, "vm_not_ready"
		}
	}

	graceSeconds := s.effectiveDetachGraceSeconds(runtimeCtx)
	state := StickyAttachmentState{
		VolumeID:           req.VolumeId,
		NodeID:             req.NodeId,
		Backend:            "local",
		DatastoreID:        runtimeCtx.DatastoreID,
		PVCNamespace:       runtimeCtx.PVCNamespace,
		PVCName:            runtimeCtx.PVCName,
		StartedAt:          time.Now().UTC(),
		ExpiresAt:          time.Now().UTC().Add(time.Duration(graceSeconds) * time.Second),
		GraceSeconds:       graceSeconds,
		Reason:             "stateful_restart",
		LastKnownNodeReady: true,
	}
	return runtimeCtx, state, true, ""
}

func (s *ControllerServer) startDetachGrace(ctx context.Context, runtimeCtx *VolumeRuntimeContext, state StickyAttachmentState) error {
	if s == nil || s.driver == nil || s.driver.stickyAttachments == nil {
		return fmt.Errorf("sticky attachment manager is not configured")
	}
	if err := s.driver.stickyAttachments.StartGrace(state); err != nil {
		return err
	}
	s.driver.metrics.RecordStickyDetach("started", state.Reason)
	s.recordPVCEventFromRuntimeContext(ctx, runtimeCtx, eventReasonDetachGraceStarted, fmt.Sprintf("delaying detach for volume %s on node %s for %ds to allow same-node restart reuse", state.VolumeID, state.NodeID, state.GraceSeconds))
	s.annotateRestartOptimization(ctx, runtimeCtx, state.NodeID, state.GraceSeconds)
	return nil
}

func (s *ControllerServer) reuseStickyAttachment(ctx context.Context, req *csi.ControllerPublishVolumeRequest, volumeID, nodeID int) (bool, *csi.ControllerPublishVolumeResponse, error) {
	if s == nil || s.driver == nil || s.driver.stickyAttachments == nil {
		return false, nil, nil
	}

	state, ok := s.driver.stickyAttachments.Get(req.VolumeId)
	if !ok {
		return false, nil, nil
	}

	if strings.TrimSpace(state.NodeID) == strings.TrimSpace(req.NodeId) {
		target, err := s.volumeProvider.GetVolumeInNode(ctx, volumeID, nodeID)
		if err != nil {
			_ = s.driver.stickyAttachments.Clear(req.VolumeId)
			return false, nil, nil
		}
		if err := s.driver.stickyAttachments.Clear(req.VolumeId); err != nil {
			return true, nil, status.Errorf(codes.Internal, "failed to clear sticky attachment state: %v", err)
		}
		s.driver.metrics.RecordStickyDetach("reused", state.Reason)
		s.driver.metrics.RecordStickyDetachResidency("reused", time.Since(state.StartedAt))
		s.driver.metrics.RecordSameNodeRestartReuse(state.Backend)
		s.recordPVCEventFromStickyState(ctx, state, eventReasonDetachGraceReused, fmt.Sprintf("reused existing local attachment on node %s for volume %s", req.NodeId, req.VolumeId))
		return true, &csi.ControllerPublishVolumeResponse{
			PublishContext: s.publishContextForVolume(ctx, req.VolumeId, target, 0, req.GetVolumeContext()),
		}, nil
	}

	oldNodeRelease := s.driver.operationLocks.Acquire(controllerNodeLockKey(state.NodeID))
	defer oldNodeRelease()

	if cooldown, ok := s.hotplugCooldownState(ctx, state.NodeID); ok {
		s.driver.metrics.RecordStickyDetach("deferred", cooldown.Reason)
		message := fmt.Sprintf("refusing to move local RWO volume %s from node %s to node %s while old node hotplug is paused: %s", req.VolumeId, state.NodeID, req.NodeId, s.formatHotplugGuardMessage(cooldown))
		s.recordPVCEventFromStickyState(ctx, state, eventReasonHotplugCooldown, message)
		return true, nil, status.Error(codes.Unavailable, message)
	}

	if desired, reason, desiredErr := s.driver.kubeRuntime.VolumeDesiredOnNode(ctx, req.VolumeId, state.NodeID); desiredErr == nil && desired {
		message := fmt.Sprintf("refusing to move local RWO volume %s from node %s to node %s because Kubernetes still desires it on the old node (%s)", req.VolumeId, state.NodeID, req.NodeId, reason)
		s.driver.metrics.RecordStickyDetach("deferred", "old_node_still_desired")
		s.recordPVCEventFromStickyState(ctx, state, eventReasonDetachGraceBypassed, message)
		return true, nil, status.Error(codes.Unavailable, message)
	}

	oldNodeID, oldNodeErr := s.volumeProvider.NodeExists(ctx, state.NodeID)
	if oldNodeErr == nil && oldNodeID != -1 {
		if _, oldAttachErr := s.volumeProvider.GetVolumeInNode(ctx, volumeID, oldNodeID); oldAttachErr != nil {
			if err := s.driver.stickyAttachments.Clear(req.VolumeId); err != nil {
				return true, nil, status.Errorf(codes.Internal, "failed to clear sticky attachment state: %v", err)
			}
			s.driver.metrics.RecordStickyDetach("already_absent", state.Reason)
			s.driver.metrics.RecordStickyDetachResidency("already_absent", time.Since(state.StartedAt))
			s.recordPVCEventFromStickyState(ctx, state, eventReasonDetachGraceCancelled, fmt.Sprintf("cleared stale attachment state for volume %s because it is already absent from node %s", req.VolumeId, state.NodeID))
			return false, nil, nil
		}
	}

	if err := s.volumeProvider.DetachVolume(ctx, req.VolumeId, state.NodeID); err != nil {
		if driftErr := s.detachMetadataDriftError(ctx, req.VolumeId, state.NodeID, err); driftErr != nil {
			return true, nil, driftErr
		}
		message := fmt.Sprintf("failed to detach stale attachment for local RWO volume %s from old node %s before publishing to requested node %s: %v", req.VolumeId, state.NodeID, req.NodeId, err)
		s.recordPVCEventFromStickyState(ctx, state, eventReasonDetachGraceBypassed, message)
		return true, nil, status.Error(codes.Internal, message)
	}
	s.clearHotplugGuardState(ctx, state.NodeID)
	if s.driver.volumeQuarantine != nil {
		_ = s.driver.volumeQuarantine.Clear(ctx, req.VolumeId)
	}
	if err := s.driver.stickyAttachments.Clear(req.VolumeId); err != nil {
		return true, nil, status.Errorf(codes.Internal, "failed to clear sticky attachment state: %v", err)
	}
	s.driver.metrics.RecordStickyDetach("cancelled", state.Reason)
	s.driver.metrics.RecordStickyDetachResidency("cancelled", time.Since(state.StartedAt))
	s.recordPVCEventFromStickyState(ctx, state, eventReasonDetachGraceCancelled, fmt.Sprintf("cancelled delayed detach for volume %s and detached from node %s before publishing to node %s", req.VolumeId, state.NodeID, req.NodeId))
	return false, nil, nil
}

func (s *ControllerServer) expireStickyDetach(ctx context.Context, state StickyAttachmentState) {
	if s == nil || s.driver == nil || s.driver.stickyAttachments == nil {
		return
	}
	current, ok := s.driver.stickyAttachments.Get(state.VolumeID)
	if !ok || current.ExpiresAt.After(time.Now()) {
		return
	}
	if cooldown, ok := s.hotplugCooldownState(ctx, current.NodeID); ok {
		s.deferStickyDetach(ctx, current, cooldown.Reason, time.Minute)
		return
	}
	_ = s.withQueuedHotplug(ctx, current.NodeID, "detach", current.VolumeID, hotplugQueuePriorityBackground, func(queueCtx context.Context) error {
		nodeRelease := s.driver.operationLocks.Acquire(controllerNodeLockKey(current.NodeID))
		defer nodeRelease()

		volumeRelease := s.driver.operationLocks.Acquire(controllerVolumeLockKey(current.VolumeID))
		defer volumeRelease()

		confirmed, ok := s.driver.stickyAttachments.Get(current.VolumeID)
		if !ok || confirmed.ExpiresAt.After(time.Now()) {
			return nil
		}

		if cooldown, ok := s.hotplugCooldownState(queueCtx, confirmed.NodeID); ok {
			s.deferStickyDetach(queueCtx, confirmed, cooldown.Reason, time.Minute)
			return nil
		}

		if desired, reason, desiredErr := s.driver.kubeRuntime.VolumeDesiredOnNode(queueCtx, confirmed.VolumeID, confirmed.NodeID); desiredErr == nil && desired {
			volumeID, _, volumeErr := s.volumeProvider.VolumeExists(queueCtx, confirmed.VolumeID)
			nodeID, nodeErr := s.volumeProvider.NodeExists(queueCtx, confirmed.NodeID)
			if volumeErr == nil && nodeErr == nil && volumeID != -1 && nodeID != -1 {
				if _, attachedErr := s.volumeProvider.GetVolumeInNode(queueCtx, volumeID, nodeID); attachedErr == nil {
					if err := s.driver.stickyAttachments.Clear(confirmed.VolumeID); err != nil {
						klog.V(2).InfoS("Failed to clear sticky attachment state after same-node desired validation", "volumeID", confirmed.VolumeID, "err", err)
						return err
					}
					s.driver.metrics.RecordStickyDetach("reused", confirmed.Reason)
					s.driver.metrics.RecordStickyDetachResidency("reused", time.Since(confirmed.StartedAt))
					s.recordPVCEventFromStickyState(queueCtx, confirmed, eventReasonSameNodeDetachRescued, fmt.Sprintf("kept expired sticky volume %s attached to node %s because Kubernetes still desires it there (%s)", confirmed.VolumeID, confirmed.NodeID, reason))
					return nil
				}
			}
		}

		volumeID, _, volumeErr := s.volumeProvider.VolumeExists(queueCtx, confirmed.VolumeID)
		nodeID, nodeErr := s.volumeProvider.NodeExists(queueCtx, confirmed.NodeID)
		if volumeErr == nil && nodeErr == nil && volumeID != -1 && nodeID != -1 {
			if _, attachedErr := s.volumeProvider.GetVolumeInNode(queueCtx, volumeID, nodeID); attachedErr != nil {
				if err := s.driver.stickyAttachments.Clear(confirmed.VolumeID); err != nil {
					klog.V(2).InfoS("Failed to clear already-absent sticky attachment state", "volumeID", confirmed.VolumeID, "err", err)
					return err
				}
				if s.driver.volumeQuarantine != nil {
					_ = s.driver.volumeQuarantine.Clear(queueCtx, confirmed.VolumeID)
				}
				s.driver.metrics.RecordStickyDetach("already_absent", confirmed.Reason)
				s.driver.metrics.RecordStickyDetachResidency("already_absent", time.Since(confirmed.StartedAt))
				s.recordPVCEventFromStickyState(queueCtx, confirmed, eventReasonDetachGraceExpired, fmt.Sprintf("cleared expired sticky state for volume %s because it is already absent from node %s", confirmed.VolumeID, confirmed.NodeID))
				return nil
			}
		}

		detachStarted := time.Now()
		if err := s.volumeProvider.DetachVolume(queueCtx, confirmed.VolumeID, confirmed.NodeID); err != nil {
			if driftErr := s.detachMetadataDriftError(queueCtx, confirmed.VolumeID, confirmed.NodeID, err); driftErr != nil {
				klog.V(2).InfoS("Classified sticky detach failure as OpenNebula metadata drift", "volumeID", confirmed.VolumeID, "nodeID", confirmed.NodeID, "err", driftErr)
				return driftErr
			}
			klog.V(2).InfoS("Failed to detach sticky attachment after grace expiry", "volumeID", confirmed.VolumeID, "nodeID", confirmed.NodeID, "err", err)
			return err
		}
		s.clearHotplugGuardState(queueCtx, confirmed.NodeID)
		if s.driver.volumeQuarantine != nil {
			_ = s.driver.volumeQuarantine.Clear(queueCtx, confirmed.VolumeID)
		}
		if sizeBytes, sizeErr := s.volumeProvider.ResolveVolumeSizeBytes(queueCtx, confirmed.VolumeID); sizeErr == nil {
			s.driver.observeAdaptiveTimeout(queueCtx, "detach", "disk", sizeBytes, time.Since(detachStarted))
		}
		if err := s.driver.stickyAttachments.Clear(confirmed.VolumeID); err != nil {
			klog.V(2).InfoS("Failed to clear sticky attachment state after detach", "volumeID", confirmed.VolumeID, "err", err)
			return err
		}
		s.driver.metrics.RecordStickyDetach("expired", confirmed.Reason)
		s.driver.metrics.RecordStickyDetachResidency("expired", time.Since(confirmed.StartedAt))
		s.recordPVCEventFromStickyState(queueCtx, confirmed, eventReasonDetachGraceExpired, fmt.Sprintf("detach grace expired for volume %s on node %s; detached volume", confirmed.VolumeID, confirmed.NodeID))
		return nil
	})
}

func (s *ControllerServer) deferStickyDetach(ctx context.Context, state StickyAttachmentState, reason string, delay time.Duration) {
	if s == nil || s.driver == nil || s.driver.stickyAttachments == nil {
		return
	}
	if delay <= 0 {
		delay = time.Minute
	}
	state.ExpiresAt = time.Now().UTC().Add(delay)
	if err := s.driver.stickyAttachments.Update(state); err != nil {
		klog.V(2).InfoS("Failed to defer sticky detach", "volumeID", state.VolumeID, "nodeID", state.NodeID, "reason", reason, "err", err)
		return
	}
	s.driver.metrics.RecordStickyDetach("deferred", reason)
	s.recordPVCEventFromStickyState(ctx, state, eventReasonHotplugCooldown, fmt.Sprintf("deferred expired sticky detach for volume %s on node %s for %s: %s", state.VolumeID, state.NodeID, delay, reason))
}

func (s *ControllerServer) clearStickyReuseState(ctx context.Context, volumeID, nodeID, backend string) {
	if s == nil || s.driver == nil || s.driver.stickyAttachments == nil {
		return
	}
	state, ok := s.driver.stickyAttachments.Get(volumeID)
	if !ok || strings.TrimSpace(state.NodeID) != strings.TrimSpace(nodeID) {
		return
	}
	if err := s.driver.stickyAttachments.Clear(volumeID); err != nil {
		klog.V(2).InfoS("Failed to clear sticky attachment state after same-node fast-path validation", "volumeID", volumeID, "nodeID", nodeID, "err", err)
		return
	}
	s.driver.metrics.RecordStickyDetach("reused", state.Reason)
	s.driver.metrics.RecordStickyDetachResidency("reused", time.Since(state.StartedAt))
	s.driver.metrics.RecordSameNodeRestartReuse(backend)
	s.recordPVCEventFromStickyState(ctx, state, eventReasonDetachGraceReused, fmt.Sprintf("reused existing local attachment on node %s for volume %s", nodeID, volumeID))
}

func (s *ControllerServer) annotateRestartOptimization(ctx context.Context, runtimeCtx *VolumeRuntimeContext, nodeID string, graceSeconds int) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil || runtimeCtx == nil {
		return
	}
	s.driver.kubeRuntime.AnnotatePVAsync(ctx, runtimeCtx.PVName, PlacementReport{
		Backend:             runtimeCtx.Backend,
		DatastoreID:         runtimeCtx.DatastoreID,
		DatastoreName:       runtimeCtx.PVAnnotations[annotationDatastoreName],
		SelectionPolicy:     runtimeCtx.PVAnnotations[annotationSelectionPolicy],
		LastAttachedNode:    nodeID,
		RestartOptimization: runtimeCtx.RestartMode,
		DetachGraceSeconds:  graceSeconds,
	})
}

func (s *ControllerServer) annotateRestartOptimizationForVolume(ctx context.Context, volumeID, nodeID string) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled {
		return
	}
	runtimeCtx, err := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, volumeID)
	if err != nil {
		return
	}
	s.annotateRestartOptimization(ctx, runtimeCtx, nodeID, s.effectiveDetachGraceSeconds(runtimeCtx))
}

func (s *ControllerServer) recordPVCEventFromRuntimeContext(ctx context.Context, runtimeCtx *VolumeRuntimeContext, reason, message string) {
	if runtimeCtx == nil {
		return
	}
	s.recordPVCEventFromParams(ctx, map[string]string{
		paramPVCNamespace: runtimeCtx.PVCNamespace,
		paramPVCName:      runtimeCtx.PVCName,
	}, reason, message)
}

func (s *ControllerServer) recordPVCWarningFromRuntimeContext(ctx context.Context, runtimeCtx *VolumeRuntimeContext, reason, message string) {
	if runtimeCtx == nil {
		return
	}
	s.recordPVCWarningFromParams(ctx, map[string]string{
		paramPVCNamespace: runtimeCtx.PVCNamespace,
		paramPVCName:      runtimeCtx.PVCName,
	}, reason, message)
}

func (s *ControllerServer) recordPVCEventFromStickyState(ctx context.Context, state StickyAttachmentState, reason, message string) {
	s.recordPVCEventFromParams(ctx, map[string]string{
		paramPVCNamespace: state.PVCNamespace,
		paramPVCName:      state.PVCName,
	}, reason, message)
}

func (s *ControllerServer) recordPVCEventForVolumeHandle(ctx context.Context, volumeHandle, reason, message string) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled {
		return
	}
	runtimeCtx, err := s.driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, volumeHandle)
	if err != nil {
		return
	}
	s.recordPVCEventFromRuntimeContext(ctx, runtimeCtx, reason, message)
}

func (s *ControllerServer) localRestartOptimizationEnabled() bool {
	if s == nil || s.driver == nil {
		return false
	}
	enabled, ok := s.driver.PluginConfig.GetBool(config.LocalRestartOptimizationEnabledVar)
	return !ok || enabled
}

func (s *ControllerServer) localRestartRequireNodeReady() bool {
	if s == nil || s.driver == nil {
		return true
	}
	enabled, ok := s.driver.PluginConfig.GetBool(config.LocalRestartRequireNodeReadyVar)
	return !ok || enabled
}

func (s *ControllerServer) nodeHotplugGuardEnabled() bool {
	if s == nil || s.driver == nil {
		return true
	}
	enabled, ok := s.driver.PluginConfig.GetBool(config.NodeHotplugGuardEnabledVar)
	return !ok || enabled
}

func (s *ControllerServer) nodeHotplugGuardFailureThreshold() int {
	if s == nil || s.driver == nil {
		return 2
	}
	threshold, ok := s.driver.PluginConfig.GetInt(config.NodeHotplugGuardFailureThresholdVar)
	if !ok || threshold <= 0 {
		return 2
	}
	return threshold
}

func (s *ControllerServer) nodeHotplugGuardRequireKubernetesReady() bool {
	if s == nil || s.driver == nil {
		return true
	}
	enabled, ok := s.driver.PluginConfig.GetBool(config.NodeHotplugGuardRequireKubernetesReadyVar)
	return !ok || enabled
}

func (s *ControllerServer) nodeHotplugGuardRequireOpenNebulaReady() bool {
	if s == nil || s.driver == nil {
		return true
	}
	enabled, ok := s.driver.PluginConfig.GetBool(config.NodeHotplugGuardRequireOpenNebulaReadyVar)
	return !ok || enabled
}

func (s *ControllerServer) effectiveDetachGraceSeconds(runtimeCtx *VolumeRuntimeContext) int {
	defaultGrace, ok := s.driver.PluginConfig.GetInt(config.LocalRestartDetachGraceSecondsVar)
	if !ok || defaultGrace <= 0 {
		defaultGrace = 90
	}
	maxGrace, ok := s.driver.PluginConfig.GetInt(config.LocalRestartDetachGraceMaxSecondsVar)
	if !ok || maxGrace <= 0 {
		maxGrace = 300
	}
	grace := defaultGrace
	if runtimeCtx != nil && runtimeCtx.DetachGraceHint > 0 {
		grace = runtimeCtx.DetachGraceHint
	}
	if grace < 30 {
		grace = 30
	}
	if grace > maxGrace {
		grace = maxGrace
	}
	return grace
}

func hasSingleWriterAccessMode(modes []corev1.PersistentVolumeAccessMode) bool {
	if len(modes) == 0 {
		return false
	}
	for _, mode := range modes {
		switch mode {
		case corev1.ReadWriteMany, corev1.ReadOnlyMany:
			return false
		}
	}
	for _, mode := range modes {
		if mode == corev1.ReadWriteOnce || mode == corev1.ReadWriteOncePod {
			return true
		}
	}
	return false
}
