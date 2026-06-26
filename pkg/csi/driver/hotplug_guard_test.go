package driver

import (
	"testing"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHotplugGuardPausesOnlyAfterFailureThreshold(t *testing.T) {
	guard := NewHotplugGuard(time.Minute)

	state, paused := guard.MarkUnhealthyFailure("node-a", "detach", "vol-a", 30*time.Second, true, false, false, true, 2, "kubernetes_node_not_ready")
	require.False(t, paused)
	assert.Equal(t, 1, state.FailureCount)
	_, blocking := guard.Get("node-a")
	assert.False(t, blocking)

	state, paused = guard.MarkUnhealthyFailure("node-a", "detach", "vol-a", 30*time.Second, true, false, false, true, 2, "kubernetes_node_not_ready")
	require.True(t, paused)
	assert.Equal(t, 2, state.FailureCount)
	assert.True(t, state.PauseUntilReady)
	assert.True(t, state.ExpiresAt.IsZero())

	blockingState, blocking := guard.Get("node-a")
	require.True(t, blocking)
	assert.True(t, blockingState.PauseUntilReady)
}

func TestHotplugGuardLoadKeepsPauseUntilReadyState(t *testing.T) {
	guard := NewHotplugGuard(time.Minute)
	guard.Load(map[string]opennebula.HotplugCooldownState{
		"node-a": {
			Node:            "node-a",
			Operation:       "detach",
			Volume:          "vol-a",
			FailureCount:    2,
			PauseUntilReady: true,
			Reason:          "opennebula_vm_not_running",
		},
	})

	state, ok := guard.Get("node-a")
	require.True(t, ok)
	assert.True(t, state.PauseUntilReady)
	assert.Equal(t, 2, state.FailureCount)
}
