package driver

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"
)

const (
	sharedPublishContextShareBackend   = "shareBackend"
	sharedPublishContextCephFSMonitors = "cephfsMonitors"
	sharedPublishContextCephFSFSName   = "cephfsFSName"
	sharedPublishContextCephFSSubpath  = "cephfsSubpath"
	sharedPublishContextCephFSReadonly = "cephfsReadonly"
	sharedPublishContextCephFSMounts   = "cephfsMountOptions"

	sharedNodeStageSecretUserIDKey  = "userID"
	sharedNodeStageSecretUserKeyKey = "userKey"

	sharedCephFSTempDirSuffix = ".cephfs"
	sharedCephFSKeyringFile   = "ceph.client.keyring"
)

func isSharedFilesystemRequest(volumeID string, publishContext map[string]string) bool {
	return opennebula.IsSharedFilesystemVolumeID(volumeID) || strings.TrimSpace(publishContext[sharedPublishContextShareBackend]) != ""
}

func (ns *NodeServer) handleSharedFilesystemStage(req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	release := ns.acquireSharedFilesystemOperationLock(req.GetVolumeId())
	defer release()

	volumeCapability := req.GetVolumeCapability()
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	if _, ok := volumeCapability.GetAccessType().(*csi.VolumeCapability_Block); ok {
		return nil, status.Error(codes.InvalidArgument, "shared filesystem volumes require a filesystem volume capability")
	}

	publishContext := req.GetPublishContext()
	if strings.TrimSpace(publishContext[sharedPublishContextShareBackend]) != "cephfs" {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported shared filesystem backend %q", publishContext[sharedPublishContextShareBackend])
	}

	stagingTargetPath := req.GetStagingTargetPath()
	if stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	if err := os.MkdirAll(stagingTargetPath, 0775); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create staging target path %s: %v", stagingTargetPath, err)
	}

	session, err := sharedFilesystemSessionFromStageRequest(req)
	if err != nil {
		return nil, err
	}

	secrets := req.GetSecrets()
	userID := strings.TrimSpace(secrets[sharedNodeStageSecretUserIDKey])
	userKey := strings.TrimSpace(secrets[sharedNodeStageSecretUserKeyKey])
	if userID == "" || userKey == "" {
		return nil, status.Errorf(codes.InvalidArgument, "CephFS node staging requires secret keys %q and %q", sharedNodeStageSecretUserIDKey, sharedNodeStageSecretUserKeyKey)
	}

	keyringPath, secretFilePath, err := ensureSharedFilesystemCredentials(stagingTargetPath, userID, userKey)
	if err != nil {
		return nil, err
	}
	session.KeyringPath = keyringPath
	session.SecretFilePath = secretFilePath

	notMountPoint, err := ns.mounter.Interface.IsLikelyNotMountPoint(stagingTargetPath)
	if err == nil && !notMountPoint {
		if staleErr := detectStaleSharedFilesystemMount(stagingTargetPath); staleErr == nil {
			ns.recordSharedFilesystemSession(session)
			return &csi.NodeStageVolumeResponse{}, nil
		} else {
			ns.Driver.metrics.RecordCephFSSubvolume("stale_mount_detected", "failure")
			klog.Warningf("detected stale CephFS mount at %s: %v", stagingTargetPath, staleErr)
			if !ns.Driver.featureGates.CephFSSelfHealing {
				return nil, status.Errorf(codes.FailedPrecondition, "stale CephFS mount detected at %s: %v", stagingTargetPath, staleErr)
			}
			if err := mountCleanup(stagingTargetPath, ns); err != nil {
				return nil, err
			}
			ns.Driver.metrics.RecordCephFSSubvolume("self_heal_remount", "attempted")
		}
	}

	if err := ns.mountSharedFilesystemSession(session); err != nil {
		ns.Driver.metrics.RecordCephFSSubvolume("mount", "failure")
		return nil, err
	}
	ns.recordSharedFilesystemSession(session)

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *NodeServer) handleSharedFilesystemUnstage(req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	release := ns.acquireSharedFilesystemOperationLock(req.GetVolumeId())
	defer release()

	stagingTargetPath := req.GetStagingTargetPath()
	if stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	err := mountCleanup(stagingTargetPath, ns)
	if err != nil {
		return nil, err
	}

	_ = os.RemoveAll(sharedCephFSKeyringDir(stagingTargetPath))
	ns.deleteSharedFilesystemSession(req.GetVolumeId())
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *NodeServer) handleSharedFilesystemPublish(req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	release := ns.acquireSharedFilesystemOperationLock(req.GetVolumeId())
	defer release()

	volumeCapability := req.GetVolumeCapability()
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	if _, ok := volumeCapability.GetAccessType().(*csi.VolumeCapability_Block); ok {
		return nil, status.Error(codes.InvalidArgument, "shared filesystem volumes require a filesystem volume capability")
	}

	stagingTargetPath := req.GetStagingTargetPath()
	targetPath := req.GetTargetPath()
	if stagingTargetPath == "" || targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path and target path are required")
	}

	target := sharedFilesystemTargetFromPublishRequest(req)
	if err := ns.publishSharedFilesystemTarget(stagingTargetPath, target); err != nil {
		klog.V(0).ErrorS(err, "Failed to publish shared filesystem volume",
			"method", "handleSharedFilesystemPublish", "stagingTargetPath", stagingTargetPath, "targetPath", targetPath, "accessType", reflect.TypeOf(volumeCapability.GetAccessType()).String())
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, status.Error(codes.Internal, "failed to publish volume")
	}
	ns.updateSharedFilesystemPublishedTarget(req.GetVolumeId(), target)

	return &csi.NodePublishVolumeResponse{}, nil
}

func mountCleanup(stagingTargetPath string, ns *NodeServer) error {
	err := mount.CleanupMountPoint(stagingTargetPath, ns.mounter.Interface, true)
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to clean mount point of staging target path",
			"method", "handleSharedFilesystemUnstage", "stagingTargetPath", stagingTargetPath)
		return status.Error(codes.Internal, "failed to cleanup mount point of staging target path")
	}

	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		unique = append(unique, trimmed)
	}

	return unique
}

func (ns *NodeServer) publishSharedFilesystemTarget(stagingTargetPath string, target sharedFilesystemPublishedTarget) error {
	targetPath := strings.TrimSpace(target.TargetPath)
	if stagingTargetPath == "" || targetPath == "" {
		return status.Error(codes.InvalidArgument, "staging target path and target path are required")
	}

	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		if err := os.MkdirAll(targetPath, 0750); err != nil {
			return status.Errorf(codes.Internal, "failed to create target path %s: %v", targetPath, err)
		}
	}

	mountCheck, err := ns.checkMountPoint(stagingTargetPath, targetPath, nil)
	if err != nil {
		return err
	}
	if mountCheck.targetIsMountPoint {
		if _, err := nodeVolumePathStat(targetPath); err != nil && isDisconnectedSharedFilesystemError(err) {
			if ns.Driver != nil && ns.Driver.metrics != nil {
				ns.Driver.metrics.RecordCephFSSubvolume("stale_mount_detected", "failure")
			}
			return status.Errorf(codes.FailedPrecondition, "stale CephFS target detected at %s: %v", targetPath, err)
		}
		return nil
	}

	options := append([]string{"bind"}, uniqueStrings(target.MountOptions)...)
	if err := ns.mounter.Interface.Mount(stagingTargetPath, targetPath, "", options); err != nil {
		return fmt.Errorf("failed to bind shared filesystem target %s: %w", targetPath, err)
	}

	return nil
}

func (ns *NodeServer) mountSharedFilesystemSession(session sharedFilesystemSession) error {
	if err := ensureSharedFilesystemStagePath(session.StagingTargetPath); err != nil {
		return err
	}

	switch session.Mounter {
	case sharedFilesystemMounterKernel:
		return ns.mountSharedFilesystemKernel(session)
	case sharedFilesystemMounterFuse:
		return ns.mountSharedFilesystemFuse(session)
	default:
		return status.Errorf(codes.InvalidArgument, "unsupported CephFS mounter %q", session.Mounter)
	}
}

func ensureSharedFilesystemStagePath(stagingTargetPath string) error {
	stagingTargetPath = strings.TrimSpace(stagingTargetPath)
	if stagingTargetPath == "" {
		return status.Error(codes.InvalidArgument, "staging target path is required")
	}
	if err := os.MkdirAll(stagingTargetPath, 0o775); err != nil {
		return status.Errorf(codes.Internal, "failed to create staging target path %s: %v", stagingTargetPath, err)
	}
	return nil
}

func (ns *NodeServer) mountSharedFilesystemFuse(session sharedFilesystemSession) error {
	confPath, err := ensureSharedFilesystemCephConf(session.StagingTargetPath)
	if err != nil {
		return err
	}
	args := []string{
		session.StagingTargetPath,
		"-c", confPath,
		"-m", strings.Join(session.Monitors, ","),
		"--id", session.UserID,
		"-k", session.KeyringPath,
		"--client_mountpoint", session.Subpath,
		"--client_fs", session.FSName,
	}
	if len(session.StageMountOptions) > 0 {
		args = append(args, "-o", strings.Join(uniqueStrings(session.StageMountOptions), ","))
	}

	output, err := ns.mounter.Exec.Command("ceph-fuse", args...).CombinedOutput()
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to mount CephFS volume",
			"method", "mountSharedFilesystemFuse", "stagingTargetPath", session.StagingTargetPath, "output", string(output))
		return status.Errorf(codes.Internal, "failed to mount CephFS volume: %s", strings.TrimSpace(string(output)))
	}
	ns.Driver.metrics.RecordCephFSSubvolume("mount", "success")
	ns.recordSharedFilesystemTestStageMount(session)
	return nil
}

func (ns *NodeServer) mountSharedFilesystemKernel(session sharedFilesystemSession) error {
	if !ns.Driver.featureGates.CephFSKernelMounts {
		return status.Error(codes.FailedPrecondition, "CephFS kernel mounts require feature gate cephfsKernelMounts=true")
	}
	if err := sharedFilesystemKernelSupported(); err != nil {
		return status.Errorf(codes.FailedPrecondition, "CephFS kernel mount prerequisites failed: %v", err)
	}
	if _, err := ns.mounter.Exec.LookPath("mount.ceph"); err != nil {
		return status.Errorf(codes.FailedPrecondition, "mount.ceph helper is not available in PATH: %v", err)
	}
	if strings.TrimSpace(session.SecretFilePath) == "" {
		return status.Error(codes.FailedPrecondition, "CephFS kernel mounts require a secretfile path")
	}

	device := fmt.Sprintf("%s@.%s=%s", session.UserID, session.FSName, session.Subpath)
	options := []string{
		"mon_addr=" + sharedFilesystemKernelMonitorOption(session.Monitors),
		"secretfile=" + session.SecretFilePath,
	}
	options = append(options, uniqueStrings(session.StageMountOptions)...)
	args := []string{"-t", "ceph", device, session.StagingTargetPath, "-o", strings.Join(options, ",")}

	output, err := ns.mounter.Exec.Command("mount", args...).CombinedOutput()
	if err != nil {
		klog.V(0).ErrorS(err, "Failed to mount CephFS volume with kernel client",
			"method", "mountSharedFilesystemKernel", "stagingTargetPath", session.StagingTargetPath, "output", string(output))
		return status.Errorf(codes.Internal, "failed to mount CephFS volume with kernel client: %s", strings.TrimSpace(string(output)))
	}
	ns.Driver.metrics.RecordCephFSSubvolume("mount", "success")
	ns.recordSharedFilesystemTestStageMount(session)
	return nil
}

func (ns *NodeServer) recordSharedFilesystemTestStageMount(session sharedFilesystemSession) {
	fake, ok := ns.mounter.Interface.(*mount.FakeMounter)
	if !ok {
		return
	}
	for _, mountPoint := range fake.MountPoints {
		if mountPoint.Path == session.StagingTargetPath {
			return
		}
	}
	fsType := "fuse.ceph-fuse"
	if session.Mounter == sharedFilesystemMounterKernel {
		fsType = "ceph"
	}
	fake.MountPoints = append(fake.MountPoints, mount.MountPoint{
		Device: strings.Join(session.Monitors, ","),
		Path:   session.StagingTargetPath,
		Type:   fsType,
		Opts:   uniqueStrings(session.StageMountOptions),
	})
}

func sharedCephFSKeyringDir(stagingTargetPath string) string {
	return stagingTargetPath + sharedCephFSTempDirSuffix
}

func detectStaleSharedFilesystemMount(stagingTargetPath string) error {
	entries, err := os.ReadDir(stagingTargetPath)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	return nil
}

func isDisconnectedSharedFilesystemError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "transport endpoint is not connected")
}
