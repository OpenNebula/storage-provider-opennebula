package driver

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
)

const (
	driverMetricsPath = "/metrics"
)

type DriverMetrics struct {
	registry *prometheus.Registry

	operationTotal               *prometheus.CounterVec
	operationDuration            *prometheus.HistogramVec
	datastoreSelectionTotal      *prometheus.CounterVec
	attachValidationTotal        *prometheus.CounterVec
	cephFSSubvolumeTotal         *prometheus.CounterVec
	localVolumeHealthTotal       *prometheus.CounterVec
	snapshotTotal                *prometheus.CounterVec
	preflightTotal               *prometheus.CounterVec
	hotplugGuardTotal            *prometheus.CounterVec
	hotplugTimeoutTotal          *prometheus.CounterVec
	controllerPublishDuration    *prometheus.HistogramVec
	nodeStageDuration            *prometheus.HistogramVec
	nodeDeviceResolutionDuration *prometheus.HistogramVec
	nodeDeviceResolutionTotal    *prometheus.CounterVec
	deviceCacheTotal             *prometheus.CounterVec
	hotplugRecoveryTotal         *prometheus.CounterVec
	stickyDetachTotal            *prometheus.CounterVec
	stickyDetachResidency        *prometheus.HistogramVec
	sameNodeRestartReuseTotal    *prometheus.CounterVec
	hotplugQueueDepth            *prometheus.GaugeVec
	hotplugQueueWait             *prometheus.HistogramVec
	hotplugQueueDispatchTotal    *prometheus.CounterVec
	hotplugQueueCoalescedTotal   *prometheus.CounterVec
	hotplugDiagnosisTotal        *prometheus.CounterVec
	hotplugStateAge              *prometheus.GaugeVec
	lastNodePreferenceTotal      *prometheus.CounterVec
	maintenanceActive            prometheus.Gauge
	maintenanceProtectedVolumes  prometheus.Gauge
	maintenanceTotal             *prometheus.CounterVec
	maintenanceBlockedTotal      *prometheus.CounterVec
	metadataDriftTotal           *prometheus.CounterVec
	volumeQuarantineTotal        *prometheus.CounterVec
	hostArtifactQuarantineTotal  *prometheus.CounterVec
	attachmentReconcilerTotal    *prometheus.CounterVec
	hotplugRecommendedTimeout    *prometheus.GaugeVec
	hotplugObservationSamples    *prometheus.GaugeVec
	datastoreFreeBytes           *prometheus.GaugeVec
	datastoreTotalBytes          *prometheus.GaugeVec
	buildInfo                    *prometheus.GaugeVec
}

func NewDriverMetrics(version, commit string) *DriverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	metrics := &DriverMetrics{
		registry: registry,
		operationTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_operation_total",
			Help: "Total number of CSI driver operations partitioned by operation, backend, and outcome.",
		}, []string{"operation", "backend", "outcome"}),
		operationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "opennebula_csi_operation_duration_seconds",
			Help:    "Duration of CSI driver operations partitioned by operation, backend, and outcome.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation", "backend", "outcome"}),
		datastoreSelectionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_datastore_selection_total",
			Help: "Total number of datastore selection outcomes by policy, backend, datastore, and outcome.",
		}, []string{"policy", "backend", "datastore_id", "outcome"}),
		attachValidationTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_attach_validation_total",
			Help: "Total number of attach validation outcomes by backend and deployment mode.",
		}, []string{"backend", "mode", "outcome"}),
		cephFSSubvolumeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_cephfs_subvolume_total",
			Help: "Total number of CephFS subvolume operations by operation and outcome.",
		}, []string{"operation", "outcome"}),
		localVolumeHealthTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_local_volume_health_total",
			Help: "Total number of local RWO mount health and recovery outcomes by operation and outcome.",
		}, []string{"operation", "outcome"}),
		snapshotTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_snapshot_total",
			Help: "Total number of snapshot operations by backend, operation, and outcome.",
		}, []string{"backend", "operation", "outcome"}),
		preflightTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_preflight_total",
			Help: "Total number of preflight checks by check name and outcome.",
		}, []string{"check", "outcome"}),
		hotplugGuardTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_hotplug_guard_total",
			Help: "Total number of hotplug guard decisions by operation, reason, and outcome.",
		}, []string{"operation", "reason", "outcome"}),
		hotplugTimeoutTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_hotplug_timeout_total",
			Help: "Total number of block hotplug timeout outcomes by operation, backend, and outcome.",
		}, []string{"operation", "backend", "outcome"}),
		controllerPublishDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "opennebula_csi_controller_publish_duration_seconds",
			Help:    "Duration of ControllerPublishVolume operations by backend and outcome.",
			Buckets: prometheus.DefBuckets,
		}, []string{"backend", "outcome"}),
		nodeStageDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "opennebula_csi_node_stage_duration_seconds",
			Help:    "Duration of NodeStageVolume operations by backend and outcome.",
			Buckets: prometheus.DefBuckets,
		}, []string{"backend", "outcome"}),
		nodeDeviceResolutionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "opennebula_csi_node_device_resolution_duration_seconds",
			Help:    "Duration of node device path resolution by backend and outcome.",
			Buckets: prometheus.DefBuckets,
		}, []string{"backend", "outcome"}),
		nodeDeviceResolutionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_node_device_resolution_total",
			Help: "Total number of node device resolution attempts partitioned by method and outcome.",
		}, []string{"method", "outcome"}),
		deviceCacheTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_device_cache_total",
			Help: "Total number of node device cache operations by operation and outcome.",
		}, []string{"operation", "outcome"}),
		hotplugRecoveryTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_hotplug_recovery_total",
			Help: "Total number of hotplug recovery path decisions by operation, reason, and outcome.",
		}, []string{"operation", "reason", "outcome"}),
		stickyDetachTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_sticky_detach_total",
			Help: "Total number of sticky detach lifecycle outcomes by outcome and reason.",
		}, []string{"outcome", "reason"}),
		stickyDetachResidency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "opennebula_csi_sticky_detach_residency_seconds",
			Help:    "How long a sticky detach state remained active before completion.",
			Buckets: prometheus.DefBuckets,
		}, []string{"outcome"}),
		sameNodeRestartReuseTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_same_node_restart_reuse_total",
			Help: "Total number of same-node restart attachment reuses by backend.",
		}, []string{"backend"}),
		hotplugQueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "opennebula_csi_hotplug_queue_depth",
			Help: "Current hotplug queue depth by priority.",
		}, []string{"priority"}),
		hotplugQueueWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "opennebula_csi_hotplug_queue_wait_seconds",
			Help:    "Time spent waiting in the hotplug queue by operation and outcome.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation", "outcome"}),
		hotplugQueueDispatchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_hotplug_queue_dispatch_total",
			Help: "Total number of hotplug queue dispatch decisions by operation, priority, and outcome.",
		}, []string{"operation", "priority", "outcome"}),
		hotplugQueueCoalescedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_hotplug_queue_coalesced_total",
			Help: "Total number of duplicate hotplug queue requests coalesced by operation and outcome.",
		}, []string{"operation", "outcome"}),
		hotplugDiagnosisTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_hotplug_diagnosis_total",
			Help: "Total number of OpenNebula hotplug diagnosis observations by classification and reason.",
		}, []string{"classification", "reason"}),
		hotplugStateAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "opennebula_csi_hotplug_state_age_seconds",
			Help: "Observed age of a node hotplug state by node, LCM state, and classification.",
		}, []string{"node", "lcm_state", "classification"}),
		lastNodePreferenceTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_last_node_preference_total",
			Help: "Total number of last-node preference webhook outcomes by outcome and reason.",
		}, []string{"outcome", "reason"}),
		maintenanceActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "opennebula_csi_maintenance_active",
			Help: "Whether ConfigMap-driven maintenance mode is currently active.",
		}),
		maintenanceProtectedVolumes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "opennebula_csi_maintenance_protected_volumes",
			Help: "Number of local single-writer volumes prepared during the latest maintenance scan.",
		}),
		maintenanceTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_maintenance_total",
			Help: "Total number of maintenance mode lifecycle and preparation outcomes.",
		}, []string{"operation", "outcome"}),
		maintenanceBlockedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_maintenance_block_total",
			Help: "Total number of requests blocked by maintenance mode by operation and reason.",
		}, []string{"operation", "reason"}),
		metadataDriftTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_metadata_drift_total",
			Help: "Total number of OpenNebula attachment metadata drift observations by classification and outcome.",
		}, []string{"classification", "outcome"}),
		volumeQuarantineTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_volume_quarantine_total",
			Help: "Total number of volume quarantine decisions by reason and outcome.",
		}, []string{"reason", "outcome"}),
		hostArtifactQuarantineTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_host_artifact_quarantine_total",
			Help: "Total number of host artifact quarantine decisions by classification and outcome.",
		}, []string{"classification", "outcome"}),
		attachmentReconcilerTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opennebula_csi_attachment_reconciler_total",
			Help: "Total number of attachment reconciliation checks by check, action, and outcome.",
		}, []string{"check", "action", "outcome"}),
		hotplugRecommendedTimeout: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "opennebula_csi_hotplug_recommended_timeout_seconds",
			Help: "Latest adaptive hotplug timeout recommendation by operation, backend, and size bucket.",
		}, []string{"operation", "backend", "size_bucket"}),
		hotplugObservationSamples: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "opennebula_csi_hotplug_observation_samples",
			Help: "Number of adaptive timeout observations by operation, backend, and size bucket.",
		}, []string{"operation", "backend", "size_bucket"}),
		datastoreFreeBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "opennebula_csi_datastore_free_bytes",
			Help: "Latest observed free capacity for a datastore by backend and datastore ID.",
		}, []string{"backend", "datastore_id"}),
		datastoreTotalBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "opennebula_csi_datastore_total_bytes",
			Help: "Latest observed total capacity for a datastore by backend and datastore ID.",
		}, []string{"backend", "datastore_id"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "opennebula_csi_build_info",
			Help: "Build metadata for the SparkAI OpenNebula CSI driver.",
		}, []string{"version", "commit"}),
	}

	registry.MustRegister(
		metrics.operationTotal,
		metrics.operationDuration,
		metrics.datastoreSelectionTotal,
		metrics.attachValidationTotal,
		metrics.cephFSSubvolumeTotal,
		metrics.localVolumeHealthTotal,
		metrics.snapshotTotal,
		metrics.preflightTotal,
		metrics.hotplugGuardTotal,
		metrics.hotplugTimeoutTotal,
		metrics.controllerPublishDuration,
		metrics.nodeStageDuration,
		metrics.nodeDeviceResolutionDuration,
		metrics.nodeDeviceResolutionTotal,
		metrics.deviceCacheTotal,
		metrics.hotplugRecoveryTotal,
		metrics.stickyDetachTotal,
		metrics.stickyDetachResidency,
		metrics.sameNodeRestartReuseTotal,
		metrics.hotplugQueueDepth,
		metrics.hotplugQueueWait,
		metrics.hotplugQueueDispatchTotal,
		metrics.hotplugQueueCoalescedTotal,
		metrics.hotplugDiagnosisTotal,
		metrics.hotplugStateAge,
		metrics.lastNodePreferenceTotal,
		metrics.maintenanceActive,
		metrics.maintenanceProtectedVolumes,
		metrics.maintenanceTotal,
		metrics.maintenanceBlockedTotal,
		metrics.metadataDriftTotal,
		metrics.volumeQuarantineTotal,
		metrics.hostArtifactQuarantineTotal,
		metrics.attachmentReconcilerTotal,
		metrics.hotplugRecommendedTimeout,
		metrics.hotplugObservationSamples,
		metrics.datastoreFreeBytes,
		metrics.datastoreTotalBytes,
		metrics.buildInfo,
	)

	metrics.buildInfo.WithLabelValues(version, commit).Set(1)

	return metrics
}

func (m *DriverMetrics) RecordOperation(operation, backend, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.operationTotal.WithLabelValues(operation, backend, outcome).Inc()
	m.operationDuration.WithLabelValues(operation, backend, outcome).Observe(duration.Seconds())
}

func (m *DriverMetrics) RecordDatastoreSelection(policy, backend string, datastoreID int, outcome string) {
	if m == nil {
		return
	}
	m.datastoreSelectionTotal.WithLabelValues(policy, backend, strconv.Itoa(datastoreID), outcome).Inc()
}

func (m *DriverMetrics) RecordAttachValidation(backend, mode, outcome string) {
	if m == nil {
		return
	}
	m.attachValidationTotal.WithLabelValues(backend, mode, outcome).Inc()
}

func (m *DriverMetrics) RecordCephFSSubvolume(operation, outcome string) {
	if m == nil {
		return
	}
	m.cephFSSubvolumeTotal.WithLabelValues(operation, outcome).Inc()
}

func (m *DriverMetrics) RecordLocalVolumeHealth(operation, outcome string) {
	if m == nil {
		return
	}
	m.localVolumeHealthTotal.WithLabelValues(operation, outcome).Inc()
}

func (m *DriverMetrics) RecordSnapshot(backend, operation, outcome string) {
	if m == nil {
		return
	}
	m.snapshotTotal.WithLabelValues(backend, operation, outcome).Inc()
}

func (m *DriverMetrics) RecordPreflight(check, outcome string) {
	if m == nil {
		return
	}
	m.preflightTotal.WithLabelValues(check, outcome).Inc()
}

func (m *DriverMetrics) RecordHotplugGuard(operation, reason, outcome string) {
	if m == nil {
		return
	}
	m.hotplugGuardTotal.WithLabelValues(operation, reason, outcome).Inc()
}

func (m *DriverMetrics) RecordHotplugTimeout(operation, backend, outcome string) {
	if m == nil {
		return
	}
	m.hotplugTimeoutTotal.WithLabelValues(operation, backend, outcome).Inc()
}

func (m *DriverMetrics) RecordControllerPublishDuration(backend, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.controllerPublishDuration.WithLabelValues(backend, outcome).Observe(duration.Seconds())
}

func (m *DriverMetrics) RecordNodeStageDuration(backend, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.nodeStageDuration.WithLabelValues(backend, outcome).Observe(duration.Seconds())
}

func (m *DriverMetrics) RecordNodeDeviceResolutionDuration(backend, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.nodeDeviceResolutionDuration.WithLabelValues(backend, outcome).Observe(duration.Seconds())
}

func (m *DriverMetrics) RecordNodeDeviceResolution(method, outcome string) {
	if m == nil {
		return
	}
	m.nodeDeviceResolutionTotal.WithLabelValues(method, outcome).Inc()
}

func (m *DriverMetrics) RecordDeviceCache(operation, outcome string) {
	if m == nil {
		return
	}
	m.deviceCacheTotal.WithLabelValues(operation, outcome).Inc()
}

func (m *DriverMetrics) RecordHotplugRecovery(operation, reason, outcome string) {
	if m == nil {
		return
	}
	m.hotplugRecoveryTotal.WithLabelValues(operation, reason, outcome).Inc()
}

func (m *DriverMetrics) RecordStickyDetach(outcome, reason string) {
	if m == nil {
		return
	}
	m.stickyDetachTotal.WithLabelValues(outcome, reason).Inc()
}

func (m *DriverMetrics) RecordStickyDetachResidency(outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.stickyDetachResidency.WithLabelValues(outcome).Observe(duration.Seconds())
}

func (m *DriverMetrics) RecordSameNodeRestartReuse(backend string) {
	if m == nil {
		return
	}
	m.sameNodeRestartReuseTotal.WithLabelValues(backend).Inc()
}

func (m *DriverMetrics) SetHotplugQueueDepth(priority string, depth int) {
	if m == nil {
		return
	}
	m.hotplugQueueDepth.WithLabelValues(priority).Set(float64(depth))
}

func (m *DriverMetrics) RecordHotplugQueueWait(operation, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.hotplugQueueWait.WithLabelValues(operation, outcome).Observe(duration.Seconds())
}

func (m *DriverMetrics) RecordHotplugQueueDispatch(operation, priority, outcome string) {
	if m == nil {
		return
	}
	m.hotplugQueueDispatchTotal.WithLabelValues(operation, priority, outcome).Inc()
}

func (m *DriverMetrics) RecordHotplugQueueCoalesced(operation, outcome string) {
	if m == nil {
		return
	}
	m.hotplugQueueCoalescedTotal.WithLabelValues(operation, outcome).Inc()
}

func (m *DriverMetrics) RecordHotplugDiagnosis(classification, reason string) {
	if m == nil {
		return
	}
	m.hotplugDiagnosisTotal.WithLabelValues(classification, reason).Inc()
}

func (m *DriverMetrics) SetHotplugStateAge(node, lcmState, classification string, ageSeconds int64) {
	if m == nil {
		return
	}
	m.hotplugStateAge.WithLabelValues(node, lcmState, classification).Set(float64(ageSeconds))
}

func (m *DriverMetrics) RecordLastNodePreference(outcome, reason string) {
	if m == nil {
		return
	}
	m.lastNodePreferenceTotal.WithLabelValues(outcome, reason).Inc()
}

func (m *DriverMetrics) SetMaintenanceActive(active bool) {
	if m == nil {
		return
	}
	if active {
		m.maintenanceActive.Set(1)
		return
	}
	m.maintenanceActive.Set(0)
}

func (m *DriverMetrics) SetMaintenanceProtectedVolumes(count int) {
	if m == nil {
		return
	}
	m.maintenanceProtectedVolumes.Set(float64(count))
}

func (m *DriverMetrics) RecordMaintenance(operation, outcome string) {
	if m == nil {
		return
	}
	m.maintenanceTotal.WithLabelValues(operation, outcome).Inc()
}

func (m *DriverMetrics) RecordMaintenanceBlock(operation, reason string) {
	if m == nil {
		return
	}
	m.maintenanceBlockedTotal.WithLabelValues(operation, reason).Inc()
}

func (m *DriverMetrics) RecordMetadataDrift(classification, outcome string) {
	if m == nil {
		return
	}
	m.metadataDriftTotal.WithLabelValues(classification, outcome).Inc()
}

func (m *DriverMetrics) RecordVolumeQuarantine(reason, outcome string) {
	if m == nil {
		return
	}
	m.volumeQuarantineTotal.WithLabelValues(reason, outcome).Inc()
}

func (m *DriverMetrics) RecordHostArtifactQuarantine(classification, outcome string) {
	if m == nil {
		return
	}
	m.hostArtifactQuarantineTotal.WithLabelValues(classification, outcome).Inc()
}

func (m *DriverMetrics) RecordAttachmentReconciler(check, action, outcome string) {
	if m == nil {
		return
	}
	m.attachmentReconcilerTotal.WithLabelValues(check, action, outcome).Inc()
}

func (m *DriverMetrics) SetHotplugRecommendation(operation, backend, sizeBucket string, timeout time.Duration, samples int) {
	if m == nil {
		return
	}
	m.hotplugRecommendedTimeout.WithLabelValues(operation, backend, sizeBucket).Set(timeout.Seconds())
	m.hotplugObservationSamples.WithLabelValues(operation, backend, sizeBucket).Set(float64(samples))
}

func (m *DriverMetrics) SetDatastoreCapacity(backend string, datastoreID int, freeBytes, totalBytes int64) {
	if m == nil {
		return
	}
	labels := []string{backend, strconv.Itoa(datastoreID)}
	m.datastoreFreeBytes.WithLabelValues(labels...).Set(float64(freeBytes))
	m.datastoreTotalBytes.WithLabelValues(labels...).Set(float64(totalBytes))
}

type MetricsServer struct {
	endpoint string
	server   *http.Server
}

func NewMetricsServer(cfg config.CSIPluginConfig, metrics *DriverMetrics) *MetricsServer {
	endpoint, ok := cfg.GetString(config.MetricsEndpointVar)
	if !ok {
		endpoint = ""
	}
	if endpoint == "" || metrics == nil {
		return &MetricsServer{}
	}

	mux := http.NewServeMux()
	mux.Handle(driverMetricsPath, promhttp.HandlerFor(metrics.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))

	return &MetricsServer{
		endpoint: endpoint,
		server: &http.Server{
			Addr:              endpoint,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

func (s *MetricsServer) Start() {
	if s == nil || s.server == nil || s.endpoint == "" {
		return
	}

	go func() {
		klog.InfoS("Starting driver metrics server", "endpoint", s.endpoint, "path", driverMetricsPath)
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.ErrorS(err, "Driver metrics server failed", "endpoint", s.endpoint)
		}
	}()
}

func (s *MetricsServer) Stop(ctx context.Context) {
	if s == nil || s.server == nil {
		return
	}
	if err := s.server.Shutdown(ctx); err != nil {
		klog.ErrorS(err, "Failed to stop driver metrics server")
	}
}
