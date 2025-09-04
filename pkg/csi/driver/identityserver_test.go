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
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type MockDriver struct {
	name    string
	version string
}

func NewMockDriver(name, version string) *MockDriver {
	return &MockDriver{
		name:    name,
		version: version,
	}
}

func TestNewIdentityServer(t *testing.T) {
	driver := &Driver{
		name:    "test-driver",
		version: "1.0.0",
	}

	is := NewIdentityServer(driver)

	if is == nil {
		t.Fatal("Expected non-nil IdentityServer")
	}

	if is.Driver != driver {
		t.Error("Expected Driver to be set correctly")
	}
}

func TestGetPluginInfo_Success(t *testing.T) {
	driver := &Driver{
		name:    "test-csi-driver",
		version: "v1.2.3",
	}

	is := NewIdentityServer(driver)
	ctx := context.Background()
	req := &csi.GetPluginInfoRequest{}

	resp, err := is.GetPluginInfo(ctx, req)

	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if resp.Name != driver.name {
		t.Errorf("Expected name %s, got %s", driver.name, resp.Name)
	}

	if resp.VendorVersion != driver.version {
		t.Errorf("Expected version %s, got %s", driver.version, resp.VendorVersion)
	}
}

func TestGetPluginInfo_DriverNotInitialized(t *testing.T) {
	is := &IdentityServer{
		Driver: nil,
	}

	ctx := context.Background()
	req := &csi.GetPluginInfoRequest{}

	resp, err := is.GetPluginInfo(ctx, req)

	if resp != nil {
		t.Error("Expected nil response")
	}

	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatal("Expected gRPC status error")
	}

	if st.Code() != codes.Unavailable {
		t.Errorf("Expected code %v, got %v", codes.Unavailable, st.Code())
	}

	expectedMsg := "driver not initialized"
	if st.Message() != expectedMsg {
		t.Errorf("Expected message %s, got %s", expectedMsg, st.Message())
	}
}

func TestGetPluginInfo_EmptyDriverName(t *testing.T) {
	driver := &Driver{
		name:    "",
		version: "1.0.0",
	}

	is := NewIdentityServer(driver)
	ctx := context.Background()
	req := &csi.GetPluginInfoRequest{}

	resp, err := is.GetPluginInfo(ctx, req)

	if resp != nil {
		t.Error("Expected nil response")
	}

	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatal("Expected gRPC status error")
	}

	if st.Code() != codes.Unavailable {
		t.Errorf("Expected code %v, got %v", codes.Unavailable, st.Code())
	}

	expectedMsg := "driver name not set"
	if st.Message() != expectedMsg {
		t.Errorf("Expected message %s, got %s", expectedMsg, st.Message())
	}
}

func TestGetPluginInfo_EmptyDriverVersion(t *testing.T) {
	driver := &Driver{
		name:    "test-driver",
		version: "",
	}

	is := NewIdentityServer(driver)
	ctx := context.Background()
	req := &csi.GetPluginInfoRequest{}

	resp, err := is.GetPluginInfo(ctx, req)

	if resp != nil {
		t.Error("Expected nil response")
	}

	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatal("Expected gRPC status error")
	}

	if st.Code() != codes.Unavailable {
		t.Errorf("Expected code %v, got %v", codes.Unavailable, st.Code())
	}

	expectedMsg := "driver version not set"
	if st.Message() != expectedMsg {
		t.Errorf("Expected message %s, got %s", expectedMsg, st.Message())
	}
}

func TestGetPluginCapabilities(t *testing.T) {
	driver := &Driver{
		name:    "test-driver",
		version: "1.0.0",
	}

	is := NewIdentityServer(driver)
	ctx := context.Background()
	req := &csi.GetPluginCapabilitiesRequest{}

	resp, err := is.GetPluginCapabilities(ctx, req)

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	expectedTypes := map[csi.PluginCapability_Service_Type]bool{
		csi.PluginCapability_Service_CONTROLLER_SERVICE: false,
	}

	for _, cap := range resp.Capabilities {
		service := cap.GetService()
		if service == nil {
			t.Errorf("expected service capability, got nil")
			continue
		}
		if _, ok := expectedTypes[service.Type]; !ok {
			t.Errorf("unexpected capability type: %v", service.Type)
		} else {
			expectedTypes[service.Type] = true
		}
	}

	for capType, found := range expectedTypes {
		if !found {
			t.Errorf("missing expected capability: %v", capType)
		}
	}
}
func TestProbe(t *testing.T) {
	driver := &Driver{
		name:    "test-driver",
		version: "1.0.0",
	}

	is := NewIdentityServer(driver)
	ctx := context.Background()
	req := &csi.ProbeRequest{}

	resp, err := is.Probe(ctx, req)

	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if resp == nil {
		t.Fatal("Expected non-nil response")
	}

	if resp.Ready == nil {
		t.Fatal("Expected Ready field to be set")
	}

	if !resp.Ready.Value {
		t.Error("Expected Ready.Value to be true")
	}
}

func TestProbe_WithNilContext(t *testing.T) {
	driver := &Driver{
		name:    "test-driver",
		version: "1.0.0",
	}

	is := NewIdentityServer(driver)
	req := &csi.ProbeRequest{}

	resp, err := is.Probe(nil, req)

	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if resp == nil {
		t.Fatal("Expected non-nil response")
	}

	if resp.Ready == nil {
		t.Fatal("Expected Ready field to be set")
	}

	if !resp.Ready.Value {
		t.Error("Expected Ready.Value to be true")
	}
}
