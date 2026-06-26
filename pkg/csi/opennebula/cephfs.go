package opennebula

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/OpenNebula/one/src/oca/go/src/goca"
	"k8s.io/klog/v2"
	utilexec "k8s.io/utils/exec"
)

const (
	cephProvisionerSecretAdminIDKey  = "adminID"
	cephProvisionerSecretAdminKeyKey = "adminKey"
	cephFsCommandBin                 = "ceph"
	cephFSCloneTimeout               = 5 * time.Minute
	cephFSClonePollInterval          = 2 * time.Second
)

type CephFSVolumeProvider struct {
	ctrl *goca.Controller
	exec utilexec.Interface
}

func NewCephFSVolumeProvider(client *OpenNebulaClient, exec utilexec.Interface) (*CephFSVolumeProvider, error) {
	if client == nil {
		return nil, fmt.Errorf("client reference is nil")
	}
	if exec == nil {
		return nil, fmt.Errorf("exec reference is nil")
	}

	return &CephFSVolumeProvider{
		ctrl: goca.NewController(client.Client),
		exec: exec,
	}, nil
}

func (p *CephFSVolumeProvider) CreateSharedVolume(ctx context.Context, req SharedVolumeRequest) (*SharedVolumeCreateResult, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, fmt.Errorf("shared volume name cannot be empty")
	}

	candidates, err := p.resolveProvisioningDatastores(ctx, req.Selection)
	if err != nil {
		return nil, err
	}

	staticPath := sharedFilesystemPath(req.Parameters)
	overrideGroup := sharedFilesystemSubvolumeGroup(req.Parameters)
	var insufficientCapacity []string
	attemptedDatastores := make([]int, 0, len(candidates))

	for _, datastore := range candidates {
		attemptedDatastores = append(attemptedDatastores, datastore.ID)
		if datastore.CephFS == nil {
			return nil, &datastoreConfigError{message: fmt.Sprintf("datastore %d does not expose CephFS metadata", datastore.ID)}
		}

		metadata := SharedVolumeMetadata{
			DatastoreID:    datastore.ID,
			FSName:         datastore.CephFS.FSName,
			SubvolumeGroup: datastore.CephFS.SubvolumeGroup,
			Backend:        sharedBackendCephFS,
		}
		if overrideGroup != "" {
			metadata.SubvolumeGroup = overrideGroup
		}

		if staticPath != "" {
			metadata.Mode = SharedVolumeModeStatic
			metadata.Subpath, err = normalizeSharedSubpath(datastore.CephFS.RootPath, staticPath)
			if err != nil {
				return nil, err
			}

			volumeID, err := EncodeSharedVolumeID(metadata)
			if err != nil {
				return nil, err
			}

			return &SharedVolumeCreateResult{
				VolumeID:              volumeID,
				CapacityBytes:         req.SizeBytes,
				Datastore:             datastore,
				Metadata:              metadata,
				FallbackUsed:          len(attemptedDatastores) > 1,
				AttemptedDatastoreIDs: append([]int(nil), attemptedDatastores...),
			}, nil
		}

		if req.SizeBytes > 0 && datastore.FreeBytes > 0 && datastore.FreeBytes < req.SizeBytes {
			recordDatastoreProvisioningResult(datastore.ID, false)
			insufficientCapacity = append(insufficientCapacity, strconv.Itoa(datastore.ID))
			continue
		}

		if metadata.SubvolumeGroup == "" {
			return nil, &datastoreConfigError{message: fmt.Sprintf("datastore %d is missing a CephFS subvolume group", datastore.ID)}
		}

		metadata.Mode = SharedVolumeModeDynamic
		metadata.SubvolumeName = sanitizeSubvolumeName(req.Name)

		finishAttempt := beginDatastoreAttempt(datastore.ID)
		if err := p.ensureSubvolume(ctx, datastore, metadata, req.SizeBytes, req.Secrets); err != nil {
			finishAttempt()
			recordDatastoreProvisioningResult(datastore.ID, false)
			if IsDatastoreCapacityError(err) {
				insufficientCapacity = append(insufficientCapacity, strconv.Itoa(datastore.ID))
				continue
			}
			return nil, err
		}
		finishAttempt()
		recordDatastoreProvisioningResult(datastore.ID, true)

		subpath, err := p.getSubvolumePath(ctx, datastore, metadata, req.Secrets)
		if err != nil {
			return nil, err
		}
		metadata.Subpath = cleanSharedPath(subpath)

		volumeID, err := EncodeSharedVolumeID(metadata)
		if err != nil {
			return nil, err
		}

		return &SharedVolumeCreateResult{
			VolumeID:              volumeID,
			CapacityBytes:         req.SizeBytes,
			Datastore:             datastore,
			Metadata:              metadata,
			FallbackUsed:          len(attemptedDatastores) > 1,
			AttemptedDatastoreIDs: append([]int(nil), attemptedDatastores...),
		}, nil
	}

	if len(insufficientCapacity) > 0 {
		return nil, &datastoreCapacityError{
			message: fmt.Sprintf("none of the configured CephFS datastores had enough free capacity for %d bytes; attempted datastores: %s", req.SizeBytes, strings.Join(insufficientCapacity, ",")),
		}
	}

	return nil, &datastoreConfigError{message: "no eligible CephFS datastores were available for shared filesystem provisioning"}
}

func (p *CephFSVolumeProvider) DeleteSharedVolume(ctx context.Context, volumeID string, secrets map[string]string) error {
	metadata, datastore, err := p.validateSharedVolume(ctx, volumeID)
	if err != nil {
		return err
	}

	if metadata.Mode == SharedVolumeModeStatic {
		return nil
	}
	if metadata.SubvolumeName == "" {
		return &datastoreConfigError{message: fmt.Sprintf("shared volume %q is missing subvolume metadata", volumeID)}
	}

	args := []string{"fs", "subvolume", "rm", metadata.FSName, metadata.SubvolumeName}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}

	output, err := p.runCephCommand(ctx, datastore, secrets, args...)
	if err != nil {
		if isCephNotFoundError(err, output) {
			return nil
		}
		return err
	}

	return nil
}

func (p *CephFSVolumeProvider) CloneSharedVolume(ctx context.Context, req SharedVolumeCloneRequest) (*SharedVolumeCreateResult, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, fmt.Errorf("shared volume name cannot be empty")
	}
	if strings.TrimSpace(req.SourceVolumeID) == "" && strings.TrimSpace(req.SourceSnapshotID) == "" {
		return nil, &datastoreConfigError{message: "shared filesystem clone requires a source volume or source snapshot"}
	}
	if strings.TrimSpace(req.SourceVolumeID) != "" && strings.TrimSpace(req.SourceSnapshotID) != "" {
		return nil, &datastoreConfigError{message: "shared filesystem clone requires exactly one source reference"}
	}

	var (
		sourceMetadata        SharedVolumeMetadata
		sourceDatastore       Datastore
		sourceSnapshot        SharedVolumeSnapshotMetadata
		err                   error
		temporarySnapshot     bool
		temporarySnapshotName string
	)

	if strings.TrimSpace(req.SourceSnapshotID) != "" {
		sourceSnapshot, sourceDatastore, err = p.validateSharedSnapshot(ctx, req.SourceSnapshotID)
		if err != nil {
			return nil, err
		}
		sourceMetadata = SharedVolumeMetadata{
			DatastoreID:    sourceSnapshot.DatastoreID,
			FSName:         sourceSnapshot.FSName,
			SubvolumeGroup: sourceSnapshot.SubvolumeGroup,
			SubvolumeName:  sourceSnapshot.SubvolumeName,
			Backend:        sourceSnapshot.Backend,
			Mode:           SharedVolumeModeDynamic,
		}
	} else {
		sourceMetadata, sourceDatastore, err = p.validateSharedVolume(ctx, req.SourceVolumeID)
		if err != nil {
			return nil, err
		}
		if sourceMetadata.Mode != SharedVolumeModeDynamic {
			return nil, &datastoreConfigError{message: fmt.Sprintf("shared volume %q is static and cannot be used as a CephFS clone source", req.SourceVolumeID)}
		}

		temporarySnapshot = true
		temporarySnapshotName = sanitizeCephFSSnapshotName(req.Name + "-seed")
		if err := p.createSubvolumeSnapshot(ctx, sourceDatastore, sourceMetadata, temporarySnapshotName, req.Secrets); err != nil {
			return nil, err
		}

		sourceSnapshot = SharedVolumeSnapshotMetadata{
			DatastoreID:    sourceMetadata.DatastoreID,
			Backend:        sharedSnapshotBackendCephFS,
			FSName:         sourceMetadata.FSName,
			SubvolumeGroup: sourceMetadata.SubvolumeGroup,
			SubvolumeName:  sourceMetadata.SubvolumeName,
			SnapshotName:   temporarySnapshotName,
			SourceVolumeID: req.SourceVolumeID,
		}
	}

	candidates, err := p.resolveProvisioningDatastores(ctx, req.Selection)
	if err != nil {
		if temporarySnapshot {
			p.cleanupTemporarySnapshot(ctx, sourceDatastore, sourceMetadata, temporarySnapshotName, req.Secrets)
		}
		return nil, err
	}

	overrideGroup := sharedFilesystemSubvolumeGroup(req.Parameters)
	attemptedDatastores := make([]int, 0, len(candidates))
	insufficientCapacity := make([]string, 0)

	for _, datastore := range candidates {
		attemptedDatastores = append(attemptedDatastores, datastore.ID)
		if err := validateCephFSCloneTarget(sourceDatastore, datastore); err != nil {
			continue
		}
		if req.SizeBytes > 0 && datastore.FreeBytes > 0 && datastore.FreeBytes < req.SizeBytes {
			recordDatastoreProvisioningResult(datastore.ID, false)
			insufficientCapacity = append(insufficientCapacity, strconv.Itoa(datastore.ID))
			continue
		}

		metadata := SharedVolumeMetadata{
			DatastoreID:    datastore.ID,
			Mode:           SharedVolumeModeDynamic,
			FSName:         datastore.CephFS.FSName,
			SubvolumeGroup: datastore.CephFS.SubvolumeGroup,
			Backend:        sharedBackendCephFS,
			SubvolumeName:  sanitizeSubvolumeName(req.Name),
		}
		if overrideGroup != "" {
			metadata.SubvolumeGroup = overrideGroup
		}
		if metadata.SubvolumeGroup == "" {
			return nil, &datastoreConfigError{message: fmt.Sprintf("datastore %d is missing a CephFS subvolume group", datastore.ID)}
		}

		finishAttempt := beginDatastoreAttempt(datastore.ID)
		err := p.cloneFromSnapshot(ctx, datastore, sourceSnapshot, metadata, req.Secrets)
		finishAttempt()
		if err != nil {
			recordDatastoreProvisioningResult(datastore.ID, false)
			if IsDatastoreCapacityError(err) {
				insufficientCapacity = append(insufficientCapacity, strconv.Itoa(datastore.ID))
				continue
			}
			if temporarySnapshot {
				p.cleanupTemporarySnapshot(ctx, sourceDatastore, sourceMetadata, temporarySnapshotName, req.Secrets)
			}
			return nil, err
		}
		recordDatastoreProvisioningResult(datastore.ID, true)

		if req.SizeBytes > 0 {
			if _, err := p.expandDynamicSubvolume(ctx, datastore, metadata, req.SizeBytes, req.Secrets); err != nil {
				if temporarySnapshot {
					p.cleanupTemporarySnapshot(ctx, sourceDatastore, sourceMetadata, temporarySnapshotName, req.Secrets)
				}
				return nil, err
			}
		}

		subpath, err := p.getSubvolumePath(ctx, datastore, metadata, req.Secrets)
		if err != nil {
			if temporarySnapshot {
				p.cleanupTemporarySnapshot(ctx, sourceDatastore, sourceMetadata, temporarySnapshotName, req.Secrets)
			}
			return nil, err
		}
		metadata.Subpath = cleanSharedPath(subpath)

		volumeID, err := EncodeSharedVolumeID(metadata)
		if err != nil {
			if temporarySnapshot {
				p.cleanupTemporarySnapshot(ctx, sourceDatastore, sourceMetadata, temporarySnapshotName, req.Secrets)
			}
			return nil, err
		}

		if temporarySnapshot {
			p.cleanupTemporarySnapshot(ctx, sourceDatastore, sourceMetadata, temporarySnapshotName, req.Secrets)
		}

		info, infoErr := p.getSubvolumeInfo(ctx, datastore, metadata, req.Secrets)
		if infoErr != nil {
			info = cephFSSubvolumeInfo{BytesQuota: req.SizeBytes}
		}
		capacityBytes := req.SizeBytes
		if info.BytesQuota > 0 {
			capacityBytes = info.BytesQuota
		}

		return &SharedVolumeCreateResult{
			VolumeID:              volumeID,
			CapacityBytes:         capacityBytes,
			Datastore:             datastore,
			Metadata:              metadata,
			FallbackUsed:          len(attemptedDatastores) > 1,
			AttemptedDatastoreIDs: append([]int(nil), attemptedDatastores...),
		}, nil
	}

	if temporarySnapshot {
		p.cleanupTemporarySnapshot(ctx, sourceDatastore, sourceMetadata, temporarySnapshotName, req.Secrets)
	}

	if len(insufficientCapacity) > 0 {
		return nil, &datastoreCapacityError{
			message: fmt.Sprintf("none of the configured CephFS datastores had enough free capacity for clone target %q; attempted datastores: %s", req.Name, strings.Join(insufficientCapacity, ",")),
		}
	}

	return nil, &datastoreConfigError{message: "no eligible CephFS datastores were available for shared filesystem clone provisioning"}
}

func (p *CephFSVolumeProvider) PublishSharedVolume(ctx context.Context, volumeID string, readonly bool) (map[string]string, error) {
	metadata, datastore, err := p.validateSharedVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}

	publishContext := map[string]string{
		publishContextShareBackend:   sharedBackendCephFS,
		publishContextCephFSMonitors: strings.Join(datastore.CephFS.Monitors, ","),
		publishContextCephFSFSName:   metadata.FSName,
		publishContextCephFSSubpath:  metadata.Subpath,
		publishContextCephFSReadonly: strconv.FormatBool(readonly),
	}
	if len(datastore.CephFS.MountOptions) > 0 {
		publishContext[publishContextCephFSMounts] = strings.Join(datastore.CephFS.MountOptions, ",")
	}

	return publishContext, nil
}

func (p *CephFSVolumeProvider) ExpandSharedVolume(ctx context.Context, volumeID string, sizeBytes int64, secrets map[string]string) (int64, error) {
	if sizeBytes <= 0 {
		return 0, &datastoreConfigError{message: "requested CephFS volume size must be greater than zero"}
	}

	metadata, datastore, err := p.validateSharedVolume(ctx, volumeID)
	if err != nil {
		return 0, err
	}
	if metadata.Mode != SharedVolumeModeDynamic {
		return 0, &datastoreConfigError{message: fmt.Sprintf("shared volume %q is static and cannot be expanded", volumeID)}
	}

	return p.expandDynamicSubvolume(ctx, datastore, metadata, sizeBytes, secrets)
}

func (p *CephFSVolumeProvider) CreateSharedSnapshot(ctx context.Context, sourceVolumeID string, snapshotName string, secrets map[string]string) (*VolumeSnapshot, error) {
	metadata, datastore, err := p.validateSharedVolume(ctx, sourceVolumeID)
	if err != nil {
		return nil, err
	}
	if metadata.Mode != SharedVolumeModeDynamic {
		return nil, &datastoreConfigError{message: fmt.Sprintf("shared volume %q is static and cannot be snapshotted", sourceVolumeID)}
	}

	snapshotName = sanitizeCephFSSnapshotName(snapshotName)
	if err := p.createSubvolumeSnapshot(ctx, datastore, metadata, snapshotName, secrets); err != nil {
		return nil, err
	}

	snapshotMetadata := SharedVolumeSnapshotMetadata{
		DatastoreID:    metadata.DatastoreID,
		Backend:        sharedSnapshotBackendCephFS,
		FSName:         metadata.FSName,
		SubvolumeGroup: metadata.SubvolumeGroup,
		SubvolumeName:  metadata.SubvolumeName,
		SnapshotName:   snapshotName,
		SourceVolumeID: sourceVolumeID,
	}

	info, err := p.getSnapshotInfo(ctx, datastore, snapshotMetadata, secrets)
	if err != nil {
		return nil, err
	}

	subvolumeInfo, subvolumeErr := p.getSubvolumeInfo(ctx, datastore, metadata, secrets)
	if subvolumeErr != nil {
		subvolumeInfo = cephFSSubvolumeInfo{}
	}

	size := subvolumeInfo.BytesQuota
	if size == 0 {
		size = subvolumeInfo.BytesUsed
	}
	snapshotMetadata.CreationUnix = info.CreatedAt.UTC().Unix()
	snapshotMetadata.SizeBytes = size
	snapshotID, err := EncodeSharedVolumeSnapshotID(snapshotMetadata)
	if err != nil {
		return nil, err
	}

	return &VolumeSnapshot{
		SnapshotID:     snapshotID,
		SourceVolumeID: sourceVolumeID,
		CreationTime:   info.CreatedAt,
		SizeBytes:      size,
		ReadyToUse:     true,
	}, nil
}

func (p *CephFSVolumeProvider) DeleteSharedSnapshot(ctx context.Context, snapshotID string, secrets map[string]string) error {
	metadata, datastore, err := p.validateSharedSnapshot(ctx, snapshotID)
	if err != nil {
		return err
	}

	args := []string{"fs", "subvolume", "snapshot", "rm", metadata.FSName, metadata.SubvolumeName, metadata.SnapshotName}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}
	args = append(args, "--force")

	output, err := p.runCephCommand(ctx, datastore, secrets, args...)
	if err != nil {
		if isCephNotFoundError(err, output) {
			return nil
		}
		return err
	}

	return nil
}

func (p *CephFSVolumeProvider) ListSharedSnapshots(ctx context.Context, snapshotID string, sourceVolumeID string, maxEntries int32, startingToken string, secrets map[string]string) ([]VolumeSnapshot, string, error) {
	var snapshots []VolumeSnapshot

	switch {
	case strings.TrimSpace(snapshotID) != "":
		metadata, datastore, err := p.validateSharedSnapshot(ctx, snapshotID)
		if err != nil {
			return nil, "", err
		}
		size := metadata.SizeBytes
		createdAt := time.Unix(metadata.CreationUnix, 0).UTC()
		if metadata.CreationUnix == 0 && len(secrets) > 0 {
			info, err := p.getSnapshotInfo(ctx, datastore, metadata, secrets)
			if err != nil {
				return nil, "", err
			}
			createdAt = info.CreatedAt
		}
		if size == 0 && len(secrets) > 0 {
			subvolumeInfo, _ := p.getSubvolumeInfo(ctx, datastore, SharedVolumeMetadata{
				DatastoreID:    metadata.DatastoreID,
				Mode:           SharedVolumeModeDynamic,
				FSName:         metadata.FSName,
				SubvolumeGroup: metadata.SubvolumeGroup,
				SubvolumeName:  metadata.SubvolumeName,
				Backend:        metadata.Backend,
			}, secrets)
			size = subvolumeInfo.BytesQuota
			if size == 0 {
				size = subvolumeInfo.BytesUsed
			}
		}
		snapshots = append(snapshots, VolumeSnapshot{
			SnapshotID:     snapshotID,
			SourceVolumeID: metadata.SourceVolumeID,
			CreationTime:   createdAt,
			SizeBytes:      size,
			ReadyToUse:     true,
		})
	case strings.TrimSpace(sourceVolumeID) != "":
		volumeMetadata, datastore, err := p.validateSharedVolume(ctx, sourceVolumeID)
		if err != nil {
			return nil, "", err
		}
		if volumeMetadata.Mode != SharedVolumeModeDynamic {
			return nil, "", &datastoreConfigError{message: fmt.Sprintf("shared volume %q is static and does not support snapshots", sourceVolumeID)}
		}

		names, err := p.listSnapshotNames(ctx, datastore, volumeMetadata, secrets)
		if err != nil {
			return nil, "", err
		}
		subvolumeInfo, _ := p.getSubvolumeInfo(ctx, datastore, volumeMetadata, secrets)
		size := subvolumeInfo.BytesQuota
		if size == 0 {
			size = subvolumeInfo.BytesUsed
		}
		for _, name := range names {
			snapshotMetadata := SharedVolumeSnapshotMetadata{
				DatastoreID:    volumeMetadata.DatastoreID,
				Backend:        sharedSnapshotBackendCephFS,
				FSName:         volumeMetadata.FSName,
				SubvolumeGroup: volumeMetadata.SubvolumeGroup,
				SubvolumeName:  volumeMetadata.SubvolumeName,
				SnapshotName:   name,
				SourceVolumeID: sourceVolumeID,
			}
			info, err := p.getSnapshotInfo(ctx, datastore, snapshotMetadata, secrets)
			if err != nil {
				return nil, "", err
			}
			encodedSnapshotID, err := EncodeSharedVolumeSnapshotID(snapshotMetadata)
			if err != nil {
				return nil, "", err
			}
			snapshots = append(snapshots, VolumeSnapshot{
				SnapshotID:     encodedSnapshotID,
				SourceVolumeID: sourceVolumeID,
				CreationTime:   info.CreatedAt,
				SizeBytes:      size,
				ReadyToUse:     true,
			})
		}
	default:
		return nil, "", &datastoreConfigError{message: "listing all CephFS snapshots without a source volume is not supported"}
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

func (p *CephFSVolumeProvider) ValidateSharedVolume(ctx context.Context, volumeID string) (*SharedVolumeMetadata, error) {
	metadata, _, err := p.validateSharedVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}

	return &metadata, nil
}

func (p *CephFSVolumeProvider) validateSharedVolume(ctx context.Context, volumeID string) (SharedVolumeMetadata, Datastore, error) {
	metadata, err := DecodeSharedVolumeID(volumeID)
	if err != nil {
		return SharedVolumeMetadata{}, Datastore{}, err
	}

	datastores, err := p.ctrl.Datastores().InfoContext(ctx)
	if err != nil {
		return SharedVolumeMetadata{}, Datastore{}, fmt.Errorf("failed to inspect datastores for shared volume %q: %w", volumeID, err)
	}

	sourceDatastore, err := findDatastoreByID(datastores.Datastores, metadata.DatastoreID)
	if err != nil {
		return SharedVolumeMetadata{}, Datastore{}, err
	}

	datastore := datastoreFromSchema(sourceDatastore)
	if datastore.CephFS == nil {
		return SharedVolumeMetadata{}, Datastore{}, &datastoreConfigError{message: fmt.Sprintf("datastore %d no longer validates as a CephFS datastore", metadata.DatastoreID)}
	}

	return metadata, datastore, nil
}

func (p *CephFSVolumeProvider) validateSharedSnapshot(ctx context.Context, snapshotID string) (SharedVolumeSnapshotMetadata, Datastore, error) {
	metadata, err := DecodeSharedVolumeSnapshotID(snapshotID)
	if err != nil {
		return SharedVolumeSnapshotMetadata{}, Datastore{}, err
	}

	datastores, err := p.ctrl.Datastores().InfoContext(ctx)
	if err != nil {
		return SharedVolumeSnapshotMetadata{}, Datastore{}, fmt.Errorf("failed to inspect datastores for shared snapshot %q: %w", snapshotID, err)
	}

	sourceDatastore, err := findDatastoreByID(datastores.Datastores, metadata.DatastoreID)
	if err != nil {
		return SharedVolumeSnapshotMetadata{}, Datastore{}, err
	}

	datastore := datastoreFromSchema(sourceDatastore)
	if datastore.CephFS == nil {
		return SharedVolumeSnapshotMetadata{}, Datastore{}, &datastoreConfigError{message: fmt.Sprintf("datastore %d no longer validates as a CephFS datastore", metadata.DatastoreID)}
	}

	return metadata, datastore, nil
}

func (p *CephFSVolumeProvider) resolveProvisioningDatastores(ctx context.Context, selection DatastoreSelectionConfig) ([]Datastore, error) {
	datastores, err := p.ctrl.Datastores().InfoContext(ctx)
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

func (p *CephFSVolumeProvider) expandDynamicSubvolume(ctx context.Context, datastore Datastore, metadata SharedVolumeMetadata, sizeBytes int64, secrets map[string]string) (int64, error) {
	args := []string{"fs", "subvolume", "resize", metadata.FSName, metadata.SubvolumeName, strconv.FormatInt(sizeBytes, 10)}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}
	args = append(args, "--no_shrink")

	if _, err := p.runCephCommand(ctx, datastore, secrets, args...); err != nil {
		return 0, err
	}

	info, err := p.getSubvolumeInfo(ctx, datastore, metadata, secrets)
	if err != nil {
		return sizeBytes, nil
	}
	if info.BytesQuota > 0 {
		return info.BytesQuota, nil
	}

	return sizeBytes, nil
}

func (p *CephFSVolumeProvider) ensureSubvolume(ctx context.Context, datastore Datastore, metadata SharedVolumeMetadata, sizeBytes int64, secrets map[string]string) error {
	args := []string{"fs", "subvolume", "create", metadata.FSName, metadata.SubvolumeName}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}
	if sizeBytes > 0 {
		args = append(args, "--size", strconv.FormatInt(sizeBytes, 10))
	}

	output, err := p.runCephCommand(ctx, datastore, secrets, args...)
	if err != nil {
		if isCephAlreadyExistsError(err, output) {
			return nil
		}
		return err
	}

	return nil
}

func (p *CephFSVolumeProvider) createSubvolumeSnapshot(ctx context.Context, datastore Datastore, metadata SharedVolumeMetadata, snapshotName string, secrets map[string]string) error {
	args := []string{"fs", "subvolume", "snapshot", "create", metadata.FSName, metadata.SubvolumeName, snapshotName}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}

	output, err := p.runCephCommand(ctx, datastore, secrets, args...)
	if err != nil {
		if isCephAlreadyExistsError(err, output) {
			return nil
		}
		return err
	}

	return nil
}

func (p *CephFSVolumeProvider) cloneFromSnapshot(ctx context.Context, datastore Datastore, source SharedVolumeSnapshotMetadata, target SharedVolumeMetadata, secrets map[string]string) error {
	args := []string{"fs", "subvolume", "snapshot", "clone", source.FSName, source.SubvolumeName, source.SnapshotName, target.SubvolumeName}
	if source.SubvolumeGroup != "" {
		args = append(args, "--group_name", source.SubvolumeGroup)
	}
	if target.SubvolumeGroup != "" {
		args = append(args, "--target_group_name", target.SubvolumeGroup)
	}

	output, err := p.runCephCommand(ctx, datastore, secrets, args...)
	if err != nil {
		if isCephAlreadyExistsError(err, output) {
			return nil
		}
		return err
	}

	return p.waitForCloneCompletion(ctx, datastore, target, secrets)
}

func (p *CephFSVolumeProvider) getSubvolumePath(ctx context.Context, datastore Datastore, metadata SharedVolumeMetadata, secrets map[string]string) (string, error) {
	args := []string{"fs", "subvolume", "getpath", metadata.FSName, metadata.SubvolumeName}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}

	output, err := p.runCephCommand(ctx, datastore, secrets, args...)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(output)), nil
}

func (p *CephFSVolumeProvider) getSubvolumeInfo(ctx context.Context, datastore Datastore, metadata SharedVolumeMetadata, secrets map[string]string) (cephFSSubvolumeInfo, error) {
	args := []string{"--format", "json", "fs", "subvolume", "info", metadata.FSName, metadata.SubvolumeName}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}

	output, err := p.runCephCommand(ctx, datastore, secrets, args...)
	if err != nil {
		return cephFSSubvolumeInfo{}, err
	}

	var info cephFSSubvolumeInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return cephFSSubvolumeInfo{}, fmt.Errorf("failed to parse CephFS subvolume info: %w", err)
	}
	return info, nil
}

func (p *CephFSVolumeProvider) getSnapshotInfo(ctx context.Context, datastore Datastore, metadata SharedVolumeSnapshotMetadata, secrets map[string]string) (cephFSSnapshotInfo, error) {
	args := []string{"--format", "json", "fs", "subvolume", "snapshot", "info", metadata.FSName, metadata.SubvolumeName, metadata.SnapshotName}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}

	output, err := p.runCephCommand(ctx, datastore, secrets, args...)
	if err != nil {
		return cephFSSnapshotInfo{}, err
	}

	var raw struct {
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(output, &raw); err != nil {
		return cephFSSnapshotInfo{}, fmt.Errorf("failed to parse CephFS snapshot info: %w", err)
	}

	return cephFSSnapshotInfo{CreatedAt: parseCephTimestamp(raw.CreatedAt)}, nil
}

func (p *CephFSVolumeProvider) listSnapshotNames(ctx context.Context, datastore Datastore, metadata SharedVolumeMetadata, secrets map[string]string) ([]string, error) {
	args := []string{"--format", "json", "fs", "subvolume", "snapshot", "ls", metadata.FSName, metadata.SubvolumeName}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}

	output, err := p.runCephCommand(ctx, datastore, secrets, args...)
	if err != nil {
		return nil, err
	}

	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(output, &entries); err != nil {
		return nil, fmt.Errorf("failed to parse CephFS snapshot list: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.Name) == "" {
			continue
		}
		names = append(names, entry.Name)
	}

	return names, nil
}

func (p *CephFSVolumeProvider) waitForCloneCompletion(ctx context.Context, datastore Datastore, metadata SharedVolumeMetadata, secrets map[string]string) error {
	deadline := time.Now().Add(cephFSCloneTimeout)
	for {
		status, err := p.getCloneStatus(ctx, datastore, metadata, secrets)
		if err != nil {
			return err
		}

		switch status.State {
		case "complete":
			return nil
		case "pending", "in-progress":
		case "failed", "canceled", "cancelled":
			return &datastoreConfigError{message: fmt.Sprintf("CephFS clone for subvolume %s failed: %s", metadata.SubvolumeName, status.Error())}
		default:
			return &datastoreConfigError{message: fmt.Sprintf("CephFS clone for subvolume %s entered unexpected state %q", metadata.SubvolumeName, status.State)}
		}

		if time.Now().After(deadline) {
			return &datastoreConfigError{message: fmt.Sprintf("timed out waiting for CephFS clone %s to become ready", metadata.SubvolumeName)}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cephFSClonePollInterval):
		}
	}
}

func (p *CephFSVolumeProvider) getCloneStatus(ctx context.Context, datastore Datastore, metadata SharedVolumeMetadata, secrets map[string]string) (cephFSCloneStatus, error) {
	args := []string{"--format", "json", "fs", "clone", "status", metadata.FSName, metadata.SubvolumeName}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}

	output, err := p.runCephCommand(ctx, datastore, secrets, args...)
	if err != nil {
		return cephFSCloneStatus{}, err
	}

	var raw struct {
		Status struct {
			State   string `json:"state"`
			Failure struct {
				ErrNo  string `json:"errno"`
				ErrStr string `json:"errstr"`
			} `json:"failure"`
		} `json:"status"`
	}
	if err := json.Unmarshal(output, &raw); err != nil {
		return cephFSCloneStatus{}, fmt.Errorf("failed to parse CephFS clone status: %w", err)
	}

	return cephFSCloneStatus{
		State:  raw.Status.State,
		ErrNo:  raw.Status.Failure.ErrNo,
		ErrStr: raw.Status.Failure.ErrStr,
	}, nil
}

func (p *CephFSVolumeProvider) runCephCommand(_ context.Context, datastore Datastore, secrets map[string]string, args ...string) ([]byte, error) {
	if datastore.CephFS == nil {
		return nil, &datastoreConfigError{message: fmt.Sprintf("datastore %d does not expose CephFS metadata", datastore.ID)}
	}

	adminID := strings.TrimSpace(secrets[cephProvisionerSecretAdminIDKey])
	adminKey := strings.TrimSpace(secrets[cephProvisionerSecretAdminKeyKey])
	if adminID == "" || adminKey == "" {
		return nil, &datastoreConfigError{message: fmt.Sprintf("CephFS provisioning requires secret keys %q and %q", cephProvisionerSecretAdminIDKey, cephProvisionerSecretAdminKeyKey)}
	}

	keyFile, err := os.CreateTemp("", "cephfs-admin-key-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary CephFS key file: %w", err)
	}
	keyFilePath := keyFile.Name()
	defer os.Remove(keyFilePath)
	if _, err := keyFile.WriteString(adminKey); err != nil {
		keyFile.Close()
		return nil, fmt.Errorf("failed to write temporary CephFS key file: %w", err)
	}
	keyFile.Close()
	if err := os.Chmod(keyFilePath, 0600); err != nil {
		return nil, fmt.Errorf("failed to secure temporary CephFS key file: %w", err)
	}

	cephConfPath := strings.TrimSpace(datastore.CephFS.OptionalCephConf)
	commandArgs := make([]string, 0, len(args)+8)
	if cephConfPath == "" {
		commandArgs = append(commandArgs, "-c", "/dev/null")
	} else {
		if _, err := os.Stat(cephConfPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				klog.V(2).InfoS("Configured Ceph config file is not present inside the controller container, falling back to an empty config", "datastoreID", datastore.ID, "path", cephConfPath)
				commandArgs = append(commandArgs, "-c", "/dev/null")
			} else {
				return nil, fmt.Errorf("failed to stat configured Ceph config file %q: %w", cephConfPath, err)
			}
		} else {
			commandArgs = append(commandArgs, "-c", cephConfPath)
		}
	}

	commandArgs = append(commandArgs, "-m", strings.Join(datastore.CephFS.Monitors, ","), "--id", adminID, "--keyfile", keyFilePath)
	commandArgs = append(commandArgs, args...)

	cmd := p.exec.Command(cephFsCommandBin, commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		trimmedOutput := strings.TrimSpace(string(output))
		if trimmedOutput != "" {
			return output, fmt.Errorf("ceph command failed: %w: %s", err, trimmedOutput)
		}
		return output, fmt.Errorf("ceph command failed: %w", err)
	}

	return output, nil
}

func (p *CephFSVolumeProvider) cleanupTemporarySnapshot(ctx context.Context, datastore Datastore, metadata SharedVolumeMetadata, snapshotName string, secrets map[string]string) {
	if snapshotName == "" {
		return
	}

	args := []string{"fs", "subvolume", "snapshot", "rm", metadata.FSName, metadata.SubvolumeName, snapshotName}
	if metadata.SubvolumeGroup != "" {
		args = append(args, "--group_name", metadata.SubvolumeGroup)
	}
	args = append(args, "--force")

	if _, err := p.runCephCommand(ctx, datastore, secrets, args...); err != nil {
		klog.V(2).InfoS("Failed to clean up temporary CephFS snapshot", "datastoreID", datastore.ID, "subvolume", metadata.SubvolumeName, "snapshotName", snapshotName, "err", err)
	}
}

func validateCephFSCloneTarget(source, target Datastore) error {
	if source.CephFS == nil || target.CephFS == nil {
		return &datastoreConfigError{message: "source or target datastore is missing CephFS metadata"}
	}
	if source.CephFS.FSName != target.CephFS.FSName {
		return &datastoreConfigError{message: fmt.Sprintf("CephFS clone requires matching filesystem names but source uses %q and target uses %q", source.CephFS.FSName, target.CephFS.FSName)}
	}
	if !sameStringSet(source.CephFS.Monitors, target.CephFS.Monitors) {
		return &datastoreConfigError{message: fmt.Sprintf("CephFS clone requires matching monitor sets between datastores %d and %d", source.ID, target.ID)}
	}

	return nil
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}

	seen := make(map[string]int, len(left))
	for _, item := range left {
		seen[item]++
	}
	for _, item := range right {
		seen[item]--
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}

func sanitizeCephFSSnapshotName(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = invalidSubvolumeChars.ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-")
	if normalized == "" {
		return "one-csi-snapshot"
	}
	if !strings.HasPrefix(normalized, "one-csi-snap-") {
		normalized = "one-csi-snap-" + normalized
	}
	if len(normalized) > 96 {
		normalized = strings.Trim(normalized[:96], "-")
	}
	return normalized
}

type cephFSSubvolumeInfo struct {
	BytesQuota int64  `json:"-"`
	BytesUsed  int64  `json:"bytes_used"`
	Path       string `json:"path"`
	CreatedRaw string `json:"created_at"`
}

func (i *cephFSSubvolumeInfo) UnmarshalJSON(data []byte) error {
	type rawInfo struct {
		BytesQuota any    `json:"bytes_quota"`
		BytesUsed  int64  `json:"bytes_used"`
		Path       string `json:"path"`
		CreatedRaw string `json:"created_at"`
	}
	var raw rawInfo
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	i.BytesQuota = coerceJSONInt64(raw.BytesQuota)
	i.BytesUsed = raw.BytesUsed
	i.Path = raw.Path
	i.CreatedRaw = raw.CreatedRaw
	return nil
}

type cephFSSnapshotInfo struct {
	CreatedAt time.Time
}

type cephFSCloneStatus struct {
	State  string
	ErrNo  string
	ErrStr string
}

func (s cephFSCloneStatus) Error() string {
	if strings.TrimSpace(s.ErrStr) != "" {
		if strings.TrimSpace(s.ErrNo) != "" {
			return fmt.Sprintf("%s (errno=%s)", s.ErrStr, s.ErrNo)
		}
		return s.ErrStr
	}
	if strings.TrimSpace(s.ErrNo) != "" {
		return fmt.Sprintf("errno=%s", s.ErrNo)
	}
	return s.State
}

func coerceJSONInt64(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case string:
		if strings.EqualFold(strings.TrimSpace(typed), "infinite") || strings.EqualFold(strings.TrimSpace(typed), "inf") {
			return 0
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

func parseCephTimestamp(value string) time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Now().UTC()
	}

	layouts := []string{
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		time.RFC3339,
		time.RFC3339Nano,
	}
	for _, layout := range layouts {
		parsed, err := time.ParseInLocation(layout, trimmed, time.UTC)
		if err == nil {
			return parsed.UTC()
		}
	}

	return time.Now().UTC()
}

func isCephAlreadyExistsError(err error, output []byte) bool {
	if err == nil {
		return false
	}

	message := strings.ToLower(err.Error() + " " + string(output))
	return strings.Contains(message, "already exists") || strings.Contains(message, "eexist")
}

func isCephNotFoundError(err error, output []byte) bool {
	if err == nil {
		return false
	}

	message := strings.ToLower(err.Error() + " " + string(output))
	return strings.Contains(message, "not found") || strings.Contains(message, "enoent")
}
