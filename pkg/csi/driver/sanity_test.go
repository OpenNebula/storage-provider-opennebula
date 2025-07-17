package driver

import (
	"os"
	"path"
	"testing"

	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/config"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
)

func TestDriver(t *testing.T) {
	tmpDir := os.TempDir()
	defer os.Remove(tmpDir)

	socket := path.Join(tmpDir, "csi.sock")
	grpcEndpoint := "unix://" + socket

	csiPluginConfig := config.LoadConfiguration()
	csiPluginConfig.OverrideVal(config.OpenNebulaRPCEndpointVar, "http://localhost:2633/RPC2")
	csiPluginConfig.OverrideVal(config.OpenNebulaCredentialsVar, "oneadmin:opennebula")
	driverOptions := &DriverOptions{
		DriverName:         "csi.opennebula.io",
		NodeID:             "test-node",
		GRPCServerEndpoint: grpcEndpoint,
		PluginConfig:       csiPluginConfig,
	}

	driver := NewDriver(driverOptions)
	if driver == nil {
		t.Fatalf("Failed to create driver")
	}

	go driver.Run(t.Context())

	config := sanity.NewTestConfig()
	config.Address = grpcEndpoint
	config.TestVolumeSize = 10 * 1024 * 1024 // 10MB
	config.TestVolumeParameters = map[string]string{}
	//TODO: Add more parameters

	sanity.Test(t, config)
}
