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
package config

import (
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
)

const (
	//Config var names
	OpenNebulaRPCEndpointVar                   = "ONE_XMLRPC"
	OpenNebulaCredentialsVar                   = "ONE_AUTH"
	DefaultDatastoresVar                       = "ONE_CSI_DEFAULT_DATASTORES"
	DatastorePolicyVar                         = "ONE_CSI_DATASTORE_SELECTION_POLICY"
	AllowedDatastoreTypesVar                   = "ONE_CSI_ALLOWED_DATASTORE_TYPES"
	FeatureGatesVar                            = "ONE_CSI_FEATURE_GATES"
	MetricsEndpointVar                         = "ONE_CSI_METRICS_ENDPOINT"
	NodeTopologySystemDSVar                    = "ONE_CSI_NODE_TOPOLOGY_SYSTEM_DS"
	VMHotplugTimeoutVar                        = "ONE_CSI_VM_HOTPLUG_TIMEOUT_SECONDS"
	VMHotplugTimeoutBaseVar                    = "ONE_CSI_VM_HOTPLUG_TIMEOUT_BASE_SECONDS"
	VMHotplugTimeoutPer100GiVar                = "ONE_CSI_VM_HOTPLUG_TIMEOUT_PER_100GI_SECONDS"
	VMHotplugTimeoutMaxVar                     = "ONE_CSI_VM_HOTPLUG_TIMEOUT_MAX_SECONDS"
	VMHotplugStuckCooldownSecondsVar           = "ONE_CSI_VM_HOTPLUG_STUCK_VM_COOLDOWN_SECONDS"
	NodeDeviceDiscoveryTimeoutVar              = "ONE_CSI_NODE_DEVICE_DISCOVERY_TIMEOUT_SECONDS"
	NodeExpandVerifyTimeoutSecondsVar          = "ONE_CSI_NODE_EXPAND_VERIFY_TIMEOUT_SECONDS"
	NodeExpandRetryIntervalSecondsVar          = "ONE_CSI_NODE_EXPAND_RETRY_INTERVAL_SECONDS"
	NodeExpandSizeToleranceBytesVar            = "ONE_CSI_NODE_EXPAND_SIZE_TOLERANCE_BYTES"
	NodeDeviceCacheEnabledVar                  = "ONE_CSI_NODE_DEVICE_CACHE_ENABLED"
	NodeDeviceCacheTTLSecondsVar               = "ONE_CSI_NODE_DEVICE_CACHE_TTL_SECONDS"
	NodeDeviceUdevSettleTimeoutSecondsVar      = "ONE_CSI_NODE_DEVICE_UDEV_SETTLE_TIMEOUT_SECONDS"
	NodeDeviceRescanOnMissEnabledVar           = "ONE_CSI_NODE_DEVICE_RESCAN_ON_MISS_ENABLED"
	HotplugQueueEnabledVar                     = "ONE_CSI_HOTPLUG_QUEUE_ENABLED"
	HotplugQueueMaxWaitSecondsVar              = "ONE_CSI_HOTPLUG_QUEUE_MAX_WAIT_SECONDS"
	HotplugQueueAgeBoostSecondsVar             = "ONE_CSI_HOTPLUG_QUEUE_AGE_BOOST_SECONDS"
	HotplugQueueDedupeEnabledVar               = "ONE_CSI_HOTPLUG_QUEUE_DEDUPE_ENABLED"
	HotplugQueuePerItemWaitSecondsVar          = "ONE_CSI_HOTPLUG_QUEUE_PER_ITEM_WAIT_SECONDS"
	HotplugQueueMaxWaitCapSecondsVar           = "ONE_CSI_HOTPLUG_QUEUE_MAX_WAIT_CAP_SECONDS"
	HotplugQueueMaxActiveSecondsVar            = "ONE_CSI_HOTPLUG_QUEUE_MAX_ACTIVE_SECONDS"
	HotplugQueueSnapshotDebounceSecondsVar     = "ONE_CSI_HOTPLUG_QUEUE_SNAPSHOT_DEBOUNCE_SECONDS"
	HotplugDiagnosticsEnabledVar               = "ONE_CSI_HOTPLUG_DIAGNOSTICS_ENABLED"
	HotplugDiagnosticsStuckAfterSecondsVar     = "ONE_CSI_HOTPLUG_DIAGNOSTICS_STUCK_AFTER_SECONDS"
	HotplugDiagnosticsProgressWindowSecondsVar = "ONE_CSI_HOTPLUG_DIAGNOSTICS_PROGRESS_WINDOW_SECONDS"
	HotplugDiagnosticsRecoveryModeVar          = "ONE_CSI_HOTPLUG_DIAGNOSTICS_RECOVERY_MODE"
	NodeHotplugGuardEnabledVar                 = "ONE_CSI_NODE_HOTPLUG_GUARD_ENABLED"
	NodeHotplugGuardFailureThresholdVar        = "ONE_CSI_NODE_HOTPLUG_GUARD_FAILURE_THRESHOLD"
	NodeHotplugGuardRequireKubernetesReadyVar  = "ONE_CSI_NODE_HOTPLUG_GUARD_REQUIRE_KUBERNETES_READY"
	NodeHotplugGuardRequireOpenNebulaReadyVar  = "ONE_CSI_NODE_HOTPLUG_GUARD_REQUIRE_OPENNEBULA_READY"
	LocalRestartOptimizationEnabledVar         = "ONE_CSI_LOCAL_RESTART_OPTIMIZATION_ENABLED"
	LocalRestartDetachGraceSecondsVar          = "ONE_CSI_LOCAL_RESTART_DETACH_GRACE_SECONDS"
	LocalRestartDetachGraceMaxSecondsVar       = "ONE_CSI_LOCAL_RESTART_DETACH_GRACE_MAX_SECONDS"
	LocalRestartRequireNodeReadyVar            = "ONE_CSI_LOCAL_RESTART_REQUIRE_NODE_READY"
	MaintenanceReleaseMinSecondsVar            = "ONE_CSI_MAINTENANCE_RELEASE_MIN_SECONDS"
	MaintenanceReleaseMaxSecondsVar            = "ONE_CSI_MAINTENANCE_RELEASE_MAX_SECONDS"
	MetadataDriftQuarantineEnabledVar          = "ONE_CSI_METADATA_DRIFT_QUARANTINE_ENABLED"
	MetadataDriftQuarantineFailureThresholdVar = "ONE_CSI_METADATA_DRIFT_QUARANTINE_FAILURE_THRESHOLD"
	MetadataDriftQuarantineTTLSecondsVar       = "ONE_CSI_METADATA_DRIFT_QUARANTINE_TTL_SECONDS"
	HostArtifactQuarantineEnabledVar           = "ONE_CSI_HOST_ARTIFACT_QUARANTINE_ENABLED"
	HostArtifactQuarantineFailureThresholdVar  = "ONE_CSI_HOST_ARTIFACT_QUARANTINE_FAILURE_THRESHOLD"
	HostArtifactQuarantineTTLSecondsVar        = "ONE_CSI_HOST_ARTIFACT_QUARANTINE_TTL_SECONDS"
	LocalDeviceRecoveryEnabledVar              = "ONE_CSI_LOCAL_DEVICE_RECOVERY_ENABLED"
	LocalDeviceRecoveryMinAttemptsVar          = "ONE_CSI_LOCAL_DEVICE_RECOVERY_MIN_ATTEMPTS"
	LocalDeviceRecoveryMinAgeSecondsVar        = "ONE_CSI_LOCAL_DEVICE_RECOVERY_MIN_AGE_SECONDS"
	LocalDeviceRecoveryIntervalSecondsVar      = "ONE_CSI_LOCAL_DEVICE_RECOVERY_INTERVAL_SECONDS"
	LocalDeviceRecoveryCooldownSecondsVar      = "ONE_CSI_LOCAL_DEVICE_RECOVERY_COOLDOWN_SECONDS"
	LocalDeviceRecoveryMaxAttemptsVar          = "ONE_CSI_LOCAL_DEVICE_RECOVERY_MAX_ATTEMPTS_PER_VOLUME"
	LocalRWOStaleMountActivePodRecoveryVar     = "ONE_CSI_LOCAL_RWO_STALE_MOUNT_ACTIVE_POD_RECOVERY"
	LocalRWOStaleMountMaxAttemptsVar           = "ONE_CSI_LOCAL_RWO_STALE_MOUNT_MAX_ATTEMPTS"
	LocalRWOStaleMountBackoffSecondsVar        = "ONE_CSI_LOCAL_RWO_STALE_MOUNT_BACKOFF_SECONDS"
	LastNodePreferenceEnabledVar               = "ONE_CSI_LAST_NODE_PREFERENCE_ENABLED"
	LastNodePreferencePolicyVar                = "ONE_CSI_LAST_NODE_PREFERENCE_POLICY"
	LastNodePreferenceWebhookEnabledVar        = "ONE_CSI_LAST_NODE_PREFERENCE_WEBHOOK_ENABLED"
	LastNodePreferenceWebhookPortVar           = "ONE_CSI_LAST_NODE_PREFERENCE_WEBHOOK_PORT"
	LastNodePreferenceWebhookFailurePolicyVar  = "ONE_CSI_LAST_NODE_PREFERENCE_FAILURE_POLICY"
	StuckAttachmentReconcilerEnabledVar        = "ONE_CSI_STUCK_ATTACHMENT_RECONCILER_ENABLED"
	StuckAttachmentReconcilerIntervalVar       = "ONE_CSI_STUCK_ATTACHMENT_RECONCILER_INTERVAL_SECONDS"
	StuckAttachmentOrphanGraceSecondsVar       = "ONE_CSI_STUCK_ATTACHMENT_ORPHAN_GRACE_SECONDS"
	StuckAttachmentStaleVAGraceSecondsVar      = "ONE_CSI_STUCK_ATTACHMENT_STALE_VA_GRACE_SECONDS"
	HotplugAdaptiveTimeoutEnabledVar           = "ONE_CSI_HOTPLUG_ADAPTIVE_TIMEOUT_ENABLED"
	HotplugAdaptiveMinSamplesVar               = "ONE_CSI_HOTPLUG_ADAPTIVE_MIN_SAMPLES"
	HotplugAdaptiveSampleWindowVar             = "ONE_CSI_HOTPLUG_ADAPTIVE_SAMPLE_WINDOW"
	HotplugAdaptiveP95MultiplierPercentVar     = "ONE_CSI_HOTPLUG_ADAPTIVE_P95_MULTIPLIER_PERCENT"
	HotplugAdaptiveMaxSecondsVar               = "ONE_CSI_HOTPLUG_ADAPTIVE_MAX_SECONDS"
	PreflightLocalImmediateBindingPolicyVar    = "ONE_CSI_PREFLIGHT_LOCAL_IMMEDIATE_BINDING_POLICY"
	ControllerLeaderElectionEnabledVar         = "ONE_CSI_CONTROLLER_LEADER_ELECTION_ENABLED"
	ControllerLeaderElectionLeaseNameVar       = "ONE_CSI_CONTROLLER_LEADER_ELECTION_LEASE_NAME"
	ControllerLeaderElectionLeaseNamespaceVar  = "ONE_CSI_CONTROLLER_LEADER_ELECTION_LEASE_NAMESPACE"
	ControllerLeaderElectionLeaseDurationVar   = "ONE_CSI_CONTROLLER_LEADER_ELECTION_LEASE_DURATION_SECONDS"
	ControllerLeaderElectionRenewDeadlineVar   = "ONE_CSI_CONTROLLER_LEADER_ELECTION_RENEW_DEADLINE_SECONDS"
	ControllerLeaderElectionRetryPeriodVar     = "ONE_CSI_CONTROLLER_LEADER_ELECTION_RETRY_PERIOD_SECONDS"
	InventoryControllerEnabledVar              = "ONE_CSI_INVENTORY_CONTROLLER_ENABLED"
	InventoryDatastoreAuthorityModeVar         = "ONE_CSI_INVENTORY_DATASTORE_AUTHORITY_MODE"
	InventoryControllerResyncDatastoresVar     = "ONE_CSI_INVENTORY_RESYNC_DATASTORES_SECONDS"
	InventoryControllerResyncNodesVar          = "ONE_CSI_INVENTORY_RESYNC_NODES_SECONDS"
	InventoryControllerLeaderElectionIDVar     = "ONE_CSI_INVENTORY_CONTROLLER_LEADER_ELECTION_ID"
	InventoryControllerNamespaceVar            = "ONE_CSI_INVENTORY_CONTROLLER_NAMESPACE"
	InventoryValidationEnabledVar              = "ONE_CSI_INVENTORY_VALIDATION_ENABLED"
	InventoryValidationDefaultImageVar         = "ONE_CSI_INVENTORY_VALIDATION_DEFAULT_IMAGE"

	//Default values
	defaultOpenNebulaRPCEndpoint                   = "http://localhost:2633/RPC2"
	defaultDatastorePolicy                         = "least-used"
	defaultAllowedDatastoreTypes                   = "local,ceph,cephfs"
	defaultFeatureGates                            = "compatibilityAwareSelection=true,detachedDiskExpansion=false,cephfsExpansion=false,cephfsSnapshots=false,cephfsClones=false,cephfsSelfHealing=false,localRWOStaleMountRecovery=false,topologyAccessibility=false"
	defaultMetricsEndpoint                         = ":9810"
	defaultVMHotplugTimeout                        = 60
	defaultVMHotplugTimeoutBase                    = 120
	defaultVMHotplugTimeoutStep                    = 60
	defaultVMHotplugTimeoutMax                     = 900
	defaultVMHotplugCooldown                       = 300
	defaultNodeDeviceDiscoveryTimeout              = 30
	defaultNodeExpandVerifyTimeoutSeconds          = 120
	defaultNodeExpandRetryIntervalSeconds          = 2
	defaultNodeExpandSizeToleranceBytes            = 134217728
	defaultNodeDeviceCacheEnabled                  = true
	defaultNodeDeviceCacheTTLSeconds               = 600
	defaultNodeDeviceUdevSettleTimeoutSeconds      = 10
	defaultNodeDeviceRescanOnMissEnabled           = true
	defaultHotplugQueueEnabled                     = true
	defaultHotplugQueueMaxWaitSeconds              = 180
	defaultHotplugQueueAgeBoostSeconds             = 30
	defaultHotplugQueueDedupeEnabled               = true
	defaultHotplugQueuePerItemWaitSeconds          = 60
	defaultHotplugQueueMaxWaitCapSeconds           = 900
	defaultHotplugQueueMaxActiveSeconds            = 900
	defaultHotplugQueueSnapshotDebounceSeconds     = 2
	defaultHotplugDiagnosticsEnabled               = true
	defaultHotplugDiagnosticsStuckAfterSeconds     = 300
	defaultHotplugDiagnosticsProgressWindowSeconds = 60
	defaultHotplugDiagnosticsRecoveryMode          = "readOnly"
	defaultNodeHotplugGuardEnabled                 = true
	defaultNodeHotplugGuardFailureThreshold        = 2
	defaultNodeHotplugGuardRequireKubernetesReady  = true
	defaultNodeHotplugGuardRequireOpenNebulaReady  = true
	defaultLocalRestartOptimizationEnabled         = true
	defaultLocalRestartDetachGraceSeconds          = 90
	defaultLocalRestartDetachGraceMaxSeconds       = 300
	defaultLocalRestartRequireNodeReady            = true
	defaultMaintenanceReleaseMinSeconds            = 300
	defaultMaintenanceReleaseMaxSeconds            = 1800
	defaultMetadataDriftQuarantineEnabled          = true
	defaultMetadataDriftQuarantineFailureThreshold = 2
	defaultMetadataDriftQuarantineTTLSeconds       = 1800
	defaultHostArtifactQuarantineEnabled           = true
	defaultHostArtifactQuarantineFailureThreshold  = 1
	defaultHostArtifactQuarantineTTLSeconds        = 3600
	defaultLocalDeviceRecoveryEnabled              = true
	defaultLocalDeviceRecoveryMinAttempts          = 3
	defaultLocalDeviceRecoveryMinAgeSeconds        = 60
	defaultLocalDeviceRecoveryIntervalSeconds      = 15
	defaultLocalDeviceRecoveryCooldownSeconds      = 300
	defaultLocalDeviceRecoveryMaxAttempts          = 2
	defaultLocalRWOStaleMountActivePodRecovery     = false
	defaultLocalRWOStaleMountMaxAttempts           = 3
	defaultLocalRWOStaleMountBackoffSeconds        = 10
	defaultLastNodePreferenceEnabled               = true
	defaultLastNodePreferencePolicy                = "local-single-writer"
	defaultLastNodePreferenceWebhookEnabled        = true
	defaultLastNodePreferenceWebhookPort           = 9443
	defaultLastNodePreferenceWebhookFailurePolicy  = "Ignore"
	defaultStuckAttachmentReconcilerEnabled        = true
	defaultStuckAttachmentReconcilerInterval       = 60
	defaultStuckAttachmentOrphanGraceSeconds       = 120
	defaultStuckAttachmentStaleVAGraceSeconds      = 90
	defaultHotplugAdaptiveTimeoutEnabled           = true
	defaultHotplugAdaptiveMinSamples               = 8
	defaultHotplugAdaptiveSampleWindow             = 20
	defaultHotplugAdaptiveP95MultiplierPercent     = 400
	defaultHotplugAdaptiveMaxSeconds               = 1800
	defaultPreflightLocalImmediateBindingPolicy    = "warn"
	defaultControllerLeaderElectionEnabled         = false
	defaultControllerLeaderElectionLeaseDuration   = 45
	defaultControllerLeaderElectionRenewDeadline   = 30
	defaultControllerLeaderElectionRetryPeriod     = 10
	defaultInventoryControllerEnabled              = false
	defaultInventoryDatastoreAuthorityMode         = "strict"
	defaultInventoryControllerResyncDatastores     = 60
	defaultInventoryControllerResyncNodes          = 30
	defaultInventoryControllerLeaderElectionID     = "opennebula-csi-inventory-controller"
	defaultInventoryControllerNamespace            = "kube-system"
	defaultInventoryValidationEnabled              = true
	defaultInventoryValidationDefaultImage         = ""
)

// CSIPluginConfig holds the configuration for the CSI plugin
// TODO: implement thread safety
type CSIPluginConfig struct {
	viper *viper.Viper
}

func LoadConfiguration() CSIPluginConfig {
	return CSIPluginConfig{
		viper: initViper(),
	}
}

func initViper() *viper.Viper {
	viper := viper.New()

	//TODO: Bind to flags
	viper.SetConfigName("opennebula-csi-config")
	viper.SetConfigType("yaml")
	//default config file locations
	viper.AddConfigPath(".")
	viper.AddConfigPath("$HOME/.csi-driver-opennebula/")
	viper.AddConfigPath("/etc/csi-driver-opennebula/")
	if err := viper.ReadInConfig(); err == nil {
		klog.Infof("Using config file: %s", viper.ConfigFileUsed())
	}
	viper.WatchConfig()
	viper.OnConfigChange(func(e fsnotify.Event) {
		klog.Warningf("Config file changed: %s", e.Name)
	})

	viper.SetDefault(OpenNebulaRPCEndpointVar, defaultOpenNebulaRPCEndpoint)
	viper.SetDefault(DatastorePolicyVar, defaultDatastorePolicy)
	viper.SetDefault(AllowedDatastoreTypesVar, defaultAllowedDatastoreTypes)
	viper.SetDefault(FeatureGatesVar, defaultFeatureGates)
	viper.SetDefault(MetricsEndpointVar, defaultMetricsEndpoint)
	viper.SetDefault(VMHotplugTimeoutVar, defaultVMHotplugTimeout)
	viper.SetDefault(VMHotplugTimeoutBaseVar, defaultVMHotplugTimeoutBase)
	viper.SetDefault(VMHotplugTimeoutPer100GiVar, defaultVMHotplugTimeoutStep)
	viper.SetDefault(VMHotplugTimeoutMaxVar, defaultVMHotplugTimeoutMax)
	viper.SetDefault(VMHotplugStuckCooldownSecondsVar, defaultVMHotplugCooldown)
	viper.SetDefault(NodeDeviceDiscoveryTimeoutVar, defaultNodeDeviceDiscoveryTimeout)
	viper.SetDefault(NodeExpandVerifyTimeoutSecondsVar, defaultNodeExpandVerifyTimeoutSeconds)
	viper.SetDefault(NodeExpandRetryIntervalSecondsVar, defaultNodeExpandRetryIntervalSeconds)
	viper.SetDefault(NodeExpandSizeToleranceBytesVar, defaultNodeExpandSizeToleranceBytes)
	viper.SetDefault(NodeDeviceCacheEnabledVar, defaultNodeDeviceCacheEnabled)
	viper.SetDefault(NodeDeviceCacheTTLSecondsVar, defaultNodeDeviceCacheTTLSeconds)
	viper.SetDefault(NodeDeviceUdevSettleTimeoutSecondsVar, defaultNodeDeviceUdevSettleTimeoutSeconds)
	viper.SetDefault(NodeDeviceRescanOnMissEnabledVar, defaultNodeDeviceRescanOnMissEnabled)
	viper.SetDefault(HotplugQueueEnabledVar, defaultHotplugQueueEnabled)
	viper.SetDefault(HotplugQueueMaxWaitSecondsVar, defaultHotplugQueueMaxWaitSeconds)
	viper.SetDefault(HotplugQueueAgeBoostSecondsVar, defaultHotplugQueueAgeBoostSeconds)
	viper.SetDefault(HotplugQueueDedupeEnabledVar, defaultHotplugQueueDedupeEnabled)
	viper.SetDefault(HotplugQueuePerItemWaitSecondsVar, defaultHotplugQueuePerItemWaitSeconds)
	viper.SetDefault(HotplugQueueMaxWaitCapSecondsVar, defaultHotplugQueueMaxWaitCapSeconds)
	viper.SetDefault(HotplugQueueMaxActiveSecondsVar, defaultHotplugQueueMaxActiveSeconds)
	viper.SetDefault(HotplugQueueSnapshotDebounceSecondsVar, defaultHotplugQueueSnapshotDebounceSeconds)
	viper.SetDefault(HotplugDiagnosticsEnabledVar, defaultHotplugDiagnosticsEnabled)
	viper.SetDefault(HotplugDiagnosticsStuckAfterSecondsVar, defaultHotplugDiagnosticsStuckAfterSeconds)
	viper.SetDefault(HotplugDiagnosticsProgressWindowSecondsVar, defaultHotplugDiagnosticsProgressWindowSeconds)
	viper.SetDefault(HotplugDiagnosticsRecoveryModeVar, defaultHotplugDiagnosticsRecoveryMode)
	viper.SetDefault(NodeHotplugGuardEnabledVar, defaultNodeHotplugGuardEnabled)
	viper.SetDefault(NodeHotplugGuardFailureThresholdVar, defaultNodeHotplugGuardFailureThreshold)
	viper.SetDefault(NodeHotplugGuardRequireKubernetesReadyVar, defaultNodeHotplugGuardRequireKubernetesReady)
	viper.SetDefault(NodeHotplugGuardRequireOpenNebulaReadyVar, defaultNodeHotplugGuardRequireOpenNebulaReady)
	viper.SetDefault(LocalRestartOptimizationEnabledVar, defaultLocalRestartOptimizationEnabled)
	viper.SetDefault(LocalRestartDetachGraceSecondsVar, defaultLocalRestartDetachGraceSeconds)
	viper.SetDefault(LocalRestartDetachGraceMaxSecondsVar, defaultLocalRestartDetachGraceMaxSeconds)
	viper.SetDefault(LocalRestartRequireNodeReadyVar, defaultLocalRestartRequireNodeReady)
	viper.SetDefault(MaintenanceReleaseMinSecondsVar, defaultMaintenanceReleaseMinSeconds)
	viper.SetDefault(MaintenanceReleaseMaxSecondsVar, defaultMaintenanceReleaseMaxSeconds)
	viper.SetDefault(MetadataDriftQuarantineEnabledVar, defaultMetadataDriftQuarantineEnabled)
	viper.SetDefault(MetadataDriftQuarantineFailureThresholdVar, defaultMetadataDriftQuarantineFailureThreshold)
	viper.SetDefault(MetadataDriftQuarantineTTLSecondsVar, defaultMetadataDriftQuarantineTTLSeconds)
	viper.SetDefault(HostArtifactQuarantineEnabledVar, defaultHostArtifactQuarantineEnabled)
	viper.SetDefault(HostArtifactQuarantineFailureThresholdVar, defaultHostArtifactQuarantineFailureThreshold)
	viper.SetDefault(HostArtifactQuarantineTTLSecondsVar, defaultHostArtifactQuarantineTTLSeconds)
	viper.SetDefault(LocalDeviceRecoveryEnabledVar, defaultLocalDeviceRecoveryEnabled)
	viper.SetDefault(LocalDeviceRecoveryMinAttemptsVar, defaultLocalDeviceRecoveryMinAttempts)
	viper.SetDefault(LocalDeviceRecoveryMinAgeSecondsVar, defaultLocalDeviceRecoveryMinAgeSeconds)
	viper.SetDefault(LocalDeviceRecoveryIntervalSecondsVar, defaultLocalDeviceRecoveryIntervalSeconds)
	viper.SetDefault(LocalDeviceRecoveryCooldownSecondsVar, defaultLocalDeviceRecoveryCooldownSeconds)
	viper.SetDefault(LocalDeviceRecoveryMaxAttemptsVar, defaultLocalDeviceRecoveryMaxAttempts)
	viper.SetDefault(LocalRWOStaleMountActivePodRecoveryVar, defaultLocalRWOStaleMountActivePodRecovery)
	viper.SetDefault(LocalRWOStaleMountMaxAttemptsVar, defaultLocalRWOStaleMountMaxAttempts)
	viper.SetDefault(LocalRWOStaleMountBackoffSecondsVar, defaultLocalRWOStaleMountBackoffSeconds)
	viper.SetDefault(LastNodePreferenceEnabledVar, defaultLastNodePreferenceEnabled)
	viper.SetDefault(LastNodePreferencePolicyVar, defaultLastNodePreferencePolicy)
	viper.SetDefault(LastNodePreferenceWebhookEnabledVar, defaultLastNodePreferenceWebhookEnabled)
	viper.SetDefault(LastNodePreferenceWebhookPortVar, defaultLastNodePreferenceWebhookPort)
	viper.SetDefault(LastNodePreferenceWebhookFailurePolicyVar, defaultLastNodePreferenceWebhookFailurePolicy)
	viper.SetDefault(StuckAttachmentReconcilerEnabledVar, defaultStuckAttachmentReconcilerEnabled)
	viper.SetDefault(StuckAttachmentReconcilerIntervalVar, defaultStuckAttachmentReconcilerInterval)
	viper.SetDefault(StuckAttachmentOrphanGraceSecondsVar, defaultStuckAttachmentOrphanGraceSeconds)
	viper.SetDefault(StuckAttachmentStaleVAGraceSecondsVar, defaultStuckAttachmentStaleVAGraceSeconds)
	viper.SetDefault(HotplugAdaptiveTimeoutEnabledVar, defaultHotplugAdaptiveTimeoutEnabled)
	viper.SetDefault(HotplugAdaptiveMinSamplesVar, defaultHotplugAdaptiveMinSamples)
	viper.SetDefault(HotplugAdaptiveSampleWindowVar, defaultHotplugAdaptiveSampleWindow)
	viper.SetDefault(HotplugAdaptiveP95MultiplierPercentVar, defaultHotplugAdaptiveP95MultiplierPercent)
	viper.SetDefault(HotplugAdaptiveMaxSecondsVar, defaultHotplugAdaptiveMaxSeconds)
	viper.SetDefault(PreflightLocalImmediateBindingPolicyVar, defaultPreflightLocalImmediateBindingPolicy)
	viper.SetDefault(ControllerLeaderElectionEnabledVar, defaultControllerLeaderElectionEnabled)
	viper.SetDefault(ControllerLeaderElectionLeaseDurationVar, defaultControllerLeaderElectionLeaseDuration)
	viper.SetDefault(ControllerLeaderElectionRenewDeadlineVar, defaultControllerLeaderElectionRenewDeadline)
	viper.SetDefault(ControllerLeaderElectionRetryPeriodVar, defaultControllerLeaderElectionRetryPeriod)
	viper.SetDefault(InventoryControllerEnabledVar, defaultInventoryControllerEnabled)
	viper.SetDefault(InventoryDatastoreAuthorityModeVar, defaultInventoryDatastoreAuthorityMode)
	viper.SetDefault(InventoryControllerResyncDatastoresVar, defaultInventoryControllerResyncDatastores)
	viper.SetDefault(InventoryControllerResyncNodesVar, defaultInventoryControllerResyncNodes)
	viper.SetDefault(InventoryControllerLeaderElectionIDVar, defaultInventoryControllerLeaderElectionID)
	viper.SetDefault(InventoryControllerNamespaceVar, defaultInventoryControllerNamespace)
	viper.SetDefault(InventoryValidationEnabledVar, defaultInventoryValidationEnabled)
	viper.SetDefault(InventoryValidationDefaultImageVar, defaultInventoryValidationDefaultImage)

	viper.AutomaticEnv()
	viper.SetTypeByDefaultValue(true)

	return viper
}

// Following methods are for abstracting the viper library
// and provide a cleaner interface for getting configuration values.

func (c *CSIPluginConfig) GetString(key string) (string, bool) {
	value := c.viper.GetString(key)
	return value, c.viper.IsSet(key)
}

func (c *CSIPluginConfig) GetBool(key string) (bool, bool) {
	value := c.viper.GetBool(key)
	return value, c.viper.IsSet(key)
}

func (c *CSIPluginConfig) GetInt(key string) (int, bool) {
	value := c.viper.GetInt(key)
	return value, c.viper.IsSet(key)
}

func (c *CSIPluginConfig) GetInt32(key string) (int32, bool) {
	value := c.viper.GetInt32(key)
	return value, c.viper.IsSet(key)
}

func (c *CSIPluginConfig) GetStringSlice(key string) ([]string, bool) {
	if !c.viper.IsSet(key) {
		return nil, false
	}

	if raw := c.viper.GetStringSlice(key); len(raw) > 0 {
		return normalizeCSVValues(raw), true
	}

	return normalizeCSVValues(strings.Split(c.viper.GetString(key), ",")), true
}

func (c *CSIPluginConfig) OverrideVal(key string, value any) {
	c.viper.Set(key, value)
}

func normalizeCSVValues(values []string) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			normalized = append(normalized, trimmed)
		}
	}

	return normalized
}
