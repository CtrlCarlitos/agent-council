package opencode

import (
	"context"
	"errors"
	"fmt"
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
	terminal        bool // Collect observed a terminal outcome
}

// launchReservation reserves a turn identity during process creation.
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
	servers    *serverManager
	dispatches map[adapter.TurnRef]*managedDispatch
	launching  map[adapter.TurnRef]*launchReservation
	unknown    map[adapter.TurnRef]string // ref → messageID of an ambiguous attempt
	parkTimers map[adapter.SessionID]*time.Timer
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
		dispatches:    make(map[adapter.TurnRef]*managedDispatch),
		launching:     make(map[adapter.TurnRef]*launchReservation),
		unknown:       make(map[adapter.TurnRef]string),
		parkTimers:    make(map[adapter.SessionID]*time.Timer),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// NewOpenCodeAdapterWithLaunch is like NewOpenCodeAdapter but accepts an
// explicit SessionLaunchSource (the production storage-backed
// implementation).
func NewOpenCodeAdapterWithLaunch(
	executor execpolicy.PolicyExecutor,
	probeTemplate ProbeLaunchTemplate,
	identity DispatchIdentitySource,
	launch SessionLaunchSource,
	opts ...OpenCodeAdapterOption,
) *OpenCodeAdapter {
	a := NewOpenCodeAdapter(executor, probeTemplate, identity, opts...)
	a.servers = newServerManager(executor, launch)
	return a
}

// ── Probe ───────────────────────────────────────────────────────────────

// ── CreateSession ───────────────────────────────────────────────────────

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

// ── ResumeSession ───────────────────────────────────────────────────────

func (a *OpenCodeAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	return nil
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

	a.cancelIdleParkTimer(ref.SessionID)

	client, err := a.clientForOrResume(ctx, ref.SessionID)
	if err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	// Reservation: concurrent Dispatch calls for the same ref share one
	// native attempt and the launcher's verdict.
	a.mu.Lock()
	if _, ok := a.dispatches[ref]; ok {
		a.mu.Unlock()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
	}
	if res, ok := a.launching[ref]; ok {
		a.mu.Unlock()
		<-res.ready
		return res.outcome, res.err
	}
	res := &launchReservation{ready: make(chan struct{})}
	a.launching[ref] = res
	a.mu.Unlock()

	outcome, err := a.dispatchNative(ctx, ref, client, msgID, prompt)

	a.mu.Lock()
	delete(a.launching, ref)
	switch outcome.Status {
	case adapter.DispatchAccepted:
		delete(a.unknown, ref)
		a.dispatches[ref] = &managedDispatch{userMessageID: msgID, nativeSessionID: string(ref.SessionID)}
	case adapter.DispatchUnknown:
		a.unknown[ref] = msgID
	}
	a.mu.Unlock()

	res.outcome, res.err = outcome, err
	close(res.ready)
	return outcome, err
}

// dispatchNative performs the authenticated POST for one turn attempt. For a
// ref with a prior ambiguous attempt it first verifies by GET-by-message-ID:
// only a verified 404 authorizes resubmission; an already-recorded message is
// accepted without a second prompt_async.
func (a *OpenCodeAdapter) dispatchNative(ctx context.Context, ref adapter.TurnRef, client *NativeClient, msgID, prompt string) (adapter.DispatchOutcome, error) {
	a.mu.Lock()
	_, priorUnknown := a.unknown[ref]
	a.mu.Unlock()

	if priorUnknown {
		_, recorded, verifyErr := client.GetMessage(ctx, string(ref.SessionID), msgID)
		if verifyErr != nil {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown, Reason: "retry verification failed: " + verifyErr.Error()}, verifyErr
		}
		if recorded {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted, Reason: "message already recorded; resubmission skipped"}, nil
		}
	}

	if err := client.PromptAsync(ctx, string(ref.SessionID), msgID, prompt); err != nil {
		if IsPostWriteError(err) {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown, Reason: "post-write transport failure: " + err.Error()}, err
		}
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}
	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
}

// ── Observe ─────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	stream := adapter.NewBufferedStream(ref, 64)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
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
	client, err := a.clientFor(ref.SessionID)
	if err != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown, Reason: err.Error()}, err
	}
	if err := client.Abort(ctx, string(ref.SessionID)); err != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown, Reason: err.Error()}, err
	}
	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed}, nil
}

// ── Collect ─────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	a.mu.Lock()
	d, ok := a.dispatches[ref]
	a.mu.Unlock()
	if !ok {
		return adapter.TurnResult{
			Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultPending,
		}, errors.New("turn not dispatched")
	}
	msg, found := a.findAssistantByParentID(ctx, string(ref.SessionID), d.userMessageID)
	if !found {
		return adapter.TurnResult{
			Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultPending,
		}, nil
	}
	d.terminal = true
	a.scheduleIdleParkIfIdle(ref.SessionID)
	text := ""
	for _, p := range msg.Parts {
		if p.Type == "text" {
			text = p.Text
			break
		}
	}
	now := time.Now().UTC()
	if msg.Error != nil {
		return adapter.TurnResult{
			Ref: ref, Status: council.TurnFailed, ResultStatus: adapter.ResultFailed,
			Output: text, CompletedAt: now,
		}, nil
	}
	return adapter.TurnResult{
		Ref: ref, Status: council.TurnCompleted, ResultStatus: adapter.ResultAvailable,
		Output: text, CompletedAt: now,
	}, nil
}

// ── Reconcile ───────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	msg, found := a.findAssistantByParentID(ctx, string(ref.TurnRef.SessionID), "")
	if !found {
		// No assistant message yet: the native execution may still be running
		// or the process may have died. Uncertain.
		return adapter.ReconciliationOutcome{
			Ref: ref, Reachability: council.VisibilityHostLost,
			Status: adapter.ReconciliationUncertain, Observed: council.TurnRunning,
		}, nil
	}
	observed := council.TurnCompleted
	if msg.Error != nil {
		observed = council.TurnFailed
	}
	return adapter.ReconciliationOutcome{
		Ref: ref, Reachability: council.VisibilityReachable,
		Status:   adapter.ReconciliationReachableTerminal,
		Observed: observed,
	}, nil
}

// findAssistantByParentID polls the server for an assistant message whose
// parentID matches the given ID. Returns the message and true when found.
func (a *OpenCodeAdapter) findAssistantByParentID(ctx context.Context, sessionID string, parentID string) (*NativeMessage, bool) {
	client, err := a.clientFor(adapter.SessionID(sessionID))
	if err != nil {
		return nil, false
	}
	msgs, err := client.ListMessages(ctx, sessionID)
	if err != nil {
		return nil, false
	}
	for i := range msgs {
		m := &msgs[i]
		if m.Role == "assistant" && m.ParentID == parentID {
			return m, true
		}
	}
	return nil, false
}

// ── Internal helpers ────────────────────────────────────────────────────

// clientFor returns an authenticated typed client for the session's serve
// child.
func (a *OpenCodeAdapter) clientFor(sessionID adapter.SessionID) (*NativeClient, error) {
	if a.servers == nil {
		return nil, errors.New("no server manager wired")
	}
	sp, err := a.servers.child(string(sessionID))
	if err != nil {
		return nil, err
	}
	return newNativeClient(sp.endpoint, sp.username, sp.password), nil
}

// clientForOrResume resolves the client for a session, launching the
// session's serve child when it is not running (including resuming a
// parked child, which start verifies against the persisted native
// session). The launch context is detached from the caller: it must not
// carry the caller's deadline, because the executor binds the child
// process lifetime to it — a canceled launch context would kill a
// healthy child. Launch duration is bounded by healthTimeout inside
// start; the child's lifetime is owned by the server manager.
func (a *OpenCodeAdapter) clientForOrResume(ctx context.Context, sessionID adapter.SessionID) (*NativeClient, error) {
	client, err := a.clientFor(sessionID)
	if err == nil {
		return client, nil
	}
	if a.servers == nil {
		return nil, err
	}
	if _, startErr := a.servers.start(context.WithoutCancel(ctx), sessionID); startErr != nil {
		return nil, fmt.Errorf("start serve child for %s: %w", sessionID, startErr)
	}
	return a.clientFor(sessionID)
}

// cancelIdleParkTimer stops any pending idle park for the session.
func (a *OpenCodeAdapter) cancelIdleParkTimer(sessionID adapter.SessionID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if timer, ok := a.parkTimers[sessionID]; ok {
		timer.Stop()
		delete(a.parkTimers, sessionID)
	}
}

// scheduleIdleParkIfIdle parks the session's serve child after the idle
// grace period when every turn for the session has reached a terminal
// outcome and none is in flight.
func (a *OpenCodeAdapter) scheduleIdleParkIfIdle(sessionID adapter.SessionID) {
	if a.idleGrace <= 0 || a.servers == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for ref, d := range a.dispatches {
		if ref.SessionID == sessionID && !d.terminal {
			return
		}
	}
	if _, parked := a.parkTimers[sessionID]; parked {
		return
	}
	a.parkTimers[sessionID] = time.AfterFunc(a.idleGrace, func() {
		a.mu.Lock()
		delete(a.parkTimers, sessionID)
		for ref, d := range a.dispatches {
			if ref.SessionID == sessionID && !d.terminal {
				a.mu.Unlock()
				return
			}
		}
		a.mu.Unlock()
		parkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.servers.park(parkCtx, sessionID)
	})
}
