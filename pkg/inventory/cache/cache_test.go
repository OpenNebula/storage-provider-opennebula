package cache

import (
	"testing"

	inventoryv1alpha1 "github.com/SparkAIUR/storage-provider-opennebula/pkg/inventory/apis/storageprovider/v1alpha1"
)

func TestFilterIdentifiersHonorsDisplayPhases(t *testing.T) {
	p := &Provider{
		enabled:       true,
		authorityMode: AuthorityModeStrict,
		entries: map[int]DatastoreEligibility{
			1: {ID: 1, Enabled: true, Allowed: true, Phase: inventoryv1alpha1.DatastorePhaseEnabled},
			2: {ID: 2, Enabled: true, Allowed: true, Phase: inventoryv1alpha1.DatastorePhaseAvailable},
			3: {ID: 3, Enabled: false, Allowed: true, Phase: inventoryv1alpha1.DatastorePhaseDisabled},
			4: {ID: 4, Enabled: true, Allowed: true, Phase: inventoryv1alpha1.DatastorePhaseUnavailable},
		},
	}

	filtered, err := p.FilterIdentifiers([]string{"1", "2"})
	if err != nil {
		t.Fatalf("expected enabled/available datastores to pass, got error: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("expected 2 filtered identifiers, got %d", len(filtered))
	}

	if _, err := p.FilterIdentifiers([]string{"3"}); err == nil {
		t.Fatal("expected disabled datastore to be rejected")
	}
	if _, err := p.FilterIdentifiers([]string{"4"}); err == nil {
		t.Fatal("expected unavailable datastore to be rejected")
	}
}
