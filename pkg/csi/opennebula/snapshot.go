package opennebula

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	snapshotIDPrefix            = "image-snapshot:"
	sharedSnapshotIDPrefix      = "cephfs-snapshot:"
	sharedSnapshotBackendCephFS = "cephfs"
)

type VolumeSnapshot struct {
	SnapshotID     string
	SourceVolumeID string
	CreationTime   time.Time
	SizeBytes      int64
	ReadyToUse     bool
}

type SharedVolumeSnapshotMetadata struct {
	DatastoreID    int    `json:"datastoreID"`
	Backend        string `json:"backend"`
	FSName         string `json:"fsName"`
	SubvolumeGroup string `json:"subvolumeGroup,omitempty"`
	SubvolumeName  string `json:"subvolumeName"`
	SnapshotName   string `json:"snapshotName"`
	SourceVolumeID string `json:"sourceVolumeID"`
	CreationUnix   int64  `json:"creationUnix,omitempty"`
	SizeBytes      int64  `json:"sizeBytes,omitempty"`
}

func EncodeVolumeSnapshotID(imageID, snapshotID int) string {
	return fmt.Sprintf("%s%d:%d", snapshotIDPrefix, imageID, snapshotID)
}

func DecodeVolumeSnapshotID(value string) (int, int, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, snapshotIDPrefix) {
		return 0, 0, &datastoreConfigError{message: fmt.Sprintf("invalid OpenNebula snapshot ID %q", value)}
	}

	payload := strings.TrimPrefix(trimmed, snapshotIDPrefix)
	imageIDRaw, snapshotIDRaw, ok := strings.Cut(payload, ":")
	if !ok {
		return 0, 0, &datastoreConfigError{message: fmt.Sprintf("invalid OpenNebula snapshot ID %q", value)}
	}

	imageID, err := strconv.Atoi(imageIDRaw)
	if err != nil {
		return 0, 0, &datastoreConfigError{message: fmt.Sprintf("invalid image ID in snapshot reference %q", value)}
	}
	snapshotID, err := strconv.Atoi(snapshotIDRaw)
	if err != nil {
		return 0, 0, &datastoreConfigError{message: fmt.Sprintf("invalid snapshot ID in snapshot reference %q", value)}
	}

	return imageID, snapshotID, nil
}

func IsSharedFilesystemSnapshotID(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), sharedSnapshotIDPrefix)
}

func EncodeSharedVolumeSnapshotID(metadata SharedVolumeSnapshotMetadata) (string, error) {
	payload, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("failed to encode shared snapshot metadata: %w", err)
	}

	return sharedSnapshotIDPrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func DecodeSharedVolumeSnapshotID(value string) (SharedVolumeSnapshotMetadata, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, sharedSnapshotIDPrefix) {
		return SharedVolumeSnapshotMetadata{}, &datastoreConfigError{message: fmt.Sprintf("invalid shared filesystem snapshot ID %q", value)}
	}

	payload := strings.TrimPrefix(trimmed, sharedSnapshotIDPrefix)
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return SharedVolumeSnapshotMetadata{}, &datastoreConfigError{message: fmt.Sprintf("failed to decode shared filesystem snapshot ID %q: %v", value, err)}
	}

	var metadata SharedVolumeSnapshotMetadata
	if err := json.Unmarshal(decoded, &metadata); err != nil {
		return SharedVolumeSnapshotMetadata{}, &datastoreConfigError{message: fmt.Sprintf("failed to parse shared filesystem snapshot ID %q: %v", value, err)}
	}

	if metadata.Backend == "" {
		metadata.Backend = sharedSnapshotBackendCephFS
	}
	if metadata.DatastoreID <= 0 || metadata.FSName == "" || metadata.SubvolumeName == "" || metadata.SnapshotName == "" {
		return SharedVolumeSnapshotMetadata{}, &datastoreConfigError{message: fmt.Sprintf("shared filesystem snapshot ID %q is missing required metadata", value)}
	}

	return metadata, nil
}
