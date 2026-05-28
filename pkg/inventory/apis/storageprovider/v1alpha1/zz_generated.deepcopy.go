package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *ObjectRefSummary) DeepCopyInto(out *ObjectRefSummary) {
	*out = *in
}

func (in *ObjectRefSummary) DeepCopy() *ObjectRefSummary {
	if in == nil {
		return nil
	}
	out := new(ObjectRefSummary)
	in.DeepCopyInto(out)
	return out
}

func (in *ValidationJobTemplate) DeepCopyInto(out *ValidationJobTemplate) {
	*out = *in
	if in.AccessModes != nil {
		out.AccessModes = append([]corev1.PersistentVolumeAccessMode(nil), in.AccessModes...)
	}
	if in.FioArgs != nil {
		out.FioArgs = append([]string(nil), in.FioArgs...)
	}
	if in.NodeSelector != nil {
		out.NodeSelector = make(map[string]string, len(in.NodeSelector))
		for key, value := range in.NodeSelector {
			out.NodeSelector[key] = value
		}
	}
	if in.Tolerations != nil {
		out.Tolerations = make([]corev1.Toleration, len(in.Tolerations))
		copy(out.Tolerations, in.Tolerations)
	}
	in.Resources.DeepCopyInto(&out.Resources)
	if in.ActiveDeadlineSeconds != nil {
		value := *in.ActiveDeadlineSeconds
		out.ActiveDeadlineSeconds = &value
	}
	if in.TTLSecondsAfterFinished != nil {
		value := *in.TTLSecondsAfterFinished
		out.TTLSecondsAfterFinished = &value
	}
}

func (in *ValidationJobTemplate) DeepCopy() *ValidationJobTemplate {
	if in == nil {
		return nil
	}
	out := new(ValidationJobTemplate)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenNebulaDatastoreValidationSpec) DeepCopyInto(out *OpenNebulaDatastoreValidationSpec) {
	*out = *in
	in.JobTemplate.DeepCopyInto(&out.JobTemplate)
}

func (in *OpenNebulaDatastoreDiscoverySpec) DeepCopyInto(out *OpenNebulaDatastoreDiscoverySpec) {
	*out = *in
}

func (in *OpenNebulaDatastoreSpec) DeepCopyInto(out *OpenNebulaDatastoreSpec) {
	*out = *in
	in.Validation.DeepCopyInto(&out.Validation)
	in.Discovery.DeepCopyInto(&out.Discovery)
}

func (in *OpenNebulaDatastoreOpenNebulaStatus) DeepCopyInto(out *OpenNebulaDatastoreOpenNebulaStatus) {
	*out = *in
	if in.ClusterIDs != nil {
		out.ClusterIDs = append([]int(nil), in.ClusterIDs...)
	}
	if in.CompatibleSystemDatastores != nil {
		out.CompatibleSystemDatastores = append([]int(nil), in.CompatibleSystemDatastores...)
	}
}

func (in *OpenNebulaDatastoreCapacityStatus) DeepCopyInto(out *OpenNebulaDatastoreCapacityStatus) {
	*out = *in
}

func (in *OpenNebulaDatastoreUsageStatus) DeepCopyInto(out *OpenNebulaDatastoreUsageStatus) {
	*out = *in
	if in.StorageClasses != nil {
		out.StorageClasses = append([]string(nil), in.StorageClasses...)
	}
	if in.BoundClaims != nil {
		out.BoundClaims = make([]ObjectRefSummary, len(in.BoundClaims))
		copy(out.BoundClaims, in.BoundClaims)
	}
}

func (in *StorageClassDetail) DeepCopyInto(out *StorageClassDetail) {
	*out = *in
	if in.Warnings != nil {
		out.Warnings = append([]string(nil), in.Warnings...)
	}
}

func (in *StorageClassDetail) DeepCopy() *StorageClassDetail {
	if in == nil {
		return nil
	}
	out := new(StorageClassDetail)
	in.DeepCopyInto(out)
	return out
}

func (in *ValidationResult) DeepCopyInto(out *ValidationResult) {
	*out = *in
	if in.ReadIops != nil {
		value := *in.ReadIops
		out.ReadIops = &value
	}
	if in.WriteIops != nil {
		value := *in.WriteIops
		out.WriteIops = &value
	}
	if in.ReadBwBytes != nil {
		value := *in.ReadBwBytes
		out.ReadBwBytes = &value
	}
	if in.WriteBwBytes != nil {
		value := *in.WriteBwBytes
		out.WriteBwBytes = &value
	}
	if in.LatencyP50Micros != nil {
		value := *in.LatencyP50Micros
		out.LatencyP50Micros = &value
	}
	if in.LatencyP99Micros != nil {
		value := *in.LatencyP99Micros
		out.LatencyP99Micros = &value
	}
}

func (in *BenchmarkThresholds) DeepCopyInto(out *BenchmarkThresholds) {
	*out = *in
	if in.MinReadIops != nil {
		value := *in.MinReadIops
		out.MinReadIops = &value
	}
	if in.MinWriteIops != nil {
		value := *in.MinWriteIops
		out.MinWriteIops = &value
	}
	if in.MinReadBwBytes != nil {
		value := *in.MinReadBwBytes
		out.MinReadBwBytes = &value
	}
	if in.MinWriteBwBytes != nil {
		value := *in.MinWriteBwBytes
		out.MinWriteBwBytes = &value
	}
	if in.MaxLatencyP99Micros != nil {
		value := *in.MaxLatencyP99Micros
		out.MaxLatencyP99Micros = &value
	}
}

func (in *BenchmarkThresholds) DeepCopy() *BenchmarkThresholds {
	if in == nil {
		return nil
	}
	out := new(BenchmarkThresholds)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenNebulaDatastoreValidationStatus) DeepCopyInto(out *OpenNebulaDatastoreValidationStatus) {
	*out = *in
	if in.LastRunTime != nil {
		value := in.LastRunTime.DeepCopy()
		out.LastRunTime = value
	}
	if in.LastCompletedTime != nil {
		value := in.LastCompletedTime.DeepCopy()
		out.LastCompletedTime = value
	}
	in.Result.DeepCopyInto(&out.Result)
}

func (in *OpenNebulaDatastoreBenchmarkRunSpec) DeepCopyInto(out *OpenNebulaDatastoreBenchmarkRunSpec) {
	*out = *in
	if in.AccessModes != nil {
		out.AccessModes = append([]corev1.PersistentVolumeAccessMode(nil), in.AccessModes...)
	}
	if in.FioArgs != nil {
		out.FioArgs = append([]string(nil), in.FioArgs...)
	}
	in.Thresholds.DeepCopyInto(&out.Thresholds)
	if in.NodeSelector != nil {
		out.NodeSelector = make(map[string]string, len(in.NodeSelector))
		for key, value := range in.NodeSelector {
			out.NodeSelector[key] = value
		}
	}
	if in.Tolerations != nil {
		out.Tolerations = make([]corev1.Toleration, len(in.Tolerations))
		copy(out.Tolerations, in.Tolerations)
	}
	in.Resources.DeepCopyInto(&out.Resources)
	if in.ActiveDeadlineSeconds != nil {
		value := *in.ActiveDeadlineSeconds
		out.ActiveDeadlineSeconds = &value
	}
	if in.TTLSecondsAfterFinished != nil {
		value := *in.TTLSecondsAfterFinished
		out.TTLSecondsAfterFinished = &value
	}
}

func (in *OpenNebulaDatastoreBenchmarkRunStatus) DeepCopyInto(out *OpenNebulaDatastoreBenchmarkRunStatus) {
	*out = *in
	if in.StartedAt != nil {
		value := in.StartedAt.DeepCopy()
		out.StartedAt = value
	}
	if in.CompletedAt != nil {
		value := in.CompletedAt.DeepCopy()
		out.CompletedAt = value
	}
	in.Result.DeepCopyInto(&out.Result)
}

func (in *OpenNebulaDatastoreBenchmarkRun) DeepCopyInto(out *OpenNebulaDatastoreBenchmarkRun) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *OpenNebulaDatastoreBenchmarkRun) DeepCopy() *OpenNebulaDatastoreBenchmarkRun {
	if in == nil {
		return nil
	}
	out := new(OpenNebulaDatastoreBenchmarkRun)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenNebulaDatastoreBenchmarkRun) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *OpenNebulaDatastoreBenchmarkRunList) DeepCopyInto(out *OpenNebulaDatastoreBenchmarkRunList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]OpenNebulaDatastoreBenchmarkRun, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *OpenNebulaDatastoreBenchmarkRunList) DeepCopy() *OpenNebulaDatastoreBenchmarkRunList {
	if in == nil {
		return nil
	}
	out := new(OpenNebulaDatastoreBenchmarkRunList)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenNebulaDatastoreBenchmarkRunList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *OpenNebulaDatastoreStatus) DeepCopyInto(out *OpenNebulaDatastoreStatus) {
	*out = *in
	in.OpenNebula.DeepCopyInto(&out.OpenNebula)
	in.Capacity.DeepCopyInto(&out.Capacity)
	in.Usage.DeepCopyInto(&out.Usage)
	if in.StorageClassDetails != nil {
		out.StorageClassDetails = make([]StorageClassDetail, len(in.StorageClassDetails))
		for i := range in.StorageClassDetails {
			in.StorageClassDetails[i].DeepCopyInto(&out.StorageClassDetails[i])
		}
	}
	in.Validation.DeepCopyInto(&out.Validation)
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}

func (in *OpenNebulaDatastore) DeepCopyInto(out *OpenNebulaDatastore) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *OpenNebulaDatastore) DeepCopy() *OpenNebulaDatastore {
	if in == nil {
		return nil
	}
	out := new(OpenNebulaDatastore)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenNebulaDatastore) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *OpenNebulaDatastoreList) DeepCopyInto(out *OpenNebulaDatastoreList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]OpenNebulaDatastore, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *OpenNebulaDatastoreList) DeepCopy() *OpenNebulaDatastoreList {
	if in == nil {
		return nil
	}
	out := new(OpenNebulaDatastoreList)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenNebulaDatastoreList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *OpenNebulaNodeSpec) DeepCopyInto(out *OpenNebulaNodeSpec) {
	*out = *in
}

func (in *NodeConditionSummary) DeepCopyInto(out *NodeConditionSummary) {
	*out = *in
}

func (in *AttachedDiskSummary) DeepCopyInto(out *AttachedDiskSummary) {
	*out = *in
}

func (in *OpenNebulaNodeKubernetesStatus) DeepCopyInto(out *OpenNebulaNodeKubernetesStatus) {
	*out = *in
	if in.Labels != nil {
		out.Labels = make(map[string]string, len(in.Labels))
		for key, value := range in.Labels {
			out.Labels[key] = value
		}
	}
	if in.Taints != nil {
		out.Taints = make([]corev1.Taint, len(in.Taints))
		copy(out.Taints, in.Taints)
	}
	if in.Conditions != nil {
		out.Conditions = make([]NodeConditionSummary, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
	if in.Addresses != nil {
		out.Addresses = make([]corev1.NodeAddress, len(in.Addresses))
		copy(out.Addresses, in.Addresses)
	}
}

func (in *OpenNebulaNodeOpenNebulaStatus) DeepCopyInto(out *OpenNebulaNodeOpenNebulaStatus) {
	*out = *in
}

func (in *OpenNebulaNodeStorageStatus) DeepCopyInto(out *OpenNebulaNodeStorageStatus) {
	*out = *in
	if in.AttachedVolumeIDs != nil {
		out.AttachedVolumeIDs = append([]string(nil), in.AttachedVolumeIDs...)
	}
	if in.AttachedPersistentDisks != nil {
		out.AttachedPersistentDisks = make([]AttachedDiskSummary, len(in.AttachedPersistentDisks))
		copy(out.AttachedPersistentDisks, in.AttachedPersistentDisks)
	}
}

func (in *OpenNebulaNodeHotplugStatus) DeepCopyInto(out *OpenNebulaNodeHotplugStatus) {
	*out = *in
	if in.CooldownExpiresAt != nil {
		out.CooldownExpiresAt = in.CooldownExpiresAt.DeepCopy()
	}
	if in.LastObservedAttached != nil {
		value := *in.LastObservedAttached
		out.LastObservedAttached = &value
	}
	if in.LastObservedReady != nil {
		value := *in.LastObservedReady
		out.LastObservedReady = &value
	}
	if in.KubernetesReady != nil {
		value := *in.KubernetesReady
		out.KubernetesReady = &value
	}
	if in.OpenNebulaReady != nil {
		value := *in.OpenNebulaReady
		out.OpenNebulaReady = &value
	}
	if in.Unschedulable != nil {
		value := *in.Unschedulable
		out.Unschedulable = &value
	}
	in.Diagnosis.DeepCopyInto(&out.Diagnosis)
}

func (in *OpenNebulaNodeHotplugDiagnosisStatus) DeepCopyInto(out *OpenNebulaNodeHotplugDiagnosisStatus) {
	*out = *in
	if in.FirstObservedAt != nil {
		out.FirstObservedAt = in.FirstObservedAt.DeepCopy()
	}
	if in.LastObservedAt != nil {
		out.LastObservedAt = in.LastObservedAt.DeepCopy()
	}
	if in.LastChangedAt != nil {
		out.LastChangedAt = in.LastChangedAt.DeepCopy()
	}
}

func (in *OpenNebulaNodeHotplugDiagnosisStatus) DeepCopy() *OpenNebulaNodeHotplugDiagnosisStatus {
	if in == nil {
		return nil
	}
	out := new(OpenNebulaNodeHotplugDiagnosisStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenNebulaNodeStatus) DeepCopyInto(out *OpenNebulaNodeStatus) {
	*out = *in
	in.Kubernetes.DeepCopyInto(&out.Kubernetes)
	in.OpenNebula.DeepCopyInto(&out.OpenNebula)
	in.Storage.DeepCopyInto(&out.Storage)
	in.Hotplug.DeepCopyInto(&out.Hotplug)
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}

func (in *OpenNebulaNode) DeepCopyInto(out *OpenNebulaNode) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *OpenNebulaNode) DeepCopy() *OpenNebulaNode {
	if in == nil {
		return nil
	}
	out := new(OpenNebulaNode)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenNebulaNode) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *OpenNebulaNodeList) DeepCopyInto(out *OpenNebulaNodeList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]OpenNebulaNode, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *OpenNebulaNodeList) DeepCopy() *OpenNebulaNodeList {
	if in == nil {
		return nil
	}
	out := new(OpenNebulaNodeList)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenNebulaNodeList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}
