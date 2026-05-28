package driver

import (
	"context"
	"strconv"
	"strings"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
)

func (d *Driver) nodeAccessibleTopology(ctx context.Context) []*csi.Topology {
	if d == nil || !d.featureGates.TopologyAccessibility {
		return nil
	}

	if value, ok := d.PluginConfig.GetString(config.NodeTopologySystemDSVar); ok {
		if topology := topologyForSystemDatastore(strings.TrimSpace(value)); topology != nil {
			return []*csi.Topology{topology}
		}
	}

	if d.kubeRuntime == nil {
		return nil
	}

	value, err := d.kubeRuntime.GetNodeLabel(ctx, d.nodeID, topologySystemDSLabel)
	if err != nil {
		return nil
	}

	if topology := topologyForSystemDatastore(value); topology != nil {
		return []*csi.Topology{topology}
	}

	return nil
}

func accessibleTopologyForDatastore(datastore opennebula.Datastore) []*csi.Topology {
	if len(datastore.CompatibleSystemDatastores) == 0 {
		return nil
	}

	topologies := make([]*csi.Topology, 0, len(datastore.CompatibleSystemDatastores))
	for _, systemDSID := range datastore.CompatibleSystemDatastores {
		topology := topologyForSystemDatastore(strconv.Itoa(systemDSID))
		if topology == nil {
			continue
		}
		topologies = append(topologies, topology)
	}

	if len(topologies) == 0 {
		return nil
	}

	return topologies
}

func topologyForSystemDatastore(value string) *csi.Topology {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}

	return &csi.Topology{
		Segments: map[string]string{
			topologySystemDSLabel: trimmed,
		},
	}
}
