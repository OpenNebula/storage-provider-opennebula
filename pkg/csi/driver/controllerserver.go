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
	"strconv"

	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/config"
	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

var controllerCapabilityTypes = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
	csi.ControllerServiceCapability_RPC_GET_CAPACITY,
}

type ControllerServer struct {
	driver         *Driver
	volumeProvider opennebula.OpenNebulaVolumeProvider
	csi.UnimplementedControllerServer
}

func NewControllerServer(d *Driver, vp opennebula.OpenNebulaVolumeProvider) *ControllerServer {
	return &ControllerServer{
		driver:         d,
		volumeProvider: vp,
	}
}

func (s *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	klog.V(1).InfoS("CreateVolume called", "req", protosanitizer.StripSecrets(req))

	if req.Name == "" {
		klog.V(0).ErrorS(nil, "method", "CreateVolume", "CreateVolume called with empty volume name")
		return nil, status.Error(codes.InvalidArgument, "missing volume name")
	}

	if len(req.VolumeCapabilities) == 0 {
		klog.V(0).ErrorS(nil, "method", "CreateVolume", "CreateVolume called with empty volume capabilities")
		return nil, status.Error(codes.InvalidArgument, "missing volume capabilities")
	}

	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	if requiredBytes == 0 {
		requiredBytes = DefaultVolumeSizeBytes
	}

	volumeID, volumeSize, _ := s.volumeProvider.VolumeExists(ctx, req.Name)

	if volumeID != -1 {
		if int64(volumeSize) == requiredBytes {
			klog.V(3).InfoS("Volume already exists with the same size",
				"method", "CreateVolume", "volumeID", volumeID, "requiredSize", requiredBytes)
			return &csi.CreateVolumeResponse{
				Volume: &csi.Volume{
					VolumeId:      req.Name,
					CapacityBytes: requiredBytes,
				},
			}, nil
		} else {
			klog.V(0).ErrorS(nil, "Volume with the same name already exists with different size",
				"method", "CreateVolume", "volumeID", volumeID, "existingSize", volumeSize, "requiredSize", requiredBytes)
			return nil, status.Error(codes.AlreadyExists,
				"volume with the same name already exists with different size")
		}
	}

	klog.V(3).InfoS("Creating new volume", "volumeName", req.Name, "requiredBytes", requiredBytes)

	err := s.volumeProvider.CreateVolume(ctx, req.Name, requiredBytes, DefaultDriverName)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to create volume",
			"method", "CreateVolume", "volumeName", req.Name, "requiredBytes", requiredBytes, "defaultDriverName", DefaultDriverName)
		return nil, status.Error(codes.Internal, "failed to create volume")
	}

	klog.V(1).InfoS("Volume created successfully",
		"method", "CreateVolume", "volumeName", req.Name, "requiredBytes", requiredBytes)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      req.Name,
			CapacityBytes: requiredBytes,
		},
	}, nil
}

func (s *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.V(1).InfoS("DeleteVolume called", "req", protosanitizer.StripSecrets(req))

	if req.VolumeId == "" {
		klog.V(0).ErrorS(nil, "method", "DeleteVolume", "DeleteVolume called with empty volume ID")
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}

	klog.V(3).InfoS("Deleting volume", "method", "DeleteVolume", "volumeID", req.VolumeId)

	err := s.volumeProvider.DeleteVolume(ctx, req.VolumeId)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to delete volume", "method", "DeleteVolume", "volumeID", req.VolumeId)
		return nil, status.Error(codes.FailedPrecondition, "failed to delete volume")
	}

	klog.V(1).InfoS("Volume deleted successfully", "method", "DeleteVolume", "volumeID", req.VolumeId)
	return &csi.DeleteVolumeResponse{}, nil
}

// TODO: Process VolumeCapability, readonly
func (s *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	klog.V(1).InfoS("ControllerPublishVolume called", "req", protosanitizer.StripSecrets(req))
	if req.VolumeId == "" {
		klog.V(0).ErrorS(nil, "method", "ControllerPublishVolume", "ControllerPublishVolume called with empty volume ID")
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}

	if req.NodeId == "" {
		klog.V(0).ErrorS(nil, "method", "ControllerPublishVolume", "ControllerPublishVolume called with empty node ID")
		return nil, status.Error(codes.InvalidArgument, "missing node ID")
	}

	if req.VolumeCapability == nil {
		klog.V(0).ErrorS(nil, "method", "ControllerPublishVolume", "ControllerPublishVolume called with empty volume capability")
		return nil, status.Error(codes.InvalidArgument, "missing volume capability")
	}

	volumeID, _, err := s.volumeProvider.VolumeExists(ctx, req.VolumeId)
	if err != nil || volumeID == -1 {
		klog.V(0).ErrorS(err, "Volume does not exist", "method", "ControllerPublishVolume", "volumeID", req.VolumeId)
		return nil, status.Error(codes.NotFound, "volume not found")
	}

	nodeID, err := s.volumeProvider.NodeExists(ctx, req.NodeId)
	if err != nil || nodeID == -1 {
		klog.V(0).ErrorS(err, "Node does not exist", "method", "ControllerPublishVolume", "nodeID", req.NodeId)
		return nil, status.Error(codes.NotFound, "node not found")
	}

	target, err := s.volumeProvider.GetVolumeInNode(ctx, volumeID, nodeID)
	if err == nil {
		klog.V(1).InfoS("Volume already attached to node",
			"method", "ControllerPublishVolume", "volumeID", volumeID, "nodeID", nodeID, "volumeName", target)
		return &csi.ControllerPublishVolumeResponse{
			PublishContext: map[string]string{
				"volumeName": target,
			},
		}, nil
	}

	// TODO: Validate VolumeCapability

	klog.V(3).InfoS("Attaching volume to node",
		"method", "ControllerPublishVolume", "volumeID", volumeID, "nodeID", nodeID)
	err = s.volumeProvider.AttachVolume(ctx, req.VolumeId, req.NodeId)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to attach volume",
			"method", "ControllerPublishVolume", "volumeID", req.VolumeId, "nodeID", req.NodeId)
		return nil, status.Error(codes.Internal, "failed to attach volume")
	}

	klog.V(3).InfoS("Checking if volume is attached",
		"method", "ControllerPublishVolume", "volumeID", volumeID, "nodeID", nodeID)
	target, err = s.volumeProvider.GetVolumeInNode(ctx, volumeID, nodeID)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to get volume in node",
			"method", "ControllerPublishVolume", "volumeID", volumeID, "nodeID", nodeID)
		return nil, status.Error(codes.Internal, "failed to get volume in node")
	}

	klog.V(1).InfoS("Volume attached successfully",
		"method", "ControllerPublishVolume", "volumeID", volumeID, "nodeID", nodeID, "volumeName", target)

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{
			"volumeName": target,
		},
	}, nil
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

	volumeID, _, err := s.volumeProvider.VolumeExists(ctx, req.VolumeId)
	if err != nil || volumeID == -1 {
		klog.V(1).InfoS("Volume not found, skipping volume unpublish",
			"method", "ControllerUnpublishVolume", "volumeID", req.VolumeId)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	nodeID, err := s.volumeProvider.NodeExists(ctx, req.NodeId)
	if err != nil || nodeID == -1 {
		klog.V(0).ErrorS(err, "Node not found",
			"method", "ControllerUnpublishVolume", "nodeID", req.NodeId)
		return nil, status.Error(codes.NotFound, "node not found")
	}

	_, err = s.volumeProvider.GetVolumeInNode(ctx, volumeID, nodeID)
	if err != nil {
		klog.V(1).InfoS("Volume does not exist in node, skipping unpublish",
			"method", "ControllerUnpublishVolume", "volumeID", req.VolumeId, "nodeID", req.NodeId)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	klog.V(3).InfoS("Detaching volume from node",
		"method", "ControllerUnpublishVolume", "volumeID", volumeID, "nodeID", nodeID)

	err = s.volumeProvider.DetachVolume(ctx, req.VolumeId, req.NodeId)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to detach volume",
			"method", "ControllerUnpublishVolume", "volumeID", req.VolumeId, "nodeID", req.NodeId)
		return nil, status.Error(codes.Internal, "failed to detach volume")
	}

	klog.V(1).InfoS("Volume detached successfully",
		"method", "ControllerUnpublishVolume", "volumeID", volumeID, "nodeID", nodeID)
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

	volumeID, _, err := s.volumeProvider.VolumeExists(ctx, req.VolumeId)
	if err != nil || volumeID == -1 {
		klog.V(0).ErrorS(err, "Volume not found",
			"method", "ValidateVolumeCapabilities", "volumeID", req.VolumeId)
		return nil, status.Error(codes.NotFound, "volume not found")
	}
	supportedModes := map[csi.VolumeCapability_AccessMode_Mode]bool{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER: true,
	}

	for _, cap := range req.VolumeCapabilities {
		if cap.AccessMode == nil || !supportedModes[cap.AccessMode.Mode] {
			return &csi.ValidateVolumeCapabilitiesResponse{}, nil
		}
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
	klog.V(1).InfoS("GetCapacity called", "req", protosanitizer.StripSecrets(req))

	availableCapacity, err := s.volumeProvider.GetCapacity(ctx)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to get available capacity", "method", "GetCapacity")
		return nil, status.Error(codes.Internal, "failed to get capacity")
	}

	klog.V(1).InfoS("Available capacity retrieved successfully",
		"method", "GetCapacity", "availableCapacity", availableCapacity)
	return &csi.GetCapacityResponse{
		AvailableCapacity: availableCapacity,
	}, nil
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
