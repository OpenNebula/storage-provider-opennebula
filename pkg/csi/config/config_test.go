package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetStringSliceParsesCSVValues(t *testing.T) {
	cfg := LoadConfiguration()
	cfg.OverrideVal(DefaultDatastoresVar, "100, 101 ,default")

	values, ok := cfg.GetStringSlice(DefaultDatastoresVar)
	require.True(t, ok)
	assert.Equal(t, []string{"100", "101", "default"}, values)
}

func TestNodeExpandDefaults(t *testing.T) {
	cfg := LoadConfiguration()

	verifyTimeout, ok := cfg.GetInt(NodeExpandVerifyTimeoutSecondsVar)
	require.True(t, ok)
	assert.Equal(t, 120, verifyTimeout)

	retryInterval, ok := cfg.GetInt(NodeExpandRetryIntervalSecondsVar)
	require.True(t, ok)
	assert.Equal(t, 2, retryInterval)

	sizeToleranceBytes, ok := cfg.GetInt(NodeExpandSizeToleranceBytesVar)
	require.True(t, ok)
	assert.Equal(t, 134217728, sizeToleranceBytes)
}

func TestNodeExpandOverrides(t *testing.T) {
	cfg := LoadConfiguration()
	cfg.OverrideVal(NodeExpandVerifyTimeoutSecondsVar, 30)
	cfg.OverrideVal(NodeExpandRetryIntervalSecondsVar, 5)
	cfg.OverrideVal(NodeExpandSizeToleranceBytesVar, 67108864)

	verifyTimeout, ok := cfg.GetInt(NodeExpandVerifyTimeoutSecondsVar)
	require.True(t, ok)
	assert.Equal(t, 30, verifyTimeout)

	retryInterval, ok := cfg.GetInt(NodeExpandRetryIntervalSecondsVar)
	require.True(t, ok)
	assert.Equal(t, 5, retryInterval)

	sizeToleranceBytes, ok := cfg.GetInt(NodeExpandSizeToleranceBytesVar)
	require.True(t, ok)
	assert.Equal(t, 67108864, sizeToleranceBytes)
}
