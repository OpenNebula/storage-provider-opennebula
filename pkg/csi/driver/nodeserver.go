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
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

var (
	nodeVolumePathStat = os.Stat
	nodeVolumePathFS   = unix.Statfs
	nodeDeviceSleep    = time.Sleep
	nodeNow            = time.Now
	nodeRuntimeGOOS    = runtime.GOOS
	nodeResizeFS       = func(exec utilexec.Interface, devicePath, deviceMountPath string) (bool, error) {
		return mount.NewResizeFs(exec).Resize(devicePath, deviceMountPath)
	}
)

const (
	defaultFSType               = "ext4" // Default filesystem type for volumes
	defaultNodeDevicePollPeriod = time.Second
	defaultNodeExpandTimeout    = 120 * time.Second
	defaultNodeExpandRetry      = 2 * time.Second
	defaultNodeExpandTolerance  = int64(128 * 1024 * 1024)
	minNodeExpandTimeout        = 10 * time.Second
	minNodeExpandRetry          = time.Second
)

var defaultDiskPath = "/dev/" // Path to disk devices (probably we should include in volumecontext)

type mountPointMatch struct {
	targetIsMountPoint bool // The target path is a mount point
	deviceIsMounted    bool // The device is mounted at the target path
	fsTypeMatches      bool // The filesystem type matches the expected type
	//mountFlagsMatch    bool // The mount flags match the expected flags
	volumeCapabilitySupported      bool // The volume capability is supported by the volume
	compatibleWithVolumeCapability bool // The mount point is compatible with the volume capability
}

type NodeServer struct {
	Driver                   *Driver
	mounter                  *mount.SafeFormatAndMount
	deviceResolver           *NodeDeviceResolver
	sharedFilesystemRecovery *sharedFilesystemRecoveryManager
	localDiskSessions        *localDiskSessionStore
	csi.UnimplementedNodeServer
}

func NewNodeServer(d *Driver, mounter *mount.SafeFormatAndMount) *NodeServer {
	if d.operationLocks == nil {
		d.operationLocks = NewOperationLocks()
	}
	ns := &NodeServer{
		Driver:         d,
		mounter:        mounter,
		deviceResolver: NewNodeDeviceResolver(d.PluginConfig, mounter.Exec, nil),
	}
	ns.deviceResolver.deviceCandidatesFn = ns.deviceCandidates
	ns.sharedFilesystemRecovery = newSharedFilesystemRecoveryManager(ns)
	ns.localDiskSessions = newLocalDiskSessionStore(localDiskSessionRootPath)
	return ns
}

func (ns *NodeServer) StartBackgroundWorkers(ctx context.Context) {
	if ns == nil || ns.sharedFilesystemRecovery == nil {
		return
	}
	ns.sharedFilesystemRecovery.Start(ctx)
}

//Following functions are RPC implementations defined in
// - https://github.com/container-storage-interface/spec/blob/master/spec.md#rpc-interface
// - https://github.com/container-storage-interface/spec/blob/master/spec.md#node-service-rpc

// The NodeStageVolume method behaves differently depending on the access type of the volume.
// For block access type, it skips mounting and formatting, while for mount access type, it
// performs the necessary operations to prepare the volume for use, like formatting and mounting
// the volume at the staging target path.
func (ns *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	started := time.Now()

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

	if isSharedFilesystemRequest(volumeID, req.GetPublishContext()) {
		return ns.handleSharedFilesystemStage(req)
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
	deviceTimeout := ns.deviceDiscoveryTimeout(volumeContext)
	devicePath, resolution, err := ns.resolveDevicePathWithContext(volumeID, volName, volumeContext, deviceTimeout)
	if err != nil {
		ns.Driver.metrics.RecordNodeDeviceResolutionDuration("disk", "timeout", time.Since(started))
		ns.recordLocalDeviceMissing(ctx, volumeID, volName, stagingTargetPath, volumeContext, err)
		ns.recordPVCWarningFromPublishContext(ctx, volumeContext, eventReasonDeviceDiscoveryTimeout, err.Error())
		klog.V(0).ErrorS(err, "Failed to resolve device path",
			"method", "NodeStageVolume", "volumeID", volumeID, "volumeName", volName, "deviceDiscoveryTimeout", deviceTimeout)
		return nil, status.Error(codes.DeadlineExceeded, err.Error())
	}
	ns.recordDeviceResolutionFromPublishContext(ctx, volumeContext, resolution)
	ns.Driver.metrics.RecordNodeDeviceResolutionDuration("disk", "success", resolution.Latency)
	ns.Driver.observeAdaptiveTimeout(ctx, "device_resolution", ns.publishContextBackend(volumeContext), 0, resolution.Latency)
	if resolution.Latency > 10*time.Second {
		ns.recordPVCWarningFromPublishContext(ctx, volumeContext, eventReasonDeviceDiscoverySlow, fmt.Sprintf("device discovery for volume %s took %s", volumeID, resolution.Latency))
	}
	klog.V(2).InfoS("Resolved staged device path",
		"method", "NodeStageVolume", "volumeID", volumeID, "devicePath", devicePath, "deviceDiscoveryTimeout", deviceTimeout,
		"resolvedBy", resolution.ResolvedBy, "resolutionLatency", resolution.Latency)

	volMount := volumeCapability.GetMount()
	mountFlags := volMount.GetMountFlags()
	fsType := volMount.GetFsType()
	if fsType == "" {
		fsType = defaultFSType
	}

	accessMode := volumeCapability.GetAccessMode()
	if accessMode == nil || accessMode.Mode == csi.VolumeCapability_AccessMode_UNKNOWN {
		return nil, status.Error(codes.InvalidArgument, "access mode is required")
	}

	if accessMode.Mode == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		mountFlags = append(mountFlags, "ro")
	}

	mountCheck, err := ns.checkMountPoint(devicePath, stagingTargetPath, volumeCapability)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to check mount point",
			"method", "NodeStageVolume", "devicePath", devicePath, "stagingTargetPath", stagingTargetPath)
		return nil, status.Error(codes.Internal, "failed to check mount point")
	}
	if mountCheck.targetIsMountPoint {
		health, healthErr := ns.evaluateLocalDiskPath(volumeID, stagingTargetPath)
		if healthErr != nil {
			klog.V(0).ErrorS(healthErr, "Failed to evaluate local disk stage mount health",
				"method", "NodeStageVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath)
			return nil, status.Error(codes.Internal, "failed to evaluate local disk stage mount health")
		}
		if health.Stale || !mountCheck.deviceIsMounted {
			message := health.Message
			if message == "" {
				message = fmt.Sprintf("stage is mounted from unexpected source %q", health.MountSource)
			}
			ns.recordPVCWarningFromPublishContext(context.Background(), volumeContext, eventReasonLocalVolumeStaleMount, message)
			ns.recordLocalVolumeHealth("stage_stale_mount_detected", "observed")
			if !ns.localRWOStaleMountRecoveryEnabled() {
				return nil, status.Errorf(codes.FailedPrecondition, "stale local disk stage detected at %s: %s", stagingTargetPath, message)
			}
			if err := mount.CleanupMountPoint(stagingTargetPath, ns.mounter.Interface, true); err != nil {
				ns.recordLocalVolumeHealth("stage_cleanup", "failed")
				return nil, status.Errorf(codes.FailedPrecondition, "failed to cleanup stale local disk stage %s: %v", stagingTargetPath, err)
			}
			ns.recordLocalVolumeHealth("stage_cleanup", "succeeded")
			mountCheck.targetIsMountPoint = false
			mountCheck.deviceIsMounted = false
		}
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
		ns.recordLocalDiskStageSession(req, devicePath, fsType, mountFlags)
		ns.clearLocalDeviceMissing(ctx, volumeID)
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
		if ns.deviceResolver != nil {
			ns.deviceResolver.Invalidate(volumeID)
		}
		ns.recordLocalDeviceMountFailure(ctx, volumeID, volName, devicePath, stagingTargetPath, fsType, resolution, volumeContext, err)
		klog.V(0).ErrorS(err, "Failed to format and mount volume",
			"method", "NodeStageVolume", "devicePath", devicePath,
			"stagingTargetPath", stagingTargetPath, "fsType", fsType)
		return nil, status.Errorf(codes.Internal, "failed to format and mount volume: %s", err)
	}

	ns.Driver.metrics.RecordNodeStageDuration("disk", "success", time.Since(started))
	klog.V(1).InfoS("Volume staged successfully",
		"method", "NodeStageVolume", "volumeID", volumeID, "devicePath", devicePath,
		"stagingTargetPath", stagingTargetPath, "fsType", fsType)

	ns.recordLocalDiskStageSession(req, devicePath, fsType, mountFlags)
	ns.clearLocalDeviceMissing(ctx, volumeID)
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

	if opennebula.IsSharedFilesystemVolumeID(volumeID) {
		return ns.handleSharedFilesystemUnstage(req)
	}

	klog.V(3).InfoS("Cleaning staging target path volume mount point",
		"method", "NodeUnstageVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath)

	if ns.deviceResolver != nil {
		ns.deviceResolver.Invalidate(volumeID)
	}
	ns.deleteLocalDiskSession(volumeID)

	err := mount.CleanupMountPoint(stagingTargetPath, ns.mounter.Interface, true)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to clean mount point of staging target path",
			"method", "NodeUnstageVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath)
		return nil, status.Error(codes.Internal, "failed to cleanup mount point of staging target path")
	}

	klog.V(1).InfoS("Volume unstaged successfully",
		"method", "NodeUnstageVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath)

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {

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

	if isSharedFilesystemRequest(volumeID, req.GetPublishContext()) {
		return ns.handleSharedFilesystemPublish(req)
	}

	volumeContext := req.GetPublishContext()
	volName := volumeContext["volumeName"]
	if len(volName) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volueName is required in volume context")
	}

	//TODO: Check if the volume with volumeID exists
	// If not, return 5 NOT_FOUND error

	klog.V(3).InfoS("Publishing volume",
		"method", "NodePublishVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath,
		"targetPath", targetPath)

	targetDir := targetPath
	accessType := volumeCapability.GetAccessType()
	if _, ok := accessType.(*csi.VolumeCapability_Block); ok {
		targetDir = filepath.Dir(targetPath)
	}

	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		//TODO: Review permissions
		if err := os.MkdirAll(targetDir, 0750); err != nil {
			klog.V(0).ErrorS(err, "Failed to create target path",
				"method", "NodePublishVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath,
				"targetPath", targetDir)
			return nil, status.Errorf(codes.Internal,
				"failed to create target path %s: %v", targetDir, err)
		}
	}

	options := []string{"bind"}
	if req.Readonly {
		options = append(options, "ro")
	}

	var err error
	var resp *csi.NodePublishVolumeResponse
	switch accessType.(type) {
	case *csi.VolumeCapability_Block:
		deviceTimeout := ns.deviceDiscoveryTimeout(volumeContext)
		devicePath, resolution, resolveErr := ns.resolveDevicePathWithContext(volumeID, volName, volumeContext, deviceTimeout)
		if resolveErr != nil {
			ns.Driver.metrics.RecordNodeDeviceResolutionDuration("disk", "timeout", deviceTimeout)
			ns.recordLocalDeviceMissing(ctx, volumeID, volName, stagingTargetPath, volumeContext, resolveErr)
			ns.recordPVCWarningFromPublishContext(ctx, volumeContext, eventReasonDeviceDiscoveryTimeout, resolveErr.Error())
			klog.V(0).ErrorS(resolveErr, "Failed to resolve device path for block volume publish",
				"method", "NodePublishVolume", "volumeID", volumeID, "volumeName", volName, "deviceDiscoveryTimeout", deviceTimeout)
			return nil, status.Error(codes.DeadlineExceeded, resolveErr.Error())
		}
		ns.clearLocalDeviceMissing(ctx, volumeID)
		ns.recordDeviceResolutionFromPublishContext(ctx, volumeContext, resolution)
		ns.Driver.metrics.RecordNodeDeviceResolutionDuration("disk", "success", resolution.Latency)
		ns.Driver.observeAdaptiveTimeout(ctx, "device_resolution", ns.publishContextBackend(volumeContext), 0, resolution.Latency)
		klog.V(2).InfoS("Resolved block device path",
			"method", "NodePublishVolume", "volumeID", volumeID, "devicePath", devicePath, "deviceDiscoveryTimeout", deviceTimeout,
			"resolvedBy", resolution.ResolvedBy, "resolutionLatency", resolution.Latency)
		resp, err = ns.handleBlockVolumePublish(devicePath, targetPath, volumeCapability, options)
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
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, status.Error(codes.Internal, "failed to publish volume")
	}

	klog.V(1).InfoS("Volume published successfully",
		"method", "NodePublishVolume", "volumeID", volumeID, "stagingTargetPath", stagingTargetPath, "targetPath", targetPath)

	if _, ok := accessType.(*csi.VolumeCapability_Mount); ok {
		ns.updateLocalDiskPublishedTarget(volumeID, targetPath, options)
	}
	return resp, nil
}

func (ns NodeServer) handleBlockVolumePublish(devicePath, targetPath string, volumeCapability *csi.VolumeCapability, options []string) (*csi.NodePublishVolumeResponse, error) {

	checkMountPoint, err := ns.checkMountPoint(devicePath, targetPath, volumeCapability)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to check mount point",
			"method", "handleBlockVolumePublish", "stagingPath", devicePath, "targetPath", targetPath)
		return nil, fmt.Errorf("failed to check mount point: %w", err)
	}

	//volume is already mounted at targetPath
	if checkMountPoint.targetIsMountPoint && checkMountPoint.deviceIsMounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	klog.V(0).InfoS("Mounting block volume at target path",
		"method", "handleBlockVolumePublish", "stagingPath", devicePath, "targetPath", targetPath)

	// Create an empty file at the target path
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		targetFile, err := os.Create(targetPath)
		if err != nil {
			klog.V(0).ErrorS(err, "Failed to create target path file",
				"method", "handleBlockVolumePublish", "stagingPath", devicePath, "targetPath", targetPath)
			return nil, fmt.Errorf("failed to create target path file: %w", err)
		}
		defer targetFile.Close()
	}

	// mount the device at the target path
	fsType := "" // Block volumes do not require a filesystem type
	err = ns.mounter.Mount(devicePath, targetPath, fsType, options)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to mount device at target path",
			"method", "handleBlockVolumePublish", "stagingPath", devicePath, "targetPath", targetPath)
		return nil, fmt.Errorf("failed to mount device at target path: %w", err)
	}

	klog.V(0).InfoS("Block volume successfully mounted at target path",
		"method", "handleBlockVolumePublish", "stagingPath", devicePath, "targetPath", targetPath)

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
		if health, healthErr := ns.evaluateLocalDiskPath("", targetPath); healthErr == nil && health.Stale {
			return nil, status.Errorf(codes.FailedPrecondition, "stale local disk target detected at %s: %s", targetPath, health.Message)
		}
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

	err := mount.CleanupMountPoint(targetPath, ns.mounter.Interface, true)
	if err != nil {
		if opennebula.IsSharedFilesystemVolumeID(volumeID) && ns.cleanupOrphanedSharedFilesystemTargetOnUnpublish(context.Background(), volumeID, targetPath, err) {
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		klog.V(0).ErrorS(err, "Failed to unmount volume at target path",
			"method", "NodeUnpublishVolume", "volumeID", volumeID, "targetPath", targetPath)
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to unmount volume at target path %s: %v", targetPath, err))
	}

	klog.V(1).InfoS("Volume successfully unpublished from target path",
		"method", "NodeUnpublishVolume", "volumeID", volumeID, "targetPath", targetPath)
	if opennebula.IsSharedFilesystemVolumeID(volumeID) {
		ns.removeSharedFilesystemPublishedTarget(volumeID, targetPath)
	} else {
		ns.removeLocalDiskPublishedTarget(volumeID, targetPath)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	volumePath := req.GetVolumePath()
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}

	info, err := nodeVolumePathStat(volumePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "volume path %s was not found", volumePath)
		}
		if opennebula.IsSharedFilesystemVolumeID(volumeID) && isDisconnectedSharedFilesystemError(err) {
			return nil, ns.handleDisconnectedSharedFilesystemPath(volumeID, volumePath, err)
		}
		if isStaleLocalDiskPathError(err) {
			return nil, ns.handleStaleLocalDiskPath(volumeID, volumePath, err)
		}
		return nil, status.Errorf(codes.Internal, "failed to stat volume path %s: %v", volumePath, err)
	}

	if info.IsDir() {
		var fs unix.Statfs_t
		if err := nodeVolumePathFS(volumePath, &fs); err != nil {
			if opennebula.IsSharedFilesystemVolumeID(volumeID) && isDisconnectedSharedFilesystemError(err) {
				return nil, ns.handleDisconnectedSharedFilesystemPath(volumeID, volumePath, err)
			}
			if isStaleLocalDiskPathError(err) {
				return nil, ns.handleStaleLocalDiskPath(volumeID, volumePath, err)
			}
			return nil, status.Errorf(codes.Internal, "failed to collect filesystem stats for %s: %v", volumePath, err)
		}

		totalBytes := int64(fs.Blocks) * int64(fs.Bsize)
		availableBytes := int64(fs.Bavail) * int64(fs.Bsize)
		usedBytes := totalBytes - (int64(fs.Bfree) * int64(fs.Bsize))
		totalInodes := int64(fs.Files)
		availableInodes := int64(fs.Ffree)
		usedInodes := totalInodes - availableInodes

		return &csi.NodeGetVolumeStatsResponse{
			Usage: []*csi.VolumeUsage{
				{
					Unit:      csi.VolumeUsage_BYTES,
					Total:     totalBytes,
					Available: availableBytes,
					Used:      usedBytes,
				},
				{
					Unit:      csi.VolumeUsage_INODES,
					Total:     totalInodes,
					Available: availableInodes,
					Used:      usedInodes,
				},
			},
		}, nil
	}

	sizeBytes, err := ns.getBlockVolumeSize(volumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to collect block volume stats for %s: %v", volumePath, err)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Total:     sizeBytes,
				Available: 0,
				Used:      sizeBytes,
			},
		},
	}, nil
}

func (ns *NodeServer) NodeExpandVolume(_ context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	started := nodeNow()
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		ns.recordNodeExpandOperation(started, "unknown", "invalid_argument")
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	volumePath := req.GetVolumePath()
	if volumePath == "" {
		ns.recordNodeExpandOperation(started, "unknown", "invalid_argument")
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}

	capacityRange := req.GetCapacityRange()
	if capacityRange == nil || capacityRange.GetRequiredBytes() <= 0 {
		ns.recordNodeExpandOperation(started, "unknown", "invalid_argument")
		return nil, status.Error(codes.InvalidArgument, "required capacity is missing")
	}

	requiredBytes := capacityRange.GetRequiredBytes()
	klog.V(1).InfoS("NodeExpandVolume called",
		"method", "NodeExpandVolume",
		"volumeID", volumeID,
		"volumePath", volumePath,
		"requiredBytes", requiredBytes)

	if capability := req.GetVolumeCapability(); capability != nil {
		if _, ok := capability.GetAccessType().(*csi.VolumeCapability_Block); ok {
			ns.recordNodeExpandOperation(started, "disk", "success")
			return &csi.NodeExpandVolumeResponse{
				CapacityBytes: requiredBytes,
			}, nil
		}
	}

	if opennebula.IsSharedFilesystemVolumeID(volumeID) {
		ns.recordNodeExpandOperation(started, "cephfs", "unimplemented")
		return nil, status.Error(codes.Unimplemented, "CephFS shared filesystem expansion is not supported in v0.4.1")
	}

	// The node-side filesystem resizer is only available in the linux build of mount-utils.
	// Keep the RPC successful in non-linux unit-test environments.
	if nodeRuntimeGOOS != "linux" {
		ns.recordNodeExpandOperation(started, "disk", "success")
		return &csi.NodeExpandVolumeResponse{
			CapacityBytes: requiredBytes,
		}, nil
	}

	devicePath, err := ns.getMountedDevicePath(req.GetStagingTargetPath())
	if err != nil && req.GetStagingTargetPath() != volumePath {
		devicePath, err = ns.getMountedDevicePath(volumePath)
	}
	if err != nil {
		ns.recordNodeExpandOperation(started, "disk", "failed_precondition")
		return nil, status.Errorf(codes.FailedPrecondition, "failed to resolve device path for volume expansion: %v", err)
	}

	policy := ns.nodeExpandPolicy()
	targetMinBytes := requiredBytes - policy.sizeToleranceBytes
	if targetMinBytes < 0 {
		targetMinBytes = 0
	}

	deadline := nodeNow().Add(policy.verifyTimeout)
	var (
		lastDeviceBytes int64
		lastFSBytes     int64
		attempts        int
		resized         bool
	)

	for {
		attempts++

		lastDeviceBytes, err = ns.getBlockVolumeSize(devicePath)
		if err != nil {
			ns.recordNodeExpandOperation(started, "disk", "internal")
			return nil, status.Errorf(codes.Internal, "failed to collect device size for volume %s at %s: %v", volumeID, devicePath, err)
		}

		if lastDeviceBytes < targetMinBytes {
			if !nodeNow().Before(deadline) {
				ns.recordNodeExpandOperation(started, "disk", "deadline_exceeded")
				klog.V(0).InfoS("NodeExpandVolume timed out waiting for device size",
					"method", "NodeExpandVolume",
					"volumeID", volumeID,
					"volumePath", volumePath,
					"devicePath", devicePath,
					"requiredBytes", requiredBytes,
					"targetMinBytes", targetMinBytes,
					"deviceBytes", lastDeviceBytes,
					"filesystemBytes", lastFSBytes,
					"attempts", attempts)
				return nil, status.Errorf(
					codes.DeadlineExceeded,
					"filesystem resize did not converge for volume %s: requested_bytes=%d target_min_bytes=%d device_bytes=%d filesystem_bytes=%d attempts=%d",
					volumeID, requiredBytes, targetMinBytes, lastDeviceBytes, lastFSBytes, attempts,
				)
			}

			klog.V(2).InfoS("NodeExpandVolume waiting for device capacity",
				"method", "NodeExpandVolume",
				"volumeID", volumeID,
				"volumePath", volumePath,
				"devicePath", devicePath,
				"requiredBytes", requiredBytes,
				"targetMinBytes", targetMinBytes,
				"deviceBytes", lastDeviceBytes,
				"attempt", attempts)
			nodeDeviceSleep(policy.retryInterval)
			continue
		}

		resized, err = nodeResizeFS(ns.mounter.Exec, devicePath, volumePath)
		if err != nil {
			ns.recordNodeExpandOperation(started, "disk", "internal")
			return nil, status.Errorf(codes.Internal, "failed to resize filesystem for volume %s: %v", volumeID, err)
		}

		lastFSBytes, err = ns.filesystemSizeBytes(volumePath)
		if err != nil {
			ns.recordNodeExpandOperation(started, "disk", "internal")
			return nil, status.Errorf(codes.Internal, "failed to collect filesystem size for volume %s at %s: %v", volumeID, volumePath, err)
		}

		if lastFSBytes >= targetMinBytes {
			ns.recordNodeExpandOperation(started, "disk", "success")
			klog.V(1).InfoS("NodeExpandVolume converged",
				"method", "NodeExpandVolume",
				"volumeID", volumeID,
				"volumePath", volumePath,
				"devicePath", devicePath,
				"requiredBytes", requiredBytes,
				"targetMinBytes", targetMinBytes,
				"deviceBytes", lastDeviceBytes,
				"filesystemBytes", lastFSBytes,
				"resized", resized,
				"attempts", attempts)
			return &csi.NodeExpandVolumeResponse{
				CapacityBytes: requiredBytes,
			}, nil
		}

		if !nodeNow().Before(deadline) {
			ns.recordNodeExpandOperation(started, "disk", "deadline_exceeded")
			klog.V(0).InfoS("NodeExpandVolume timed out waiting for filesystem growth",
				"method", "NodeExpandVolume",
				"volumeID", volumeID,
				"volumePath", volumePath,
				"devicePath", devicePath,
				"requiredBytes", requiredBytes,
				"targetMinBytes", targetMinBytes,
				"deviceBytes", lastDeviceBytes,
				"filesystemBytes", lastFSBytes,
				"resized", resized,
				"attempts", attempts)
			return nil, status.Errorf(
				codes.DeadlineExceeded,
				"filesystem resize did not converge for volume %s: requested_bytes=%d target_min_bytes=%d device_bytes=%d filesystem_bytes=%d attempts=%d",
				volumeID, requiredBytes, targetMinBytes, lastDeviceBytes, lastFSBytes, attempts,
			)
		}

		klog.V(2).InfoS("NodeExpandVolume waiting for filesystem capacity",
			"method", "NodeExpandVolume",
			"volumeID", volumeID,
			"volumePath", volumePath,
			"devicePath", devicePath,
			"requiredBytes", requiredBytes,
			"targetMinBytes", targetMinBytes,
			"deviceBytes", lastDeviceBytes,
			"filesystemBytes", lastFSBytes,
			"resized", resized,
			"attempt", attempts)
		nodeDeviceSleep(policy.retryInterval)
	}
}

type nodeExpandPolicy struct {
	verifyTimeout      time.Duration
	retryInterval      time.Duration
	sizeToleranceBytes int64
}

func (ns *NodeServer) nodeExpandPolicy() nodeExpandPolicy {
	policy := nodeExpandPolicy{
		verifyTimeout:      defaultNodeExpandTimeout,
		retryInterval:      defaultNodeExpandRetry,
		sizeToleranceBytes: defaultNodeExpandTolerance,
	}
	if ns == nil || ns.Driver == nil {
		return policy
	}

	if timeoutSeconds, ok := ns.Driver.PluginConfig.GetInt(config.NodeExpandVerifyTimeoutSecondsVar); ok {
		timeout := time.Duration(timeoutSeconds) * time.Second
		if timeout >= minNodeExpandTimeout {
			policy.verifyTimeout = timeout
		}
	}

	if retrySeconds, ok := ns.Driver.PluginConfig.GetInt(config.NodeExpandRetryIntervalSecondsVar); ok {
		retry := time.Duration(retrySeconds) * time.Second
		if retry >= minNodeExpandRetry {
			policy.retryInterval = retry
		}
	}

	if toleranceBytes, ok := ns.Driver.PluginConfig.GetInt(config.NodeExpandSizeToleranceBytesVar); ok {
		if toleranceBytes >= 0 {
			policy.sizeToleranceBytes = int64(toleranceBytes)
		}
	}

	return policy
}

func (ns *NodeServer) filesystemSizeBytes(volumePath string) (int64, error) {
	var fs unix.Statfs_t
	if err := nodeVolumePathFS(volumePath, &fs); err != nil {
		return 0, err
	}
	return int64(fs.Blocks) * int64(fs.Bsize), nil
}

func (ns *NodeServer) recordNodeExpandOperation(started time.Time, backend, outcome string) {
	if ns == nil || ns.Driver == nil || ns.Driver.metrics == nil {
		return
	}
	ns.Driver.metrics.RecordOperation("node_expand_volume", backend, outcome, nodeNow().Sub(started))
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
	}

	return &csi.NodeGetCapabilitiesResponse{Capabilities: capabilities}, nil
}

func (ns *NodeServer) getBlockVolumeSize(volumePath string) (int64, error) {
	info, err := os.Stat(volumePath)
	if err != nil {
		return 0, err
	}

	if info.Mode().IsRegular() {
		return info.Size(), nil
	}

	output, err := ns.mounter.Exec.Command("blockdev", "--getsize64", volumePath).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("blockdev failed: %w: %s", err, strings.TrimSpace(string(output)))
	}

	sizeBytes, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid block size output %q: %w", strings.TrimSpace(string(output)), err)
	}

	return sizeBytes, nil
}

func (ns *NodeServer) NodeGetInfo(_ context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	klog.V(1).InfoS("NodeGetInfo called", "req", protosanitizer.StripSecrets(req).String())

	nodeId := ns.Driver.nodeID
	maxVolumesPerNode := ns.Driver.maxVolumesPerNode

	klog.V(3).InfoS("Returning node info",
		"nodeId", nodeId, "maxVolumesPerNode", maxVolumesPerNode)

	response := &csi.NodeGetInfoResponse{
		NodeId:            nodeId,
		MaxVolumesPerNode: maxVolumesPerNode,
	}
	if accessibleTopology := ns.Driver.nodeAccessibleTopology(context.Background()); len(accessibleTopology) > 0 {
		response.AccessibleTopology = accessibleTopology[0]
	}

	return response, nil
}

func (ns *NodeServer) getDeviceName(volumeName string) string {
	return path.Join(defaultDiskPath, volumeName)
}

type deviceResolutionResult struct {
	ResolvedBy string
	Latency    time.Duration
}

func (ns *NodeServer) resolveDevicePath(volumeName string, timeout time.Duration) (string, deviceResolutionResult, error) {
	return ns.resolveDevicePathWithContext("", volumeName, nil, timeout)
}

func (ns *NodeServer) resolveDevicePathWithContext(volumeID, volumeName string, publishContext map[string]string, timeout time.Duration) (string, deviceResolutionResult, error) {
	if ns.deviceResolver != nil {
		return ns.deviceResolver.Resolve(context.Background(), volumeID, volumeName, publishContext, timeout)
	}

	started := time.Now()
	candidates := ns.deviceCandidates(volumeName)
	deadline := time.Now().Add(timeout)
	var lastErr error
	expected := ns.getDeviceName(volumeName)

	for {
		for _, candidate := range candidates {
			if _, err := nodeVolumePathStat(candidate); err == nil {
				result := deviceResolutionResult{
					ResolvedBy: "exact",
					Latency:    time.Since(started),
				}
				if candidate != expected {
					result.ResolvedBy = "alias"
					klog.V(2).InfoS("Resolved device path via alias",
						"method", "resolveDevicePath", "volumeName", volumeName, "devicePath", candidate)
				}
				return candidate, result, nil
			} else if !os.IsNotExist(err) {
				lastErr = err
			}
		}

		if time.Now().After(deadline) {
			break
		}

		nodeDeviceSleep(defaultNodeDevicePollPeriod)
	}

	if lastErr != nil {
		return "", deviceResolutionResult{}, fmt.Errorf(
			"timed out after %s waiting for device path for volume %q (checked %s, last error: %v)",
			timeout, volumeName, strings.Join(candidates, ", "), lastErr,
		)
	}

	return "", deviceResolutionResult{}, fmt.Errorf(
		"timed out after %s waiting for device path for volume %q (checked %s)",
		timeout, volumeName, strings.Join(candidates, ", "),
	)
}

func (ns *NodeServer) deviceDiscoveryTimeout(publishContext map[string]string) time.Duration {
	if publishContext != nil {
		if raw, ok := publishContext[publishContextDeviceDiscoveryTimeoutSeconds]; ok && raw != "" {
			timeoutSeconds, err := strconv.Atoi(raw)
			if err == nil && timeoutSeconds > 0 {
				return time.Duration(timeoutSeconds) * time.Second
			}
		}
		if raw, ok := publishContext[publishContextHotplugTimeoutSeconds]; ok && raw != "" {
			timeoutSeconds, err := strconv.Atoi(raw)
			if err == nil && timeoutSeconds > 0 {
				return time.Duration(timeoutSeconds) * time.Second
			}
		}
	}

	timeoutSeconds, ok := ns.Driver.PluginConfig.GetInt(config.NodeDeviceDiscoveryTimeoutVar)
	if !ok || timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}

	return time.Duration(timeoutSeconds) * time.Second
}

func (ns *NodeServer) recordPVCWarningFromPublishContext(ctx context.Context, publishContext map[string]string, reason, message string) {
	if ns == nil || ns.Driver == nil || ns.Driver.kubeRuntime == nil || publishContext == nil {
		return
	}
	namespace := strings.TrimSpace(publishContext[paramPVCNamespace])
	name := strings.TrimSpace(publishContext[paramPVCName])
	if namespace == "" || name == "" {
		return
	}
	ns.Driver.kubeRuntime.EmitWarningEventOnPVC(ctx, namespace, name, reason, message)
}

func (ns *NodeServer) recordPVCEventFromPublishContext(ctx context.Context, publishContext map[string]string, reason, message string) {
	if ns == nil || ns.Driver == nil || ns.Driver.kubeRuntime == nil || publishContext == nil {
		return
	}
	namespace := strings.TrimSpace(publishContext[paramPVCNamespace])
	name := strings.TrimSpace(publishContext[paramPVCName])
	if namespace == "" || name == "" {
		return
	}
	ns.Driver.kubeRuntime.EmitPVCEvent(ctx, namespace, name, reason, message)
}

func (ns *NodeServer) publishContextBackend(publishContext map[string]string) string {
	if publishContext == nil {
		return "disk"
	}
	if backend := strings.TrimSpace(publishContext[annotationBackend]); backend != "" {
		return backend
	}
	return "disk"
}

func (ns *NodeServer) recordDeviceResolutionFromPublishContext(ctx context.Context, publishContext map[string]string, resolution deviceResolutionResult) {
	if ns == nil || ns.Driver == nil || ns.Driver.metrics == nil {
		return
	}
	method := resolution.ResolvedBy
	if method == "" {
		method = "unknown"
	}
	ns.Driver.metrics.RecordNodeDeviceResolution(method, "success")
	switch {
	case strings.HasPrefix(method, "cache"):
		ns.Driver.metrics.RecordDeviceCache("lookup", "hit")
		ns.recordPVCEventFromPublishContext(ctx, publishContext, eventReasonDeviceCacheHit, fmt.Sprintf("resolved device using cache via %s", method))
	case strings.Contains(method, "by-id"):
		ns.Driver.metrics.RecordDeviceCache("lookup", "miss")
		ns.recordPVCEventFromPublishContext(ctx, publishContext, eventReasonDeviceByIDResolved, fmt.Sprintf("resolved device using /dev/disk/by-id via %s", method))
	default:
		ns.Driver.metrics.RecordDeviceCache("lookup", "miss")
		ns.recordPVCEventFromPublishContext(ctx, publishContext, eventReasonDeviceCacheMiss, fmt.Sprintf("resolved device after cache miss via %s", method))
	}
}

func (ns *NodeServer) deviceCandidates(volumeName string) []string {
	baseName := filepath.Base(volumeName)
	names := []string{baseName}

	switch {
	case strings.HasPrefix(baseName, "sd"):
		suffix := strings.TrimPrefix(baseName, "sd")
		names = append(names, "vd"+suffix, "xvd"+suffix)
	case strings.HasPrefix(baseName, "vd"):
		suffix := strings.TrimPrefix(baseName, "vd")
		names = append(names, "sd"+suffix, "xvd"+suffix)
	case strings.HasPrefix(baseName, "xvd"):
		suffix := strings.TrimPrefix(baseName, "xvd")
		names = append(names, "vd"+suffix, "sd"+suffix)
	}

	candidates := make([]string, 0, len(names))
	for _, name := range names {
		candidate := ns.getDeviceName(name)
		if slices.Contains(candidates, candidate) {
			continue
		}
		candidates = append(candidates, candidate)
	}

	return candidates
}

func (ns *NodeServer) getMountedDevicePath(targetPath string) (string, error) {
	if targetPath == "" {
		return "", fmt.Errorf("mount path is required")
	}

	mountPoints, err := ns.mounter.List()
	if err != nil {
		return "", fmt.Errorf("failed to list mount points: %w", err)
	}

	for _, mountPoint := range mountPoints {
		if mountPoint.Path == targetPath {
			if mountPoint.Device == "" {
				return "", fmt.Errorf("mount point %s does not expose a backing device", targetPath)
			}
			return mountPoint.Device, nil
		}
	}

	return "", fmt.Errorf("mount point %s was not found", targetPath)
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
