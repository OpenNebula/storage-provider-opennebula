package driver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
)

const hotplugDiagnosticsConfigMapName = "opennebula-csi-hotplug-diagnostics"

type hotplugDiagnoseProvider interface {
	DiagnoseNodeHotplug(ctx context.Context, node string, previous *opennebula.HotplugObservation, cfg opennebula.HotplugDiagnosisConfig) (opennebula.HotplugDiagnosis, opennebula.HotplugObservation, error)
}

func (s *ControllerServer) diagnoseAndPersistHotplug(ctx context.Context, node string) (opennebula.HotplugDiagnosis, bool) {
	if s == nil || s.driver == nil || !hotplugDiagnosticsEnabled(s.driver.PluginConfig) {
		return opennebula.HotplugDiagnosis{}, false
	}
	provider, ok := s.volumeProvider.(hotplugDiagnoseProvider)
	if !ok {
		return opennebula.HotplugDiagnosis{}, false
	}
	previous := s.loadHotplugObservation(ctx, node)
	diagnosis, observation, err := provider.DiagnoseNodeHotplug(ctx, node, previous, loadHotplugDiagnosisConfig(s.driver.PluginConfig))
	if err != nil {
		klog.V(2).InfoS("Failed to diagnose OpenNebula hotplug state", "node", node, "err", err)
		return opennebula.HotplugDiagnosis{}, false
	}
	s.persistHotplugObservation(ctx, observation)
	if s.driver.metrics != nil {
		s.driver.metrics.RecordHotplugDiagnosis(diagnosis.Classification, diagnosis.Reason)
		s.driver.metrics.SetHotplugStateAge(diagnosis.Node, diagnosis.LCMState, diagnosis.Classification, diagnosis.AgeSeconds)
	}
	return diagnosis, true
}

func (s *ControllerServer) loadHotplugObservation(ctx context.Context, node string) *opennebula.HotplugObservation {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled || strings.TrimSpace(node) == "" {
		return nil
	}
	cm, err := s.driver.kubeRuntime.GetConfigMap(ctx, namespaceFromServiceAccount(), hotplugDiagnosticsConfigMapName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.V(4).InfoS("Failed to load hotplug diagnosis observation", "node", node, "err", err)
		}
		return nil
	}
	raw := strings.TrimSpace(cm.Data[node])
	if raw == "" {
		return nil
	}
	var observation opennebula.HotplugObservation
	if err := json.Unmarshal([]byte(raw), &observation); err != nil {
		klog.V(4).InfoS("Failed to unmarshal hotplug diagnosis observation", "node", node, "err", err)
		return nil
	}
	return &observation
}

func (s *ControllerServer) persistHotplugObservation(ctx context.Context, observation opennebula.HotplugObservation) {
	if s == nil || s.driver == nil || s.driver.kubeRuntime == nil || !s.driver.kubeRuntime.enabled || strings.TrimSpace(observation.Node) == "" {
		return
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		klog.V(4).InfoS("Failed to marshal hotplug diagnosis observation", "node", observation.Node, "err", err)
		return
	}
	if err := s.driver.kubeRuntime.UpsertConfigMapData(ctx, namespaceFromServiceAccount(), hotplugDiagnosticsConfigMapName, map[string]string{
		observation.Node: string(payload),
	}); err != nil {
		klog.V(4).InfoS("Failed to persist hotplug diagnosis observation", "node", observation.Node, "err", err)
	}
}

func hotplugDiagnosticsEnabled(cfg config.CSIPluginConfig) bool {
	enabled, ok := cfg.GetBool(config.HotplugDiagnosticsEnabledVar)
	return !ok || enabled
}
