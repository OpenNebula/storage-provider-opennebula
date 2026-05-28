package driver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func newLocalDeviceRecoveryTestDriver(t *testing.T, objects ...runtime.Object) *Driver {
	t.Helper()
	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.LocalDeviceRecoveryEnabledVar, true)
	cfg.OverrideVal(config.LocalDeviceRecoveryMinAttemptsVar, 3)
	cfg.OverrideVal(config.LocalDeviceRecoveryMinAgeSecondsVar, 0)
	cfg.OverrideVal(config.LocalDeviceRecoveryCooldownSecondsVar, 0)
	cfg.OverrideVal(config.LocalDeviceRecoveryMaxAttemptsVar, 2)
	cfg.OverrideVal(config.HotplugQueueEnabledVar, false)
	runtime := &KubeRuntime{client: fake.NewSimpleClientset(objects...), enabled: true}
	return &Driver{
		name:           DefaultDriverName,
		PluginConfig:   cfg,
		kubeRuntime:    runtime,
		metrics:        NewDriverMetrics("test", "test"),
		operationLocks: NewOperationLocks(),
		hotplugGuard:   NewHotplugGuard(5 * time.Minute),
	}
}

func TestNodeRecordsAndClearsLocalDeviceMissingReport(t *testing.T) {
	driver := newLocalDeviceRecoveryTestDriver(t)
	driver.nodeID = "node-a"
	ns := &NodeServer{Driver: driver}
	publishContext := map[string]string{
		annotationBackend:                   "local",
		paramPVCNamespace:                   "default",
		paramPVCName:                        "pvc-vol-1",
		paramPVName:                         "pv-vol-1",
		publishContextDeviceSerial:          "onecsi-439",
		publishContextOpenNebulaImageID:     "439",
		publishContextHotplugTimeoutSeconds: "120",
	}

	ns.recordLocalDeviceMissing(context.Background(), "vol-1", "/dev/sdd", "/stage/vol-1", publishContext, errors.New("device not found"))
	ns.recordLocalDeviceMissing(context.Background(), "vol-1", "/dev/sdd", "/stage/vol-1", publishContext, errors.New("device not found"))

	cm, err := driver.kubeRuntime.client.CoreV1().ConfigMaps(namespaceFromServiceAccount()).Get(context.Background(), localDeviceStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	key := localDeviceReportKey("node-a", "vol-1")
	raw := cm.Data[key]
	require.NotEmpty(t, raw)
	var report LocalDeviceMissingReport
	require.NoError(t, json.Unmarshal([]byte(raw), &report))
	assert.Equal(t, "node-a", report.Node)
	assert.Equal(t, "vol-1", report.VolumeID)
	assert.Equal(t, localDeviceFailureClassMissingDevice, report.FailureClass)
	assert.Equal(t, "onecsi-439", report.DeviceSerial)
	assert.Equal(t, 2, report.Attempts)

	ns.clearLocalDeviceMissing(context.Background(), "vol-1")
	cm, err = driver.kubeRuntime.client.CoreV1().ConfigMaps(namespaceFromServiceAccount()).Get(context.Background(), localDeviceStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, cm.Data, key)
}

func TestNodeRecordsLocalDeviceMountFailureReport(t *testing.T) {
	driver := newLocalDeviceRecoveryTestDriver(t)
	driver.nodeID = "node-a"
	ns := &NodeServer{Driver: driver}
	publishContext := map[string]string{
		annotationBackend:                   "local",
		paramPVCNamespace:                   "default",
		paramPVCName:                        "pvc-vol-1",
		paramPVName:                         "pv-vol-1",
		publishContextDeviceSerial:          "onecsi-439",
		publishContextOpenNebulaImageID:     "439",
		publishContextHotplugTimeoutSeconds: "120",
	}

	ns.recordLocalDeviceMountFailure(
		context.Background(),
		"vol-1",
		"sdd",
		"/dev/sdd",
		"/stage/vol-1",
		"xfs",
		deviceResolutionResult{ResolvedBy: "cache", Latency: time.Millisecond},
		publishContext,
		errors.New("can't read superblock"),
	)

	cm, err := driver.kubeRuntime.client.CoreV1().ConfigMaps(namespaceFromServiceAccount()).Get(context.Background(), localDeviceStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	key := localDeviceReportKey("node-a", "vol-1")
	raw := cm.Data[key]
	require.NotEmpty(t, raw)
	var report LocalDeviceMissingReport
	require.NoError(t, json.Unmarshal([]byte(raw), &report))
	assert.Equal(t, "node-a", report.Node)
	assert.Equal(t, "vol-1", report.VolumeID)
	assert.Equal(t, localDeviceFailureClassMountFailed, report.FailureClass)
	assert.Equal(t, "/dev/sdd", report.DevicePath)
	assert.Equal(t, "onecsi-439", report.DeviceSerial)
	assert.Equal(t, "439", report.OpenNebulaImageID)
	assert.Equal(t, "xfs", report.FsType)
	assert.Equal(t, "cache", report.ResolvedBy)
	assert.Equal(t, "can't read superblock", report.LastError)
	assert.Equal(t, 1, report.Attempts)
}

func TestLocalDeviceRecoveryReattachesSameNodeAndClearsReport(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-device", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	pvc.Spec.VolumeName = pv.Name
	pvName := pv.Name
	report := LocalDeviceMissingReport{
		Node:              "node-a",
		VolumeID:          "vol-device",
		VolumeName:        "/dev/sdd",
		PVCNamespace:      "default",
		PVCName:           pvc.Name,
		PVName:            pv.Name,
		FirstObservedAt:   time.Now().Add(-2 * time.Minute),
		LastObservedAt:    time.Now().Add(-time.Minute),
		Attempts:          3,
		DeviceSerial:      "onecsi-394",
		OpenNebulaImageID: "394",
	}
	key := localDeviceReportKey(report.Node, report.VolumeID)
	payload, err := json.Marshal(report)
	require.NoError(t, err)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: localDeviceStateConfigMapName, Namespace: namespaceFromServiceAccount()},
		Data:       map[string]string{key: string(payload)},
	}
	va := &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "va-device"},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: DefaultDriverName,
			NodeName: "node-a",
			Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pvName},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: true},
	}
	driver := newLocalDeviceRecoveryTestDriver(t, pv, pvc, newReadyNode("node-a", true), va, cm)
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	mockProvider.On("NodeReady", mock.Anything, "node-a").Return(true, nil).Once()
	mockProvider.On("VolumeExists", mock.Anything, "vol-device").Return(394, 1, nil).Once()
	mockProvider.On("NodeExists", mock.Anything, "node-a").Return(208, nil).Once()
	mockProvider.On("GetVolumeInNode", mock.Anything, 394, 208).Return("sdd", nil).Once()
	mockProvider.On("DetachVolume", mock.Anything, "vol-device", "node-a").Return(nil).Once()
	mockProvider.On("AttachVolume", mock.Anything, "vol-device", "node-a", false, mock.Anything).Return(nil).Once()
	mockProvider.On("GetVolumeInNode", mock.Anything, 394, 208).Return("sdd", nil).Once()
	server := NewControllerServer(driver, mockProvider, nil)

	require.NoError(t, server.recoverLocalDeviceReport(context.Background(), key, report))
	updated, err := driver.kubeRuntime.client.CoreV1().ConfigMaps(namespaceFromServiceAccount()).Get(context.Background(), localDeviceStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updated.Data, key)
	mockProvider.AssertExpectations(t)
}

func TestLocalDeviceRecoveryClearsIneligibleReportWithoutOpenNebulaMutation(t *testing.T) {
	pv, pvc := newLocalPVAndPVC("vol-rwx", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, map[string]string{
		annotationBackend: "cephfs",
	})
	pvc.Spec.VolumeName = pv.Name
	report := LocalDeviceMissingReport{
		Node:            "node-a",
		VolumeID:        "vol-rwx",
		PVCNamespace:    "default",
		PVCName:         pvc.Name,
		PVName:          pv.Name,
		FirstObservedAt: time.Now().Add(-2 * time.Minute),
		LastObservedAt:  time.Now().Add(-time.Minute),
		Attempts:        3,
	}
	key := localDeviceReportKey(report.Node, report.VolumeID)
	payload, err := json.Marshal(report)
	require.NoError(t, err)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: localDeviceStateConfigMapName, Namespace: namespaceFromServiceAccount()},
		Data:       map[string]string{key: string(payload)},
	}
	driver := newLocalDeviceRecoveryTestDriver(t, pv, pvc, cm)
	mockProvider := &MockOpenNebulaVolumeProviderTestify{}
	server := NewControllerServer(driver, mockProvider, nil)

	require.NoError(t, server.recoverLocalDeviceReport(context.Background(), key, report))
	updated, err := driver.kubeRuntime.client.CoreV1().ConfigMaps(namespaceFromServiceAccount()).Get(context.Background(), localDeviceStateConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updated.Data, key)
	mockProvider.AssertNotCalled(t, "DetachVolume", mock.Anything, mock.Anything, mock.Anything)
	mockProvider.AssertNotCalled(t, "AttachVolume", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestLocalDeviceReportReadyStopsSameFailureAfterMaxAttempts(t *testing.T) {
	driver := newLocalDeviceRecoveryTestDriver(t)
	server := NewControllerServer(driver, &MockOpenNebulaVolumeProviderTestify{}, nil)
	now := time.Now().UTC()
	recoveredAt := now.Add(-10 * time.Minute)
	report := LocalDeviceMissingReport{
		Node:                  "node-a",
		VolumeID:              "vol-1",
		VolumeName:            "sdg",
		DeviceSerial:          "onecsi-439",
		FirstObservedAt:       now.Add(-30 * time.Minute),
		LastObservedAt:        now.Add(-time.Minute),
		Attempts:              3,
		RecoveryAttempts:      2,
		LastRecoveryAt:        &recoveredAt,
		LastRecoverySignature: localDeviceRecoverySignature(LocalDeviceMissingReport{VolumeName: "sdg", DeviceSerial: "onecsi-439"}),
	}

	ready, reason := server.localDeviceReportReady(report, now)
	assert.False(t, ready)
	assert.Equal(t, "max_recovery_attempts_same_failure", reason)

	report.LastObservedAt = recoveredAt.Add(-time.Minute)
	ready, reason = server.localDeviceReportReady(report, now)
	assert.False(t, ready)
	assert.Equal(t, "max_recovery_attempts", reason)
}

func TestLocalDeviceReportReadyRetriesAfterFailureSignatureChanges(t *testing.T) {
	driver := newLocalDeviceRecoveryTestDriver(t)
	server := NewControllerServer(driver, &MockOpenNebulaVolumeProviderTestify{}, nil)
	now := time.Now().UTC()
	recoveredAt := now.Add(-10 * time.Minute)
	report := LocalDeviceMissingReport{
		Node:                  "node-a",
		VolumeID:              "vol-1",
		VolumeName:            "sdg",
		DeviceSerial:          "onecsi-439",
		FirstObservedAt:       now.Add(-30 * time.Minute),
		LastObservedAt:        now.Add(-time.Minute),
		Attempts:              3,
		RecoveryAttempts:      2,
		LastRecoveryAt:        &recoveredAt,
		LastRecoverySignature: localDeviceRecoverySignature(LocalDeviceMissingReport{VolumeName: "sdf", DeviceSerial: "onecsi-439"}),
	}

	ready, reason := server.localDeviceReportReady(report, now)
	assert.True(t, ready)
	assert.Empty(t, reason)
}
