package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	DatastoreValidationModeDisabled = "Disabled"
	DatastoreValidationModeManual   = "Manual"

	DatastorePhaseEnabled     = "Enabled"
	DatastorePhaseAvailable   = "Available"
	DatastorePhaseDisabled    = "Disabled"
	DatastorePhaseUnavailable = "Unavailable"

	DatastoreHealthHealthy         = "Healthy"
	DatastoreHealthBackendMismatch = "BackendMismatch"
	DatastoreHealthNotFound        = "NotFound"
	DatastoreHealthLookupFailed    = "LookupFailed"
	DatastoreHealthInvalid         = "Invalid"

	NodePhaseReady    = "Ready"
	NodePhaseDegraded = "Degraded"
	NodePhaseDisabled = "Disabled"
	NodePhaseNotFound = "NotFound"

	ValidationPhaseIdle      = "Idle"
	ValidationPhaseRunning   = "Running"
	ValidationPhaseSucceeded = "Succeeded"
	ValidationPhaseFailed    = "Failed"
)

type ObjectRefSummary struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
}

type ValidationJobTemplate struct {
	Image                   string                              `json:"image,omitempty"`
	ImagePullPolicy         corev1.PullPolicy                   `json:"imagePullPolicy,omitempty"`
	StorageClassName        string                              `json:"storageClassName,omitempty"`
	Size                    string                              `json:"size,omitempty"`
	AccessModes             []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
	FioArgs                 []string                            `json:"fioArgs,omitempty"`
	NodeSelector            map[string]string                   `json:"nodeSelector,omitempty"`
	Tolerations             []corev1.Toleration                 `json:"tolerations,omitempty"`
	Resources               corev1.ResourceRequirements         `json:"resources,omitempty"`
	ActiveDeadlineSeconds   *int64                              `json:"activeDeadlineSeconds,omitempty"`
	TTLSecondsAfterFinished *int32                              `json:"ttlSecondsAfterFinished,omitempty"`
}

type OpenNebulaDatastoreValidationSpec struct {
	Mode        string                `json:"mode,omitempty"`
	JobTemplate ValidationJobTemplate `json:"jobTemplate,omitempty"`
	RunNonce    string                `json:"runNonce,omitempty"`
}

type OpenNebulaDatastoreDiscoverySpec struct {
	OpenNebulaDatastoreID int    `json:"opennebulaDatastoreID"`
	ExpectedBackend       string `json:"expectedBackend,omitempty"`
	Allowed               bool   `json:"allowed,omitempty"`
}

type OpenNebulaDatastoreSpec struct {
	DisplayName        string                            `json:"displayName,omitempty"`
	Enabled            bool                              `json:"enabled,omitempty"`
	MaintenanceMode    bool                              `json:"maintenanceMode,omitempty"`
	MaintenanceMessage string                            `json:"maintenanceMessage,omitempty"`
	Validation         OpenNebulaDatastoreValidationSpec `json:"validation,omitempty"`
	Discovery          OpenNebulaDatastoreDiscoverySpec  `json:"discovery"`
	Notes              string                            `json:"notes,omitempty"`
}

type OpenNebulaDatastoreOpenNebulaStatus struct {
	ID                         int    `json:"id,omitempty"`
	Name                       string `json:"name,omitempty"`
	State                      string `json:"state,omitempty"`
	DSMad                      string `json:"dsMad,omitempty"`
	TMMad                      string `json:"tmMad,omitempty"`
	Type                       string `json:"type,omitempty"`
	ClusterIDs                 []int  `json:"clusterIDs,omitempty"`
	CompatibleSystemDatastores []int  `json:"compatibleSystemDatastores,omitempty"`
}

type OpenNebulaDatastoreCapacityStatus struct {
	TotalBytes         int64 `json:"totalBytes,omitempty"`
	FreeBytes          int64 `json:"freeBytes,omitempty"`
	UsedBytesApprox    int64 `json:"usedBytesApprox,omitempty"`
	ProvisionedPVBytes int64 `json:"provisionedPVBytes,omitempty"`
	RequestedPVCBytes  int64 `json:"requestedPVCBytes,omitempty"`
}

type OpenNebulaDatastoreUsageStatus struct {
	PersistentVolumeCount int32              `json:"persistentVolumeCount,omitempty"`
	BoundPVCCount         int32              `json:"boundPVCCount,omitempty"`
	AttachedVolumeCount   int32              `json:"attachedVolumeCount,omitempty"`
	StorageClasses        []string           `json:"storageClasses,omitempty"`
	BoundClaims           []ObjectRefSummary `json:"boundClaims,omitempty"`
}

type StorageClassDetail struct {
	Name                 string   `json:"name,omitempty"`
	VolumeBindingMode    string   `json:"volumeBindingMode,omitempty"`
	AllowVolumeExpansion bool     `json:"allowVolumeExpansion,omitempty"`
	BackendCompatible    bool     `json:"backendCompatible,omitempty"`
	Warnings             []string `json:"warnings,omitempty"`
}

type ValidationResult struct {
	ReadIops         *int64 `json:"readIops,omitempty"`
	WriteIops        *int64 `json:"writeIops,omitempty"`
	ReadBwBytes      *int64 `json:"readBwBytes,omitempty"`
	WriteBwBytes     *int64 `json:"writeBwBytes,omitempty"`
	LatencyP50Micros *int64 `json:"latencyP50Micros,omitempty"`
	LatencyP99Micros *int64 `json:"latencyP99Micros,omitempty"`
}

type BenchmarkThresholds struct {
	MinReadIops         *int64 `json:"minReadIops,omitempty"`
	MinWriteIops        *int64 `json:"minWriteIops,omitempty"`
	MinReadBwBytes      *int64 `json:"minReadBwBytes,omitempty"`
	MinWriteBwBytes     *int64 `json:"minWriteBwBytes,omitempty"`
	MaxLatencyP99Micros *int64 `json:"maxLatencyP99Micros,omitempty"`
}

type OpenNebulaDatastoreValidationStatus struct {
	LastRunTime       *metav1.Time     `json:"lastRunTime,omitempty"`
	LastCompletedTime *metav1.Time     `json:"lastCompletedTime,omitempty"`
	LastRunNonce      string           `json:"lastRunNonce,omitempty"`
	Phase             string           `json:"phase,omitempty"`
	JobName           string           `json:"jobName,omitempty"`
	Message           string           `json:"message,omitempty"`
	Result            ValidationResult `json:"result,omitempty"`
}

type OpenNebulaDatastoreStatus struct {
	ObservedGeneration       int64                               `json:"observedGeneration,omitempty"`
	Phase                    string                              `json:"phase,omitempty"`
	Health                   string                              `json:"health,omitempty"`
	ID                       int                                 `json:"id,omitempty"`
	Name                     string                              `json:"name,omitempty"`
	Type                     string                              `json:"type,omitempty"`
	Backend                  string                              `json:"backend,omitempty"`
	CapacityDisplay          string                              `json:"capacityDisplay,omitempty"`
	StorageClassesDisplay    string                              `json:"storageClassesDisplay,omitempty"`
	MetricsDisplay           string                              `json:"metricsDisplay,omitempty"`
	ReferencedByStorageClass bool                                `json:"referencedByStorageClass,omitempty"`
	ReferenceCount           int32                               `json:"referenceCount,omitempty"`
	ValidationEligible       bool                                `json:"validationEligible,omitempty"`
	ValidationLastOutcome    string                              `json:"validationLastOutcome,omitempty"`
	LastValidationAge        string                              `json:"lastValidationAge,omitempty"`
	LastValidationPhase      string                              `json:"lastValidationPhase,omitempty"`
	LastValidationSummary    string                              `json:"lastValidationSummary,omitempty"`
	OpenNebula               OpenNebulaDatastoreOpenNebulaStatus `json:"opennebula,omitempty"`
	Capacity                 OpenNebulaDatastoreCapacityStatus   `json:"capacity,omitempty"`
	Usage                    OpenNebulaDatastoreUsageStatus      `json:"usage,omitempty"`
	StorageClassDetails      []StorageClassDetail                `json:"storageClassDetails,omitempty"`
	Validation               OpenNebulaDatastoreValidationStatus `json:"validation,omitempty"`
	Conditions               []metav1.Condition                  `json:"conditions,omitempty"`
}

type OpenNebulaDatastoreBenchmarkRunSpec struct {
	DatastoreID             int                                 `json:"datastoreID"`
	StorageClassName        string                              `json:"storageClassName,omitempty"`
	Profile                 string                              `json:"profile,omitempty"`
	Size                    string                              `json:"size,omitempty"`
	AccessModes             []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
	FioArgs                 []string                            `json:"fioArgs,omitempty"`
	Thresholds              BenchmarkThresholds                 `json:"thresholds,omitempty"`
	Image                   string                              `json:"image,omitempty"`
	ImagePullPolicy         corev1.PullPolicy                   `json:"imagePullPolicy,omitempty"`
	NodeSelector            map[string]string                   `json:"nodeSelector,omitempty"`
	Tolerations             []corev1.Toleration                 `json:"tolerations,omitempty"`
	Resources               corev1.ResourceRequirements         `json:"resources,omitempty"`
	ActiveDeadlineSeconds   *int64                              `json:"activeDeadlineSeconds,omitempty"`
	TTLSecondsAfterFinished *int32                              `json:"ttlSecondsAfterFinished,omitempty"`
	Notes                   string                              `json:"notes,omitempty"`
}

type OpenNebulaDatastoreBenchmarkRunStatus struct {
	ObservedGeneration int64            `json:"observedGeneration,omitempty"`
	Phase              string           `json:"phase,omitempty"`
	DatastoreID        int              `json:"datastoreID,omitempty"`
	DatastoreName      string           `json:"datastoreName,omitempty"`
	Backend            string           `json:"backend,omitempty"`
	JobName            string           `json:"jobName,omitempty"`
	PVCName            string           `json:"pvcName,omitempty"`
	StartedAt          *metav1.Time     `json:"startedAt,omitempty"`
	CompletedAt        *metav1.Time     `json:"completedAt,omitempty"`
	Summary            string           `json:"summary,omitempty"`
	Message            string           `json:"message,omitempty"`
	FailureLog         string           `json:"failureLog,omitempty"`
	Result             ValidationResult `json:"result,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=onedsbench
type OpenNebulaDatastoreBenchmarkRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OpenNebulaDatastoreBenchmarkRunSpec   `json:"spec,omitempty"`
	Status OpenNebulaDatastoreBenchmarkRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type OpenNebulaDatastoreBenchmarkRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OpenNebulaDatastoreBenchmarkRun `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=oneds
type OpenNebulaDatastore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OpenNebulaDatastoreSpec   `json:"spec,omitempty"`
	Status OpenNebulaDatastoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type OpenNebulaDatastoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OpenNebulaDatastore `json:"items"`
}

type OpenNebulaNodeSpec struct {
	OpenNebulaVMName string `json:"opennebulaVMName,omitempty"`
	ClusterName      string `json:"clusterName,omitempty"`
	Enabled          bool   `json:"enabled,omitempty"`
}

type NodeConditionSummary struct {
	Type               string      `json:"type,omitempty"`
	Status             string      `json:"status,omitempty"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

type AttachedDiskSummary struct {
	VolumeHandle     string `json:"volumeHandle,omitempty"`
	PersistentDiskID int    `json:"persistentDiskID,omitempty"`
	DeviceName       string `json:"deviceName,omitempty"`
	ClaimNamespace   string `json:"claimNamespace,omitempty"`
	ClaimName        string `json:"claimName,omitempty"`
}

type OpenNebulaNodeKubernetesStatus struct {
	Name       string                 `json:"name,omitempty"`
	UID        string                 `json:"uid,omitempty"`
	ProviderID string                 `json:"providerID,omitempty"`
	Labels     map[string]string      `json:"labels,omitempty"`
	Taints     []corev1.Taint         `json:"taints,omitempty"`
	Conditions []NodeConditionSummary `json:"conditions,omitempty"`
	Addresses  []corev1.NodeAddress   `json:"addresses,omitempty"`
}

type OpenNebulaNodeOpenNebulaStatus struct {
	VMID                int    `json:"vmID,omitempty"`
	VMName              string `json:"vmName,omitempty"`
	State               string `json:"state,omitempty"`
	LCMState            string `json:"lcmState,omitempty"`
	HostID              int    `json:"hostID,omitempty"`
	HostName            string `json:"hostName,omitempty"`
	ClusterID           int    `json:"clusterID,omitempty"`
	SystemDatastoreID   int    `json:"systemDatastoreID,omitempty"`
	SystemDatastoreName string `json:"systemDatastoreName,omitempty"`
}

type OpenNebulaNodeStorageStatus struct {
	TopologySystemDatastore string                `json:"topologySystemDatastore,omitempty"`
	AttachedVolumeCount     int32                 `json:"attachedVolumeCount,omitempty"`
	AttachedVolumeIDs       []string              `json:"attachedVolumeIDs,omitempty"`
	AttachedPersistentDisks []AttachedDiskSummary `json:"attachedPersistentDisks,omitempty"`
}

type OpenNebulaNodeHotplugStatus struct {
	Ready                 bool                                 `json:"ready,omitempty"`
	InCooldown            bool                                 `json:"inCooldown,omitempty"`
	CooldownExpiresAt     *metav1.Time                         `json:"cooldownExpiresAt,omitempty"`
	LastCooldownOperation string                               `json:"lastCooldownOperation,omitempty"`
	LastCooldownVolume    string                               `json:"lastCooldownVolume,omitempty"`
	LastObservedAttached  *bool                                `json:"lastObservedAttached,omitempty"`
	LastObservedReady     *bool                                `json:"lastObservedReady,omitempty"`
	FailureCount          int                                  `json:"failureCount,omitempty"`
	Reason                string                               `json:"reason,omitempty"`
	PauseUntilReady       bool                                 `json:"pauseUntilReady,omitempty"`
	KubernetesReady       *bool                                `json:"kubernetesReady,omitempty"`
	OpenNebulaReady       *bool                                `json:"openNebulaReady,omitempty"`
	Unschedulable         *bool                                `json:"unschedulable,omitempty"`
	Diagnosis             OpenNebulaNodeHotplugDiagnosisStatus `json:"diagnosis,omitempty"`
}

type OpenNebulaNodeHotplugDiagnosisStatus struct {
	Classification    string       `json:"classification,omitempty"`
	Operation         string       `json:"operation,omitempty"`
	VolumeHandle      string       `json:"volumeHandle,omitempty"`
	PersistentDiskID  int          `json:"persistentDiskID,omitempty"`
	VMDiskID          int          `json:"vmDiskID,omitempty"`
	DiskTarget        string       `json:"diskTarget,omitempty"`
	DiskAttachFlag    string       `json:"diskAttachFlag,omitempty"`
	FirstObservedAt   *metav1.Time `json:"firstObservedAt,omitempty"`
	LastObservedAt    *metav1.Time `json:"lastObservedAt,omitempty"`
	LastChangedAt     *metav1.Time `json:"lastChangedAt,omitempty"`
	AgeSeconds        int64        `json:"ageSeconds,omitempty"`
	StuckAfterSeconds int64        `json:"stuckAfterSeconds,omitempty"`
	Reason            string       `json:"reason,omitempty"`
	Message           string       `json:"message,omitempty"`
	RecommendedAction string       `json:"recommendedAction,omitempty"`
}

type OpenNebulaNodeStatus struct {
	ObservedGeneration           int64                          `json:"observedGeneration,omitempty"`
	Phase                        string                         `json:"phase,omitempty"`
	DisplayState                 string                         `json:"displayState,omitempty"`
	ActiveHotplugPressure        int32                          `json:"activeHotplugPressure,omitempty"`
	SystemDatastoreDisplay       string                         `json:"systemDatastoreDisplay,omitempty"`
	AttachedVolumeHandlesDisplay string                         `json:"attachedVolumeHandlesDisplay,omitempty"`
	Kubernetes                   OpenNebulaNodeKubernetesStatus `json:"kubernetes,omitempty"`
	OpenNebula                   OpenNebulaNodeOpenNebulaStatus `json:"opennebula,omitempty"`
	Storage                      OpenNebulaNodeStorageStatus    `json:"storage,omitempty"`
	Hotplug                      OpenNebulaNodeHotplugStatus    `json:"hotplug,omitempty"`
	Conditions                   []metav1.Condition             `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=onenode
type OpenNebulaNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OpenNebulaNodeSpec   `json:"spec,omitempty"`
	Status OpenNebulaNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type OpenNebulaNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OpenNebulaNode `json:"items"`
}
