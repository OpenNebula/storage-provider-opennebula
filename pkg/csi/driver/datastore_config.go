package driver

import (
	"fmt"
	"strings"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	storageClassParamDatastoreIDs             = "datastoreIDs"
	storageClassParamDatastoreSelectionPolicy = "datastoreSelectionPolicy"
	storageClassParamFSType                   = "fsType"
	storageClassParamSharedFilesystemPath     = "sharedFilesystemPath"
	storageClassParamSharedFilesystemGroup    = "sharedFilesystemSubvolumeGroup"
	storageClassParamCephFSMounter            = "cephfsMounter"
)

func (d *Driver) GetDatastoreSelectionConfig(params map[string]string) (opennebula.DatastoreSelectionConfig, error) {
	policy := opennebula.NormalizeDatastoreSelectionPolicy(d.getDefaultDatastorePolicy())
	if rawPolicy := strings.TrimSpace(params[storageClassParamDatastoreSelectionPolicy]); rawPolicy != "" {
		policy = opennebula.NormalizeDatastoreSelectionPolicy(rawPolicy)
	}

	identifiers := d.getDefaultDatastoreIdentifiers()
	if rawIdentifiers := strings.TrimSpace(params[storageClassParamDatastoreIDs]); rawIdentifiers != "" {
		identifiers = splitCSV(rawIdentifiers)
	}

	if len(identifiers) == 0 {
		return opennebula.DatastoreSelectionConfig{}, fmt.Errorf("no datastores configured; set %s or storage class parameter %s", config.DefaultDatastoresVar, storageClassParamDatastoreIDs)
	}

	if d.inventoryEligibility != nil && d.inventoryEligibility.Enabled() {
		filtered, err := d.inventoryEligibility.FilterIdentifiers(identifiers)
		if err != nil {
			return opennebula.DatastoreSelectionConfig{}, err
		}
		identifiers = filtered
	}

	if len(identifiers) == 0 {
		return opennebula.DatastoreSelectionConfig{}, fmt.Errorf("no eligible datastores remain after inventory authority filtering")
	}

	return opennebula.DatastoreSelectionConfig{
		Identifiers:  identifiers,
		Policy:       policy,
		AllowedTypes: d.getAllowedDatastoreTypes(),
	}, nil
}

func filterProvisioningParams(params map[string]string) map[string]string {
	filtered := make(map[string]string, len(params))
	for key, value := range params {
		switch key {
		case storageClassParamDatastoreIDs, storageClassParamDatastoreSelectionPolicy, storageClassParamFSType, storageClassParamSharedFilesystemPath, storageClassParamSharedFilesystemGroup, storageClassParamCephFSMounter:
			continue
		default:
			if strings.HasPrefix(key, "csi.storage.k8s.io/") {
				continue
			}
			filtered[key] = value
		}
	}

	return filtered
}

func getRequestedFSType(params map[string]string, capability *csi.VolumeCapability) string {
	fsType := ""
	if capability != nil && capability.GetMount() != nil {
		fsType = capability.GetMount().GetFsType()
	}

	if strings.TrimSpace(fsType) != "" {
		return fsType
	}

	return strings.TrimSpace(params[storageClassParamFSType])
}

func (d *Driver) getDefaultDatastoreIdentifiers() []string {
	values, ok := d.PluginConfig.GetStringSlice(config.DefaultDatastoresVar)
	if !ok {
		return nil
	}

	return values
}

func (d *Driver) getDefaultDatastorePolicy() string {
	value, ok := d.PluginConfig.GetString(config.DatastorePolicyVar)
	if !ok {
		return string(opennebula.DatastoreSelectionPolicyLeastUsed)
	}

	return value
}

func (d *Driver) getAllowedDatastoreTypes() []string {
	values, ok := d.PluginConfig.GetStringSlice(config.AllowedDatastoreTypesVar)
	if !ok {
		return []string{"local", "ceph", "cephfs"}
	}

	return values
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		normalized = append(normalized, trimmed)
	}

	return normalized
}
