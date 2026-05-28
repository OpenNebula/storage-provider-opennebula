package opennebula

import (
	"errors"
	"testing"

	vmSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
)

func TestResolveNodeVMIDWithFallbackByName(t *testing.T) {
	id, path, err := resolveNodeVMIDWithFallback(
		"hplcsiw01",
		func(string) (int, error) { return 17, nil },
		func() ([]vmSchema.VM, error) { return nil, nil },
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if id != 17 || path != VMResolutionByName {
		t.Fatalf("unexpected resolution result: id=%d path=%q", id, path)
	}
}

func TestResolveNodeVMIDWithFallbackPoolExactMatch(t *testing.T) {
	id, path, err := resolveNodeVMIDWithFallback(
		"hplcsiw01",
		func(string) (int, error) { return -1, errors.New("resource not found") },
		func() ([]vmSchema.VM, error) {
			return []vmSchema.VM{
				{ID: 17, Name: "HPLCSIW01"},
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if id != 17 || path != VMResolutionPoolExactMatch {
		t.Fatalf("unexpected resolution result: id=%d path=%q", id, path)
	}
}

func TestResolveNodeVMIDWithFallbackNotFound(t *testing.T) {
	_, path, err := resolveNodeVMIDWithFallback(
		"hplcsiw01",
		func(string) (int, error) { return -1, errors.New("resource not found") },
		func() ([]vmSchema.VM, error) {
			return []vmSchema.VM{{ID: 18, Name: "other"}}, nil
		},
	)
	if path != VMResolutionNotFound {
		t.Fatalf("expected not_found path, got %q", path)
	}
	var notFound *VMNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected VMNotFoundError, got %v", err)
	}
}

func TestResolveNodeVMIDWithFallbackDuplicateMatch(t *testing.T) {
	_, path, err := resolveNodeVMIDWithFallback(
		"hplcsiw01",
		func(string) (int, error) { return -1, errors.New("multiple resources with that name") },
		func() ([]vmSchema.VM, error) {
			return []vmSchema.VM{
				{ID: 17, Name: "hplcsiw01"},
				{ID: 19, Name: "HPLCSIW01"},
			}, nil
		},
	)
	if path != VMResolutionDuplicateMatch {
		t.Fatalf("expected duplicate_match path, got %q", path)
	}
	var duplicate *VMDuplicateNameError
	if !errors.As(err, &duplicate) {
		t.Fatalf("expected VMDuplicateNameError, got %v", err)
	}
}
