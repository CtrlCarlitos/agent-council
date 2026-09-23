package codex

// AC-006 adapter contract over the codex app-server transport (AC-009
// spec §3.4/§3.5): create/resume/dispatch with the enforceable gate
// ordering. The adapter does not persist bindings (§3.4): it generates
// and verifies, the service/storage layer persists. Task 6 (rollout
// trust + observe/cancel/collect/reconcile) completes the remaining
// surfaces; the phased stubs below fail toward the honest classification
// instead of fabricating results.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ── Typed failures ──────────────────────────────────────────────────────

// ErrProductionEligibilityMissing reports CreateSession refused because
// no valid isolation attestation is available for the frozen (codex
// version, platform, manifest) tuple (spec §3.3 ordering step 1). No
// process was started: the check is durable Council-side state evaluated
// before any child launch.
type ErrProductionEligibilityMissing struct {
	SessionID adapter.SessionID
	Reason    string
}

func (e *ErrProductionEligibilityMissing) Error() string {
	return fmt.Sprintf("production eligibility missing for session %s: %s", e.SessionID, e.Reason)
}

// ErrCodexAuthRequired reports the account/read auth gate (§3.3
// ordering step 2): the child is unauthenticated or the check was
// inconclusive (error, timeout, unknown shape — all fail closed). The
// child is terminated; no thread or turn was created.
type ErrCodexAuthRequired struct {
	SessionID adapter.SessionID
	Reason    string
	Err       error
}

func (e *ErrCodexAuthRequired) Error() string {
	return fmt.Sprintf("codex auth gate failed for session %s (%s)", e.SessionID, e.Reason)
}

func (e *ErrCodexAuthRequired) Unwrap() error { return e.Err }

// ErrProfileDrift reports a pre-transmission effective-config mismatch
// (spec §3.5): the complete thread/resume configuration disagreed with
// the frozen profile, the child was terminated, and the prompt was
// NEVER transmitted (pre-acceptance rejection).
type ErrProfileDrift struct {
	ThreadID string
	Field    string
	Want     string
	Have     string
}

func (e *ErrProfileDrift) Error() string {
	return fmt.Sprintf("codex profile drift on %s for thread %s: frozen %q, native %q",
		e.Field, e.ThreadID, e.Want, e.Have)
}

// ErrSessionConfigMismatch reports a CreateSession/ResumeSession caller
// whose frozen config disagrees with the session's persisted binding.
// Mismatched callers fail closed: the binding is never relabeled to
// match the request (AC-008 pattern).
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

// ErrObservationUnavailable is the typed phased-delivery stub for
// Observe: live turn observation (event streams, approval mirroring,
// usage) completes in Task 6 (rollout/observe). The turn itself is
// unaffected: notifications keep flowing into the pump.
var ErrObservationUnavailable = errors.New("codex turn observation is not wired yet (Task 6: rollout/observe)")

// AttestationLookup reports availability of a valid isolation
// attestation for the frozen launch tuple (spec §3.3). Production
// construction takes the lookup as a required seam; Task 9 wires the
// production lookup. A nil lookup fails closed — there is no inference
// of eligibility from absent evidence.
type AttestationLookup func() (attestationID string, ok bool)

// DispatchIdentitySource supplies the persisted attempt identity for a
// TurnRef before dispatch (same seam as AC-007/AC-008).
type DispatchIdentitySource interface {
	AttemptFor(ctx context.Context, ref adapter.TurnRef) (attempt string, ok bool)
}

// Bounds for the adapter-owned gate waits. Package-level so tests can
// shorten them; caller contexts never bound the child, pump, or gates.
var (
	authGateTimeout    = 10 * time.Second
	creationAckTimeout = 15 * time.Second
	dispatchAckTimeout = 15 * time.Second
	turnStartedGrace   = 5 * time.Second
)

// ── Adapter ─────────────────────────────────────────────────────────────

// CodexAdapter implements the AC-006 adapter contract over one
// app-server child per contributor session (spec §3.2).
type CodexAdapter struct {
	store         *storage.Store
	server        *CodexServer
	policy        CodexLaunchPolicy
	profileDigest string
	identity      DispatchIdentitySource
	attestation   AttestationLookup

	mu        sync.Mutex
	turns     map[adapter.TurnRef]*codexTurnRun
	singleFlt map[string]*codexSlot

	createMu  sync.Mutex
	creations map[adapter.SessionID]*codexCreationCall
	// uncertain is the in-adapter §3.4 tombstone: a logical session whose
	// creation ended uncertain may never be auto-retried in this process.
	// Cleared ONLY by ResolveCreationUncertainty (the service layer's
	// explicit resolution seam); durable cross-restart blocking is
	// recorded by the service journal, not here.
	uncertain map[adapter.SessionID]error
}

var _ adapter.Adapter = (*CodexAdapter)(nil)

// NewCodexAdapter wires the adapter to its durable store, child manager,
// frozen launch policy (ValidateCodexHarness output), frozen profile
// digest, attempt-identity source, and the production-eligibility
// attestation lookup (spec §3.3). A nil attestation lookup fails closed
// on CreateSession — production wiring must supply it.
func NewCodexAdapter(
	store *storage.Store,
	server *CodexServer,
	policy CodexLaunchPolicy,
	profileDigest string,
	identity DispatchIdentitySource,
	attestation AttestationLookup,
) *CodexAdapter {
	return &CodexAdapter{
		store:         store,
		server:        server,
		policy:        policy,
		profileDigest: profileDigest,
		identity:      identity,
		attestation:   attestation,
		turns:         make(map[adapter.TurnRef]*codexTurnRun),
		singleFlt:     make(map[string]*codexSlot),
		creations:     make(map[adapter.SessionID]*codexCreationCall),
		uncertain:     make(map[adapter.SessionID]error),
	}
}

type codexSlot struct {
	released chan struct{}
	owner    string
}

// codexCreationCall is the per-logical-session creation reservation
// (§3.4): the caller that installs the call performs the creation;
// concurrent callers wait on done and share the outcome — the same
// native identity on success, the creator's typed failure otherwise.
type codexCreationCall struct {
	done chan struct{}

	binding     adapter.SessionBinding
	err         error
	contributor council.Contributor
	model       string
	workspace   string
}

type codexTurnRun struct {
	ref            adapter.TurnRef
	attemptID      string
	nativeThreadID string
	launchSeq      int64
	pump           *EventPump
	tap            *TurnTap
	routeInstalled bool
}

// ── Probe ───────────────────────────────────────────────────────────────

// Probe reports design-verified capabilities with honest availability:
// the transport surfaces (resumption, cancellation, approval routing,
// streaming) are schema-verified, while version/model inventory needs
// the operator-owned probe template and stays unavailable until that
// wiring lands (§3.8 probe template; Task 9 production wiring).
func (a *CodexAdapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	return adapter.ProbeReport{
		HarnessVersion: adapter.UsageMetric[string]{Available: false},
		Capabilities: adapter.AdapterCapabilities{
			SessionResumption:    adapter.CapabilitySupported,
			MidTurnCancellation:  adapter.CapabilitySupported,
			ToolApprovalRouting:  adapter.CapabilitySupported,
			StreamingObservation: adapter.CapabilitySupported,
			StructuredOutput:     adapter.CapabilitySupported,
		},
		ModelInventory: adapter.UsageMetric[[]string]{Available: false},
	}, nil
}

// ── CreateSession ───────────────────────────────────────────────────────

// CreateSession resolves the provider-free creation path in the
// enforceable §3.4 order: isolation-attestation check (durable, pre-child —
// failure means NO process started, §3.3 step 1) → persisted-binding
// idempotency (no child needed) → child start (handshake attestation
// gate inside the server) → account/read auth gate (unauthenticated or
// inconclusive ⇒ child terminated, typed rejection, no thread or turn
// created, §3.3 step 2) → creation-reservation route → thread/start →
// id equality confirmed → binding with materialized=false. Concurrent
// duplicates share the reserved outcome; repeated identical config is
// idempotent; changed config fails closed. A creation that ended
// UNCERTAIN tombstones the logical session (§3.4: automatic recreation
// blocked): any further CreateSession for that SessionID returns the
// typed uncertain error until the service layer explicitly resolves it.
func (a *CodexAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
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
	if cause, blocked := a.uncertain[req.SessionID]; blocked {
		a.createMu.Unlock()
		// §3.4: automatic recreation is blocked after an uncertain
		// creation. The in-adapter tombstone stops retries in THIS
		// process; durable cross-restart blocking is the service
		// journal's responsibility (Task 9/10 acceptance asserts it).
		return adapter.SessionBinding{}, &adapter.ErrSessionCreationUncertain{
			SessionID:   req.SessionID,
			Contributor: req.Contributor,
			Err:         fmt.Errorf("creation uncertainty is unresolved for this session: %w", cause),
		}
	}
	if call, ok := a.creations[req.SessionID]; ok {
		a.createMu.Unlock()
		<-call.done
		if call.err != nil {
			// Shared typed failure: the waiting caller receives the
			// creator's outcome, it does not retry independently.
			return adapter.SessionBinding{}, call.err
		}
		if err := compareCreationConfig(req, call.contributor, call.model, call.workspace, a.profileDigest, a.profileDigest); err != nil {
			return adapter.SessionBinding{}, err
		}
		return call.binding, nil
	}
	call := &codexCreationCall{done: make(chan struct{})}
	a.creations[req.SessionID] = call
	a.createMu.Unlock()

	binding, err := a.createBinding(ctx, req)
	call.binding = binding
	call.err = err
	call.contributor = req.Contributor
	call.model = req.Config.Model
	call.workspace = req.Config.WorkspaceRoot
	// Publish the outcome BEFORE retiring a failed reservation: a caller
	// arriving in the retirement window finds this call, sees done closed,
	// and shares the result (AC-008 pattern).
	close(call.done)
	if err != nil {
		// §3.4 tombstone: an uncertain creation blocks automatic
		// recreation for this logical session until the service layer
		// explicitly resolves it.
		var unc *adapter.ErrSessionCreationUncertain
		if errors.As(err, &unc) {
			a.createMu.Lock()
			a.uncertain[req.SessionID] = err
			a.createMu.Unlock()
		}
		a.createMu.Lock()
		if a.creations[req.SessionID] == call {
			delete(a.creations, req.SessionID)
		}
		a.createMu.Unlock()
	}
	return binding, err
}

// ResolveCreationUncertainty clears the in-adapter creation-uncertain
// tombstone for a logical session, permitting a new CreateSession. It is
// the explicit, controller-visible resolution seam the service layer
// owns (Task 9): automatic retries NEVER clear it, and durable
// cross-restart blocking is recorded by the service journal — this
// in-memory map is one process's enforcement of the same §3.4 rule.
// It reports whether a tombstone was cleared.
func (a *CodexAdapter) ResolveCreationUncertainty(sessionID adapter.SessionID) bool {
	a.createMu.Lock()
	defer a.createMu.Unlock()
	_, ok := a.uncertain[sessionID]
	if ok {
		delete(a.uncertain, sessionID)
	}
	return ok
}

// createBinding performs the validated creation in §3.4 gate order.
func (a *CodexAdapter) createBinding(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	// §3.3 ordering step 1 — BEFORE any child starts: the isolation
	// attestation is durable Council-side state. Missing ⇒ typed
	// ErrProductionEligibilityMissing, no process started.
	if err := a.checkProductionEligibility(req.SessionID); err != nil {
		return adapter.SessionBinding{}, err
	}

	meta, err := a.store.GetSessionMetadata(ctx, string(req.SessionID))
	if err != nil {
		return adapter.SessionBinding{}, fmt.Errorf("session metadata lookup: %w", err)
	}
	if meta.Contributor != "codex" {
		return adapter.SessionBinding{}, fmt.Errorf(
			"session %s contributor is %q; refusing to create a codex binding",
			req.SessionID, meta.Contributor)
	}

	// An already-persisted binding is authoritative: identical config is
	// idempotent (no child needed), changed config fails closed (§3.4).
	if existing, err := a.store.GetCodexSessionBinding(ctx, string(req.SessionID)); err != nil {
		return adapter.SessionBinding{}, err
	} else if existing != nil {
		if err := compareStoredConfig(req, existing, a.profileDigest); err != nil {
			return adapter.SessionBinding{}, err
		}
		return a.bindingFor(req, existing.NativeID), nil
	}

	// §3.3 ordering step 2 — child start (only after step 1 passed). The
	// server's handshake gate compares codexHome/platform against the
	// frozen policy and terminates the child on mismatch.
	a.server.markBusy(req.SessionID)
	child, err := a.server.start(ctx, req.SessionID)
	if err != nil {
		a.server.markIdle(req.SessionID)
		return adapter.SessionBinding{}, err
	}
	authRejected := false
	defer func() {
		if authRejected {
			a.server.markIdle(req.SessionID)
		}
	}()

	// §3.3 ordering step 2 — provider-free account/read auth gate.
	// Unauthenticated AND inconclusive (error, timeout, unknown shape)
	// fail closed: child terminated, typed rejection, no thread or turn
	// created. Adapter-owned context: a disconnected controller cannot
	// abandon the gate mid-check.
	gctx, gcancel := context.WithTimeout(context.WithoutCancel(ctx), authGateTimeout)
	defer gcancel()
	acct, err := child.client.AccountRead(gctx)
	if err != nil {
		authRejected = true
		a.server.stop(ctx, req.SessionID)
		return adapter.SessionBinding{}, &ErrCodexAuthRequired{
			SessionID: req.SessionID,
			Reason:    "inconclusive auth check: " + err.Error(),
			Err:       err,
		}
	}
	if acct.RequiresOpenaiAuth {
		authRejected = true
		a.server.stop(ctx, req.SessionID)
		return adapter.SessionBinding{}, &ErrCodexAuthRequired{
			SessionID: req.SessionID,
			Reason:    "child is unauthenticated (requiresOpenaiAuth)",
		}
	}

	// Creation-reservation route → thread/start → id equality (the pump
	// confirms the response id equals the notified thread id before the
	// binding publishes; spec §3.2). The write is bounded by the
	// adapter-owned ack timeout.
	tctx, tcancel := context.WithTimeout(context.WithoutCancel(ctx), creationAckTimeout)
	defer tcancel()
	thread, err := child.client.ThreadStart(tctx, ThreadStartParamsFor(req.Config.WorkspaceRoot))
	if err != nil {
		a.server.stop(ctx, req.SessionID)
		var wr *ErrRequestWrite
		if errors.As(err, &wr) && !wr.PostWrite() {
			// The request was never transmitted: nothing native was
			// created, so this is a clean rejection — not uncertainty.
			return adapter.SessionBinding{}, fmt.Errorf("thread/start was not transmitted: %w", err)
		}
		// Lost response, abandoned reservation, or protocol drift: the
		// native thread may exist; automatic recreation is blocked
		// (tombstone on this child, typed uncertainty to the caller).
		return adapter.SessionBinding{}, &adapter.ErrSessionCreationUncertain{
			SessionID:   req.SessionID,
			Contributor: req.Contributor,
			Err:         err,
		}
	}
	if !isValidUUIDv7(thread.ID) {
		// Id drift fail-closed (spec §3.5): the confirmed thread id is not
		// a canonical UUIDv7 — never bind it.
		a.server.poisonKey(req.SessionID, &ErrProtocolDrift{
			Method: "thread/start",
			Reason: "confirmed thread id is not canonical UUIDv7: " + thread.ID,
		})
		return adapter.SessionBinding{}, &adapter.ErrSessionCreationUncertain{
			SessionID:       req.SessionID,
			Contributor:     req.Contributor,
			PartialNativeID: thread.ID,
			Err:             fmt.Errorf("thread/start returned a non-canonical id"),
		}
	}

	a.server.markIdle(req.SessionID)
	return a.bindingFor(req, thread.ID), nil
}

func (a *CodexAdapter) bindingFor(req adapter.CreateSessionRequest, nativeID string) adapter.SessionBinding {
	return adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: nativeID,
		Config:          req.Config,
	}
}

// checkProductionEligibility applies the §3.3 eligibility gate: a valid
// isolation attestation must exist for the frozen (codex version,
// platform, manifest) tuple. It fires BEFORE any child starts — at
// CreateSession (ordering step 1) and at every Dispatch that launches a
// replacement child (§3.9: "at every launch"). A nil lookup fails
// closed; there is no inference of eligibility from absent evidence.
func (a *CodexAdapter) checkProductionEligibility(sessionID adapter.SessionID) error {
	if a.attestation == nil {
		return &ErrProductionEligibilityMissing{
			SessionID: sessionID,
			Reason:    "no attestation lookup is wired into the production constructor",
		}
	}
	if _, ok := a.attestation(); !ok {
		return &ErrProductionEligibilityMissing{
			SessionID: sessionID,
			Reason:    "no valid isolation attestation for the frozen (codex version, platform, manifest) tuple",
		}
	}
	return nil
}

// compareCreationConfig enforces the §3.4 fail-closed mismatch rule
// against a shared in-flight creation outcome.
func compareCreationConfig(req adapter.CreateSessionRequest, contributor council.Contributor, model, workspace, storedDigest, frozenDigest string) error {
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
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "profile_digest",
			Want: frozenDigest, Have: storedDigest}
	}
	return nil
}

// compareStoredConfig enforces the same rule against the persisted
// binding (idempotent-replay path).
func compareStoredConfig(req adapter.CreateSessionRequest, existing *storage.CodexSessionBinding, profileDigest string) error {
	if req.Contributor != council.Contributor("codex") {
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "contributor",
			Want: string(req.Contributor), Have: "codex"}
	}
	if req.Config.Model != existing.Model {
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "model",
			Want: req.Config.Model, Have: existing.Model}
	}
	if req.Config.WorkspaceRoot != existing.Workspace {
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "workspace",
			Want: req.Config.WorkspaceRoot, Have: existing.Workspace}
	}
	if existing.ProfileDigest != profileDigest {
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "profile_digest",
			Want: profileDigest, Have: existing.ProfileDigest}
	}
	return nil
}

// ── ResumeSession ───────────────────────────────────────────────────────

// ResumeSession performs validated LOCAL inspection only (§3.4): the
// persisted binding must be well-formed, and a materialized binding must
// still have its rollout at the recorded path with session_meta matching
// the native id. NO native call is made; native verification is deferred
// to the next dispatch's provider-free thread/resume (which surfaces
// ErrNativeSessionMissing on the verbatim deterministic error).
func (a *CodexAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	stored, err := a.store.GetCodexSessionBinding(ctx, string(binding.SessionID))
	if err != nil {
		return err
	}
	if stored == nil {
		return fmt.Errorf("resume requires a persisted codex session binding for %s", binding.SessionID)
	}
	if !isValidUUIDv7(stored.NativeID) {
		return &ErrNativeSessionMissing{ThreadID: stored.NativeID}
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
	if stored.ProfileDigest != a.profileDigest {
		return &ErrSessionConfigMismatch{SessionID: binding.SessionID, Field: "profile_digest",
			Want: a.profileDigest, Have: stored.ProfileDigest}
	}
	if stored.Materialized {
		if stored.RolloutPath == nil || strings.TrimSpace(*stored.RolloutPath) == "" {
			return fmt.Errorf("materialized binding %s has no recorded rollout path", binding.SessionID)
		}
		if err := verifyRolloutIdentity(*stored.RolloutPath, a.policy.ExpectedCodexHome, stored.NativeID); err != nil {
			return fmt.Errorf("rollout verification for session %s: %w", binding.SessionID, err)
		}
	}
	return nil
}

// ── Dispatch ────────────────────────────────────────────────────────────

// Dispatch submits one turn under the §3.5 discipline: pre-transmission
// verification for EVERY turn (first included) via the provider-free
// thread/resume effective-config compare, per-turn pins from the frozen
// policy on turn/start, crash-safe durable ordering (attempt+baseline
// before launch; launch reserved in the same transaction as the
// launch_count increment BEFORE the write; first_stdin_byte_at at the
// write boundary), and single-flight per native thread. Acceptance
// follows §3.9: definitive error response ⇒ DispatchRejected, lost
// response / post-write failure ⇒ DispatchUnknown, pre-write failure ⇒
// DispatchRejected with pre-acceptance evidence.
func (a *CodexAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	if err := ref.Validate(); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}
	attempt, ok := a.identity.AttemptFor(ctx, ref)
	if !ok || strings.TrimSpace(attempt) == "" {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "no attempt identity"}, fmt.Errorf("no attempt identity for %s/%s", ref.SessionID, ref.TurnKey)
	}

	binding, err := a.store.GetCodexSessionBinding(ctx, string(ref.SessionID))
	if err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}
	if binding == nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "no codex session binding for " + string(ref.SessionID)}, nil
	}
	nativeID := binding.NativeID
	if !isValidUUIDv7(nativeID) {
		// Non-canonical ids are never transmitted (spec §6 scenario 5).
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "binding native id is not a canonical UUIDv7"}, nil
	}

	// §3.5 blocking semantics: an unresolved (uncertain, undisposed)
	// attempt blocks every subsequent turn on this native session —
	// including across restarts — until the controller records a
	// disposition. Durable state, not process memory. This check runs
	// BEFORE slot acquisition: an in-flight turn is itself uncertain in
	// durable state, so the block IS the single-flight guarantee
	// (a second turn is rejected, never queued).
	blocked, err := a.store.HasCodexUnresolvedAttempts(ctx, nativeID)
	if err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}
	if blocked {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "native session " + nativeID + " has an unresolved attempt; blocked until the controller records a disposition"}, nil
	}

	if err := a.acquireSlot(nativeID, attempt); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}
	accepted := false
	releaseIfRejected := func() {
		if !accepted {
			a.releaseSlot(nativeID, attempt)
		}
	}

	a.server.markBusy(ref.SessionID)

	// §3.9: the eligibility gate fires at every launch. A dispatch that
	// needs a replacement child (parked, crashed, or first dispatch after
	// restart) re-checks the attestation BEFORE any process starts — the
	// check precedes markBusy so a refused launch leaves no state behind.
	if !a.server.hasChild(ref.SessionID) {
		if err := a.checkProductionEligibility(ref.SessionID); err != nil {
			a.server.markIdle(ref.SessionID)
			releaseIfRejected()
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
				Reason: err.Error()}, err
		}
	}

	// Pre-transmission verification (§3.5 step 1) — FIRST TURN INCLUDED.
	// The child may be a replacement after parking; the handshake gate
	// applies there too.
	child, err := a.server.start(ctx, ref.SessionID)
	if err != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "child start failed: " + err.Error()}, err
	}
	child.pump.BindThread(nativeID)

	cfg, err := child.client.ResumeProbe(nativeID)
	if err != nil {
		a.server.stop(ctx, ref.SessionID)
		releaseIfRejected()
		var missing *ErrNativeSessionMissing
		if errors.As(err, &missing) {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
				Reason: missing.Error()}, missing
		}
		// The probe failed before ANY prompt byte was written: rejecting
		// is definitive-safe (the turn can never have started).
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "resume verification failed: " + err.Error()}, err
	}
	if drift := compareEffectiveConfig(cfg, nativeID, a.policy, binding.Model, binding.Workspace); drift != nil {
		a.server.stop(ctx, ref.SessionID)
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: drift.Error()}, drift
	}

	promptDigest, err := PromptDigest(nativeID, ref.TurnKey, attempt, prompt)
	if err != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}
	turnParams, err := TurnStartParamsFor(a.policy, binding.Model, binding.Workspace, nativeID, prompt)
	if err != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}

	// §3.11 crash-safe ordering: attempt+baseline durable BEFORE launch.
	baseline, err := a.observeAttemptBaseline(ctx, binding)
	if err != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "rollout baseline: " + err.Error()}, err
	}
	attemptRow := storage.CodexTurnAttempt{
		AttemptID: attempt, SessionID: string(ref.SessionID),
		TurnKey: ref.TurnKey, PromptDigest: promptDigest,
		BaselineIdentity:     baseline.identity,
		BaselineSize:         baseline.size,
		BaselineEntries:      baseline.entries,
		BaselineMaterialized: baseline.materialized,
		RolloutProtection:    "advisory", // Task 6 (rollout trust) freezes protection at launch
	}
	if err := a.store.InsertCodexTurnAttempt(ctx, attemptRow); err != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
	}

	// Launch reserved in the SAME transaction as the launch_count
	// increment, BEFORE the request is written (spec §3.11).
	seq, resErr := a.store.ReserveCodexLaunch(ctx, attempt, "codex-app-server", a.server.generation(ref.SessionID))
	if resErr != nil {
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: resErr.Error()}, resErr
	}
	if err := a.store.RecordCodexLaunchState(ctx, attempt, seq, "started", nil); err != nil {
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "child ready but launch state could not be recorded"}, nil
	}

	// The turn/start write rides an adapter-owned context: a disconnected
	// controller cannot skip the verification chain or abandon the ack.
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), dispatchAckTimeout)
	defer wcancel()
	raw, turnErr := child.client.TurnStart(wctx, turnParams)
	if turnErr != nil {
		return a.classifyTurnStartFailure(ctx, ref, attempt, seq, turnErr, releaseIfRejected)
	}

	// The frame was fully written: record the transmission boundary
	// (first-byte-wins; the request provably reached the child's stdin).
	if err := a.store.RecordCodexStdinTransmitted(ctx, attempt, seq); err != nil {
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "turn/start acknowledged but the transmission boundary could not be persisted: " + err.Error()}, nil
	}

	turnID := nativeTurnIDFromResult(raw)
	if turnID == "" {
		// A definitive success without a correlatable native turn id:
		// single-flight cannot map the turn honestly — fail toward
		// uncertainty (the turn may be running).
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "turn/start succeeded but the result carries no native turn id"}, nil
	}

	run := &codexTurnRun{
		ref:            ref,
		attemptID:      attempt,
		nativeThreadID: nativeID,
		launchSeq:      seq,
		pump:           child.pump,
	}
	tap, terr := child.pump.InstallTurnRoute(nativeID, turnID)
	if terr != nil {
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "turn route could not be installed: " + terr.Error()}, terr
	}
	run.tap = tap
	run.routeInstalled = true

	// Bind the native turn id in-life from turn/started (replayed from
	// the park buffer when the notification arrived before the route).
	if started := a.awaitTurnStarted(run); started {
		if err := a.store.RecordCodexNativeTurnID(ctx, attempt, tap.TurnID()); err != nil {
			a.server.markIdle(ref.SessionID)
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
				Reason: "turn accepted but the native turn id could not be bound: " + err.Error()}, nil
		}
	}

	a.mu.Lock()
	a.turns[ref] = run
	a.mu.Unlock()

	// First acceptance: observe the rollout baseline (bounded UUID-suffix
	// scan; session_meta verified) and materialize the binding. Task 6
	// generalizes this into ResolveRollout. A gap here leaves the binding
	// unmaterialized — the next dispatch re-verifies provider-free; the
	// accepted turn itself is unaffected.
	if !binding.Materialized {
		_ = a.recordFirstAcceptance(string(ref.SessionID), attempt, nativeID)
	}

	accepted = true
	go a.runTurn(run)

	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
}

// classifyTurnStartFailure applies the §3.9 acceptance taxonomy to a
// failed turn/start: definitive error response ⇒ Rejected; pre-write
// failure ⇒ Rejected with pre-acceptance evidence; everything after the
// write ⇒ Unknown (the turn may have started).
func (a *CodexAdapter) classifyTurnStartFailure(
	ctx context.Context,
	ref adapter.TurnRef,
	attempt string,
	seq int64,
	turnErr error,
	releaseIfRejected func(),
) (adapter.DispatchOutcome, error) {
	bg := context.Background()
	var rpcErr *RPCError
	var wrErr *ErrRequestWrite
	switch {
	case errors.As(turnErr, &rpcErr):
		// Definitive native refusal AFTER the request was written: the
		// frame provably reached stdin (record the boundary) and the
		// launch concluded without a turn.
		_ = a.store.RecordCodexStdinTransmitted(bg, attempt, seq)
		_ = a.store.RecordCodexLaunchState(bg, attempt, seq, "dead", nil)
		_ = a.store.SetCodexAttemptObservedStatus(bg, attempt, "missing")
		releaseIfRejected()
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "native refusal: " + rpcErr.Error()}, turnErr
	case errors.As(turnErr, &wrErr) && !wrErr.PostWrite():
		// Write refused before any byte: nothing native was created.
		_ = a.store.RecordCodexLaunchState(bg, attempt, seq, "dead", nil)
		_ = a.store.SetCodexAttemptObservedStatus(bg, attempt, "missing")
		releaseIfRejected()
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "turn/start write failed before any byte: " + wrErr.Error()}, turnErr
	case errors.Is(turnErr, context.DeadlineExceeded) || errors.Is(turnErr, context.Canceled):
		// The write completed (Call reaches the ack wait only after a
		// successful write): the turn may have started.
		_ = a.store.RecordCodexStdinTransmitted(bg, attempt, seq)
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "turn/start ack not observed: " + turnErr.Error()}, nil
	default:
		if wrErr != nil && wrErr.PostWrite() {
			_ = a.store.RecordCodexStdinTransmitted(bg, attempt, seq)
		}
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "turn/start outcome ambiguous: " + turnErr.Error()}, nil
	}
}

// awaitTurnStarted waits bounded for the turn/started notification on the
// installed route (replayed from the park buffer when it arrived before
// the route). The route id IS the correlation id; the notification proves
// the native side started the turn.
func (a *CodexAdapter) awaitTurnStarted(run *codexTurnRun) bool {
	timer := time.NewTimer(turnStartedGrace)
	defer timer.Stop()
	select {
	case n := <-run.tap.C():
		return n.Method == "turn/started"
	case <-run.tap.Done():
		return false
	case <-timer.C:
		return false
	}
}

// runTurn owns an accepted turn until a verified terminal classification
// or the route dies. Every exit releases the single-flight slot: the slot
// guards ACTIVE execution only, while durable attempt state carries the
// §3.5 block. item/*, usage, and approval traffic is mirrored into the
// event stream by Task 6; this loop consumes only terminal evidence.
func (a *CodexAdapter) runTurn(run *codexTurnRun) {
	defer a.finishTurn(run)
	bg := context.Background()
	for {
		select {
		case n, ok := <-run.tap.C():
			if !ok {
				return
			}
			switch n.Method {
			case "turn/completed", "turn/failed":
				observed, classified := nativeTurnOutcome(n.Params)
				if !classified {
					// Unparseable terminal shape: never fabricate an
					// outcome — the attempt stays uncertain.
					return
				}
				if observed == "" {
					// interrupted / other statuses are Task 6 cancel
					// semantics; leave the attempt uncertain.
					return
				}
				if err := a.store.SetCodexAttemptTerminal(bg, run.attemptID, observed, string(n.Params), ""); err != nil {
					// Terminal persistence failed: the outcome cannot be
					// resolved, so the attempt stays uncertain and the
					// durable block persists.
					return
				}
				return
			}
		case <-run.tap.Done():
			// Child death or protocol drift: no verified terminal — the
			// launch is recorded dead and the attempt stays uncertain.
			_ = a.store.RecordCodexLaunchState(bg, run.attemptID, run.launchSeq, "dead", nil)
			return
		}
	}
}

// finishTurn releases everything an accepted turn held.
func (a *CodexAdapter) finishTurn(run *codexTurnRun) {
	if run.routeInstalled && run.pump != nil {
		run.pump.RemoveTurnRoute(run.nativeThreadID, run.tap.TurnID())
	}
	a.mu.Lock()
	delete(a.turns, run.ref)
	a.mu.Unlock()
	a.releaseSlot(run.nativeThreadID, run.attemptID)
	a.server.markIdle(run.ref.SessionID)
}

// ── Phased-delivery surfaces (Task 6: rollout trust + observe/cancel/
// collect/reconcile). The stubs are honest classifications, never
// fabricated results: Cancel reports CancelUnknown (an interrupt request
// is NOT terminal evidence, §3.9), Collect reads only durable state,
// Reconcile refuses to claim more than durable evidence proves.

// Observe is completed by Task 6 (rollout/observe): live event streams,
// approval mirroring, and usage mirroring. The underlying turn is NOT
// affected — notifications keep flowing into the pump, and a
// disconnected observer never cancels a worker (AC-006).
func (a *CodexAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	a.mu.Lock()
	_, live := a.turns[ref]
	a.mu.Unlock()
	if !live {
		return nil, fmt.Errorf("turn %s/%s is not dispatched or already completed", ref.SessionID, ref.TurnKey)
	}
	return nil, ErrObservationUnavailable
}

// Cancel is completed by Task 6 (turn/interrupt with verified
// interrupted-terminal evidence). Until then any cancel reports
// CancelUnknown — a disposition that provably claims nothing.
func (a *CodexAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	a.mu.Lock()
	_, live := a.turns[ref]
	a.mu.Unlock()
	if !live {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
			Reason: "turn not dispatched"}, nil
	}
	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
		Reason: "codex interruption wiring completes in Task 6 (rollout/observe)"}, nil
}

// Collect reports from durable state only: a verified terminal is
// returned, anything else stays pending. Usage mirroring and result
// shaping complete in Task 6.
func (a *CodexAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	attempt, err := a.store.GetLatestCodexTurnAttempt(ctx, string(ref.SessionID), ref.TurnKey)
	if err != nil {
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
			ResultStatus: adapter.ResultUnavailable}, err
	}
	if attempt == nil {
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
			ResultStatus: adapter.ResultUnavailable}, errors.New("turn not dispatched")
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

// Reconcile is completed by Task 6 (protected-mode rollout terminal
// records, verified absence, one-redispatch authorization). Until then
// the only claims it makes are the live in-process run (reachable-active)
// and Uncertain for everything else — child unreachable is host
// visibility lost, never definitive failure (§3.2/§3.10).
func (a *CodexAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
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
	return adapter.ReconciliationOutcome{Ref: ref,
		Reachability: council.VisibilityHostLost,
		Status:       adapter.ReconciliationUncertain,
		Observed:     council.TurnRunning,
	}, nil
}

// ── Single-flight ───────────────────────────────────────────────────────

// acquireSlot claims the active-execution slot for a native thread.
// Per §3.5 a second dispatch is REJECTED rather than queued: the
// conflict is an immediate typed busy error (the caller's ctx bounds
// only the claim attempt, never a wait). The durable uncertain-attempt
// block is the primary single-flight guarantee; this slot guards the
// in-process race window and allows same-attempt re-entry.
func (a *CodexAdapter) acquireSlot(nativeID, owner string) error {
	a.mu.Lock()
	slot, held := a.singleFlt[nativeID]
	if !held {
		a.singleFlt[nativeID] = &codexSlot{released: make(chan struct{}), owner: owner}
		a.mu.Unlock()
		return nil
	}
	if slot.owner == owner {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	return fmt.Errorf("native thread %s is busy: an in-flight turn holds the single-flight slot", nativeID)
}

func (a *CodexAdapter) releaseSlot(nativeID, owner string) {
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

// ── Effective-config verification (spec §3.5 step 1) ────────────────────

// compareEffectiveConfig verifies the COMPLETE thread/resume effective
// configuration against the frozen profile: the id-equality drift check
// (the returned thread id must BE the bound native id), then model,
// modelProvider, approvalPolicy (canonical byte comparison), sandbox
// (canonical byte comparison), approvalsReviewer, instructionSources,
// and cwd. The FIRST disagreement is returned as a typed ErrProfileDrift;
// nil means verified.
func compareEffectiveConfig(cfg EffectiveConfig, nativeID string, policy CodexLaunchPolicy, model, workspaceRoot string) *ErrProfileDrift {
	drift := func(field, want, have string) *ErrProfileDrift {
		return &ErrProfileDrift{ThreadID: cfg.ThreadID, Field: field, Want: want, Have: have}
	}
	if cfg.ThreadID != nativeID {
		// Id drift: any response whose thread id differs from the binding
		// is a protocol violation (spec §3.5).
		return drift("thread_id", nativeID, cfg.ThreadID)
	}
	if cfg.Model != model {
		return drift("model", model, cfg.Model)
	}
	if cfg.ModelProvider != policy.ModelProvider {
		return drift("modelProvider", policy.ModelProvider, cfg.ModelProvider)
	}
	nativeApproval, err := canonicalJSON(cfg.ApprovalPolicy)
	if err != nil {
		return drift("approvalPolicy", policy.ApprovalPolicyCanonical,
			"unparseable: "+err.Error())
	}
	if nativeApproval != policy.ApprovalPolicyCanonical {
		return drift("approvalPolicy", policy.ApprovalPolicyCanonical, nativeApproval)
	}
	frozenSandbox, err := CanonicalSandboxPolicy(policy)
	if err != nil {
		return drift("sandbox", "", "unencodable frozen policy: "+err.Error())
	}
	nativeSandbox, err := canonicalJSON(cfg.Sandbox)
	if err != nil {
		return drift("sandbox", string(frozenSandbox), "unparseable: "+err.Error())
	}
	if nativeSandbox != string(frozenSandbox) {
		return drift("sandbox", string(frozenSandbox), nativeSandbox)
	}
	if cfg.ApprovalsReviewer != policy.ApprovalsReviewer {
		return drift("approvalsReviewer", policy.ApprovalsReviewer, cfg.ApprovalsReviewer)
	}
	wantSources := canonicalStringSet(policy.ExpectedInstructionSources)
	haveSources := canonicalStringSet(cfg.InstructionSources)
	if fmt.Sprint(wantSources) != fmt.Sprint(haveSources) {
		return drift("instructionSources",
			strings.Join(wantSources, ", "), strings.Join(haveSources, ", "))
	}
	if cfg.CWD != workspaceRoot {
		return drift("cwd", workspaceRoot, cfg.CWD)
	}
	return nil
}

// nativeTurnIDFromResult extracts the native turn id from the turn/start
// result ({id, threadId, status}).
func nativeTurnIDFromResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return ""
	}
	return r.ID
}

// nativeTurnOutcome classifies a turn/completed|turn/failed notification
// body: "completed" or "failed" for a parseable terminal status, ""
// (uncategorized) for interrupted or unknown statuses, and !ok for an
// unparseable body (never fabricate an outcome from an unknown shape).
// The verified evidence carries the turn status BOTH as a plain string
// (turn/completed rollout/fixture shape) and as an object
// ({"type":"inProgress"} in the turn/start result); both parse.
func nativeTurnOutcome(params json.RawMessage) (observed string, ok bool) {
	if len(params) == 0 {
		return "", false
	}
	var p struct {
		Turn *struct {
			Status json.RawMessage `json:"status"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Turn == nil {
		return "", false
	}
	status := strings.Trim(string(p.Turn.Status), `" `)
	if len(p.Turn.Status) > 0 && p.Turn.Status[0] == '{' {
		var st ThreadStatus
		if err := json.Unmarshal(p.Turn.Status, &st); err != nil {
			return "", false
		}
		status = st.Type
	}
	switch status {
	case "completed":
		return "completed", true
	case "failed":
		return "failed", true
	default:
		return "", true
	}
}

// ── Rollout baseline (first acceptance; Task 6 generalizes) ─────────────

type attemptBaseline struct {
	identity     string
	size         int64
	entries      int
	materialized bool
}

// observeAttemptBaseline records the CURRENT rollout state for the
// attempt (§3.11: attempt+baseline durable before launch): a materialized
// binding reads its recorded rollout; an unmaterialized binding starts
// from the zero baseline (the first-acceptance scan records the real
// baseline after acceptance).
func (a *CodexAdapter) observeAttemptBaseline(ctx context.Context, binding *storage.CodexSessionBinding) (attemptBaseline, error) {
	if !binding.Materialized {
		return attemptBaseline{}, nil
	}
	if binding.RolloutPath == nil || strings.TrimSpace(*binding.RolloutPath) == "" {
		return attemptBaseline{}, fmt.Errorf("materialized binding %s has no recorded rollout path", binding.SessionID)
	}
	identity, size, entries, err := scanRolloutBaseline(*binding.RolloutPath, binding.NativeID)
	if err != nil {
		return attemptBaseline{}, err
	}
	return attemptBaseline{identity: identity, size: size, entries: entries, materialized: true}, nil
}

// recordFirstAcceptance locates the rollout by a bounded UUID-suffix scan
// under the frozen CODEX_HOME sessions root, verifies
// session_meta.session_id == native id, and records the baseline
// (file-identity, byte size, entry count) plus the binding
// materialization in one durable transition.
func (a *CodexAdapter) recordFirstAcceptance(sessionID, attemptID, nativeID string) error {
	path, err := locateRollout(a.policy.ExpectedCodexHome, nativeID)
	if err != nil {
		return err
	}
	identity, size, entries, err := scanRolloutBaseline(path, nativeID)
	if err != nil {
		return err
	}
	return a.store.RecordCodexFirstAcceptance(context.Background(), sessionID, attemptID, path, identity, size, entries)
}

// verifyRolloutIdentity is the local ResumeSession check for a
// materialized binding: the rollout exists at the recorded path, is a
// regular non-symlink file contained under the expected sessions root,
// and its session_meta carries the native id.
func verifyRolloutIdentity(path, codexHome, nativeID string) error {
	if err := checkRolloutPath(path, codexHome); err != nil {
		return err
	}
	_, _, _, err := scanRolloutBaseline(path, nativeID)
	return err
}

// isValidUUIDv7 reports whether s is a canonical lowercase UUIDv7
// (matches the storage schema CHECK shape).
func isValidUUIDv7(s string) bool {
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
	if s[14] != '7' {
		return false
	}
	variant := s[19]
	return variant == '8' || variant == '9' || variant == 'a' || variant == 'b'
}
