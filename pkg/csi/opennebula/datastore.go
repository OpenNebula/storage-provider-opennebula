package opennebula

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	datastoreSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/datastore"
)

const mib = 1024 * 1024

type DatastoreSelectionPolicy string

const (
	DatastoreSelectionPolicyLeastUsed DatastoreSelectionPolicy = "least-used"
	DatastoreSelectionPolicyOrdered   DatastoreSelectionPolicy = "ordered"
	DatastoreSelectionPolicyAutopilot DatastoreSelectionPolicy = "autopilot"
)

type DatastoreSelectionConfig struct {
	Identifiers  []string
	Policy       DatastoreSelectionPolicy
	AllowedTypes []string
}

type Datastore struct {
	ID                         int
	Name                       string
	Category                   string
	Type                       string
	Backend                    string
	DSMad                      string
	TMMad                      string
	DiskType                   string
	FreeBytes                  int64
	TotalBytes                 int64
	Enabled                    bool
	CompatibleSystemDatastores []int
	Ceph                       *CephDatastoreAttributes
	CephFS                     *CephFSDatastoreAttributes
}

type VolumeCreateResult struct {
	Datastore             Datastore
	CapacityBytes         int64
	FallbackUsed          bool
	AttemptedDatastoreIDs []int
}

type DatastoreSelector interface {
	Sort([]Datastore) []Datastore
}

type datastoreConfigError struct {
	message string
}

func (e *datastoreConfigError) Error() string {
	return e.message
}

type datastoreCapacityError struct {
	message string
}

func (e *datastoreCapacityError) Error() string {
	return e.message
}

func IsDatastoreConfigError(err error) bool {
	_, ok := err.(*datastoreConfigError)
	return ok
}

func IsDatastoreCapacityError(err error) bool {
	_, ok := err.(*datastoreCapacityError)
	return ok
}

func NewDatastoreConfigError(message string) error {
	return &datastoreConfigError{message: message}
}

func NewDatastoreCapacityError(message string) error {
	return &datastoreCapacityError{message: message}
}

func NormalizeDatastoreSelectionPolicy(policy string) DatastoreSelectionPolicy {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", string(DatastoreSelectionPolicyLeastUsed):
		return DatastoreSelectionPolicyLeastUsed
	case string(DatastoreSelectionPolicyOrdered):
		return DatastoreSelectionPolicyOrdered
	case string(DatastoreSelectionPolicyAutopilot):
		return DatastoreSelectionPolicyAutopilot
	default:
		return DatastoreSelectionPolicy(strings.ToLower(strings.TrimSpace(policy)))
	}
}

func NewDatastoreSelector(policy DatastoreSelectionPolicy) (DatastoreSelector, error) {
	switch NormalizeDatastoreSelectionPolicy(string(policy)) {
	case DatastoreSelectionPolicyLeastUsed:
		return leastUsedDatastoreSelector{}, nil
	case DatastoreSelectionPolicyOrdered:
		return orderedDatastoreSelector{}, nil
	case DatastoreSelectionPolicyAutopilot:
		return autopilotDatastoreSelector{}, nil
	default:
		return nil, &datastoreConfigError{message: fmt.Sprintf("unsupported datastore selection policy %q", policy)}
	}
}

func ResolveDatastores(pool []datastoreSchema.Datastore, selection DatastoreSelectionConfig) ([]Datastore, error) {
	if len(selection.Identifiers) == 0 {
		return nil, &datastoreConfigError{message: "no datastores configured for provisioning"}
	}

	allowedTypes := normalizeAllowedDatastoreTypes(selection.AllowedTypes)
	poolByID := make(map[int]datastoreSchema.Datastore, len(pool))
	var defaultDatastore *datastoreSchema.Datastore
	for _, candidate := range pool {
		poolByID[candidate.ID] = candidate
		if candidate.Name == "default" {
			candidateCopy := candidate
			defaultDatastore = &candidateCopy
		}
	}

	resolved := make([]Datastore, 0, len(selection.Identifiers))
	seen := make(map[int]struct{}, len(selection.Identifiers))
	for _, identifier := range selection.Identifiers {
		candidate, err := resolveDatastoreIdentifier(identifier, poolByID, defaultDatastore)
		if err != nil {
			return nil, err
		}

		normalized := datastoreFromSchema(candidate)
		if !normalized.Enabled {
			return nil, &datastoreConfigError{message: fmt.Sprintf("datastore %q is not enabled", identifier)}
		}

		if err := validateProvisioningDatastoreCategory(candidate); err != nil {
			return nil, err
		}

		if _, ok := allowedTypes[normalized.Type]; !ok {
			return nil, &datastoreConfigError{
				message: fmt.Sprintf("datastore %d resolved as type %q, which is not allowed", normalized.ID, normalized.Type),
			}
		}

		profile, err := GetDatastoreBackendProfile(normalized.Type)
		if err != nil {
			return nil, err
		}
		if err := profile.ValidateDatastore(candidate); err != nil {
			return nil, err
		}
		normalized.Backend = profile.Type

		if _, ok := seen[normalized.ID]; ok {
			continue
		}
		seen[normalized.ID] = struct{}{}
		resolved = append(resolved, normalized)
	}

	if len(resolved) == 0 {
		return nil, &datastoreConfigError{message: "no eligible datastores found for provisioning"}
	}

	return resolved, nil
}

func OrderDatastores(candidates []Datastore, policy DatastoreSelectionPolicy) ([]Datastore, error) {
	selector, err := NewDatastoreSelector(policy)
	if err != nil {
		return nil, err
	}

	return selector.Sort(candidates), nil
}

func SumDatastoreCapacity(candidates []Datastore) int64 {
	var total int64
	for _, candidate := range candidates {
		total += candidate.FreeBytes
	}

	return total
}

func normalizeAllowedDatastoreTypes(values []string) map[string]struct{} {
	if len(values) == 0 {
		values = []string{"local"}
	}

	normalized := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized[normalizeAllowedDatastoreType(value)] = struct{}{}
	}

	return normalized
}

func resolveDatastoreIdentifier(identifier string, poolByID map[int]datastoreSchema.Datastore, defaultDatastore *datastoreSchema.Datastore) (datastoreSchema.Datastore, error) {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return datastoreSchema.Datastore{}, &datastoreConfigError{message: "datastore identifier cannot be empty"}
	}

	if strings.EqualFold(trimmed, "default") {
		if defaultDatastore == nil {
			return datastoreSchema.Datastore{}, &datastoreConfigError{message: "default datastore alias was requested but no datastore named \"default\" exists"}
		}

		return *defaultDatastore, nil
	}

	id, err := strconv.Atoi(trimmed)
	if err != nil {
		return datastoreSchema.Datastore{}, &datastoreConfigError{
			message: fmt.Sprintf("unsupported datastore identifier %q: use datastore IDs or the default alias", trimmed),
		}
	}

	candidate, ok := poolByID[id]
	if !ok {
		return datastoreSchema.Datastore{}, &datastoreConfigError{message: fmt.Sprintf("datastore %d was not found", id)}
	}

	return candidate, nil
}

func datastoreFromSchema(source datastoreSchema.Datastore) Datastore {
	enabled := true
	if state, err := source.State(); err == nil {
		enabled = state == datastoreSchema.Ready
	}

	return Datastore{
		ID:                         source.ID,
		Name:                       source.Name,
		Category:                   normalizeDatastoreCategory(source),
		Type:                       normalizeAllowedDatastoreType(inferDatastoreType(source)),
		Backend:                    normalizeAllowedDatastoreType(inferDatastoreType(source)),
		DSMad:                      strings.ToLower(strings.TrimSpace(source.DSMad)),
		TMMad:                      strings.ToLower(strings.TrimSpace(source.TMMad)),
		DiskType:                   normalizeDiskType(source),
		FreeBytes:                  int64(source.FreeMB) * mib,
		TotalBytes:                 int64(source.TotalMB) * mib,
		Enabled:                    enabled,
		CompatibleSystemDatastores: compatibleSystemDatastores(source),
		Ceph:                       datastoreCephAttributes(source),
		CephFS:                     datastoreCephFSAttributes(source),
	}
}

func compatibleSystemDatastores(ds datastoreSchema.Datastore) []int {
	raw := getDatastoreAttribute(ds, compatibleSysDSAttr)
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	replacer := strings.NewReplacer(",", " ", ";", " ")
	parts := strings.Fields(replacer.Replace(raw))
	ids := make([]int, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		id, err := strconv.Atoi(value)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}

	if len(ids) == 0 {
		return nil
	}

	return ids
}

func datastoreCephAttributes(source datastoreSchema.Datastore) *CephDatastoreAttributes {
	if normalizeAllowedDatastoreType(inferDatastoreType(source)) != datastoreTypeCeph {
		return nil
	}

	attrs := extractCephDatastoreAttributes(source)
	return &attrs
}

func inferDatastoreType(source datastoreSchema.Datastore) string {
	if strings.EqualFold(getDatastoreAttribute(source, sharedBackendAttr), sharedBackendCephFS) {
		return datastoreTypeCephFS
	}

	dsMad := strings.ToLower(strings.TrimSpace(source.DSMad))
	tmMad := strings.ToLower(strings.TrimSpace(source.TMMad))
	category := normalizeDatastoreCategory(source)

	switch dsMad {
	case "fs", "fs_lvm", "fs_lvm_ssh":
		if category == string(datastoreSchema.Image) || category == string(datastoreSchema.File) || tmMad == "ssh" || tmMad == "shared" || tmMad == "local" {
			return datastoreTypeLocal
		}
	case datastoreTypeCeph:
		return datastoreTypeCeph
	}

	values := []string{
		tmMad,
		dsMad,
		strings.ToLower(strings.TrimSpace(source.Type)),
	}

	for _, value := range values {
		switch value {
		case "local", "ceph", "nfs", "nas", "fs_lvm", "fs_lvm_ssh", datastoreTypeCephFS:
			return value
		case "fs":
			if strings.EqualFold(source.TMMad, "local") {
				return "local"
			}
		}
	}

	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return "unknown"
}

func validateProvisioningDatastoreCategory(source datastoreSchema.Datastore) error {
	switch normalizeDatastoreCategory(source) {
	case "", string(datastoreSchema.Image), string(datastoreSchema.File):
		return nil
	default:
		templateType := strings.TrimSpace(getDatastoreAttribute(source, "TYPE"))
		return &datastoreConfigError{
			message: fmt.Sprintf("datastore %d has OpenNebula type %q (template TYPE=%q) and cannot be used for CSI provisioning; select an IMAGE or FILE datastore", source.ID, source.Type, templateType),
		}
	}
}

func normalizeDatastoreCategory(source datastoreSchema.Datastore) string {
	candidates := []string{
		strings.TrimSpace(source.Type),
		getDatastoreAttribute(source, "TYPE"),
	}

	for _, candidate := range candidates {
		switch strings.ToUpper(strings.TrimSpace(candidate)) {
		case "", "-":
			continue
		case string(datastoreSchema.Image), "IMAGE_DS", "IMG", "0":
			return string(datastoreSchema.Image)
		case string(datastoreSchema.System), "SYSTEM_DS", "SYS", "1":
			return string(datastoreSchema.System)
		case string(datastoreSchema.File), "FILE_DS", "FILES", "FIL", "2":
			return string(datastoreSchema.File)
		}
	}

	return strings.ToUpper(strings.TrimSpace(source.Type))
}

func normalizeAllowedDatastoreType(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", "nas":
		return "nfs"
	case "fs", "fs_lvm", "fs_lvm_ssh":
		return "local"
	default:
		return normalized
	}
}

func datastoreCephFSAttributes(source datastoreSchema.Datastore) *CephFSDatastoreAttributes {
	if normalizeAllowedDatastoreType(inferDatastoreType(source)) != datastoreTypeCephFS {
		return nil
	}

	attrs := extractCephFSDatastoreAttributes(source)
	return &attrs
}

type leastUsedDatastoreSelector struct{}

func (leastUsedDatastoreSelector) Sort(candidates []Datastore) []Datastore {
	sorted := append([]Datastore(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].FreeBytes == sorted[j].FreeBytes {
			return false
		}
		return sorted[i].FreeBytes > sorted[j].FreeBytes
	})
	return sorted
}

type orderedDatastoreSelector struct{}

func (orderedDatastoreSelector) Sort(candidates []Datastore) []Datastore {
	return append([]Datastore(nil), candidates...)
}
