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
	"strconv"
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
	volumeID, err := volumeProvider.CreateVolume(ctx, volumeTestName, volumeSize, testDriverName)
	if err != nil {
		t.Fatalf("failed to create volume: %v", err)
	}
	if volumeID == -1 {
		t.Fatal("created volume ID is invalid")
	}
	t.Logf("volume created successfully: %d", volumeID)

	var ready bool
	for attempt := 1; attempt <= maxRetries; attempt++ {
		ready, err = volumeProvider.VolumeReady(strconv.Itoa(volumeID))
		if err != nil {
			t.Fatalf("error checking if volume %d is ready: %v", volumeID, err)
		}
		if ready {
			break
		}
		time.Sleep(retryDelay)
	}

	if !ready {
		t.Fatalf("volume %d is not ready after %d attempts", volumeID, maxRetries)
	}

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

	// TODO: CREATE VM
	// volumeProvider.AttachVolume(ctx, strconv.Itoa(volumeID), strconv.Itoa(2986))

	// ready, err = waitNodeReady(t, volumeProvider, strconv.Itoa(2986))
	// if err != nil {
	// 	t.Fatalf("error checking if node is ready: %v", err)
	// }

	// if !ready {
	// 	t.Fatalf("node %d is not ready after %d attempts", volumeID, maxRetries)
	// }

	// t.Logf("volume %d attached successfully", volumeID)

	// err = volumeProvider.DetachVolume(ctx, strconv.Itoa(volumeID), strconv.Itoa(2986))
	// if err != nil {
	// 	t.Fatalf("failed to detach volume %d: %v", volumeID, err)
	// }

	// ready, err = waitNodeReady(t, volumeProvider, strconv.Itoa(2986))
	// if err != nil {
	// 	t.Fatalf("error checking if node is ready: %v", err)
	// }

	// if !ready {
	// 	t.Fatalf("node %d is not ready after %d attempts", volumeID, maxRetries)
	// }

	// t.Logf("volume %d detached successfully", volumeID)

	err = volumeProvider.DeleteVolume(ctx, strconv.Itoa(volumeID))
	if err != nil {
		t.Fatalf("failed to delete volume %d: %v", volumeID, err)
	}
	t.Logf("volume %d deleted successfully", volumeID)
}

func waitNodeReady(t *testing.T, volumeProvider *PersistentDiskVolumeProvider, nodeID string) (bool, error) {
	var ready bool
	var err error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		ready, err = volumeProvider.NodeReady(nodeID)
		if err != nil {
			t.Fatalf("error checking if node %s is ready: %v", nodeID, err)
		}
		if ready {
			break
		}
		time.Sleep(retryDelay)
	}

	return ready, nil
}
