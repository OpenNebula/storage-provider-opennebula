package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/klog/v2"
)

const stickyAttachmentStateConfigMapName = "opennebula-csi-sticky-attachment-state"
const stickyAttachmentWatchResync = 30 * time.Second
const stickyAttachmentWatchTimeout = 10 * time.Minute
const stickyAttachmentWatchRetryWait = 5 * time.Second

type StickyAttachmentState struct {
	VolumeID           string    `json:"volumeID"`
	NodeID             string    `json:"nodeID"`
	Backend            string    `json:"backend"`
	DatastoreID        int       `json:"datastoreID"`
	PVCNamespace       string    `json:"pvcNamespace"`
	PVCName            string    `json:"pvcName"`
	StartedAt          time.Time `json:"startedAt"`
	ExpiresAt          time.Time `json:"expiresAt"`
	GraceSeconds       int       `json:"graceSeconds"`
	Reason             string    `json:"reason"`
	LastKnownNodeReady bool      `json:"lastKnownNodeReady"`
}

type StickyAttachmentManager struct {
	mu        sync.RWMutex
	runtime   *KubeRuntime
	namespace string
	entries   map[string]StickyAttachmentState
}

func NewStickyAttachmentManager(runtime *KubeRuntime, namespace string) *StickyAttachmentManager {
	return &StickyAttachmentManager{
		runtime:   runtime,
		namespace: namespace,
		entries:   map[string]StickyAttachmentState{},
	}
}

func (m *StickyAttachmentManager) StartGrace(state StickyAttachmentState) error {
	if m == nil {
		return nil
	}
	if strings.TrimSpace(state.VolumeID) == "" {
		return fmt.Errorf("volumeID is required")
	}
	m.mu.Lock()
	m.entries[state.VolumeID] = state
	m.mu.Unlock()
	if err := m.persistEntry(context.Background(), state); err != nil {
		m.mu.Lock()
		delete(m.entries, state.VolumeID)
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *StickyAttachmentManager) Get(volumeID string) (StickyAttachmentState, bool) {
	if m == nil {
		return StickyAttachmentState{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	state, ok := m.entries[volumeID]
	return state, ok
}

func (m *StickyAttachmentManager) Clear(volumeID string) error {
	if m == nil || strings.TrimSpace(volumeID) == "" {
		return nil
	}
	m.mu.Lock()
	delete(m.entries, volumeID)
	m.mu.Unlock()
	return m.deleteEntry(context.Background(), volumeID)
}

func (m *StickyAttachmentManager) Update(state StickyAttachmentState) error {
	if m == nil {
		return nil
	}
	if strings.TrimSpace(state.VolumeID) == "" {
		return fmt.Errorf("volumeID is required")
	}
	m.mu.Lock()
	previous, hadPrevious := m.entries[state.VolumeID]
	m.entries[state.VolumeID] = state
	m.mu.Unlock()
	if err := m.persistEntry(context.Background(), state); err != nil {
		m.mu.Lock()
		if hadPrevious {
			m.entries[state.VolumeID] = previous
		} else {
			delete(m.entries, state.VolumeID)
		}
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *StickyAttachmentManager) ListExpired(now time.Time) []StickyAttachmentState {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	expired := make([]StickyAttachmentState, 0)
	for _, entry := range m.entries {
		if !entry.ExpiresAt.After(now) {
			expired = append(expired, entry)
		}
	}
	return expired
}

func (m *StickyAttachmentManager) ListByReason(reason string) []StickyAttachmentState {
	if m == nil {
		return nil
	}
	reason = strings.TrimSpace(reason)
	m.mu.RLock()
	defer m.mu.RUnlock()
	states := make([]StickyAttachmentState, 0)
	for _, entry := range m.entries {
		if strings.TrimSpace(entry.Reason) == reason {
			states = append(states, entry)
		}
	}
	sort.Slice(states, func(i, j int) bool {
		return states[i].VolumeID < states[j].VolumeID
	})
	return states
}

func (m *StickyAttachmentManager) LoadFromConfigMap(ctx context.Context) error {
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return nil
	}
	cm, err := m.runtime.GetConfigMap(ctx, m.namespace, stickyAttachmentStateConfigMapName)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	loaded := map[string]StickyAttachmentState{}
	for key, raw := range cm.Data {
		var state StickyAttachmentState
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			klog.Warningf("failed to decode sticky attachment state %s: %v", key, err)
			continue
		}
		if strings.TrimSpace(state.VolumeID) == "" {
			state.VolumeID = key
		}
		loaded[state.VolumeID] = state
	}

	m.mu.Lock()
	m.entries = loaded
	m.mu.Unlock()
	return nil
}

func (m *StickyAttachmentManager) RunConfigMapSync(ctx context.Context) {
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return
	}
	_ = m.LoadFromConfigMap(ctx)
	for {
		if err := m.watchOnce(ctx); err != nil && ctx.Err() == nil {
			klog.V(2).InfoS("Sticky attachment state watch failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(stickyAttachmentWatchRetryWait):
		}
	}
}

func (m *StickyAttachmentManager) watchOnce(ctx context.Context) error {
	timeout := int64(stickyAttachmentWatchTimeout.Seconds())
	watcher, err := m.runtime.client.CoreV1().ConfigMaps(m.namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:  fields.OneTermEqualSelector("metadata.name", stickyAttachmentStateConfigMapName).String(),
		TimeoutSeconds: &timeout,
	})
	if err != nil {
		return err
	}
	defer watcher.Stop()

	ticker := time.NewTicker(stickyAttachmentWatchResync)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-watcher.ResultChan():
			if !ok {
				return nil
			}
			_ = m.LoadFromConfigMap(ctx)
		case <-ticker.C:
			_ = m.LoadFromConfigMap(ctx)
		}
	}
}

func (m *StickyAttachmentManager) persistEntry(ctx context.Context, state StickyAttachmentState) error {
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return nil
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return m.runtime.UpsertConfigMapData(ctx, m.namespace, stickyAttachmentStateConfigMapName, map[string]string{
		state.VolumeID: string(payload),
	})
}

func (m *StickyAttachmentManager) deleteEntry(ctx context.Context, volumeID string) error {
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return nil
	}
	return m.runtime.DeleteConfigMapKey(ctx, m.namespace, stickyAttachmentStateConfigMapName, volumeID)
}
