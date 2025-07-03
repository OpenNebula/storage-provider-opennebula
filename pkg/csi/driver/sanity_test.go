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
	csiPluginConfig.OverrideVal("ONE_XMLRPC", "http://localhost:2633/RPC2")
	csiPluginConfig.OverrideVal("ONE_AUTH", "oneadmin:opennebula")

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
	driver.Run()

	config := sanity.NewTestConfig()
	config.Address = grpcEndpoint
	config.TestVolumeSize = 10 * 1024 * 1024 // 10MB
	config.TestVolumeParameters = map[string]string{}
	//TODO: Add more parameters

	sanity.Test(t, config)
}
