package driver

import (
	"context"
	"fmt"
	"testing"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeInventoryEligibilityProvider struct {
	filtered []string
	err      error
}

func (f *fakeInventoryEligibilityProvider) Start(_ context.Context) error {
	return nil
}

func (f *fakeInventoryEligibilityProvider) Enabled() bool {
	return true
}

func (f *fakeInventoryEligibilityProvider) FilterIdentifiers(_ []string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]string(nil), f.filtered...), nil
}

func TestGetDatastoreSelectionConfigUsesStorageClassOverride(t *testing.T) {
	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.DefaultDatastoresVar, "100,101")
	pluginConfig.OverrideVal(config.DatastorePolicyVar, "ordered")
	pluginConfig.OverrideVal(config.AllowedDatastoreTypesVar, "local")

	driver := &Driver{PluginConfig: pluginConfig}

	selection, err := driver.GetDatastoreSelectionConfig(map[string]string{
		storageClassParamDatastoreIDs:             "200,201",
		storageClassParamDatastoreSelectionPolicy: "least-used",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"200", "201"}, selection.Identifiers)
	assert.Equal(t, opennebula.DatastoreSelectionPolicyLeastUsed, selection.Policy)
	assert.Equal(t, []string{"local"}, selection.AllowedTypes)
}

func TestGetDatastoreSelectionConfigFallsBackToDriverDefaults(t *testing.T) {
	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.DefaultDatastoresVar, "100,101")
	pluginConfig.OverrideVal(config.DatastorePolicyVar, "ordered")
	pluginConfig.OverrideVal(config.AllowedDatastoreTypesVar, "local,ceph,cephfs")

	driver := &Driver{PluginConfig: pluginConfig}

	selection, err := driver.GetDatastoreSelectionConfig(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"100", "101"}, selection.Identifiers)
	assert.Equal(t, opennebula.DatastoreSelectionPolicyOrdered, selection.Policy)
	assert.Equal(t, []string{"local", "ceph", "cephfs"}, selection.AllowedTypes)
}

func TestGetDatastoreSelectionConfigSupportsAutopilotPolicy(t *testing.T) {
	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.DefaultDatastoresVar, "100,101")
	pluginConfig.OverrideVal(config.DatastorePolicyVar, "autopilot")
	pluginConfig.OverrideVal(config.AllowedDatastoreTypesVar, "local,ceph")

	driver := &Driver{PluginConfig: pluginConfig}

	selection, err := driver.GetDatastoreSelectionConfig(nil)
	require.NoError(t, err)
	assert.Equal(t, opennebula.DatastoreSelectionPolicyAutopilot, selection.Policy)
}

func TestFilterProvisioningParamsRemovesReservedKeys(t *testing.T) {
	filtered := filterProvisioningParams(map[string]string{
		storageClassParamDatastoreIDs:               "100",
		storageClassParamDatastoreSelectionPolicy:   "least-used",
		storageClassParamFSType:                     "xfs",
		storageClassParamSharedFilesystemPath:       "/static/path",
		storageClassParamSharedFilesystemGroup:      "csi",
		"csi.storage.k8s.io/node-stage-secret-name": "cephfs-node",
		"driver": "raw",
		"cache":  "none",
	})

	assert.Equal(t, map[string]string{
		"driver": "raw",
		"cache":  "none",
	}, filtered)
}

func TestGetAllowedDatastoreTypesUsesCephEnabledDefault(t *testing.T) {
	pluginConfig := config.LoadConfiguration()

	driver := &Driver{PluginConfig: pluginConfig}

	assert.Equal(t, []string{"local", "ceph", "cephfs"}, driver.getAllowedDatastoreTypes())
}

func TestGetDatastoreSelectionConfigAppliesInventoryFiltering(t *testing.T) {
	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.DefaultDatastoresVar, "100,101")

	driver := &Driver{
		PluginConfig:         pluginConfig,
		inventoryEligibility: &fakeInventoryEligibilityProvider{filtered: []string{"101"}},
	}

	selection, err := driver.GetDatastoreSelectionConfig(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"101"}, selection.Identifiers)
}

func TestGetDatastoreSelectionConfigReturnsInventoryFilteringError(t *testing.T) {
	pluginConfig := config.LoadConfiguration()
	pluginConfig.OverrideVal(config.DefaultDatastoresVar, "100")

	driver := &Driver{
		PluginConfig:         pluginConfig,
		inventoryEligibility: &fakeInventoryEligibilityProvider{err: fmt.Errorf("inventory rejected datastore")},
	}

	_, err := driver.GetDatastoreSelectionConfig(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inventory rejected datastore")
}
