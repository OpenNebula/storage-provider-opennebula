package driver

import (
	"context"
	"testing"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	inventoryv1alpha1 "github.com/SparkAIUR/storage-provider-opennebula/pkg/inventory/apis/storageprovider/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestValidationSummaryForCommandPrefersDisplaySummary(t *testing.T) {
	status := inventoryv1alpha1.OpenNebulaDatastoreStatus{
		LastValidationSummary: "R:12k W:8k p99:4ms",
		Validation: inventoryv1alpha1.OpenNebulaDatastoreValidationStatus{
			Phase:   inventoryv1alpha1.ValidationPhaseSucceeded,
			Message: "validation completed",
		},
	}
	if got := validationSummaryForCommand(status); got != "R:12k W:8k p99:4ms" {
		t.Fatalf("unexpected validation summary: %q", got)
	}
}

func TestBenchmarkSummaryForCommandPrefersDisplaySummary(t *testing.T) {
	status := inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunStatus{
		Phase:   inventoryv1alpha1.ValidationPhaseSucceeded,
		Summary: "R:12k W:8k p99:4ms",
		Message: "benchmark completed",
	}
	if got := benchmarkSummaryForCommand(status); got != "R:12k W:8k p99:4ms" {
		t.Fatalf("unexpected benchmark summary: %q", got)
	}
}

func TestFlattenStorageClassDetails(t *testing.T) {
	datastores := []inventoryv1alpha1.OpenNebulaDatastore{
		{
			Status: inventoryv1alpha1.OpenNebulaDatastoreStatus{
				StorageClassDetails: []inventoryv1alpha1.StorageClassDetail{{
					Name:              "opennebula-local",
					VolumeBindingMode: string(storagev1.VolumeBindingImmediate),
					Warnings:          []string{"local datastore uses Immediate binding"},
				}},
			},
		},
	}
	classes := []storagev1.StorageClass{{ObjectMeta: metav1.ObjectMeta{Name: "opennebula-local"}}}
	flattened := flattenStorageClassDetails(datastores, classes)
	if len(flattened) != 1 {
		t.Fatalf("expected 1 flattened storage class detail, got %d", len(flattened))
	}
	if len(flattened[0].Warnings) != 1 {
		t.Fatalf("expected warnings to be preserved, got %#v", flattened[0].Warnings)
	}
}

func TestSupportBundleConfigIncludesInventorySettings(t *testing.T) {
	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.InventoryControllerEnabledVar, true)
	cfg.OverrideVal(config.InventoryDatastoreAuthorityModeVar, "strict")
	cfg.OverrideVal(config.PreflightLocalImmediateBindingPolicyVar, "warn")
	cfg.OverrideVal(config.LocalDeviceRecoveryEnabledVar, true)
	cfg.OverrideVal(config.LocalDeviceRecoveryMinAttemptsVar, 3)
	cfg.OverrideVal(config.HostArtifactQuarantineEnabledVar, true)
	cfg.OverrideVal(config.HostArtifactQuarantineFailureThresholdVar, 1)

	bundleCfg := supportBundleConfig(cfg)
	if enabled, ok := bundleCfg["inventoryControllerEnabled"].(bool); !ok || !enabled {
		t.Fatalf("expected inventory controller enabled in support bundle config, got %#v", bundleCfg["inventoryControllerEnabled"])
	}
	if bundleCfg["inventoryAuthorityMode"] != "strict" {
		t.Fatalf("unexpected authority mode: %#v", bundleCfg["inventoryAuthorityMode"])
	}
	if enabled, ok := bundleCfg["localDeviceRecoveryEnabled"].(bool); !ok || !enabled {
		t.Fatalf("expected local device recovery enabled in support bundle config, got %#v", bundleCfg["localDeviceRecoveryEnabled"])
	}
	if bundleCfg["localDeviceRecoveryMinAttempts"] != 3 {
		t.Fatalf("unexpected local device recovery min attempts: %#v", bundleCfg["localDeviceRecoveryMinAttempts"])
	}
	if enabled, ok := bundleCfg["hostArtifactQuarantineEnabled"].(bool); !ok || !enabled {
		t.Fatalf("expected host artifact quarantine enabled in support bundle config, got %#v", bundleCfg["hostArtifactQuarantineEnabled"])
	}
	if bundleCfg["hostArtifactQuarantineFailureThreshold"] != 1 {
		t.Fatalf("unexpected host artifact quarantine threshold: %#v", bundleCfg["hostArtifactQuarantineFailureThreshold"])
	}
}

func TestCollectVolumeHealthReportsFlagsLegacyLastAttachedNodeAnnotation(t *testing.T) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pv-legacy",
			Annotations: map[string]string{
				annotationLegacyLastAttachedNode: "node-old",
				annotationBackend:                "local",
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			ClaimRef: &corev1.ObjectReference{
				Namespace: "default",
				Name:      "pvc-legacy",
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       DefaultDriverName,
					VolumeHandle: "vol-legacy",
				},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pvc-legacy",
			Annotations: map[string]string{
				annotationLegacyLastAttachedNode: "node-old",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name},
	}
	kubeClient := fake.NewSimpleClientset(pv, pvc)
	cfg := config.LoadConfiguration()
	cfg.OverrideVal(config.OpenNebulaCredentialsVar, "")

	reports, err := collectVolumeHealthReports(context.Background(), kubeClient, cfg, VolumeHealthOptions{VolumeID: "vol-legacy"})
	if err != nil {
		t.Fatalf("collectVolumeHealthReports returned error: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(reports))
	}
	if len(reports[0].AnnotationAudit) != 2 {
		t.Fatalf("expected PV and PVC legacy annotation audit findings, got %#v", reports[0].AnnotationAudit)
	}
	for _, finding := range reports[0].AnnotationAudit {
		if finding.CanonicalKey != annotationLastAttachedNode {
			t.Fatalf("expected canonical annotation key %q, got %#v", annotationLastAttachedNode, finding)
		}
	}
}
