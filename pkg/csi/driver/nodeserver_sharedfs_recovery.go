package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OpenNebula/one/src/oca/go/src/goca"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"
)

const (
	sharedPublishContextCephFSMounter = "cephfsMounter"

	sharedCephFSSecretFile = "ceph.secret"
	sharedCephFSConfFile   = "ceph.conf"

	defaultSharedFilesystemSessionDir           = "/var/lib/kubelet/plugins/csi.opennebula.io/cephfs-sessions"
	defaultSharedFilesystemRecoveryStartupDelay = 10 * time.Second
	defaultSharedFilesystemRecoveryInterval     = 2 * time.Minute
)

var (
	sharedFilesystemSessionRootPath      = defaultSharedFilesystemSessionDir
	sharedFilesystemRecoveryStartupDelay = defaultSharedFilesystemRecoveryStartupDelay
	sharedFilesystemRecoveryInterval     = defaultSharedFilesystemRecoveryInterval
	sharedFilesystemProcFilesystemsPath  = "/proc/filesystems"
)

type sharedFilesystemMounter string

const (
	sharedFilesystemMounterFuse   sharedFilesystemMounter = "fuse"
	sharedFilesystemMounterKernel sharedFilesystemMounter = "kernel"
)

type sharedFilesystemPublishedTarget struct {
	TargetPath   string   `json:"targetPath"`
	MountOptions []string `json:"mountOptions,omitempty"`
}

type sharedFilesystemSession struct {
	VolumeID          string                            `json:"volumeID"`
	Mounter           sharedFilesystemMounter           `json:"mounter"`
	UserID            string                            `json:"userID"`
	StagingTargetPath string                            `json:"stagingTargetPath"`
	Monitors          []string                          `json:"monitors"`
	FSName            string                            `json:"fsName"`
	Subpath           string                            `json:"subpath"`
	StageMountOptions []string                          `json:"stageMountOptions,omitempty"`
	KeyringPath       string                            `json:"keyringPath,omitempty"`
	SecretFilePath    string                            `json:"secretFilePath,omitempty"`
	PublishedTargets  []sharedFilesystemPublishedTarget `json:"publishedTargets,omitempty"`
}

type sharedFilesystemSessionHealth struct {
	RecoverStage    bool
	Reason          string
	TargetsToRebind []sharedFilesystemPublishedTarget
}

type sharedFilesystemSessionStore struct {
	root string
	mu   sync.Mutex
}

type sharedFilesystemRecoveryManager struct {
	ns           *NodeServer
	store        *sharedFilesystemSessionStore
	queue        chan string
	mu           sync.Mutex
	queued       map[string]struct{}
	lastFailures map[string]time.Time
}

func newSharedFilesystemRecoveryManager(ns *NodeServer) *sharedFilesystemRecoveryManager {
	if ns == nil || ns.Driver == nil {
		return nil
	}
	return &sharedFilesystemRecoveryManager{
		ns:           ns,
		store:        newSharedFilesystemSessionStore(sharedFilesystemSessionRootPath),
		queue:        make(chan string, 128),
		queued:       make(map[string]struct{}),
		lastFailures: make(map[string]time.Time),
	}
}

func newSharedFilesystemSessionStore(root string) *sharedFilesystemSessionStore {
	root = strings.TrimSpace(root)
	if root == "" {
		root = defaultSharedFilesystemSessionDir
	}
	return &sharedFilesystemSessionStore{root: root}
}

func (s *sharedFilesystemSessionStore) Save(session sharedFilesystemSession) error {
	if strings.TrimSpace(session.VolumeID) == "" {
		return fmt.Errorf("shared filesystem session is missing volume ID")
	}
	session.StageMountOptions = uniqueStrings(session.StageMountOptions)
	session.PublishedTargets = normalizeSharedFilesystemPublishedTargets(session.PublishedTargets)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	targetPath := s.pathForVolume(session.VolumeID)
	tmpPath := targetPath + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, targetPath)
}

func (s *sharedFilesystemSessionStore) Load(volumeID string) (sharedFilesystemSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(volumeID)
}

func (s *sharedFilesystemSessionStore) loadLocked(volumeID string) (sharedFilesystemSession, bool, error) {
	payload, err := os.ReadFile(s.pathForVolume(volumeID))
	if err != nil {
		if os.IsNotExist(err) {
			return sharedFilesystemSession{}, false, nil
		}
		return sharedFilesystemSession{}, false, err
	}
	var session sharedFilesystemSession
	if err := json.Unmarshal(payload, &session); err != nil {
		return sharedFilesystemSession{}, false, err
	}
	session.StageMountOptions = uniqueStrings(session.StageMountOptions)
	session.PublishedTargets = normalizeSharedFilesystemPublishedTargets(session.PublishedTargets)
	if session.Mounter == "" {
		session.Mounter = sharedFilesystemMounterFuse
	}
	return session, true, nil
}

func (s *sharedFilesystemSessionStore) Delete(volumeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.pathForVolume(volumeID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *sharedFilesystemSessionStore) List() ([]sharedFilesystemSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	sessions := make([]sharedFilesystemSession, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(s.root, entry.Name()))
		if err != nil {
			return nil, err
		}
		var session sharedFilesystemSession
		if err := json.Unmarshal(payload, &session); err != nil {
			return nil, err
		}
		session.StageMountOptions = uniqueStrings(session.StageMountOptions)
		session.PublishedTargets = normalizeSharedFilesystemPublishedTargets(session.PublishedTargets)
		if session.Mounter == "" {
			session.Mounter = sharedFilesystemMounterFuse
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].VolumeID < sessions[j].VolumeID
	})
	return sessions, nil
}

func (s *sharedFilesystemSessionStore) pathForVolume(volumeID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(volumeID)))
	return filepath.Join(s.root, hex.EncodeToString(sum[:])+".json")
}

func (m *sharedFilesystemRecoveryManager) Start(ctx context.Context) {
	if m == nil || m.ns == nil || m.ns.Driver == nil || !m.ns.Driver.featureGates.CephFSPersistentRecovery {
		return
	}
	go m.run(ctx)
}

func (m *sharedFilesystemRecoveryManager) run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(sharedFilesystemRecoveryStartupDelay):
	}

	m.seedSessionsFromMounts(ctx)
	m.enqueueUnhealthySessions(ctx, "startup")

	ticker := time.NewTicker(sharedFilesystemRecoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case volumeID := <-m.queue:
			m.recoverQueuedVolume(ctx, volumeID)
		case <-ticker.C:
			m.seedSessionsFromMounts(ctx)
			m.enqueueUnhealthySessions(ctx, "periodic")
		}
	}
}

func (m *sharedFilesystemRecoveryManager) enqueue(volumeID, reason string) {
	if m == nil || strings.TrimSpace(volumeID) == "" {
		return
	}

	m.mu.Lock()
	if _, ok := m.queued[volumeID]; ok {
		m.mu.Unlock()
		return
	}
	m.queued[volumeID] = struct{}{}
	m.mu.Unlock()

	select {
	case m.queue <- volumeID:
		klog.V(4).InfoS("Queued shared filesystem recovery", "volumeID", volumeID, "reason", reason)
	default:
		m.mu.Lock()
		delete(m.queued, volumeID)
		m.mu.Unlock()
		klog.V(2).InfoS("Shared filesystem recovery queue is full", "volumeID", volumeID, "reason", reason)
	}
}

func (m *sharedFilesystemRecoveryManager) recoverQueuedVolume(ctx context.Context, volumeID string) {
	defer func() {
		m.mu.Lock()
		delete(m.queued, volumeID)
		m.mu.Unlock()
	}()

	if err := m.recoverVolume(ctx, volumeID); err != nil {
		m.mu.Lock()
		lastFailure := m.lastFailures[volumeID]
		if time.Since(lastFailure) > 10*time.Minute {
			klog.ErrorS(err, "Shared filesystem recovery failed", "volumeID", volumeID)
			m.lastFailures[volumeID] = time.Now()
		} else {
			klog.V(3).InfoS("Shared filesystem recovery still failing", "volumeID", volumeID, "err", err)
		}
		m.mu.Unlock()
	}
}

func (m *sharedFilesystemRecoveryManager) enqueueUnhealthySessions(ctx context.Context, reason string) {
	sessions, err := m.store.List()
	if err != nil {
		klog.ErrorS(err, "Failed to list shared filesystem sessions for recovery")
		return
	}
	for _, session := range sessions {
		if m.garbageCollectOrphanedSession(ctx, session) {
			continue
		}
		health, err := m.ns.evaluateSharedFilesystemSession(session)
		if err != nil {
			klog.ErrorS(err, "Failed to evaluate shared filesystem session health", "volumeID", session.VolumeID)
			continue
		}
		if health.RecoverStage || len(health.TargetsToRebind) > 0 {
			m.ns.Driver.metrics.RecordCephFSSubvolume("recovery_detected_"+string(session.Mounter), "observed")
			m.enqueue(session.VolumeID, reason)
		}
	}

	m.seedSessionsFromMounts(ctx)
}

func (m *sharedFilesystemRecoveryManager) garbageCollectOrphanedSession(ctx context.Context, session sharedFilesystemSession) bool {
	if m == nil || m.ns == nil || strings.TrimSpace(session.VolumeID) == "" || len(session.PublishedTargets) == 0 {
		return false
	}
	allPodsGone := true
	for _, target := range session.PublishedTargets {
		podUID := podUIDFromKubeletPath(target.TargetPath)
		if podUID == "" {
			allPodsGone = false
			break
		}
		if m.ns.podUIDExists(ctx, podUID) {
			allPodsGone = false
			break
		}
	}
	if !allPodsGone {
		return false
	}
	if m.ns.Driver != nil && m.ns.Driver.kubeRuntime != nil {
		if _, err := m.ns.Driver.kubeRuntime.ResolveVolumeRuntimeContext(ctx, session.VolumeID); err == nil {
			return false
		}
	}
	for _, target := range session.PublishedTargets {
		if mp, mounted, err := m.ns.mountPointForPath(target.TargetPath); err == nil && mounted {
			if _, statErr := nodeVolumePathStat(target.TargetPath); statErr == nil {
				klog.V(2).InfoS("Skipping CephFS session GC because target still appears mounted", "volumeID", session.VolumeID, "target", target.TargetPath, "source", mp.Device)
				return false
			}
		}
		_ = mount.CleanupMountPoint(target.TargetPath, m.ns.mounter.Interface, true)
		_ = os.RemoveAll(target.TargetPath)
	}
	if err := m.store.Delete(session.VolumeID); err != nil {
		klog.ErrorS(err, "Failed to delete orphaned CephFS session", "volumeID", session.VolumeID)
		return false
	}
	if m.ns.Driver != nil && m.ns.Driver.metrics != nil {
		m.ns.Driver.metrics.RecordCephFSSubvolume("session_gc", "succeeded")
	}
	klog.InfoS("Garbage collected orphaned CephFS session", "volumeID", session.VolumeID)
	return true
}

func (m *sharedFilesystemRecoveryManager) recoverVolume(ctx context.Context, volumeID string) error {
	if m == nil || m.ns == nil {
		return nil
	}
	session, exists, err := m.store.Load(volumeID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	release := m.ns.acquireSharedFilesystemOperationLock(volumeID)
	defer release()

	session, exists, err = m.store.Load(volumeID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if m.garbageCollectOrphanedSession(ctx, session) {
		return nil
	}

	health, err := m.ns.evaluateSharedFilesystemSession(session)
	if err != nil {
		return err
	}
	if !health.RecoverStage && len(health.TargetsToRebind) == 0 {
		return nil
	}

	m.ns.Driver.metrics.RecordCephFSSubvolume("recovery_attempt_"+string(session.Mounter), "attempted")

	if health.RecoverStage {
		for _, target := range session.PublishedTargets {
			if err := mount.CleanupMountPoint(target.TargetPath, m.ns.mounter.Interface, true); err != nil {
				return fmt.Errorf("failed to cleanup stale target %s before stage recovery: %w", target.TargetPath, err)
			}
		}
		if err := mountCleanup(session.StagingTargetPath, m.ns); err != nil {
			return status.Errorf(codes.Internal, "failed to cleanup stale shared filesystem stage %s: %v", session.StagingTargetPath, err)
		}
		if err := m.ns.mountSharedFilesystemSession(session); err != nil {
			m.ns.Driver.metrics.RecordCephFSSubvolume("recovery_"+string(session.Mounter), "failed")
			return err
		}
		for _, target := range session.PublishedTargets {
			if err := m.ns.publishSharedFilesystemTarget(session.StagingTargetPath, target); err != nil {
				m.ns.Driver.metrics.RecordCephFSSubvolume("recovery_"+string(session.Mounter), "failed")
				return err
			}
		}
		m.ns.Driver.metrics.RecordCephFSSubvolume("recovery_"+string(session.Mounter), "succeeded")
		return nil
	}

	for _, target := range health.TargetsToRebind {
		if err := mount.CleanupMountPoint(target.TargetPath, m.ns.mounter.Interface, true); err != nil {
			return fmt.Errorf("failed to cleanup shared filesystem target %s before rebinding: %w", target.TargetPath, err)
		}
		if err := m.ns.publishSharedFilesystemTarget(session.StagingTargetPath, target); err != nil {
			m.ns.Driver.metrics.RecordCephFSSubvolume("recovery_"+string(session.Mounter), "failed")
			return err
		}
	}

	m.ns.Driver.metrics.RecordCephFSSubvolume("recovery_"+string(session.Mounter), "succeeded")
	return nil
}

func (m *sharedFilesystemRecoveryManager) seedSessionsFromMounts(ctx context.Context) {
	if m == nil || m.ns == nil {
		return
	}

	mountPoints, err := m.ns.mounter.List()
	if err != nil {
		klog.ErrorS(err, "Failed to list mount points while seeding shared filesystem sessions")
		return
	}

	datastoreCache := make(map[int]opennebula.Datastore)
	for _, mountPoint := range mountPoints {
		volumeID := sharedFilesystemVolumeIDFromStagingPath(mountPoint.Path)
		if volumeID == "" {
			continue
		}

		session, exists, err := m.store.Load(volumeID)
		if err != nil {
			klog.ErrorS(err, "Failed to load shared filesystem session during seed", "volumeID", volumeID)
			continue
		}
		if !exists {
			session, err = m.ns.reconstructSharedFilesystemSessionFromMount(ctx, mountPoint, mountPoints, datastoreCache)
			if err != nil {
				klog.V(2).ErrorS(err, "Failed to reconstruct shared filesystem session from mount state", "stagePath", mountPoint.Path)
				continue
			}
		}

		session.StagingTargetPath = mountPoint.Path
		session.Mounter = sharedFilesystemMounterFromMountType(mountPoint.Type)
		if session.KeyringPath == "" {
			session.KeyringPath = sharedCephFSKeyringPath(mountPoint.Path)
		}
		if session.SecretFilePath == "" {
			session.SecretFilePath = sharedCephFSSecretPath(mountPoint.Path)
		}
		if session.UserID == "" {
			session.UserID = sharedFilesystemUserIDFromKeyring(session.KeyringPath)
		}
		session.StageMountOptions = uniqueStrings(append(session.StageMountOptions, sharedFilesystemMountOptionsFromMountPoint(mountPoint)...))
		session.PublishedTargets = sharedFilesystemPublishedTargetsFromMounts(mountPoints, mountPoint)
		if err := m.store.Save(session); err != nil {
			klog.ErrorS(err, "Failed to persist seeded shared filesystem session", "volumeID", volumeID)
		}
	}
}

func (ns *NodeServer) evaluateSharedFilesystemSession(session sharedFilesystemSession) (sharedFilesystemSessionHealth, error) {
	health := sharedFilesystemSessionHealth{}
	if strings.TrimSpace(session.StagingTargetPath) == "" {
		return health, nil
	}

	stageCheck, err := ns.checkMountPoint("", session.StagingTargetPath, nil)
	if err != nil {
		return health, err
	}
	if !stageCheck.targetIsMountPoint {
		health.RecoverStage = true
		health.Reason = "stage_missing"
		return health, nil
	}
	if err := detectStaleSharedFilesystemMount(session.StagingTargetPath); err != nil {
		health.RecoverStage = true
		health.Reason = "stage_stale"
		return health, nil
	}

	for _, target := range session.PublishedTargets {
		targetCheck, err := ns.checkMountPoint(session.StagingTargetPath, target.TargetPath, nil)
		if err != nil {
			return health, err
		}
		if !targetCheck.targetIsMountPoint {
			health.TargetsToRebind = append(health.TargetsToRebind, target)
			continue
		}
		if _, err := nodeVolumePathStat(target.TargetPath); err != nil && isDisconnectedSharedFilesystemError(err) {
			health.TargetsToRebind = append(health.TargetsToRebind, target)
		}
	}
	health.TargetsToRebind = normalizeSharedFilesystemPublishedTargets(health.TargetsToRebind)
	return health, nil
}

func (ns *NodeServer) reconstructSharedFilesystemSessionFromMount(ctx context.Context, stageMount mount.MountPoint, mountPoints []mount.MountPoint, datastoreCache map[int]opennebula.Datastore) (sharedFilesystemSession, error) {
	volumeID := sharedFilesystemVolumeIDFromStagingPath(stageMount.Path)
	if volumeID == "" {
		return sharedFilesystemSession{}, fmt.Errorf("stage path %s does not map to a shared filesystem volume", stageMount.Path)
	}

	metadata, err := opennebula.DecodeSharedVolumeID(volumeID)
	if err != nil {
		return sharedFilesystemSession{}, err
	}

	datastore, ok := datastoreCache[metadata.DatastoreID]
	if !ok {
		datastore, err = ns.resolveSharedFilesystemDatastore(ctx, metadata.DatastoreID)
		if err != nil {
			return sharedFilesystemSession{}, err
		}
		datastoreCache[metadata.DatastoreID] = datastore
	}

	session := sharedFilesystemSession{
		VolumeID:          volumeID,
		Mounter:           sharedFilesystemMounterFromMountType(stageMount.Type),
		UserID:            sharedFilesystemUserIDFromKeyring(sharedCephFSKeyringPath(stageMount.Path)),
		StagingTargetPath: stageMount.Path,
		Monitors:          append([]string(nil), datastore.CephFS.Monitors...),
		FSName:            metadata.FSName,
		Subpath:           metadata.Subpath,
		StageMountOptions: uniqueStrings(append(append([]string(nil), datastore.CephFS.MountOptions...), sharedFilesystemMountOptionsFromMountPoint(stageMount)...)),
		KeyringPath:       sharedCephFSKeyringPath(stageMount.Path),
		SecretFilePath:    sharedCephFSSecretPath(stageMount.Path),
		PublishedTargets:  sharedFilesystemPublishedTargetsFromMounts(mountPoints, stageMount),
	}
	return session, nil
}

func (ns *NodeServer) resolveSharedFilesystemDatastore(ctx context.Context, datastoreID int) (opennebula.Datastore, error) {
	endpoint, ok := ns.Driver.PluginConfig.GetString(config.OpenNebulaRPCEndpointVar)
	if !ok || strings.TrimSpace(endpoint) == "" {
		return opennebula.Datastore{}, fmt.Errorf("ONE_XMLRPC is not configured")
	}
	credentials, ok := ns.Driver.PluginConfig.GetString(config.OpenNebulaCredentialsVar)
	if !ok || strings.TrimSpace(credentials) == "" {
		return opennebula.Datastore{}, fmt.Errorf("ONE_AUTH is not configured")
	}

	client := opennebula.NewClient(opennebula.OpenNebulaConfig{
		Endpoint:    endpoint,
		Credentials: credentials,
	})
	pool, err := goca.NewController(client.Client).Datastores().InfoContext(ctx)
	if err != nil {
		return opennebula.Datastore{}, err
	}

	selection := opennebula.DatastoreSelectionConfig{
		Identifiers:  []string{fmt.Sprintf("%d", datastoreID)},
		AllowedTypes: []string{"cephfs"},
	}
	resolved, err := opennebula.ResolveDatastores(pool.Datastores, selection)
	if err != nil {
		return opennebula.Datastore{}, err
	}
	if len(resolved) == 0 {
		return opennebula.Datastore{}, fmt.Errorf("datastore %d was not resolved", datastoreID)
	}
	return resolved[0], nil
}

func (ns *NodeServer) acquireSharedFilesystemOperationLock(volumeID string) func() {
	if ns == nil || ns.Driver == nil || ns.Driver.operationLocks == nil {
		return func() {}
	}
	return ns.Driver.operationLocks.Acquire("sharedfs:" + strings.TrimSpace(volumeID))
}

func (ns *NodeServer) handleDisconnectedSharedFilesystemPath(volumeID, volumePath string, err error) error {
	if ns != nil && ns.Driver != nil {
		ns.Driver.metrics.RecordCephFSSubvolume("stale_mount_detected", "failure")
		if ns.sharedFilesystemRecovery != nil {
			ns.sharedFilesystemRecovery.enqueue(volumeID, "volume_stats")
		}
	}
	return status.Errorf(codes.FailedPrecondition, "stale CephFS mount detected at %s: %v; restage the volume to recover", volumePath, err)
}

func (ns *NodeServer) recordSharedFilesystemSession(session sharedFilesystemSession) {
	if ns == nil || ns.sharedFilesystemRecovery == nil {
		return
	}
	if err := ns.sharedFilesystemRecovery.store.Save(session); err != nil {
		klog.ErrorS(err, "Failed to persist shared filesystem session", "volumeID", session.VolumeID)
	}
}

func (ns *NodeServer) deleteSharedFilesystemSession(volumeID string) {
	if ns == nil || ns.sharedFilesystemRecovery == nil {
		return
	}
	if err := ns.sharedFilesystemRecovery.store.Delete(volumeID); err != nil {
		klog.ErrorS(err, "Failed to delete shared filesystem session", "volumeID", volumeID)
	}
}

func (ns *NodeServer) updateSharedFilesystemPublishedTarget(volumeID string, target sharedFilesystemPublishedTarget) {
	if ns == nil || ns.sharedFilesystemRecovery == nil || strings.TrimSpace(volumeID) == "" {
		return
	}
	session, exists, err := ns.sharedFilesystemRecovery.store.Load(volumeID)
	if err != nil || !exists {
		if err != nil {
			klog.ErrorS(err, "Failed to load shared filesystem session for publish update", "volumeID", volumeID)
		}
		return
	}

	targets := make([]sharedFilesystemPublishedTarget, 0, len(session.PublishedTargets)+1)
	replaced := false
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
	ns.recordSharedFilesystemSession(session)
}

func (ns *NodeServer) removeSharedFilesystemPublishedTarget(volumeID, targetPath string) {
	if ns == nil || ns.sharedFilesystemRecovery == nil || strings.TrimSpace(volumeID) == "" {
		return
	}
	session, exists, err := ns.sharedFilesystemRecovery.store.Load(volumeID)
	if err != nil || !exists {
		if err != nil {
			klog.ErrorS(err, "Failed to load shared filesystem session for target removal", "volumeID", volumeID)
		}
		return
	}

	filtered := make([]sharedFilesystemPublishedTarget, 0, len(session.PublishedTargets))
	for _, target := range session.PublishedTargets {
		if target.TargetPath == targetPath {
			continue
		}
		filtered = append(filtered, target)
	}
	session.PublishedTargets = filtered
	if len(filtered) == 0 && strings.TrimSpace(session.StagingTargetPath) == "" {
		ns.deleteSharedFilesystemSession(volumeID)
		return
	}
	ns.recordSharedFilesystemSession(session)
}

func (ns *NodeServer) cleanupOrphanedSharedFilesystemTargetOnUnpublish(ctx context.Context, volumeID, targetPath string, cause error) bool {
	if ns == nil || ns.sharedFilesystemRecovery == nil {
		return false
	}
	podUID := podUIDFromKubeletPath(targetPath)
	if podUID == "" || ns.podUIDExists(ctx, podUID) {
		return false
	}
	if mountPoint, mounted, err := ns.mountPointForPath(targetPath); err == nil && mounted {
		if _, statErr := nodeVolumePathStat(targetPath); statErr == nil {
			klog.V(2).InfoS("Skipping orphaned CephFS target cleanup because target still appears usable", "volumeID", volumeID, "targetPath", targetPath, "source", mountPoint.Device)
			return false
		}
	}
	ns.removeSharedFilesystemPublishedTarget(volumeID, targetPath)
	_ = os.RemoveAll(targetPath)
	if ns.Driver != nil && ns.Driver.metrics != nil {
		ns.Driver.metrics.RecordCephFSSubvolume("target_gc", "succeeded")
	}
	klog.InfoS("Cleaned orphaned CephFS target during unpublish", "volumeID", volumeID, "targetPath", targetPath, "cause", cause)
	return true
}

func normalizeSharedFilesystemPublishedTargets(targets []sharedFilesystemPublishedTarget) []sharedFilesystemPublishedTarget {
	if len(targets) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(targets))
	normalized := make([]sharedFilesystemPublishedTarget, 0, len(targets))
	for _, target := range targets {
		target.TargetPath = strings.TrimSpace(target.TargetPath)
		if target.TargetPath == "" {
			continue
		}
		if _, ok := seen[target.TargetPath]; ok {
			continue
		}
		target.MountOptions = uniqueStrings(target.MountOptions)
		seen[target.TargetPath] = struct{}{}
		normalized = append(normalized, target)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].TargetPath < normalized[j].TargetPath
	})
	return normalized
}

func sharedFilesystemPublishedTargetsFromMounts(mountPoints []mount.MountPoint, stageMount mount.MountPoint) []sharedFilesystemPublishedTarget {
	targets := make([]sharedFilesystemPublishedTarget, 0)
	for _, mountPoint := range mountPoints {
		if mountPoint.Path == stageMount.Path {
			continue
		}
		if mountPoint.Device != stageMount.Path && mountPoint.Device != stageMount.Device {
			continue
		}
		targets = append(targets, sharedFilesystemPublishedTarget{
			TargetPath:   mountPoint.Path,
			MountOptions: sharedFilesystemMountOptionsFromMountPoint(mountPoint),
		})
	}
	return normalizeSharedFilesystemPublishedTargets(targets)
}

func sharedFilesystemMountOptionsFromMountPoint(mountPoint mount.MountPoint) []string {
	options := make([]string, 0, len(mountPoint.Opts))
	for _, option := range mountPoint.Opts {
		trimmed := strings.TrimSpace(option)
		switch trimmed {
		case "", "bind", "rw", "defaults":
			continue
		default:
			options = append(options, trimmed)
		}
	}
	return uniqueStrings(options)
}

func sharedFilesystemVolumeIDFromStagingPath(stagingTargetPath string) string {
	cleaned := filepath.Clean(stagingTargetPath)
	if filepath.Base(cleaned) != "globalmount" {
		return ""
	}
	volumeID := filepath.Base(filepath.Dir(cleaned))
	if !opennebula.IsSharedFilesystemVolumeID(volumeID) {
		return ""
	}
	return volumeID
}

func normalizeSharedFilesystemMounter(raw string) (sharedFilesystemMounter, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(sharedFilesystemMounterFuse):
		return sharedFilesystemMounterFuse, nil
	case string(sharedFilesystemMounterKernel):
		return sharedFilesystemMounterKernel, nil
	default:
		return "", fmt.Errorf("unsupported CephFS mounter %q", raw)
	}
}

func sharedFilesystemMounterFromMountType(fsType string) sharedFilesystemMounter {
	switch strings.ToLower(strings.TrimSpace(fsType)) {
	case "ceph":
		return sharedFilesystemMounterKernel
	default:
		return sharedFilesystemMounterFuse
	}
}

func sharedFilesystemStageMountOptions(req *csi.NodeStageVolumeRequest) []string {
	publishContext := req.GetPublishContext()
	options := splitCSV(publishContext[sharedPublishContextCephFSMounts])
	if capability := req.GetVolumeCapability(); capability != nil && capability.GetMount() != nil {
		options = append(options, capability.GetMount().GetMountFlags()...)
	}
	if strings.EqualFold(strings.TrimSpace(publishContext[sharedPublishContextCephFSReadonly]), "true") {
		options = append(options, "ro")
	}
	return uniqueStrings(options)
}

func sharedFilesystemBindMountOptions(req *csi.NodePublishVolumeRequest) []string {
	options := make([]string, 0, 4)
	if req.GetReadonly() {
		options = append(options, "ro")
	}
	if capability := req.GetVolumeCapability(); capability != nil && capability.GetMount() != nil {
		options = append(options, capability.GetMount().GetMountFlags()...)
	}
	return uniqueStrings(options)
}

func sharedFilesystemSessionFromStageRequest(req *csi.NodeStageVolumeRequest) (sharedFilesystemSession, error) {
	mounterName, err := normalizeSharedFilesystemMounter(firstNonEmpty(
		req.GetPublishContext()[sharedPublishContextCephFSMounter],
		req.GetVolumeContext()[storageClassParamCephFSMounter],
	))
	if err != nil {
		return sharedFilesystemSession{}, status.Error(codes.InvalidArgument, err.Error())
	}

	publishContext := req.GetPublishContext()
	monitors := splitCSV(publishContext[sharedPublishContextCephFSMonitors])
	fsName := strings.TrimSpace(publishContext[sharedPublishContextCephFSFSName])
	subpath := strings.TrimSpace(publishContext[sharedPublishContextCephFSSubpath])
	if len(monitors) == 0 || fsName == "" || subpath == "" {
		return sharedFilesystemSession{}, status.Error(codes.InvalidArgument, "CephFS publish context is incomplete")
	}

	secrets := req.GetSecrets()
	userID := strings.TrimSpace(secrets[sharedNodeStageSecretUserIDKey])
	if userID == "" {
		return sharedFilesystemSession{}, status.Errorf(codes.InvalidArgument, "CephFS node staging requires secret key %q", sharedNodeStageSecretUserIDKey)
	}

	stagingTargetPath := req.GetStagingTargetPath()
	return sharedFilesystemSession{
		VolumeID:          req.GetVolumeId(),
		Mounter:           mounterName,
		UserID:            userID,
		StagingTargetPath: stagingTargetPath,
		Monitors:          monitors,
		FSName:            fsName,
		Subpath:           subpath,
		StageMountOptions: sharedFilesystemStageMountOptions(req),
		KeyringPath:       sharedCephFSKeyringPath(stagingTargetPath),
		SecretFilePath:    sharedCephFSSecretPath(stagingTargetPath),
	}, nil
}

func sharedFilesystemTargetFromPublishRequest(req *csi.NodePublishVolumeRequest) sharedFilesystemPublishedTarget {
	return sharedFilesystemPublishedTarget{
		TargetPath:   req.GetTargetPath(),
		MountOptions: sharedFilesystemBindMountOptions(req),
	}
}

func sharedCephFSKeyringPath(stagingTargetPath string) string {
	return filepath.Join(sharedCephFSKeyringDir(stagingTargetPath), sharedCephFSKeyringFile)
}

func sharedCephFSSecretPath(stagingTargetPath string) string {
	return filepath.Join(sharedCephFSKeyringDir(stagingTargetPath), sharedCephFSSecretFile)
}

func sharedCephFSConfPath(stagingTargetPath string) string {
	return filepath.Join(sharedCephFSKeyringDir(stagingTargetPath), sharedCephFSConfFile)
}

func ensureSharedFilesystemCephConf(stagingTargetPath string) (string, error) {
	keyringDir := sharedCephFSKeyringDir(stagingTargetPath)
	if err := os.MkdirAll(keyringDir, 0o700); err != nil {
		return "", status.Errorf(codes.Internal, "failed to create CephFS config directory: %v", err)
	}
	confPath := sharedCephFSConfPath(stagingTargetPath)
	if _, err := os.Stat(confPath); err == nil {
		return confPath, nil
	}
	contents := []byte("[global]\n")
	if err := os.WriteFile(confPath, contents, 0o600); err != nil {
		return "", status.Errorf(codes.Internal, "failed to write CephFS config file: %v", err)
	}
	return confPath, nil
}

func ensureSharedFilesystemCredentials(stagingTargetPath, userID, userKey string) (string, string, error) {
	keyringDir := sharedCephFSKeyringDir(stagingTargetPath)
	if err := os.MkdirAll(keyringDir, 0o700); err != nil {
		return "", "", status.Errorf(codes.Internal, "failed to create CephFS credential directory: %v", err)
	}

	keyringPath := sharedCephFSKeyringPath(stagingTargetPath)
	keyringContents := []byte("[client." + userID + "]\n\tkey = " + userKey + "\n")
	if err := os.WriteFile(keyringPath, keyringContents, 0o600); err != nil {
		return "", "", status.Errorf(codes.Internal, "failed to write CephFS keyring file: %v", err)
	}

	secretPath := sharedCephFSSecretPath(stagingTargetPath)
	if err := os.WriteFile(secretPath, []byte(userKey+"\n"), 0o600); err != nil {
		return "", "", status.Errorf(codes.Internal, "failed to write CephFS secret file: %v", err)
	}

	return keyringPath, secretPath, nil
}

func sharedFilesystemUserIDFromKeyring(keyringPath string) string {
	payload, err := os.ReadFile(keyringPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[client.") && strings.HasSuffix(line, "]") {
			return strings.TrimSuffix(strings.TrimPrefix(line, "[client."), "]")
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func sharedFilesystemKernelSupported() error {
	supported, err := hostSupportsCephKernelClient(sharedFilesystemProcFilesystemsPath)
	if err != nil {
		return err
	}
	if !supported {
		return fmt.Errorf("host kernel does not expose CephFS client support in %s; on Omni/Talos this must come from the node image or system extensions", sharedFilesystemProcFilesystemsPath)
	}
	return nil
}

func hostSupportsCephKernelClient(procFilesystemsPath string) (bool, error) {
	payload, err := os.ReadFile(procFilesystemsPath)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		if fields[len(fields)-1] == "ceph" {
			return true, nil
		}
	}
	return false, nil
}

func sharedFilesystemKernelMonitorOption(monitors []string) string {
	if len(monitors) == 0 {
		return ""
	}
	return strings.Join(uniqueStrings(monitors), "/")
}
