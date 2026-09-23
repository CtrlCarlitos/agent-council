package claude

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
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

// ErrSessionConfigMismatch reports a CreateSession/ResumeSession caller
// whose frozen config disagrees with the session's reserved or persisted
// binding. Mismatched callers fail closed: the binding is never
// relabeled to match the request.
type ErrSessionConfigMismatch struct {
	SessionID adapter.SessionID
	Field     string
	Want      string
	Have      string
}

func (e *ErrSessionConfigMismatch) Error() string {
	return fmt.Sprintf("session %s config mismatch on %s: binding has %q, request has %q",
		e.SessionID, e.Field, e.Have, e.Want)
}

// ClaudeAdapter implements the AC-006 adapter contract over the Claude
// Code CLI. Per spec §3.3 the adapter does not persist bindings: it
// generates and materializes, the service/storage layer persists.
type ClaudeAdapter struct {
	store        *storage.Store
	wm           *workspace.WorkspaceManager
	executor     execpolicy.PolicyExecutor
	launchSource *ClaudeTurnLaunchSource
	identity     DispatchIdentitySource
	configBase   string
	templateDir  string
	// materialize is the config-root materialization step. It is
	// adapter-owned (not a package global) so tests can instrument it
	// without racing parallel production-path callers.
	materialize func(templateDir, base, runID, sessionID string) (string, string, error)
	// probeTemplate runs the operator-owned contract probes. Nil until
	// wired by NewProductionClaudeAdapter; Probe fails closed on nil.
	probeTemplate ClaudeProbeLaunchTemplate

	mu        sync.Mutex
	turns     map[adapter.TurnRef]*turnRun
	singleFlt map[string]*claudeSlot

	createMu  sync.Mutex
	creations map[adapter.SessionID]*creationCall
}

// creationCall is the per-logical-session creation reservation (§3.3):
// the caller that installs the call performs the creation; concurrent
// callers wait on done and share the outcome — the same native
// identity on success, the same typed failure otherwise.
type creationCall struct {
	done chan struct{}

	binding        adapter.SessionBinding
	err            error
	contributor    council.Contributor
	model          string
	workspace      string
	templateDigest string
}

type turnRun struct {
	ref            adapter.TurnRef
	attemptID      string
	nativeID       string
	launchSeq      int64
	firstTurn      bool
	model          string
	workspaceRoot  string
	configRoot     string
	boundWorkspace string
	manifest       storage.ToolkitManifest
	universeTools  []string
	stream         *adapter.BufferedStream
	proc           execpolicy.ManagedProcess
	cancel         context.CancelFunc
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
		materialize:  MaterializeConfigRoot,
		turns:        make(map[adapter.TurnRef]*turnRun),
		singleFlt:    make(map[string]*claudeSlot),
		creations:    make(map[adapter.SessionID]*creationCall),
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

// CreateSession validates the frozen config, generates the UUIDv4
// native ID once inside the per-logical-session creation reservation,
// materializes the per-session config root, and returns the binding
// with materialized=false. The adapter does NOT persist (§3.3): the
// service/storage layer persists the returned binding. Concurrent and
// duplicate requests share the reserved outcome — one native identity
// on success, the creator's typed failure otherwise — and mismatched
// callers fail closed.
func (a *ClaudeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}
	if strings.TrimSpace(req.Config.Model) == "" {
		return adapter.SessionBinding{}, fmt.Errorf("session %s requires a frozen model", req.SessionID)
	}
	if strings.TrimSpace(req.Config.WorkspaceRoot) == "" {
		return adapter.SessionBinding{}, fmt.Errorf("session %s requires a workspace root", req.SessionID)
	}

	a.createMu.Lock()
	if call, ok := a.creations[req.SessionID]; ok {
		a.createMu.Unlock()
		<-call.done
		if call.err != nil {
			// Shared typed failure: the waiting caller receives the
			// creator's outcome, it does not retry independently.
			return adapter.SessionBinding{}, call.err
		}
		frozenDigest, derr := TemplateDigest(a.templateDir)
		if derr != nil {
			return adapter.SessionBinding{}, fmt.Errorf("frozen template digest: %w", derr)
		}
		if err := compareConfig(req, call.contributor, call.model, call.workspace, call.templateDigest, frozenDigest); err != nil {
			return adapter.SessionBinding{}, err
		}
		return call.binding, nil
	}
	call := &creationCall{done: make(chan struct{})}
	a.creations[req.SessionID] = call
	a.createMu.Unlock()

	binding, digest, err := a.createBinding(ctx, req)
	call.binding = binding
	call.err = err
	call.contributor = req.Contributor
	call.model = req.Config.Model
	call.workspace = req.Config.WorkspaceRoot
	call.templateDigest = digest
	// Publish the outcome BEFORE retiring a failed reservation: a
	// caller arriving in the retirement window finds this call, sees
	// done closed, and shares the result instead of rerunning
	// materialization concurrently.
	close(call.done)
	if err != nil {
		a.createMu.Lock()
		if a.creations[req.SessionID] == call {
			delete(a.creations, req.SessionID)
		}
		a.createMu.Unlock()
	}
	return binding, err
}

// createBinding performs the validated creation: session metadata
// gate, persisted-binding mismatch checks, UUIDv4 generation, and
// atomic config-root materialization. No persistence happens here.
func (a *ClaudeAdapter) createBinding(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, string, error) {
	meta, err := a.store.GetSessionMetadata(ctx, string(req.SessionID))
	if err != nil {
		return adapter.SessionBinding{}, "", fmt.Errorf("session metadata lookup: %w", err)
	}
	if meta.Contributor != "claude" {
		return adapter.SessionBinding{}, "", fmt.Errorf(
			"session %s contributor is %q; refusing to create a Claude binding",
			req.SessionID, meta.Contributor)
	}

	if err := validateTemplateReservesRuntimeDirs(a.templateDir); err != nil {
		return adapter.SessionBinding{}, "", err
	}
	templateDigest, err := TemplateDigest(a.templateDir)
	if err != nil {
		return adapter.SessionBinding{}, "", fmt.Errorf("frozen template digest: %w", err)
	}

	// An already-persisted binding is authoritative; requests must
	// match its frozen config.
	if existing, err := a.store.GetClaudeSessionBinding(ctx, string(req.SessionID)); err != nil {
		return adapter.SessionBinding{}, "", err
	} else if existing != nil {
		if err := compareConfig(req, "claude", existing.Model, existing.Workspace, existing.TemplateDigest, templateDigest); err != nil {
			return adapter.SessionBinding{}, "", err
		}
		return a.reservedBinding(req, existing.NativeID), existing.TemplateDigest, nil
	}

	runID, err := a.store.GetSessionRunID(ctx, string(req.SessionID))
	if err != nil {
		return adapter.SessionBinding{}, "", fmt.Errorf("session run lookup: %w", err)
	}
	if req.Contributor != "claude" {
		return adapter.SessionBinding{}, "", &ErrSessionConfigMismatch{SessionID: req.SessionID,
			Field: "contributor", Want: string(req.Contributor), Have: "claude"}
	}
	nativeID, err := sessionUUID()
	if err != nil {
		return adapter.SessionBinding{}, "", err
	}
	_, materializedDigest, err := a.materialize(a.templateDir, a.configBase, runID, string(req.SessionID))
	if err != nil {
		return adapter.SessionBinding{}, "", fmt.Errorf("materialize config root: %w", err)
	}
	return a.reservedBinding(req, nativeID), materializedDigest, nil
}

func (a *ClaudeAdapter) reservedBinding(req adapter.CreateSessionRequest, nativeID string) adapter.SessionBinding {
	return adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: nativeID,
		Config:          req.Config,
	}
}

// compareConfig enforces §3.3's fail-closed mismatch rule against the
// reserved or persisted binding identity.
func compareConfig(req adapter.CreateSessionRequest, contributor council.Contributor, model, workspace, storedDigest, frozenDigest string) error {
	if req.Contributor != contributor {
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "contributor",
			Want: string(req.Contributor), Have: string(contributor)}
	}
	if req.Config.Model != model {
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "model",
			Want: req.Config.Model, Have: model}
	}
	if req.Config.WorkspaceRoot != workspace {
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "workspace",
			Want: req.Config.WorkspaceRoot, Have: workspace}
	}
	if storedDigest != "" && storedDigest != frozenDigest {
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "template_digest",
			Want: frozenDigest, Have: storedDigest}
	}
	return nil
}

// ── ResumeSession ───────────────────────────────────────────────────────

// ResumeSession performs validated local inspection only (§3.4); no
// native call is made and native verification is deferred to the next
// authorized dispatch. Unmaterialized bindings inspect steps 1–2 and
// succeed — they are never classified as lost. Materialized bindings
// additionally require the bound transcript to exist and pass local
// integrity checks (§3.6 derivation, §3.4 step 3's prompt-digest
// correlation lands with the §3.5 acceptance machinery).
func (a *ClaudeAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	stored, err := a.store.GetClaudeSessionBinding(ctx, string(binding.SessionID))
	if err != nil {
		return err
	}
	if stored == nil {
		return fmt.Errorf("resume requires a persisted claude session binding for %s", binding.SessionID)
	}
	if !isValidUUIDv4(stored.NativeID) {
		return &ErrNativeSessionMissing{SessionID: binding.SessionID, NativeSessionID: stored.NativeID}
	}
	if binding.NativeSessionID != stored.NativeID {
		return &ErrSessionConfigMismatch{SessionID: binding.SessionID, Field: "native_session_id",
			Want: binding.NativeSessionID, Have: stored.NativeID}
	}
	if strings.TrimSpace(binding.Config.Model) != "" && binding.Config.Model != stored.Model {
		return &ErrSessionConfigMismatch{SessionID: binding.SessionID, Field: "model",
			Want: binding.Config.Model, Have: stored.Model}
	}
	if strings.TrimSpace(binding.Config.WorkspaceRoot) != "" && binding.Config.WorkspaceRoot != stored.Workspace {
		return &ErrSessionConfigMismatch{SessionID: binding.SessionID, Field: "workspace",
			Want: binding.Config.WorkspaceRoot, Have: stored.Workspace}
	}
	templateDigest, err := TemplateDigest(a.templateDir)
	if err != nil {
		return fmt.Errorf("frozen template digest: %w", err)
	}
	if stored.TemplateDigest != templateDigest {
		return &ErrSessionConfigMismatch{SessionID: binding.SessionID, Field: "template_digest",
			Want: templateDigest, Have: stored.TemplateDigest}
	}
	if st, err := os.Stat(stored.ConfigRoot); err != nil || !st.IsDir() {
		return fmt.Errorf("per-session config root %s is missing", stored.ConfigRoot)
	}
	// The MATERIALIZED root itself must still match the frozen
	// template: mutation of the copied tree is detected here. The §3.6
	// runtime transcript subtree is excluded — transcripts are trusted
	// through their own inspection and correlation model.
	rootDigest, err := TemplateDigestExcluding(stored.ConfigRoot, []string{"projects"})
	if err != nil {
		return fmt.Errorf("materialized config root %s failed digest: %w", stored.ConfigRoot, err)
	}
	if stored.TemplateDigest != rootDigest {
		return &ErrSessionConfigMismatch{SessionID: binding.SessionID, Field: "config_root_digest",
			Want: stored.TemplateDigest, Have: rootDigest}
	}
	if stored.Materialized {
		path, err := TranscriptPath(stored.ConfigRoot, stored.Workspace, stored.NativeID)
		if err != nil {
			return err
		}
		attempts, err := a.store.ClaudeSessionTurnAttempts(ctx, string(binding.SessionID))
		if err != nil {
			return err
		}
		// §3.4 step 3: the transcript must carry the accepted user
		// entry matching the attempt records' prompt digests — proof
		// THIS transcript is THIS session's.
		return CorrelateTranscriptPrompt(path, attempts)
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

	// §3.5 blocking semantics: blocking derives from durable attempt
	// state, not process memory. An unresolved (uncertain, undisposed)
	// attempt blocks every subsequent turn on this native session —
	// including after restart — until the controller records a
	// disposition.
	blocked, err := a.store.HasClaudeUnresolvedAttempts(ctx, nativeID)
	if err != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}
	if blocked {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "native session " + nativeID + " has an unresolved attempt; blocked until the controller records a disposition"}, nil
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

	// Freeze transcript protection at baseline: an attestation matching
	// the four binding fields in force (probed CLI version, platform,
	// manifest digest, template digest) upgrades the attempt to
	// protected and freezes its id; no match means advisory. A lookup
	// FAILURE is never silently downgraded — the dispatch is rejected.
	attestationID, protection, pErr := a.resolveTranscriptProtection(ctx, launch, binding.TemplateDigest)
	if pErr != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: pErr.Error()}, pErr
	}

	baseline := storage.ClaudeTurnAttempt{
		AttemptID: attempt, SessionID: string(ref.SessionID),
		TurnKey: ref.TurnKey, NativeID: nativeID, PromptDigest: promptDigest,
		BaselineMaterialized: false, TranscriptProtection: protection,
		AttestationID: attestationID,
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
		_ = a.store.RecordClaudeLaunchState(ctx, attempt, seq, "dead", nil)
		// The process started: the attempt stays uncertain and the
		// native session is blocked until disposition.
		a.releaseSlot(nativeID, attempt)
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
		// Post-start failures are never safe rejections (§3.9): the
		// attempt stays uncertain and blocks the native session until
		// disposition. Active execution has ended, so the in-memory
		// slot is released; the durable state carries the block.
		a.releaseSlot(nativeID, attempt)
		if stdin.TransmissionBegan() {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
				Reason: "prompt transmission began but failed: " + stdinErr.Error()}, nil
		}
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "stdin failed before transmission after process start: " + stdinErr.Error()}, nil
	}
	if bwErr := bw.boundaryError(); bwErr != nil {
		_ = proc.Terminate(lifecycleCtx)
		lifecycleCancel()
		_ = a.store.RecordClaudeLaunchState(ctx, attempt, seq, "dead", nil)
		a.releaseSlot(nativeID, attempt)
		// The boundary evidence could not be persisted: the outcome
		// cannot be classified; the durable uncertainty blocks.
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "first-byte transmission boundary could not be persisted: " + bwErr.Error()}, nil
	}
	if err := proc.Stdin().Close(); err != nil {
		_ = proc.Terminate(lifecycleCtx)
		lifecycleCancel()
		a.releaseSlot(nativeID, attempt)
		// The full prompt was written; a close failure is ambiguous.
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "stdin close failed: " + err.Error()}, nil
	}

	stream := adapter.NewBufferedStream(ref, 64)
	run := &turnRun{
		ref: ref, attemptID: attempt, nativeID: nativeID, launchSeq: seq,
		firstTurn:      firstTurn,
		model:          launch.Model,
		workspaceRoot:  launch.Paths.Root,
		configRoot:     binding.ConfigRoot,
		boundWorkspace: binding.Workspace,
		manifest:       launch.Profile.ToolkitManifest.ToolkitManifest,
		universeTools:  launch.UniverseTools,
		stream:         stream, proc: proc, cancel: lifecycleCancel,
	}
	a.mu.Lock()
	a.turns[ref] = run
	a.mu.Unlock()

	accepted = true
	go a.runTurn(run)

	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
}

// runTurn owns the accepted turn through its terminal classification.
// Every exit releases the single-flight slot: the slot guards ACTIVE
// execution only, while durable attempt state carries blocking (§3.5).
func (a *ClaudeAdapter) runTurn(run *turnRun) {
	defer a.releaseSlot(run.nativeID, run.attemptID)

	cfg := StreamConfig{
		WorkspaceRoot:         run.workspaceRoot,
		ExpectedVersion:       run.manifest.ProbedCLIVersion,
		ExpectedModelIdentity: run.model,
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

	// The turn is no longer active the moment its stdout has ended:
	// later Observe calls fail and Reconcile no longer reports it as
	// reachable-active, regardless of how the verdict lands below.
	a.removeTurn(run.ref)

	if out.Poisoned || !out.Terminal {
		// Uncertain: no verified terminal proof. The attempt stays
		// uncertain and blocks the native session until the controller
		// records a disposition.
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
		// so the attempt stays uncertain and the block persists.
		run.stream.CloseWithErr(fmt.Errorf("terminal persistence: %w", err))
		return
	}
	if run.firstTurn {
		if err := a.observeAndMarkMaterialized(run); err != nil {
			run.stream.SendOrOverflow(adapter.Event{
				Ref: run.ref, Type: adapter.EventTerminal,
				Status: turnStatus, Payload: out.ResultText,
			})
			run.stream.CloseWithErr(fmt.Errorf("materialization: %w", err))
			return
		}
	}
	run.stream.SendOrOverflow(adapter.Event{
		Ref: run.ref, Type: adapter.EventTerminal,
		Status: turnStatus, Payload: out.ResultText,
	})
	run.stream.Close()
}

func drainReader(r interface{ Read([]byte) (int, error) }) {
	buf := make([]byte, 4096)
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
	}
}

// resolveTranscriptProtection selects the transcript-protection class
// for a new attempt: an attestation matching the launch's frozen
// (claude version, platform, manifest digest) and the binding's frozen
// template digest upgrades the attempt to protected with its id frozen;
// no matching attestation keeps the attempt advisory. Lookup failures
// are returned — protection is never silently downgraded by an error.
func (a *ClaudeAdapter) resolveTranscriptProtection(ctx context.Context, launch execpolicy.LaunchRequest, templateDigest string) (string, string, error) {
	id, err := a.store.FindClaudeProtectionAttestation(ctx,
		launch.Profile.ToolkitManifest.ToolkitManifest.ProbedCLIVersion,
		platformIdentity(),
		launch.ProfileDigest,
		templateDigest)
	if err != nil {
		return "", "", err
	}
	if id == "" {
		return "", "advisory", nil
	}
	return id, "protected", nil
}

// platformIdentity is the attestation binding string for the running
// platform (§3.6: os/arch).
func platformIdentity() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// observeAndMarkMaterialized enforces the §3.3 evidence boundary: the
// binding flips to materialized only after the bound transcript —
// derived from the binding's own fields — is affirmatively observed to
// exist as a regular file. A verified stdout result alone proves the
// turn outcome, never that the transcript exists or belongs to this
// session.
func (a *ClaudeAdapter) observeAndMarkMaterialized(run *turnRun) error {
	path, err := TranscriptPath(run.configRoot, run.boundWorkspace, run.nativeID)
	if err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("bound transcript was not observed after the first turn: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return fmt.Errorf("observed transcript %s is not a regular file", path)
	}
	return a.store.MarkClaudeSessionMaterialized(context.Background(), string(run.ref.SessionID))
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
