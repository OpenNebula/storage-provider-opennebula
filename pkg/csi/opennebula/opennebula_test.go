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

package opennebula

import (
	"context"
	"os"
	"testing"

	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/config"
)

func TestOpenNebulaClientProbe(t *testing.T) {
	cfg := OpenNebulaConfig{
		Endpoint:    os.Getenv(config.OpenNebulaRPCEndpointVar),
		Credentials: os.Getenv(config.OpenNebulaCredentialsVar),
	}

	if cfg.Endpoint == "" || cfg.Credentials == "" {
		t.Skipf("%s or %s not set, skipping integration test",
			config.OpenNebulaRPCEndpointVar,
			config.OpenNebulaCredentialsVar)
	}

	client := NewClient(cfg)
	if client == nil {
		t.Fatal("failed to create OpenNebula client")
	}

	ctx := context.Background()

	if err := client.Probe(ctx); err != nil {
		t.Fatalf("Probe failed: %v", err)
	}
}
