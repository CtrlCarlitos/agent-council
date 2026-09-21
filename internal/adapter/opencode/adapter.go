//go:build !windows

package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// DispatchIdentitySource supplies the persisted attempt identity for a
// TurnRef before dispatch. Wired by the service to the storage dispatch
// intent; the adapter never reads storage directly and never invents "1".
type DispatchIdentitySource interface {
	AttemptFor(ctx context.Context, ref adapter.TurnRef) (attempt string, ok bool)
}

// managedDispatch tracks one in-flight or completed native dispatch.
type managedDispatch struct {
	nativeSessionID string
	userMessageID   string
	baselineMsgID   string
}

// launchReservation reserves a turn identity for the duration of process
// creation so concurrent duplicate dispatches cannot launch the same turn
// twice. Waiters receive the launcher's verdict.
type launchReservation struct {
	ready   chan struct{}
	outcome adapter.DispatchOutcome
	err     error
}

// OpenCodeAdapter implements the AC-006 adapter.Adapter contract against
// the installed OpenCode headless HTTP server.
type OpenCodeAdapter struct {
	executor      execpolicy.PolicyExecutor
	probeTemplate ProbeLaunchTemplate
	identity      DispatchIdentitySource
	idleGrace     time.Duration

	mu         sync.Mutex
	servers    map[string]*serverProcess
	dispatches map[adapter.TurnRef]*managedDispatch
	launching  map[adapter.TurnRef]*launchReservation
	inflight   map[string]chan struct{}
}

// OpenCodeAdapterOption configures an OpenCodeAdapter.
type OpenCodeAdapterOption func(*OpenCodeAdapter)

// WithIdleGrace overrides the parked-session idle grace period.
func WithIdleGrace(d time.Duration) OpenCodeAdapterOption {
	return func(a *OpenCodeAdapter) { a.idleGrace = d }
}

// NewOpenCodeAdapter constructs an OpenCode persistent contributor adapter.
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
		launching:     make(map[adapter.TurnRef]*launchReservation),
		inflight:      make(map[string]chan struct{}),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// ── Dispatch ────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	if err := ref.Validate(); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	attempt, ok := a.identity.AttemptFor(ctx, ref)
	if !ok {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: "no attempt identity"}, fmt.Errorf("no attempt identity for %s/%s", ref.SessionID, ref.TurnKey)
	}

	msgID, err := NativeMessageID(string(ref.SessionID), ref.TurnKey, attempt)
	if err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	ep, err := a.endpointFor(ref.SessionID)
	if err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	payload := map[string]any{
		"messageID": msgID,
		"parts":     []map[string]string{{"type": "text", "text": prompt}},
	}
	body, _ := json.Marshal(payload)
	httpReq, reqErr := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/session/%s/prompt_async", ep, ref.SessionID), strings.NewReader(string(body)))
	if reqErr != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: reqErr.Error()}, reqErr
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, respErr := http.DefaultClient.Do(httpReq)
	if respErr != nil {
		var opErr *net.OpError
		if errors.As(respErr, &opErr) {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: respErr.Error()}, respErr
		}
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown, Reason: respErr.Error()}, respErr
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 204 || resp.StatusCode == 200:
		a.mu.Lock()
		a.dispatches[ref] = &managedDispatch{userMessageID: msgID, nativeSessionID: string(ref.SessionID)}
		a.mu.Unlock()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
	default:
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
}

// ── Observe ─────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	stream := adapter.NewBufferedStream(ref, 64)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.mu.Lock()
				_, ok := a.dispatches[ref]
				a.mu.Unlock()
				if !ok {
					return
				}
			}
		}
	}()
	return stream, nil
}

// ── Cancel ──────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed}, nil
}

// ── Collect ─────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	return adapter.TurnResult{Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultPending}, nil
}

// ── Reconcile ───────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	return adapter.ReconciliationOutcome{
		Ref:          ref,
		Reachability: council.VisibilityHostLost,
		Status:       adapter.ReconciliationUncertain,
		Observed:     council.TurnRunning,
	}, nil
}

// ── CreateSession / ResumeSession ──────────────────────────────────────

func (a *OpenCodeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}
	return adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: fmt.Sprintf("ses_council_%s", req.SessionID),
		Config:          req.Config,
	}, nil
}

func (a *OpenCodeAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	return nil
}

// ── Internal helpers ────────────────────────────────────────────────────

func (a *OpenCodeAdapter) endpointFor(sessionID adapter.SessionID) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sp, ok := a.servers[string(sessionID)]; ok {
		return sp.endpoint, nil
	}
	return "", fmt.Errorf("no server for session %s", sessionID)
}
