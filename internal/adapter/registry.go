package adapter

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

var (
	// ErrUnknownAdapter is returned when an unrecognized contributor adapter is requested.
	ErrUnknownAdapter = errors.New("unknown or unsupported adapter")

	// ErrFakeAdapterProhibited is returned when the test fake is requested from the production registry.
	ErrFakeAdapterProhibited = errors.New("fake adapter is prohibited in production registry")
)

// Registry manages production adapter implementations for council contributors.
// Test implementations (such as adaptertest.FakeAdapter) must never be registered in production.
type Registry struct {
	mu       sync.RWMutex
	adapters map[council.Contributor]Adapter
}

// NewRegistry initializes an empty production adapter registry.
func NewRegistry() *Registry {
	return &Registry{
		adapters: make(map[council.Contributor]Adapter),
	}
}

// Register registers a production adapter for a contributor.
func (r *Registry) Register(contrib council.Contributor, ad Adapter) error {
	if !council.ValidContributor(contrib) {
		return errors.New("invalid contributor")
	}
	if ad == nil {
		return errors.New("nil adapter")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[contrib] = ad
	return nil
}

// Resolve returns the registered adapter for a contributor.
func (r *Registry) Resolve(contrib council.Contributor) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ad, exists := r.adapters[contrib]
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrUnknownAdapter, contrib)
	}
	return ad, nil
}

// ResolveByName resolves an adapter by contributor name or fails closed.
func (r *Registry) ResolveByName(name string) (Adapter, error) {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "fake" || strings.Contains(lower, "fake") {
		return nil, ErrFakeAdapterProhibited
	}
	contrib := council.Contributor(lower)
	if !council.ValidContributor(contrib) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownAdapter, name)
	}
	return r.Resolve(contrib)
}
