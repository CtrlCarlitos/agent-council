package claude

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
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
	templateDir  string

	mu        sync.Mutex
	turns     map[adapter.TurnRef]*turnRun
	singleFlt map[string]*claudeSlot
}

type turnRun struct {
	ref           adapter.TurnRef
	attemptID     string
	nativeID      string
	launchSeq     int64
	firstTurn     bool
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
	templateDir string,
) *ClaudeAdapter {
	return &ClaudeAdapter{
		store:        store,
		wm:           wm,
		executor:     executor,
		launchSource: launchSource,
		identity:     identity,
		configBase:   configBase,
		templateDir:  templateDir,
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

func (a *ClaudeAdapter) removeTurn(ref adapter.TurnRef) {
	a.mu.Lock()
	delete(a.turns, ref)
	a.mu.Unlock()
}

// ── CreateSession ───────────────────────────────────────────────────────

// CreateSession generates the native UUIDv4 identity, materializes the
// per-session config root from the frozen template, and persists the
// unmaterialized binding. Duplicate requests return the existing
// binding idempotently. No native call is made: materialization is
// local, and the binding stays materialized=false until a verified
// first-turn result proves the native session exists.
func (a *ClaudeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}
	if existing, err := a.store.GetClaudeSessionBinding(ctx, string(req.SessionID)); err != nil {
		return adapter.SessionBinding{}, err
	} else if existing != nil {
		return a.existingBinding(req, existing), nil
	}

	runID, err := a.store.GetSessionRunID(ctx, string(req.SessionID))
	if err != nil {
		return adapter.SessionBinding{}, fmt.Errorf("session run lookup: %w", err)
	}
	meta, err := a.store.GetSessionMetadata(ctx, string(req.SessionID))
	if err != nil {
		return adapter.SessionBinding{}, fmt.Errorf("session metadata lookup: %w", err)
	}
	if meta.Contributor != "claude" {
		return adapter.SessionBinding{}, fmt.Errorf(
			"session %s contributor is %q, not claude; refusing to create a Claude binding", req.SessionID, meta.Contributor)
	}

	nativeID, err := sessionUUID()
	if err != nil {
		return adapter.SessionBinding{}, err
	}
	root, templateDigest, err := MaterializeConfigRoot(a.templateDir, a.configBase, runID, string(req.SessionID))
	if err != nil {
		return adapter.SessionBinding{}, fmt.Errorf("materialize config root: %w", err)
	}
	if err := a.store.InsertClaudeSessionBinding(ctx, storage.ClaudeSessionBinding{
		SessionID:      string(req.SessionID),
		NativeID:       nativeID,
		Model:          req.Config.Model,
		Workspace:      req.Config.WorkspaceRoot,
		ConfigRoot:     root,
		TemplateDigest: templateDigest,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		// Lost a creation race: the winner's binding is authoritative.
		if existing, gerr := a.store.GetClaudeSessionBinding(ctx, string(req.SessionID)); gerr == nil && existing != nil {
			return a.existingBinding(req, existing), nil
		}
		return adapter.SessionBinding{}, fmt.Errorf("insert claude session binding: %w", err)
	}
	return adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: nativeID,
		Config:          req.Config,
	}, nil
}

func (a *ClaudeAdapter) existingBinding(req adapter.CreateSessionRequest, existing *storage.ClaudeSessionBinding) adapter.SessionBinding {
	return adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: existing.NativeID,
		Config:          req.Config,
	}
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
	releaseIfRejected := func() {
		if !accepted {
			a.releaseSlot(nativeID, attempt)
		}
	}

	// The binding's materialization state selects the native identity
	// flag: an unmaterialized binding is a first turn (--session-id); a
	// materialized one resumes the proven native session (--resume).
	firstTurn := !binding.Materialized
	launch, err := a.launchSource.ClaudeTurnLaunch(ctx, ref.SessionID, nativeID, firstTurn, promptDigest)
	if err != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}

	baseline := storage.ClaudeTurnAttempt{
		AttemptID: attempt, SessionID: string(ref.SessionID),
		TurnKey: ref.TurnKey, NativeID: nativeID, PromptDigest: promptDigest,
		BaselineMaterialized: false, TranscriptProtection: "advisory",
	}
	if err := a.store.InsertClaudeTurnAttempt(ctx, baseline); err != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}

	seq, resErr := a.store.ReserveClaudeLaunch(ctx, attempt, "claude-turn")
	if resErr != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: resErr.Error()}, resErr
	}

	// Adapter-owned detached context: the caller's HTTP context must
	// not kill an accepted turn.
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	proc, startErr := a.executor.Start(lifecycleCtx, launch)
	if startErr != nil {
		lifecycleCancel()
		if recErr := a.store.RecordClaudeLaunchState(ctx, attempt, seq, "start_failed", nil); recErr != nil {
			// The reservation evidence could not be closed out: the
			// outcome is not provably pre-acceptance.
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
				Reason: "start failed and launch state could not be recorded: " + recErr.Error()}, recErr
		}
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "start failed: " + startErr.Error()}, startErr
	}
	if err := a.store.RecordClaudeLaunchState(ctx, attempt, seq, "started", nil); err != nil {
		_ = proc.Terminate(lifecycleCtx)
		lifecycleCancel()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "started but could not record launch state"}, nil
	}

	// stderr is drained exactly once, for the process lifetime.
	go drainReader(proc.Stderr())

	// The prompt crosses the stdin boundary through a writer that
	// persists the first-byte transmission evidence at the write
	// boundary itself (spec §3.11 step 4).
	bw := &boundaryWriter{w: proc.Stdin(), store: a.store, ctx: ctx, attemptID: attempt, seq: seq}
	stdin := NewStdinWriter(bw)
	if stdinErr := stdin.WritePrompt([]byte(prompt)); stdinErr != nil {
		_ = proc.Terminate(lifecycleCtx)
		lifecycleCancel()
		_ = a.store.RecordClaudeLaunchState(ctx, attempt, seq, "dead", nil)
		if stdin.TransmissionBegan() {
			// Ambiguous: the native session is blocked until
			// disposition. The slot is NOT released.
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
				Reason: "prompt transmission began but failed: " + stdinErr.Error()}, nil
		}
		// Pre-transmission: no prompt byte reached the process, a safe
		// pre-acceptance rejection.
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "stdin rejected before transmission: " + stdinErr.Error()}, nil
	}
	if bwErr := bw.boundaryError(); bwErr != nil {
		_ = proc.Terminate(lifecycleCtx)
		lifecycleCancel()
		_ = a.store.RecordClaudeLaunchState(ctx, attempt, seq, "dead", nil)
		// The boundary evidence could not be persisted: the outcome
		// cannot be classified. Slot retained.
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "first-byte transmission boundary could not be persisted: " + bwErr.Error()}, nil
	}
	if err := proc.Stdin().Close(); err != nil {
		_ = proc.Terminate(lifecycleCtx)
		lifecycleCancel()
		// The full prompt was written; a close failure is ambiguous.
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "stdin close failed: " + err.Error()}, nil
	}

	stream := adapter.NewBufferedStream(ref, 64)
	run := &turnRun{
		ref: ref, attemptID: attempt, nativeID: nativeID, launchSeq: seq,
		firstTurn:     firstTurn,
		workspaceRoot: launch.Paths.Root,
		manifest:      launch.Profile.ToolkitManifest.ToolkitManifest,
		universeTools: launch.UniverseTools,
		stream:        stream, proc: proc, cancel: lifecycleCancel,
	}
	a.mu.Lock()
	a.turns[ref] = run
	a.mu.Unlock()

	accepted = true
	go a.runTurn(run)

	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
}

func (a *ClaudeAdapter) runTurn(run *turnRun) {
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

	exitCode, _ := run.proc.Wait()
	_ = a.store.RecordClaudeLaunchState(context.Background(), run.attemptID, run.launchSeq, "dead", &exitCode)

	if out.Poisoned || !out.Terminal {
		// Uncertain: no verified terminal proof. The slot is NOT
		// released: the native session is blocked until the controller
		// disposes the uncertain attempt.
		a.removeTurn(run.ref)
		run.stream.CloseWithErr(fmt.Errorf("stream ended without verified result: %s", out.PoisonReason))
		return
	}

	// A verified result is terminal in both directions: success maps to
	// completed, a verified error result (e.g. error_max_turns) maps to
	// failed.
	status := "completed"
	turnStatus := council.TurnCompleted
	if !out.Completed {
		status = "failed"
		turnStatus = council.TurnFailed
	}
	usageJSON := ""
	if out.ResultUsage != nil {
		usageJSON = out.ResultUsage.Raw
	}
	if err := a.store.SetClaudeAttemptTerminal(context.Background(), run.attemptID, status, out.ResultText, usageJSON); err != nil {
		// Terminal persistence failed: the outcome cannot be resolved,
		// so the attempt stays uncertain and the slot is retained.
		a.removeTurn(run.ref)
		run.stream.CloseWithErr(fmt.Errorf("terminal persistence: %w", err))
		return
	}
	if run.firstTurn {
		if err := a.store.MarkClaudeSessionMaterialized(context.Background(), string(run.ref.SessionID)); err != nil {
			a.removeTurn(run.ref)
			run.stream.SendOrOverflow(adapter.Event{
				Ref: run.ref, Type: adapter.EventTerminal,
				Status: turnStatus, Payload: out.ResultText,
			})
			run.stream.CloseWithErr(fmt.Errorf("mark session materialized: %w", err))
			a.releaseSlot(run.nativeID, run.attemptID)
			return
		}
	}
	a.removeTurn(run.ref)
	run.stream.SendOrOverflow(adapter.Event{
		Ref: run.ref, Type: adapter.EventTerminal,
		Status: turnStatus, Payload: out.ResultText,
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

// boundaryWriter persists the first-byte transmission boundary at the
// write boundary. A storage failure is captured and surfaced by the
// dispatcher — it is durable ambiguity evidence, never dropped.
type boundaryWriter struct {
	w         io.Writer
	store     *storage.Store
	ctx       context.Context
	attemptID string
	seq       int64

	recorded    atomic.Bool
	recordError error
}

func (b *boundaryWriter) Write(p []byte) (int, error) {
	n, werr := b.w.Write(p)
	if n > 0 && b.recorded.CompareAndSwap(false, true) {
		if rerr := b.store.RecordClaudeStdinTransmitted(b.ctx, b.attemptID, b.seq); rerr != nil {
			b.recordError = rerr
		}
	}
	return n, werr
}

func (b *boundaryWriter) boundaryError() error { return b.recordError }

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
		status := council.TurnCompleted
		resultStatus := adapter.ResultAvailable
		if attempt.ObservedStatus == "failed" {
			status = council.TurnFailed
			resultStatus = adapter.ResultFailed
		}
		return adapter.TurnResult{
			Ref: ref, Status: status,
			ResultStatus: resultStatus,
			Output:       *attempt.ResultPayload,
			CompletedAt:  time.Now().UTC(),
		}, nil
	}
	return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
		ResultStatus: adapter.ResultPending}, nil
}

// Reconcile classifies a recovered turn from durable evidence only
// (spec §3.10): a live in-process run is reachable-active; a verified
// terminal is committed (completed or failed); anything else —
// including a missing attempt row or a reserved/started/dead launch —
// stays Uncertain. Only retained start_failed rows are positive
// pre-start evidence that no process was ever created
// (DefinitivelyMissing); absence of evidence is never positive proof.
func (a *ClaudeAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	a.mu.Lock()
	_, live := a.turns[ref.TurnRef]
	a.mu.Unlock()
	if live {
		return adapter.ReconciliationOutcome{Ref: ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableActive,
			Observed:     council.TurnRunning,
		}, nil
	}

	attempt, err := a.store.GetLatestClaudeTurnAttempt(ctx, string(ref.TurnRef.SessionID), ref.TurnRef.TurnKey)
	if err != nil || attempt == nil {
		return uncertainReconciliation(ref), nil
	}
	if attempt.Terminal {
		observed := council.TurnCompleted
		if attempt.ObservedStatus == "failed" {
			observed = council.TurnFailed
		}
		return adapter.ReconciliationOutcome{Ref: ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableTerminal,
			Observed:     observed,
			Result:       ptrDeref(attempt.ResultPayload),
		}, nil
	}
	states, err := a.store.ClaudeAttemptLaunchStates(ctx, attempt.AttemptID)
	if err != nil {
		return uncertainReconciliation(ref), nil
	}
	for _, state := range states {
		if state == "reserved" || state == "started" || state == "dead" {
			return uncertainReconciliation(ref), nil
		}
	}
	if len(states) > 0 {
		return adapter.ReconciliationOutcome{Ref: ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationDefinitivelyMissing,
			Observed:     council.TurnFailed,
		}, nil
	}
	return uncertainReconciliation(ref), nil
}

func uncertainReconciliation(ref adapter.RecoveryRef) adapter.ReconciliationOutcome {
	return adapter.ReconciliationOutcome{Ref: ref,
		Reachability: council.VisibilityHostLost,
		Status:       adapter.ReconciliationUncertain,
		Observed:     council.TurnRunning,
	}
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
