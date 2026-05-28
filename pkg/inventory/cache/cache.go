package cache

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	inventoryv1alpha1 "github.com/SparkAIUR/storage-provider-opennebula/pkg/inventory/apis/storageprovider/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	AuthorityModeStrict     = "strict"
	AuthorityModePermissive = "permissive"
)

type DatastoreEligibility struct {
	ID      int
	Enabled bool
	Allowed bool
	Phase   string
}

type DatastoreEligibilityProvider interface {
	Start(context.Context) error
	Enabled() bool
	FilterIdentifiers([]string) ([]string, error)
}

type Provider struct {
	client        ctrlclient.Client
	resync        time.Duration
	authorityMode string

	mu      sync.RWMutex
	enabled bool
	entries map[int]DatastoreEligibility
}

func NewProvider(restConfig *rest.Config, resync time.Duration, authorityMode string) (*Provider, error) {
	if restConfig == nil {
		return nil, fmt.Errorf("rest config is required")
	}
	runtimeScheme := runtime.NewScheme()
	if err := inventoryv1alpha1.AddToScheme(runtimeScheme); err != nil {
		return nil, err
	}
	client, err := ctrlclient.New(restConfig, ctrlclient.Options{Scheme: runtimeScheme})
	if err != nil {
		return nil, err
	}
	if resync <= 0 {
		resync = time.Minute
	}
	mode := strings.ToLower(strings.TrimSpace(authorityMode))
	if mode == "" {
		mode = AuthorityModeStrict
	}
	return &Provider{
		client:        client,
		resync:        resync,
		authorityMode: mode,
		enabled:       true,
		entries:       make(map[int]DatastoreEligibility),
	}, nil
}

func (p *Provider) Start(ctx context.Context) error {
	if p == nil || !p.enabled {
		return nil
	}
	if err := p.refresh(ctx); err != nil {
		return err
	}
	go p.loop(ctx)
	return nil
}

func (p *Provider) Enabled() bool {
	return p != nil && p.enabled
}

func (p *Provider) FilterIdentifiers(identifiers []string) ([]string, error) {
	if p == nil || !p.Enabled() {
		return append([]string(nil), identifiers...), nil
	}

	filtered := make([]string, 0, len(identifiers))
	for _, identifier := range identifiers {
		trimmed := strings.TrimSpace(identifier)
		if trimmed == "" {
			continue
		}
		id, err := strconv.Atoi(trimmed)
		if err != nil {
			filtered = append(filtered, trimmed)
			continue
		}
		entry, ok := p.lookup(id)
		if !ok {
			if p.authorityMode == AuthorityModeStrict {
				return nil, fmt.Errorf("inventory authority rejected datastore %d because no OpenNebulaDatastore object exists", id)
			}
			filtered = append(filtered, trimmed)
			continue
		}
		if !entry.Enabled || !entry.Allowed || (entry.Phase != "" && entry.Phase != inventoryv1alpha1.DatastorePhaseEnabled && entry.Phase != inventoryv1alpha1.DatastorePhaseAvailable) {
			return nil, fmt.Errorf("inventory authority rejected datastore %d because provisioning is disabled", id)
		}
		filtered = append(filtered, trimmed)
	}
	return filtered, nil
}

func (p *Provider) lookup(id int) (DatastoreEligibility, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, ok := p.entries[id]
	return entry, ok
}

func (p *Provider) loop(ctx context.Context) {
	ticker := time.NewTicker(p.resync)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = p.refresh(ctx)
		}
	}
}

func (p *Provider) refresh(ctx context.Context) error {
	list := &inventoryv1alpha1.OpenNebulaDatastoreList{}
	if err := p.client.List(ctx, list); err != nil {
		return err
	}
	entries := make(map[int]DatastoreEligibility, len(list.Items))
	for _, item := range list.Items {
		entries[item.Spec.Discovery.OpenNebulaDatastoreID] = DatastoreEligibility{
			ID:      item.Spec.Discovery.OpenNebulaDatastoreID,
			Enabled: item.Spec.Enabled,
			Allowed: item.Spec.Discovery.Allowed,
			Phase:   item.Status.Phase,
		}
	}
	p.mu.Lock()
	p.entries = entries
	p.mu.Unlock()
	return nil
}
