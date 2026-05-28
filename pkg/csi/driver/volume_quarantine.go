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
	"k8s.io/klog/v2"
)

const volumeQuarantineStateConfigMapName = "opennebula-csi-volume-quarantine-state"

type VolumeQuarantineState struct {
	VolumeID        string    `json:"volumeID"`
	Reason          string    `json:"reason"`
	Classification  string    `json:"classification"`
	Message         string    `json:"message"`
	RequestedNode   string    `json:"requestedNode,omitempty"`
	OwnerVMIDs      []int     `json:"ownerVMIDs,omitempty"`
	FailureCount    int       `json:"failureCount"`
	FirstObservedAt time.Time `json:"firstObservedAt"`
	LastObservedAt  time.Time `json:"lastObservedAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

type VolumeQuarantineManager struct {
	mu        sync.RWMutex
	runtime   *KubeRuntime
	namespace string
	entries   map[string]VolumeQuarantineState
}

func NewVolumeQuarantineManager(runtime *KubeRuntime, namespace string) *VolumeQuarantineManager {
	if strings.TrimSpace(namespace) == "" {
		namespace = namespaceFromServiceAccount()
	}
	return &VolumeQuarantineManager{
		runtime:   runtime,
		namespace: namespace,
		entries:   map[string]VolumeQuarantineState{},
	}
}

func (m *VolumeQuarantineManager) LoadFromConfigMap(ctx context.Context) error {
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return nil
	}
	cm, err := m.runtime.GetConfigMap(ctx, m.namespace, volumeQuarantineStateConfigMapName)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	loaded := map[string]VolumeQuarantineState{}
	now := time.Now().UTC()
	for key, raw := range cm.Data {
		var state VolumeQuarantineState
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			klog.Warningf("failed to decode volume quarantine state %s: %v", key, err)
			continue
		}
		if strings.TrimSpace(state.VolumeID) == "" {
			state.VolumeID = key
		}
		if state.ExpiresAt.IsZero() || state.ExpiresAt.After(now) {
			loaded[state.VolumeID] = state
		}
	}
	m.mu.Lock()
	m.entries = loaded
	m.mu.Unlock()
	return nil
}

func (m *VolumeQuarantineManager) GetActive(volumeID string, now time.Time) (VolumeQuarantineState, bool) {
	if m == nil || strings.TrimSpace(volumeID) == "" {
		return VolumeQuarantineState{}, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	m.mu.RLock()
	state, ok := m.entries[volumeID]
	m.mu.RUnlock()
	if !ok || state.ExpiresAt.IsZero() || !state.ExpiresAt.After(now) {
		return VolumeQuarantineState{}, false
	}
	return state, true
}

func (m *VolumeQuarantineManager) MarkFailure(ctx context.Context, state VolumeQuarantineState, threshold int, ttl time.Duration) (VolumeQuarantineState, bool, error) {
	if m == nil || strings.TrimSpace(state.VolumeID) == "" {
		return state, false, nil
	}
	if threshold <= 0 {
		threshold = 2
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	now := time.Now().UTC()
	state.VolumeID = strings.TrimSpace(state.VolumeID)
	state.Reason = strings.TrimSpace(state.Reason)
	state.Classification = strings.TrimSpace(state.Classification)
	state.RequestedNode = strings.TrimSpace(state.RequestedNode)
	sort.Ints(state.OwnerVMIDs)
	state.LastObservedAt = now

	m.mu.Lock()
	previous, hadPrevious := m.entries[state.VolumeID]
	if hadPrevious && sameQuarantineSeries(previous, state) {
		state.FirstObservedAt = previous.FirstObservedAt
		state.FailureCount = previous.FailureCount + 1
	} else {
		state.FirstObservedAt = now
		state.FailureCount = 1
	}
	active := state.FailureCount >= threshold
	if active {
		state.ExpiresAt = now.Add(ttl)
	} else {
		state.ExpiresAt = time.Time{}
	}
	m.entries[state.VolumeID] = state
	m.mu.Unlock()

	if err := m.persistEntry(ctx, state); err != nil {
		return state, active, err
	}
	return state, active, nil
}

func sameQuarantineSeries(previous, next VolumeQuarantineState) bool {
	return strings.TrimSpace(previous.Reason) == strings.TrimSpace(next.Reason) &&
		strings.TrimSpace(previous.Classification) == strings.TrimSpace(next.Classification) &&
		strings.TrimSpace(previous.RequestedNode) == strings.TrimSpace(next.RequestedNode)
}

func (m *VolumeQuarantineManager) Clear(ctx context.Context, volumeID string) error {
	if m == nil || strings.TrimSpace(volumeID) == "" {
		return nil
	}
	volumeID = strings.TrimSpace(volumeID)
	m.mu.Lock()
	delete(m.entries, volumeID)
	m.mu.Unlock()
	if m.runtime == nil || !m.runtime.enabled {
		return nil
	}
	return m.runtime.DeleteConfigMapKey(ctx, m.namespace, volumeQuarantineStateConfigMapName, volumeID)
}

func (m *VolumeQuarantineManager) Snapshot() map[string]VolumeQuarantineState {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := make(map[string]VolumeQuarantineState, len(m.entries))
	for key, state := range m.entries {
		snapshot[key] = state
	}
	return snapshot
}

func (m *VolumeQuarantineManager) persistEntry(ctx context.Context, state VolumeQuarantineState) error {
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return nil
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal volume quarantine state: %w", err)
	}
	return m.runtime.UpsertConfigMapData(ctx, m.namespace, volumeQuarantineStateConfigMapName, map[string]string{
		state.VolumeID: string(payload),
	})
}
