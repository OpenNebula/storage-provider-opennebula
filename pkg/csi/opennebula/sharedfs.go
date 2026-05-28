package opennebula

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

const (
	datastoreTypeCephFS = "cephfs"

	sharedBackendAttr            = "SPARKAI_CSI_SHARE_BACKEND"
	sharedBackendCephFS          = "cephfs"
	cephFSAttrFSName             = "SPARKAI_CSI_CEPHFS_FS_NAME"
	cephFSAttrRootPath           = "SPARKAI_CSI_CEPHFS_ROOT_PATH"
	cephFSAttrSubvolumeGroup     = "SPARKAI_CSI_CEPHFS_SUBVOLUME_GROUP"
	cephFSAttrMountOptions       = "SPARKAI_CSI_CEPHFS_MOUNT_OPTIONS"
	sharedFilesystemPathParam    = "sharedFilesystemPath"
	sharedFilesystemGroupParam   = "sharedFilesystemSubvolumeGroup"
	sharedVolumeIDPrefix         = "cephfs:"
	publishContextShareBackend   = "shareBackend"
	publishContextCephFSMonitors = "cephfsMonitors"
	publishContextCephFSFSName   = "cephfsFSName"
	publishContextCephFSSubpath  = "cephfsSubpath"
	publishContextCephFSReadonly = "cephfsReadonly"
	publishContextCephFSMounts   = "cephfsMountOptions"
)

type VolumeAccessModel string

const (
	VolumeAccessModelDisk     VolumeAccessModel = "disk"
	VolumeAccessModelSharedFS VolumeAccessModel = "shared-fs"
)

type SharedVolumeMode string

const (
	SharedVolumeModeStatic  SharedVolumeMode = "static"
	SharedVolumeModeDynamic SharedVolumeMode = "dynamic"
)

type CephFSDatastoreAttributes struct {
	FSName           string
	RootPath         string
	SubvolumeGroup   string
	Monitors         []string
	MountOptions     []string
	OptionalCephConf string
}

type SharedVolumeMetadata struct {
	DatastoreID    int              `json:"datastoreID"`
	Mode           SharedVolumeMode `json:"mode"`
	FSName         string           `json:"fsName"`
	SubvolumeGroup string           `json:"subvolumeGroup,omitempty"`
	Subpath        string           `json:"subpath"`
	Backend        string           `json:"backend"`
	SubvolumeName  string           `json:"subvolumeName,omitempty"`
}

type SharedVolumeRequest struct {
	Name       string
	SizeBytes  int64
	Selection  DatastoreSelectionConfig
	Parameters map[string]string
	Secrets    map[string]string
}

type SharedVolumeCloneRequest struct {
	Name             string
	SizeBytes        int64
	Selection        DatastoreSelectionConfig
	Parameters       map[string]string
	Secrets          map[string]string
	SourceVolumeID   string
	SourceSnapshotID string
}

type SharedVolumeCreateResult struct {
	VolumeID              string
	CapacityBytes         int64
	Datastore             Datastore
	VolumeContext         map[string]string
	Metadata              SharedVolumeMetadata
	FallbackUsed          bool
	AttemptedDatastoreIDs []int
}

func IsSharedFilesystemVolumeID(volumeID string) bool {
	return strings.HasPrefix(strings.TrimSpace(volumeID), sharedVolumeIDPrefix)
}

func EncodeSharedVolumeID(metadata SharedVolumeMetadata) (string, error) {
	payload, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("failed to encode shared volume metadata: %w", err)
	}

	return sharedVolumeIDPrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func DecodeSharedVolumeID(volumeID string) (SharedVolumeMetadata, error) {
	if !IsSharedFilesystemVolumeID(volumeID) {
		return SharedVolumeMetadata{}, &datastoreConfigError{message: fmt.Sprintf("volume ID %q is not a shared filesystem volume", volumeID)}
	}

	encoded := strings.TrimPrefix(strings.TrimSpace(volumeID), sharedVolumeIDPrefix)
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return SharedVolumeMetadata{}, &datastoreConfigError{message: fmt.Sprintf("failed to decode shared volume ID %q: %v", volumeID, err)}
	}

	var metadata SharedVolumeMetadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return SharedVolumeMetadata{}, &datastoreConfigError{message: fmt.Sprintf("failed to parse shared volume metadata from %q: %v", volumeID, err)}
	}
	if metadata.Backend == "" {
		metadata.Backend = sharedBackendCephFS
	}
	if metadata.DatastoreID <= 0 || metadata.FSName == "" || metadata.Subpath == "" {
		return SharedVolumeMetadata{}, &datastoreConfigError{message: fmt.Sprintf("shared volume ID %q is missing required metadata", volumeID)}
	}

	return metadata, nil
}

func sharedFilesystemPath(params map[string]string) string {
	return strings.TrimSpace(params[sharedFilesystemPathParam])
}

func sharedFilesystemSubvolumeGroup(params map[string]string) string {
	return strings.TrimSpace(params[sharedFilesystemGroupParam])
}

func normalizeSharedSubpath(rootPath, requestedPath string) (string, error) {
	rootPath = cleanSharedPath(rootPath)
	requestedPath = cleanSharedPath(requestedPath)
	if requestedPath == "" {
		return "", &datastoreConfigError{message: "sharedFilesystemPath cannot be empty"}
	}

	if strings.HasPrefix(requestedPath, "/") {
		if rootPath == "" || rootPath == "/" {
			return requestedPath, nil
		}
		if requestedPath == rootPath || strings.HasPrefix(requestedPath, rootPath+"/") {
			return requestedPath, nil
		}

		return "", &datastoreConfigError{message: fmt.Sprintf("sharedFilesystemPath %q must stay under datastore root path %q", requestedPath, rootPath)}
	}

	if rootPath == "" {
		rootPath = "/"
	}
	return cleanSharedPath(path.Join(rootPath, requestedPath)), nil
}

func cleanSharedPath(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	cleaned := path.Clean(trimmed)
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + strings.TrimPrefix(cleaned, "/")
	}
	return cleaned
}

var invalidSubvolumeChars = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeSubvolumeName(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = invalidSubvolumeChars.ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-")
	if normalized == "" {
		return "one-csi-volume"
	}

	if !strings.HasPrefix(normalized, "one-csi-") {
		normalized = "one-csi-" + normalized
	}

	if len(normalized) > 96 {
		normalized = normalized[:96]
		normalized = strings.Trim(normalized, "-")
	}

	return normalized
}

func sharedFilesystemBackend(volumeContext map[string]string) string {
	return strings.TrimSpace(volumeContext[publishContextShareBackend])
}

func isSharedFilesystemPublishContext(volumeContext map[string]string) bool {
	return sharedFilesystemBackend(volumeContext) != ""
}
