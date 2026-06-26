package driver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAdaptiveTimeoutSourceIDUsesValidConfigMapDataKeys(t *testing.T) {
	assert.Equal(t, "", (*Driver)(nil).adaptiveTimeoutSourceID())
	assert.Equal(t, "controller", (&Driver{}).adaptiveTimeoutSourceID())
	assert.Equal(t, "node.hplbravow02", (&Driver{nodeID: "hplbravow02"}).adaptiveTimeoutSourceID())
	assert.Equal(t, "node.node_a_1", (&Driver{nodeID: "node/a:1"}).adaptiveTimeoutSourceID())
	assert.Equal(t, "___", sanitizeConfigMapDataKey(":::"))
}
