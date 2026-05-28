package driver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func newWebhookTestDriver(objects ...runtime.Object) *Driver {
	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.LastNodePreferenceEnabledVar, true)
	cfg.OverrideVal(config.LastNodePreferenceWebhookEnabledVar, true)
	runtime := &KubeRuntime{client: fake.NewSimpleClientset(objects...), enabled: true}
	return &Driver{
		name:         DefaultDriverName,
		PluginConfig: cfg,
		kubeRuntime:  runtime,
		metrics:      NewDriverMetrics("test", "test"),
	}
}

func TestLastNodePreferenceWebhookInjectsSoftAffinity(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-1", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pv.Annotations[annotationLastAttachedNode] = "node-a"
	pvc.Spec.VolumeName = pv.Name
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{corev1.LabelHostname: "host-a"}}}
	driver := newWebhookTestDriver(pv, pvc, node)
	webhook := NewLastNodePreferenceWebhook(driver)

	patch, warnings, err := webhook.buildPatch(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-0", Namespace: "default"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name},
				},
			}},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, warnings)
	require.NotEmpty(t, patch)

	raw, err := json.Marshal(patch)
	require.NoError(t, err)
	assert.Contains(t, string(raw), jsonPointerEscape(annotationPreferredLastNode))
	assert.Contains(t, string(raw), jsonPointerEscape(annotationLastNodeInjected))
	assert.Contains(t, string(raw), "host-a")
}

func TestLastNodePreferenceWebhookInjectsHardAffinityDuringMaintenance(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-1", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pv.Annotations[annotationLastAttachedNode] = "node-a"
	pvc.Spec.VolumeName = pv.Name
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{corev1.LabelHostname: "host-a"}}}
	driver := newWebhookTestDriver(pv, pvc, node)
	driver.maintenanceMode = NewMaintenanceModeManager(driver, "default")
	driver.maintenanceMode.setState(true, true, false)
	webhook := NewLastNodePreferenceWebhook(driver)

	patch, warnings, err := webhook.buildPatch(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-0", Namespace: "default"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name},
				},
			}},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, warnings)
	require.NotEmpty(t, patch)

	raw, err := json.Marshal(patch)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "requiredDuringSchedulingIgnoredDuringExecution")
	assert.Contains(t, string(raw), "host-a")
	assert.NotContains(t, string(raw), "preferredDuringSchedulingIgnoredDuringExecution")
}

func TestLastNodePreferenceWebhookRejectsConflictingNodesDuringMaintenance(t *testing.T) {
	pv1, pvc1 := newLocalPVAndPVC("vol-1", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pv2, pvc2 := newLocalPVAndPVC("vol-2", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pv1.Annotations[annotationLastAttachedNode] = "node-a"
	pv2.Annotations[annotationLastAttachedNode] = "node-b"
	pvc1.Spec.VolumeName = pv1.Name
	pvc2.Spec.VolumeName = pv2.Name
	driver := newWebhookTestDriver(pv1, pvc1, pv2, pvc2)
	driver.maintenanceMode = NewMaintenanceModeManager(driver, "default")
	driver.maintenanceMode.setState(true, true, false)
	webhook := NewLastNodePreferenceWebhook(driver)

	patch, warnings, err := webhook.buildPatch(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-0", Namespace: "default"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "data-a", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc1.Name}}},
				{Name: "data-b", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc2.Name}}},
			},
		},
	})
	require.Error(t, err)
	assert.Empty(t, patch)
	assert.NotEmpty(t, warnings)
}

func TestLastNodePreferenceWebhookSkipsConflictingPVCs(t *testing.T) {
	pv1, pvc1 := newLocalPVAndPVC("vol-1", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pv2, pvc2 := newLocalPVAndPVC("vol-2", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pv1.Annotations[annotationLastAttachedNode] = "node-a"
	pv2.Annotations[annotationLastAttachedNode] = "node-b"
	pvc1.Spec.VolumeName = pv1.Name
	pvc2.Spec.VolumeName = pv2.Name
	driver := newWebhookTestDriver(pv1, pvc1, pv2, pvc2)
	webhook := NewLastNodePreferenceWebhook(driver)

	patch, warnings, err := webhook.buildPatch(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-0", Namespace: "default"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "data-a", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc1.Name}}},
				{Name: "data-b", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc2.Name}}},
			},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, patch)
	assert.NotEmpty(t, warnings)
}

func TestLastNodePreferenceWebhookHonorsOptOut(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-1", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pv.Annotations[annotationLastAttachedNode] = "node-a"
	pvc.Spec.VolumeName = pv.Name
	driver := newWebhookTestDriver(pv, pvc)
	webhook := NewLastNodePreferenceWebhook(driver)

	patch, warnings, err := webhook.buildPatch(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "minio-0",
			Namespace:   "default",
			Annotations: map[string]string{annotationLastNodePref: lastNodePreferenceDisabledValue},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name},
				},
			}},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, patch)
	assert.Empty(t, warnings)
}
