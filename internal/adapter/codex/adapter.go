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
	"runtime"
	"sort"
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

// ErrCodexThreadStartRejected reports a definitive native refusal of
// thread/start (spec §3.4/§3.8): the child answered the request with a
// JSON-RPC error (e.g. the deterministic trust-precondition failure
// "Not inside a trusted directory …"), so the outcome is NOT creation
// uncertainty — no thread was created and the child is terminated. No
// §3.4 tombstone is recorded: a corrected creation attempt is permitted.
type ErrCodexThreadStartRejected struct {
	SessionID adapter.SessionID
	Code      int
	Message   string
	Err       error
}

func (e *ErrCodexThreadStartRejected) Error() string {
	return fmt.Sprintf("thread/start was definitively refused for session %s (%d: %s)", e.SessionID, e.Code, e.Message)
}

func (e *ErrCodexThreadStartRejected) Unwrap() error { return e.Err }

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
	// fixtureScope is the explicit test-only construction marker (spec
	// §3.3): set ONLY by NewFixtureScopedAdapter, never inferred. It
	// skips the production-eligibility gate and nothing else — every
	// protocol validation stays in force. The production constructor
	// rejects the marker, so production wiring can never set it.
	fixtureScope bool

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
// on CreateSession — production wiring must supply it. The variadic
// option surface is the NON-production construction seam only: handing
// the test-only fixture option to this constructor fails closed with a
// typed ErrFixtureModeProhibited — production construction has no path
// to fixture mode, and the service wiring passes no options at all.
func NewCodexAdapter(
	store *storage.Store,
	server *CodexServer,
	policy CodexLaunchPolicy,
	profileDigest string,
	identity DispatchIdentitySource,
	attestation AttestationLookup,
	opts ...ConstructionOption,
) (*CodexAdapter, error) {
	var settings constructionSettings
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&settings)
	}
	if settings.fixtureScope {
		return nil, &ErrFixtureModeProhibited{Reason: "NewCodexAdapter is the production constructor; the test-only fixture scope is built through the codextest package (spec §3.3)"}
	}
	return buildCodexAdapter(store, server, policy, profileDigest, identity, attestation, false), nil
}

// buildCodexAdapter is the shared construction body: identical for the
// production and the test-only fixture scope except the explicit marker
// that selects eligibility enforcement.
func buildCodexAdapter(
	store *storage.Store,
	server *CodexServer,
	policy CodexLaunchPolicy,
	profileDigest string,
	identity DispatchIdentitySource,
	attestation AttestationLookup,
	fixtureScope bool,
) *CodexAdapter {
	a := &CodexAdapter{
		store:         store,
		server:        server,
		policy:        policy,
		profileDigest: profileDigest,
		identity:      identity,
		attestation:   attestation,
		fixtureScope:  fixtureScope,
		turns:         make(map[adapter.TurnRef]*codexTurnRun),
		singleFlt:     make(map[string]*codexSlot),
		creations:     make(map[adapter.SessionID]*codexCreationCall),
		uncertain:     make(map[adapter.SessionID]error),
	}
	// The approval responder is installed for every child this server
	// starts (spec §3.6): registration happens at child start, BEFORE
	// the framing reader runs, so a server→client request can never
	// arrive unhandled.
	if server != nil {
		server.SetServerRequestHandler(a.handleApprovalRequest)
	}
	return a
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
	child          *codexChild
	stream         *adapter.BufferedStream

	// usageMu guards the latest cumulative token-usage snapshot
	// (thread/tokenUsage/updated notifications are cumulative; an older
	// snapshot never overwrites a newer one — spec §3.9).
	usageMu   sync.Mutex
	usageIn   int64
	usageOut  int64
	usageSeen bool

	// cancel state: the interrupt request is NOT terminal evidence
	// (§3.9); only a verified interrupted terminal confirms the cancel.
	cancelMu        sync.Mutex
	cancelRequested bool
	// termCh is closed exactly once, when the run loop commits a
	// verified terminal classification; termStatus carries the status.
	termCh     chan string
	termOnce   sync.Once
	termStatus string
	terminated bool

	// Approval responder state (spec §3.6): answered denials mirror into
	// the stream exactly once per ApprovalID, and responder diagnostics
	// (late denials, fail-closed receipts) are kept as bounded in-memory
	// notes on the attempt run.
	mirrorMu    sync.Mutex
	mirrored    map[string]struct{}
	diagMu      sync.Mutex
	diagnostics []string
}

// markApprovalMirrored claims the once-per-ApprovalID mirror slot: the
// first claimant mirrors the tool_requested/tool_denied pair; duplicate
// re-denies are idempotent on the wire but never re-mirror.
func (r *codexTurnRun) markApprovalMirrored(approvalID string) bool {
	r.mirrorMu.Lock()
	defer r.mirrorMu.Unlock()
	if r.mirrored == nil {
		r.mirrored = make(map[string]struct{})
	}
	if _, dup := r.mirrored[approvalID]; dup {
		return false
	}
	r.mirrored[approvalID] = struct{}{}
	return true
}

// runDiagnostics returns a copy of the bounded responder diagnostic log.
func (r *codexTurnRun) runDiagnostics() []string {
	r.diagMu.Lock()
	defer r.diagMu.Unlock()
	out := make([]string, len(r.diagnostics))
	copy(out, r.diagnostics)
	return out
}

// markCancelRequested records an accepted turn/interrupt request and
// reports whether this was the first.
func (r *codexTurnRun) markCancelRequested() bool {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	if r.cancelRequested {
		return false
	}
	r.cancelRequested = true
	return true
}

// notifyTerminal delivers the verified terminal status exactly once.
func (r *codexTurnRun) notifyTerminal(observed string) {
	r.termOnce.Do(func() {
		r.cancelMu.Lock()
		r.terminated = true
		r.termStatus = observed
		r.cancelMu.Unlock()
		close(r.termCh)
	})
}

// terminalStatus returns the verified terminal status once committed.
func (r *codexTurnRun) terminalStatus() (string, bool) {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	return r.termStatus, r.terminated
}

// observeUsage adopts a cumulative usage snapshot under the monotonic
// rule (an older snapshot never overwrites a newer one) and reports
// whether it became the latest.
func (r *codexTurnRun) observeUsage(in, out int64) bool {
	r.usageMu.Lock()
	defer r.usageMu.Unlock()
	if r.usageSeen && in+out < r.usageIn+r.usageOut {
		return false
	}
	r.usageIn, r.usageOut, r.usageSeen = in, out, true
	return true
}

// usageResult renders the latest usage snapshot as JSON. Cost is never
// fabricated: Available=false is structural (spec §3.9).
func (r *codexTurnRun) usageResult() string {
	r.usageMu.Lock()
	in, out, seen := r.usageIn, r.usageOut, r.usageSeen
	r.usageMu.Unlock()
	if !seen {
		return ""
	}
	return fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d,"cost_available":false}`, in, out)
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
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			// Definitive native refusal AFTER the request was written
			// (§3.8 trust precondition, §3.4): the child answered with a
			// JSON-RPC error, so nothing is uncertain — no thread was
			// created. Typed pre-acceptance rejection with NO tombstone
			// and NO journal-uncertainty record; only lost responses
			// take the uncertain path below.
			return adapter.SessionBinding{}, &ErrCodexThreadStartRejected{
				SessionID: req.SessionID,
				Code:      rpcErr.Code,
				Message:   rpcErr.Message,
				Err:       err,
			}
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
	if a.fixtureScope {
		// Test-only construction mode (spec §3.3): production eligibility
		// was skipped EXPLICITLY at construction — never inferred — while
		// every protocol validation stays in force. Reachable only from
		// adapters built by NewFixtureScopedAdapter.
		return nil
	}
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

	// §3.9: the eligibility gate fires at EVERY launch. The bound launch
	// is the replacement child a parked/crashed/restarted session needs;
	// a re-check even with a live child closes the TOCTOU window where
	// eligibility could lapse between two dispatches sharing a child.
	// The check precedes markBusy so a refused launch leaves no state.
	if err := a.checkProductionEligibility(ref.SessionID); err != nil {
		a.server.markIdle(ref.SessionID)
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: err.Error()}, err
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

	// §3.8 toolkit verification — dispatch-time MCP inventory check on
	// the SESSION child (the same verification path as the resume
	// compare, before any prompt byte is written): the observed
	// mcpServerStatus/list inventory must EQUAL the frozen
	// ExpectedMCPServers set. An unknown or missing server is drift; an
	// unavailable or unparseable inventory fails closed. The wiring's
	// probe capture stays operator evidence; this check is the
	// enforceable gate.
	if drift := a.checkMCPInventory(ctx, child, nativeID); drift != nil {
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
	// Freeze rollout protection at launch (§3.7 protected-evidence
	// upgrade): protected only while the isolation attestation in force
	// matches the frozen tuple; advisory is the default; a lookup
	// FAILURE is never silently downgraded.
	protection, attestationID, pErr := a.resolveRolloutProtection(ctx)
	if pErr != nil {
		a.server.markIdle(ref.SessionID)
		releaseIfRejected()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected,
			Reason: "rollout protection resolution: " + pErr.Error()}, pErr
	}
	attemptRow := storage.CodexTurnAttempt{
		AttemptID: attempt, SessionID: string(ref.SessionID),
		TurnKey: ref.TurnKey, PromptDigest: promptDigest,
		BaselineIdentity:     baseline.identity,
		BaselineSize:         baseline.size,
		BaselineEntries:      baseline.entries,
		BaselineMaterialized: baseline.materialized,
		RolloutProtection:    protection,
		AttestationID:        attestationID,
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

	// The run is registered BEFORE the turn/start write: a server→client
	// approval request that races the dispatch ack must find its turn
	// (the §3.6 responder mirrors it into this run's stream). The turn
	// route is installed after the ack carries the native turn id.
	run := &codexTurnRun{
		ref:            ref,
		attemptID:      attempt,
		nativeThreadID: nativeID,
		launchSeq:      seq,
		pump:           child.pump,
		child:          child,
		stream:         adapter.NewBufferedStream(ref, adapter.DefaultBufferCapacity),
		termCh:         make(chan string),
	}
	a.mu.Lock()
	a.turns[ref] = run
	a.mu.Unlock()

	// The turn/start write rides an adapter-owned context: a disconnected
	// controller cannot skip the verification chain or abandon the ack.
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), dispatchAckTimeout)
	defer wcancel()
	raw, turnErr := child.client.TurnStart(wctx, turnParams)
	if turnErr != nil {
		a.abandonPreAcceptanceRun(ref, run, turnErr.Error())
		return a.classifyTurnStartFailure(ctx, ref, attempt, seq, turnErr, releaseIfRejected)
	}

	// The frame was fully written: record the transmission boundary
	// (first-byte-wins; the request provably reached the child's stdin).
	if err := a.store.RecordCodexStdinTransmitted(ctx, attempt, seq); err != nil {
		a.abandonPreAcceptanceRun(ref, run, "the transmission boundary could not be persisted: "+err.Error())
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "turn/start acknowledged but the transmission boundary could not be persisted: " + err.Error()}, nil
	}

	turnID := nativeTurnIDFromResult(raw)
	if turnID == "" {
		// A definitive success without a correlatable native turn id:
		// single-flight cannot map the turn honestly — fail toward
		// uncertainty (the turn may be running).
		a.abandonPreAcceptanceRun(ref, run, "turn/start succeeded but the result carries no native turn id")
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "turn/start succeeded but the result carries no native turn id"}, nil
	}

	tap, terr := child.pump.InstallTurnRoute(nativeID, turnID)
	if terr != nil {
		a.abandonPreAcceptanceRun(ref, run, "the turn route could not be installed: "+terr.Error())
		a.server.markIdle(ref.SessionID)
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "turn route could not be installed: " + terr.Error()}, terr
	}
	// Publish the route under the turns lock: a Cancel racing the
	// pre-route window reads run.tap under the same lock (nil ⇒ unknown,
	// never a dereference of the uninstalled route).
	a.mu.Lock()
	run.tap = tap
	run.routeInstalled = true
	a.mu.Unlock()

	// Bind the native turn id in-life from turn/started (replayed from
	// the park buffer when the notification arrived before the route).
	if started := a.awaitTurnStarted(run); started {
		if err := a.store.RecordCodexNativeTurnID(ctx, attempt, tap.TurnID()); err != nil {
			a.server.markIdle(ref.SessionID)
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
				Reason: "turn accepted but the native turn id could not be bound: " + err.Error()}, nil
		}
	}

	// First acceptance: resolve the rollout by the bounded UUID-suffix
	// scan (session_meta verified, path integrity enforced) and
	// materialize the binding with the recorded baseline. A gap here
	// leaves the binding unmaterialized — the next dispatch re-verifies
	// provider-free; the accepted turn itself is unaffected.
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
// or the route dies. It is the SOLE consumer of the turn tap: item/* and
// token-usage traffic is mirrored into the observation stream (bounded,
// overflow terminates the OBSERVER tap only — the native stream is
// unaffected), usage snapshots are adopted monotonically, and terminal
// evidence is classified once. In protected mode the §3.5 step-3
// post-launch turn_context confirmation gates the terminal commit: a
// policy mismatch leaves the attempt Uncertain (drift recorded), while
// verified correlation upgrades acceptance durably. Every exit releases
// the single-flight slot: the slot guards ACTIVE execution only, while
// durable attempt state carries the §3.5 block.
func (a *CodexAdapter) runTurn(run *codexTurnRun) {
	defer a.finishTurn(run)
	bg := context.Background()
	for {
		select {
		case n, ok := <-run.tap.C():
			if !ok {
				a.concludeChildDeath(run, bg)
				return
			}
			if a.handleTurnNotification(run, n) {
				return
			}
		case <-run.child.conn.Done():
			// The child's stdout ended (exit): nothing further can
			// arrive on the stream. Drain what was already routed —
			// the terminal notification may still be buffered — then
			// conclude the child death honestly.
			a.drainAndConclude(run, bg)
			return
		}
	}
}

// handleTurnNotification processes one routed notification for the
// turn: mirrors item/usage traffic and applies the terminal semantics.
// It reports true when the turn reached a classification exit (the run
// loop must stop; stream and durable state are already finalized).
func (a *CodexAdapter) handleTurnNotification(run *codexTurnRun, n NativeNotification) bool {
	a.mirrorTurnNotification(run, n)
	switch n.Method {
	case "turn/completed", "turn/failed":
		status, bodyOK := turnStatusFromBody(n.Params)
		if !bodyOK {
			// Unparseable terminal shape: never fabricate an outcome —
			// the attempt stays uncertain.
			run.stream.CloseWithErr(errors.New("terminal notification body is unparseable"))
			return true
		}
		if status == "interrupted" {
			// Verified interrupted terminal (§3.9): the accepted
			// interrupt confirmed. Terminal, TurnCancelled — the
			// interrupted turn produced no result.
			if err := a.store.SetCodexAttemptTerminal(context.Background(), run.attemptID, "interrupted", string(n.Params), run.usageResult()); err != nil {
				return true
			}
			run.notifyTerminal("interrupted")
			_ = run.stream.SendOrOverflow(adapter.Event{
				Ref: run.ref, Type: adapter.EventTerminal,
				Status: council.TurnCancelled, Payload: "",
			})
			run.stream.Close()
			return true
		}
		if !isVerifiedTerminalStatus(status) {
			// Unknown status shapes stay honest: uncertain.
			run.stream.CloseWithErr(fmt.Errorf("unclassified turn status %q", status))
			return true
		}
		a.commitVerifiedTerminal(run, n, status)
		return true
	}
	return false
}

// drainAndConclude consumes everything already routed to the tap before
// the stream ended, then records the child death.
func (a *CodexAdapter) drainAndConclude(run *codexTurnRun, bg context.Context) {
	for {
		select {
		case n, ok := <-run.tap.C():
			if !ok {
				a.concludeChildDeath(run, bg)
				return
			}
			if a.handleTurnNotification(run, n) {
				return
			}
		default:
			a.concludeChildDeath(run, bg)
			return
		}
	}
}

// concludeChildDeath records the launch as durably known-dead and ends
// the observation stream without a verified terminal — the attempt
// stays uncertain (§3.9).
func (a *CodexAdapter) concludeChildDeath(run *codexTurnRun, bg context.Context) {
	_ = a.store.RecordCodexLaunchState(bg, run.attemptID, run.launchSeq, "dead", nil)
	run.stream.CloseWithErr(errors.New("stream ended without verified result: the native stream ended before a verified terminal"))
}

// commitVerifiedTerminal applies the §3.5 step-3 protected confirmation
// and commits the in-life verified terminal. Protected mode requires the
// rollout turn_context to match the frozen pins (a mismatch means the
// turn executed under unverified policy ⇒ Uncertain, child terminated,
// drift recorded) and upgrades acceptance when the durable pdig
// correlation matches; advisory mode records the same signals as
// diagnostics only.
func (a *CodexAdapter) commitVerifiedTerminal(run *codexTurnRun, n NativeNotification, status string) {
	bg := context.Background()

	attempt, err := a.store.GetCodexTurnAttempt(bg, run.attemptID)
	if err != nil || attempt == nil {
		run.stream.CloseWithErr(fmt.Errorf("attempt lookup failed: %v", err))
		return
	}

	if attempt.RolloutProtection == "protected" || attempt.RolloutProtection == "advisory" {
		diagnostic, drift, pdigMatched, ok := a.confirmRolloutTurn(run, attempt)
		if !ok && attempt.RolloutProtection == "protected" {
			// The rollout evidence needed for the protected confirmation
			// was unreadable or corrupt: the turn cannot be classified
			// from unconfirmable evidence — Uncertain.
			run.stream.CloseWithErr(errors.New("protected rollout confirmation failed; attempt stays uncertain"))
			return
		}
		if attempt.RolloutProtection == "protected" {
			if drift != "" {
				// §3.5 step 3: the turn executed under unverified policy
				// ⇒ Uncertain; the drift is recorded, the child
				// terminated, and NO terminal is committed.
				_ = a.store.SetCodexAttemptObservedStatus(bg, run.attemptID, "uncertain")
				a.server.stop(bg, run.ref.SessionID)
				run.stream.CloseWithErr(errors.New("protected turn_context drift: " + drift))
				return
			}
			if pdigMatched {
				// §3.7 protected acceptance upgrade: the ordered
				// correlation proved THIS turn's prompt in the rollout.
				if err := a.store.SetCodexAttemptAccepted(bg, run.attemptID); err != nil {
					// The acceptance upgrade could not be committed: the
					// protected evidence is inconsistent — Uncertain.
					run.stream.CloseWithErr(fmt.Errorf("acceptance upgrade failed: %v", err))
					return
				}
			}
		} else if diagnostic != "" {
			// Advisory mode: diagnostic only — the gap is recorded on
			// the observation stream; the verified in-life terminal
			// stands.
			_ = run.stream.SendOrOverflow(adapter.Event{
				Ref: run.ref, Type: adapter.EventProgress,
				Status: council.TurnRunning, Payload: truncateEventPayload(diagnostic),
			})
		}
	}

	usageJSON := run.usageResult()
	if err := a.store.SetCodexAttemptTerminal(bg, run.attemptID, status, string(n.Params), usageJSON); err != nil {
		// Terminal persistence failed: the outcome cannot be resolved,
		// so the attempt stays uncertain and the durable block persists.
		run.stream.CloseWithErr(fmt.Errorf("terminal persistence: %w", err))
		return
	}
	run.notifyTerminal(status)
	turnStatus := council.TurnCompleted
	if status == "failed" {
		turnStatus = council.TurnFailed
	}
	_ = run.stream.SendOrOverflow(adapter.Event{
		Ref: run.ref, Type: adapter.EventTerminal,
		Status: turnStatus, Payload: truncateEventPayload(string(n.Params)),
		Usage: usageFromJSON(usageJSON),
	})
	run.stream.Close()
}

// confirmRolloutTurn reads the rollout tail past the attempt's baseline
// and returns (diagnostic, drift, pdigMatched, ok): the §3.5 step-3
// turn_context signal for the attempt's protection class and whether
// the §3.7 ordered correlation matched the attempt's stored prompt
// digest. ok=false marks an unreadable or corrupt rollout (protected
// mode treats that as Uncertain); drift carries the first pinned field
// the native turn_context disagreed on.
func (a *CodexAdapter) confirmRolloutTurn(run *codexTurnRun, attempt *storage.CodexTurnAttempt) (diagnostic, drift string, pdigMatched, ok bool) {
	binding, err := a.store.GetCodexSessionBinding(context.Background(), string(run.ref.SessionID))
	if err != nil || binding == nil || !binding.Materialized || binding.RolloutPath == nil {
		return "", "", false, false
	}
	entries, err := readRolloutTail(*binding.RolloutPath, attempt.BaselineSize)
	if err != nil {
		return "", "", false, false
	}
	if len(entries) == 0 {
		return "", "", false, true
	}
	pins, err := RolloutPinsFor(a.policy, binding.Model, binding.Workspace)
	if err != nil {
		return "", "", false, false
	}
	ev, err := CorrelateRolloutTurn(entries, pins, run.nativeThreadID, run.ref.TurnKey, run.attemptID, attempt.PromptDigest)
	if err != nil {
		return "", "", false, false
	}
	if ev.Drift != nil {
		return fmt.Sprintf("rollout turn_context drift on %s: frozen %q, native %q",
			ev.Drift.Field, ev.Drift.Want, ev.Drift.Have), rollDriftText(ev.Drift), ev.PromptDigestMatch, true
	}
	if ev.TurnContextMatch && ev.PromptDigestMatch {
		return "", "", true, true
	}
	return "rollout correlation incomplete: turn_context match=" +
		fmt.Sprint(ev.TurnContextMatch) + " prompt digest match=" + fmt.Sprint(ev.PromptDigestMatch), "", false, true
}

func rollDriftText(d *RolloutPinDrift) string {
	return d.Field + ": frozen " + d.Want + ", native " + d.Have
}

// mirrorTurnNotification fans one routed notification into the bounded
// observation stream: item/* notifications become progress events
// (payload clamped with a visible truncation marker), and cumulative
// token-usage snapshots are adopted monotonically and mirrored as
// usage-bearing progress events. An overflow closes the OBSERVER stream
// only; the native stream and the turn are unaffected (AC-006).
func (a *CodexAdapter) mirrorTurnNotification(run *codexTurnRun, n NativeNotification) {
	if strings.HasPrefix(n.Method, "item/") {
		_ = run.stream.SendOrOverflow(adapter.Event{
			Ref: run.ref, Type: adapter.EventProgress,
			Status:  council.TurnRunning,
			Payload: truncateEventPayload(string(n.Params)),
		})
		return
	}
	if n.Method == "thread/tokenUsage/updated" {
		if in, out, ok := parseTokenUsageSnapshot(n.Params); ok && run.observeUsage(in, out) {
			_ = run.stream.SendOrOverflow(adapter.Event{
				Ref: run.ref, Type: adapter.EventProgress,
				Status:  council.TurnRunning,
				Payload: "token usage snapshot",
				Usage: adapter.ExecutionUsage{
					InputTokens:  adapter.UsageMetric[int64]{Value: in, Available: true},
					OutputTokens: adapter.UsageMetric[int64]{Value: out, Available: true},
					TotalCostUSD: adapter.UsageMetric[float64]{Available: false},
				},
			})
		}
	}
}

// parseTokenUsageSnapshot extracts the cumulative token counts from a
// thread/tokenUsage/updated notification, tolerating the observed
// nesting variants (info.total.token_usage, total.token_usage, flat).
func parseTokenUsageSnapshot(params json.RawMessage) (in, out int64, ok bool) {
	if len(params) == 0 {
		return 0, 0, false
	}
	var p struct {
		Info *struct {
			Total *struct {
				TokenUsage *struct {
					InputTokens  *int64 `json:"input_tokens"`
					OutputTokens *int64 `json:"output_tokens"`
				} `json:"token_usage"`
			} `json:"total"`
		} `json:"info"`
		Total *struct {
			TokenUsage *struct {
				InputTokens  *int64 `json:"input_tokens"`
				OutputTokens *int64 `json:"output_tokens"`
			} `json:"token_usage"`
		} `json:"total"`
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return 0, 0, false
	}
	switch {
	case p.Info != nil && p.Info.Total != nil && p.Info.Total.TokenUsage != nil &&
		p.Info.Total.TokenUsage.InputTokens != nil && p.Info.Total.TokenUsage.OutputTokens != nil:
		return *p.Info.Total.TokenUsage.InputTokens, *p.Info.Total.TokenUsage.OutputTokens, true
	case p.Total != nil && p.Total.TokenUsage != nil &&
		p.Total.TokenUsage.InputTokens != nil && p.Total.TokenUsage.OutputTokens != nil:
		return *p.Total.TokenUsage.InputTokens, *p.Total.TokenUsage.OutputTokens, true
	case p.InputTokens != nil && p.OutputTokens != nil:
		return *p.InputTokens, *p.OutputTokens, true
	default:
		return 0, 0, false
	}
}

// usageFromJSON parses a stored result_usage snapshot into the AC-006
// usage shape. Cost is ALWAYS Available=false — it is never fabricated.
func usageFromJSON(raw string) adapter.ExecutionUsage {
	usage := adapter.ExecutionUsage{
		TotalCostUSD: adapter.UsageMetric[float64]{Available: false},
	}
	if strings.TrimSpace(raw) == "" {
		return usage
	}
	var parsed struct {
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return usage
	}
	if parsed.InputTokens != nil {
		usage.InputTokens = adapter.UsageMetric[int64]{Value: *parsed.InputTokens, Available: true}
	}
	if parsed.OutputTokens != nil {
		usage.OutputTokens = adapter.UsageMetric[int64]{Value: *parsed.OutputTokens, Available: true}
	}
	return usage
}

// abandonPreAcceptanceRun deregisters a run whose turn/start did not
// reach a verified acceptance (§3.9 taxonomy): the run leaves the live
// map and its observation stream closes with the reason. Any denial
// mirrored while the write was in flight remains honest evidence on the
// attempt; the classification itself is classifyTurnStartFailure's.
func (a *CodexAdapter) abandonPreAcceptanceRun(ref adapter.TurnRef, run *codexTurnRun, reason string) {
	a.mu.Lock()
	if cur, ok := a.turns[ref]; ok && cur == run {
		delete(a.turns, ref)
	}
	a.mu.Unlock()
	run.stream.CloseWithErr(fmt.Errorf("turn/start did not reach a verified acceptance: %s", reason))
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

// ── Observe / Cancel / Collect (§3.9) ───────────────────────────────────
//
// Observe hands out the turn's bounded observation stream: events mirror
// item/* progress and cumulative token-usage snapshots, and terminate
// with the verified terminal event. Overflow terminates the OBSERVER tap
// only (ErrBufferOverflow) — the native stream and the turn are
// unaffected; cancelling the caller's context detaches the observer and
// never touches the turn (AC-006: client disconnect is not
// cancellation).

func (a *CodexAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	a.mu.Lock()
	run, live := a.turns[ref]
	a.mu.Unlock()
	if !live || run == nil || run.stream == nil {
		return nil, fmt.Errorf("turn %s/%s is not dispatched or already completed", ref.SessionID, ref.TurnKey)
	}
	// Detach watcher: the observer's context bounds only its own
	// observation. Cancelling it closes this stream (release the
	// buffers); the turn, the tap, and the child continue.
	go func() {
		select {
		case <-ctx.Done():
			_ = run.stream.Close()
		case <-run.stream.Done():
		}
	}()
	return run.stream, nil
}

// cancelTerminalGrace bounds the wait for a verified terminal after an
// accepted turn/interrupt. Package-level so tests can shorten it. When
// it fires, the child is gracefully terminated then killed; without a
// verified terminal the attempt stays Uncertain (§3.9).
var cancelTerminalGrace = 5 * time.Second

// Cancel applies the §3.9 interruption semantics: an accepted
// turn/interrupt request is CancelRequested (the slot is NOT released —
// the turn may still run); only a verified interrupted terminal is
// CancelConfirmed; a turn already at a verified terminal is
// CancelAlreadyTerminal; a definitive native refusal is CancelRejected;
// a lost or timed-out interruption escalates to graceful terminate →
// kill and reports CancelUnknown (the attempt stays Uncertain without
// terminal evidence).
func (a *CodexAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	a.mu.Lock()
	run := a.turns[ref]
	var tap *TurnTap
	if run != nil {
		tap = run.tap
	}
	a.mu.Unlock()

	if run == nil {
		// Not live: durable state decides between already-terminal and
		// unknown — never a fabricated disposition.
		attempt, err := a.store.GetLatestCodexTurnAttempt(ctx, string(ref.SessionID), ref.TurnKey)
		if err == nil && attempt != nil && attempt.Terminal {
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelAlreadyTerminal,
				Reason: "turn already reached a verified terminal (" + attempt.ObservedStatus + ")"}, nil
		}
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
			Reason: "turn not dispatched"}, nil
	}
	if tap == nil {
		// Pre-route window (§3.9): the run is registered but the
		// turn/start ack is still pending, so no turn route exists and
		// the interrupt cannot be addressed. The run and its slot stay
		// held; report unknown without blocking the caller.
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
			Reason: "turn is still awaiting its dispatch ack; no interrupt route exists yet"}, nil
	}
	if _, terminal := run.terminalStatus(); terminal {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelAlreadyTerminal,
			Reason: "turn already reached a verified terminal"}, nil
	}

	// The interrupt rides an adapter-owned bounded context: a
	// disconnected controller cannot abandon the request mid-write.
	ictx, icancel := context.WithTimeout(context.WithoutCancel(ctx), dispatchAckTimeout)
	defer icancel()
	err := run.child.client.TurnInterrupt(ictx, run.nativeThreadID, tap.TurnID())
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			// Definitive native refusal of the interrupt itself.
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelRejected,
				Reason: "turn/interrupt was refused: " + rpcErr.Error()}, nil
		}
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
			Reason: "turn/interrupt outcome ambiguous: " + err.Error()}, nil
	}
	if run.markCancelRequested() {
		_ = run.stream.SendOrOverflow(adapter.Event{
			Ref: run.ref, Type: adapter.EventProgress,
			Status: council.TurnCancelling, Payload: "turn/interrupt accepted",
		})
	}

	// Bounded wait for the verified terminal. The request alone is NOT
	// evidence: only the interrupted terminal confirms the cancel.
	timer := time.NewTimer(cancelTerminalGrace)
	defer timer.Stop()
	awaitBuffered := func() (string, bool) {
		// The run loop drains buffered notifications when the child's
		// stream ends; give that a brief beat before concluding.
		deadline := time.Now().Add(250 * time.Millisecond)
		for {
			if observed, ok := run.terminalStatus(); ok {
				return observed, true
			}
			if time.Now().After(deadline) {
				return "", false
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	for {
		select {
		case <-run.termCh:
			// The channel closes exactly once when a verified terminal
			// is committed; the status lives on the run.
			if observed, ok := run.terminalStatus(); ok && observed == "interrupted" {
				return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed,
					Reason: "verified interrupted terminal"}, nil
			} else if ok {
				return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelAlreadyTerminal,
					Reason: "turn reached a verified terminal around the interrupt"}, nil
			}
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
				Reason: "terminal signal lost"}, nil
		case <-tap.Done():
			if observed, ok := awaitBuffered(); ok {
				if observed == "interrupted" {
					return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed,
						Reason: "verified interrupted terminal"}, nil
				}
				return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelAlreadyTerminal,
					Reason: "turn reached a verified terminal (" + observed + ") around the interrupt"}, nil
			}
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
				Reason: "native stream ended after the interrupt without a verified terminal"}, nil
		case <-run.child.conn.Done():
			if observed, ok := awaitBuffered(); ok {
				if observed == "interrupted" {
					return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed,
						Reason: "verified interrupted terminal"}, nil
				}
				return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelAlreadyTerminal,
					Reason: "turn reached a verified terminal (" + observed + ") around the interrupt"}, nil
			}
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
				Reason: "native stream ended after the interrupt without a verified terminal"}, nil
		case <-ctx.Done():
			// The requesting controller went away: the request stands,
			// the in-life escalation remains armed, the slot is NOT
			// released.
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelRequested,
				Reason: "turn/interrupt accepted; awaiting the verified terminal"}, nil
		case <-timer.C:
			// Deadline exceeded: graceful terminate → kill (§3.9).
			// Without a verified terminal the attempt stays Uncertain.
			a.server.stop(context.Background(), ref.SessionID)
			if observed, ok := awaitBuffered(); ok {
				if observed == "interrupted" {
					return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed,
						Reason: "verified interrupted terminal"}, nil
				}
				return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelAlreadyTerminal,
					Reason: "turn reached a verified terminal (" + observed + ") during termination"}, nil
			}
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
				Reason: "interrupt accepted but no terminal arrived within the bound; child terminated — attempt uncertain"}, nil
		}
	}
}

// Collect reports from durable state only, gated on THIS turn's verified
// terminal (the route-bound native turn id from dispatch): ResultPending
// until then; a verified terminal returns the recorded result payload
// and usage; a stored payload that is not well-formed JSON evidence is
// ResultMalformed, never reinterpreted. Cost is always Available=false.
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
	if !attempt.Terminal || attempt.ResultPayload == nil {
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
			ResultStatus: adapter.ResultPending}, nil
	}
	// Terminal gating: a terminal not attributable to THIS turn's bound
	// native turn id is not collectable evidence.
	if attempt.NativeTurnID == nil || strings.TrimSpace(*attempt.NativeTurnID) == "" {
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
			ResultStatus: adapter.ResultUnavailable}, nil
	}
	// Malformed evidence is reported, never reinterpreted.
	var sanity map[string]any
	if err := json.Unmarshal([]byte(*attempt.ResultPayload), &sanity); err != nil {
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
			ResultStatus: adapter.ResultMalformed}, nil
	}
	usage := usageFromJSON(ptrStr(attempt.ResultUsage))
	switch attempt.ObservedStatus {
	case "completed":
		return adapter.TurnResult{Ref: ref, Status: council.TurnCompleted,
			ResultStatus: adapter.ResultAvailable,
			Output:       *attempt.ResultPayload,
			Usage:        usage,
			CompletedAt:  time.Now().UTC(),
		}, nil
	case "failed":
		return adapter.TurnResult{Ref: ref, Status: council.TurnFailed,
			ResultStatus: adapter.ResultFailed,
			Output:       *attempt.ResultPayload,
			Usage:        usage,
			CompletedAt:  time.Now().UTC(),
		}, nil
	case "interrupted":
		// Verified interrupted terminal (§3.9): TurnCancelled, and the
		// turn produced no consumable result.
		return adapter.TurnResult{Ref: ref, Status: council.TurnCancelled,
			ResultStatus: adapter.ResultUnavailable,
			Usage:        usage,
			CompletedAt:  time.Now().UTC(),
		}, nil
	default:
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning,
			ResultStatus: adapter.ResultUnavailable}, nil
	}
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// handleApprovalRequest routes one server→child request to the §3.6
// approval responder, replying through the session's live child. The
// lookup is per call and bounded-retried: a request racing the handshake
// (e.g. emitted before the initialize response) must still be answered
// once the child registers.
func (a *CodexAdapter) handleApprovalRequest(sessionID adapter.SessionID, sr ServerRequest) {
	reply := func(id json.RawMessage, result any) error {
		deadline := time.Now().Add(approvalReplyResolutionGrace)
		for {
			child, err := a.server.child(sessionID)
			if err == nil {
				return child.conn.Respond(id, result)
			}
			if time.Now().After(deadline) {
				return err
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	newResponder(a, sessionID, reply).HandleRequest(sr)
}

// runForNativeThread returns the live turn run bound to a native thread,
// or nil. Single-flight guarantees at most one live turn per thread; the
// run is registered BEFORE turn/start is written so an approval request
// racing the dispatch ack still finds its turn.
func (a *CodexAdapter) runForNativeThread(threadID string) *codexTurnRun {
	if threadID == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, run := range a.turns {
		if run.nativeThreadID == threadID {
			return run
		}
	}
	return nil
}

// slotHeld reports whether the single-flight slot for a native thread is
// held: a dispatch is in flight even in the window before its run is
// observable.
func (a *CodexAdapter) slotHeld(nativeID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, held := a.singleFlt[nativeID]
	return held
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

// checkMCPInventory verifies the session child's live mcpServerStatus/list
// inventory against the frozen ExpectedMCPServers set (spec §3.8): exact
// set equality — an unknown server and a missing server are both drift;
// an unavailable or unparseable inventory fails closed. The typed
// ErrProfileDrift fires BEFORE any prompt byte is written. The response
// item shape is not pinned by the committed schema evidence (not
// live-exercised at 0.154.0 research time), so name extraction is
// tolerant — each entry may be a bare name string or an object carrying a
// "name" field — and any other shape is drift, never a guess.
func (a *CodexAdapter) checkMCPInventory(ctx context.Context, child *codexChild, nativeID string) *ErrProfileDrift {
	frozen := append([]string(nil), a.policy.ExpectedMCPServers...)
	sort.Strings(frozen)
	want := strings.Join(frozen, ", ")
	raw, err := child.client.MCPServerStatusList(ctx)
	if err != nil {
		return &ErrProfileDrift{ThreadID: nativeID, Field: "mcpServers",
			Want: want, Have: "inventory unavailable: " + err.Error()}
	}
	observed, err := mcpServerNames(raw)
	if err != nil {
		return &ErrProfileDrift{ThreadID: nativeID, Field: "mcpServers",
			Want: want, Have: "unparseable inventory: " + err.Error()}
	}
	sort.Strings(observed)
	if strings.Join(frozen, ", ") != strings.Join(observed, ", ") {
		return &ErrProfileDrift{ThreadID: nativeID, Field: "mcpServers",
			Want: want, Have: strings.Join(observed, ", ")}
	}
	return nil
}

// mcpServerNames extracts the server names from a mcpServerStatus/list
// result. Recognized shapes: {"servers":[…]} or a bare […]; each entry a
// JSON string or an object with a non-empty "name" string. Anything else
// is an error (fail closed).
func mcpServerNames(raw json.RawMessage) ([]string, error) {
	var top struct {
		Servers []json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		var arr []json.RawMessage
		if arrErr := json.Unmarshal(raw, &arr); arrErr != nil {
			return nil, fmt.Errorf("response is neither {\"servers\":[…]} nor an array: %w", err)
		}
		top.Servers = arr
	}
	names := make([]string, 0, len(top.Servers))
	for i, entry := range top.Servers {
		var name string
		if json.Unmarshal(entry, &name) == nil && strings.TrimSpace(name) != "" {
			names = append(names, name)
			continue
		}
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(entry, &obj) == nil && strings.TrimSpace(obj.Name) != "" {
			names = append(names, obj.Name)
			continue
		}
		return nil, fmt.Errorf("server entry %d carries no recognizable name", i)
	}
	return names, nil
}

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

// ── Rollout baseline (first acceptance; §3.7) ───────────────────────────

// lookupCoveredAttestation is the durable half of the §3.3/§3.7
// attestation lookup shared by the production eligibility seam and the
// launch-time protection freeze: the cprot-v2 row must match the frozen
// (codex version, platform, manifest digest, profile digest) tuple
// EXACTLY, and its record frame must decode and COVER the frozen
// profile (coverage.go — every enabled path, every mutation operation,
// every pinned approval method). A tuple match whose records do not
// cover the profile is reported as no attestation ("", nil): absent or
// partial evidence never unlocks anything. A storage failure is
// returned as an error so protection is never silently downgraded.
func lookupCoveredAttestation(ctx context.Context, store *storage.Store, policy CodexLaunchPolicy, profileDigest string) (string, error) {
	id, err := store.FindCodexProtectionAttestation(ctx,
		policy.AppServerVersion, codexPlatformIdentity(policy), policy.ManifestDigest, profileDigest)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(id) == "" {
		return "", nil
	}
	raw, err := store.CodexProtectionProbeResults(ctx, id)
	if err != nil {
		return "", err
	}
	if err := CoverageFor(policy, profileDigest).ValidateFrame(raw); err != nil {
		return "", nil
	}
	return id, nil
}

// resolveRolloutProtection freezes the rollout protection class for a
// new attempt (§3.7): the isolation-attestation seam must report a valid
// attestation AND a durable cprot-v2 row must exist matching the frozen
// (codex version, platform, manifest digest, profile digest) tuple for
// the same id, with records covering the frozen profile. Advisory is the default; the unverified platform degrades
// to integrity=unverified. A lookup FAILURE is returned — protection is
// never silently downgraded by an error (the AC-008 rule).
func (a *CodexAdapter) resolveRolloutProtection(ctx context.Context) (string, *string, error) {
	seamID, seamOK := "", false
	if a.attestation != nil {
		seamID, seamOK = a.attestation()
	}
	rowID := ""
	if seamOK && strings.TrimSpace(seamID) != "" {
		var err error
		rowID, err = lookupCoveredAttestation(ctx, a.store, a.policy, a.profileDigest)
		if err != nil {
			return "", nil, err
		}
	}
	class, id := rolloutProtectionClass(runtime.GOOS, seamOK, seamID, rowID)
	if id == "" {
		return class, nil, nil
	}
	frozen := id
	return class, &frozen, nil
}

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

// recordFirstAcceptance resolves the rollout via ResolveRollout
// (bounded UUID-suffix scan under the frozen CODEX_HOME sessions root,
// session_meta verified, path integrity enforced) and records the
// baseline (file-identity, byte size, entry count) plus the binding
// materialization in one durable transition.
func (a *CodexAdapter) recordFirstAcceptance(sessionID, attemptID, nativeID string) error {
	path, err := ResolveRollout(a.policy.ExpectedCodexHome, nativeID)
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
