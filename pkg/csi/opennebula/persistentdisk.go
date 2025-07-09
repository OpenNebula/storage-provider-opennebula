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

	"github.com/OpenNebula/one/src/oca/go/src/goca"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image"
	img "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image"
	imk "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image/keys"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/shared"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
)

const (
	owner_tag       = "OWNER"
	size_conversion = 1024 * 1024
)

type PersistentDiskVolumeProvider struct {
	ctrl *goca.Controller
}

func NewPersistentDiskVolumeProvider(client *OpenNebulaClient) (*PersistentDiskVolumeProvider, error) {
	if client == nil {
		return nil, fmt.Errorf("client reference is nil")
	}
	return &PersistentDiskVolumeProvider{ctrl: goca.NewController(client.Client)}, nil
}

func (p *PersistentDiskVolumeProvider) CreateVolume(ctx context.Context, name string, size int64, owner string) (int, error) {
	if name == "" {
		return -1, fmt.Errorf("volume name cannot be empty")
	}

	if size <= 0 {
		return -1, fmt.Errorf("invalid volume size: must be greater than 0")
	}

	// size is in bytes and we need it in MB
	sizeMB := size / size_conversion
	if sizeMB <= 0 {
		return -1, fmt.Errorf("invalid volume size: must be greater than 0 MB")
	}

	tpl := img.NewTemplate()
	tpl.Add(imk.Name, name)
	tpl.Add(imk.Size, int(sizeMB))
	tpl.Add(imk.Persistent, "YES")
	tpl.AddPair(owner_tag, owner)
	tpl.Add(imk.Type, string(image.Datablock))

	volumeID, err := p.ctrl.Images().Create(tpl.String(), 1)
	if err != nil {
		return -1, fmt.Errorf("failed to create volume: %w", err)
	}
	return volumeID, nil
}

func (p *PersistentDiskVolumeProvider) DeleteVolume(ctx context.Context, volume string) error {
	id, err := strconv.Atoi(volume)
	if err != nil || !p.VolumeExists(ctx, id) {
		return nil
	}

	image, err := p.ctrl.Image(id).Info(true)
	if err == nil {
		state, err := image.State()
		if err == nil && state == img.Used {
			return fmt.Errorf("cannot delete volume %s, it is currently in use", volume)
		}
	}

	// Force delete
	p.ctrl.Client.CallContext(ctx, "one.image.delete", id, true)
	return nil
}

func (p *PersistentDiskVolumeProvider) AttachVolume(ctx context.Context, volume string, node string) error {
	if volume == "" {
		return fmt.Errorf("invalid volume format")
	}

	if node == "" {
		return fmt.Errorf("invalid node format")
	}

	volumeID, err := strconv.Atoi(volume)
	if err != nil {
		return fmt.Errorf("invalid volume ID format: %w", err)
	}

	nodeID, err := strconv.Atoi(node)
	if err != nil {
		return fmt.Errorf("invalid node ID format: %w", err)
	}

	disk := shared.NewDisk()
	disk.Add(shared.ImageID, volumeID)

	err = p.ctrl.VM(nodeID).DiskAttach(disk.String())
	if err != nil {
		return fmt.Errorf("failed to attach volume %s to node %s: %w", volume, node, err)
	}
	return nil
}

func (p *PersistentDiskVolumeProvider) DetachVolume(ctx context.Context, volume string, node string) error {
	if volume == "" {
		return fmt.Errorf("invalid volume format")
	}

	if node == "" {
		return fmt.Errorf("invalid node format")
	}

	nodeID, err := strconv.Atoi(node)
	if err != nil {
		return fmt.Errorf("invalid node ID format: %w", err)
	}

	vmController := p.ctrl.VM(nodeID)
	vmInfo, err := vmController.Info(true)
	if err != nil {
		return fmt.Errorf("failed to get VM info: %w", err)
	}

	for _, disk := range vmInfo.Template.GetDisks() {
		diskImageID, err := disk.Get(shared.ImageID)
		if err == nil && diskImageID == volume {
			diskID, err := disk.Get(shared.DiskID)
			if err != nil {
				return fmt.Errorf("failed to get disk ID from volume %s on node %s: %w", volume, node, err)
			}
			diskIDInt, err := strconv.Atoi(diskID)
			if err != nil {
				return fmt.Errorf("invalid disk ID format: %w", err)
			}
			err = vmController.Disk(diskIDInt).Detach()
			if err != nil {
				return fmt.Errorf("failed to detach volume %s from node %s: %w", diskID, node, err)
			}
			return nil
		}
	}
	return fmt.Errorf("volume: %s not found on node %s", volume, node)
}

func (p *PersistentDiskVolumeProvider) ListVolumes(ctx context.Context, owner string) ([]string, error) {
	images, err := p.ctrl.Images().Info()
	if err != nil {
		return nil, fmt.Errorf("failed to list volumes: %w", err)
	}
	var volumeIDs []string
	for _, img := range images.Images {
		imageOwner, err := img.Template.Get(owner_tag)
		if err == nil && imageOwner == owner {
			volumeIDs = append(volumeIDs, strconv.Itoa(img.ID))
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

func (p *PersistentDiskVolumeProvider) VolumeReady(volume string) (bool, error) {
	if volume == "" {
		return false, fmt.Errorf("invalid volume format")
	}
	id, err := strconv.Atoi(volume)
	if err != nil {
		return false, fmt.Errorf("invalid volume ID format: %w", err)
	}
	image, err := p.ctrl.Image(id).Info(true)
	if err != nil {
		return false, fmt.Errorf("failed to get Disk info: %w", err)
	}

	state, err := image.State()
	if err != nil {
		return false, fmt.Errorf("failed to get Disk state: %w", err)
	}

	return state == img.Ready || state == img.Used, nil
}

func (p *PersistentDiskVolumeProvider) NodeReady(node string) (bool, error) {
	if node == "" {
		return false, fmt.Errorf("invalid node format")
	}
	id, err := strconv.Atoi(node)
	if err != nil {
		return false, fmt.Errorf("invalid node ID format: %w", err)
	}

	vmInfo, err := p.ctrl.VM(id).Info(true)
	if err != nil {
		return false, fmt.Errorf("failed to get VM info: %w", err)
	}
	_, vmLCMState, err := vmInfo.State()
	if err != nil {
		return false, fmt.Errorf("failed to get VM state: %w", err)
	}

	return vmLCMState == vm.Running, nil
}

func (p *PersistentDiskVolumeProvider) DuplicatedVolume(ctx context.Context, volume string) (int, int, error) {
	images, err := p.ctrl.Images().Info()
	if err != nil {
		return -1, -1, fmt.Errorf("failed to list volumes: %w", err)
	}
	for _, img := range images.Images {
		if img.Name == volume {
			sizeBytes := img.Size * size_conversion
			return img.ID, sizeBytes, nil
		}
	}
	return -1, -1, nil
}

func (p *PersistentDiskVolumeProvider) VolumeExists(ctx context.Context, volume int) bool {
	_, err := p.ctrl.Image(volume).Info(true)
	return err == nil
}

func (p *PersistentDiskVolumeProvider) NodeExists(ctx context.Context, node string) bool {
	id, err := strconv.Atoi(node)
	if err != nil {
		return false
	}
	_, err = p.ctrl.VM(id).Info(true)
	return err == nil
}
