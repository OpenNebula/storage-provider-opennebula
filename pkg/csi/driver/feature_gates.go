package driver

import (
	"strconv"
	"strings"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
)

type FeatureGates struct {
	CompatibilityAwareSelection bool
	DetachedDiskExpansion       bool
	CephFSExpansion             bool
	CephFSSnapshots             bool
	CephFSClones                bool
	CephFSSelfHealing           bool
	CephFSPersistentRecovery    bool
	CephFSKernelMounts          bool
	LocalRWOStaleMountRecovery  bool
	TopologyAccessibility       bool
}

func defaultFeatureGates() FeatureGates {
	return FeatureGates{
		CompatibilityAwareSelection: true,
		DetachedDiskExpansion:       true,
		CephFSExpansion:             true,
		CephFSSnapshots:             false,
		CephFSClones:                false,
		CephFSSelfHealing:           false,
		CephFSPersistentRecovery:    true,
		CephFSKernelMounts:          false,
		LocalRWOStaleMountRecovery:  false,
		TopologyAccessibility:       false,
	}
}

func loadFeatureGates(cfg config.CSIPluginConfig) FeatureGates {
	gates := defaultFeatureGates()
	raw, ok := cfg.GetString(config.FeatureGatesVar)
	if !ok {
		return gates
	}

	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		enabled, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			continue
		}

		switch strings.TrimSpace(key) {
		case "compatibilityAwareSelection":
			gates.CompatibilityAwareSelection = enabled
		case "detachedDiskExpansion":
			gates.DetachedDiskExpansion = enabled
		case "cephfsExpansion":
			gates.CephFSExpansion = enabled
		case "cephfsSnapshots":
			gates.CephFSSnapshots = enabled
		case "cephfsClones":
			gates.CephFSClones = enabled
		case "cephfsSelfHealing":
			gates.CephFSSelfHealing = enabled
		case "cephfsPersistentRecovery":
			gates.CephFSPersistentRecovery = enabled
		case "cephfsKernelMounts":
			gates.CephFSKernelMounts = enabled
		case "localRWOStaleMountRecovery":
			gates.LocalRWOStaleMountRecovery = enabled
		case "topologyAccessibility":
			gates.TopologyAccessibility = enabled
		}
	}

	return gates
}
