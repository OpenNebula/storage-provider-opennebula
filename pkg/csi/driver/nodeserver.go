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
	"fmt"
	"os"
	"path"
	"reflect"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"
)

const (
	defaultFSType   = "ext4"  // Default filesystem type for volumes
	defaultDiskPath = "/dev/" // Path to disk devices (probably we should include in volumecontext)
)

type mountPointMatch struct {
	targetIsMountPoint bool // The target path is a mount point
	deviceIsMounted    bool // The device is mounted at the target path
	fsTypeMatches      bool // The filesystem type matches the expected type
	//mountFlagsMatch    bool // The mount flags match the expected flags
	volumeCapabilitySupported      bool // The volume capability is supported by the volume
	compatibleWithVolumeCapability bool // The mount point is compatible with the volume capability
}

type NodeServer struct {
	Driver  *Driver
	mounter *mount.SafeFormatAndMount
	csi.UnimplementedNodeServer
}

func NewNodeServer(d *Driver, mounter *mount.SafeFormatAndMount) *NodeServer {
	return &NodeServer{
		Driver:  d,
		mounter: mounter,
	}
}

//Following functions are RPC implementations defined in
// - https://github.com/container-storage-interface/spec/blob/master/spec.md#rpc-interface
// - https://github.com/container-storage-interface/spec/blob/master/spec.md#node-service-rpc

// The NodeStageVolume method behaves differently depending on the access type of the volume.
// For block access type, it skips mounting and formatting, while for mount access type, it
// performs the necessary operations to prepare the volume for use, like formatting and mounting
// the volume at the staging target path.
func (ns *NodeServer) NodeStageVolume(_ context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {

	log.Debug().
		Str("args", protosanitizer.StripSecrets(req).String()).
		Msg("NodeStageVolume called")

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	// volume capability defines the access type (block or mount) and access mode of the volume
	volumeCapability := req.GetVolumeCapability()
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	accessType := volumeCapability.GetAccessType()
	if _, ok := accessType.(*csi.VolumeCapability_Block); ok {
		// Block access type -> skip mounting and formatting
		log.Info().Msg("Block access type detected, skipping formatting and mounting")
		return &csi.NodeStageVolumeResponse{}, nil
	}

	volumeContext := req.GetPublishContext()
	volName := volumeContext["volumeName"]
	if len(volName) == 0 {
		return nil, status.Error(codes.InvalidArgument, "[volumeName] entry is required in volume context")
	}
	devicePath := ns.getDeviceName(volName)

	volMount := volumeCapability.GetMount()
	mountFlags := volMount.GetMountFlags()
	fsType := volMount.GetFsType()
	if fsType == "" {
		fsType = defaultFSType
	}

	// Check if volume with volumeID exists
	if _, err := os.Stat(devicePath); os.IsNotExist(err) {
		log.Error().
			Str("volumeID", volumeID).
			Str("devicePath", devicePath).
			Msg("Device path does not exist, cannot format and mount volume")
		return nil, status.Error(codes.NotFound, "device path does not exist")
	}

	mountCheck, err := ns.checkMountPoint(devicePath, stagingTargetPath, volumeCapability)
	if err != nil {
		log.Error().
			Err(err).
			Str("volumeName", volName).
			Str("stagingTargetPath", stagingTargetPath).
			Msg("Failed to check mount point")
		return nil, status.Error(codes.Internal, "failed to check mount point")
	}

	//If the volume capability are not supported by the volume
	// return 9 FAILED_PRECONDITION error
	if !mountCheck.volumeCapabilitySupported {
		log.Error().
			Str("volumeName", volName).
			Str("stagingTargetPath", stagingTargetPath).
			Msg("Volume capability is not supported by the volume")
		return nil, status.Error(codes.FailedPrecondition, "volume capability is not supported by the volume")
	}

	if mountCheck.targetIsMountPoint && mountCheck.deviceIsMounted {
		//Check if volume_id is already staged in stagingTargetPath and is identical
		// to the volumeCapability provided in the request, then return 0 OK response
		if mountCheck.fsTypeMatches {
			return &csi.NodeStageVolumeResponse{}, nil
		}
		//TODO: Correct this

		//Check if the volume with volumeID is already staged at stagingTargetPath
		// but is incompatible with the volumeCapability provided in the request,
		// then return 6 ALREADY_EXISTS error
		if !mountCheck.compatibleWithVolumeCapability {
			log.Error().
				Str("volumeName", volName).
				Str("stagingTargetPath", stagingTargetPath).
				Msg("Volume capability is not compatible with the volume")
			return nil, status.Error(codes.AlreadyExists, "volume capability is not compatible with the volume")
		}
	}

	log.Info().
		Str("volumeID", volumeID).
		Str("devicePath", devicePath).
		Str("stagingTargetPath", stagingTargetPath).
		Str("fsType", fsType).
		//Strs("mountFlags", mountFlags). //TODO: Warning, could contain sensitive data
		Msg("Formatting and mounting volume")

	if _, err := os.Stat(stagingTargetPath); os.IsNotExist(err) {
		if err := os.MkdirAll(stagingTargetPath, 0775); err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to create staging target path %s: %v", stagingTargetPath, err)
		}
	}

	err = ns.mounter.FormatAndMount(devicePath, stagingTargetPath, fsType, mountFlags)
	if err != nil {
		log.Error().
			Err(err).
			Str("source", devicePath).
			Str("stagingTargetPath", stagingTargetPath).
			Str("fsType", fsType).
			//Strs("mountFlags", mountFlags). //TODO: Warning, could contain sensitive data
			Msg("Failed to format and mount volume")
		return nil, status.Errorf(codes.Internal, "failed to format and mount volume: %s", err)
	}

	log.Info().Msg("Volume staged successfully")

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	log.Debug().
		Str("args", protosanitizer.StripSecrets(req).String()).
		Msg("NodeUnstageVolume called")

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	log.Info().
		Str("volumeID", volumeID).
		Str("stagingTargetPath", stagingTargetPath).
		Msg("Cleaning staging target path volume mount point")

	err := mount.CleanupMountPoint(stagingTargetPath, ns.mounter, true)
	if err != nil {
		log.Error().
			Err(err).
			Str("volumeID", volumeID).
			Str("stagingTargetPath", stagingTargetPath).
			Msg("Failed to clean mount point of staging target path")
		return nil, status.Error(codes.Internal, "failed to clean mount point of staging target path")
	}

	log.Info().
		Str("volumeID", volumeID).
		Str("stagingTargetPath", stagingTargetPath).
		Msg("Volume unmounted successfully")

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *NodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {

	log.Debug().
		Str("args", protosanitizer.StripSecrets(req).String()).
		Msg("NodePublishVolume called")

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	// volume capability defines the access type (block or mount) and access mode of the volume
	volumeCapability := req.GetVolumeCapability()
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	//TODO: Check if the volume with volumeID exists
	// If not, return 5 NOT_FOUND error

	log.Info().
		Str("volumeID", volumeID).
		Str("stagingTargetPath", stagingTargetPath).
		Str("targetPath", targetPath).
		Msg("Publishing volume")

	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		if err := os.MkdirAll(targetPath, 0750); err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to create target path %s: %v", targetPath, err)
		}
	}

	options := []string{"bind"}
	if req.Readonly {
		options = append(options, "ro")
	}

	accessType := volumeCapability.GetAccessType()
	switch accessType.(type) {
	case *csi.VolumeCapability_Block:
		return ns.handleBlockVolumePublish(stagingTargetPath, targetPath, volumeCapability, options)
	case *csi.VolumeCapability_Mount:
		return ns.handleMountVolumePublish(stagingTargetPath, targetPath, volumeCapability, options)
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported access type for volume capability")
	}
}

func (ns NodeServer) handleBlockVolumePublish(stagingPath, targetPath string, volumeCapability *csi.VolumeCapability, options []string) (*csi.NodePublishVolumeResponse, error) {

	checkMountPoint, err := ns.checkMountPoint(stagingPath, targetPath, volumeCapability)
	if err != nil {
		log.Error().
			Err(err).
			Str("stagingPath", stagingPath).
			Str("targetPath", targetPath).
			Msg("Failed to check mount point")
		return nil, status.Error(codes.Internal, "failed to check mount point")
	}

	//volume is already mounted at targetPath
	if checkMountPoint.targetIsMountPoint && checkMountPoint.deviceIsMounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	log.Info().
		Str("stagingPath", stagingPath).
		Str("targetPath", targetPath).
		Msg("Mounting block volume at target path")

	// TODO: Create the target path if it does not exist?

	// mount the device at the target path
	fsType := "" // Block volumes do not require a filesystem type
	err = ns.mounter.Mount(stagingPath, targetPath, fsType, options)
	if err != nil {
		log.Error().
			Err(err).
			Str("stagingPath", stagingPath).
			Str("targetPath", targetPath).
			Msg("Failed to mount device at target path")
		return nil, status.Error(codes.Internal, "failed to mount device at target path")
	}

	log.Info().
		Str("stagingPath", stagingPath).
		Str("targetPath", targetPath).
		Msg("Block volume successfully mounted at target path")

	//TODO: check if volume is incompatible with the volume capability

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns NodeServer) handleMountVolumePublish(stagingPath, targetPath string, volumeCapability *csi.VolumeCapability, options []string) (*csi.NodePublishVolumeResponse, error) {

	checkMountPoint, err := ns.checkMountPoint(stagingPath, targetPath, volumeCapability)
	if err != nil {
		log.Error().
			Err(err).
			Str("stagingPath", stagingPath).
			Str("targetPath", targetPath).
			Msg("Failed to check mount point")
		return nil, status.Error(codes.Internal, "failed to check mount point")
	}

	//volume is already mounted at targetPath
	if checkMountPoint.targetIsMountPoint {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	mount := volumeCapability.GetMount()
	for _, flag := range mount.GetMountFlags() {
		options = append(options, flag)
	}

	fsType := mount.GetFsType()
	if fsType == "" {
		fsType = defaultFSType
	}

	log.Info().
		Str("nodeId", ns.Driver.nodeID).
		Str("stagingPath", stagingPath).
		Str("targetPath", targetPath).
		Str("fsType", fsType).
		Str("mountFlags", fmt.Sprintf("%v", options)).
		Msg("Mounting file system volume at target path")

	err = ns.mounter.Mount(stagingPath, targetPath, fsType, options)
	if err != nil {
		log.Error().
			Err(err).
			Str("nodeId", ns.Driver.nodeID).
			Str("stagingPath", stagingPath).
			Str("targetPath", targetPath).
			Str("fsType", fsType).
			Str("options", fmt.Sprintf("%v", options)).
			//Strs("mountFlags", mountFlags). //TODO: Warning, could contain sensitive data
			Msg("Failed to mount file system volume at target path")
		return nil, status.Error(codes.Internal, "failed to mount file system volume at target path")
	}

	log.Info().
		Str("nodeId", ns.Driver.nodeID).
		Str("stagingPath", stagingPath).
		Str("targetPath", targetPath).
		Msg("File system volume successfully mounted at target path")

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	//TODO: Check if the volume with volumeID exists

	log.Info().
		Str("volumeID", volumeID).
		Str("targetPath", targetPath).
		Msg("Unpublishing volume")

	err := mount.CleanupMountPoint(targetPath, ns.mounter, true)
	if err != nil {
		log.Error().
			Err(err).
			Str("volumeID", volumeID).
			Str("targetPath", targetPath).
			Msg("Failed to unmount volume at target path")
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to unmount volume at target path %s: %v", targetPath, err))
	}

	log.Info().
		Str("volumeID", volumeID).
		Str("targetPath", targetPath).
		Msg("Volume successfully unpublished from target path")

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	//TODO: Implement
	//A Node plugin MUST implement this RPC call if it has GET_VOLUME_STATS node capability or VOLUME_CONDITION node capability
	return &csi.NodeGetVolumeStatsResponse{}, status.Error(codes.Unimplemented, "NodeGetVolumeStats is not implemented")
}

func (ns *NodeServer) NodeExpandVolume(_ context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	//A Node Plugin MUST implement this RPC call if it has EXPAND_VOLUME node capability.
	//TODO: Implement
	return &csi.NodeExpandVolumeResponse{}, status.Error(codes.Unimplemented, "NodeExpandVolume is not implemented")
}

func (ns *NodeServer) NodeGetCapabilities(_ context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	capabilities := []*csi.NodeServiceCapability{
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
				},
			},
		},
		//TODO: implement and add NodeServiceCapability_RPC_EXPAND_VOLUME capability
		//TODO: implement and add NodeServiceCapability_RPC_GET_VOLUME_STATS capability
	}

	log.Info().
		Msg("NodeGetCapabilities called")

	return &csi.NodeGetCapabilitiesResponse{Capabilities: capabilities}, nil
}

func (ns *NodeServer) NodeGetInfo(_ context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	nodeId := ns.Driver.nodeID
	maxVolumesPerNode := ns.Driver.maxVolumesPerNode
	log.Info().
		Str("nodeId", nodeId).
		Int("maxVolumesPerNode", int(maxVolumesPerNode)).
		Msg("NodeGetInfo called")

	return &csi.NodeGetInfoResponse{
		NodeId:            nodeId,
		MaxVolumesPerNode: maxVolumesPerNode,
	}, nil
}

func (ns *NodeServer) getDeviceName(volumeName string) string {
	return path.Join(defaultDiskPath, volumeName)
}

func (ns *NodeServer) checkMountPoint(srcPath string, targetPath string, volumeCapability *csi.VolumeCapability) (mountPointMatch, error) {

	mountPointMatch := mountPointMatch{
		targetIsMountPoint:             false,
		deviceIsMounted:                false,
		fsTypeMatches:                  false,
		volumeCapabilitySupported:      false,
		compatibleWithVolumeCapability: false,
	}

	mountPoints, err := ns.mounter.List()
	if err != nil {
		return mountPointMatch, fmt.Errorf("failed to list mount points: %w", err)
	}

	foundMountPoint := mount.MountPoint{}
	for _, mp := range mountPoints {
		if mp.Path == targetPath {
			foundMountPoint = mp
			log.Info().
				Str("srcPath", srcPath).
				Str("targetPath", targetPath).
				Str("foundMountPoint", fmt.Sprintf("%+v", mp)).
				Msg("Found mount point matching target path")
			break
		}
	}

	if !reflect.ValueOf(foundMountPoint).IsZero() {
		mountPointMatch.targetIsMountPoint = true
		if foundMountPoint.Device == srcPath {
			mountPointMatch.deviceIsMounted = true
		}
	}

	log.Info().
		Str("srcPath", srcPath).
		Str("targetPath", targetPath).
		Bool("targetIsMountPoint", mountPointMatch.targetIsMountPoint).
		Bool("deviceIsMounted", mountPointMatch.deviceIsMounted).
		Msg("Mount point check results")

	//if volumeCapability is nil, skip volume capability checks
	if volumeCapability == nil {
		return mountPointMatch, nil
	}

	//check volume capability compatibility
	switch accessType := volumeCapability.GetAccessType().(type) {
	case *csi.VolumeCapability_Mount:
		// Check if the filesystem type matches
		fsType := accessType.Mount.GetFsType()
		log.Info().
			Str("fsType", fsType).
			Str("foundMountPointType", foundMountPoint.Type).
			Msg("Checking filesystem type for mount point compatibility")
		if len(fsType) == 0 || foundMountPoint.Type == fsType {
			mountPointMatch.fsTypeMatches = true
		}
		// TODO: Check if the mount flags match
		mountPointMatch.volumeCapabilitySupported = true
		//TODO: Check
		mountPointMatch.compatibleWithVolumeCapability = true
		break
	case *csi.VolumeCapability_Block:
		// For block access type, we don't check fsType or mountFlags
		mountPointMatch.fsTypeMatches = true
		mountPointMatch.volumeCapabilitySupported = true
		//TODO: Check
		mountPointMatch.compatibleWithVolumeCapability = true
		break
	default:
		mountPointMatch.volumeCapabilitySupported = false
		mountPointMatch.compatibleWithVolumeCapability = false
	}

	log.Info().
		Str("srcPath", srcPath).
		Str("targetPath", targetPath).
		Bool("targetIsMountPoint", mountPointMatch.targetIsMountPoint).
		Bool("deviceIsMounted", mountPointMatch.deviceIsMounted).
		Bool("fsTypeMatches", mountPointMatch.fsTypeMatches).
		Bool("volumeCapabilitySupported", mountPointMatch.volumeCapabilitySupported).
		Bool("compatibleWithVolumeCapability", mountPointMatch.compatibleWithVolumeCapability).
		Msg("Mount point check results")

	return mountPointMatch, nil
}
