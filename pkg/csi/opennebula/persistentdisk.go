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
	"strconv"
	"time"

	"github.com/OpenNebula/one/src/oca/go/src/goca"
	"github.com/OpenNebula/one/src/oca/go/src/goca/parameters"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image"
	img "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image"
	imk "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image/keys"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/shared"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
)

const (
	ownerTag       = "OWNER"
	sizeConversion = 1024 * 1024
	timeout        = 5 * time.Second
)

type PersistentDiskVolumeProvider struct {
	ctrl *goca.Controller
}

func NewPersistentDiskVolumeProvider(client *OpenNebulaClient) (*PersistentDiskVolumeProvider, error) {
	if client == nil {
		return nil, fmt.Errorf("client reference is nil")
	}
	return &PersistentDiskVolumeProvider{
		ctrl: goca.NewController(client.Client),
	}, nil
}

func (p *PersistentDiskVolumeProvider) CreateVolume(ctx context.Context, name string, size int64, owner string) error {
	if name == "" {
		return fmt.Errorf("volume name cannot be empty")
	}

	if size <= 0 {
		return fmt.Errorf("invalid volume size: must be greater than 0")
	}

	// size is in bytes and we need it in MB
	sizeMB := size / sizeConversion
	if sizeMB <= 0 {
		return fmt.Errorf("invalid volume size: must be greater than 0 MB")
	}

	tpl := img.NewTemplate()
	tpl.Add(imk.Name, name)
	tpl.Add(imk.Size, int(sizeMB))
	tpl.Add(imk.Persistent, "YES")
	tpl.AddPair(ownerTag, owner)
	tpl.Add(imk.Type, string(image.Datablock))

	imageID, err := p.ctrl.Images().Create(tpl.String(), 1)
	if err != nil {
		return fmt.Errorf("failed to create volume: %w", err)
	}

	err = p.waitForResourceReady(imageID, timeout, p.volumeReady)
	if err != nil {
		return fmt.Errorf("failed to wait for volume readiness: %w", err)
	}
	return nil
}

func (p *PersistentDiskVolumeProvider) DeleteVolume(ctx context.Context, volume string) error {
	volumeID, _, err := p.VolumeExists(ctx, volume)
	if err != nil || volumeID == -1 {
		return nil
	}

	image, err := p.ctrl.Image(volumeID).Info(true)
	if err == nil {
		state, err := image.State()
		if err == nil && state == img.Used {
			return fmt.Errorf("cannot delete volume %s, it is currently in use",
				volume)
		}
	}

	// Force delete
	p.ctrl.Client.CallContext(ctx, "one.image.delete", volumeID, true)
	err = p.waitForResourceReady(volumeID, timeout, p.volumeDeleted)
	if err != nil {
		return fmt.Errorf("failed to wait for volume deletion: %w", err)
	}
	return nil
}

func (p *PersistentDiskVolumeProvider) AttachVolume(ctx context.Context, volume, node string) error {
	nodeID, err := p.NodeExists(ctx, node)
	if err != nil || nodeID == -1 {
		return fmt.Errorf("failed to check if node exists: %w", err)
	}

	volumeID, _, err := p.VolumeExists(ctx, volume)
	if err != nil || volumeID == -1 {
		return fmt.Errorf("failed to check if volume exists: %w", err)
	}
	disk := shared.NewDisk()
	disk.Add(shared.ImageID, volumeID)

	err = p.ctrl.VM(nodeID).DiskAttach(disk.String())
	if err != nil {
		return fmt.Errorf("failed to attach volume %s to node %s: %w",
			volume, node, err)
	}
	err = p.waitForResourceReady(nodeID, timeout, p.nodeReady)
	if err != nil {
		return fmt.Errorf("failed to wait for node readiness: %w", err)
	}
	return nil
}

func (p *PersistentDiskVolumeProvider) DetachVolume(ctx context.Context, volume, node string) error {
	nodeID, err := p.NodeExists(ctx, node)
	if err != nil || nodeID == -1 {
		return fmt.Errorf("failed to check if node exists: %w", err)
	}

	volumeID, _, err := p.VolumeExists(ctx, volume)
	if err != nil || volumeID == -1 {
		return fmt.Errorf("failed to check if volume exists: %w", err)
	}

	vmController := p.ctrl.VM(nodeID)
	vmInfo, err := vmController.Info(true)
	if err != nil {
		return fmt.Errorf("failed to get VM info: %w", err)
	}

	for _, disk := range vmInfo.Template.GetDisks() {
		diskImageID, err := disk.GetI(shared.ImageID)
		if err == nil && diskImageID == volumeID {
			diskID, err := disk.Get(shared.DiskID)
			if err != nil {
				return fmt.Errorf(
					"failed to get disk ID from volume %s on node %s: %w",
					volume, node, err)
			}
			diskIDInt, err := strconv.Atoi(diskID)
			if err != nil {
				return fmt.Errorf("invalid disk ID format: %w", err)
			}
			err = vmController.Disk(diskIDInt).Detach()
			if err != nil {
				return fmt.Errorf("failed to detach volume %s from node %s: %w",
					diskID, node, err)
			}
			err = p.waitForResourceReady(nodeID, timeout, p.nodeReady)
			if err != nil {
				return fmt.Errorf("failed to wait for node readiness: %w", err)
			}
			return nil
		}
	}
	return fmt.Errorf("volume: %s not found on node %s", volume, node)
}

func (p *PersistentDiskVolumeProvider) ListVolumes(ctx context.Context, owner string, maxEntries int32, startingToken string) ([]string, error) {

	listingParams := []int{parameters.PoolWhoAll, -1, -1}

	if startingToken != "" {
		startIndex, err := strconv.Atoi(startingToken)
		if err != nil || startIndex < 0 {
			return nil, fmt.Errorf("invalid starting token: %w", err)
		}
		listingParams[1] = -int(startIndex) //pagination offset
	}

	if maxEntries < 0 {
		return nil, fmt.Errorf("maxEntries must be non-negative")
	}

	if maxEntries > 0 {
		listingParams[2] = int(maxEntries) // page size
	}

	images, err := p.ctrl.Images().Info(listingParams...)
	if err != nil {
		return nil, fmt.Errorf("failed to list volumes: %w", err)
	}

	//Filter images by the owner tag
	var volumeIDs []string
	for _, img := range images.Images {
		imageOwner, err := img.Template.Get(ownerTag)
		if err == nil && imageOwner == owner {
			volumeIDs = append(volumeIDs, img.Name)
		}
	}
	return volumeIDs, nil
}

func (p *PersistentDiskVolumeProvider) GetCapacity(ctx context.Context) (int64, error) {
	datastores, err := p.ctrl.Datastores().Info()
	if err != nil {
		return 0, fmt.Errorf("failed to get datastores info: %w", err)
	}
	for _, ds := range datastores.Datastores {
		if ds.Name == "default" {
			return int64(ds.FreeMB) * 1024 * 1024, nil
		}
	}
	return 0, fmt.Errorf("default datastore not found")
}

func (p *PersistentDiskVolumeProvider) VolumeExists(ctx context.Context, volume string) (int, int, error) {
	imgID, err := p.ctrl.Images().ByName(volume)
	if err != nil {
		return -1, -1, fmt.Errorf("failed to get volume by name: %w", err)
	}
	img, err := p.ctrl.Image(imgID).Info(true)
	if err != nil {
		return -1, -1, fmt.Errorf("failed to get volume info: %w", err)
	}
	return imgID, (img.Size * sizeConversion), nil
}

func (p *PersistentDiskVolumeProvider) NodeExists(ctx context.Context, node string) (int, error) {
	vmID, err := p.ctrl.VMs().ByName(node)
	if err != nil {
		return -1, fmt.Errorf("Failed to fetch VM: %w", err)
	}

	return vmID, nil
}

func (p *PersistentDiskVolumeProvider) volumeReady(volumeID int) (bool, error) {
	image, err := p.ctrl.Image(volumeID).Info(true)
	if err != nil {
		return false, fmt.Errorf("failed to get Disk info: %w", err)
	}

	state, err := image.State()
	if err != nil {
		return false, fmt.Errorf("failed to get Disk state: %w", err)
	}

	return state == img.Ready || state == img.Used, nil
}

func (p *PersistentDiskVolumeProvider) volumeDeleted(volumeID int) (bool, error) {
	_, err := p.ctrl.Image(volumeID).Info(true)
	if err != nil {
		return true, nil
	}
	return false, fmt.Errorf("volume %d still exists: %w", volumeID, err)
}

func (p *PersistentDiskVolumeProvider) nodeReady(nodeID int) (bool, error) {
	vmInfo, err := p.ctrl.VM(nodeID).Info(true)
	if err != nil {
		return false, fmt.Errorf("failed to get VM info: %w", err)
	}
	_, vmLCMState, err := vmInfo.State()
	if err != nil {
		return false, fmt.Errorf("failed to get VM state: %w", err)
	}

	return vmLCMState == vm.Running, nil
}

func (p *PersistentDiskVolumeProvider) waitForResourceReady(volumeID int, timeout time.Duration, checkFunc func(int) (bool, error)) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for volume %d to be ready", volumeID)
		case <-ticker.C:
			ready, err := checkFunc(volumeID)
			if err != nil {
				return fmt.Errorf("error checking volume readiness: %w", err)
			}
			if ready {
				return nil
			}
		}
	}
}

func (p *PersistentDiskVolumeProvider) GetVolumeInNode(ctx context.Context, volumeID int, nodeID int) (string, error) {
	vmInfo, err := p.ctrl.VM(nodeID).Info(true)
	if err != nil {
		return "", fmt.Errorf("failed to get VM info: %w", err)
	}

	for _, disk := range vmInfo.Template.GetDisks() {
		diskImageID, err := disk.GetI(shared.ImageID)
		if err != nil {
			continue
		}
		if diskImageID == volumeID {
			target, err := disk.Get("TARGET")
			if err != nil {
				return "", fmt.Errorf(
					"failed to get target for volume %d on node %d: %w",
					volumeID, nodeID, err)
			}
			return target, nil
		}
	}
	return "", fmt.Errorf("volume %d not found on node %d", volumeID, nodeID)
}
