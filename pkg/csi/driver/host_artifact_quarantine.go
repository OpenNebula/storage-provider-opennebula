package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
)

const (
	hostArtifactStateConfigMapName = "opennebula-csi-host-artifact-state"
	hostArtifactReason             = "external_host_artifact_repair_required"
)

type HostArtifactQuarantineState struct {
	Key                 string    `json:"key"`
	Classification      string    `json:"classification"`
	Reason              string    `json:"reason"`
	Message             string    `json:"message"`
	VolumeID            string    `json:"volumeID,omitempty"`
	ImageID             int       `json:"imageID,omitempty"`
	NodeName            string    `json:"nodeName,omitempty"`
	VMID                int       `json:"vmID,omitempty"`
	HostID              int       `json:"hostID,omitempty"`
	HostName            string    `json:"hostName,omitempty"`
	SystemDatastoreID   int       `json:"systemDatastoreID,omitempty"`
	SystemDatastoreName string    `json:"systemDatastoreName,omitempty"`
	SystemDatastoreTM   string    `json:"systemDatastoreTM,omitempty"`
	DiskID              int       `json:"diskID,omitempty"`
	Target              string    `json:"target,omitempty"`
	LVName              string    `json:"lvName,omitempty"`
	FailureCount        int       `json:"failureCount"`
	FirstObservedAt     time.Time `json:"firstObservedAt"`
	LastObservedAt      time.Time `json:"lastObservedAt"`
	ExpiresAt           time.Time `json:"expiresAt"`
	RepairHint          string    `json:"repairHint,omitempty"`
}

type HostArtifactQuarantineManager struct {
	mu        sync.RWMutex
	runtime   *KubeRuntime
	namespace string
	entries   map[string]HostArtifactQuarantineState
}

func NewHostArtifactQuarantineManager(runtime *KubeRuntime, namespace string) *HostArtifactQuarantineManager {
	if strings.TrimSpace(namespace) == "" {
		namespace = namespaceFromServiceAccount()
	}
	return &HostArtifactQuarantineManager{
		runtime:   runtime,
		namespace: namespace,
		entries:   map[string]HostArtifactQuarantineState{},
	}
}

func (m *HostArtifactQuarantineManager) LoadFromConfigMap(ctx context.Context) error {
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return nil
	}
	cm, err := m.runtime.GetConfigMap(ctx, m.namespace, hostArtifactStateConfigMapName)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	loaded := map[string]HostArtifactQuarantineState{}
	now := time.Now().UTC()
	for key, raw := range cm.Data {
		var state HostArtifactQuarantineState
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			klog.Warningf("failed to decode host artifact quarantine state %s: %v", key, err)
			continue
		}
		if strings.TrimSpace(state.Key) == "" {
			state.Key = key
		}
		if state.ExpiresAt.IsZero() || state.ExpiresAt.After(now) {
			loaded[state.Key] = state
		}
	}
	m.mu.Lock()
	m.entries = loaded
	m.mu.Unlock()
	return nil
}

func (m *HostArtifactQuarantineManager) GetActive(key string, now time.Time) (HostArtifactQuarantineState, bool) {
	if m == nil || strings.TrimSpace(key) == "" {
		return HostArtifactQuarantineState{}, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	m.mu.RLock()
	state, ok := m.entries[strings.TrimSpace(key)]
	m.mu.RUnlock()
	if !ok || state.ExpiresAt.IsZero() || !state.ExpiresAt.After(now) {
		return HostArtifactQuarantineState{}, false
	}
	return state, true
}

func (m *HostArtifactQuarantineManager) MarkFailure(ctx context.Context, state HostArtifactQuarantineState, threshold int, ttl time.Duration) (HostArtifactQuarantineState, bool, error) {
	if m == nil {
		return state, false, nil
	}
	state.Key = strings.TrimSpace(state.Key)
	if state.Key == "" {
		return state, false, nil
	}
	if threshold <= 0 {
		threshold = 1
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	now := time.Now().UTC()
	state.Classification = strings.TrimSpace(state.Classification)
	if state.Classification == "" {
		state.Classification = opennebula.HostArtifactConflictClassification
	}
	state.Reason = strings.TrimSpace(state.Reason)
	if state.Reason == "" {
		state.Reason = hostArtifactReason
	}
	state.LastObservedAt = now

	m.mu.Lock()
	previous, hadPrevious := m.entries[state.Key]
	if hadPrevious && sameHostArtifactSeries(previous, state) {
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
	m.entries[state.Key] = state
	m.mu.Unlock()

	if err := m.persistEntry(ctx, state); err != nil {
		return state, active, err
	}
	return state, active, nil
}

func sameHostArtifactSeries(previous, next HostArtifactQuarantineState) bool {
	return strings.TrimSpace(previous.Classification) == strings.TrimSpace(next.Classification) &&
		previous.VMID == next.VMID &&
		previous.DiskID == next.DiskID &&
		strings.TrimSpace(previous.LVName) == strings.TrimSpace(next.LVName)
}

func (m *HostArtifactQuarantineManager) Clear(ctx context.Context, key string) error {
	if m == nil || strings.TrimSpace(key) == "" {
		return nil
	}
	key = strings.TrimSpace(key)
	m.mu.Lock()
	delete(m.entries, key)
	m.mu.Unlock()
	if m.runtime == nil || !m.runtime.enabled {
		return nil
	}
	return m.runtime.DeleteConfigMapKey(ctx, m.namespace, hostArtifactStateConfigMapName, key)
}

func (m *HostArtifactQuarantineManager) Snapshot() map[string]HostArtifactQuarantineState {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := make(map[string]HostArtifactQuarantineState, len(m.entries))
	for key, state := range m.entries {
		snapshot[key] = state
	}
	return snapshot
}

func (m *HostArtifactQuarantineManager) persistEntry(ctx context.Context, state HostArtifactQuarantineState) error {
	if m == nil || m.runtime == nil || !m.runtime.enabled {
		return nil
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal host artifact quarantine state: %w", err)
	}
	return m.runtime.UpsertConfigMapData(ctx, m.namespace, hostArtifactStateConfigMapName, map[string]string{
		state.Key: string(payload),
	})
}

func hostArtifactQuarantineMessage(state HostArtifactQuarantineState) string {
	if strings.TrimSpace(state.Message) != "" {
		if strings.TrimSpace(state.RepairHint) != "" {
			return fmt.Sprintf("%s; %s; host artifact quarantine active until %s after %d matching failure(s)", state.Message, state.RepairHint, state.ExpiresAt.Format(time.RFC3339), state.FailureCount)
		}
		return fmt.Sprintf("%s; host artifact quarantine active until %s after %d matching failure(s)", state.Message, state.ExpiresAt.Format(time.RFC3339), state.FailureCount)
	}
	return fmt.Sprintf("host artifact quarantine active for %s until %s after %d matching failure(s)", state.Key, state.ExpiresAt.Format(time.RFC3339), state.FailureCount)
}
