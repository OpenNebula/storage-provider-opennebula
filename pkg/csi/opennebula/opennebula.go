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
	"sort"
	"time"

	"github.com/OpenNebula/one/src/oca/go/src/goca"
)

type HotplugTimeoutPolicy struct {
	BaseTimeout  time.Duration
	Per100GiB    time.Duration
	MaxTimeout   time.Duration
	PollInterval time.Duration
}

type HotplugObservationKey struct {
	Operation  string `json:"operation"`
	Backend    string `json:"backend"`
	SizeBucket string `json:"sizeBucket"`
}

type HotplugTimeoutRecommendation struct {
	Key            HotplugObservationKey `json:"key"`
	SampleCount    int                   `json:"sampleCount"`
	Recommended    time.Duration         `json:"recommended"`
	StaticFloor    time.Duration         `json:"staticFloor"`
	AdaptiveMax    time.Duration         `json:"adaptiveMax"`
	LastObservedAt time.Time             `json:"lastObservedAt"`
}

type ObservedAttachment struct {
	VolumeHandle string `json:"volumeHandle"`
	ImageID      int    `json:"imageID"`
	NodeName     string `json:"nodeName"`
	NodeID       int    `json:"nodeID"`
	DiskID       int    `json:"diskID"`
	Backend      string `json:"backend"`
}

type VolumeDiskRecord struct {
	NodeName string `json:"nodeName,omitempty"`
	NodeID   int    `json:"nodeID,omitempty"`
	DiskID   int    `json:"diskID,omitempty"`
	Target   string `json:"target,omitempty"`
	Serial   string `json:"serial,omitempty"`
}

type VolumeAttachmentMetadata struct {
	VolumeHandle            string             `json:"volumeHandle,omitempty"`
	ImageID                 int                `json:"imageID,omitempty"`
	ImageName               string             `json:"imageName,omitempty"`
	ImageState              string             `json:"imageState,omitempty"`
	ImageStateRaw           int                `json:"imageStateRaw,omitempty"`
	ImageRunningVMs         int                `json:"imageRunningVMs,omitempty"`
	ImageVMIDs              []int              `json:"imageVMIDs,omitempty"`
	RequestedNode           string             `json:"requestedNode,omitempty"`
	RequestedNodeID         int                `json:"requestedNodeID,omitempty"`
	AttachedToRequestedNode bool               `json:"attachedToRequestedNode,omitempty"`
	DiskRecords             []VolumeDiskRecord `json:"diskRecords,omitempty"`
}

func (m VolumeAttachmentMetadata) ConflictingOwnerVMIDs() []int {
	seen := map[int]struct{}{}
	for _, vmID := range m.ImageVMIDs {
		if vmID <= 0 || (m.RequestedNodeID > 0 && vmID == m.RequestedNodeID) {
			continue
		}
		seen[vmID] = struct{}{}
	}
	for _, record := range m.DiskRecords {
		if record.NodeID <= 0 || (m.RequestedNodeID > 0 && record.NodeID == m.RequestedNodeID) {
			continue
		}
		seen[record.NodeID] = struct{}{}
	}
	ids := make([]int, 0, len(seen))
	for vmID := range seen {
		ids = append(ids, vmID)
	}
	sort.Ints(ids)
	return ids
}

type VolumeAttachmentInspector interface {
	InspectVolumeAttachment(ctx context.Context, volume string, node string) (*VolumeAttachmentMetadata, error)
}

type HotplugCooldownState struct {
	Node                 string
	Operation            string
	Volume               string
	ExpiresAt            time.Time
	Timeout              time.Duration
	LastObservedReady    bool
	LastObservedAttached bool
	FailureCount         int
	Reason               string
	PauseUntilReady      bool
	KubernetesReady      bool
	OpenNebulaReady      bool
	Unschedulable        bool
	FirstFailureAt       time.Time
	LastFailureAt        time.Time
}

type HotplugTimeoutError struct {
	Operation            string
	Volume               string
	Node                 string
	Timeout              time.Duration
	LastObservedAttached bool
	LastObservedReady    bool
	Cause                error
}

func (e *HotplugTimeoutError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "hotplug timeout"
}

func (e *HotplugTimeoutError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

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
	ResolveProvisioningDatastores(ctx context.Context, selection DatastoreSelectionConfig) ([]Datastore, error)
	CreateVolume(ctx context.Context, name string, size int64, owner string, immutable bool, fsType string, params map[string]string, selection DatastoreSelectionConfig) (*VolumeCreateResult, error)
	CloneVolume(ctx context.Context, name string, sourceVolume string, selection DatastoreSelectionConfig) (*VolumeCreateResult, error)
	DeleteVolume(ctx context.Context, volume string) error
	ExpandVolume(ctx context.Context, volume string, size int64, allowDetached bool) (int64, error)
	AttachVolume(ctx context.Context, volume string, node string, immutable bool, params map[string]string) error
	DetachVolume(ctx context.Context, volume string, node string) error
	ResolveVolumeSizeBytes(ctx context.Context, volume string) (int64, error)
	ComputeHotplugTimeout(sizeBytes int64) time.Duration
	HotplugPolicy() HotplugTimeoutPolicy
	NodeReady(ctx context.Context, node string) (bool, error)
	ListVolumes(ctx context.Context, owner string, maxEntries int32, startingToken string) ([]string, error)
	GetCapacity(ctx context.Context, selection DatastoreSelectionConfig) (int64, error)
	VolumeExists(ctx context.Context, volume string) (int, int, error)
	CreateSnapshot(ctx context.Context, sourceVolume string, snapshotName string) (*VolumeSnapshot, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) error
	ListSnapshots(ctx context.Context, snapshotID string, sourceVolumeID string, maxEntries int32, startingToken string) ([]VolumeSnapshot, string, error)
	NodeExists(ctx context.Context, node string) (int, error)
	GetVolumeInNode(ctx context.Context, volumeID int, nodeID int) (string, error)
	VolumeReadyWithTimeout(volumeID int) (bool, error)
	ListCurrentAttachments(ctx context.Context) ([]ObservedAttachment, error)
}

type SharedFilesystemProvider interface {
	CreateSharedVolume(ctx context.Context, req SharedVolumeRequest) (*SharedVolumeCreateResult, error)
	CloneSharedVolume(ctx context.Context, req SharedVolumeCloneRequest) (*SharedVolumeCreateResult, error)
	DeleteSharedVolume(ctx context.Context, volumeID string, secrets map[string]string) error
	ExpandSharedVolume(ctx context.Context, volumeID string, sizeBytes int64, secrets map[string]string) (int64, error)
	CreateSharedSnapshot(ctx context.Context, sourceVolumeID string, snapshotName string, secrets map[string]string) (*VolumeSnapshot, error)
	DeleteSharedSnapshot(ctx context.Context, snapshotID string, secrets map[string]string) error
	ListSharedSnapshots(ctx context.Context, snapshotID string, sourceVolumeID string, maxEntries int32, startingToken string, secrets map[string]string) ([]VolumeSnapshot, string, error)
	PublishSharedVolume(ctx context.Context, volumeID string, readonly bool) (map[string]string, error)
	ValidateSharedVolume(ctx context.Context, volumeID string) (*SharedVolumeMetadata, error)
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
