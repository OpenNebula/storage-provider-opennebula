package driver

import (
	"context"
	"testing"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func newMaintenanceTestDriver(t *testing.T, objects ...runtime.Object) *Driver {
	t.Helper()
	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.MaintenanceReleaseMinSecondsVar, 10)
	cfg.OverrideVal(config.MaintenanceReleaseMaxSecondsVar, 20)
	runtime := &KubeRuntime{client: fake.NewSimpleClientset(objects...), enabled: true}
	driver := &Driver{
		name:              DefaultDriverName,
		PluginConfig:      cfg,
		kubeRuntime:       runtime,
		metrics:           NewDriverMetrics("test", "test"),
		stickyAttachments: NewStickyAttachmentManager(runtime, "default"),
	}
	driver.maintenanceMode = NewMaintenanceModeManager(driver, "default")
	return driver
}

func TestMaintenanceModeReconcilePreparesLocalRWOPV(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-maint", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pvc.Spec.VolumeName = pv.Name
	pvName := pv.Name
	va := &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "va-maint"},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: DefaultDriverName,
			NodeName: "node-a",
			Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pvName},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: true},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: hotplugStateConfigMapName, Namespace: "default"},
		Data:       map[string]string{maintenanceModeKey: "true"},
	}
	driver := newMaintenanceTestDriver(t, pv, pvc, va, cm)

	require.NoError(t, driver.maintenanceMode.Reconcile(context.Background()))
	assert.True(t, driver.maintenanceMode.Active())
	assert.True(t, driver.maintenanceMode.Ready())

	updatedCM, err := driver.kubeRuntime.client.CoreV1().ConfigMaps("default").Get(context.Background(), hotplugStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", updatedCM.Data[maintenanceReadyKey])

	updatedPV, err := driver.kubeRuntime.client.CoreV1().PersistentVolumes().Get(context.Background(), pv.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "node-a", updatedPV.Annotations[annotationLastAttachedNode])
}

func TestMaintenanceModePrepareHonorsPreferredLastNodeOverride(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-maint-override", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pvc.Spec.VolumeName = pv.Name
	pv.Annotations[annotationPreferredLastNode] = "node-operator"
	pvName := pv.Name
	va := &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "va-maint-override"},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: DefaultDriverName,
			NodeName: "node-stale",
			Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pvName},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: true},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: hotplugStateConfigMapName, Namespace: "default"},
		Data:       map[string]string{maintenanceModeKey: "true"},
	}
	driver := newMaintenanceTestDriver(t, pv, pvc, va, cm)

	require.NoError(t, driver.maintenanceMode.Reconcile(context.Background()))

	updatedPV, err := driver.kubeRuntime.client.CoreV1().PersistentVolumes().Get(context.Background(), pv.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "node-operator", updatedPV.Annotations[annotationLastAttachedNode])
}

func TestMaintenanceModeReconcileClearsReadyAndStaggersRelease(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: hotplugStateConfigMapName, Namespace: "default"},
		Data: map[string]string{
			maintenanceModeKey:       "false",
			maintenanceReadyKey:      "true",
			maintenanceReadyAliasKey: "true",
		},
	}
	driver := newMaintenanceTestDriver(t, cm)
	require.NoError(t, driver.stickyAttachments.StartGrace(StickyAttachmentState{
		VolumeID:     "vol-held",
		NodeID:       "node-a",
		Backend:      "local",
		PVCNamespace: "default",
		PVCName:      "pvc-held",
		StartedAt:    time.Now().Add(-time.Minute),
		ExpiresAt:    time.Now().Add(time.Hour),
		GraceSeconds: 3600,
		Reason:       maintenanceStickyReason,
	}))

	require.NoError(t, driver.maintenanceMode.Reconcile(context.Background()))
	assert.False(t, driver.maintenanceMode.Active())

	updatedCM, err := driver.kubeRuntime.client.CoreV1().ConfigMaps("default").Get(context.Background(), hotplugStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updatedCM.Data, maintenanceReadyKey)
	assert.NotContains(t, updatedCM.Data, maintenanceReadyAliasKey)

	state, ok := driver.stickyAttachments.Get("vol-held")
	require.True(t, ok)
	assert.Equal(t, maintenanceReleaseReason, state.Reason)
	assert.GreaterOrEqual(t, state.GraceSeconds, 10)
	assert.LessOrEqual(t, state.GraceSeconds, 20)
}

func TestMaintenanceModeActiveSupportsAlias(t *testing.T) {
	active, alias := maintenanceModeActive(map[string]string{maintenanceModeAliasKey: "true"})
	assert.True(t, active)
	assert.True(t, alias)
}
