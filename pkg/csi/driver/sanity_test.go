package driver

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"google.golang.org/grpc"
)

func TestCSISanity(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "csi-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	endpoint := filepath.Join(tmpDir, "csi.sock")

	driver := &Driver{
		name:    "test.csi.driver",
		version: "1.0.0",
	}

	server := grpc.NewServer()

	identityServer := NewIdentityServer(driver)
	csi.RegisterIdentityServer(server, identityServer)

	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}

	go func() {
		if err := server.Serve(listener); err != nil {
			t.Logf("Server error: %v", err)
		}
	}()
	defer server.Stop()

	config := sanity.NewTestConfig()
	config.Address = endpoint

	config.TestVolumeSize = 10 * 1024 * 1024 // 10MB
	config.TestVolumeParameters = map[string]string{}
	config.IdempotentCount = 1
	config.TestNodeVolumeAttachLimit = false

	sanity.Test(t, config)
}

func TestCSIIdentityOnly(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "csi-identity-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	endpoint := filepath.Join(tmpDir, "csi-identity.sock")

	driver := &Driver{
		name:    "identity.test.csi.driver",
		version: "1.0.0",
	}

	server := grpc.NewServer()
	identityServer := NewIdentityServer(driver)
	csi.RegisterIdentityServer(server, identityServer)

	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}

	go func() {
		server.Serve(listener)
	}()
	defer server.Stop()

	config := sanity.NewTestConfig()
	config.Address = endpoint
	config.TestVolumeSize = 1024
	config.IdempotentCount = 1
	config.TestNodeVolumeAttachLimit = false

	sanity.Test(t, config)
}

func TestCSIWithCustomConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping CSI sanity tests in short mode")
	}

	tmpDir, err := os.MkdirTemp("", "csi-custom-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	endpoint := filepath.Join(tmpDir, "csi-custom.sock")

	driver := &Driver{
		name:    "custom.test.csi.driver",
		version: "0.1.0-test",
	}

	server := grpc.NewServer()
	identityServer := NewIdentityServer(driver)
	csi.RegisterIdentityServer(server, identityServer)

	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}

	go func() {
		server.Serve(listener)
	}()
	defer server.Stop()

	config := sanity.NewTestConfig()
	config.Address = endpoint
	config.TestVolumeSize = 1024 * 1024 // 1MB
	config.IdempotentCount = 3

	config.TestVolumeParameters = map[string]string{
		"type": "test",
		"zone": "test-zone",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx

	sanity.Test(t, config)
}

func waitForServer(address string) error {
	conn, err := net.Dial("unix", address)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}
