package driver

import (
	"fmt"
	"strings"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	"github.com/container-storage-interface/spec/lib/go/csi"
)

func resolveVolumeAccessModel(capability *csi.VolumeCapability, allowedBackends []string, params map[string]string, candidates []opennebula.Datastore) (opennebula.VolumeAccessModel, error) {
	if capability == nil || capability.AccessMode == nil {
		return "", fmt.Errorf("missing access mode")
	}

	if err := validateNoMixedSharedFilesystemCandidates(candidates); err != nil {
		return "", err
	}

	mode := capability.AccessMode.Mode
	if _, ok := capability.GetAccessType().(*csi.VolumeCapability_Block); ok {
		if isSharedFilesystemAccessMode(mode) {
			return "", fmt.Errorf("access mode %q is not supported for block volumes; CephFS shared filesystem support requires a filesystem volume capability", mode.String())
		}
		return opennebula.VolumeAccessModelDisk, nil
	}

	if isSharedFilesystemAccessMode(mode) {
		if anyCandidateBackend(candidates, "cephfs") || len(candidates) == 0 {
			profiles := backendCapabilityProfiles(allowedBackends)
			for _, profile := range profiles {
				if profile.SupportsFilesystemRWX {
					return opennebula.VolumeAccessModelSharedFS, nil
				}
			}
			return "", unsupportedRWXError(mode, profiles)
		}

		return "", unsupportedRWXError(mode, backendCapabilityProfiles(candidateBackends(candidates)))
	}

	if storageClassRequestsSharedFilesystem(params) || allCandidatesBackend(candidates, "cephfs") {
		if !anyCandidateBackend(candidates, "cephfs") && len(candidates) > 0 {
			return "", fmt.Errorf("storage class requests CephFS shared filesystem provisioning but selected datastores do not resolve as cephfs")
		}
		return opennebula.VolumeAccessModelSharedFS, nil
	}

	return opennebula.VolumeAccessModelDisk, nil
}

func validateAccessMode(volumeCapabilities []*csi.VolumeCapability, allowedBackends []string) error {
	supportedModes := make(map[csi.VolumeCapability_AccessMode_Mode]bool)
	for _, mode := range supportedAccessModes {
		supportedModes[mode] = true
	}

	backendProfiles := backendCapabilityProfiles(allowedBackends)
	for _, cap := range volumeCapabilities {
		if cap.AccessMode == nil {
			return fmt.Errorf("missing access mode")
		}

		mode := cap.AccessMode.Mode
		if supportedModes[mode] {
			continue
		}

		if isSharedFilesystemAccessMode(mode) {
			if _, err := resolveVolumeAccessModel(cap, allowedBackends, nil, nil); err != nil {
				return err
			}
			continue
		}

		return fmt.Errorf("unsupported access mode %q; supported modes are %s",
			mode.String(),
			strings.Join(supportedAccessModeNames(), ", "))
	}

	_ = backendProfiles
	return nil
}

func storageClassRequestsSharedFilesystem(params map[string]string) bool {
	if params == nil {
		return false
	}
	if strings.TrimSpace(params[storageClassParamSharedFilesystemPath]) != "" || strings.TrimSpace(params[storageClassParamSharedFilesystemGroup]) != "" {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(params["driver"]), "cephfs") {
		return true
	}
	return false
}

func validateNoMixedSharedFilesystemCandidates(candidates []opennebula.Datastore) error {
	if len(candidates) == 0 {
		return nil
	}
	hasCephFS := false
	hasOther := false
	for _, candidate := range candidates {
		if strings.EqualFold(candidate.Backend, "cephfs") || strings.EqualFold(candidate.Type, "cephfs") {
			hasCephFS = true
		} else {
			hasOther = true
		}
	}
	if hasCephFS && hasOther {
		return fmt.Errorf("storage class mixes CephFS and disk datastores; use separate StorageClasses for cephfs and image-backed datastores")
	}
	return nil
}

func anyCandidateBackend(candidates []opennebula.Datastore, backend string) bool {
	for _, candidate := range candidates {
		if strings.EqualFold(candidate.Backend, backend) || strings.EqualFold(candidate.Type, backend) {
			return true
		}
	}
	return false
}

func allCandidatesBackend(candidates []opennebula.Datastore, backend string) bool {
	if len(candidates) == 0 {
		return false
	}
	for _, candidate := range candidates {
		if !strings.EqualFold(candidate.Backend, backend) && !strings.EqualFold(candidate.Type, backend) {
			return false
		}
	}
	return true
}

func candidateBackends(candidates []opennebula.Datastore) []string {
	backends := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		backend := strings.TrimSpace(candidate.Backend)
		if backend == "" {
			backend = strings.TrimSpace(candidate.Type)
		}
		if backend != "" {
			backends = append(backends, backend)
		}
	}
	return backends
}

func isSharedFilesystemAccessMode(mode csi.VolumeCapability_AccessMode_Mode) bool {
	switch mode {
	case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
		return true
	default:
		return false
	}
}
