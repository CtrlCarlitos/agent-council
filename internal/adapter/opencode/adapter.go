package opencode

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	verdicts   map[adapter.TurnRef]adapter.DispatchStatus
	bindings   map[adapter.SessionID]*sessionBinding
	pumps      map[string]*sessionPump // native session ID → event pump
	parkTimers map[adapter.SessionID]*time.Timer
	slots      map[string]*sessionSlot // native session ID → single-flight slot
}

// sessionSlot enforces the per-NATIVE-SESSION single-flight rule: at most
// one dispatched turn may be in flight on a native session at a time,
// regardless of how many TurnRefs map to it. The owning ref may re-enter
// (retry after ambiguity). The channel is closed to release the slot.
type sessionSlot struct {
	released chan struct{}
	owner    adapter.TurnRef
}

// sessionBinding records the server-assigned native session for a logical
// session, established by CreateSession and re-established by
// ResumeSession. The native ID is never synthesized by the adapter.
type sessionBinding struct {
	nativeID    string
	model       string
	directory   string              // expected project directory of the native session
	contributor council.Contributor // bound contributor identity
	tooling     []string            // bound tooling allowlist
}

// parseNativeModel splits the frozen "provider/model" identifier into the
// structured native model reference and rejects malformed identifiers.
func parseNativeModel(model string) (NativeModel, error) {
	provider, id, ok := strings.Cut(model, "/")
	provider, id = strings.TrimSpace(provider), strings.TrimSpace(id)
	if !ok || provider == "" || id == "" || strings.Contains(provider, "/") || strings.Contains(id, "/") {
		return NativeModel{}, fmt.Errorf(
			"model identifier %q is malformed; expected provider/model for the native session", model)
	}
	return NativeModel{ProviderID: provider, ID: id}, nil
}

// nativeAgentPreset is the agent preset Council requests for contributor
// sessions. It is a fixed preset, never derived from profile tooling or
// environment allowlists (those are not tool-approval authorities).
const nativeAgentPreset = "council"

// ErrNativeSessionMissing reports a persisted binding whose native session
// no longer exists on the harness server (verified 404). Distinct from
// ErrSessionCreationUncertain: the binding is missing, not uncertain.
type ErrNativeSessionMissing struct {
	SessionID       adapter.SessionID
	NativeSessionID string
}

func (e *ErrNativeSessionMissing) Error() string {
	return fmt.Sprintf("native session %s is missing for session %s", e.NativeSessionID, e.SessionID)
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
		verdicts:      make(map[adapter.TurnRef]adapter.DispatchStatus),
		bindings:      make(map[adapter.SessionID]*sessionBinding),
		pumps:         make(map[string]*sessionPump),
		parkTimers:    make(map[adapter.SessionID]*time.Timer),
		slots:         make(map[string]*sessionSlot),
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
	a.servers.nativeFor = a.resolveNativeID
	return a
}

// resolveNativeID returns the persisted native session for a child key,
// or the key itself when no binding exists (direct-dispatch paths).
func (a *OpenCodeAdapter) resolveNativeID(sessionID adapter.SessionID) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b, ok := a.bindings[sessionID]; ok {
		return b.nativeID
	}
	return string(sessionID)
}

// ── Probe ───────────────────────────────────────────────────────────────

// ── CreateSession ───────────────────────────────────────────────────────

func (a *OpenCodeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}

	binding := adapter.SessionBinding{
		SessionID:   req.SessionID,
		Contributor: req.Contributor,
		Config:      req.Config,
	}

	model := strings.TrimSpace(req.Config.Model)
	nativeModel, err := parseNativeModel(model)
	if err != nil {
		return adapter.SessionBinding{}, err
	}

	// Idempotent ONLY on matching configuration: contributor, model,
	// workspace, and tooling must all match the existing binding — a
	// mismatched duplicate is rejected, never relabelled onto the old
	// native session.
	a.mu.Lock()
	if existing, ok := a.bindings[req.SessionID]; ok {
		mismatch := existing.contributor != req.Contributor ||
			existing.model != model ||
			existing.directory != req.Config.WorkspaceRoot ||
			!slices.Equal(existing.tooling, req.Config.Tooling)
		a.mu.Unlock()
		if mismatch {
			return adapter.SessionBinding{}, fmt.Errorf(
				"session %s is already bound with a different configuration; duplicate create fails closed",
				req.SessionID)
		}
		return adapter.SessionBinding{
			SessionID:       req.SessionID,
			Contributor:     req.Contributor,
			NativeSessionID: existing.nativeID,
			Config:          req.Config,
		}, nil
	}
	a.mu.Unlock()

	// Model validation fails closed BEFORE any native resource is
	// created: the configured provider/model must be present in the
	// credential-free model inventory.
	client, err := a.clientForOrResume(ctx, req.SessionID)
	if err != nil {
		return adapter.SessionBinding{}, &adapter.ErrSessionCreationUncertain{
			SessionID: req.SessionID, Contributor: req.Contributor, Err: err,
		}
	}
	models, err := client.Models(ctx)
	if err != nil {
		return adapter.SessionBinding{}, &adapter.ErrSessionCreationUncertain{
			SessionID: req.SessionID, Contributor: req.Contributor, Err: err,
		}
	}
	modelKnown := false
	for _, m := range models {
		if m == model {
			modelKnown = true
			break
		}
	}
	if model == "" || !modelKnown {
		return adapter.SessionBinding{}, fmt.Errorf(
			"model %q is not present in the native model inventory; session creation fails closed", model)
	}

	// The native session ID is assigned by the server, never synthesized.
	// The payload carries the frozen harness configuration: selected
	// model, council agent preset, deny-by-default permissions, and the
	// workspace directory context.
	nativeID, err := client.CreateSession(ctx, NativeCreateSession{
		Title:      fmt.Sprintf("council %s", req.SessionID),
		Directory:  req.Config.WorkspaceRoot,
		Model:      nativeModel,
		Agent:      nativeAgentPreset,
		Permission: "deny",
	})
	if err != nil {
		if IsPostWriteError(err) {
			return adapter.SessionBinding{}, &adapter.ErrSessionCreationUncertain{
				SessionID: req.SessionID, Contributor: req.Contributor, Err: err,
			}
		}
		return adapter.SessionBinding{}, err
	}

	a.mu.Lock()
	a.bindings[req.SessionID] = &sessionBinding{
		nativeID:    nativeID,
		model:       model,
		directory:   req.Config.WorkspaceRoot,
		contributor: req.Contributor,
		tooling:     append([]string(nil), req.Config.Tooling...),
	}
	a.mu.Unlock()
	binding.NativeSessionID = nativeID
	return binding, nil
}

// ── ResumeSession ───────────────────────────────────────────────────────

func (a *OpenCodeAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	if strings.TrimSpace(binding.NativeSessionID) == "" {
		return fmt.Errorf("resume requires a persisted native session binding")
	}
	client, err := a.clientForOrResume(ctx, binding.SessionID)
	if err != nil {
		return err
	}
	meta, exists, err := client.GetSession(ctx, binding.NativeSessionID)
	if err != nil {
		return err
	}
	if !exists {
		return &ErrNativeSessionMissing{
			SessionID:       binding.SessionID,
			NativeSessionID: binding.NativeSessionID,
		}
	}
	// Fail closed on project-context mismatch: a native session that
	// belongs to a different directory is not this session's runtime.
	if expected := strings.TrimSpace(binding.Config.WorkspaceRoot); expected != "" &&
		strings.TrimSpace(meta.Directory) != expected {
		return fmt.Errorf("native session %s belongs to directory %q, expected %q; resume fails closed",
			binding.NativeSessionID, meta.Directory, expected)
	}
	a.mu.Lock()
	a.bindings[binding.SessionID] = &sessionBinding{
		nativeID:  binding.NativeSessionID,
		model:     binding.Config.Model,
		directory: binding.Config.WorkspaceRoot,
	}
	a.mu.Unlock()
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
		a.recordVerdict(ref, adapter.DispatchRejected)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	a.cancelIdleParkTimer(ref.SessionID)

	client, err := a.clientForOrResume(ctx, ref.SessionID)
	if err != nil {
		a.recordVerdict(ref, adapter.DispatchRejected)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	// The native session comes from the persisted binding when one
	// exists; direct-dispatch (fixture/embedded) paths address the child
	// session directly.
	nativeID := string(ref.SessionID)
	a.mu.Lock()
	if b, ok := a.bindings[ref.SessionID]; ok {
		nativeID = b.nativeID
	}
	a.mu.Unlock()

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

	// Per-native-session single-flight, launcher-only: a second turn
	// bound to the same native session waits until the active turn
	// reaches a terminal outcome or its dispatch fails before
	// acceptance.
	if err := a.acquireNativeSlot(ctx, ref, nativeID); err != nil {
		a.mu.Lock()
		delete(a.launching, ref)
		a.mu.Unlock()
		close(res.ready)
		a.recordVerdict(ref, adapter.DispatchRejected)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}
	nativeAccepted := false

	outcome, err := a.dispatchNative(ctx, ref, client, nativeID, msgID, prompt)
	// Pre-acceptance failure releases the native slot; an accepted or
	// ambiguous dispatch holds it until the turn reaches a terminal
	// outcome (Collect) or reconciliation resolves it.
	releaseIfNotAccepted := func() {
		if !nativeAccepted && outcome.Status == adapter.DispatchRejected {
			a.releaseNativeSlot(nativeID)
		}
	}

	a.mu.Lock()
	delete(a.launching, ref)
	a.verdicts[ref] = outcome.Status
	switch outcome.Status {
	case adapter.DispatchAccepted:
		nativeAccepted = true
		delete(a.unknown, ref)
		a.dispatches[ref] = &managedDispatch{userMessageID: msgID, nativeSessionID: nativeID}
		// Start the session pump loop now; taps are registered by Observe
		// callers only, so no event is ever delivered to an orphaned
		// buffer.
		pump := a.ensurePumpLocked(nativeID, client)
		pump.start()
	case adapter.DispatchUnknown:
		a.unknown[ref] = msgID
	}
	a.mu.Unlock()

	res.outcome, res.err = outcome, err
	close(res.ready)
	releaseIfNotAccepted()
	return outcome, err
}

// dispatchNative performs the authenticated POST for one turn attempt. For a
// ref with a prior ambiguous attempt it first verifies by GET-by-message-ID:
// only a verified 404 authorizes resubmission; an already-recorded message is
// accepted without a second prompt_async.
func (a *OpenCodeAdapter) dispatchNative(ctx context.Context, ref adapter.TurnRef, client *NativeClient, nativeID, msgID, prompt string) (adapter.DispatchOutcome, error) {
	a.mu.Lock()
	_, priorUnknown := a.unknown[ref]
	a.mu.Unlock()

	if priorUnknown {
		_, recorded, verifyErr := client.GetMessage(ctx, nativeID, msgID)
		if verifyErr != nil {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown, Reason: "retry verification failed: " + verifyErr.Error()}, verifyErr
		}
		if recorded {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted, Reason: "message already recorded; resubmission skipped"}, nil
		}
	}

	if err := client.PromptAsync(ctx, nativeID, msgID, prompt); err != nil {
		if IsPostWriteError(err) {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown, Reason: "post-write transport failure: " + err.Error()}, err
		}
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}
	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
}

// ── Observe ─────────────────────────────────────────────────────────────

// Observe attaches a buffered tap to the session's adapter-owned event
// pump. Cancelling ctx detaches the tap only: the native stream keeps
// draining and events keep flowing into the pump's turn registry.
func (a *OpenCodeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	a.mu.Lock()
	d, ok := a.dispatches[ref]
	var pump *sessionPump
	if ok {
		pump = a.pumps[d.nativeSessionID]
	}
	a.mu.Unlock()
	if !ok {
		return nil, errors.New("turn not dispatched")
	}
	if pump == nil {
		client, err := a.clientFor(ref.SessionID)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		pump = a.ensurePumpLocked(d.nativeSessionID, client)
		a.mu.Unlock()
	}
	stream := adapter.NewBufferedStream(ref, 64)
	tap := &turnTap{ref: ref, stream: stream, owner: a, done: ctx.Done()}
	pump.register(d.userMessageID, tap)

	// Detach on caller cancellation: the tap ends, the native stream and
	// the pump continue. A nil Done channel (caller passed a context
	// without cancellation) simply never detaches.
	if tap.done != nil {
		go func() {
			<-tap.done
			pump.detach(d.userMessageID)
		}()
	}
	return stream, nil
}

// ── Cancel ──────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	client, err := a.clientFor(ref.SessionID)
	if err != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown, Reason: err.Error()}, err
	}
	// Abort addresses the NATIVE session of the dispatched turn.
	nativeID := string(ref.SessionID)
	a.mu.Lock()
	if d, ok := a.dispatches[ref]; ok {
		nativeID = d.nativeSessionID
	}
	a.mu.Unlock()
	if nativeID == string(ref.SessionID) {
		nativeID = a.resolveNativeID(ref.SessionID)
	}
	if err := client.Abort(ctx, nativeID); err != nil {
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
	// The message query addresses the NATIVE session from the dispatch
	// record; the child lookup stays keyed by the logical session.
	msg, found := a.findAssistantByParentID(ctx, ref.SessionID, d.nativeSessionID, d.userMessageID)
	if !found {
		return adapter.TurnResult{
			Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultPending,
		}, nil
	}
	d.terminal = true
	a.releaseNativeSlotFor(d.nativeSessionID, ref)
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

// Reconcile applies the spec §3.5 evidence rules, correlating the exact
// native message — never the bare session: a verified assistant reply
// (parentID == deterministic user-message ID) is a terminal outcome; a
// verified live submission is reachable-active; transport loss, 404 after
// a possible acceptance, and ambiguous idle are uncertain; a recorded
// pre-acceptance failure with the message provably absent is
// definitively missing.
func (a *OpenCodeAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	a.mu.Lock()
	verdict := a.verdicts[ref.TurnRef]
	msgID := ""
	if d, ok := a.dispatches[ref.TurnRef]; ok {
		msgID = d.userMessageID
	}
	if msgID == "" {
		msgID = a.unknown[ref.TurnRef]
	}
	nativeID := string(ref.TurnRef.SessionID)
	if b, ok := a.bindings[ref.TurnRef.SessionID]; ok {
		nativeID = b.nativeID
	}
	a.mu.Unlock()

	// Positive pre-acceptance evidence: the dispatch was rejected before
	// the server could accept it.
	neverAccepted := verdict == adapter.DispatchRejected
	definitivelyMissing := func() adapter.ReconciliationOutcome {
		return adapter.ReconciliationOutcome{
			Ref: ref, Reachability: council.VisibilityReachable,
			Status: adapter.ReconciliationDefinitivelyMissing, Observed: council.TurnFailed,
		}
	}
	uncertain := func() adapter.ReconciliationOutcome {
		return adapter.ReconciliationOutcome{
			Ref: ref, Reachability: council.VisibilityHostLost,
			Status: adapter.ReconciliationUncertain, Observed: council.TurnRunning,
		}
	}

	client, err := a.clientFor(ref.TurnRef.SessionID)
	if err != nil {
		if neverAccepted {
			return definitivelyMissing(), nil
		}
		return uncertain(), nil
	}

	_, exists, err := client.GetSession(ctx, nativeID)
	if err != nil {
		// Transport loss to the server: 404-proof is unavailable, and a
		// lost connection proves nothing about an orphan worker's work.
		if neverAccepted {
			return definitivelyMissing(), nil
		}
		return uncertain(), nil
	}
	if !exists {
		// Session 404 after a possibly-accepted dispatch proves nothing
		// about the work.
		if neverAccepted {
			return definitivelyMissing(), nil
		}
		return uncertain(), nil
	}

	if msgID == "" {
		// Ambiguous idle: nothing correlates this turn to native state.
		return uncertain(), nil
	}

	_, found, err := client.GetMessage(ctx, nativeID, msgID)
	if err != nil {
		if neverAccepted {
			return definitivelyMissing(), nil
		}
		return uncertain(), nil
	}
	if !found {
		if neverAccepted {
			return definitivelyMissing(), nil
		}
		return uncertain(), nil
	}

	// The submission is on the server: answered? A transport failure
	// here establishes neither reachability nor activity — the evidence
	// model requires uncertainty.
	msgs, err := client.ListMessages(ctx, nativeID)
	if err != nil {
		return uncertain(), nil
	}
	for i := range msgs {
		m := &msgs[i]
		if m.Role == "assistant" && m.ParentID == msgID {
			observed := council.TurnCompleted
			result := ""
			for _, part := range m.Parts {
				if part.Type == "text" {
					result = part.Text
					break
				}
			}
			if m.Error != nil {
				observed = council.TurnFailed
				result = m.Error.Message
			}
			// Verified terminal evidence releases the owning dispatch
			// slot even without a Collect call.
			a.markTurnTerminal(nativeID, msgID)
			return adapter.ReconciliationOutcome{
				Ref: ref, Reachability: council.VisibilityReachable,
				Status:   adapter.ReconciliationReachableTerminal,
				Observed: observed, Result: result,
			}, nil
		}
	}
	return adapter.ReconciliationOutcome{
		Ref: ref, Reachability: council.VisibilityReachable,
		Status: adapter.ReconciliationReachableActive, Observed: council.TurnRunning,
	}, nil
}

// findAssistantByParentID polls the server for an assistant message whose
// parentID matches the given ID. Returns the message and true when found.
func (a *OpenCodeAdapter) findAssistantByParentID(ctx context.Context, childKey adapter.SessionID, nativeSessionID, parentID string) (*NativeMessage, bool) {
	client, err := a.clientFor(childKey)
	if err != nil {
		return nil, false
	}
	msgs, err := client.ListMessages(ctx, nativeSessionID)
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

// acquireNativeSlot blocks until the native session's dispatch slot is
// free, then holds it for the given ref. The slot's owner re-enters
// without blocking (retry after ambiguity). Fails on caller cancellation.
func (a *OpenCodeAdapter) acquireNativeSlot(ctx context.Context, ref adapter.TurnRef, nativeID string) error {
	for {
		a.mu.Lock()
		if a.slots == nil {
			a.slots = make(map[string]*sessionSlot)
		}
		slot, held := a.slots[nativeID]
		if !held {
			a.slots[nativeID] = &sessionSlot{released: make(chan struct{}), owner: ref}
			a.mu.Unlock()
			return nil
		}
		if slot.owner == ref {
			a.mu.Unlock()
			return nil
		}
		released := slot.released
		a.mu.Unlock()
		select {
		case <-released:
			// freed; loop to re-acquire
		case <-ctx.Done():
			return fmt.Errorf("dispatch slot wait cancelled for native session %s: %w", nativeID, ctx.Err())
		}
	}
}

// releaseNativeSlot frees the native session's dispatch slot, if held.
func (a *OpenCodeAdapter) releaseNativeSlot(nativeID string) {
	a.mu.Lock()
	slot, ok := a.slots[nativeID]
	if ok {
		delete(a.slots, nativeID)
	}
	a.mu.Unlock()
	if ok {
		close(slot.released)
	}
}

// releaseNativeSlotFor frees the slot only when the given ref owns it.
func (a *OpenCodeAdapter) releaseNativeSlotFor(nativeID string, ref adapter.TurnRef) {
	a.mu.Lock()
	slot, ok := a.slots[nativeID]
	a.mu.Unlock()
	if ok && slot.owner == ref {
		a.releaseNativeSlot(nativeID)
	}
}

// markTurnTerminal records verified terminal evidence for a turn and
// releases its native-session slot: the native side is conclusively done,
// so a follow-up dispatch must not wait.
func (a *OpenCodeAdapter) markTurnTerminal(nativeID, userMessageID string) {
	a.mu.Lock()
	var ref adapter.TurnRef
	found := false
	for r, d := range a.dispatches {
		if d.nativeSessionID == nativeID && d.userMessageID == userMessageID {
			d.terminal = true
			ref = r
			found = true
		}
	}
	a.mu.Unlock()
	if found {
		a.releaseNativeSlotFor(nativeID, ref)
	}
}

// recordVerdict stores the latest dispatch verdict for reconciliation
// evidence rules.
func (a *OpenCodeAdapter) recordVerdict(ref adapter.TurnRef, status adapter.DispatchStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.verdicts[ref] = status
}

// ensurePumpLocked returns the pump for a native session, creating it if
// absent. Callers must hold a.mu.
func (a *OpenCodeAdapter) ensurePumpLocked(nativeID string, client *NativeClient) *sessionPump {
	if p, ok := a.pumps[nativeID]; ok {
		return p
	}
	p := newSessionPump(nativeID, client)
	p.onTerminal = func(userMessageID string) {
		a.markTurnTerminal(nativeID, userMessageID)
	}
	a.pumps[nativeID] = p
	return p
}

// IdleParkedSessions reports how many sessions currently have a parked
// serve child (stopped after idle grace, resumable). Evidence helper for
// lifecycle acceptance.
func (a *OpenCodeAdapter) IdleParkedSessions() int {
	if a.servers == nil {
		return 0
	}
	a.servers.mu.Lock()
	defer a.servers.mu.Unlock()
	return len(a.servers.parked)
}

// stopPump ends the drain loop for a native session and drops its taps.
func (a *OpenCodeAdapter) stopPump(nativeID string) {
	a.mu.Lock()
	p, ok := a.pumps[nativeID]
	delete(a.pumps, nativeID)
	a.mu.Unlock()
	if ok {
		p.stop()
	}
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
		if err := a.servers.park(parkCtx, sessionID); err == nil {
			// The child is gone; its pump must not reconnect to a dead
			// endpoint. The pump is keyed by the NATIVE session ID.
			// Resume re-registers a fresh pump.
			a.stopPump(a.resolveNativeID(sessionID))
		}
	})
}
