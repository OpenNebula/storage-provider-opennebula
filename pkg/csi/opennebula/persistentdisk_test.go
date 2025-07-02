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
	"time"
)

const (
	maxRetries = 5
	retryDelay = 2 * time.Second
)

func TestPersistentDiskLifecycle(t *testing.T) {
	cfg := OpenNebulaConfig{
		Endpoint:    os.Getenv("ONE_XMLRPC"),
		Credentials: os.Getenv("ONE_AUTH"),
	}

	if cfg.Endpoint == "" || cfg.Credentials == "" {
		t.Skip("ONE_XMLRPC or ONE_AUTH not set, skipping integration test")
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

	volumeID, err := volumeProvider.CreateVolume(ctx, "integration-disk", 10) // 10 MB
	if err != nil {
		t.Fatalf("failed to create volume: %v", err)
	}
	if volumeID == -1 {
		t.Fatal("created volume ID is invalid")
	}
	t.Logf("volume created successfully: %d", volumeID)

	var ready bool
	for attempt := 1; attempt <= maxRetries; attempt++ {
		ready, err = volumeProvider.DiskReady(volumeID)
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

	err = volumeProvider.DeleteVolume(ctx, volumeID)
	if err != nil {
		t.Fatalf("failed to delete volume %d: %v", volumeID, err)
	}
	t.Logf("volume %d deleted successfully", volumeID)
}
