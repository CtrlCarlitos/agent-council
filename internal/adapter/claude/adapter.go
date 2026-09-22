package claude

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
	identity     DispatchIdentitySource
	configBase   string

	mu        sync.Mutex
	turns     map[adapter.TurnRef]*turnRun
	singleFlt map[string]*claudeSlot
}

type turnRun struct {
	ref           adapter.TurnRef
	attemptID     string
	nativeID      string
	workspaceRoot string
	manifest      storage.ToolkitManifest
	universeTools []string
	stream        *adapter.BufferedStream
	proc          execpolicy.ManagedProcess
	cancel        context.CancelFunc
}

type claudeSlot struct {
	released chan struct{}
	owner    string
}

func NewClaudeAdapter(
	store *storage.Store,
	wm *workspace.WorkspaceManager,
	executor execpolicy.PolicyExecutor,
	launchSource *ClaudeTurnLaunchSource,
	identity DispatchIdentitySource,
	configBase string,
) *ClaudeAdapter {
	return &ClaudeAdapter{
		store:        store,
		wm:           wm,
		executor:     executor,
		launchSource: launchSource,
		identity:     identity,
		configBase:   configBase,
		turns:        make(map[adapter.TurnRef]*turnRun),
		singleFlt:    make(map[string]*claudeSlot),
	}
}

func sessionUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func (a *ClaudeAdapter) acquireSlot(ctx context.Context, nativeID, owner string) error {
	for {
		a.mu.Lock()
		slot, held := a.singleFlt[nativeID]
		if !held {
			a.singleFlt[nativeID] = &claudeSlot{released: make(chan struct{}), owner: owner}
			a.mu.Unlock()
			return nil
		}
		if slot.owner == owner {
			a.mu.Unlock()
			return nil
		}
		released := slot.released
		a.mu.Unlock()
		select {
		case <-released:
		case <-ctx.Done():
			return fmt.Errorf("single-flight wait cancelled: %w", ctx.Err())
		}
	}
}

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
		close(slot.released)
	}
}

// ── CreateSession ───────────────────────────────────────────────────────

func (a *ClaudeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}
	return adapter.SessionBinding{
		SessionID:   req.SessionID,
		Contributor: req.Contributor,
		Config:      req.Config,
	}, nil
}

// GenerateNativeSessionID produces a crypto/rand UUIDv4 native identity
// and materializes the per-session config root.
func (a *ClaudeAdapter) GenerateNativeSessionID(templateDir string) (string, error) {
	nativeID, err := sessionUUID()
	if err != nil {
		return "", err
	}
	if _, _, err := MaterializeConfigRoot(templateDir, a.configBase, nativeID, nativeID); err != nil {
		return "", fmt.Errorf("materialize config root: %w", err)
	}
	return nativeID, nil
}

// ── ResumeSession ───────────────────────────────────────────────────────

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

func (a *ClaudeAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	attempt, ok := a.identity.AttemptFor(ctx, ref)
	if !ok {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "no attempt identity"}, fmt.Errorf("no attempt identity for %s/%s", ref.SessionID, ref.TurnKey)
	}
	promptDigest := PromptDigest(prompt, attempt)

	binding, err := a.store.GetClaudeSessionBinding(ctx, string(ref.SessionID))
	if err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}
	if binding == nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "no claude session binding for " + string(ref.SessionID)}, nil
	}
	nativeID := binding.NativeID

	if err := a.acquireSlot(ctx, nativeID, attempt); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}
	accepted := false
	releaseIfRejected := func(stage string) {
		if !accepted {
			a.releaseSlot(nativeID, attempt)
		}
	}

	launch, err := a.launchSource.ClaudeTurnLaunch(ctx, ref.SessionID, nativeID, false, promptDigest)
	if err != nil {
		releaseIfRejected("launch source")
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}

	baseline := storage.ClaudeTurnAttempt{
		AttemptID: attempt, SessionID: string(ref.SessionID),
		TurnKey: ref.TurnKey, NativeID: nativeID, PromptDigest: promptDigest,
		BaselineMaterialized: false, TranscriptProtection: "advisory",
	}
	if err := a.store.InsertClaudeTurnAttempt(ctx, baseline); err != nil {
		releaseIfRejected("baseline")
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}

	seq, resErr := a.store.ReserveClaudeLaunch(ctx, attempt, "claude-turn")
	if resErr != nil {
		releaseIfRejected("reservation")
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: resErr.Error()}, resErr
	}

	// Adapter-owned detached context: the caller's HTTP context must
	// not kill an accepted turn.
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	proc, startErr := a.executor.Start(lifecycleCtx, launch)
	if startErr != nil {
		lifecycleCancel()
		_ = a.store.RecordClaudeLaunchState(ctx, attempt, int64(seq), "start_failed", nil)
		a.releaseSlot(nativeID, attempt)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "start failed: " + startErr.Error()}, startErr
	}
	if err := a.store.RecordClaudeLaunchState(ctx, attempt, int64(seq), "started", nil); err != nil {
		_ = proc.Terminate(lifecycleCtx)
		lifecycleCancel()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "started but could not record launch state"}, nil
	}

	go drainReader(proc.Stderr())

	stdin := NewStdinWriter(proc.Stdin())
	if stdinErr := stdin.WritePrompt([]byte(prompt)); stdinErr != nil {
		_ = proc.Terminate(lifecycleCtx)
		lifecycleCancel()
		a.releaseSlot(nativeID, attempt)
		if stdin.TransmissionBegan() {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
				Reason: "prompt transmission began but failed: " + stdinErr.Error()}, nil
		}
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "stdin rejected before transmission: " + stdinErr.Error()}, nil
	}
	if err := proc.Stdin().Close(); err != nil {
		lifecycleCancel()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "stdin close failed: " + err.Error()}, nil
	}

	if err := a.store.RecordClaudeStdinTransmitted(ctx, attempt, int64(seq)); err != nil {
		// Non-fatal: the turn may still succeed.
		_ = err
	}

	stream := adapter.NewBufferedStream(ref, 64)
	run := &turnRun{
		ref: ref, attemptID: attempt, nativeID: nativeID,
		workspaceRoot: launch.Paths.Root,
		manifest:      launch.Profile.ToolkitManifest.ToolkitManifest,
		universeTools: launch.UniverseTools,
		stream:        stream, proc: proc, cancel: lifecycleCancel,
	}
	a.mu.Lock()
	a.turns[ref] = run
	a.mu.Unlock()

	accepted = true
	go a.runTurn(run, lifecycleCtx, lifecycleCancel)

	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
}

func (a *ClaudeAdapter) runTurn(run *turnRun, lifecycleCtx context.Context, lifecycleCancel context.CancelFunc) {
	cfg := StreamConfig{
		WorkspaceRoot:         run.workspaceRoot,
		ExpectedVersion:       run.manifest.ProbedCLIVersion,
		ExpectedModelIdentity: "claude-haiku-4-5-20251001",
		ExpectedSessionID:     run.nativeID,
		Manifest:              run.manifest,
		UniverseTools:         run.universeTools,
		MaxLineBytes:          1 << 20,
		MaxTotalBytes:         8 << 20,
	}
	out, _ := ParseStream(run.proc.Stdout(), cfg, func(ev StreamEvent) {
		switch ev.Type {
		case EventToolDenied:
			run.stream.SendOrOverflow(adapter.Event{
				Ref: run.ref, Type: adapter.EventToolDenied,
				Status: council.TurnRunning, ApprovalID: run.attemptID,
				Payload: ev.DeniedClass + ": " + ev.ToolName + ": " + ev.Text,
			})
		case EventProgress:
			run.stream.SendOrOverflow(adapter.Event{
				Ref: run.ref, Type: adapter.EventProgress,
				Status: council.TurnRunning, Payload: ev.Text,
			})
		}
	})

	go drainReader(run.proc.Stderr())
	_, _ = run.proc.Wait()

	if out.Poisoned || !out.Terminal {
		// Uncertain: no verified terminal proof. The slot is NOT
		// released: the native session is blocked until the controller
		// disposes the uncertain attempt.
		a.mu.Lock()
		delete(a.turns, run.ref)
		a.mu.Unlock()
		run.stream.CloseWithErr(fmt.Errorf("stream ended without verified result: %s", out.PoisonReason))
		return
	}

	usageJSON := ""
	if out.ResultUsage != nil {
		usageJSON = out.ResultUsage.Raw
	}
	if err := a.store.SetClaudeAttemptTerminal(context.Background(), run.attemptID, out.ResultText, usageJSON); err != nil {
		run.stream.CloseWithErr(fmt.Errorf("terminal persistence: %w", err))
		a.releaseSlot(run.nativeID, run.attemptID)
		return
	}
	run.stream.SendOrOverflow(adapter.Event{
		Ref: run.ref, Type: adapter.EventTerminal,
		Status: council.TurnCompleted, Payload: out.ResultText,
	})
	run.stream.Close()
	a.releaseSlot(run.nativeID, run.attemptID)
}

func drainReader(r interface{ Read([]byte) (int, error) }) {
	buf := make([]byte, 4096)
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
	}
}

func (a *ClaudeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	a.mu.Lock()
	run, ok := a.turns[ref]
	var stream *adapter.BufferedStream
	if ok {
		stream = run.stream
	}
	a.mu.Unlock()
	if !ok || stream == nil {
		return nil, errors.New("turn not dispatched or already completed")
	}
	return stream, nil
}

func (a *ClaudeAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	a.mu.Lock()
	run := a.turns[ref]
	a.mu.Unlock()
	if run == nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
			Reason: "turn not dispatched"}, nil
	}
	termCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := run.proc.Terminate(termCtx); err != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
			Reason: err.Error()}, err
	}
	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
		Reason: "process terminated; no terminal result observed"}, nil
}

func (a *ClaudeAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	attempt, err := a.store.GetLatestClaudeTurnAttempt(ctx, string(ref.SessionID), ref.TurnKey)
	if err != nil {
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
			ResultStatus: adapter.ResultPending}, err
	}
	if attempt == nil {
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
			ResultStatus: adapter.ResultPending}, errors.New("turn not dispatched")
	}
	if attempt.Terminal && attempt.ResultPayload != nil {
		return adapter.TurnResult{
			Ref: ref, Status: council.TurnCompleted,
			ResultStatus: adapter.ResultAvailable,
			Output:       *attempt.ResultPayload,
			CompletedAt:  time.Now().UTC(),
		}, nil
	}
	return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
		ResultStatus: adapter.ResultPending}, nil
}

func (a *ClaudeAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	attempt, err := a.store.GetLatestClaudeTurnAttempt(ctx, string(ref.TurnRef.SessionID), ref.TurnRef.TurnKey)
	if err != nil {
		return adapter.ReconciliationOutcome{Ref: ref,
			Reachability: council.VisibilityHostLost,
			Status:       adapter.ReconciliationUncertain,
			Observed:     council.TurnRunning,
		}, nil
	}
	if attempt == nil {
		return adapter.ReconciliationOutcome{Ref: ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationDefinitivelyMissing,
			Observed:     council.TurnFailed,
		}, nil
	}
	if attempt.Terminal {
		return adapter.ReconciliationOutcome{Ref: ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableTerminal,
			Observed:     council.TurnCompleted,
			Result:       ptrDeref(attempt.ResultPayload),
		}, nil
	}
	return adapter.ReconciliationOutcome{Ref: ref,
		Reachability: council.VisibilityHostLost,
		Status:       adapter.ReconciliationUncertain,
		Observed:     council.TurnRunning,
	}, nil
}

func ptrDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
