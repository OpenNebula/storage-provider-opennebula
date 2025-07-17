/*
Copyright 2025, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package driver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/config"
	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const (
	DefaultDriverName         = "csi.opennebula.io" //TODO: get from a repo metadata file or from a build flag
	DefaultGRPCServerEndpoint = "unix:///tmp/csi.sock"
	DefaultVolumeSizeBytes    = 1 * 1024 * 1024 * 1024
	GracefulShutdownTimeout   = 25 * time.Second
)

var (
	driverVersion = "v0.0.1" //TODO: get from a repo metadata file or from a build flag
)

// TODO: This should be a struct with a map of locks
// to avoid locking the entire driver
// and allow concurrent access to different volumes
type VolumeLocks sync.Mutex

type Driver struct {
	name               string
	grpcServerEndpoint string
	nodeID             string
	version            string

	PluginConfig config.CSIPluginConfig

	controllerServerCapabilities []*csi.ControllerServiceCapability

	volumeLocks *VolumeLocks

	maxVolumesPerNode int64

	mounter *mount.SafeFormatAndMount
}

type DriverOptions struct {
	NodeID             string
	DriverName         string
	MaxVolumesPerNode  int64
	GRPCServerEndpoint string
	PluginConfig       config.CSIPluginConfig
	Mounter            *mount.SafeFormatAndMount
}

func NewDriver(options *DriverOptions) *Driver {
	return &Driver{
		name:               options.DriverName,
		version:            driverVersion,
		nodeID:             options.NodeID,
		grpcServerEndpoint: options.GRPCServerEndpoint,
		PluginConfig:       options.PluginConfig,
		maxVolumesPerNode:  options.MaxVolumesPerNode,
		mounter:            options.Mounter,
	}

	//TODO: Initialize volumeLocks

}

func (d *Driver) Run(ctx context.Context) error {
	//TODO: Show driver metadata

	grpcServer := NewGRPCServer()

	endpoint, ok := d.PluginConfig.GetString(config.OpenNebulaRPCEndpointVar)
	if !ok {
		return fmt.Errorf("failed to get %s endpoint from config", config.OpenNebulaRPCEndpointVar)
	}

	credentials, ok := d.PluginConfig.GetString(config.OpenNebulaCredentialsVar)
	if !ok {
		return fmt.Errorf("failed to get %s credentials from config", config.OpenNebulaCredentialsVar)
	}

	volumeProvider, err := opennebula.NewPersistentDiskVolumeProvider(
		opennebula.NewClient(opennebula.OpenNebulaConfig{
			Endpoint:    endpoint,
			Credentials: credentials,
		}))
	if err != nil || volumeProvider == nil {
		return fmt.Errorf("failed to create PersistentDiskVolumeProvider: %v", err)
	}

	grpcServer.Start(
		d.grpcServerEndpoint,
		NewIdentityServer(d),
		NewNodeServer(d, d.mounter),
		NewControllerServer(d, volumeProvider),
	)

	go func() {
		<-ctx.Done()
		klog.Info("Received shutdown signal, stopping driver...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), GracefulShutdownTimeout)
		defer cancel()
		grpcServer.Stop(shutdownCtx)
	}()

	grpcServer.Wait()

	return nil
}
