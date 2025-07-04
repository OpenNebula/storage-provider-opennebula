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

package main

import (
	"flag"
	"os"

	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/config"
	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/driver"
	"k8s.io/klog/v2"
)

var (
	driverName     = flag.String("drivername", driver.DefaultDriverName, "CSI driver name")
	pluginEndpoint = flag.String("endpoint", driver.DefaultGRPCServerEndpoint, "CSI plugin endpoint")
	nodeID         = flag.String("nodeid", "", "Node ID")
)

func main() {
	klog.InitFlags(nil)
	_ = flag.Set("logtostderr", "true")
	flag.Parse()

	if *nodeID == "" {
		klog.Warning("nodeid is empty")
	}

	config := config.LoadConfiguration()

	handle(config)
	os.Exit(0)
}

func handle(cfg config.CSIPluginConfig) {
	driverOptions := &driver.DriverOptions{
		NodeID:             *nodeID,
		DriverName:         *driverName,
		GRPCServerEndpoint: *pluginEndpoint,
		PluginConfig:       cfg,
	}
	driver := driver.NewDriver(driverOptions)
	driver.Run()
}
