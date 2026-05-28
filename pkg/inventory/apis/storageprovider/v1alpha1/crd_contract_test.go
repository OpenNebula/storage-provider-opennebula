package v1alpha1

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDatastoreCRDIncludesDisplayColumnsAndTypedStatus(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve current file path")
	}
	crdPath := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../../../../../helm/opennebula-csi/crds/opennebuladatastores.storageprovider.opennebula.sparkaiur.io.yaml"))
	payload, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("failed reading datastore CRD: %v", err)
	}
	text := string(payload)

	requiredSnippets := []string{
		"- name: Status",
		"- name: ID",
		"- name: Name",
		"- name: Capacity",
		"- name: Type",
		"- name: Backend",
		"- name: SCs",
		"- name: Metrics",
		"capacityDisplay:",
		"storageClassesDisplay:",
		"metricsDisplay:",
		"health:",
		"maintenanceMode:",
		"storageClassDetails:",
		"validationLastOutcome:",
	}
	for _, snippet := range requiredSnippets {
		if !strings.Contains(text, snippet) {
			t.Fatalf("expected datastore CRD to contain %q", snippet)
		}
	}
	if strings.Contains(text, "- name: Enabled") || strings.Contains(text, "jsonPath: .status.capacity.freeBytes") {
		t.Fatal("legacy datastore printer columns are still present")
	}
}

func TestBenchmarkRunCRDExists(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve current file path")
	}
	crdPath := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../../../../../helm/opennebula-csi/crds/opennebuladatastorebenchmarkruns.storageprovider.opennebula.sparkaiur.io.yaml"))
	payload, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("failed reading benchmark run CRD: %v", err)
	}
	text := string(payload)
	requiredSnippets := []string{
		"kind: OpenNebulaDatastoreBenchmarkRun",
		"plural: opennebuladatastorebenchmarkruns",
		"- name: Datastore",
		"- name: Phase",
		"datastoreID:",
		"accessModes:",
		"fioArgs:",
		"activeDeadlineSeconds:",
		"summary:",
	}
	for _, snippet := range requiredSnippets {
		if !strings.Contains(text, snippet) {
			t.Fatalf("expected benchmark CRD to contain %q", snippet)
		}
	}
}

func TestNodeCRDIncludesHotplugDiagnosisStatus(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve current file path")
	}
	crdPath := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../../../../../helm/opennebula-csi/crds/opennebulaNodes.storageprovider.opennebula.sparkaiur.io.yaml"))
	payload, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("failed reading node CRD: %v", err)
	}
	text := string(payload)
	requiredSnippets := []string{
		"- name: Hotplug",
		"jsonPath: .status.hotplug.diagnosis.classification",
		"diagnosis:",
		"classification:",
		"volumeHandle:",
		"stuckAfterSeconds:",
		"recommendedAction:",
	}
	for _, snippet := range requiredSnippets {
		if !strings.Contains(text, snippet) {
			t.Fatalf("expected node CRD to contain %q", snippet)
		}
	}
}
