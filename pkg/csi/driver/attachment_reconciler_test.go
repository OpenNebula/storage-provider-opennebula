package driver

import (
	"context"
	"testing"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func newAttachmentTestDriver(objects ...runtime.Object) *Driver {
	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.StuckAttachmentReconcilerEnabledVar, true)
	runtime := &KubeRuntime{client: fake.NewSimpleClientset(objects...), enabled: true}
	return &Driver{
		name:           DefaultDriverName,
		PluginConfig:   cfg,
		kubeRuntime:    runtime,
		metrics:        NewDriverMetrics("test", "test"),
		operationLocks: NewOperationLocks(),
		hotplugGuard:   NewHotplugGuard(time.Minute),
	}
}

func TestAttachmentReconcilerDetachesOrphanAttachment(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-1", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	driver := newAttachmentTestDriver(pv, pvc)
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	mockProvider.On("ListCurrentAttachments", mock.Anything).Return([]opennebula.ObservedAttachment{{
		VolumeHandle: "vol-1",
		ImageID:      1,
		NodeName:     "node-a",
		NodeID:       101,
		Backend:      "local",
	}}, nil).Once()
	mockProvider.On("GetVolumeInNode", mock.Anything, 1, 101).Return("vdb", nil).Once()
	mockProvider.On("DetachVolume", mock.Anything, "vol-1", "node-a").Return(nil).Once()
	mockProvider.On("ResolveVolumeSizeBytes", mock.Anything, "vol-1").Return(int64(1024), nil).Once()

	server := NewControllerServer(driver, mockProvider, &MockSharedFilesystemProviderTestify{})
	reconciler := NewAttachmentReconciler(server)
	reconciler.orphanSeen["vol-1@node-a"] = time.Now().Add(-2 * reconciler.orphanGrace)

	require.NoError(t, reconciler.ReconcileOnce(context.Background()))
	mockProvider.AssertExpectations(t)
}

func TestAttachmentReconcilerDeletesStaleVolumeAttachment(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-1", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	va := &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "va-1"},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: DefaultDriverName,
			NodeName: "node-a",
			Source: storagev1.VolumeAttachmentSource{
				PersistentVolumeName: &pv.Name,
			},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: true},
	}
	driver := newAttachmentTestDriver(pv, pvc, va)
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	mockProvider.On("ListCurrentAttachments", mock.Anything).Return([]opennebula.ObservedAttachment{}, nil).Once()

	server := NewControllerServer(driver, mockProvider, &MockSharedFilesystemProviderTestify{})
	reconciler := NewAttachmentReconciler(server)
	reconciler.staleVASeen["va-1"] = time.Now().Add(-2 * reconciler.staleVAGrace)

	require.NoError(t, reconciler.ReconcileOnce(context.Background()))

	_, err := driver.kubeRuntime.client.StorageV1().VolumeAttachments().Get(context.Background(), "va-1", metav1.GetOptions{})
	assert.Error(t, err)
	mockProvider.AssertExpectations(t)
}
