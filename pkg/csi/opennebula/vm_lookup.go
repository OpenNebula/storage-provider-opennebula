package opennebula

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/OpenNebula/one/src/oca/go/src/goca"
	vmSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
)

const (
	VMResolutionByName         = "by_name"
	VMResolutionPoolExactMatch = "pool_exact_match"
	VMResolutionNotFound       = "not_found"
	VMResolutionDuplicateMatch = "duplicate_match"
)

type VMNotFoundError struct {
	Name string
}

func (e *VMNotFoundError) Error() string {
	if e == nil {
		return "opennebula vm not found"
	}
	return fmt.Sprintf("opennebula vm %q not found", e.Name)
}

type VMDuplicateNameError struct {
	Name  string
	Match []int
}

func (e *VMDuplicateNameError) Error() string {
	if e == nil {
		return "multiple opennebula vms matched"
	}
	return fmt.Sprintf("multiple opennebula vms matched %q: %v", e.Name, e.Match)
}

func ResolveNodeVMID(ctx context.Context, ctrl *goca.Controller, nodeName string) (int, string, error) {
	if ctrl == nil {
		return -1, VMResolutionNotFound, errors.New("opennebula controller is nil")
	}
	return resolveNodeVMIDWithFallback(
		nodeName,
		func(name string) (int, error) {
			return ctrl.VMs().ByNameContext(ctx, name)
		},
		func() ([]vmSchema.VM, error) {
			vmPool, err := ctrl.VMs().InfoContext(ctx)
			if err != nil {
				return nil, err
			}
			if vmPool == nil {
				return nil, nil
			}
			return vmPool.VMs, nil
		},
	)
}

func resolveNodeVMIDWithFallback(nodeName string, byName func(string) (int, error), listVMs func() ([]vmSchema.VM, error)) (int, string, error) {
	if id, err := byName(nodeName); err == nil {
		return id, VMResolutionByName, nil
	}

	vms, err := listVMs()
	if err != nil {
		return -1, VMResolutionNotFound, fmt.Errorf("failed to list OpenNebula VMs: %w", err)
	}

	matches := make([]int, 0, 1)
	for _, candidate := range vms {
		if strings.EqualFold(candidate.Name, nodeName) {
			matches = append(matches, candidate.ID)
		}
	}

	switch len(matches) {
	case 0:
		return -1, VMResolutionNotFound, &VMNotFoundError{Name: nodeName}
	case 1:
		return matches[0], VMResolutionPoolExactMatch, nil
	default:
		return -1, VMResolutionDuplicateMatch, &VMDuplicateNameError{Name: nodeName, Match: matches}
	}
}
