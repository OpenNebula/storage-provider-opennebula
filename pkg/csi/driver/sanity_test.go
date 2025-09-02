package driver

import (
	"context"
	"os"
	"path"
	"testing"

	"github.com/OpenNebula/storage-provider-opennebula/pkg/csi/config"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"
	"k8s.io/utils/exec/testing"
)

func getDriver() *Driver {

	commandScriptArray := []testingexec.FakeCommandAction{}
	//TODO: Simulate real commands
	for i := 0; i < 100; i++ {
		commandScriptArray = append(commandScriptArray, func(cmd string, args ...string) exec.Cmd {
			return &testingexec.FakeCmd{
				Argv:           append([]string{cmd}, args...),
				Stdout:         nil,
				Stderr:         nil,
				DisableScripts: true, // Disable script checking for simplicity
			}
		})
	}

	mounter := mount.NewSafeFormatAndMount(
		mount.NewFakeMounter([]mount.MountPoint{}), // using fake mounter implementation
		&testingexec.FakeExec{
			CommandScript: commandScriptArray,
		}, // using fake exec implementation
	)

	csiPluginConfig := config.LoadConfiguration()
	csiPluginConfig.OverrideVal(config.OpenNebulaRPCEndpointVar, "http://localhost:2633/RPC2")
	csiPluginConfig.OverrideVal(config.OpenNebulaCredentialsVar, "oneadmin:opennebula")
	driverOpts := &DriverOptions{
		NodeID:             "test-node-id",
		DriverName:         DefaultDriverName,
		MaxVolumesPerNode:  30,
		GRPCServerEndpoint: DefaultGRPCServerEndpoint,
		PluginConfig:       csiPluginConfig,
		Mounter:            mounter,
	}
	return NewDriver(driverOpts)
}

func TestDriver(t *testing.T) {
	tmpDir := os.TempDir()
	defer os.Remove(tmpDir)

	socket := path.Join(tmpDir, "csi.sock")
	grpcEndpoint := "unix://" + socket

	mounter := mount.NewSafeFormatAndMount(
		mount.NewFakeMounter(nil), // using fake mounter implementation
		&testingexec.FakeExec{},
	)

	csiPluginConfig := config.LoadConfiguration()
	csiPluginConfig.OverrideVal(config.OpenNebulaRPCEndpointVar, "http://localhost:2633/RPC2")
	csiPluginConfig.OverrideVal(config.OpenNebulaCredentialsVar, "oneadmin:opennebula")
	driverOptions := &DriverOptions{
		DriverName:         "csi.opennebula.io",
		NodeID:             "test-node",
		GRPCServerEndpoint: grpcEndpoint,
		PluginConfig:       csiPluginConfig,
		Mounter:            mounter,
	}

	driver := NewDriver(driverOptions)
	if driver == nil {
		t.Fatalf("Failed to create driver")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go driver.Run(ctx)

	config := sanity.NewTestConfig()
	config.Address = grpcEndpoint
	config.TestVolumeSize = 10 * 1024 * 1024 // 10MB
	config.TestVolumeParameters = map[string]string{}
	//TODO: Add more parameters

	sanity.Test(t, config)
}
