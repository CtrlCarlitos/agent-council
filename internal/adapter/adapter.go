package adapter

import (
	"context"
	"errors"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// CreateSessionRequest encapsulates requirements for starting a fresh native harness session.
type CreateSessionRequest struct {
	SessionID   SessionID
	Contributor council.Contributor
	Config      SessionConfig
}

// Validate ensures session creation parameters are sound.
func (r CreateSessionRequest) Validate() error {
	if strings.TrimSpace(string(r.SessionID)) == "" {
		return errors.New("empty session id")
	}
	if !council.ValidContributor(r.Contributor) {
		return errors.New("invalid contributor")
	}
	return nil
}

// Adapter defines the typed contract every native harness integration must implement.
// Call contexts bound individual RPCs/requests and never cancel accepted background workers.
type Adapter interface {
	// Probe discovers supported capabilities, version, and model inventory without billing.
	Probe(ctx context.Context) (ProbeReport, error)

	// CreateSession establishes an isolated conversation binding for a logical session.
	// Duplicate requests with matching config return the existing binding idempotently.
	CreateSession(ctx context.Context, req CreateSessionRequest) (SessionBinding, error)

	// ResumeSession attaches to a previously verified native session binding.
	ResumeSession(ctx context.Context, binding SessionBinding) error

	// Dispatch submits a prompt turn for execution. If the transport disconnects or times out
	// after potential worker receipt, it returns DispatchUnknown, preserving execution reservation.
	Dispatch(ctx context.Context, ref TurnRef, prompt string) (DispatchOutcome, error)

	// Observe establishes an event subscription for a running turn.
	// Cancelling ctx ends the subscription but does NOT cancel the underlying worker.
	Observe(ctx context.Context, ref TurnRef) (Stream, error)

	// Cancel requests cooperative cancellation of an active turn.
	Cancel(ctx context.Context, ref TurnRef) (CancelOutcome, error)

	// Collect retrieves the finalized result and cumulative usage metrics.
	Collect(ctx context.Context, ref TurnRef) (TurnResult, error)

	// Reconcile checks host reachability and verifies active turn status after suspected host loss.
	Reconcile(ctx context.Context, ref RecoveryRef) (ReconciliationOutcome, error)
}
