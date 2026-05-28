package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"
)

const (
	defaultLocalDiskSessionDir = "/var/lib/kubelet/plugins/csi.opennebula.io/localdisk-sessions"
)

var localDiskSessionRootPath = defaultLocalDiskSessionDir

type localDiskPublishedTarget struct {
	TargetPath   string   `json:"targetPath"`
	MountOptions []string `json:"mountOptions,omitempty"`
}

type localDiskSession struct {
	VolumeID          string                     `json:"volumeID"`
	VolumeName        string                     `json:"volumeName,omitempty"`
	StagingTargetPath string                     `json:"stagingTargetPath"`
	DevicePath        string                     `json:"devicePath,omitempty"`
	DeviceSerial      string                     `json:"deviceSerial,omitempty"`
	OpenNebulaImageID string                     `json:"opennebulaImageID,omitempty"`
	FSType            string                     `json:"fsType,omitempty"`
	StageMountOptions []string                   `json:"stageMountOptions,omitempty"`
	PVCNamespace      string                     `json:"pvcNamespace,omitempty"`
	PVCName           string                     `json:"pvcName,omitempty"`
	PVName            string                     `json:"pvName,omitempty"`
	PublishedTargets  []localDiskPublishedTarget `json:"publishedTargets,omitempty"`
	LastRecoveredAt   *time.Time                 `json:"lastRecoveredAt,omitempty"`
	LastRecoveryError string                     `json:"lastRecoveryError,omitempty"`
	RecoveryAttempts  int                        `json:"recoveryAttempts,omitempty"`
}

type localDiskMountHealth struct {
	Healthy     bool
	Stale       bool
	Reason      string
	MountSource string
	Message     string
}

type localDiskSessionStore struct {
	root string
	mu   sync.Mutex
}

func newLocalDiskSessionStore(root string) *localDiskSessionStore {
	root = strings.TrimSpace(root)
	if root == "" {
		root = defaultLocalDiskSessionDir
	}
	return &localDiskSessionStore{root: root}
}

func (s *localDiskSessionStore) Save(session localDiskSession) error {
	if strings.TrimSpace(session.VolumeID) == "" {
		return fmt.Errorf("local disk session is missing volume ID")
	}
	session.StageMountOptions = uniqueStrings(session.StageMountOptions)
	session.PublishedTargets = normalizeLocalDiskPublishedTargets(session.PublishedTargets)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.pathForVolume(session.VolumeID), payload, 0o600)
}

func (s *localDiskSessionStore) Load(volumeID string) (localDiskSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, err := os.ReadFile(s.pathForVolume(volumeID))
	if err != nil {
		if os.IsNotExist(err) {
			return localDiskSession{}, false, nil
		}
		return localDiskSession{}, false, err
	}
	var session localDiskSession
	if err := json.Unmarshal(payload, &session); err != nil {
		return localDiskSession{}, false, err
	}
	session.StageMountOptions = uniqueStrings(session.StageMountOptions)
	session.PublishedTargets = normalizeLocalDiskPublishedTargets(session.PublishedTargets)
	return session, true, nil
}

func (s *localDiskSessionStore) Delete(volumeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.pathForVolume(volumeID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *localDiskSessionStore) List() ([]localDiskSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sessions := make([]localDiskSession, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(s.root, entry.Name()))
		if err != nil {
			continue
		}
		var session localDiskSession
		if err := json.Unmarshal(payload, &session); err != nil {
			continue
		}
		session.StageMountOptions = uniqueStrings(session.StageMountOptions)
		session.PublishedTargets = normalizeLocalDiskPublishedTargets(session.PublishedTargets)
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func (s *localDiskSessionStore) pathForVolume(volumeID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(volumeID)))
	return filepath.Join(s.root, hex.EncodeToString(sum[:])+".json")
}

func (ns *NodeServer) recordLocalDiskSession(session localDiskSession) {
	if ns == nil || ns.localDiskSessions == nil {
		return
	}
	if err := ns.localDiskSessions.Save(session); err != nil {
		klog.ErrorS(err, "Failed to persist local disk session", "volumeID", session.VolumeID)
	}
}

func (ns *NodeServer) deleteLocalDiskSession(volumeID string) {
	if ns == nil || ns.localDiskSessions == nil {
		return
	}
	if err := ns.localDiskSessions.Delete(volumeID); err != nil {
		klog.ErrorS(err, "Failed to delete local disk session", "volumeID", volumeID)
	}
}

func (ns *NodeServer) recordLocalDiskStageSession(req *csi.NodeStageVolumeRequest, devicePath, fsType string, mountOptions []string) {
	if req == nil {
		return
	}
	ctx := req.GetPublishContext()
	session := localDiskSession{
		VolumeID:          req.GetVolumeId(),
		VolumeName:        strings.TrimSpace(ctx["volumeName"]),
		StagingTargetPath: req.GetStagingTargetPath(),
		DevicePath:        devicePath,
		DeviceSerial:      strings.TrimSpace(ctx[publishContextDeviceSerial]),
		OpenNebulaImageID: strings.TrimSpace(ctx[publishContextOpenNebulaImageID]),
		FSType:            fsType,
		StageMountOptions: append([]string(nil), mountOptions...),
		PVCNamespace:      strings.TrimSpace(ctx[paramPVCNamespace]),
		PVCName:           strings.TrimSpace(ctx[paramPVCName]),
		PVName:            strings.TrimSpace(ctx[paramPVName]),
	}
	if existing, exists, err := ns.loadLocalDiskSession(session.VolumeID); err == nil && exists {
		session.PublishedTargets = existing.PublishedTargets
	}
	ns.recordLocalDiskSession(session)
}

func (ns *NodeServer) updateLocalDiskPublishedTarget(volumeID, targetPath string, options []string) {
	if ns == nil || ns.localDiskSessions == nil || strings.TrimSpace(volumeID) == "" {
		return
	}
	session, exists, err := ns.localDiskSessions.Load(volumeID)
	if err != nil || !exists {
		if err != nil {
			klog.ErrorS(err, "Failed to load local disk session for publish update", "volumeID", volumeID)
		}
		return
	}
	target := localDiskPublishedTarget{TargetPath: targetPath, MountOptions: append([]string(nil), options...)}
	replaced := false
	targets := make([]localDiskPublishedTarget, 0, len(session.PublishedTargets)+1)
	for _, existing := range session.PublishedTargets {
		if existing.TargetPath == target.TargetPath {
			targets = append(targets, target)
			replaced = true
			continue
		}
		targets = append(targets, existing)
	}
	if !replaced {
		targets = append(targets, target)
	}
	session.PublishedTargets = targets
	ns.recordLocalDiskSession(session)
}

func (ns *NodeServer) removeLocalDiskPublishedTarget(volumeID, targetPath string) {
	if ns == nil || ns.localDiskSessions == nil || strings.TrimSpace(volumeID) == "" {
		return
	}
	session, exists, err := ns.localDiskSessions.Load(volumeID)
	if err != nil || !exists {
		if err != nil {
			klog.ErrorS(err, "Failed to load local disk session for target removal", "volumeID", volumeID)
		}
		return
	}
	filtered := make([]localDiskPublishedTarget, 0, len(session.PublishedTargets))
	for _, target := range session.PublishedTargets {
		if target.TargetPath == targetPath {
			continue
		}
		filtered = append(filtered, target)
	}
	session.PublishedTargets = filtered
	ns.recordLocalDiskSession(session)
}

func (ns *NodeServer) evaluateLocalDiskPath(volumeID, path string) (localDiskMountHealth, error) {
	if strings.TrimSpace(path) == "" {
		return localDiskMountHealth{}, fmt.Errorf("path is required")
	}
	if _, err := nodeVolumePathStat(path); err != nil {
		if isStaleLocalDiskPathError(err) {
			return localDiskMountHealth{Stale: true, Reason: "path_stale", Message: err.Error()}, nil
		}
		return localDiskMountHealth{}, err
	}
	mountPoint, ok, err := ns.mountPointForPath(path)
	if err != nil {
		return localDiskMountHealth{}, err
	}
	if !ok {
		return localDiskMountHealth{Healthy: true, Reason: "not_mountpoint"}, nil
	}
	health := localDiskMountHealth{Healthy: true, Reason: "mounted", MountSource: mountPoint.Device}
	if mountPoint.Device == "" {
		return localDiskMountHealth{Stale: true, Reason: "missing_mount_source", Message: "mount point has no source device"}, nil
	}
	if strings.HasPrefix(mountPoint.Device, "/dev/") {
		if _, err := nodeVolumePathStat(mountPoint.Device); err != nil {
			if os.IsNotExist(err) || isStaleLocalDiskPathError(err) {
				return localDiskMountHealth{Stale: true, Reason: "source_missing", MountSource: mountPoint.Device, Message: err.Error()}, nil
			}
			return localDiskMountHealth{}, err
		}
		if session, exists, loadErr := ns.loadLocalDiskSession(volumeID); loadErr == nil && exists && session.DeviceSerial != "" {
			if !deviceMatchesSerial(ns.mounter.Exec, mountPoint.Device, session.DeviceSerial, "") {
				return localDiskMountHealth{Stale: true, Reason: "serial_mismatch", MountSource: mountPoint.Device, Message: "mount source serial does not match expected volume serial"}, nil
			}
		}
	}
	return health, nil
}

func (ns *NodeServer) handleStaleLocalDiskPath(volumeID, volumePath string, cause error) error {
	ns.recordLocalVolumeHealth("stale_mount_detected", "observed")
	session, exists, err := ns.loadLocalDiskSession(volumeID)
	if err != nil || !exists {
		if err != nil {
			klog.ErrorS(err, "Failed to load local disk session for stale path", "volumeID", volumeID)
		}
		return status.Errorf(codes.FailedPrecondition, "stale local disk mount detected at %s: %v; restage the volume to recover", volumePath, cause)
	}
	ns.recordLocalDiskPVCWarning(session, eventReasonLocalVolumeStaleMount, fmt.Sprintf("stale local disk mount detected at %s: %v", volumePath, cause))
	if !ns.localRWOStaleMountRecoveryEnabled() {
		return status.Errorf(codes.FailedPrecondition, "stale local disk mount detected at %s: %v; local RWO stale mount recovery is disabled", volumePath, cause)
	}
	if err := ns.recoverLocalDiskSession(context.Background(), session, "volume_stats"); err != nil {
		ns.recordLocalDiskPVCWarning(session, eventReasonLocalVolumeRecoveryFailed, err.Error())
		return status.Errorf(codes.FailedPrecondition, "stale local disk mount detected at %s and recovery failed: %v", volumePath, err)
	}
	return status.Errorf(codes.FailedPrecondition, "stale local disk mount detected at %s; recovery completed and kubelet should retry stats", volumePath)
}

func (ns *NodeServer) recoverLocalDiskSession(ctx context.Context, session localDiskSession, reason string) error {
	if !ns.localRWOStaleMountRecoveryEnabled() {
		return fmt.Errorf("local RWO stale mount recovery is disabled")
	}
	if !ns.localRWOActivePodRecoveryEnabled() {
		for _, target := range session.PublishedTargets {
			if podUID := podUIDFromKubeletPath(target.TargetPath); podUID != "" && ns.podUIDExists(ctx, podUID) {
				return fmt.Errorf("active-pod local RWO recovery is disabled for target %s", target.TargetPath)
			}
		}
	}
	maxAttempts := ns.localRWORecoveryMaxAttempts()
	if maxAttempts > 0 && session.RecoveryAttempts >= maxAttempts {
		return fmt.Errorf("local RWO recovery attempts exhausted for volume %s", session.VolumeID)
	}
	if session.LastRecoveredAt != nil && session.LastRecoveryError != "" {
		if wait := ns.localRWORecoveryBackoff() - time.Since(*session.LastRecoveredAt); wait > 0 {
			return fmt.Errorf("local RWO recovery for volume %s is backing off for %s after prior failure", session.VolumeID, wait.Round(time.Second))
		}
	}
	ns.recordLocalVolumeHealth("recovery", "attempted")
	ns.recordLocalDiskPVCEvent(session, eventReasonLocalVolumeRecoveryAttempted, fmt.Sprintf("attempting local RWO stale mount recovery for volume %s: %s", session.VolumeID, reason))

	devicePath, err := ns.resolveLocalDiskRecoveryDevice(ctx, session)
	if err != nil {
		return ns.recordLocalDiskRecoveryFailure(session, err)
	}
	for _, target := range session.PublishedTargets {
		if err := mount.CleanupMountPoint(target.TargetPath, ns.mounter.Interface, true); err != nil {
			return ns.recordLocalDiskRecoveryFailure(session, fmt.Errorf("failed to cleanup stale target %s: %w", target.TargetPath, err))
		}
	}
	if err := mount.CleanupMountPoint(session.StagingTargetPath, ns.mounter.Interface, true); err != nil {
		return ns.recordLocalDiskRecoveryFailure(session, fmt.Errorf("failed to cleanup stale stage %s: %w", session.StagingTargetPath, err))
	}
	if err := os.MkdirAll(session.StagingTargetPath, 0o775); err != nil {
		return ns.recordLocalDiskRecoveryFailure(session, err)
	}
	fsType := strings.TrimSpace(session.FSType)
	if fsType == "" {
		fsType = defaultFSType
	}
	if err := ns.mounter.FormatAndMount(devicePath, session.StagingTargetPath, fsType, session.StageMountOptions); err != nil {
		return ns.recordLocalDiskRecoveryFailure(session, fmt.Errorf("failed to remount recovered device %s at %s: %w", devicePath, session.StagingTargetPath, err))
	}
	for _, target := range session.PublishedTargets {
		if err := os.MkdirAll(target.TargetPath, 0o750); err != nil {
			return ns.recordLocalDiskRecoveryFailure(session, fmt.Errorf("failed to recreate target %s: %w", target.TargetPath, err))
		}
		options := append([]string{"bind"}, target.MountOptions...)
		if err := ns.mounter.Mount(session.StagingTargetPath, target.TargetPath, "", uniqueStrings(options)); err != nil {
			return ns.recordLocalDiskRecoveryFailure(session, fmt.Errorf("failed to rebind recovered stage to %s: %w", target.TargetPath, err))
		}
	}
	now := time.Now().UTC()
	session.DevicePath = devicePath
	session.LastRecoveredAt = &now
	session.LastRecoveryError = ""
	session.RecoveryAttempts++
	ns.recordLocalDiskSession(session)
	ns.recordLocalVolumeHealth("recovery", "succeeded")
	ns.recordLocalDiskPVCEvent(session, eventReasonLocalVolumeRecoverySucceeded, fmt.Sprintf("recovered stale local RWO mount for volume %s using %s", session.VolumeID, devicePath))
	return nil
}

func (ns *NodeServer) resolveLocalDiskRecoveryDevice(ctx context.Context, session localDiskSession) (string, error) {
	publishContext := map[string]string{}
	if session.DeviceSerial != "" {
		publishContext[publishContextDeviceSerial] = session.DeviceSerial
	}
	if session.OpenNebulaImageID != "" {
		publishContext[publishContextOpenNebulaImageID] = session.OpenNebulaImageID
	}
	volumeName := session.VolumeName
	if volumeName == "" {
		volumeName = session.DevicePath
	}
	timeout := ns.deviceDiscoveryTimeout(publishContext)
	devicePath, _, err := ns.deviceResolver.Resolve(ctx, session.VolumeID, volumeName, publishContext, timeout)
	if err != nil {
		return "", err
	}
	if session.DeviceSerial != "" && !deviceMatchesSerial(ns.mounter.Exec, devicePath, session.DeviceSerial, "") {
		return "", fmt.Errorf("resolved device %s does not match expected serial %s", devicePath, session.DeviceSerial)
	}
	return devicePath, nil
}

func (ns *NodeServer) recordLocalDiskRecoveryFailure(session localDiskSession, err error) error {
	session.RecoveryAttempts++
	session.LastRecoveryError = err.Error()
	now := time.Now().UTC()
	session.LastRecoveredAt = &now
	ns.recordLocalDiskSession(session)
	ns.recordLocalVolumeHealth("recovery", "failed")
	return err
}

func (ns *NodeServer) loadLocalDiskSession(volumeID string) (localDiskSession, bool, error) {
	if ns == nil || ns.localDiskSessions == nil {
		return localDiskSession{}, false, nil
	}
	return ns.localDiskSessions.Load(volumeID)
}

func (ns *NodeServer) localRWOStaleMountRecoveryEnabled() bool {
	return ns != nil && ns.Driver != nil && ns.Driver.featureGates.LocalRWOStaleMountRecovery
}

func (ns *NodeServer) localRWOActivePodRecoveryEnabled() bool {
	if ns == nil || ns.Driver == nil {
		return false
	}
	enabled, ok := ns.Driver.PluginConfig.GetBool(config.LocalRWOStaleMountActivePodRecoveryVar)
	return ok && enabled
}

func (ns *NodeServer) localRWORecoveryMaxAttempts() int {
	if ns == nil || ns.Driver == nil {
		return 3
	}
	attempts, ok := ns.Driver.PluginConfig.GetInt(config.LocalRWOStaleMountMaxAttemptsVar)
	if !ok || attempts <= 0 {
		return 3
	}
	return attempts
}

func (ns *NodeServer) localRWORecoveryBackoff() time.Duration {
	if ns == nil || ns.Driver == nil {
		return 10 * time.Second
	}
	seconds, ok := ns.Driver.PluginConfig.GetInt(config.LocalRWOStaleMountBackoffSecondsVar)
	if !ok || seconds <= 0 {
		seconds = 10
	}
	return time.Duration(seconds) * time.Second
}

func (ns *NodeServer) recordLocalVolumeHealth(operation, outcome string) {
	if ns != nil && ns.Driver != nil && ns.Driver.metrics != nil {
		ns.Driver.metrics.RecordLocalVolumeHealth(operation, outcome)
	}
}

func (ns *NodeServer) recordLocalDiskPVCEvent(session localDiskSession, reason, message string) {
	if session.PVCNamespace == "" || session.PVCName == "" {
		return
	}
	if ns != nil && ns.Driver != nil && ns.Driver.kubeRuntime != nil {
		ns.Driver.kubeRuntime.EmitPVCEvent(context.Background(), session.PVCNamespace, session.PVCName, reason, message)
	}
}

func (ns *NodeServer) recordLocalDiskPVCWarning(session localDiskSession, reason, message string) {
	if session.PVCNamespace == "" || session.PVCName == "" {
		return
	}
	if ns != nil && ns.Driver != nil && ns.Driver.kubeRuntime != nil {
		ns.Driver.kubeRuntime.EmitWarningEventOnPVC(context.Background(), session.PVCNamespace, session.PVCName, reason, message)
	}
}

func (ns *NodeServer) podUIDExists(ctx context.Context, uid string) bool {
	if ns == nil || ns.Driver == nil || ns.Driver.kubeRuntime == nil {
		return false
	}
	exists, err := ns.Driver.kubeRuntime.PodUIDExists(ctx, uid)
	return err == nil && exists
}

func (ns *NodeServer) mountPointForPath(path string) (mount.MountPoint, bool, error) {
	mountPoints, err := ns.mounter.List()
	if err != nil {
		return mount.MountPoint{}, false, err
	}
	for _, mountPoint := range mountPoints {
		if mountPoint.Path == path {
			return mountPoint, true, nil
		}
	}
	return mount.MountPoint{}, false, nil
}

func isStaleLocalDiskPathError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, unix.EIO) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENXIO) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "input/output error") ||
		strings.Contains(lower, "no such device") ||
		strings.Contains(lower, "transport endpoint is not connected")
}

func podUIDFromKubeletPath(path string) string {
	parts := strings.Split(filepath.Clean(path), string(os.PathSeparator))
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "pods" {
			return parts[i+1]
		}
	}
	return ""
}

func normalizeLocalDiskPublishedTargets(targets []localDiskPublishedTarget) []localDiskPublishedTarget {
	seen := map[string]localDiskPublishedTarget{}
	order := make([]string, 0, len(targets))
	for _, target := range targets {
		target.TargetPath = strings.TrimSpace(target.TargetPath)
		if target.TargetPath == "" {
			continue
		}
		target.MountOptions = uniqueStrings(target.MountOptions)
		if _, ok := seen[target.TargetPath]; !ok {
			order = append(order, target.TargetPath)
		}
		seen[target.TargetPath] = target
	}
	normalized := make([]localDiskPublishedTarget, 0, len(order))
	for _, key := range order {
		normalized = append(normalized, seen[key])
	}
	return normalized
}
