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

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type IdentityServer struct {
	Driver *Driver
	csi.UnimplementedIdentityServer
}

func NewIdentityServer(d *Driver) *IdentityServer {
	return &IdentityServer{
		Driver: d,
	}
}

//Following functons are RPC implementations defined in:
// - https://github.com/container-storage-interface/spec/blob/98819c45a37a67e0cd466bd02b813faf91af4e45/spec.md#rpc-interface
// - https://github.com/container-storage-interface/spec/blob/98819c45a37a67e0cd466bd02b813faf91af4e45/spec.md#identity-service-rpc

// https://github.com/container-storage-interface/spec/blob/98819c45a37a67e0cd466bd02b813faf91af4e45/spec.md#getplugininfo
func (is *IdentityServer) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	if is.Driver == nil {
		return nil, status.Error(codes.Unavailable, "driver not initialized")
	}

	if is.Driver.name == "" {
		return nil, status.Error(codes.Unavailable, "driver name not set")
	}

	if is.Driver.version == "" {
		return nil, status.Error(codes.Unavailable, "driver version not set")
	}

	return &csi.GetPluginInfoResponse{
		Name:          is.Driver.name,
		VendorVersion: is.Driver.version,
	}, nil
}

// https://github.com/container-storage-interface/spec/blob/98819c45a37a67e0cd466bd02b813faf91af4e45/spec.md#getplugincapabilities
func (is *IdentityServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	if is.Driver == nil {
		return nil, status.Error(codes.Unavailable, "driver not initialized")
	}

	if is.Driver.name == "" {
		return nil, status.Error(codes.Unavailable, "driver name not set")
	}

	if is.Driver.version == "" {
		return nil, status.Error(codes.Unavailable, "driver version not set")
	}

	//TODO: Add plugin capabilities: https://kubernetes-csi.github.io/docs/developing.html#capabilities
	// VOLUME_ACCESSIBILITY_CONSTRAINTS
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
		},
	}, nil
}

// https://github.com/container-storage-interface/spec/blob/98819c45a37a67e0cd466bd02b813faf91af4e45/spec.md#probe
func (is *IdentityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: true}}, nil
}
