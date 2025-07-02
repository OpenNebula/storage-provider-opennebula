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

	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/config"
	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

type ControllerServer struct {
	driver *Driver
	csi.UnimplementedControllerServer
}

func NewControllerServer(d *Driver) *ControllerServer {
	return &ControllerServer{
		driver: d,
	}
}

// TODO: Implement methods specified in https://github.com/container-storage-interface/spec/blob/98819c45a37a67e0cd466bd02b813faf91af4e45/spec.md#controller-service-rpc
func (s *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(4).Infof("ControllerGetCapabilities called with request: %v", protosanitizer.StripSecrets(req))

	// Check if the driver is initialized
	if s.driver == nil {
		return nil, status.Error(codes.Unavailable, "driver not initialized")
	}

	// Define the capabilities of the controller server
	capabilities := []*csi.ControllerServiceCapability{
		{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
				},
			},
		},
	}

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: capabilities,
	}, nil
}

func (s *ControllerServer) testConnectivity() {
	endpoint, _ := s.driver.PluginConfig.GetString(config.OpenNebulaRPCEndpointVar)
	credentials, ok := s.driver.PluginConfig.GetString(config.OpenNebulaCredentialsVar)
	if !ok {
		klog.Fatalf("Missing OpenNebula credentials")
	}
	oneConfig := opennebula.OpenNebulaConfig{
		Endpoint:    endpoint,
		Credentials: credentials,
	}
	client := opennebula.NewClient(oneConfig)
	if err := client.Probe(context.TODO()); err != nil {
		klog.Fatalf("Failed to connect to OpenNebula: %v", err)
	}
	klog.Infof("Connected to OpenNebula at %s", endpoint)
}
