/*
Copyright 2025, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	inventorycache "github.com/SparkAIUR/storage-provider-opennebula/pkg/inventory/cache"
	"github.com/container-storage-interface/spec/lib/go/csi"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const (
	DefaultDriverName         = "csi.opennebula.io" //TODO: get from a repo metadata file or from a build flag
	DefaultGRPCServerEndpoint = "unix:///tmp/csi.sock"
	DefaultVolumeSizeBytes    = 1 * 1024 * 1024 * 1024
	GracefulShutdownTimeout   = 25 * time.Second
)

var (
	driverVersion   = "dev"
	driverCommit    = "unknown"
	driverBuildDate = "unknown"
)

type Driver struct {
	name               string
	grpcServerEndpoint string
	nodeID             string
	version            string
	commit             string
	buildDate          string

	PluginConfig config.CSIPluginConfig

	controllerServerCapabilities []*csi.ControllerServiceCapability

	operationLocks       *OperationLocks
	hotplugGuard         *HotplugGuard
	controllerLeadership *ControllerLeadership

	maxVolumesPerNode int64

	mounter *mount.SafeFormatAndMount

	featureGates           FeatureGates
	metrics                *DriverMetrics
	kubeRuntime            *KubeRuntime
	stickyAttachments      *StickyAttachmentManager
	volumeQuarantine       *VolumeQuarantineManager
	hostArtifactQuarantine *HostArtifactQuarantineManager
	hotplugQueue           *HotplugQueueManager
	maintenanceMode        *MaintenanceModeManager
	adaptiveTimeouts       *opennebula.AdaptiveTimeoutTracker

	inventoryEligibility inventorycache.DatastoreEligibilityProvider
}

type DriverOptions struct {
	NodeID             string
	DriverName         string
	MaxVolumesPerNode  int64
	GRPCServerEndpoint string
	PluginConfig       config.CSIPluginConfig
	Mounter            *mount.SafeFormatAndMount
}

func NewDriver(options *DriverOptions) *Driver {
	return &Driver{
		name:               options.DriverName,
		version:            driverVersion,
		commit:             driverCommit,
		buildDate:          driverBuildDate,
		nodeID:             options.NodeID,
		grpcServerEndpoint: options.GRPCServerEndpoint,
		PluginConfig:       options.PluginConfig,
		maxVolumesPerNode:  options.MaxVolumesPerNode,
		mounter:            options.Mounter,
		featureGates:       loadFeatureGates(options.PluginConfig),
		operationLocks:     NewOperationLocks(),
		hotplugGuard:       NewHotplugGuard(loadHotplugCooldown(options.PluginConfig)),
	}
}

func (d *Driver) Run(ctx context.Context) error {
	klog.InfoS("Starting OpenNebula CSI driver",
		"name", d.name,
		"version", d.version,
		"commit", d.commit,
		"buildDate", d.buildDate,
		"nodeID", d.nodeID,
		"featureGates", d.featureGates)

	d.metrics = NewDriverMetrics(d.version, d.commit)
	d.kubeRuntime = NewKubeRuntime(d.name)
	if strings.TrimSpace(d.nodeID) == "" {
		d.stickyAttachments = NewStickyAttachmentManager(d.kubeRuntime, "")
		if err := d.stickyAttachments.LoadFromConfigMap(ctx); err != nil {
			klog.V(2).InfoS("Failed to load sticky attachment state", "err", err)
		}
		d.volumeQuarantine = NewVolumeQuarantineManager(d.kubeRuntime, "")
		if err := d.volumeQuarantine.LoadFromConfigMap(ctx); err != nil {
			klog.V(2).InfoS("Failed to load volume quarantine state", "err", err)
		}
		d.hostArtifactQuarantine = NewHostArtifactQuarantineManager(d.kubeRuntime, "")
		if err := d.hostArtifactQuarantine.LoadFromConfigMap(ctx); err != nil {
			klog.V(2).InfoS("Failed to load host artifact quarantine state", "err", err)
		}
		if err := d.loadHotplugGuardState(ctx); err != nil {
			klog.V(2).InfoS("Failed to load hotplug guard state", "err", err)
		}
		d.maintenanceMode = NewMaintenanceModeManager(d, "")
	}
	d.hotplugQueue = NewHotplugQueueManager(d.kubeRuntime, "", d.metrics, loadHotplugQueueMaxWait(d.PluginConfig), loadHotplugQueueAgeBoost(d.PluginConfig))
	if d.hotplugQueue != nil {
		d.hotplugQueue.SetSnapshotDebounce(loadHotplugQueueSnapshotDebounce(d.PluginConfig))
	}
	d.adaptiveTimeouts = opennebula.NewAdaptiveTimeoutTracker(loadAdaptiveTimeoutConfig(d.PluginConfig))
	if err := d.loadAdaptiveTimeoutObservations(ctx); err != nil {
		klog.V(2).InfoS("Failed to load adaptive timeout observations", "err", err)
	}

	if enabled, _ := d.PluginConfig.GetBool(config.InventoryControllerEnabledVar); enabled && strings.TrimSpace(d.nodeID) == "" {
		restConfig, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("failed to initialize Kubernetes config for inventory eligibility cache: %w", err)
		}
		authorityMode, _ := d.PluginConfig.GetString(config.InventoryDatastoreAuthorityModeVar)
		resyncSeconds, ok := d.PluginConfig.GetInt(config.InventoryControllerResyncDatastoresVar)
		if !ok || resyncSeconds <= 0 {
			resyncSeconds = 60
		}
		provider, err := inventorycache.NewProvider(restConfig, time.Duration(resyncSeconds)*time.Second, authorityMode)
		if err != nil {
			return fmt.Errorf("failed to initialize inventory eligibility cache: %w", err)
		}
		if err := provider.Start(ctx); err != nil {
			return fmt.Errorf("failed to start inventory eligibility cache: %w", err)
		}
		d.inventoryEligibility = provider
	}

	controllerLeadership, err := NewControllerLeadership(ctx, d.PluginConfig)
	if err != nil {
		return err
	}
	d.controllerLeadership = controllerLeadership

	grpcServer := NewGRPCServer()

	endpoint, ok := d.PluginConfig.GetString(config.OpenNebulaRPCEndpointVar)
	if !ok {
		return fmt.Errorf("failed to get %s endpoint from config", config.OpenNebulaRPCEndpointVar)
	}

	credentials, ok := d.PluginConfig.GetString(config.OpenNebulaCredentialsVar)
	if !ok {
		return fmt.Errorf("failed to get %s credentials from config", config.OpenNebulaCredentialsVar)
	}
	volumeProvider, err := opennebula.NewPersistentDiskVolumeProvider(
		opennebula.NewClient(opennebula.OpenNebulaConfig{
			Endpoint:    endpoint,
			Credentials: credentials,
		}),
		loadHotplugTimeoutPolicy(d.PluginConfig),
	)
	if err != nil || volumeProvider == nil {
		return fmt.Errorf("failed to create PersistentDiskVolumeProvider: %v", err)
	}

	sharedFilesystemProvider, err := opennebula.NewCephFSVolumeProvider(
		opennebula.NewClient(opennebula.OpenNebulaConfig{
			Endpoint:    endpoint,
			Credentials: credentials,
		}),
		d.mounter.Exec,
	)
	if err != nil || sharedFilesystemProvider == nil {
		return fmt.Errorf("failed to create CephFSVolumeProvider: %v", err)
	}

	controllerServer := NewControllerServer(d, volumeProvider, sharedFilesystemProvider)
	if d.hotplugQueue != nil {
		d.hotplugQueue.Configure(
			loadHotplugQueueDedupe(d.PluginConfig),
			loadHotplugQueuePerItemWait(d.PluginConfig),
			loadHotplugQueueMaxWaitCap(d.PluginConfig),
			loadHotplugQueueMaxActive(d.PluginConfig),
			controllerServer.validateHotplugQueueRequest,
		)
	}
	nodeServer := NewNodeServer(d, d.mounter)
	grpcServer.Start(
		d.grpcServerEndpoint,
		NewIdentityServer(d),
		nodeServer,
		controllerServer,
		d.controllerLeadership,
	)
	nodeServer.StartBackgroundWorkers(ctx)
	controllerServer.StartBackgroundWorkers(ctx)
	d.maybeStartWebhookServer(ctx)

	metricsServer := NewMetricsServer(d.PluginConfig, d.metrics)
	metricsServer.Start()

	go func() {
		<-ctx.Done()
		klog.Info("Received shutdown signal, stopping driver...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), GracefulShutdownTimeout)
		defer cancel()
		metricsServer.Stop(shutdownCtx)
		grpcServer.Stop(shutdownCtx)
	}()

	grpcServer.Wait()

	return nil
}

func (d *Driver) loadHotplugGuardState(ctx context.Context) error {
	if d == nil || d.kubeRuntime == nil || d.hotplugGuard == nil {
		return nil
	}
	cm, err := d.kubeRuntime.GetConfigMap(ctx, namespaceFromServiceAccount(), hotplugStateConfigMapName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	states := make(map[string]opennebula.HotplugCooldownState, len(cm.Data))
	expiredKeys := make([]string, 0)
	now := time.Now().UTC()
	for node, raw := range cm.Data {
		if isMaintenanceConfigMapKey(node) {
			continue
		}
		var state opennebula.HotplugCooldownState
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			klog.V(2).InfoS("Skipping invalid hotplug guard state", "node", node, "err", err)
			continue
		}
		if !state.PauseUntilReady && !state.ExpiresAt.IsZero() && now.After(state.ExpiresAt) {
			expiredKeys = append(expiredKeys, node)
			continue
		}
		if state.Node == "" {
			state.Node = node
		}
		states[node] = state
	}
	d.hotplugGuard.Load(states)
	for _, key := range expiredKeys {
		if err := d.kubeRuntime.DeleteConfigMapKey(ctx, namespaceFromServiceAccount(), hotplugStateConfigMapName, key); err != nil {
			klog.V(2).InfoS("Failed to garbage-collect expired hotplug guard state", "node", key, "err", err)
		}
	}
	return nil
}

func loadHotplugTimeoutPolicy(cfg config.CSIPluginConfig) opennebula.HotplugTimeoutPolicy {
	baseTimeout, baseOK := cfg.GetInt(config.VMHotplugTimeoutBaseVar)
	if !baseOK || baseTimeout <= 0 {
		if legacyTimeout, legacyOK := cfg.GetInt(config.VMHotplugTimeoutVar); legacyOK && legacyTimeout > 0 {
			baseTimeout = legacyTimeout
		} else {
			baseTimeout = 120
		}
	}

	per100Gi, ok := cfg.GetInt(config.VMHotplugTimeoutPer100GiVar)
	if !ok || per100Gi < 0 {
		per100Gi = 60
	}

	maxTimeout, ok := cfg.GetInt(config.VMHotplugTimeoutMaxVar)
	if !ok || maxTimeout <= 0 {
		maxTimeout = 900
	}
	if maxTimeout < baseTimeout {
		maxTimeout = baseTimeout
	}

	return opennebula.HotplugTimeoutPolicy{
		BaseTimeout:  time.Duration(baseTimeout) * time.Second,
		Per100GiB:    time.Duration(per100Gi) * time.Second,
		MaxTimeout:   time.Duration(maxTimeout) * time.Second,
		PollInterval: time.Second,
	}
}

func loadHotplugCooldown(cfg config.CSIPluginConfig) time.Duration {
	cooldownSeconds, ok := cfg.GetInt(config.VMHotplugStuckCooldownSecondsVar)
	if !ok || cooldownSeconds <= 0 {
		cooldownSeconds = 300
	}

	return time.Duration(cooldownSeconds) * time.Second
}

func loadHotplugQueueMaxWait(cfg config.CSIPluginConfig) time.Duration {
	seconds, ok := cfg.GetInt(config.HotplugQueueMaxWaitSecondsVar)
	if !ok || seconds <= 0 {
		seconds = 180
	}
	return time.Duration(seconds) * time.Second
}

func loadHotplugQueueAgeBoost(cfg config.CSIPluginConfig) time.Duration {
	seconds, ok := cfg.GetInt(config.HotplugQueueAgeBoostSecondsVar)
	if !ok || seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func loadHotplugQueueDedupe(cfg config.CSIPluginConfig) bool {
	enabled, ok := cfg.GetBool(config.HotplugQueueDedupeEnabledVar)
	return !ok || enabled
}

func loadHotplugQueuePerItemWait(cfg config.CSIPluginConfig) time.Duration {
	seconds, ok := cfg.GetInt(config.HotplugQueuePerItemWaitSecondsVar)
	if !ok || seconds < 0 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func loadHotplugQueueMaxWaitCap(cfg config.CSIPluginConfig) time.Duration {
	seconds, ok := cfg.GetInt(config.HotplugQueueMaxWaitCapSecondsVar)
	if !ok || seconds <= 0 {
		seconds = 900
	}
	return time.Duration(seconds) * time.Second
}

func loadHotplugQueueMaxActive(cfg config.CSIPluginConfig) time.Duration {
	seconds, ok := cfg.GetInt(config.HotplugQueueMaxActiveSecondsVar)
	if !ok || seconds <= 0 {
		seconds = 900
	}
	return time.Duration(seconds) * time.Second
}

func loadHotplugQueueSnapshotDebounce(cfg config.CSIPluginConfig) time.Duration {
	seconds, ok := cfg.GetInt(config.HotplugQueueSnapshotDebounceSecondsVar)
	if !ok || seconds < 0 {
		seconds = 2
	}
	return time.Duration(seconds) * time.Second
}

func loadHotplugDiagnosisConfig(cfg config.CSIPluginConfig) opennebula.HotplugDiagnosisConfig {
	stuckAfter, ok := cfg.GetInt(config.HotplugDiagnosticsStuckAfterSecondsVar)
	if !ok || stuckAfter <= 0 {
		stuckAfter = 300
	}
	progressWindow, ok := cfg.GetInt(config.HotplugDiagnosticsProgressWindowSecondsVar)
	if !ok || progressWindow <= 0 {
		progressWindow = 60
	}
	return opennebula.HotplugDiagnosisConfig{
		StuckAfter:     time.Duration(stuckAfter) * time.Second,
		ProgressWindow: time.Duration(progressWindow) * time.Second,
	}
}

func loadAdaptiveTimeoutConfig(cfg config.CSIPluginConfig) opennebula.AdaptiveTimeoutConfig {
	enabled, ok := cfg.GetBool(config.HotplugAdaptiveTimeoutEnabledVar)
	if !ok {
		enabled = true
	}
	minSamples, ok := cfg.GetInt(config.HotplugAdaptiveMinSamplesVar)
	if !ok || minSamples <= 0 {
		minSamples = 8
	}
	sampleWindow, ok := cfg.GetInt(config.HotplugAdaptiveSampleWindowVar)
	if !ok || sampleWindow <= 0 {
		sampleWindow = 20
	}
	multiplier, ok := cfg.GetInt(config.HotplugAdaptiveP95MultiplierPercentVar)
	if !ok || multiplier <= 0 {
		multiplier = 400
	}
	maxSeconds, ok := cfg.GetInt(config.HotplugAdaptiveMaxSecondsVar)
	if !ok || maxSeconds <= 0 {
		maxSeconds = 1800
	}
	return opennebula.AdaptiveTimeoutConfig{
		Enabled:              enabled,
		MinSamples:           minSamples,
		SampleWindow:         sampleWindow,
		P95MultiplierPercent: multiplier,
		MaxTimeout:           time.Duration(maxSeconds) * time.Second,
	}
}

func (d *Driver) inventoryAuthorityMode() string {
	value, ok := d.PluginConfig.GetString(config.InventoryDatastoreAuthorityModeVar)
	if !ok {
		return inventorycache.AuthorityModeStrict
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return inventorycache.AuthorityModeStrict
	}
	return value
}

const adaptiveTimeoutObservationsConfigMapName = "opennebula-csi-hotplug-observations"

func (d *Driver) adaptiveTimeoutSourceID() string {
	if d == nil {
		return ""
	}
	if strings.TrimSpace(d.nodeID) == "" {
		return "controller"
	}
	return sanitizeConfigMapDataKey("node." + strings.TrimSpace(d.nodeID))
}

func sanitizeConfigMapDataKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func (d *Driver) loadAdaptiveTimeoutObservations(ctx context.Context) error {
	if d == nil || d.kubeRuntime == nil || !d.kubeRuntime.enabled || d.adaptiveTimeouts == nil {
		return nil
	}
	cm, err := d.kubeRuntime.GetConfigMap(ctx, namespaceFromServiceAccount(), adaptiveTimeoutObservationsConfigMapName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	payload, ok := cm.Data[d.adaptiveTimeoutSourceID()]
	if !ok || strings.TrimSpace(payload) == "" {
		return nil
	}
	return d.adaptiveTimeouts.UnmarshalJSON([]byte(payload))
}

func (d *Driver) persistAdaptiveTimeoutObservations(ctx context.Context) {
	if d == nil || d.kubeRuntime == nil || !d.kubeRuntime.enabled || d.adaptiveTimeouts == nil {
		return
	}
	payload, err := d.adaptiveTimeouts.MarshalJSON()
	if err != nil {
		klog.V(4).InfoS("Failed to marshal adaptive timeout observations", "source", d.adaptiveTimeoutSourceID(), "err", err)
		return
	}
	if err := d.kubeRuntime.UpsertConfigMapData(ctx, namespaceFromServiceAccount(), adaptiveTimeoutObservationsConfigMapName, map[string]string{
		d.adaptiveTimeoutSourceID(): string(payload),
	}); err != nil {
		klog.V(4).InfoS("Failed to persist adaptive timeout observations", "source", d.adaptiveTimeoutSourceID(), "err", err)
	}
}

func (d *Driver) observeAdaptiveTimeout(ctx context.Context, operation, backend string, sizeBytes int64, duration time.Duration) {
	if d == nil || d.adaptiveTimeouts == nil || duration <= 0 {
		return
	}
	key := opennebula.NormalizeObservationKey(operation, backend, sizeBytes)
	d.adaptiveTimeouts.Observe(key, duration)
	d.persistAdaptiveTimeoutObservations(ctx)
}
