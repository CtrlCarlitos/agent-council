//go:build !windows

package opencode

import (
	"context"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
)

// DispatchIdentitySource supplies the persisted attempt identity for a
// TurnRef before dispatch. Wired by the service to the storage dispatch
// intent; the adapter never reads storage directly and never invents "1".
type DispatchIdentitySource interface {
	AttemptFor(ctx context.Context, ref adapter.TurnRef) (attempt string, ok bool)
}

// ProbeLaunchTemplate is an operator-owned builder that produces fully
// validated LaunchRequest values for the adapter's Probe capability check.
// (Defined in probe.go; re-declared here for doc coherence.)
// See probe.go for the full contract.

// OpenCodeServer manages one adapter-owned `opencode serve` child per
// contributor session (construction/wiring in Task 2's ServerConfig).
// See server.go for the full lifecycle (Task 2).

// managedDispatch tracks one in-flight or completed native dispatch.
type managedDispatch struct {
	userMessageID string
	baselineMsgID string
}

// OpenCodeAdapter implements the AC-006 adapter.Adapter contract against
// the installed OpenCode headless HTTP server.
type OpenCodeAdapter struct {
	executor      execpolicy.PolicyExecutor
	probeTemplate ProbeLaunchTemplate
	identity      DispatchIdentitySource
	idleGrace     time.Duration

	mu         sync.Mutex
	servers    map[string]*serverProcess // keyed by native session ID
	dispatches map[adapter.TurnRef]*managedDispatch
	inFlight   map[string]chan struct{} // native sessionID → done channel
}

// OpenCodeAdapterOption configures an OpenCodeAdapter.
type OpenCodeAdapterOption func(*OpenCodeAdapter)

// WithIdleGrace overrides the parked-session idle grace period.
func WithIdleGrace(d time.Duration) OpenCodeAdapterOption {
	return func(a *OpenCodeAdapter) { a.idleGrace = d }
}

// NewOpenCodeAdapter constructs an OpenCode persistent contributor adapter.
// The service wires DispatchIdentitySource to the storage dispatch intent
// and ProbeLaunchTemplate to an operator-owned launch template.
func NewOpenCodeAdapter(
	executor execpolicy.PolicyExecutor,
	probeTemplate ProbeLaunchTemplate,
	identity DispatchIdentitySource,
	opts ...OpenCodeAdapterOption,
) *OpenCodeAdapter {
	a := &OpenCodeAdapter{
		executor:      executor,
		probeTemplate: probeTemplate,
		identity:      identity,
		idleGrace:     30 * time.Second,
		servers:       make(map[string]*serverProcess),
		dispatches:    make(map[adapter.TurnRef]*managedDispatch),
		inFlight:      make(map[string]chan struct{}),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}
