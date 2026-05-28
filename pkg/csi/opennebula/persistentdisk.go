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
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/OpenNebula/one/src/oca/go/src/goca"
	"github.com/OpenNebula/one/src/oca/go/src/goca/parameters"
	datastoreSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/datastore"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image"
	img "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image"
	imk "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/image/keys"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/shared"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
	"k8s.io/klog/v2"
)

const (
	ownerTag            = "OWNER"
	sizeConversion      = 1024 * 1024
	fsTypeTag           = "FS"
	volumeUsedTimeout   = 30 * time.Second
	hotplugPollInterval = time.Second
)

type PersistentDiskVolumeProvider struct {
	ctrl            *goca.Controller
	hotplugPolicy   HotplugTimeoutPolicy
	hotplugPollWait time.Duration
}

func NewPersistentDiskVolumeProvider(client *OpenNebulaClient, policy HotplugTimeoutPolicy) (*PersistentDiskVolumeProvider, error) {
	if client == nil {
		return nil, fmt.Errorf("client reference is nil")
	}
	if policy.BaseTimeout <= 0 {
		policy.BaseTimeout = 60 * time.Second
	}
	if policy.MaxTimeout <= 0 || policy.MaxTimeout < policy.BaseTimeout {
		policy.MaxTimeout = policy.BaseTimeout
	}
	if policy.PollInterval <= 0 {
		policy.PollInterval = hotplugPollInterval
	}
	return &PersistentDiskVolumeProvider{
		ctrl:            goca.NewController(client.Client),
		hotplugPolicy:   policy,
		hotplugPollWait: policy.PollInterval,
	}, nil
}

func (p *PersistentDiskVolumeProvider) CreateVolume(ctx context.Context, name string, size int64, owner string, immutable bool, fsType string, params map[string]string, selection DatastoreSelectionConfig) (*VolumeCreateResult, error) {
	if name == "" {
		return nil, fmt.Errorf("volume name cannot be empty")
	}

	if size <= 0 {
		return nil, fmt.Errorf("invalid volume size: must be greater than 0")
	}

	// size is in bytes and we need it in MB
	sizeMB := size / sizeConversion
	if sizeMB <= 0 {
		return nil, fmt.Errorf("invalid volume size: must be greater than 0 MB")
	}

	candidates, err := p.resolveProvisioningDatastores(ctx, selection)
	if err != nil {
		return nil, err
	}

	tpl := img.NewTemplate()
	tpl.Add(imk.Name, name)
	tpl.Add(imk.Size, int(sizeMB))
	tpl.Add(imk.Persistent, "YES")
	tpl.AddPair(ownerTag, owner)
	tpl.Add(imk.Type, string(image.Datablock))
	if immutable {
		tpl.Add(imk.PersistentType, "SHAREABLE")
	}
	if fsType != "" {
		tpl.Add(fsTypeTag, fsType)
	}

	if params != nil && params["devPrefix"] != "" {
		tpl.Add(imk.DevPrefix, params["devPrefix"])
	}

	var insufficientCapacity []string
	attemptedDatastores := make([]int, 0, len(candidates))
	for _, datastore := range candidates {
		attemptedDatastores = append(attemptedDatastores, datastore.ID)
		if datastore.FreeBytes < size {
			recordDatastoreProvisioningResult(datastore.ID, false)
			insufficientCapacity = append(insufficientCapacity, fmt.Sprintf("%d", datastore.ID))
			continue
		}

		finishAttempt := beginDatastoreAttempt(datastore.ID)
		imageID, err := p.ctrl.Images().CreateContext(ctx, tpl.String(), uint(datastore.ID))
		finishAttempt()
		if err != nil {
			recordDatastoreProvisioningResult(datastore.ID, false)
			if isInsufficientCapacityError(err) {
				insufficientCapacity = append(insufficientCapacity, fmt.Sprintf("%d", datastore.ID))
				continue
			}

			return nil, fmt.Errorf("failed to create volume on datastore %d: %w", datastore.ID, err)
		}
		recordDatastoreProvisioningResult(datastore.ID, true)

		err = p.waitForResourceStatus(imageID, p.hotplugPolicy.BaseTimeout, p.volumeReady)
		if err != nil {
			return nil, fmt.Errorf("failed to wait for volume readiness: %w", err)
		}

		return &VolumeCreateResult{
			Datastore:             datastore,
			CapacityBytes:         size,
			FallbackUsed:          len(attemptedDatastores) > 1,
			AttemptedDatastoreIDs: append([]int(nil), attemptedDatastores...),
		}, nil
	}

	return nil, &datastoreCapacityError{
		message: fmt.Sprintf("none of the configured datastores had enough free capacity for %d bytes; attempted datastores: %s", size, strings.Join(insufficientCapacity, ",")),
	}
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
	err = p.waitForResourceStatus(volumeID, p.hotplugPolicy.BaseTimeout, p.volumeDeleted)
	if err != nil {
		return fmt.Errorf("failed to wait for volume deletion: %w", err)
	}
	return nil
}

func (p *PersistentDiskVolumeProvider) CloneVolume(ctx context.Context, name string, sourceVolume string, selection DatastoreSelectionConfig) (*VolumeCreateResult, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("clone name cannot be empty")
	}
	if strings.TrimSpace(sourceVolume) == "" {
		return nil, fmt.Errorf("source volume cannot be empty")
	}

	sourceVolumeID, _, err := p.VolumeExists(ctx, sourceVolume)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve source volume %q: %w", sourceVolume, err)
	}
	if sourceVolumeID == -1 {
		return nil, fmt.Errorf("source volume %q was not found", sourceVolume)
	}

	sourceImage, err := p.ctrl.Image(sourceVolumeID).InfoContext(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect source volume %q: %w", sourceVolume, err)
	}

	candidates, err := p.resolveProvisioningDatastores(ctx, selection)
	if err != nil {
		return nil, err
	}

	sourceSizeBytes := int64(sourceImage.Size * sizeConversion)
	attemptedDatastores := make([]int, 0, len(candidates))
	insufficientCapacity := make([]string, 0)
	for _, datastore := range candidates {
		attemptedDatastores = append(attemptedDatastores, datastore.ID)
		if datastore.FreeBytes < sourceSizeBytes {
			recordDatastoreProvisioningResult(datastore.ID, false)
			insufficientCapacity = append(insufficientCapacity, strconv.Itoa(datastore.ID))
			continue
		}

		finishAttempt := beginDatastoreAttempt(datastore.ID)
		cloneID, err := p.ctrl.Image(sourceVolumeID).CloneContext(ctx, name, datastore.ID)
		finishAttempt()
		if err != nil {
			recordDatastoreProvisioningResult(datastore.ID, false)
			if isInsufficientCapacityError(err) {
				insufficientCapacity = append(insufficientCapacity, strconv.Itoa(datastore.ID))
				continue
			}
			return nil, fmt.Errorf("failed to clone volume %q into datastore %d: %w", sourceVolume, datastore.ID, err)
		}
		recordDatastoreProvisioningResult(datastore.ID, true)

		if err := p.waitForResourceStatus(cloneID, p.hotplugPolicy.BaseTimeout, p.volumeReady); err != nil {
			return nil, fmt.Errorf("failed waiting for clone %d to become ready: %w", cloneID, err)
		}

		return &VolumeCreateResult{
			Datastore:             datastore,
			CapacityBytes:         sourceSizeBytes,
			FallbackUsed:          len(attemptedDatastores) > 1,
			AttemptedDatastoreIDs: append([]int(nil), attemptedDatastores...),
		}, nil
	}

	return nil, &datastoreCapacityError{
		message: fmt.Sprintf("none of the configured datastores had enough free capacity to clone %q; attempted datastores: %s", sourceVolume, strings.Join(insufficientCapacity, ",")),
	}
}

func (p *PersistentDiskVolumeProvider) CreateSnapshot(ctx context.Context, sourceVolume string, snapshotName string) (*VolumeSnapshot, error) {
	if strings.TrimSpace(sourceVolume) == "" {
		return nil, fmt.Errorf("source volume cannot be empty")
	}

	sourceVolumeID, sourceSize, err := p.VolumeExists(ctx, sourceVolume)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve source volume %q: %w", sourceVolume, err)
	}
	if sourceVolumeID == -1 {
		return nil, fmt.Errorf("source volume %q was not found", sourceVolume)
	}

	if strings.TrimSpace(snapshotName) == "" {
		snapshotName = fmt.Sprintf("%s-snapshot", sourceVolume)
	}

	response, err := p.ctrl.Client.CallContext(ctx, "one.image.snapshotcreate", sourceVolumeID, snapshotName)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot for volume %q: %w", sourceVolume, err)
	}

	snapshotID := response.BodyInt()
	imageInfo, err := p.ctrl.Image(sourceVolumeID).InfoContext(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect snapshot metadata for volume %q: %w", sourceVolume, err)
	}

	snapshot, err := imageSnapshotByID(imageInfo, snapshotID)
	if err != nil {
		return nil, err
	}

	return &VolumeSnapshot{
		SnapshotID:     EncodeVolumeSnapshotID(sourceVolumeID, snapshotID),
		SourceVolumeID: sourceVolume,
		CreationTime:   time.Unix(int64(snapshot.Date), 0).UTC(),
		SizeBytes:      int64(sourceSize),
		ReadyToUse:     true,
	}, nil
}

func (p *PersistentDiskVolumeProvider) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	imageID, imageSnapshotID, err := DecodeVolumeSnapshotID(snapshotID)
	if err != nil {
		return err
	}

	err = p.ctrl.Image(imageID).Snapshot(imageSnapshotID).DeleteContext(ctx)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not found") {
		return nil
	}
	return err
}

func (p *PersistentDiskVolumeProvider) ListSnapshots(ctx context.Context, snapshotID string, sourceVolumeID string, maxEntries int32, startingToken string) ([]VolumeSnapshot, string, error) {
	snapshots := make([]VolumeSnapshot, 0)

	switch {
	case strings.TrimSpace(snapshotID) != "":
		imageID, imageSnapshotID, err := DecodeVolumeSnapshotID(snapshotID)
		if err != nil {
			return nil, "", err
		}

		imageInfo, err := p.ctrl.Image(imageID).InfoContext(ctx, true)
		if err != nil {
			return nil, "", fmt.Errorf("failed to inspect image %d while listing snapshot %q: %w", imageID, snapshotID, err)
		}
		snapshot, err := imageSnapshotByID(imageInfo, imageSnapshotID)
		if err != nil {
			return nil, "", err
		}
		snapshots = append(snapshots, convertImageSnapshot(imageInfo, snapshot))
	case strings.TrimSpace(sourceVolumeID) != "":
		imageID, _, err := p.VolumeExists(ctx, sourceVolumeID)
		if err != nil {
			return nil, "", fmt.Errorf("failed to resolve source volume %q: %w", sourceVolumeID, err)
		}
		if imageID == -1 {
			return nil, "", fmt.Errorf("source volume %q was not found", sourceVolumeID)
		}

		imageInfo, err := p.ctrl.Image(imageID).InfoContext(ctx, true)
		if err != nil {
			return nil, "", fmt.Errorf("failed to inspect source volume %q: %w", sourceVolumeID, err)
		}
		snapshots = append(snapshots, convertImageSnapshots(imageInfo)...)
	default:
		pool, err := p.ctrl.Images().InfoContext(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("failed to list images while enumerating snapshots: %w", err)
		}
		for _, imageInfo := range pool.Images {
			imageCopy := imageInfo
			snapshots = append(snapshots, convertImageSnapshots(&imageCopy)...)
		}
	}

	start := 0
	if strings.TrimSpace(startingToken) != "" {
		value, err := strconv.Atoi(startingToken)
		if err != nil || value < 0 {
			return nil, "", fmt.Errorf("invalid starting token %q", startingToken)
		}
		start = value
	}
	if start > len(snapshots) {
		return nil, "", fmt.Errorf("starting token %q is out of range", startingToken)
	}

	end := len(snapshots)
	nextToken := ""
	if maxEntries > 0 && start+int(maxEntries) < len(snapshots) {
		end = start + int(maxEntries)
		nextToken = strconv.Itoa(end)
	}

	return append([]VolumeSnapshot(nil), snapshots[start:end]...), nextToken, nil
}

func (p *PersistentDiskVolumeProvider) ExpandVolume(ctx context.Context, volume string, size int64, allowDetached bool) (int64, error) {
	if size <= 0 {
		return 0, fmt.Errorf("invalid requested volume size: must be greater than 0")
	}

	volumeID, currentSize, err := p.VolumeExists(ctx, volume)
	if err != nil {
		return 0, fmt.Errorf("failed to check if volume exists: %w", err)
	}
	if volumeID == -1 {
		return 0, fmt.Errorf("volume %s was not found", volume)
	}

	if size < int64(currentSize) {
		return 0, &datastoreConfigError{
			message: fmt.Sprintf("shrinking volumes is not supported: requested %d bytes but current size is %d bytes", size, currentSize),
		}
	}

	if size == int64(currentSize) {
		return int64(currentSize), nil
	}

	vmID, diskID, attached, attachedSizeBytes, err := p.findAttachedVolumeDisk(ctx, volumeID)
	if err != nil {
		return 0, err
	}

	sizeMB := size / sizeConversion
	if size%sizeConversion != 0 {
		sizeMB++
	}

	if attached {
		if attachedSizeBytes >= size {
			return attachedSizeBytes, nil
		}

		err = p.ctrl.VM(vmID).Disk(diskID).ResizeContext(ctx, strconv.FormatInt(sizeMB, 10))
		if err != nil {
			if attachedSizeBytes, sizeErr := p.attachedDiskSizeBytes(ctx, vmID, diskID, volumeID); sizeErr == nil && attachedSizeBytes >= size {
				return attachedSizeBytes, nil
			}
			return 0, fmt.Errorf("failed to expand volume %s on vm %d disk %d: %w", volume, vmID, diskID, err)
		}
	} else {
		if !allowDetached {
			return 0, &datastoreConfigError{
				message: "expanding detached OpenNebula persistent disks is disabled; attach the volume or enable the detachedDiskExpansion feature gate and retry expansion",
			}
		}

		tpl := img.NewTemplate()
		tpl.Add(imk.Size, int(sizeMB))
		if err := p.ctrl.Image(volumeID).UpdateContext(ctx, tpl.String(), parameters.Merge); err != nil {
			return 0, fmt.Errorf("failed to expand detached volume %s via image update: %w", volume, err)
		}
	}

	var completedSizeBytes int64
	if attached {
		err = p.waitForResourceStatus(volumeID, volumeUsedTimeout, func(resourceID int) (bool, error) {
			currentSizeBytes, sizeErr := p.attachedDiskSizeBytes(ctx, vmID, diskID, resourceID)
			if sizeErr != nil {
				return false, sizeErr
			}
			if currentSizeBytes >= size {
				completedSizeBytes = currentSizeBytes
				return true, nil
			}
			return false, nil
		})
	} else {
		err = p.waitForResourceStatus(volumeID, volumeUsedTimeout, func(resourceID int) (bool, error) {
			imageInfo, infoErr := p.ctrl.Image(resourceID).Info(true)
			if infoErr != nil {
				return false, fmt.Errorf("failed to get volume info after resize: %w", infoErr)
			}
			currentSizeBytes := int64(imageInfo.Size * sizeConversion)
			if currentSizeBytes >= size {
				completedSizeBytes = currentSizeBytes
				return true, nil
			}
			return false, nil
		})
	}
	if err != nil {
		return 0, fmt.Errorf("failed waiting for volume resize to complete: %w", err)
	}

	if completedSizeBytes == 0 {
		completedSizeBytes = sizeMB * sizeConversion
	}

	return completedSizeBytes, nil
}

func (p *PersistentDiskVolumeProvider) AttachVolume(ctx context.Context, volume string, node string, immutable bool, params map[string]string) error {
	nodeID, err := p.NodeExists(ctx, node)
	if err != nil || nodeID == -1 {
		return fmt.Errorf("failed to check if node exists: %w", err)
	}

	volumeID, _, err := p.VolumeExists(ctx, volume)
	if err != nil || volumeID == -1 {
		return fmt.Errorf("failed to check if volume exists: %w", err)
	}
	sizeBytes, err := p.ResolveVolumeSizeBytes(ctx, volume)
	if err != nil {
		return fmt.Errorf("failed to resolve volume size for attach of %s: %w", volume, err)
	}
	hotplugTimeout := p.ComputeHotplugTimeout(sizeBytes)

	report, err := p.inspectAttachmentEnvironment(ctx, volumeID, nodeID)
	if err != nil {
		return err
	}
	if report != nil {
		klog.V(1).InfoS("Validated volume attachment environment",
			"volumeID", volumeID,
			"nodeID", nodeID,
			"imageDatastoreID", report.ImageDatastore.ID,
			"imageDatastoreName", report.ImageDatastore.Name,
			"imageDatastoreType", report.ImageDatastore.Type,
			"backendProfile", report.ImageDatastore.Backend,
			"deploymentMode", report.DeploymentMode)
		if report.SystemDatastore != nil {
			klog.V(1).InfoS("Resolved target system datastore for attachment",
				"volumeID", volumeID,
				"nodeID", nodeID,
				"systemDatastoreID", report.SystemDatastore.ID,
				"systemDatastoreName", report.SystemDatastore.Name,
				"systemDatastoreTMMAD", report.SystemDatastore.TMMad)
		}
		for _, warning := range report.Warnings {
			klog.Warningf("volume %s attach validation warning: %s", volume, warning)
		}
	}
	hostArtifactTarget, targetErr := p.hostArtifactAttachmentTargetForResolved(ctx, volume, node, volumeID, nodeID, params, report)
	if targetErr != nil {
		klog.V(3).InfoS("Failed to predict host artifact attachment target",
			"volume", volume,
			"node", node,
			"err", targetErr)
	}
	disk := shared.NewDisk()
	disk.Add(shared.ImageID, volumeID)
	disk.Add(shared.Serial, fmt.Sprintf("onecsi-%d", volumeID))
	addDiskParams(disk, params)
	if immutable {
		disk.Add("READONLY", "YES")
	}
	klog.V(1).InfoS("Computed dynamic hotplug timeout for attach",
		"volume", volume,
		"node", node,
		"sizeBytes", sizeBytes,
		"timeout", hotplugTimeout)

	attachDisk := func() error {
		return p.ctrl.VM(nodeID).DiskAttach(disk.String())
	}

	attachErr := attachDisk()
	if attachErr != nil && !isHotplugStateError(attachErr) {
		if conflict, ok := p.classifyHostArtifactConflict(ctx, nodeID, report, hostArtifactTarget, attachErr); ok {
			return conflict
		}
		return fmt.Errorf("failed to attach volume %s to node %s: %w",
			volume, node, attachErr)
	}

	var retryAttach func() error
	if attachErr != nil {
		retryAttach = attachDisk
	}

	if err := p.waitForAttachState(ctx, hotplugTimeout, p.hotplugPollWait, func() (bool, bool, error) {
		attached := false
		if _, infoErr := p.GetVolumeInNode(ctx, volumeID, nodeID); infoErr == nil {
			attached = true
		}
		ready, readyErr := p.nodeReady(nodeID)
		if readyErr != nil {
			return attached, false, readyErr
		}
		return attached, ready, nil
	}, retryAttach, volume, node); err != nil {
		if conflict, ok := p.classifyHostArtifactConflict(ctx, nodeID, report, hostArtifactTarget, err); ok {
			return conflict
		}
		if attachErr != nil {
			return fmt.Errorf("%w (initial attach error: %v)", err, attachErr)
		}
		return err
	}
	return nil
}

func (p *PersistentDiskVolumeProvider) InspectHostArtifactAttachmentTarget(ctx context.Context, volume string, node string, params map[string]string) (*HostArtifactAttachmentTarget, error) {
	nodeID, err := p.NodeExists(ctx, node)
	if err != nil || nodeID == -1 {
		return nil, fmt.Errorf("failed to check if node exists: %w", err)
	}

	volumeID, _, err := p.VolumeExists(ctx, volume)
	if err != nil || volumeID == -1 {
		return nil, fmt.Errorf("failed to check if volume exists: %w", err)
	}

	report, err := p.inspectAttachmentEnvironment(ctx, volumeID, nodeID)
	if err != nil {
		return nil, err
	}
	return p.hostArtifactAttachmentTargetForResolved(ctx, volume, node, volumeID, nodeID, params, report)
}

func (p *PersistentDiskVolumeProvider) hostArtifactAttachmentTargetForResolved(ctx context.Context, volume, node string, volumeID, nodeID int, params map[string]string, report *DatastoreEnvironmentReport) (*HostArtifactAttachmentTarget, error) {
	if !hostArtifactCandidateDeployment(report) {
		return nil, nil
	}
	vmInfo, err := p.ctrl.VM(nodeID).InfoContext(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect node %d for host artifact target: %w", nodeID, err)
	}

	diskID := nextAvailableDiskID(vmInfo)
	target := nextAvailableDiskTarget(vmInfo, params)
	candidate := &HostArtifactAttachmentTarget{
		VolumeHandle: volume,
		ImageID:      volumeID,
		NodeName:     node,
		VMID:         nodeID,
		DiskID:       diskID,
		Target:       target,
	}
	if diskID > 0 {
		candidate.LVName = fmt.Sprintf("lv-one-%d-%d", nodeID, diskID)
	}
	if report != nil && report.SystemDatastore != nil {
		candidate.SystemDatastoreID = report.SystemDatastore.ID
		candidate.SystemDatastoreName = report.SystemDatastore.Name
		candidate.SystemDatastoreTM = report.SystemDatastore.TMMad
	}
	if history := latestHistoryRecord(vmInfo); history != nil {
		candidate.HostID = history.HID
		candidate.HostName = history.Hostname
	}
	return candidate, nil
}

func (p *PersistentDiskVolumeProvider) classifyHostArtifactConflict(ctx context.Context, nodeID int, report *DatastoreEnvironmentReport, target *HostArtifactAttachmentTarget, cause error) (*HostArtifactConflictError, bool) {
	if !hostArtifactCandidateDeployment(report) {
		return nil, false
	}
	candidate := HostArtifactAttachmentTarget{}
	if target != nil {
		candidate = *target
	}
	if conflict, ok := HostArtifactConflictFromMessage(causeMessage(cause), candidate, cause); ok {
		return conflict, true
	}
	vmError := strings.TrimSpace(p.latestVMError(ctx, nodeID))
	if vmError == "" {
		return nil, false
	}
	return HostArtifactConflictFromMessage(vmError, candidate, cause)
}

func (p *PersistentDiskVolumeProvider) latestVMError(ctx context.Context, nodeID int) string {
	vmInfo, err := p.ctrl.VM(nodeID).InfoContext(ctx, true)
	if err != nil {
		return ""
	}
	if value, err := vmInfo.UserTemplate.GetStr("ERROR"); err == nil {
		return value
	}
	return ""
}

func hostArtifactCandidateDeployment(report *DatastoreEnvironmentReport) bool {
	if report == nil || report.SystemDatastore == nil {
		return false
	}
	tm := strings.ToLower(strings.TrimSpace(report.SystemDatastore.TMMad))
	return report.DeploymentMode == DeploymentModeSSH &&
		(strings.Contains(tm, "lvm") || strings.Contains(tm, "fs_lvm"))
}

func causeMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func nextAvailableDiskID(vmInfo *vm.VM) int {
	if vmInfo == nil {
		return 0
	}
	used := map[int]struct{}{}
	for _, disk := range vmInfo.Template.GetDisks() {
		if diskID, err := disk.GetI(shared.DiskID); err == nil && diskID >= 0 {
			used[diskID] = struct{}{}
		}
	}
	if raw, err := vmInfo.Template.GetStrFromVec("CONTEXT", "DISK_ID"); err == nil {
		if diskID, convErr := strconv.Atoi(strings.TrimSpace(raw)); convErr == nil && diskID >= 0 {
			used[diskID] = struct{}{}
		}
	}
	for diskID := 0; diskID < 512; diskID++ {
		if _, ok := used[diskID]; !ok {
			return diskID
		}
	}
	return len(used)
}

func nextAvailableDiskTarget(vmInfo *vm.VM, params map[string]string) string {
	prefix := "sd"
	if params != nil && strings.TrimSpace(params["devPrefix"]) != "" {
		prefix = strings.TrimSpace(params["devPrefix"])
	}
	used := map[int]struct{}{}
	if vmInfo != nil {
		for _, disk := range vmInfo.Template.GetDisks() {
			target, err := disk.Get(shared.TargetDisk)
			if err != nil {
				continue
			}
			if index, ok := linuxDiskTargetIndex(strings.TrimSpace(target), prefix); ok {
				used[index] = struct{}{}
			}
		}
	}
	for index := 0; index < 512; index++ {
		if _, ok := used[index]; !ok {
			return prefix + linuxDiskLetters(index)
		}
	}
	return ""
}

func linuxDiskTargetIndex(target, prefix string) (int, bool) {
	if prefix == "" || !strings.HasPrefix(target, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(target, prefix)
	if suffix == "" {
		return 0, false
	}
	index := 0
	for _, char := range suffix {
		if char < 'a' || char > 'z' {
			return 0, false
		}
		index = index*26 + int(char-'a'+1)
	}
	return index - 1, true
}

func linuxDiskLetters(index int) string {
	if index < 0 {
		index = 0
	}
	letters := ""
	for {
		remainder := index % 26
		letters = string(rune('a'+remainder)) + letters
		index = index/26 - 1
		if index < 0 {
			break
		}
	}
	return letters
}

func (p *PersistentDiskVolumeProvider) inspectAttachmentEnvironment(ctx context.Context, volumeID, nodeID int) (*DatastoreEnvironmentReport, error) {
	imageInfo, err := p.ctrl.Image(volumeID).Info(true)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect image %d before attach: %w", volumeID, err)
	}

	if imageInfo.DatastoreID == nil {
		return nil, &datastoreConfigError{message: fmt.Sprintf("volume %d does not expose a source datastore ID", volumeID)}
	}

	datastorePool, err := p.ctrl.Datastores().Info()
	if err != nil {
		return nil, fmt.Errorf("failed to inspect datastores before attach: %w", err)
	}

	imageDatastore, err := findDatastoreByID(datastorePool.Datastores, *imageInfo.DatastoreID)
	if err != nil {
		return nil, err
	}

	report := &DatastoreEnvironmentReport{
		ImageDatastore: datastoreFromSchema(imageDatastore),
		DeploymentMode: DeploymentModeUnknown,
	}

	vmInfo, err := p.ctrl.VM(nodeID).Info(true)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect node %d before attach: %w", nodeID, err)
	}

	systemDatastoreID, err := latestHistoryDatastoreID(vmInfo)
	if err != nil {
		return nil, err
	}

	systemDatastore, err := findDatastoreByID(datastorePool.Datastores, systemDatastoreID)
	if err != nil {
		return nil, err
	}

	normalizedSystemDatastore := datastoreFromSchema(systemDatastore)
	report.SystemDatastore = &normalizedSystemDatastore
	report.DeploymentMode = resolveDeploymentMode(systemDatastore)

	if err := validateCompatibleSystemDatastore(imageDatastore, systemDatastoreID); err != nil {
		return nil, err
	}

	if report.ImageDatastore.Type != datastoreTypeCeph {
		return report, nil
	}

	if err := validateCephImageDatastore(imageDatastore); err != nil {
		return nil, err
	}

	switch report.DeploymentMode {
	case DeploymentModeCeph:
		if err := validateCephSystemDatastore(systemDatastore); err != nil {
			return nil, err
		}
		if err := compareCephConnectionIdentity(imageDatastore, systemDatastore); err != nil {
			return nil, err
		}
	case DeploymentModeSSH:
		report.Warnings = append(report.Warnings, "target VM is using an SSH system datastore; OpenNebula SSH mode limitations apply to Ceph-backed images")
	default:
		return nil, &datastoreConfigError{
			message: fmt.Sprintf("system datastore %d uses unsupported TM_MAD %q for ceph-backed volume %d", systemDatastore.ID, systemDatastore.TMMad, volumeID),
		}
	}

	return report, nil
}

func addDiskParams(disk *shared.Disk, params map[string]string) {
	for key, val := range params {
		if val == "" {
			continue
		}
		switch key {
		case "devPrefix":
			disk.Add(shared.DevPrefix, val)
		case "cache":
			disk.Add(shared.Cache, val)
		case "driver":
			disk.Add(shared.Driver, val)
		case "io":
			disk.Add(shared.IO, val)
		case "ioThread":
			disk.Add(shared.IOThread, val)
		case "virtioBLKQueues":
			disk.Add(shared.VirtioBLKQueues, val)
		case "totalBytesSec":
			disk.Add(shared.TotalBytesSec, val)
		case "readBytesSec":
			disk.Add(shared.ReadBytesSec, val)
		case "writeBytesSec":
			disk.Add(shared.WriteBytesSec, val)
		case "totalIOPSSec":
			disk.Add(shared.TotalIOPSSec, val)
		case "readIOPSSec":
			disk.Add(shared.ReadIOPSSec, val)
		case "writeIOPSSec":
			disk.Add(shared.WriteIOPSSec, val)
		case "totalBytesSecMax":
			disk.Add(shared.TotalBytesSecMax, val)
		case "readBytesSecMax":
			disk.Add(shared.ReadBytesSecMax, val)
		case "writeBytesSecMax":
			disk.Add(shared.WriteBytesSecMax, val)
		case "totalIOPSSecMax":
			disk.Add(shared.TotalIOPSSecMax, val)
		case "readIOPSSecMax":
			disk.Add(shared.ReadIOPSSecMax, val)
		case "writeIOPSSecMax":
			disk.Add(shared.WriteIOPSSecMax, val)
		case "totalBytesSecMaxLength":
			disk.Add(shared.TotalBytesSecMaxLength, val)
		case "readBytesSecMaxLength":
			disk.Add(shared.ReadBytesSecMaxLength, val)
		case "writeBytesSecMaxLength":
			disk.Add(shared.WriteBytesSecMaxLength, val)
		case "totalIOPSSecMaxLength":
			disk.Add(shared.TotalIOPSSecMaxLength, val)
		case "readIOPSSecMaxLength":
			disk.Add(shared.ReadIOPSSecMaxLength, val)
		case "writeIOPSSecMaxLength":
			disk.Add(shared.WriteIOPSSecMaxLength, val)
		case "sizeIOPSSec":
			disk.Add(shared.SizeIOPSSec, val)
		}
	}
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
	sizeBytes, err := p.resolveDetachVolumeSizeBytes(ctx, volumeID)
	if err != nil {
		return fmt.Errorf("failed to resolve volume size for detach of %s: %w", volume, err)
	}
	hotplugTimeout := p.ComputeHotplugTimeout(sizeBytes)
	klog.V(1).InfoS("Computed dynamic hotplug timeout for detach",
		"volume", volume,
		"node", node,
		"sizeBytes", sizeBytes,
		"timeout", hotplugTimeout)

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
			detachDisk := func() error {
				return vmController.Disk(diskIDInt).Detach()
			}
			detachErr := detachDisk()
			if detachErr != nil && !isHotplugStateError(detachErr) {
				return fmt.Errorf("failed to detach volume %s from node %s: %w",
					diskID, node, detachErr)
			}

			var retryDetach func() error
			if detachErr != nil {
				retryDetach = detachDisk
			}

			if err := p.waitForDetachState(ctx, hotplugTimeout, p.hotplugPollWait, func() (bool, bool, error) {
				attached := false
				if _, infoErr := p.GetVolumeInNode(ctx, volumeID, nodeID); infoErr == nil {
					attached = true
				}
				ready, readyErr := p.nodeReady(nodeID)
				if readyErr != nil {
					return attached, false, readyErr
				}
				return attached, ready, nil
			}, retryDetach, volume, node); err != nil {
				if detachErr != nil {
					return fmt.Errorf("%w (initial detach error: %v)", err, detachErr)
				}
				return err
			}
			return nil
		}
	}
	klog.V(1).InfoS("Volume already absent from node, treating detach as successful",
		"method", "DetachVolume", "volumeID", volumeID, "volumeHandle", volume, "nodeID", nodeID, "node", node)
	return nil
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

func (p *PersistentDiskVolumeProvider) GetCapacity(ctx context.Context, selection DatastoreSelectionConfig) (int64, error) {
	candidates, err := p.resolveProvisioningDatastores(ctx, selection)
	if err != nil {
		return 0, err
	}

	return SumDatastoreCapacity(candidates), nil
}

func (p *PersistentDiskVolumeProvider) ResolveProvisioningDatastores(ctx context.Context, selection DatastoreSelectionConfig) ([]Datastore, error) {
	return p.resolveProvisioningDatastores(ctx, selection)
}

func (p *PersistentDiskVolumeProvider) resolveProvisioningDatastores(ctx context.Context, selection DatastoreSelectionConfig) ([]Datastore, error) {
	datastores, err := p.ctrl.Datastores().Info()
	if err != nil {
		return nil, fmt.Errorf("failed to get datastores info: %w", err)
	}

	resolved, err := ResolveDatastores(datastores.Datastores, selection)
	if err != nil {
		return nil, err
	}

	ordered, err := OrderDatastores(resolved, selection.Policy)
	if err != nil {
		return nil, err
	}

	return ordered, nil
}

func (p *PersistentDiskVolumeProvider) findAttachedVolumeDisk(ctx context.Context, volumeID int) (int, int, bool, int64, error) {
	vmPool, err := p.ctrl.VMs().InfoContext(ctx)
	if err != nil {
		return 0, 0, false, 0, fmt.Errorf("failed to get vm info while locating attached volume %d: %w", volumeID, err)
	}

	var attachedVMID int
	var attachedDiskID int
	var attachedSizeBytes int64
	found := false
	for _, vmInfo := range vmPool.VMs {
		for _, disk := range vmInfo.Template.GetDisks() {
			diskImageID, diskErr := disk.GetI(shared.ImageID)
			if diskErr != nil || diskImageID != volumeID {
				continue
			}

			diskIDStr, diskIDErr := disk.Get(shared.DiskID)
			if diskIDErr != nil {
				return 0, 0, false, 0, fmt.Errorf("failed to determine disk ID for volume %d on vm %d: %w", volumeID, vmInfo.ID, diskIDErr)
			}
			diskID, convErr := strconv.Atoi(diskIDStr)
			if convErr != nil {
				return 0, 0, false, 0, fmt.Errorf("invalid disk ID for volume %d on vm %d: %w", volumeID, vmInfo.ID, convErr)
			}

			if found {
				return 0, 0, false, 0, &datastoreConfigError{
					message: fmt.Sprintf("volume %d is attached to multiple virtual machines; expansion currently requires a single attachment", volumeID),
				}
			}

			diskSizeBytes, sizeErr := diskSizeBytesFromTemplate(disk)
			if sizeErr != nil {
				return 0, 0, false, 0, fmt.Errorf("failed to determine attached size for volume %d on vm %d disk %d: %w", volumeID, vmInfo.ID, diskID, sizeErr)
			}

			attachedVMID = vmInfo.ID
			attachedDiskID = diskID
			attachedSizeBytes = diskSizeBytes
			found = true
		}
	}

	if !found {
		return 0, 0, false, 0, nil
	}

	return attachedVMID, attachedDiskID, true, attachedSizeBytes, nil
}

func (p *PersistentDiskVolumeProvider) attachedDiskSizeBytes(ctx context.Context, vmID, diskID, volumeID int) (int64, error) {
	vmInfo, err := p.ctrl.VM(vmID).InfoContext(ctx, true)
	if err != nil {
		return 0, fmt.Errorf("failed to inspect vm %d while checking attached volume %d size: %w", vmID, volumeID, err)
	}

	for _, disk := range vmInfo.Template.GetDisks() {
		diskImageID, diskErr := disk.GetI(shared.ImageID)
		if diskErr != nil || diskImageID != volumeID {
			continue
		}

		currentDiskID, idErr := disk.GetI(shared.DiskID)
		if idErr != nil || currentDiskID != diskID {
			continue
		}

		return diskSizeBytesFromTemplate(disk)
	}

	return 0, fmt.Errorf("failed to locate attached disk %d for volume %d on vm %d", diskID, volumeID, vmID)
}

func diskSizeBytesFromTemplate(disk shared.Disk) (int64, error) {
	sizeMB, err := disk.GetI(shared.Size)
	if err != nil {
		return 0, err
	}
	return int64(sizeMB) * sizeConversion, nil
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

func isInsufficientCapacityError(err error) bool {
	if err == nil {
		return false
	}

	message := strings.ToLower(err.Error())
	fragments := []string{
		"not enough space",
		"not enough free space",
		"insufficient space",
		"insufficient capacity",
		"no space left",
		"not enough capacity",
	}

	for _, fragment := range fragments {
		if strings.Contains(message, fragment) {
			return true
		}
	}

	return false
}

func isHotplugStateError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "wrong state hotplug")
}

func (p *PersistentDiskVolumeProvider) VolumeReadyWithTimeout(volumeID int) (bool, error) {
	err := p.waitForResourceStatus(volumeID, volumeUsedTimeout, p.volumeReady)
	if err != nil {
		return false, fmt.Errorf("failed to wait for volume ready: %w", err)
	}
	return p.volumeReady(volumeID)
}

func (p *PersistentDiskVolumeProvider) HotplugPolicy() HotplugTimeoutPolicy {
	return p.hotplugPolicy
}

func (p *PersistentDiskVolumeProvider) ComputeHotplugTimeout(sizeBytes int64) time.Duration {
	timeout := p.hotplugPolicy.BaseTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	if sizeBytes > 0 && p.hotplugPolicy.Per100GiB > 0 {
		sizeGi := int64(math.Ceil(float64(sizeBytes) / float64(sizeConversion*1024)))
		increments := int64(math.Ceil(float64(sizeGi) / 100.0))
		if increments < 1 {
			increments = 1
		}
		timeout += time.Duration(increments) * p.hotplugPolicy.Per100GiB
	}

	if p.hotplugPolicy.MaxTimeout > 0 && timeout > p.hotplugPolicy.MaxTimeout {
		timeout = p.hotplugPolicy.MaxTimeout
	}
	if timeout < p.hotplugPolicy.BaseTimeout {
		timeout = p.hotplugPolicy.BaseTimeout
	}

	return timeout
}

func (p *PersistentDiskVolumeProvider) ResolveVolumeSizeBytes(ctx context.Context, volume string) (int64, error) {
	volumeID, sizeBytes, err := p.VolumeExists(ctx, volume)
	if err != nil {
		return 0, err
	}
	if volumeID == -1 {
		return 0, fmt.Errorf("volume %s not found", volume)
	}
	if sizeBytes > 0 {
		return int64(sizeBytes), nil
	}
	return 0, fmt.Errorf("volume %s size is unavailable", volume)
}

func (p *PersistentDiskVolumeProvider) NodeExists(ctx context.Context, node string) (int, error) {
	vmID, _, err := ResolveNodeVMID(ctx, p.ctrl, node)
	if err != nil {
		return -1, fmt.Errorf("Failed to fetch VM: %w", err)
	}

	return vmID, nil
}

func (p *PersistentDiskVolumeProvider) NodeReady(ctx context.Context, node string) (bool, error) {
	nodeID, err := p.NodeExists(ctx, node)
	if err != nil {
		return false, err
	}
	return p.nodeReady(nodeID)
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

	return state == img.Ready, nil
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

func (p *PersistentDiskVolumeProvider) waitForAttachState(ctx context.Context, timeout, pollInterval time.Duration, observe func() (bool, bool, error), retryAttach func() error, volume, node string) error {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if pollInterval <= 0 {
		pollInterval = time.Second
	}

	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	lastAttached := false
	lastReady := false
	retryPending := retryAttach != nil

	for {
		attached, ready, err := observe()
		if err != nil {
			return fmt.Errorf("failed while waiting for attach of volume %s on node %s: %w", volume, node, err)
		}

		lastAttached = attached
		lastReady = ready
		if attached && ready {
			return nil
		}

		if retryPending && ready && !attached {
			retryErr := retryAttach()
			if retryErr != nil {
				if !isHotplugStateError(retryErr) {
					return fmt.Errorf("failed to retry attach of volume %s on node %s: %w", volume, node, retryErr)
				}
			} else {
				retryPending = false
			}
		}

		select {
		case <-deadlineCtx.Done():
			return &HotplugTimeoutError{
				Operation:            "attach",
				Volume:               volume,
				Node:                 node,
				Timeout:              timeout,
				LastObservedAttached: lastAttached,
				LastObservedReady:    lastReady,
				Cause: fmt.Errorf(
					"timed out after %s waiting for attach of volume %s on node %s (attached=%t ready=%t)",
					timeout,
					volume,
					node,
					lastAttached,
					lastReady,
				),
			}
		case <-ticker.C:
		}
	}
}

func (p *PersistentDiskVolumeProvider) waitForDetachState(ctx context.Context, timeout, pollInterval time.Duration, observe func() (bool, bool, error), retryDetach func() error, volume, node string) error {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if pollInterval <= 0 {
		pollInterval = time.Second
	}

	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	lastAttached := true
	lastReady := false
	retryPending := retryDetach != nil

	for {
		attached, ready, err := observe()
		if err != nil {
			return fmt.Errorf("failed while waiting for detach of volume %s on node %s: %w", volume, node, err)
		}

		lastAttached = attached
		lastReady = ready
		if !attached {
			return nil
		}

		if retryPending && ready && attached {
			retryErr := retryDetach()
			if retryErr != nil {
				if !isHotplugStateError(retryErr) {
					return fmt.Errorf("failed to retry detach of volume %s on node %s: %w", volume, node, retryErr)
				}
			} else {
				retryPending = false
			}
		}

		select {
		case <-deadlineCtx.Done():
			return &HotplugTimeoutError{
				Operation:            "detach",
				Volume:               volume,
				Node:                 node,
				Timeout:              timeout,
				LastObservedAttached: lastAttached,
				LastObservedReady:    lastReady,
				Cause: fmt.Errorf(
					"timed out after %s waiting for detach of volume %s on node %s (attached=%t ready=%t)",
					timeout,
					volume,
					node,
					lastAttached,
					lastReady,
				),
			}
		case <-ticker.C:
		}
	}
}

func (p *PersistentDiskVolumeProvider) resolveDetachVolumeSizeBytes(ctx context.Context, volumeID int) (int64, error) {
	vmID, diskID, attached, attachedSizeBytes, err := p.findAttachedVolumeDisk(ctx, volumeID)
	if err != nil {
		return 0, err
	}
	if attached && attachedSizeBytes > 0 {
		return attachedSizeBytes, nil
	}
	if attached {
		sizeBytes, sizeErr := p.attachedDiskSizeBytes(ctx, vmID, diskID, volumeID)
		if sizeErr == nil && sizeBytes > 0 {
			return sizeBytes, nil
		}
	}

	image, err := p.ctrl.Image(volumeID).Info(true)
	if err != nil {
		return 0, err
	}
	if image.Size <= 0 {
		return 0, fmt.Errorf("volume %d size is unavailable", volumeID)
	}
	return int64(image.Size * sizeConversion), nil
}

func (p *PersistentDiskVolumeProvider) waitForResourceStatus(volumeID int, timeout time.Duration, checkFunc func(int) (bool, error)) error {
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

func (p *PersistentDiskVolumeProvider) InspectVolumeAttachment(ctx context.Context, volume string, node string) (*VolumeAttachmentMetadata, error) {
	volumeID, _, err := p.VolumeExists(ctx, volume)
	if err != nil {
		return nil, fmt.Errorf("failed to check if volume exists while inspecting attachment metadata: %w", err)
	}
	if volumeID == -1 {
		return nil, fmt.Errorf("volume %s was not found while inspecting attachment metadata", volume)
	}

	requestedNodeID := -1
	if strings.TrimSpace(node) != "" {
		requestedNodeID, err = p.NodeExists(ctx, node)
		if err != nil {
			return nil, fmt.Errorf("failed to check if node exists while inspecting attachment metadata: %w", err)
		}
	}

	imageInfo, err := p.ctrl.Image(volumeID).InfoContext(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect image %d attachment metadata: %w", volumeID, err)
	}

	imageState := fmt.Sprintf("UNKNOWN_%d", imageInfo.StateRaw)
	if parsed, stateErr := imageInfo.StateString(); stateErr == nil {
		imageState = parsed
	}

	metadata := &VolumeAttachmentMetadata{
		VolumeHandle:    volume,
		ImageID:         volumeID,
		ImageName:       imageInfo.Name,
		ImageState:      imageState,
		ImageStateRaw:   imageInfo.StateRaw,
		ImageRunningVMs: imageInfo.RunningVMs,
		ImageVMIDs:      append([]int(nil), imageInfo.VMs.ID...),
		RequestedNode:   strings.TrimSpace(node),
		RequestedNodeID: requestedNodeID,
	}

	vmPool, err := p.ctrl.VMs().InfoContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list virtual machines while inspecting volume %d attachment metadata: %w", volumeID, err)
	}
	for _, vmInfo := range vmPool.VMs {
		for _, disk := range vmInfo.Template.GetDisks() {
			diskImageID, diskErr := disk.GetI(shared.ImageID)
			if diskErr != nil || diskImageID != volumeID {
				continue
			}
			record := VolumeDiskRecord{
				NodeName: vmInfo.Name,
				NodeID:   vmInfo.ID,
			}
			if diskID, diskIDErr := disk.GetI(shared.DiskID); diskIDErr == nil {
				record.DiskID = diskID
			}
			if target, targetErr := disk.Get(shared.TargetDisk); targetErr == nil {
				record.Target = target
			}
			if serial, serialErr := disk.Get(shared.Serial); serialErr == nil {
				record.Serial = serial
			}
			if requestedNodeID > 0 && vmInfo.ID == requestedNodeID {
				metadata.AttachedToRequestedNode = true
			}
			metadata.DiskRecords = append(metadata.DiskRecords, record)
		}
	}

	return metadata, nil
}

func (p *PersistentDiskVolumeProvider) ListCurrentAttachments(ctx context.Context) ([]ObservedAttachment, error) {
	vmPool, err := p.ctrl.VMs().InfoContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list virtual machines: %w", err)
	}

	attachments := make([]ObservedAttachment, 0)
	imageCache := map[int]struct {
		name    string
		backend string
	}{}
	for _, vmInfo := range vmPool.VMs {
		for _, disk := range vmInfo.Template.GetDisks() {
			diskImageID, diskErr := disk.GetI(shared.ImageID)
			if diskErr != nil || diskImageID <= 0 {
				continue
			}
			diskID, diskIDErr := disk.GetI(shared.DiskID)
			if diskIDErr != nil {
				continue
			}
			imageMeta, ok := imageCache[diskImageID]
			if !ok {
				imageInfo, imageErr := p.ctrl.Image(diskImageID).Info(true)
				if imageErr != nil {
					return nil, fmt.Errorf("failed to inspect image %d while listing attachments: %w", diskImageID, imageErr)
				}
				backend := ""
				if imageInfo.DatastoreID != nil {
					if datastorePool, poolErr := p.ctrl.Datastores().InfoContext(ctx); poolErr == nil {
						if datastore, dsErr := findDatastoreByID(datastorePool.Datastores, *imageInfo.DatastoreID); dsErr == nil {
							backend = datastoreFromSchema(datastore).Backend
						}
					}
				}
				imageMeta = struct {
					name    string
					backend string
				}{
					name:    imageInfo.Name,
					backend: backend,
				}
				imageCache[diskImageID] = imageMeta
			}
			attachments = append(attachments, ObservedAttachment{
				VolumeHandle: imageMeta.name,
				ImageID:      diskImageID,
				NodeName:     vmInfo.Name,
				NodeID:       vmInfo.ID,
				DiskID:       diskID,
				Backend:      imageMeta.backend,
			})
		}
	}
	return attachments, nil
}

func findDatastoreByID(pool []datastoreSchema.Datastore, id int) (datastoreSchema.Datastore, error) {
	for _, ds := range pool {
		if ds.ID == id {
			return ds, nil
		}
	}

	return datastoreSchema.Datastore{}, &datastoreConfigError{message: fmt.Sprintf("datastore %d was not found", id)}
}

func latestHistoryDatastoreID(vmInfo *vm.VM) (int, error) {
	if vmInfo == nil {
		return 0, &datastoreConfigError{message: "target VM information is not available"}
	}

	latest := latestHistoryRecord(vmInfo)

	if latest == nil {
		return 0, &datastoreConfigError{message: fmt.Sprintf("target VM %d does not expose a system datastore history record", vmInfo.ID)}
	}

	return latest.DSID, nil
}

func latestHistoryRecord(vmInfo *vm.VM) *vm.HistoryRecord {
	if vmInfo == nil {
		return nil
	}
	var latest *vm.HistoryRecord
	for idx := range vmInfo.HistoryRecords {
		record := &vmInfo.HistoryRecords[idx]
		if record.DSID < 0 {
			continue
		}
		if latest == nil || record.SEQ > latest.SEQ {
			latest = record
		}
	}
	return latest
}

func imageSnapshotByID(imageInfo *img.Image, snapshotID int) (shared.Snapshot, error) {
	for _, snapshot := range imageInfo.Snapshots.Snapshots {
		if snapshot.ID == snapshotID {
			return snapshot, nil
		}
	}

	return shared.Snapshot{}, fmt.Errorf("snapshot %d was not found in image %d", snapshotID, imageInfo.ID)
}

func convertImageSnapshots(imageInfo *img.Image) []VolumeSnapshot {
	converted := make([]VolumeSnapshot, 0, len(imageInfo.Snapshots.Snapshots))
	for _, snapshot := range imageInfo.Snapshots.Snapshots {
		converted = append(converted, convertImageSnapshot(imageInfo, snapshot))
	}
	return converted
}

func convertImageSnapshot(imageInfo *img.Image, snapshot shared.Snapshot) VolumeSnapshot {
	return VolumeSnapshot{
		SnapshotID:     EncodeVolumeSnapshotID(imageInfo.ID, snapshot.ID),
		SourceVolumeID: imageInfo.Name,
		CreationTime:   time.Unix(int64(snapshot.Date), 0).UTC(),
		SizeBytes:      int64(imageInfo.Size * sizeConversion),
		ReadyToUse:     true,
	}
}
