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
	klog.V(4).Infof("CreateVolume called with request: %v", protosanitizer.StripSecrets(req))
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "missing volume name")
	}

	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "missing volume capabilities")
	}

	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	if requiredBytes == 0 {
		requiredBytes = DefaultVolumeSizeBytes
	}

	volumeID, volumeSize, _ := s.volumeProvider.VolumeExists(ctx, req.Name)

	if volumeID != -1 {
		if int64(volumeSize) == requiredBytes {
			return &csi.CreateVolumeResponse{
				Volume: &csi.Volume{
					VolumeId:      req.Name,
					CapacityBytes: requiredBytes,
				},
			}, nil
		} else {
			return nil, status.Error(codes.AlreadyExists,
				"volume with the same name already exists with different size")
		}

	}

	err := s.volumeProvider.CreateVolume(ctx, req.Name, requiredBytes, DefaultDriverName)
	if err != nil {
		klog.Errorf("Failed to create volume: %v", err)
		return nil, status.Error(codes.Internal, "failed to create volume")
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      req.Name,
			CapacityBytes: requiredBytes,
		},
	}, nil
}

func (s *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.V(4).Infof("DeleteVolume called with request: %v", protosanitizer.StripSecrets(req))
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}

	err := s.volumeProvider.DeleteVolume(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "failed to delete volume")
	}

	return &csi.DeleteVolumeResponse{}, nil
}

// TODO: Process VolumeCapability, readonly
func (s *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	klog.V(4).Infof("ControllerPublishVolume called with request: %v", protosanitizer.StripSecrets(req))
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}

	if req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "missing node ID")
	}

	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "missing volume capability")
	}

	volumeID, _, err := s.volumeProvider.VolumeExists(ctx, req.VolumeId)
	if err != nil || volumeID == -1 {
		return nil, status.Error(codes.NotFound, "volume not found")
	}

	nodeID, err := s.volumeProvider.NodeExists(ctx, req.NodeId)
	if err != nil || nodeID == -1 {
		return nil, status.Error(codes.NotFound, "node not found")
	}

	target, err := s.volumeProvider.GetVolumeInNode(ctx, volumeID, nodeID)
	if err == nil {
		return &csi.ControllerPublishVolumeResponse{
			PublishContext: map[string]string{
				"volumeName": target,
			},
		}, nil
	}

	// TODO: Validate VolumeCapability
	err = s.volumeProvider.AttachVolume(ctx, req.VolumeId, req.NodeId)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to attach volume")
	}

	target, err = s.volumeProvider.GetVolumeInNode(ctx, volumeID, nodeID)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to get volume in node")
	}

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{
			"volumeName": target,
		},
	}, nil
}

func (s *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	klog.V(4).Infof("ControllerUnpublishVolume called with request: %v", protosanitizer.StripSecrets(req))
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}
	if req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "missing node ID")
	}

	err := s.volumeProvider.DetachVolume(ctx, req.VolumeId, req.NodeId)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to attach volume")
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (s *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	klog.V(4).Infof("ValidateVolumeCapabilities called with request: %v", protosanitizer.StripSecrets(req))
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "missing volume ID")
	}

	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "missing volume capabilities")
	}

	volumeID, _, err := s.volumeProvider.VolumeExists(ctx, req.VolumeId)
	if err != nil || volumeID == -1 {
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

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

func (s *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	klog.V(4).Infof("ListVolumes called with request: %v", protosanitizer.StripSecrets(req))
	volumes, err := s.volumeProvider.ListVolumes(ctx, DefaultDriverName)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to list volumes")
	}

	token := req.GetStartingToken()
	if token != "" {
		startIndex, err := strconv.Atoi(token)
		if err != nil || startIndex < 0 || startIndex >= len(volumes) {
			return nil, status.Error(codes.Aborted, "invalid starting_token")
		}
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

	return &csi.ListVolumesResponse{
		Entries: entries,
	}, nil
}

func (s *ControllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	klog.V(4).Infof("GetCapacity called with request: %v", protosanitizer.StripSecrets(req))
	availableCapacity, err := s.volumeProvider.GetCapacity(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to get capacity")
	}
	return &csi.GetCapacityResponse{
		AvailableCapacity: availableCapacity,
	}, nil
}

// TODO: Implement methods specified in https://github.com/container-storage-interface/spec/blob/98819c45a37a67e0cd466bd02b813faf91af4e45/spec.md#controller-service-rpc
func (s *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(4).Infof("ControllerGetCapabilities called with request: %v", protosanitizer.StripSecrets(req))

	capabilities := make([]*csi.ControllerServiceCapability, 0, len(controllerCapabilityTypes))
	for _, cap := range controllerCapabilityTypes {
		capabilities = append(capabilities, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: cap},
			},
		})
	}

	return &csi.ControllerGetCapabilitiesResponse{Capabilities: capabilities}, nil
}

func (s *ControllerServer) testConnectivity() {
	endpoint, _ := s.driver.PluginConfig.GetString(config.OpenNebulaRPCEndpointVar)
	credentials, ok := s.driver.PluginConfig.GetString(config.OpenNebulaCredentialsVar)
	if !ok {
		klog.Fatalf("Missing OpenNebula credentials")
	}
	oneConfig := opennebula.OpenNebulaConfig{
		Endpoint:    endpoint,
		Credentials: credentials,
	}
	client := opennebula.NewClient(oneConfig)
	if err := client.Probe(context.TODO()); err != nil {
		klog.Fatalf("Failed to connect to OpenNebula: %v", err)
	}
	klog.Infof("Connected to OpenNebula at %s", endpoint)
}
