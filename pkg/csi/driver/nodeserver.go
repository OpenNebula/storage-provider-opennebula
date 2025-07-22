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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
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

	klog.V(1).InfoS("NodeStageVolume called", "req", protosanitizer.StripSecrets(req).String())

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
		klog.V(3).Info("Block access type detected, skipping formatting and mounting")
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

	// Check if device path for volumeID exists
	if _, err := os.Stat(devicePath); os.IsNotExist(err) {
		klog.V(0).ErrorS(err, "Device path does not exist",
			"method", "NodeStageVolume", "volumeID", volumeID, "devicePath", devicePath)
		return nil, status.Error(codes.NotFound, "device path does not exist")
	}

	mountCheck, err := ns.checkMountPoint(devicePath, stagingTargetPath, volumeCapability)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to check mount point",
			"method", "NodeStageVolume", "devicePath", devicePath, "stagingTargetPath", stagingTargetPath)
		return nil, status.Error(codes.Internal, "failed to check mount point")
	}

	//If the volume capability are not supported by the volume
	// return 9 FAILED_PRECONDITION error
	if !mountCheck.volumeCapabilitySupported {
		klog.V(0).ErrorS(err, "Volume capability is not supported by the volume",
			"method", "NodeStageVolume", "volumeName", volName, "stagingTargetPath", stagingTargetPath,
			"volumeCapability", protosanitizer.StripSecrets(volumeCapability))
		return nil, status.Error(codes.FailedPrecondition, "volume capability is not supported by the volume")
	}

	if mountCheck.targetIsMountPoint && mountCheck.deviceIsMounted {
		if !mountCheck.fsTypeMatches {
			klog.V(0).InfoS(
				"Warning! Already existing filesystem does not match the expected type",
				"method", "NodeStageVolume", "volumeID", volumeID, "devicePath", devicePath,
				"stagingTargetPath", stagingTargetPath, "fsType", fsType)
		}

		//Check if the volume with volumeID is already staged at stagingTargetPath
		// but is incompatible with the volumeCapability provided in the request,
		// then return 6 ALREADY_EXISTS error
		if !mountCheck.compatibleWithVolumeCapability {
			klog.V(0).ErrorS(nil, "Volume capability is not compatible with the volume",
				"method", "NodeStageVolume", "devicePath", devicePath, "stagingTargetPath", stagingTargetPath,
				"fsType", fsType, "mountFlags", mountFlags)
			return nil, status.Error(codes.AlreadyExists, "volume capability is not compatible with the volume")
		}

		//Check if volume_id is already staged in stagingTargetPath and is identical
		// to the volumeCapability provided in the request, then return 0 OK response
		return &csi.NodeStageVolumeResponse{}, nil
	}

	klog.V(3).InfoS("Formatting and mounting volume",
		"method", "NodeStageVolume", "volumeID", volumeID, "devicePath", devicePath,
		"stagingTargetPath", stagingTargetPath, "fsType", fsType)

	if _, err := os.Stat(stagingTargetPath); os.IsNotExist(err) {
		if err := os.MkdirAll(stagingTargetPath, 0775); err != nil {
			klog.V(0).ErrorS(err, "Failed to create staging target path",
				"method", "NodeStageVolume", "stagingTargetPath", stagingTargetPath)
			return nil, status.Errorf(codes.Internal,
				"failed to create staging target path %s: %v", stagingTargetPath, err)
		}
	}

	err = ns.mounter.FormatAndMount(devicePath, stagingTargetPath, fsType, mountFlags)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to format and mount volume",
			"method", "NodeStageVolume", "devicePath", devicePath,
			"stagingTargetPath", stagingTargetPath, "fsType", fsType)
		return nil, status.Errorf(codes.Internal, "failed to format and mount volume: %s", err)
	}

	klog.V(1).InfoS("Volume staged successfully",
		"method", "NodeStageVolume", "volumeID", volumeID, "devicePath", devicePath,
		"stagingTargetPath", stagingTargetPath, "fsType", fsType)

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.V(1).InfoS("NodeUnstageVolume called", "req", protosanitizer.StripSecrets(req).String())

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	klog.V(3).InfoS("Cleaning staging target path volume mount point",
		"method", "NodeUnstageVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath)

	err := mount.CleanupMountPoint(stagingTargetPath, ns.mounter, true)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to clean mount point of staging target path",
			"method", "NodeUnstageVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath)
		return nil, status.Error(codes.Internal, "failed to cleanup mount point of staging target path")
	}

	klog.V(1).InfoS("Volume unstaged successfully",
		"method", "NodeUnstageVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath)

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *NodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {

	klog.V(1).InfoS("NodePublishVolume called", "req", protosanitizer.StripSecrets(req).String())

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

	klog.V(3).InfoS("Publishing volume",
		"method", "NodePublishVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath,
		"targetPath", targetPath)

	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		//TODO: Review permissions
		if err := os.MkdirAll(targetPath, 0750); err != nil {
			klog.V(0).ErrorS(err, "Failed to create target path",
				"method", "NodePublishVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath,
				"targetPath", targetPath)
			return nil, status.Errorf(codes.Internal,
				"failed to create target path %s: %v", targetPath, err)
		}
	}

	options := []string{"bind"}
	if req.Readonly {
		options = append(options, "ro")
	}

	var err error
	var resp *csi.NodePublishVolumeResponse
	accessType := volumeCapability.GetAccessType()
	switch accessType.(type) {
	case *csi.VolumeCapability_Block:
		resp, err = ns.handleBlockVolumePublish(stagingTargetPath, targetPath, volumeCapability, options)
	case *csi.VolumeCapability_Mount:
		resp, err = ns.handleMountVolumePublish(stagingTargetPath, targetPath, volumeCapability, options)
	default:
		klog.V(0).ErrorS(nil, "Unsupported access type for volume capability",
			"method", "NodePublishVolume", "volumeID", volumeID, "accessType", accessType)
		return nil, status.Error(codes.InvalidArgument, "unsupported access type for volume capability")
	}

	if err != nil {
		klog.V(0).ErrorS(err, "Failed to publish volume",
			"method", "NodePublishVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath,
			"targetPath", targetPath, "accessType", reflect.TypeOf(accessType).String())
		return nil, status.Error(codes.Internal, "failed to publish volume")
	}

	klog.V(1).InfoS("Volume published successfully",
		"method", "NodePublishVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath, "targetPath", targetPath)

	return resp, nil
}

func (ns NodeServer) handleBlockVolumePublish(stagingPath, targetPath string, volumeCapability *csi.VolumeCapability, options []string) (*csi.NodePublishVolumeResponse, error) {

	checkMountPoint, err := ns.checkMountPoint(stagingPath, targetPath, volumeCapability)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to check mount point",
			"method", "handleBlockVolumePublish", "stagingPath", stagingPath, "targetPath", targetPath)
		return nil, fmt.Errorf("failed to check mount point: %w", err)
	}

	//volume is already mounted at targetPath
	if checkMountPoint.targetIsMountPoint && checkMountPoint.deviceIsMounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	klog.V(3).InfoS("Mounting block volume at target path",
		"method", "handleBlockVolumePublish", "stagingPath", stagingPath, "targetPath", targetPath)

	// mount the device at the target path
	fsType := "" // Block volumes do not require a filesystem type
	err = ns.mounter.Mount(stagingPath, targetPath, fsType, options)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to mount device at target path",
			"method", "handleBlockVolumePublish", "stagingPath", stagingPath, "targetPath", targetPath)
		return nil, fmt.Errorf("failed to mount device at target path: %w", err)
	}

	klog.V(3).InfoS("Block volume successfully mounted at target path",
		"method", "handleBlockVolumePublish", "stagingPath", stagingPath, "targetPath", targetPath)

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns NodeServer) handleMountVolumePublish(stagingPath, targetPath string, volumeCapability *csi.VolumeCapability, options []string) (*csi.NodePublishVolumeResponse, error) {

	checkMountPoint, err := ns.checkMountPoint(stagingPath, targetPath, volumeCapability)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to check mount point",
			"method", "handleMountVolumePublish", "stagingPath", stagingPath, "targetPath", targetPath)
		return nil, fmt.Errorf("failed to check mount point: %w", err)
	}

	//volume is already mounted at targetPath
	if checkMountPoint.targetIsMountPoint {
		klog.V(3).InfoS("Volume is already mounted at target path",
			"method", "handleMountVolumePublish", "stagingPath", stagingPath, "targetPath", targetPath)
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

	klog.V(3).InfoS("Mounting file system volume at target path",
		"method", "handleMountVolumePublish", "nodeId", ns.Driver.nodeID, "stagingPath", stagingPath,
		"targetPath", targetPath, "fsType", fsType, "mountFlags", fmt.Sprintf("%v", options))

	err = ns.mounter.Mount(stagingPath, targetPath, fsType, options)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to mount file system volume at target path",
			"method", "handleMountVolumePublish", "nodeId", ns.Driver.nodeID,
			"stagingPath", stagingPath, "targetPath", targetPath, "fsType", fsType,
			"options", fmt.Sprintf("%v", options))
		return nil, status.Error(codes.Internal, "failed to mount file system volume at target path")
	}

	klog.V(3).InfoS("File system volume successfully mounted at target path",
		"method", "handleMountVolumePublish", "nodeId", ns.Driver.nodeID, "stagingPath", stagingPath,
		"targetPath", targetPath, "fsType", fsType, "mountFlags", fmt.Sprintf("%v", options))

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(1).InfoS("NodeUnpublishVolume called", "req", protosanitizer.StripSecrets(req).String())

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	//TODO: Check if the volume with volumeID exists

	klog.V(3).InfoS("Unpublishing volume",
		"volumeID", volumeID, "targetPath", targetPath)

	err := mount.CleanupMountPoint(targetPath, ns.mounter, true)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to unmount volume at target path",
			"method", "NodeUnpublishVolume", "volumeID", volumeID, "targetPath", targetPath)
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to unmount volume at target path %s: %v", targetPath, err))
	}

	klog.V(1).InfoS("Volume successfully unpublished from target path",
		"method", "NodeUnpublishVolume", "volumeID", volumeID, "targetPath", targetPath)

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
	klog.V(1).InfoS("NodeGetCapabilities called", "req", protosanitizer.StripSecrets(req).String())

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

	return &csi.NodeGetCapabilitiesResponse{Capabilities: capabilities}, nil
}

func (ns *NodeServer) NodeGetInfo(_ context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	klog.V(1).InfoS("NodeGetInfo called", "req", protosanitizer.StripSecrets(req).String())

	nodeId := ns.Driver.nodeID
	maxVolumesPerNode := ns.Driver.maxVolumesPerNode

	klog.V(3).InfoS("Returning node info",
		"nodeId", nodeId, "maxVolumesPerNode", maxVolumesPerNode)

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
			klog.V(5).InfoS("Found mount point matching target path",
				"method", "checkMountPoint", "srcPath", srcPath, "targetPath", targetPath,
				"foundMountPoint", fmt.Sprintf("%+v", mp))
			break
		}
	}

	if !reflect.ValueOf(foundMountPoint).IsZero() {
		mountPointMatch.targetIsMountPoint = true
		if foundMountPoint.Device == srcPath {
			mountPointMatch.deviceIsMounted = true
		}
	}

	klog.V(5).InfoS("Mount point check results",
		"srcPath", srcPath,
		"targetPath", targetPath,
		"targetIsMountPoint", mountPointMatch.targetIsMountPoint,
		"deviceIsMounted", mountPointMatch.deviceIsMounted)

	//if volumeCapability is nil, skip volume capability checks
	if volumeCapability == nil {
		return mountPointMatch, nil
	}

	//check volume capability compatibility
	switch accessType := volumeCapability.GetAccessType().(type) {
	case *csi.VolumeCapability_Mount:
		// Check if the filesystem type matches
		fsType := accessType.Mount.GetFsType()
		klog.V(5).InfoS("Checking filesystem type for mount point compatibility",
			"fsType", fsType, "foundMountPointType", foundMountPoint.Type)
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

	klog.V(5).InfoS("Mount point check results",
		"srcPath", srcPath,
		"targetPath", targetPath,
		"targetIsMountPoint", mountPointMatch.targetIsMountPoint,
		"deviceIsMounted", mountPointMatch.deviceIsMounted,
		"fsTypeMatches", mountPointMatch.fsTypeMatches,
		"volumeCapabilitySupported", mountPointMatch.volumeCapabilitySupported,
		"compatibleWithVolumeCapability", mountPointMatch.compatibleWithVolumeCapability,
	)

	return mountPointMatch, nil
}
