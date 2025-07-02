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
	"sync"

	"github.com/OpenNebula/csi-driver-opennebula/pkg/config"
	"github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	DefaultDriverName = "csi.opennebula.io" //TODO: get from a repo metadata file or from a build flag
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

	PluginConfig     config.CSIPluginConfig
	identityServer   *IdentityServer
	nodeServer       *NodeServer
	controllerServer *ControllerServer

	nodeServerCapabilities       []*csi.NodeServiceCapability
	controllerServerCapabilities []*csi.ControllerServiceCapability

	volumeLocks *VolumeLocks
}

type DriverOptions struct {
	NodeID             string
	DriverName         string
	GRPCServerEndpoint string
	PluginConfig       config.CSIPluginConfig
}

func NewDriver(options *DriverOptions) *Driver {
	return &Driver{
		name:               options.DriverName,
		version:            driverVersion,
		nodeID:             options.NodeID,
		grpcServerEndpoint: options.GRPCServerEndpoint,
	}

	//TODO: Initialize volumeLocks

}

func (d *Driver) Run() {
	//TODO: Show driver metadata

	d.identityServer = NewIdentityServer(d)
	d.nodeServer = NewNodeServer(d)
	d.controllerServer = NewControllerServer(d)

	grpcServer := NewGRPCServer()
	grpcServer.Start(
		d.grpcServerEndpoint,
		d.identityServer,
		d.nodeServer,
		d.controllerServer,
	)
	grpcServer.Wait()
}
