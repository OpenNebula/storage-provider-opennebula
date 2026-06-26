package opennebula

import (
	"fmt"
	"strconv"
	"strings"

	datastoreSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/datastore"
)

type DeploymentMode string

const (
	DeploymentModeUnknown DeploymentMode = "unknown"
	DeploymentModeCeph    DeploymentMode = "ceph"
	DeploymentModeSSH     DeploymentMode = "ssh"
)

const (
	datastoreTypeLocal = "local"
	datastoreTypeCeph  = "ceph"

	cephAttrPoolName    = "POOL_NAME"
	cephAttrHost        = "CEPH_HOST"
	cephAttrUser        = "CEPH_USER"
	cephAttrSecret      = "CEPH_SECRET"
	cephAttrConf        = "CEPH_CONF"
	cephAttrKey         = "CEPH_KEY"
	cephAttrBridgeList  = "BRIDGE_LIST"
	cephAttrRBDFormat   = "RBD_FORMAT"
	cephAttrECPoolName  = "EC_POOL_NAME"
	cephAttrDiskType    = "DISK_TYPE"
	compatibleSysDSAttr = "COMPATIBLE_SYS_DS"

	cephDiskTypeRBD = "RBD"
)

var cephRequiredAttrs = []string{
	cephAttrPoolName,
	cephAttrHost,
	cephAttrUser,
	cephAttrSecret,
	cephAttrBridgeList,
}

type DatastoreBackendProfile struct {
	Type              string
	RequiredAttrs     []string
	AllowedImageModes []DeploymentMode
	validate          func(datastoreSchema.Datastore) error
}

type BackendCapabilityProfile struct {
	Backend               string
	SupportsFilesystemRWO bool
	SupportsFilesystemROX bool
	SupportsFilesystemRWX bool
	SupportsBlockRWO      bool
	SupportsBlockROX      bool
	SupportsBlockRWX      bool
}

func (p DatastoreBackendProfile) ValidateDatastore(ds datastoreSchema.Datastore) error {
	if p.validate == nil {
		return nil
	}

	return p.validate(ds)
}

type DatastoreEnvironmentReport struct {
	ImageDatastore  Datastore
	SystemDatastore *Datastore
	DeploymentMode  DeploymentMode
	Warnings        []string
}

type CephDatastoreAttributes struct {
	PoolName   string
	CephHost   string
	CephUser   string
	CephSecret string
	CephConf   string
	CephKey    string
	DiskType   string
	BridgeList string
	RBDFormat  string
	ECPoolName string
}

func extractCephFSDatastoreAttributes(ds datastoreSchema.Datastore) CephFSDatastoreAttributes {
	return CephFSDatastoreAttributes{
		FSName:           getDatastoreAttribute(ds, cephFSAttrFSName),
		RootPath:         cleanSharedPath(getDatastoreAttribute(ds, cephFSAttrRootPath)),
		SubvolumeGroup:   getDatastoreAttribute(ds, cephFSAttrSubvolumeGroup),
		Monitors:         normalizeMonitorList(getDatastoreAttribute(ds, cephAttrHost)),
		MountOptions:     normalizeMountOptions(getDatastoreAttribute(ds, cephFSAttrMountOptions)),
		OptionalCephConf: getDatastoreAttribute(ds, cephAttrConf),
	}
}

func GetDatastoreBackendProfile(typ string) (DatastoreBackendProfile, error) {
	switch normalizeAllowedDatastoreType(typ) {
	case datastoreTypeLocal:
		return DatastoreBackendProfile{
			Type:              datastoreTypeLocal,
			AllowedImageModes: []DeploymentMode{DeploymentModeUnknown, DeploymentModeSSH, DeploymentModeCeph},
		}, nil
	case datastoreTypeCeph:
		return DatastoreBackendProfile{
			Type:              datastoreTypeCeph,
			RequiredAttrs:     append([]string(nil), cephRequiredAttrs...),
			AllowedImageModes: []DeploymentMode{DeploymentModeCeph, DeploymentModeSSH},
			validate:          validateCephImageDatastore,
		}, nil
	case datastoreTypeCephFS:
		return DatastoreBackendProfile{
			Type:          datastoreTypeCephFS,
			RequiredAttrs: []string{sharedBackendAttr, cephFSAttrFSName, cephFSAttrRootPath, cephFSAttrSubvolumeGroup, cephAttrHost},
			validate:      validateCephFSDatastore,
		}, nil
	default:
		return DatastoreBackendProfile{}, &datastoreConfigError{message: fmt.Sprintf("unsupported datastore backend %q", typ)}
	}
}

func GetBackendCapabilityProfile(backend string) BackendCapabilityProfile {
	switch normalizeAllowedDatastoreType(backend) {
	case datastoreTypeCeph:
		return BackendCapabilityProfile{
			Backend:               datastoreTypeCeph,
			SupportsFilesystemRWO: true,
			SupportsFilesystemROX: true,
			SupportsFilesystemRWX: false,
			SupportsBlockRWO:      true,
			SupportsBlockROX:      true,
			SupportsBlockRWX:      false,
		}
	case datastoreTypeCephFS:
		return BackendCapabilityProfile{
			Backend:               datastoreTypeCephFS,
			SupportsFilesystemRWO: true,
			SupportsFilesystemROX: true,
			SupportsFilesystemRWX: true,
			SupportsBlockRWO:      false,
			SupportsBlockROX:      false,
			SupportsBlockRWX:      false,
		}
	default:
		return BackendCapabilityProfile{
			Backend:               datastoreTypeLocal,
			SupportsFilesystemRWO: true,
			SupportsFilesystemROX: true,
			SupportsFilesystemRWX: false,
			SupportsBlockRWO:      true,
			SupportsBlockROX:      true,
			SupportsBlockRWX:      false,
		}
	}
}

func validateCephFSDatastore(ds datastoreSchema.Datastore) error {
	if !strings.EqualFold(strings.TrimSpace(getDatastoreAttribute(ds, sharedBackendAttr)), sharedBackendCephFS) {
		return &datastoreConfigError{message: fmt.Sprintf("datastore %d must define %s=cephfs", ds.ID, sharedBackendAttr)}
	}
	if !strings.EqualFold(normalizeDatastoreCategory(ds), string(datastoreSchema.File)) {
		return &datastoreConfigError{message: fmt.Sprintf("datastore %d must be an OpenNebula FILE datastore for CephFS RWX", ds.ID)}
	}
	if err := requireDatastoreAttributes(ds, cephFSAttrFSName, cephFSAttrRootPath, cephFSAttrSubvolumeGroup, cephAttrHost); err != nil {
		return err
	}
	if len(normalizeMonitorList(getDatastoreAttribute(ds, cephAttrHost))) == 0 {
		return &datastoreConfigError{message: fmt.Sprintf("datastore %d must define at least one Ceph monitor in %s", ds.ID, cephAttrHost)}
	}

	return nil
}

func extractCephDatastoreAttributes(ds datastoreSchema.Datastore) CephDatastoreAttributes {
	return CephDatastoreAttributes{
		PoolName:   getDatastoreAttribute(ds, cephAttrPoolName),
		CephHost:   getDatastoreAttribute(ds, cephAttrHost),
		CephUser:   getDatastoreAttribute(ds, cephAttrUser),
		CephSecret: getDatastoreAttribute(ds, cephAttrSecret),
		CephConf:   getDatastoreAttribute(ds, cephAttrConf),
		CephKey:    getDatastoreAttribute(ds, cephAttrKey),
		DiskType:   getDatastoreAttribute(ds, cephAttrDiskType),
		BridgeList: getDatastoreAttribute(ds, cephAttrBridgeList),
		RBDFormat:  getDatastoreAttribute(ds, cephAttrRBDFormat),
		ECPoolName: getDatastoreAttribute(ds, cephAttrECPoolName),
	}
}

func validateCephImageDatastore(ds datastoreSchema.Datastore) error {
	if !strings.EqualFold(strings.TrimSpace(ds.DSMad), datastoreTypeCeph) {
		return &datastoreConfigError{message: fmt.Sprintf("datastore %d must use DS_MAD=ceph", ds.ID)}
	}

	if !strings.EqualFold(strings.TrimSpace(ds.TMMad), datastoreTypeCeph) {
		return &datastoreConfigError{message: fmt.Sprintf("datastore %d must use TM_MAD=ceph", ds.ID)}
	}

	if normalizeDiskType(ds) != cephDiskTypeRBD {
		return &datastoreConfigError{message: fmt.Sprintf("datastore %d must use DISK_TYPE=RBD", ds.ID)}
	}

	return requireDatastoreAttributes(ds, cephRequiredAttrs...)
}

func validateCephSystemDatastore(ds datastoreSchema.Datastore) error {
	if !strings.EqualFold(strings.TrimSpace(ds.TMMad), datastoreTypeCeph) {
		return &datastoreConfigError{message: fmt.Sprintf("system datastore %d must use TM_MAD=ceph", ds.ID)}
	}

	if normalizeDiskType(ds) != cephDiskTypeRBD {
		return &datastoreConfigError{message: fmt.Sprintf("system datastore %d must use DISK_TYPE=RBD", ds.ID)}
	}

	return requireDatastoreAttributes(ds, cephRequiredAttrs...)
}

func compareCephConnectionIdentity(imageDS, systemDS datastoreSchema.Datastore) error {
	imageAttrs := extractCephDatastoreAttributes(imageDS)
	systemAttrs := extractCephDatastoreAttributes(systemDS)

	checks := []struct {
		name  string
		image string
		sys   string
	}{
		{name: cephAttrPoolName, image: imageAttrs.PoolName, sys: systemAttrs.PoolName},
		{name: cephAttrHost, image: imageAttrs.CephHost, sys: systemAttrs.CephHost},
		{name: cephAttrUser, image: imageAttrs.CephUser, sys: systemAttrs.CephUser},
		{name: cephAttrSecret, image: imageAttrs.CephSecret, sys: systemAttrs.CephSecret},
	}

	for _, check := range checks {
		if check.image != check.sys {
			return &datastoreConfigError{
				message: fmt.Sprintf("ceph datastore mismatch for %s between image datastore %d and system datastore %d", check.name, imageDS.ID, systemDS.ID),
			}
		}
	}

	optionalChecks := []struct {
		name  string
		image string
		sys   string
	}{
		{name: cephAttrConf, image: imageAttrs.CephConf, sys: systemAttrs.CephConf},
		{name: cephAttrKey, image: imageAttrs.CephKey, sys: systemAttrs.CephKey},
	}

	for _, check := range optionalChecks {
		if check.image == "" || check.sys == "" {
			continue
		}
		if check.image != check.sys {
			return &datastoreConfigError{
				message: fmt.Sprintf("ceph datastore mismatch for %s between image datastore %d and system datastore %d", check.name, imageDS.ID, systemDS.ID),
			}
		}
	}

	return nil
}

func resolveDeploymentMode(ds datastoreSchema.Datastore) DeploymentMode {
	switch strings.ToLower(strings.TrimSpace(ds.TMMad)) {
	case datastoreTypeCeph:
		return DeploymentModeCeph
	case "ssh":
		return DeploymentModeSSH
	default:
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(ds.TMMad)), "_ssh") {
			return DeploymentModeSSH
		}
		return DeploymentModeUnknown
	}
}

func requireDatastoreAttributes(ds datastoreSchema.Datastore, keys ...string) error {
	for _, key := range keys {
		if getDatastoreAttribute(ds, key) == "" {
			return &datastoreConfigError{message: fmt.Sprintf("datastore %d is missing required attribute %s", ds.ID, key)}
		}
	}

	return nil
}

func validateCompatibleSystemDatastore(imageDS datastoreSchema.Datastore, systemDatastoreID int) error {
	raw := getDatastoreAttribute(imageDS, compatibleSysDSAttr)
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	allowed := make(map[int]struct{})
	for _, part := range strings.Fields(strings.NewReplacer(",", " ", ";", " ").Replace(raw)) {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		id, err := strconv.Atoi(value)
		if err != nil {
			return &datastoreConfigError{
				message: fmt.Sprintf("datastore %d has invalid %s value %q", imageDS.ID, compatibleSysDSAttr, value),
			}
		}
		allowed[id] = struct{}{}
	}

	if len(allowed) == 0 {
		return nil
	}
	if _, ok := allowed[systemDatastoreID]; ok {
		return nil
	}

	return &datastoreConfigError{
		message: fmt.Sprintf("datastore %d is not compatible with target system datastore %d according to %s=%s", imageDS.ID, systemDatastoreID, compatibleSysDSAttr, strings.TrimSpace(raw)),
	}
}

func getDatastoreAttribute(ds datastoreSchema.Datastore, key string) string {
	switch strings.ToUpper(strings.TrimSpace(key)) {
	case "DS_MAD":
		return strings.TrimSpace(ds.DSMad)
	case "TM_MAD":
		return strings.TrimSpace(ds.TMMad)
	case cephAttrDiskType:
		raw := strings.TrimSpace(ds.DiskType)
		if raw != "" && !isNumericDatastoreField(raw) {
			return raw
		}
	}

	value, err := ds.Template.GetStr(strings.ToUpper(strings.TrimSpace(key)))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(value)
}

func normalizeDiskType(ds datastoreSchema.Datastore) string {
	return strings.ToUpper(getDatastoreAttribute(ds, cephAttrDiskType))
}

func isNumericDatastoreField(value string) bool {
	if value == "" {
		return false
	}

	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

func normalizeMonitorList(value string) []string {
	replacer := strings.NewReplacer(",", " ", ";", " ")
	parts := strings.Fields(replacer.Replace(strings.TrimSpace(value)))
	return parts
}

func normalizeMountOptions(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	raw := strings.Split(value, ",")
	options := make([]string, 0, len(raw))
	for _, option := range raw {
		trimmed := strings.TrimSpace(option)
		if trimmed == "" {
			continue
		}
		options = append(options, trimmed)
	}

	return options
}
