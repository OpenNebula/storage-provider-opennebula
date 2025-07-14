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
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/OpenNebula/cloud-provider-opennebula/pkg/csi/config"
	"github.com/google/uuid"
)

const (
	maxRetries     = 5
	retryDelay     = 2 * time.Second
	volumeName     = "volume-test"
	volumeSize     = 10 * 1024 * 1024
	testDriverName = "csi-test.opennebula.io"
)

func TestPersistentDiskLifecycle(t *testing.T) {
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

	volumeProvider, err := NewPersistentDiskVolumeProvider(client)
	if err != nil {
		t.Fatalf("failed to create PersistentDiskVolumeProvider: %v", err)
	}
	if volumeProvider == nil {
		t.Fatal("PersistentDiskVolumeProvider is nil")
	}

	ctx := context.Background()

	volumeTestName := fmt.Sprintf("%s-%s", volumeName, uuid.New().String())
	err = volumeProvider.CreateVolume(ctx, volumeTestName, volumeSize, testDriverName)
	if err != nil {
		t.Fatalf("failed to create volume: %v", err)
	}
	t.Logf("volume created successfully: %s", volumeTestName)

	// Sleep to allow the volume to be ready
	time.Sleep(5 * time.Second)

	volumes, err := volumeProvider.ListVolumes(ctx, testDriverName)
	if err != nil {
		t.Fatalf("failed to list volumes: %v", err)
	}
	if len(volumes) == 0 {
		t.Fatal("no volumes found after creation")
	}
	t.Logf("found %d volumes after creation", len(volumes))
	t.Logf("volumes: %v", volumes)

	dataStoreSize, err := volumeProvider.GetCapacity(ctx)
	if err != nil {
		t.Fatalf("failed to list volumes: %v", err)
	}
	t.Logf("datastore size: %d", dataStoreSize)

	err = volumeProvider.DeleteVolume(ctx, volumeTestName)
	if err != nil {
		t.Fatalf("failed to delete volume %s: %v", volumeTestName, err)
	}
	t.Logf("volume %s deleted successfully", volumeTestName)
}
