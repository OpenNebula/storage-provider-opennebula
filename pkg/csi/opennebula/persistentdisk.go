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

	"github.com/OpenNebula/one/src/oca/go/src/goca"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image"
	img "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image"
	imk "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image/keys"
	"github.com/google/uuid"
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

func (p *PersistentDiskVolumeProvider) CreateVolume(ctx context.Context, name string, size int) (int, error) {
	if name == "" {
		return -1, fmt.Errorf("volume name cannot be empty")
	}

	if size <= 0 {
		return -1, fmt.Errorf("invalid volume size: must be greater than 0")
	}

	volumeName := name + "-" + uuid.New().String()
	tpl := img.NewTemplate()
	tpl.Add(imk.Name, volumeName)
	tpl.Add(imk.Size, size)
	tpl.Add(imk.Persistent, "YES")
	tpl.Add(imk.Type, string(image.Datablock))

	volumeID, err := p.ctrl.Images().Create(tpl.String(), 1)
	if err != nil {
		return -1, fmt.Errorf("failed to create volume: %w", err)
	}
	return volumeID, nil
}

func (p *PersistentDiskVolumeProvider) DeleteVolume(ctx context.Context, id int) error {
	if id <= 0 {
		return fmt.Errorf("invalid volume ID: must be greater than 0")
	}

	if err := p.ctrl.Image(id).Delete(); err != nil {
		return fmt.Errorf("failed to delete existing volume: %w", err)
	}
	return nil
}

func (p *PersistentDiskVolumeProvider) DiskReady(id int) (bool, error) {
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
