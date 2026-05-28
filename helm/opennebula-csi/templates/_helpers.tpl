{{/*
Expand the name of the chart.
*/}}
{{- define "opennebula-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "opennebula-csi.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "opennebula-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "opennebula-csi.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end }}

{{- define "opennebula-csi.controllerServiceAccountName" -}}
{{- printf "%s-controller-sa" (include "opennebula-csi.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "opennebula-csi.nodeServiceAccountName" -}}
{{- printf "%s-node-sa" (include "opennebula-csi.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "opennebula-csi.inventoryServiceAccountName" -}}
{{- printf "%s-inventory-sa" (include "opennebula-csi.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "opennebula-csi.lastNodeWebhookServiceName" -}}
{{- printf "%s-last-node-webhook" (include "opennebula-csi.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "opennebula-csi.lastNodeWebhookSecretName" -}}
{{- printf "%s-last-node-webhook-tls" (include "opennebula-csi.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "opennebula-csi.authSecretName" -}}
{{- if .Values.credentials.existingSecret.name -}}
{{- .Values.credentials.existingSecret.name -}}
{{- else -}}
{{- $_ := required "Set credentials.existingSecret.name or credentials.inlineAuth" .Values.credentials.inlineAuth -}}
{{- printf "%s-auth" (include "opennebula-csi.fullname" .) -}}
{{- end -}}
{{- end }}

{{- define "opennebula-csi.authSecretKey" -}}
{{- default "credentials" .Values.credentials.existingSecret.key -}}
{{- end }}

{{- define "opennebula-csi.preflightImageRepository" -}}
{{- default .Values.image.repository .Values.preflight.image.repository -}}
{{- end }}

{{- define "opennebula-csi.preflightImageTag" -}}
{{- default .Values.image.tag .Values.preflight.image.tag -}}
{{- end }}

{{- define "opennebula-csi.driverCommonEnv" -}}
{{- $root := . -}}
{{- $inventoryEnabledOverrideSet := false -}}
{{- $inventoryEnabledOverride := false -}}
{{- if kindIs "map" . -}}
  {{- if hasKey . "context" -}}
    {{- $root = get . "context" -}}
  {{- end -}}
  {{- if hasKey . "inventoryEnabledOverride" -}}
    {{- $inventoryEnabledOverrideSet = true -}}
    {{- $inventoryEnabledOverride = (get . "inventoryEnabledOverride") -}}
  {{- end -}}
{{- end -}}
{{- $inventory := (get $root.Values "inventoryController") | default dict -}}
{{- $inventoryResync := (get $inventory "resyncSeconds") | default dict -}}
{{- $inventoryValidation := (get $inventory "validation") | default dict -}}
{{- $nodeExpand := (get $root.Values.driver "nodeExpand") | default dict -}}
{{- $nodeDeviceCache := (get $root.Values.driver "nodeDeviceCache") | default dict -}}
{{- $hotplugQueue := (get $root.Values.driver "hotplugQueue") | default dict -}}
{{- $hotplugDiagnostics := (get $root.Values.driver "hotplugDiagnostics") | default dict -}}
{{- $nodeHotplugGuard := (get $root.Values.driver "nodeHotplugGuard") | default dict -}}
{{- $localRestart := (get $root.Values.driver "localRestartOptimization") | default dict -}}
{{- $maintenanceMode := (get $root.Values.driver "maintenanceMode") | default dict -}}
{{- $metadataDriftQuarantine := (get $root.Values.driver "metadataDriftQuarantine") | default dict -}}
{{- $hostArtifactQuarantine := (get $root.Values.driver "hostArtifactQuarantine") | default dict -}}
{{- $localDeviceRecovery := (get $root.Values.driver "localDeviceRecovery") | default dict -}}
{{- $localRWORecovery := (get $root.Values.driver "localRWOStaleMountRecovery") | default dict -}}
{{- $lastNodePreference := (get $root.Values.driver "lastNodePreference") | default dict -}}
{{- $lastNodePreferenceWebhook := (get $lastNodePreference "webhook") | default dict -}}
{{- $stuckAttachmentReconciler := (get $root.Values.driver "stuckAttachmentReconciler") | default dict -}}
{{- $adaptiveTimeout := (get $root.Values.driver "adaptiveTimeout") | default dict -}}
{{- $nodeDeviceCacheEnabled := (get $nodeDeviceCache "enabled") | default true -}}
{{- $nodeDeviceCacheTTLSeconds := (get $nodeDeviceCache "ttlSeconds") | default 600 -}}
{{- $nodeDeviceUdevSettleTimeoutSeconds := (get $nodeDeviceCache "udevSettleTimeoutSeconds") | default 10 -}}
{{- $nodeDeviceRescanOnMissEnabled := (get $nodeDeviceCache "rescanOnMissEnabled") | default true -}}
{{- $nodeExpandVerifyTimeoutSeconds := (get $nodeExpand "verifyTimeoutSeconds") | default 120 -}}
{{- $nodeExpandRetryIntervalSeconds := (get $nodeExpand "retryIntervalSeconds") | default 2 -}}
{{- $nodeExpandSizeToleranceBytes := (get $nodeExpand "sizeToleranceBytes") | default 134217728 -}}
{{- $hotplugQueueEnabled := (get $hotplugQueue "enabled") | default true -}}
{{- $hotplugQueueMaxWaitSeconds := (get $hotplugQueue "maxWaitSeconds") | default 180 -}}
{{- $hotplugQueueAgeBoostSeconds := (get $hotplugQueue "ageBoostSeconds") | default 30 -}}
{{- $hotplugQueueDedupeEnabled := (get $hotplugQueue "dedupeEnabled") | default true -}}
{{- $hotplugQueuePerItemWaitSeconds := (get $hotplugQueue "perItemWaitSeconds") | default 60 -}}
{{- $hotplugQueueMaxWaitCapSeconds := (get $hotplugQueue "maxWaitCapSeconds") | default 900 -}}
{{- $hotplugQueueMaxActiveSeconds := (get $hotplugQueue "maxActiveSeconds") | default 900 -}}
{{- $hotplugQueueSnapshotDebounceSeconds := (get $hotplugQueue "snapshotDebounceSeconds") | default 2 -}}
{{- $hotplugDiagnosticsEnabled := (get $hotplugDiagnostics "enabled") | default true -}}
{{- $hotplugDiagnosticsStuckAfterSeconds := (get $hotplugDiagnostics "stuckAfterSeconds") | default 300 -}}
{{- $hotplugDiagnosticsProgressWindowSeconds := (get $hotplugDiagnostics "progressWindowSeconds") | default 60 -}}
{{- $hotplugDiagnosticsRecoveryMode := (get $hotplugDiagnostics "recoveryMode") | default "readOnly" -}}
{{- $nodeHotplugGuardEnabled := (get $nodeHotplugGuard "enabled") | default true -}}
{{- $nodeHotplugGuardFailureThreshold := (get $nodeHotplugGuard "failureThreshold") | default 2 -}}
{{- $nodeHotplugGuardRequireKubernetesReady := (get $nodeHotplugGuard "requireKubernetesReady") | default true -}}
{{- $nodeHotplugGuardRequireOpenNebulaReady := (get $nodeHotplugGuard "requireOpenNebulaReady") | default true -}}
{{- $localRestartEnabled := (get $localRestart "enabled") | default true -}}
{{- $localRestartDetachGrace := (get $localRestart "detachGraceSeconds") | default 90 -}}
{{- $localRestartDetachGraceMax := (get $localRestart "maxDetachGraceSeconds") | default 300 -}}
{{- $localRestartRequireNodeReady := (get $localRestart "requireNodeReady") | default true -}}
{{- $maintenanceReleaseMinSeconds := (get $maintenanceMode "releaseMinSeconds") | default 300 -}}
{{- $maintenanceReleaseMaxSeconds := (get $maintenanceMode "releaseMaxSeconds") | default 1800 -}}
{{- $metadataDriftQuarantineEnabled := (get $metadataDriftQuarantine "enabled") | default true -}}
{{- $metadataDriftQuarantineFailureThreshold := (get $metadataDriftQuarantine "failureThreshold") | default 2 -}}
{{- $metadataDriftQuarantineTTLSeconds := (get $metadataDriftQuarantine "ttlSeconds") | default 1800 -}}
{{- $hostArtifactQuarantineEnabled := (get $hostArtifactQuarantine "enabled") | default true -}}
{{- $hostArtifactQuarantineFailureThreshold := (get $hostArtifactQuarantine "failureThreshold") | default 1 -}}
{{- $hostArtifactQuarantineTTLSeconds := (get $hostArtifactQuarantine "ttlSeconds") | default 3600 -}}
{{- $localDeviceRecoveryEnabled := (get $localDeviceRecovery "enabled") | default true -}}
{{- $localDeviceRecoveryMinAttempts := (get $localDeviceRecovery "minAttempts") | default 3 -}}
{{- $localDeviceRecoveryMinAgeSeconds := (get $localDeviceRecovery "minAgeSeconds") | default 60 -}}
{{- $localDeviceRecoveryIntervalSeconds := (get $localDeviceRecovery "intervalSeconds") | default 15 -}}
{{- $localDeviceRecoveryCooldownSeconds := (get $localDeviceRecovery "cooldownSeconds") | default 300 -}}
{{- $localDeviceRecoveryMaxAttempts := (get $localDeviceRecovery "maxAttemptsPerVolume") | default 2 -}}
{{- $localRWORecoveryActivePod := (get $localRWORecovery "activePodRecovery") | default false -}}
{{- $localRWORecoveryMaxAttempts := (get $localRWORecovery "maxAttempts") | default 3 -}}
{{- $localRWORecoveryBackoffSeconds := (get $localRWORecovery "backoffSeconds") | default 10 -}}
{{- $lastNodePreferenceEnabled := (get $lastNodePreference "enabled") | default true -}}
{{- $lastNodePreferencePolicy := (get $lastNodePreference "policy") | default "local-single-writer" -}}
{{- $lastNodePreferenceWebhookEnabled := (get $lastNodePreferenceWebhook "enabled") | default true -}}
{{- $lastNodePreferenceWebhookPort := (get $lastNodePreferenceWebhook "port") | default 9443 -}}
{{- $lastNodePreferenceWebhookFailurePolicy := (get $lastNodePreferenceWebhook "failurePolicy") | default "Ignore" -}}
{{- $stuckAttachmentReconcilerEnabled := (get $stuckAttachmentReconciler "enabled") | default true -}}
{{- $stuckAttachmentReconcilerIntervalSeconds := (get $stuckAttachmentReconciler "intervalSeconds") | default 60 -}}
{{- $stuckAttachmentOrphanGraceSeconds := (get $stuckAttachmentReconciler "orphanGraceSeconds") | default 120 -}}
{{- $stuckAttachmentStaleVAGraceSeconds := (get $stuckAttachmentReconciler "staleVolumeAttachmentGraceSeconds") | default 90 -}}
{{- $adaptiveTimeoutEnabled := (get $adaptiveTimeout "enabled") | default true -}}
{{- $adaptiveTimeoutMinSamples := (get $adaptiveTimeout "minSamples") | default 8 -}}
{{- $adaptiveTimeoutSampleWindow := (get $adaptiveTimeout "sampleWindow") | default 20 -}}
{{- $adaptiveTimeoutP95MultiplierPercent := (get $adaptiveTimeout "p95MultiplierPercent") | default 400 -}}
{{- $adaptiveTimeoutMaxSeconds := (get $adaptiveTimeout "maxSeconds") | default 1800 -}}
- name: ONE_XMLRPC
  value: {{ $root.Values.oneApiEndpoint | quote }}
- name: ONE_AUTH
  valueFrom:
    secretKeyRef:
      name: {{ include "opennebula-csi.authSecretName" $root }}
      key: {{ include "opennebula-csi.authSecretKey" $root }}
{{- $inventoryEnabled := (get $inventory "enabled") | default false -}}
{{- if $inventoryEnabledOverrideSet -}}
{{- $inventoryEnabled = $inventoryEnabledOverride -}}
{{- end -}}
{{- $inventoryAuthorityMode := (get $inventory "authorityMode") | default "strict" -}}
{{- $inventoryResyncDatastores := (get $inventoryResync "datastores") | default 60 -}}
{{- $inventoryResyncNodes := (get $inventoryResync "nodes") | default 30 -}}
{{- $inventoryLeaderElectionID := (get $inventory "leaderElectionID") | default "opennebula-csi-inventory-controller" -}}
{{- $inventoryNamespace := (get $inventory "namespace") | default "" -}}
{{- if eq (trim (printf "%v" $inventoryNamespace)) "" -}}
{{- $inventoryNamespace = include "opennebula-csi.namespace" $root -}}
{{- end }}
{{- $inventoryValidationEnabled := (get $inventoryValidation "enabled") | default true -}}
{{- $inventoryValidationDefaultImage := (get $inventoryValidation "defaultImage") | default "alpine:3.22" -}}
{{- if $root.Values.driver.defaultDatastores }}
- name: ONE_CSI_DEFAULT_DATASTORES
  value: {{ join "," $root.Values.driver.defaultDatastores | quote }}
{{- end }}
{{- if $root.Values.driver.datastoreSelectionPolicy }}
- name: ONE_CSI_DATASTORE_SELECTION_POLICY
  value: {{ $root.Values.driver.datastoreSelectionPolicy | quote }}
{{- end }}
{{- if $root.Values.driver.allowedDatastoreTypes }}
- name: ONE_CSI_ALLOWED_DATASTORE_TYPES
  value: {{ join "," $root.Values.driver.allowedDatastoreTypes | quote }}
{{- end }}
{{- if $root.Values.metrics.driver.enabled }}
- name: ONE_CSI_METRICS_ENDPOINT
  value: {{ printf ":%v" $root.Values.metrics.driver.port | quote }}
{{- end }}
- name: ONE_CSI_VM_HOTPLUG_TIMEOUT_SECONDS
  value: {{ $root.Values.driver.vmHotplugTimeoutSeconds | quote }}
- name: ONE_CSI_VM_HOTPLUG_TIMEOUT_BASE_SECONDS
  value: {{ $root.Values.driver.vmHotplugTimeoutBaseSeconds | quote }}
- name: ONE_CSI_VM_HOTPLUG_TIMEOUT_PER_100GI_SECONDS
  value: {{ $root.Values.driver.vmHotplugTimeoutPer100GiSeconds | quote }}
- name: ONE_CSI_VM_HOTPLUG_TIMEOUT_MAX_SECONDS
  value: {{ $root.Values.driver.vmHotplugTimeoutMaxSeconds | quote }}
- name: ONE_CSI_VM_HOTPLUG_STUCK_VM_COOLDOWN_SECONDS
  value: {{ $root.Values.driver.vmHotplugStuckVmCooldownSeconds | quote }}
- name: ONE_CSI_NODE_DEVICE_DISCOVERY_TIMEOUT_SECONDS
  value: {{ $root.Values.driver.nodeDeviceDiscoveryTimeoutSeconds | quote }}
- name: ONE_CSI_NODE_EXPAND_VERIFY_TIMEOUT_SECONDS
  value: {{ $nodeExpandVerifyTimeoutSeconds | quote }}
- name: ONE_CSI_NODE_EXPAND_RETRY_INTERVAL_SECONDS
  value: {{ $nodeExpandRetryIntervalSeconds | quote }}
- name: ONE_CSI_NODE_EXPAND_SIZE_TOLERANCE_BYTES
  value: {{ $nodeExpandSizeToleranceBytes | int | quote }}
- name: ONE_CSI_NODE_DEVICE_CACHE_ENABLED
  value: {{ $nodeDeviceCacheEnabled | quote }}
- name: ONE_CSI_NODE_DEVICE_CACHE_TTL_SECONDS
  value: {{ $nodeDeviceCacheTTLSeconds | quote }}
- name: ONE_CSI_NODE_DEVICE_UDEV_SETTLE_TIMEOUT_SECONDS
  value: {{ $nodeDeviceUdevSettleTimeoutSeconds | quote }}
- name: ONE_CSI_NODE_DEVICE_RESCAN_ON_MISS_ENABLED
  value: {{ $nodeDeviceRescanOnMissEnabled | quote }}
- name: ONE_CSI_HOTPLUG_QUEUE_ENABLED
  value: {{ $hotplugQueueEnabled | quote }}
- name: ONE_CSI_HOTPLUG_QUEUE_MAX_WAIT_SECONDS
  value: {{ $hotplugQueueMaxWaitSeconds | quote }}
- name: ONE_CSI_HOTPLUG_QUEUE_AGE_BOOST_SECONDS
  value: {{ $hotplugQueueAgeBoostSeconds | quote }}
- name: ONE_CSI_HOTPLUG_QUEUE_DEDUPE_ENABLED
  value: {{ $hotplugQueueDedupeEnabled | quote }}
- name: ONE_CSI_HOTPLUG_QUEUE_PER_ITEM_WAIT_SECONDS
  value: {{ $hotplugQueuePerItemWaitSeconds | quote }}
- name: ONE_CSI_HOTPLUG_QUEUE_MAX_WAIT_CAP_SECONDS
  value: {{ $hotplugQueueMaxWaitCapSeconds | quote }}
- name: ONE_CSI_HOTPLUG_QUEUE_MAX_ACTIVE_SECONDS
  value: {{ $hotplugQueueMaxActiveSeconds | quote }}
- name: ONE_CSI_HOTPLUG_QUEUE_SNAPSHOT_DEBOUNCE_SECONDS
  value: {{ $hotplugQueueSnapshotDebounceSeconds | quote }}
- name: ONE_CSI_HOTPLUG_DIAGNOSTICS_ENABLED
  value: {{ $hotplugDiagnosticsEnabled | quote }}
- name: ONE_CSI_HOTPLUG_DIAGNOSTICS_STUCK_AFTER_SECONDS
  value: {{ $hotplugDiagnosticsStuckAfterSeconds | quote }}
- name: ONE_CSI_HOTPLUG_DIAGNOSTICS_PROGRESS_WINDOW_SECONDS
  value: {{ $hotplugDiagnosticsProgressWindowSeconds | quote }}
- name: ONE_CSI_HOTPLUG_DIAGNOSTICS_RECOVERY_MODE
  value: {{ $hotplugDiagnosticsRecoveryMode | quote }}
- name: ONE_CSI_NODE_HOTPLUG_GUARD_ENABLED
  value: {{ $nodeHotplugGuardEnabled | quote }}
- name: ONE_CSI_NODE_HOTPLUG_GUARD_FAILURE_THRESHOLD
  value: {{ $nodeHotplugGuardFailureThreshold | quote }}
- name: ONE_CSI_NODE_HOTPLUG_GUARD_REQUIRE_KUBERNETES_READY
  value: {{ $nodeHotplugGuardRequireKubernetesReady | quote }}
- name: ONE_CSI_NODE_HOTPLUG_GUARD_REQUIRE_OPENNEBULA_READY
  value: {{ $nodeHotplugGuardRequireOpenNebulaReady | quote }}
- name: ONE_CSI_LOCAL_RESTART_OPTIMIZATION_ENABLED
  value: {{ $localRestartEnabled | quote }}
- name: ONE_CSI_LOCAL_RESTART_DETACH_GRACE_SECONDS
  value: {{ $localRestartDetachGrace | quote }}
- name: ONE_CSI_LOCAL_RESTART_DETACH_GRACE_MAX_SECONDS
  value: {{ $localRestartDetachGraceMax | quote }}
- name: ONE_CSI_LOCAL_RESTART_REQUIRE_NODE_READY
  value: {{ $localRestartRequireNodeReady | quote }}
- name: ONE_CSI_MAINTENANCE_RELEASE_MIN_SECONDS
  value: {{ $maintenanceReleaseMinSeconds | quote }}
- name: ONE_CSI_MAINTENANCE_RELEASE_MAX_SECONDS
  value: {{ $maintenanceReleaseMaxSeconds | quote }}
- name: ONE_CSI_METADATA_DRIFT_QUARANTINE_ENABLED
  value: {{ $metadataDriftQuarantineEnabled | quote }}
- name: ONE_CSI_METADATA_DRIFT_QUARANTINE_FAILURE_THRESHOLD
  value: {{ $metadataDriftQuarantineFailureThreshold | quote }}
- name: ONE_CSI_METADATA_DRIFT_QUARANTINE_TTL_SECONDS
  value: {{ $metadataDriftQuarantineTTLSeconds | quote }}
- name: ONE_CSI_HOST_ARTIFACT_QUARANTINE_ENABLED
  value: {{ $hostArtifactQuarantineEnabled | quote }}
- name: ONE_CSI_HOST_ARTIFACT_QUARANTINE_FAILURE_THRESHOLD
  value: {{ $hostArtifactQuarantineFailureThreshold | quote }}
- name: ONE_CSI_HOST_ARTIFACT_QUARANTINE_TTL_SECONDS
  value: {{ $hostArtifactQuarantineTTLSeconds | quote }}
- name: ONE_CSI_LOCAL_DEVICE_RECOVERY_ENABLED
  value: {{ $localDeviceRecoveryEnabled | quote }}
- name: ONE_CSI_LOCAL_DEVICE_RECOVERY_MIN_ATTEMPTS
  value: {{ $localDeviceRecoveryMinAttempts | quote }}
- name: ONE_CSI_LOCAL_DEVICE_RECOVERY_MIN_AGE_SECONDS
  value: {{ $localDeviceRecoveryMinAgeSeconds | quote }}
- name: ONE_CSI_LOCAL_DEVICE_RECOVERY_INTERVAL_SECONDS
  value: {{ $localDeviceRecoveryIntervalSeconds | quote }}
- name: ONE_CSI_LOCAL_DEVICE_RECOVERY_COOLDOWN_SECONDS
  value: {{ $localDeviceRecoveryCooldownSeconds | quote }}
- name: ONE_CSI_LOCAL_DEVICE_RECOVERY_MAX_ATTEMPTS_PER_VOLUME
  value: {{ $localDeviceRecoveryMaxAttempts | quote }}
- name: ONE_CSI_LOCAL_RWO_STALE_MOUNT_ACTIVE_POD_RECOVERY
  value: {{ $localRWORecoveryActivePod | quote }}
- name: ONE_CSI_LOCAL_RWO_STALE_MOUNT_MAX_ATTEMPTS
  value: {{ $localRWORecoveryMaxAttempts | quote }}
- name: ONE_CSI_LOCAL_RWO_STALE_MOUNT_BACKOFF_SECONDS
  value: {{ $localRWORecoveryBackoffSeconds | quote }}
- name: ONE_CSI_LAST_NODE_PREFERENCE_ENABLED
  value: {{ $lastNodePreferenceEnabled | quote }}
- name: ONE_CSI_LAST_NODE_PREFERENCE_POLICY
  value: {{ $lastNodePreferencePolicy | quote }}
- name: ONE_CSI_LAST_NODE_PREFERENCE_WEBHOOK_ENABLED
  value: {{ $lastNodePreferenceWebhookEnabled | quote }}
- name: ONE_CSI_LAST_NODE_PREFERENCE_WEBHOOK_PORT
  value: {{ $lastNodePreferenceWebhookPort | quote }}
- name: ONE_CSI_LAST_NODE_PREFERENCE_FAILURE_POLICY
  value: {{ $lastNodePreferenceWebhookFailurePolicy | quote }}
- name: ONE_CSI_STUCK_ATTACHMENT_RECONCILER_ENABLED
  value: {{ $stuckAttachmentReconcilerEnabled | quote }}
- name: ONE_CSI_STUCK_ATTACHMENT_RECONCILER_INTERVAL_SECONDS
  value: {{ $stuckAttachmentReconcilerIntervalSeconds | quote }}
- name: ONE_CSI_STUCK_ATTACHMENT_ORPHAN_GRACE_SECONDS
  value: {{ $stuckAttachmentOrphanGraceSeconds | quote }}
- name: ONE_CSI_STUCK_ATTACHMENT_STALE_VA_GRACE_SECONDS
  value: {{ $stuckAttachmentStaleVAGraceSeconds | quote }}
- name: ONE_CSI_HOTPLUG_ADAPTIVE_TIMEOUT_ENABLED
  value: {{ $adaptiveTimeoutEnabled | quote }}
- name: ONE_CSI_HOTPLUG_ADAPTIVE_MIN_SAMPLES
  value: {{ $adaptiveTimeoutMinSamples | quote }}
- name: ONE_CSI_HOTPLUG_ADAPTIVE_SAMPLE_WINDOW
  value: {{ $adaptiveTimeoutSampleWindow | quote }}
- name: ONE_CSI_HOTPLUG_ADAPTIVE_P95_MULTIPLIER_PERCENT
  value: {{ $adaptiveTimeoutP95MultiplierPercent | quote }}
- name: ONE_CSI_HOTPLUG_ADAPTIVE_MAX_SECONDS
  value: {{ $adaptiveTimeoutMaxSeconds | quote }}
- name: ONE_CSI_PREFLIGHT_LOCAL_IMMEDIATE_BINDING_POLICY
  value: {{ $root.Values.preflight.localImmediateBindingPolicy | quote }}
- name: ONE_CSI_INVENTORY_CONTROLLER_ENABLED
  value: {{ $inventoryEnabled | quote }}
- name: ONE_CSI_INVENTORY_DATASTORE_AUTHORITY_MODE
  value: {{ $inventoryAuthorityMode | quote }}
- name: ONE_CSI_INVENTORY_RESYNC_DATASTORES_SECONDS
  value: {{ $inventoryResyncDatastores | quote }}
- name: ONE_CSI_INVENTORY_RESYNC_NODES_SECONDS
  value: {{ $inventoryResyncNodes | quote }}
- name: ONE_CSI_INVENTORY_CONTROLLER_LEADER_ELECTION_ID
  value: {{ $inventoryLeaderElectionID | quote }}
- name: ONE_CSI_INVENTORY_CONTROLLER_NAMESPACE
  value: {{ $inventoryNamespace | quote }}
- name: ONE_CSI_INVENTORY_VALIDATION_ENABLED
  value: {{ $inventoryValidationEnabled | quote }}
- name: ONE_CSI_INVENTORY_VALIDATION_DEFAULT_IMAGE
  value: {{ $inventoryValidationDefaultImage | quote }}
- name: ONE_CSI_FEATURE_GATES
  value: "compatibilityAwareSelection={{ $root.Values.featureGates.compatibilityAwareSelection }},detachedDiskExpansion={{ $root.Values.featureGates.detachedDiskExpansion }},cephfsExpansion={{ $root.Values.featureGates.cephfsExpansion }},cephfsSnapshots={{ $root.Values.featureGates.cephfsSnapshots }},cephfsClones={{ $root.Values.featureGates.cephfsClones }},cephfsSelfHealing={{ $root.Values.featureGates.cephfsSelfHealing }},cephfsPersistentRecovery={{ $root.Values.featureGates.cephfsPersistentRecovery }},cephfsKernelMounts={{ $root.Values.featureGates.cephfsKernelMounts }},localRWOStaleMountRecovery={{ $root.Values.featureGates.localRWOStaleMountRecovery }},topologyAccessibility={{ $root.Values.featureGates.topologyAccessibility }}"
{{- with $root.Values.driver.env }}
{{ toYaml . }}
{{- end }}
{{- end }}
