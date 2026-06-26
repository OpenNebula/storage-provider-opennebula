package driver

import (
	"sync"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
)

type HotplugGuard struct {
	mu       sync.Mutex
	cooldown time.Duration
	states   map[string]opennebula.HotplugCooldownState
}

func NewHotplugGuard(cooldown time.Duration) *HotplugGuard {
	return &HotplugGuard{
		cooldown: cooldown,
		states:   make(map[string]opennebula.HotplugCooldownState),
	}
}

func (g *HotplugGuard) MarkCooldown(node, operation, volume string, timeout time.Duration, attached, ready bool) opennebula.HotplugCooldownState {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now().UTC()
	state := opennebula.HotplugCooldownState{
		Node:                 node,
		Operation:            operation,
		Volume:               volume,
		ExpiresAt:            now.Add(g.cooldown),
		Timeout:              timeout,
		LastObservedAttached: attached,
		LastObservedReady:    ready,
		OpenNebulaReady:      ready,
		Reason:               "timeout_exhausted",
		FirstFailureAt:       now,
		LastFailureAt:        now,
	}
	g.states[node] = state
	return state
}

func (g *HotplugGuard) MarkUnhealthyFailure(node, operation, volume string, timeout time.Duration, attached, openNebulaReady, kubernetesReady, unschedulable bool, threshold int, reason string) (opennebula.HotplugCooldownState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if threshold <= 0 {
		threshold = 1
	}
	now := time.Now().UTC()
	state := g.states[node]
	if state.Node == "" || (!state.PauseUntilReady && !state.ExpiresAt.IsZero() && now.After(state.ExpiresAt)) {
		state = opennebula.HotplugCooldownState{
			Node:           node,
			FirstFailureAt: now,
		}
	}
	if state.FirstFailureAt.IsZero() {
		state.FirstFailureAt = now
	}
	state.Operation = operation
	state.Volume = volume
	state.Timeout = timeout
	state.LastObservedAttached = attached
	state.LastObservedReady = openNebulaReady
	state.KubernetesReady = kubernetesReady
	state.OpenNebulaReady = openNebulaReady
	state.Unschedulable = unschedulable
	state.LastFailureAt = now
	state.FailureCount++
	state.Reason = reason
	state.PauseUntilReady = state.FailureCount >= threshold
	if state.PauseUntilReady {
		state.ExpiresAt = time.Time{}
	} else {
		state.ExpiresAt = now.Add(g.cooldown)
	}
	g.states[node] = state
	return state, state.PauseUntilReady
}

func (g *HotplugGuard) MarkPaused(state opennebula.HotplugCooldownState) opennebula.HotplugCooldownState {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now().UTC()
	if state.Node == "" {
		return state
	}
	if state.FirstFailureAt.IsZero() {
		state.FirstFailureAt = now
	}
	if state.LastFailureAt.IsZero() {
		state.LastFailureAt = now
	}
	if state.FailureCount <= 0 {
		state.FailureCount = 1
	}
	state.PauseUntilReady = true
	state.ExpiresAt = time.Time{}
	g.states[state.Node] = state
	return state
}

func (g *HotplugGuard) Load(states map[string]opennebula.HotplugCooldownState) {
	if g == nil || len(states) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now().UTC()
	for node, state := range states {
		if node == "" {
			node = state.Node
		}
		if node == "" {
			continue
		}
		if !state.PauseUntilReady && !state.ExpiresAt.IsZero() && now.After(state.ExpiresAt) {
			continue
		}
		state.Node = node
		g.states[node] = state
	}
}

func (g *HotplugGuard) Snapshot() map[string]opennebula.HotplugCooldownState {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make(map[string]opennebula.HotplugCooldownState, len(g.states))
	for node, state := range g.states {
		out[node] = state
	}
	return out
}

func (g *HotplugGuard) Get(node string) (opennebula.HotplugCooldownState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	state, ok := g.states[node]
	if !ok {
		return opennebula.HotplugCooldownState{}, false
	}
	if state.PauseUntilReady {
		return state, true
	}
	if state.FailureCount > 0 {
		if !state.ExpiresAt.IsZero() && time.Now().UTC().After(state.ExpiresAt) {
			delete(g.states, node)
		}
		return opennebula.HotplugCooldownState{}, false
	}
	if state.ExpiresAt.IsZero() || time.Now().UTC().After(state.ExpiresAt) {
		delete(g.states, node)
		return opennebula.HotplugCooldownState{}, false
	}
	return state, true
}

func (g *HotplugGuard) Clear(node string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.states, node)
}
