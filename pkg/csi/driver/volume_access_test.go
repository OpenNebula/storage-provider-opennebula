package driver

import (
	"testing"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestResolveVolumeAccessModelDoesNotTreatGenericCSISecretsAsCephFS(t *testing.T) {
	model, err := resolveVolumeAccessModel(&csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}, []string{"local", "ceph", "cephfs"}, map[string]string{
		"csi.storage.k8s.io/provisioner-secret-name":      "generic-provisioner",
		"csi.storage.k8s.io/provisioner-secret-namespace": "kube-system",
	}, []opennebula.Datastore{{
		ID:      111,
		Name:    "local-a",
		Backend: "local",
		Type:    "local",
	}})
	if err != nil {
		t.Fatalf("expected disk routing to succeed, got %v", err)
	}
	if model != opennebula.VolumeAccessModelDisk {
		t.Fatalf("expected disk access model, got %q", model)
	}
}

func TestResolveVolumeAccessModelUsesSharedFilesystemForCephFSRWO(t *testing.T) {
	model, err := resolveVolumeAccessModel(&csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}, []string{"local", "ceph", "cephfs"}, nil, []opennebula.Datastore{{
		ID:      300,
		Name:    "cephfs-a",
		Backend: "cephfs",
		Type:    "cephfs",
	}})
	if err != nil {
		t.Fatalf("expected cephfs RWO routing to succeed, got %v", err)
	}
	if model != opennebula.VolumeAccessModelSharedFS {
		t.Fatalf("expected shared filesystem access model, got %q", model)
	}
}

func TestResolveVolumeAccessModelRejectsMixedCephFSAndDiskCandidates(t *testing.T) {
	_, err := resolveVolumeAccessModel(&csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}, []string{"local", "ceph", "cephfs"}, nil, []opennebula.Datastore{
		{ID: 300, Name: "cephfs-a", Backend: "cephfs", Type: "cephfs"},
		{ID: 111, Name: "local-a", Backend: "local", Type: "local"},
	})
	if err == nil {
		t.Fatal("expected mixed cephfs and disk datastores to fail")
	}
}
