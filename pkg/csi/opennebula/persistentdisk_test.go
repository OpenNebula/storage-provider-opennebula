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

	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/shared"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	maxRetries     = 5
	retryDelay     = 2 * time.Second
	volumeName     = "volume-test"
	volumeSize     = 10 * 1024 * 1024
	testDriverName = "csi-test.opennebula.io"
)

func TestPersistentDiskLifecycle(t *testing.T) {
	if os.Getenv("RUN_OPENNEBULA_INTEGRATION_TESTS") != "1" {
		t.Skip("set RUN_OPENNEBULA_INTEGRATION_TESTS=1 to run OpenNebula integration tests")
	}

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

	volumeProvider, err := NewPersistentDiskVolumeProvider(client, HotplugTimeoutPolicy{
		BaseTimeout:  60 * time.Second,
		Per100GiB:    60 * time.Second,
		MaxTimeout:   5 * time.Minute,
		PollInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("failed to create PersistentDiskVolumeProvider: %v", err)
	}
	if volumeProvider == nil {
		t.Fatal("PersistentDiskVolumeProvider is nil")
	}

	ctx := context.Background()
	selection := DatastoreSelectionConfig{
		Identifiers:  getIntegrationTestDatastores(),
		Policy:       DatastoreSelectionPolicyLeastUsed,
		AllowedTypes: []string{"local"},
	}

	params := map[string]string{
		"devPrefix": "vd",
	}

	volumeTestName := fmt.Sprintf("%s-%s", volumeName, uuid.New().String())
	_, err = volumeProvider.CreateVolume(ctx, volumeTestName, volumeSize, testDriverName, false, "ext4", params, selection)
	if err != nil {
		t.Fatalf("failed to create volume: %v", err)
	}
	t.Logf("volume created successfully: %s", volumeTestName)

	// Sleep to allow the volume to be ready
	time.Sleep(5 * time.Second)

	volumes, err := volumeProvider.ListVolumes(ctx, testDriverName, 10, "")
	if err != nil {
		t.Fatalf("failed to list volumes: %v", err)
	}
	if len(volumes) == 0 {
		t.Fatal("no volumes found after creation")
	}
	t.Logf("found %d volumes after creation", len(volumes))
	t.Logf("volumes: %v", volumes)

	dataStoreSize, err := volumeProvider.GetCapacity(ctx, selection)
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

func TestPersistentDiskDetachedExpansion(t *testing.T) {
	if os.Getenv("RUN_OPENNEBULA_INTEGRATION_TESTS") != "1" {
		t.Skip("set RUN_OPENNEBULA_INTEGRATION_TESTS=1 to run OpenNebula integration tests")
	}

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
	volumeProvider, err := NewPersistentDiskVolumeProvider(client, HotplugTimeoutPolicy{
		BaseTimeout:  60 * time.Second,
		Per100GiB:    60 * time.Second,
		MaxTimeout:   5 * time.Minute,
		PollInterval: time.Second,
	})
	require.NoError(t, err)

	ctx := context.Background()
	selection := DatastoreSelectionConfig{
		Identifiers:  getIntegrationTestDatastores(),
		Policy:       DatastoreSelectionPolicyLeastUsed,
		AllowedTypes: []string{"local"},
	}

	params := map[string]string{
		"devPrefix": "vd",
	}

	volumeTestName := fmt.Sprintf("%s-expand-%s", volumeName, uuid.New().String())
	initialSize := int64(128 * 1024 * 1024)
	expandedSize := int64(256 * 1024 * 1024)

	_, err = volumeProvider.CreateVolume(ctx, volumeTestName, initialSize, testDriverName, false, "ext4", params, selection)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = volumeProvider.DeleteVolume(ctx, volumeTestName)
	})

	newSize, err := volumeProvider.ExpandVolume(ctx, volumeTestName, expandedSize, true)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, newSize, expandedSize)

	_, resolvedSize, err := volumeProvider.VolumeExists(ctx, volumeTestName)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, int64(resolvedSize), expandedSize)
}

func getIntegrationTestDatastores() []string {
	values := config.LoadConfiguration()
	if datastores, ok := values.GetStringSlice(config.DefaultDatastoresVar); ok && len(datastores) > 0 {
		return datastores
	}

	return []string{"default"}
}

func TestLatestHistoryDatastoreID(t *testing.T) {
	vmInfo := &vm.VM{
		ID: 42,
		HistoryRecords: []vm.HistoryRecord{
			{SEQ: 0, DSID: 100},
			{SEQ: 1, DSID: 200},
			{SEQ: 2, DSID: 150},
		},
	}

	dsID, err := latestHistoryDatastoreID(vmInfo)
	require.NoError(t, err)
	assert.Equal(t, 150, dsID)
}

func TestLatestHistoryDatastoreIDAcceptsZeroSystemDatastore(t *testing.T) {
	vmInfo := &vm.VM{
		ID: 42,
		HistoryRecords: []vm.HistoryRecord{
			{SEQ: 0, DSID: -1},
			{SEQ: 1, DSID: 0},
		},
	}

	dsID, err := latestHistoryDatastoreID(vmInfo)
	require.NoError(t, err)
	assert.Equal(t, 0, dsID)
}

func TestLatestHistoryDatastoreIDRequiresSystemDatastoreHistory(t *testing.T) {
	_, err := latestHistoryDatastoreID(&vm.VM{ID: 42})
	require.Error(t, err)
	assert.True(t, IsDatastoreConfigError(err))
	assert.Contains(t, err.Error(), "system datastore history")
}

func TestIsHotplugStateError(t *testing.T) {
	require.True(t, isHotplugStateError(fmt.Errorf("OpenNebula error: wrong state HOTPLUG")))
	require.True(t, isHotplugStateError(fmt.Errorf("wrong state hotplug")))
	require.False(t, isHotplugStateError(fmt.Errorf("wrong state running")))
	require.False(t, isHotplugStateError(nil))
}

func TestHostArtifactConflictFromMessageParsesLocalLVName(t *testing.T) {
	conflict, ok := HostArtifactConflictFromMessage(
		`ATTACHDISK: Logical volume "lv-one-161-2" already exists`,
		HostArtifactAttachmentTarget{NodeName: "hplbravow02", ImageID: 233},
		fmt.Errorf("attach failed"),
	)

	require.True(t, ok)
	require.NotNil(t, conflict)
	assert.Equal(t, HostArtifactConflictClassification, conflict.Classification)
	assert.Equal(t, 161, conflict.Target.VMID)
	assert.Equal(t, 2, conflict.Target.DiskID)
	assert.Equal(t, "lv-one-161-2", conflict.Target.LVName)
	assert.Contains(t, conflict.Error(), "lv=lv-one-161-2")
}

func TestNextAvailableDiskIDIncludesContextDisk(t *testing.T) {
	tpl := vm.NewTemplate()
	root := shared.NewDisk()
	root.Add(shared.DiskID, 0)
	root.Add(shared.TargetDisk, "sda")
	rootVector := root.Vector
	tpl.Elements = append(tpl.Elements, &rootVector)
	require.NoError(t, tpl.AddPairToVec("CONTEXT", "DISK_ID", 1))

	vmInfo := &vm.VM{ID: 161, Template: *tpl}

	assert.Equal(t, 2, nextAvailableDiskID(vmInfo))
	assert.Equal(t, "sdb", nextAvailableDiskTarget(vmInfo, map[string]string{"devPrefix": "sd"}))
}

func TestComputeHotplugTimeoutScalesWithVolumeSize(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{
		hotplugPolicy: HotplugTimeoutPolicy{
			BaseTimeout: 120 * time.Second,
			Per100GiB:   60 * time.Second,
			MaxTimeout:  15 * time.Minute,
		},
	}

	assert.Equal(t, 3*time.Minute, provider.ComputeHotplugTimeout(1*1024*1024*1024))
	assert.Equal(t, 5*time.Minute, provider.ComputeHotplugTimeout(300*1024*1024*1024))
	assert.Equal(t, 6*time.Minute, provider.ComputeHotplugTimeout(301*1024*1024*1024))
}

func TestComputeHotplugTimeoutClampsToMax(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{
		hotplugPolicy: HotplugTimeoutPolicy{
			BaseTimeout: 120 * time.Second,
			Per100GiB:   60 * time.Second,
			MaxTimeout:  5 * time.Minute,
		},
	}

	assert.Equal(t, 5*time.Minute, provider.ComputeHotplugTimeout(900*1024*1024*1024))
}

func TestWaitForAttachStateReturnsHotplugTimeoutError(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{}

	err := provider.waitForAttachState(context.Background(), 5*time.Millisecond, time.Millisecond, func() (bool, bool, error) {
		return false, false, nil
	}, nil, "vol-a", "node-a")

	var timeoutErr *HotplugTimeoutError
	require.ErrorAs(t, err, &timeoutErr)
	assert.Equal(t, "attach", timeoutErr.Operation)
	assert.False(t, timeoutErr.LastObservedAttached)
	assert.False(t, timeoutErr.LastObservedReady)
	assert.Equal(t, 5*time.Millisecond, timeoutErr.Timeout)
}

func TestWaitForDetachStateReturnsHotplugTimeoutError(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{}

	err := provider.waitForDetachState(context.Background(), 5*time.Millisecond, time.Millisecond, func() (bool, bool, error) {
		return true, false, nil
	}, nil, "vol-a", "node-a")

	var timeoutErr *HotplugTimeoutError
	require.ErrorAs(t, err, &timeoutErr)
	assert.Equal(t, "detach", timeoutErr.Operation)
	assert.True(t, timeoutErr.LastObservedAttached)
	assert.False(t, timeoutErr.LastObservedReady)
	assert.Equal(t, 5*time.Millisecond, timeoutErr.Timeout)
}

func TestWaitForDetachStateSucceedsWhenVolumeAbsentAndNodeNotReady(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{}

	err := provider.waitForDetachState(context.Background(), 50*time.Millisecond, time.Millisecond, func() (bool, bool, error) {
		return false, false, nil
	}, nil, "vol-a", "node-a")

	require.NoError(t, err)
}

func TestWaitForAttachStateRetriesAfterNodeBecomesReady(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{}
	retryCount := 0
	attached := false

	err := provider.waitForAttachState(context.Background(), 50*time.Millisecond, time.Millisecond, func() (bool, bool, error) {
		if retryCount == 0 {
			return false, true, nil
		}
		if attached {
			return true, true, nil
		}
		return false, false, nil
	}, func() error {
		retryCount++
		attached = true
		return nil
	}, "vol-a", "node-a")

	require.NoError(t, err)
	assert.Equal(t, 1, retryCount)
}

func TestWaitForAttachStateRetriesHotplugUntilSuccess(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{}
	retryCount := 0

	err := provider.waitForAttachState(context.Background(), 50*time.Millisecond, time.Millisecond, func() (bool, bool, error) {
		if retryCount >= 2 {
			return true, true, nil
		}
		return false, true, nil
	}, func() error {
		retryCount++
		if retryCount == 1 {
			return fmt.Errorf("wrong state HOTPLUG")
		}
		return nil
	}, "vol-a", "node-a")

	require.NoError(t, err)
	assert.Equal(t, 2, retryCount)
}

func TestWaitForAttachStateFailsOnNonHotplugRetryError(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{}

	err := provider.waitForAttachState(context.Background(), 50*time.Millisecond, time.Millisecond, func() (bool, bool, error) {
		return false, true, nil
	}, func() error {
		return fmt.Errorf("permission denied")
	}, "vol-a", "node-a")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to retry attach")
}

func TestWaitForDetachStateRetriesAfterNodeBecomesReady(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{}
	retryCount := 0
	attached := true

	err := provider.waitForDetachState(context.Background(), 50*time.Millisecond, time.Millisecond, func() (bool, bool, error) {
		if retryCount == 0 {
			return true, true, nil
		}
		if !attached {
			return false, true, nil
		}
		return true, false, nil
	}, func() error {
		retryCount++
		attached = false
		return nil
	}, "vol-a", "node-a")

	require.NoError(t, err)
	assert.Equal(t, 1, retryCount)
}

func TestWaitForDetachStateRetriesHotplugUntilSuccess(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{}
	retryCount := 0

	err := provider.waitForDetachState(context.Background(), 50*time.Millisecond, time.Millisecond, func() (bool, bool, error) {
		if retryCount >= 2 {
			return false, true, nil
		}
		return true, true, nil
	}, func() error {
		retryCount++
		if retryCount == 1 {
			return fmt.Errorf("wrong state HOTPLUG")
		}
		return nil
	}, "vol-a", "node-a")

	require.NoError(t, err)
	assert.Equal(t, 2, retryCount)
}

func TestWaitForDetachStateFailsOnNonHotplugRetryError(t *testing.T) {
	provider := &PersistentDiskVolumeProvider{}

	err := provider.waitForDetachState(context.Background(), 50*time.Millisecond, time.Millisecond, func() (bool, bool, error) {
		return true, true, nil
	}, func() error {
		return fmt.Errorf("permission denied")
	}, "vol-a", "node-a")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to retry detach")
}
