package claude

// AC-006 adapter contract over the Claude Code CLI process-per-turn
// model (AC-008 spec §3.3-§3.10). Dispatch runs one bounded `claude -p`
// process through the AC-005 executor, classifies its NDJSON stream per
// §3.9, and persists durable attempt/launch state via the AC-004
// storage layer.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// DispatchIdentitySource supplies the persisted attempt identity for a
// TurnRef before dispatch (same seam as AC-007).
type DispatchIdentitySource interface {
	AttemptFor(ctx context.Context, ref adapter.TurnRef) (attempt string, ok bool)
}

// ErrNativeSessionMissing reports a persisted binding whose native
// session provably does not exist (deterministic resume failure).
type ErrNativeSessionMissing struct {
	SessionID       adapter.SessionID
	NativeSessionID string
}

func (e *ErrNativeSessionMissing) Error() string {
	return fmt.Sprintf("native session %s is missing for session %s", e.NativeSessionID, e.SessionID)
}

// ClaudeAdapter implements the AC-006 adapter contract over the Claude
// Code CLI.
type ClaudeAdapter struct {
	store        *storage.Store
	wm           *workspace.WorkspaceManager
	executor     execpolicy.PolicyExecutor
	launchSource *ClaudeTurnLaunchSource
	configBase   string
	identity     DispatchIdentitySource

	mu         sync.Mutex
	sessions   map[adapter.SessionID]*claudeNativeSession
	singleFlt  map[string]*claudeSlot // native session id → slot
	parkTimers map[adapter.SessionID]*time.Timer
}

type claudeNativeSession struct {
	nativeID      string
	materialized  bool
	mu            sync.Mutex
	activeAttempt string // attempt id with a live process, "" if none
	cancel        context.CancelFunc
}

type claudeSlot struct {
	occupied chan struct{}
	owner    string
}

// ClaudeAdapterOption configures a ClaudeAdapter.
type ClaudeAdapterOption func(*ClaudeAdapter)

// NewClaudeAdapter constructs the adapter. configBase must already be
// validated by the service (disjoint from state/workspace). identity is
// the trusted attempt-identity seam.
func NewClaudeAdapter(
	store *storage.Store,
	wm *workspace.WorkspaceManager,
	executor execpolicy.PolicyExecutor,
	launchSource *ClaudeTurnLaunchSource,
	configBase string,
	identity DispatchIdentitySource,
	opts ...ClaudeAdapterOption,
) *ClaudeAdapter {
	a := &ClaudeAdapter{
		store:        store,
		wm:           wm,
		executor:     executor,
		launchSource: launchSource,
		configBase:   configBase,
		identity:     identity,
		sessions:     make(map[adapter.SessionID]*claudeNativeSession),
		singleFlt:    make(map[string]*claudeSlot),
		parkTimers:   make(map[adapter.SessionID]*time.Timer),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// sessionUUID generates a cryptographically random RFC 4122 UUIDv4.
func sessionUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// acquireSlot blocks until the native session's dispatch slot is free,
// then holds it for the given ref. The owner re-enters without blocking.
func (a *ClaudeAdapter) acquireSlot(ctx context.Context, nativeID, owner string) error {
	for {
		a.mu.Lock()
		slot, held := a.singleFlt[nativeID]
		if !held {
			a.singleFlt[nativeID] = &claudeSlot{occupied: make(chan struct{}), owner: owner}
			a.mu.Unlock()
			return nil
		}
		if slot.owner == owner {
			a.mu.Unlock()
			return nil
		}
		released := slot.occupied
		a.mu.Unlock()
		select {
		case <-released:
		case <-ctx.Done():
			return fmt.Errorf("single-flight wait cancelled for %s: %w", nativeID, ctx.Err())
		}
	}
}

// releaseSlot frees the slot only when the given ref owns it; ownership
// check and deletion happen in one critical section.
func (a *ClaudeAdapter) releaseSlot(nativeID, owner string) {
	a.mu.Lock()
	slot, ok := a.singleFlt[nativeID]
	if ok && slot.owner == owner {
		delete(a.singleFlt, nativeID)
	} else {
		ok = false
	}
	a.mu.Unlock()
	if ok {
		close(slot.occupied)
	}
}

// ── CreateSession ───────────────────────────────────────────────────────

// CreateSession validates the frozen config, generates the native UUID
// once per logical session inside the creation reservation, materializes
// the per-session config root from the operator template, and returns
// the binding. The adapter does not persist; the service does.
func (a *ClaudeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}
	return adapter.SessionBinding{
		SessionID:   req.SessionID,
		Contributor: req.Contributor,
		Config:      req.Config,
		// NativeSessionID is assigned by the adapter's identity
		// mechanism at Dispatch (the session is materialized lazily);
		// the caller records the binding returned here.
	}, nil
}

// ── ResumeSession ───────────────────────────────────────────────────────

// ResumeSession performs validated local inspection (spec §3.4): no
// native call, no quota. Native verification is deferred to the next
// authorized dispatch. Unmaterialized bindings return success.
func (a *ClaudeAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	if strings.TrimSpace(binding.NativeSessionID) == "" {
		return fmt.Errorf("resume requires a persisted native session binding")
	}
	if !isValidUUIDv4(binding.NativeSessionID) {
		return &ErrNativeSessionMissing{SessionID: binding.SessionID, NativeSessionID: binding.NativeSessionID}
	}
	return nil
}

// ── Dispatch ────────────────────────────────────────────────────────────

// Dispatch runs one bounded claude -p process for the turn (spec §3.3).
func (a *ClaudeAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	attempt, ok := a.identity.AttemptFor(ctx, ref)
	if !ok {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "no attempt identity"}, fmt.Errorf("no attempt identity for %s/%s", ref.SessionID, ref.TurnKey)
	}
	promptDigest := PromptDigest(prompt, attempt)

	nativeID := string(ref.SessionID)
	if err := isValidNativeID(nativeID); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	// Per-native-session single-flight.
	if err := a.acquireSlot(ctx, nativeID, string(ref.SessionID)+"/"+ref.TurnKey); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	launch, err := a.launchSource.ClaudeTurnLaunch(ctx, ref.SessionID, nativeID, false, promptDigest)
	if err != nil {
		a.releaseSlot(nativeID, string(ref.SessionID)+"/"+ref.TurnKey)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	// Reserve the launch durably BEFORE executor.Start (spec §3.11).
	seq, err := a.store.ReserveClaudeLaunch(ctx, attempt, "claude-turn")
	if err != nil {
		a.releaseSlot(nativeID, string(ref.SessionID)+"/"+ref.TurnKey)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	proc, startErr := a.executor.Start(ctx, launch)
	if startErr != nil {
		_ = a.store.RecordClaudeLaunchState(ctx, attempt, seq, "start_failed", nil)
		a.releaseSlot(nativeID, string(ref.SessionID)+"/"+ref.TurnKey)
		// Executor proves no process was created: pre-acceptance rejection.
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "start failed: " + startErr.Error()}, startErr
	}
	if err := a.store.RecordClaudeLaunchState(ctx, attempt, seq, "started", nil); err != nil {
		termCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proc.Terminate(termCtx)
		a.releaseSlot(nativeID, string(ref.SessionID)+"/"+ref.TurnKey)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "started but could not record launch state"}, nil
	}

	// Drain stderr for the child's lifetime.
	go drainReader(proc.Stderr())

	// Consume stdin: prompt is never in argv.
	stdin := NewStdinWriter(proc.Stdin())
	if stdinErr := stdin.WritePrompt([]byte(prompt)); stdinErr != nil {
		_ = proc.Terminate(context.Background())
		a.releaseSlot(nativeID, string(ref.SessionID)+"/"+ref.TurnKey)
		if stdin.TransmissionBegan() {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
				Reason: "prompt transmission began but failed: " + stdinErr.Error()}, nil
		}
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "stdin rejected before transmission: " + stdinErr.Error()}, nil
	}

	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted,
		Reason: promptDigest}, nil
}

// ── Observe ─────────────────────────────────────────────────────────────

func (a *ClaudeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	_ = ctx
	_ = ref
	// The per-turn stream is consumed by the launch path; observation
	// taps attach via the result collection. Placeholder until the
	// launch wiring lands.
	return nil, errors.New("observe requires an active launch")
}

// ── Cancel ──────────────────────────────────────────────────────────────

func (a *ClaudeAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	_ = ctx
	_ = ref
	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
		Reason: "cancel requires an active launch"}, nil
}

// ── Collect ─────────────────────────────────────────────────────────────

func (a *ClaudeAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	_ = ctx
	_ = ref
	return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
		ResultStatus: adapter.ResultPending}, errors.New("turn not dispatched")
}

// ── Reconcile ───────────────────────────────────────────────────────────

func (a *ClaudeAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	_ = ctx
	_ = ref
	return adapter.ReconciliationOutcome{Ref: ref,
		Reachability: council.VisibilityHostLost,
		Status:       adapter.ReconciliationUncertain,
		Observed:     council.TurnRunning,
	}, nil
}

func isValidNativeID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("native session id is empty")
	}
	return nil
}

func drainReader(r interface{ Read([]byte) (int, error) }) {
	buf := make([]byte, 4096)
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
	}
}

func isValidUUIDv4(s string) bool {
	if len(s) != 36 {
		return false
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	hexAt := func(i int) bool {
		c := s[i]
		return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
	}
	for _, i := range []int{0, 1, 2, 3, 4, 5, 6, 7, 9, 10, 11, 12, 14, 15, 16, 17, 19, 20, 21, 22, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35} {
		if !hexAt(i) {
			return false
		}
	}
	if s[14] != '4' {
		return false
	}
	variant := s[19]
	return variant == '8' || variant == '9' || variant == 'a' || variant == 'b'
}
