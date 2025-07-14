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

	"github.com/OpenNebula/one/src/oca/go/src/goca"
)

type OpenNebulaConfig struct {
	Endpoint    string
	Credentials string
}

type OpenNebulaClient struct {
	*goca.Client
}

type OpenNebulaProvider struct {
	Client *OpenNebulaClient
}

type OpenNebulaVolumeProvider interface {
	CreateVolume(ctx context.Context, name string, size int64, owner string) error
	DeleteVolume(ctx context.Context, volume string) error
	AttachVolume(ctx context.Context, volume string, node string) error
	DetachVolume(ctx context.Context, volume string, node string) error
	ListVolumes(ctx context.Context, volume string) ([]string, error)
	GetCapacity(ctx context.Context) (int64, error)
	VolumeExists(ctx context.Context, volume string) (int, int, error)
	NodeExists(ctx context.Context, node string) (int, error)
	GetVolumeInNode(ctx context.Context, volumeID int, nodeID int) (string, error)
}

func NewClient(config OpenNebulaConfig) *OpenNebulaClient {
	return &OpenNebulaClient{
		Client: goca.NewDefaultClient(
			goca.OneConfig{
				Token:    config.Credentials,
				Endpoint: config.Endpoint,
			},
		),
	}
}

func (c *OpenNebulaClient) Probe(ctx context.Context) error {
	_, err := c.Client.CallContext(ctx, "one.system.version")
	if err != nil {
		return err
	}
	return nil
}
