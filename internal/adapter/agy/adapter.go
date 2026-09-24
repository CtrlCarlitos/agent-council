package agy

// AC-006 adapter contract over the agy CLI (AC-010 spec §3.1–§3.6,
// §3.9–§3.11): one sealed, executor-launched process per turn, the
// prompt only on stdin, and every launch's init verified BEFORE any
// prompt byte exists on the wire.
//
//   - CreateSession: eligibility → persisted-binding idempotency →
//     toolkit configured state (Task 6) →
//     frozen creation launch (no --conversation) → init verified (UUIDv4
//     id, permission mode, model, cwd, tools) → stdin closed WITHOUT a
//     message → bounded exit. The adapter never persists: the service
//     binds (BindAgySession). Concurrent duplicates share one in-memory
//     reservation; an uncertain creation tombstones the session.
//   - ResumeSession: local inspection only (binding + conversation file).
//   - Dispatch: single flight per native conversation → durable block
//     check → eligibility → toolkit configured state → models auth
//     gate (Task 6) → attempt+pdig AND launch reservation in ONE
//     transaction → sealed Start under the inherited operator HOME (exe
//     identity recorded) → init verified BEFORE the
//     write (drift ⇒ child terminated, prompt never written, Rejected) →
//     first stdin byte recorded at the write boundary → stdin closed →
//     the turn goroutine owns acceptance (user_input DONE), tool
//     observation, required-tool verification and the single terminal.
//   - Cancel: SIGINT ⇒ CancelRequested; CancelConfirmed only on the
//     verified result{ERROR,"interrupted"}; grace ⇒ terminate ⇒ Uncertain.
//   - Collect/Reconcile: durable evidence only; the in-life result is the
//     only terminal evidence (advisory reconciliation, §3.10).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ── Typed failures ──────────────────────────────────────────────────────

// ErrProductionEligibilityMissing reports a launch refused because no
// valid isolation attestation exists for the frozen tuple (spec §3.2
// gate step 1). No process was started.
type ErrProductionEligibilityMissing struct {
	SessionID adapter.SessionID
	Reason    string
}

func (e *ErrProductionEligibilityMissing) Error() string {
	return fmt.Sprintf("agy production eligibility missing for session %s: %s", e.SessionID, e.Reason)
}

// ErrConversationDrift reports an init.conversation_id that is not the
// requested id (a resume that silently fell back to a NEW conversation,
// hazard 2) or, on creation, not a UUIDv4. The child was terminated and
// no prompt was written; Observed is the orphan id, kept as a diagnostic
// and never bound.
type ErrConversationDrift struct {
	Requested string
	Observed  string
}

func (e *ErrConversationDrift) Error() string {
	if e.Requested == "" {
		return fmt.Sprintf("agy conversation drift: creation id %q is not a UUIDv4", e.Observed)
	}
	return fmt.Sprintf("agy conversation drift: requested %s, init reported %q (orphan id recorded as diagnostic)", e.Requested, e.Observed)
}

// ErrProfileDrift reports an init field (permission_mode, model, cwd)
// that disagrees with the frozen profile: pre-acceptance, the prompt was
// never written.
type ErrProfileDrift struct {
	Field string
	Want  string
	Have  string
}

func (e *ErrProfileDrift) Error() string {
	return fmt.Sprintf("agy profile drift on %s: frozen %q, init reported %q", e.Field, e.Want, e.Have)
}

// ErrToolInventoryDrift reports init.tools ≠ the frozen expected_tools
// set (an empty or missing inventory included): pre-acceptance.
type ErrToolInventoryDrift struct {
	Missing    []string
	Unexpected []string
	Reason     string
}

func (e *ErrToolInventoryDrift) Error() string {
	if e.Reason != "" {
		return "agy tool inventory drift: " + e.Reason
	}
	return fmt.Sprintf("agy tool inventory drift: missing %v, unexpected %v", e.Missing, e.Unexpected)
}

// ErrSessionConfigMismatch reports a caller whose config disagrees with
// the persisted binding or the frozen launch: fail closed, never
// relabeled.
type ErrSessionConfigMismatch struct {
	SessionID adapter.SessionID
	Field     string
	Want      string
	Have      string
}

func (e *ErrSessionConfigMismatch) Error() string {
	return fmt.Sprintf("agy session %s config mismatch on %s: bound %q, request %q",
		e.SessionID, e.Field, e.Have, e.Want)
}

// ErrConversationBusy reports a dispatch rejected by single flight: an
// in-flight turn already holds the native conversation (spec §3.5).
var ErrConversationBusy = errors.New("agy conversation busy: an in-flight turn holds the native conversation")

// Adapter-owned bounds. Package-level so tests can shorten them; caller
// contexts never bound the child, its readers, or these waits.
var (
	initTimeout         = 30 * time.Second
	creationExitTimeout = 10 * time.Second
	cancelGrace         = 5 * time.Second
	postResultExitGrace = 10 * time.Second
	terminateTimeout    = 3 * time.Second

	// turnBoundFor is Council's turn bound, enforced by SIGINT (§3.5):
	// strictly inside the --print-timeout backstop so the backstop never
	// fires first on a healthy run.
	turnBoundFor = func(p AgyLaunchPolicy) time.Duration {
		b := p.PrintTimeoutBackstop - 30*time.Second
		if half := p.PrintTimeoutBackstop / 2; b < half {
			b = half
		}
		return b
	}
)

// finishedCap bounds how many classified runs keep their in-memory
// history for replay after they leave the live map.
const finishedCap = 64

// historyCap bounds the in-memory event history replayed to a late
// observer; it stays below the observer buffer so replay never overflows.
const historyCap = adapter.DefaultBufferCapacity - 16

// executorIdentity names the launch mechanism on the reservation row.
const executorIdentity = "agy-process-per-turn"

// ── Adapter ─────────────────────────────────────────────────────────────

// AgyAdapter implements the AC-006 contract for the agy CLI.
type AgyAdapter struct {
	store         *storage.Store
	executor      execpolicy.PolicyExecutor
	launch        AgyTurnLaunchSource
	allocations   AllocationLookup
	policy        AgyLaunchPolicy
	profileDigest string
	image         *execpolicy.SealedImage
	identity      AttemptIdentitySource
	required      RequiredToolsSource
	attestation   AttestationLookup
	// fixtureScope is the explicit test-only construction marker: set
	// ONLY by NewFixtureScopedAdapter. It skips the production-
	// eligibility gate and admits the fixture/diagnostic launch-matrix
	// rows; every protocol validation stays in force.
	fixtureScope bool

	mu    sync.Mutex
	turns map[adapter.TurnRef]*agyTurnRun
	// finished keeps the most recent finishedCap classified runs for
	// in-process history replay (bounded; durable replay covers the rest).
	finished      map[adapter.TurnRef]*agyTurnRun
	finishedOrder []adapter.TurnRef
	slots         map[string]string // native id → owning attempt

	createMu  sync.Mutex
	creations map[adapter.SessionID]*agyCreationCall
	uncertain map[adapter.SessionID]error

	// materializeErrs holds a failed post-acceptance materialization
	// record per session; ResumeSession retries it and fails while the
	// record cannot be written.
	materializeErrs map[adapter.SessionID]error

	// fault is the test-only fault-injection seam on durable
	// transitions (nil in production): a non-nil return replaces the
	// store call with that error.
	fault func(op string) error

	// preLaunchCheck is the per-launch toolkit configured-state check
	// (Task 6, toolkit.go): run after the launch is assembled and
	// validated and before the creation or turn child starts (a turn's
	// before its durable reservation). Every constructor sets it to
	// toolkitCheck — the fixture scope included (validation, not
	// eligibility); a non-nil error refuses the launch, no child started.
	preLaunchCheck func(ctx context.Context, sessionID adapter.SessionID) error

	// The `models` auth gate's per-session pass cache (authgate.go):
	// authPassed[session] is when the gate last passed; a pass older
	// than authTTL (0 = never cached) re-runs the gate; ResumeSession
	// and any failure drop the entry. now is the clock (injectable).
	authMu     sync.Mutex
	authPassed map[adapter.SessionID]time.Time
	authTTL    time.Duration
	now        func() time.Time
}

// Durable transition names passed to the fault seam.
const (
	opNativeStepIndex = "native_step_index"
	opLaunchDead      = "launch_dead"
	opAttemptMissing  = "attempt_missing"
	opMaterialize     = "materialize"
	opOrphan          = "orphan_conversation"
)

// durable runs one durable transition through the fault seam.
func (a *AgyAdapter) durable(op string, fn func() error) error {
	if a.fault != nil {
		if err := a.fault(op); err != nil {
			return err
		}
	}
	return fn()
}

var _ adapter.Adapter = (*AgyAdapter)(nil)

// NewAgyAdapter is the production constructor. Options are decoded
// through applyConstructionOptions, so the test-only fixture option fails
// closed with ErrFixtureModeProhibited. Production requires the
// attestation lookup, the sealed image of the pinned binary (its digest
// must equal the frozen binary digest), and a Linux frozen platform on a
// Linux host (spec §3.12).
func NewAgyAdapter(
	store *storage.Store,
	executor execpolicy.PolicyExecutor,
	launch AgyTurnLaunchSource,
	allocations AllocationLookup,
	policy AgyLaunchPolicy,
	profileDigest string,
	image *execpolicy.SealedImage,
	identity AttemptIdentitySource,
	required RequiredToolsSource,
	attestation AttestationLookup,
	opts ...ConstructionOption,
) (*AgyAdapter, error) {
	if _, err := applyConstructionOptions(opts...); err != nil {
		return nil, err
	}
	if attestation == nil {
		return nil, errors.New("production agy construction requires the attestation lookup")
	}
	if image == nil {
		return nil, errors.New("production agy launches are sealed: the sealed image of the pinned binary is required")
	}
	if runtime.GOOS != "linux" || policy.PlatformOS != "linux" || policy.PlatformFamily != "unix" {
		return nil, &ErrUnsupportedProfile{AlgoVersion: "cprof-v4",
			Reason: fmt.Sprintf("production agy is eligible only for the linux/unix platform on a linux host (frozen %s/%s, host %s)",
				policy.PlatformOS, policy.PlatformFamily, runtime.GOOS)}
	}
	return buildAgyAdapter(store, executor, launch, allocations, policy, profileDigest, image, identity, required, attestation, false)
}

// NewFixtureScopedAdapter builds an AgyAdapter under the explicit
// test-only fixture scope: production eligibility is skipped and the
// network-none launch-matrix rows are admitted; nothing else changes.
// The marker argument is mandatory (never inferred). For tests and the
// operator-invoked manual evidence executable only — production wiring
// has no path to it (agytest guard).
func NewFixtureScopedAdapter(
	store *storage.Store,
	executor execpolicy.PolicyExecutor,
	launch AgyTurnLaunchSource,
	allocations AllocationLookup,
	policy AgyLaunchPolicy,
	profileDigest string,
	image *execpolicy.SealedImage,
	identity AttemptIdentitySource,
	required RequiredToolsSource,
	_ FixtureMode,
) (*AgyAdapter, error) {
	return buildAgyAdapter(store, executor, launch, allocations, policy, profileDigest, image, identity, required, nil, true)
}

func buildAgyAdapter(
	store *storage.Store,
	executor execpolicy.PolicyExecutor,
	launch AgyTurnLaunchSource,
	allocations AllocationLookup,
	policy AgyLaunchPolicy,
	profileDigest string,
	image *execpolicy.SealedImage,
	identity AttemptIdentitySource,
	required RequiredToolsSource,
	attestation AttestationLookup,
	fixtureScope bool,
) (*AgyAdapter, error) {
	switch {
	case store == nil:
		return nil, errors.New("agy adapter requires the store")
	case executor == nil:
		return nil, errors.New("agy adapter requires the policy executor")
	case launch == nil:
		return nil, errors.New("agy adapter requires the launch source")
	case allocations == nil:
		return nil, errors.New("agy adapter requires the AC-005 allocation lookup")
	case identity == nil:
		return nil, errors.New("agy adapter requires the attempt identity source")
	case strings.TrimSpace(profileDigest) == "":
		return nil, errors.New("agy adapter requires the frozen profile digest")
	case strings.TrimSpace(policy.Model) == "":
		return nil, errors.New("agy adapter requires the frozen model")
	}
	if image != nil && image.Digest != policy.BinaryDigest {
		return nil, fmt.Errorf("sealed image digest %s differs from the frozen binary digest %s", image.Digest, policy.BinaryDigest)
	}
	if _, err := agyHomeDir(policy.ExpectedHome); err != nil {
		return nil, err
	}
	a := &AgyAdapter{
		store: store, executor: executor, launch: launch, allocations: allocations, policy: policy,
		profileDigest: profileDigest, image: image, identity: identity,
		required: required, attestation: attestation, fixtureScope: fixtureScope,
		turns:     make(map[adapter.TurnRef]*agyTurnRun),
		finished:  make(map[adapter.TurnRef]*agyTurnRun),
		slots:     make(map[string]string),
		creations: make(map[adapter.SessionID]*agyCreationCall),
		uncertain: make(map[adapter.SessionID]error),

		materializeErrs: make(map[adapter.SessionID]error),

		authPassed: make(map[adapter.SessionID]time.Time),
		authTTL:    defaultAuthGateTTL,
		now:        time.Now,
	}
	a.preLaunchCheck = a.toolkitCheck
	return a, nil
}

// ── Probe ───────────────────────────────────────────────────────────────

// Probe reports the adapter's FROZEN identity (CLI version, model) and
// design capabilities without launching anything: the model inventory
// is the frozen pin, not a live catalog (the `models` auth gate runs
// before a session's first dispatch, authgate.go, never from Probe).
func (a *AgyAdapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	cancellation := adapter.CapabilitySupported
	if runtime.GOOS == "windows" {
		cancellation = adapter.CapabilityUnsupported // SIGINT is POSIX (§3.12)
	}
	return adapter.ProbeReport{
		HarnessVersion: adapter.UsageMetric[string]{Value: a.policy.CLIVersion, Available: true},
		Capabilities: adapter.AdapterCapabilities{
			SessionResumption:    adapter.CapabilitySupported,
			MidTurnCancellation:  cancellation,
			ToolApprovalRouting:  adapter.CapabilityUnsupported, // native auto-deny (§3.2)
			StreamingObservation: adapter.CapabilitySupported,
			StructuredOutput:     adapter.CapabilityUnsupported,
		},
		ModelInventory: adapter.UsageMetric[[]string]{Value: []string{a.policy.Model}, Available: true},
	}, nil
}

// ── CreateSession ───────────────────────────────────────────────────────

type agyCreationCall struct {
	done        chan struct{}
	binding     adapter.SessionBinding
	err         error
	contributor council.Contributor
	model       string
	workspace   string
}

// CreateSession creates the native conversation provider-free (spec
// §3.3). See the file comment for the ordering.
func (a *AgyAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}
	if req.Contributor != council.Agy {
		return adapter.SessionBinding{}, fmt.Errorf("agy adapter cannot create a %q session", req.Contributor)
	}
	if strings.TrimSpace(req.Config.Model) == "" || strings.TrimSpace(req.Config.WorkspaceRoot) == "" {
		return adapter.SessionBinding{}, fmt.Errorf("session %s requires a frozen model and a workspace root", req.SessionID)
	}

	a.createMu.Lock()
	if cause, blocked := a.uncertain[req.SessionID]; blocked {
		a.createMu.Unlock()
		return adapter.SessionBinding{}, &adapter.ErrSessionCreationUncertain{
			SessionID: req.SessionID, Contributor: req.Contributor,
			Err: fmt.Errorf("creation uncertainty is unresolved for this session: %w", cause),
		}
	}
	if call, ok := a.creations[req.SessionID]; ok {
		a.createMu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			// This waiter gives up; the shared creation continues.
			return adapter.SessionBinding{}, ctx.Err()
		}
		if call.err != nil {
			return adapter.SessionBinding{}, call.err
		}
		if err := compareSharedCreation(req, call); err != nil {
			return adapter.SessionBinding{}, err
		}
		return call.binding, nil
	}
	call := &agyCreationCall{done: make(chan struct{}), contributor: req.Contributor,
		model: req.Config.Model, workspace: req.Config.WorkspaceRoot}
	a.creations[req.SessionID] = call
	a.createMu.Unlock()

	call.binding, call.err = a.createBinding(ctx, req)
	// The shared call ends here, success or failure: a later call re-runs
	// the durable uncertainty check and the persisted-binding compare.
	a.createMu.Lock()
	if call.err != nil {
		if _, unc := isCreationUncertain(call.err); unc {
			a.uncertain[req.SessionID] = call.err
		}
	}
	if a.creations[req.SessionID] == call {
		delete(a.creations, req.SessionID)
	}
	a.createMu.Unlock()
	close(call.done)
	return call.binding, call.err
}

func isCreationUncertain(err error) (*adapter.ErrSessionCreationUncertain, bool) {
	var unc *adapter.ErrSessionCreationUncertain
	ok := errors.As(err, &unc)
	return unc, ok
}

// ResolveCreationUncertainty clears the in-process creation tombstone
// (the service's explicit, controller-authorized resolution seam; the
// durable episode is resolved by the service). Reports whether one was
// cleared.
func (a *AgyAdapter) ResolveCreationUncertainty(sessionID adapter.SessionID) bool {
	a.createMu.Lock()
	defer a.createMu.Unlock()
	_, ok := a.uncertain[sessionID]
	delete(a.uncertain, sessionID)
	return ok
}

func compareSharedCreation(req adapter.CreateSessionRequest, call *agyCreationCall) error {
	switch {
	case req.Contributor != call.contributor:
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "contributor", Want: string(req.Contributor), Have: string(call.contributor)}
	case req.Config.Model != call.model:
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "model", Want: req.Config.Model, Have: call.model}
	case req.Config.WorkspaceRoot != call.workspace:
		return &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "workspace", Want: req.Config.WorkspaceRoot, Have: call.workspace}
	}
	return nil
}

// creationEpisodeKey carries the caller's own service-owned in-flight
// creation marker (storage.BeginAgyCreationInFlight) into CreateSession.
type creationEpisodeKey struct{}

type creationEpisode struct {
	sessionID adapter.SessionID
	episode   int64
}

// WithCreationEpisode marks ctx as the creation that OWNS the session's
// open in-flight marker `episode` (opened by the service immediately
// before this call). The adapter's durable block then admits exactly
// that unannotated marker — every other open episode (a crashed
// creation's marker, an annotated one, any uncertainty episode) still
// blocks. Drift/orphan records land on the marker itself
// (RecordAgyCreationUncertain annotates the one open episode), so one
// creation never yields two open episodes.
func WithCreationEpisode(ctx context.Context, sessionID adapter.SessionID, episode int64) context.Context {
	return context.WithValue(ctx, creationEpisodeKey{}, creationEpisode{sessionID: sessionID, episode: episode})
}

func ownsCreationEpisode(ctx context.Context, sessionID adapter.SessionID, open *storage.AgyCreationUncertaintyEpisode) bool {
	carried, ok := ctx.Value(creationEpisodeKey{}).(creationEpisode)
	return ok && open != nil && carried.sessionID == sessionID && carried.episode == open.Episode &&
		open.Disposition == nil && open.Reason == storage.AgyCreationInFlightReason
}

func (a *AgyAdapter) createBinding(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	// Durable §3.3 block: an open creation-uncertainty episode survives
	// restarts; no child may start until the controller resolves it —
	// except the caller's own, still-unannotated in-flight marker.
	if open, err := a.store.OpenAgyCreationUncertainty(ctx, string(req.SessionID)); err != nil {
		return adapter.SessionBinding{}, fmt.Errorf("creation uncertainty lookup: %w", err)
	} else if open != nil && !ownsCreationEpisode(ctx, req.SessionID, open) {
		return adapter.SessionBinding{}, &adapter.ErrSessionCreationUncertain{
			SessionID: req.SessionID, Contributor: req.Contributor,
			Err: fmt.Errorf("an open durable creation-uncertainty episode (%d) blocks recreation", open.Episode),
		}
	}
	if err := a.checkEligibility(req.SessionID); err != nil {
		return adapter.SessionBinding{}, err
	}
	if existing, err := a.store.GetAgySessionBinding(ctx, string(req.SessionID)); err != nil {
		return adapter.SessionBinding{}, err
	} else if existing != nil {
		if err := a.compareStoredBinding(req.SessionID, req.Config, existing); err != nil {
			return adapter.SessionBinding{}, err
		}
		return a.bindingFor(req, existing.NativeID), nil
	}

	launch, err := a.launch.AgyTurnLaunch(ctx, req.SessionID, "", "", LaunchCreate)
	if err != nil {
		return adapter.SessionBinding{}, fmt.Errorf("creation launch: %w", err)
	}
	if err := checkLaunchMatrix(launch.Profile, a.fixtureScope); err != nil {
		return adapter.SessionBinding{}, err
	}
	if req.Config.Model != launch.Model {
		return adapter.SessionBinding{}, &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "model", Want: req.Config.Model, Have: launch.Model}
	}
	if !sameDir(req.Config.WorkspaceRoot, launch.Paths.Root) {
		return adapter.SessionBinding{}, &ErrSessionConfigMismatch{SessionID: req.SessionID, Field: "workspace", Want: req.Config.WorkspaceRoot, Have: launch.Paths.Root}
	}
	if err := a.validateLaunch(ctx, launch, req.SessionID, LaunchCreate, "", launch.Model, ""); err != nil {
		return adapter.SessionBinding{}, err
	}
	if err := a.runPreLaunchCheck(ctx, req.SessionID); err != nil {
		return adapter.SessionBinding{}, err
	}

	proc, err := a.executor.Start(context.Background(), launch)
	if err != nil {
		// The executor proved no process exists: a clean rejection.
		return adapter.SessionBinding{}, fmt.Errorf("creation child start: %w", err)
	}
	p := watchProcess(proc)
	uncertain := func(partial string, cause error) (adapter.SessionBinding, error) {
		return adapter.SessionBinding{}, &adapter.ErrSessionCreationUncertain{
			SessionID: req.SessionID, Contributor: req.Contributor, PartialNativeID: partial, Err: cause,
		}
	}

	it, err := p.awaitInit(initTimeout)
	if err != nil {
		// A lost init may still have created a conversation file.
		p.kill()
		p.wait()
		return uncertain("", fmt.Errorf("creation init not observed: %w", err))
	}
	if err := a.verifyInit(it.ev, LaunchCreate, "", launch.Model, launch.Paths.Root); err != nil {
		var drift *ErrConversationDrift
		if errors.As(err, &drift) {
			p.kill()
			p.wait()
			return uncertain(drift.Observed, err)
		}
		// A valid UUIDv4 init already persisted a native conversation:
		// record the orphan id durably BEFORE terminating the child.
		orphan := it.ev.ConversationID
		episode, recErr := a.recordCreationOrphan(req, orphan, err)
		p.kill()
		p.wait()
		if recErr != nil {
			return uncertain(orphan, fmt.Errorf("%w; the orphan conversation id could not be recorded durably: %v", err, recErr))
		}
		return adapter.SessionBinding{}, &ErrCreationDrift{SessionID: req.SessionID, NativeID: orphan, Episode: episode, Err: err}
	}
	nativeID := it.ev.ConversationID

	// Close stdin WITHOUT any message (verified: exit 0, no turn, the
	// conversation file persisted), then wait bounded for the exit.
	if err := proc.Stdin().Close(); err != nil {
		p.kill()
		p.wait()
		return uncertain(nativeID, fmt.Errorf("creation stdin close: %w", err))
	}
	deadline := time.NewTimer(creationExitTimeout)
	defer deadline.Stop()
	for {
		select {
		case extra, ok := <-p.items:
			if ok {
				cause := &ErrProtocolDrift{Event: string(extra.ev.Kind), Reason: "creation child emitted an event after init with no input"}
				_, recErr := a.recordCreationOrphan(req, nativeID, cause)
				p.kill()
				p.wait()
				if recErr != nil {
					return uncertain(nativeID, fmt.Errorf("%w; the orphan conversation id could not be recorded durably: %v", cause, recErr))
				}
				return uncertain(nativeID, cause)
			}
			if p.readErr != nil {
				p.kill()
				p.wait()
				return uncertain(nativeID, fmt.Errorf("creation reader failed after init: %w", p.readErr))
			}
			code := p.settle(creationExitTimeout)
			if code != 0 {
				return uncertain(nativeID, fmt.Errorf("creation child exited %d after init", code))
			}
			return a.bindingFor(req, nativeID), nil
		case <-deadline.C:
			p.kill()
			p.wait()
			return uncertain(nativeID, errors.New("creation child did not exit within the bound after stdin closed"))
		}
	}
}

// ErrCreationDrift reports a creation whose init carried a valid UUIDv4
// conversation id but drifted from the frozen profile (permission mode,
// model, cwd, tools): the native conversation exists as an orphan. Its
// id was recorded durably on the session's creation-uncertainty episode
// (Episode) before the child was terminated, so recreation stays blocked
// until a controller resolves that episode. Err is the typed drift
// (*ErrProfileDrift or *ErrToolInventoryDrift).
type ErrCreationDrift struct {
	SessionID adapter.SessionID
	NativeID  string
	Episode   int64
	Err       error
}

func (e *ErrCreationDrift) Error() string {
	return fmt.Sprintf("agy creation for session %s drifted after init; orphan conversation %s recorded on uncertainty episode %d: %v",
		e.SessionID, e.NativeID, e.Episode, e.Err)
}

func (e *ErrCreationDrift) Unwrap() error { return e.Err }

// creationRecorder is the identity attributed on adapter-recorded
// creation-uncertainty episodes.
const creationRecorder = "agy-adapter"

// recordCreationOrphan durably opens (or replays) the session's
// creation-uncertainty episode carrying the orphan native id.
func (a *AgyAdapter) recordCreationOrphan(req adapter.CreateSessionRequest, orphan string, cause error) (int64, error) {
	bg := context.Background()
	var episode int64
	err := a.durable(opOrphan, func() error {
		meta, err := a.store.GetSessionMetadata(bg, string(req.SessionID))
		if err != nil {
			return fmt.Errorf("session metadata lookup: %w", err)
		}
		episode, _, err = a.store.RecordAgyCreationUncertain(bg, storage.AgyCreationUncertainty{
			RunID: meta.RunID, SessionID: string(req.SessionID),
			Reason:     "creation drift after a valid init: " + cause.Error(),
			RecordedBy: creationRecorder, CauseOpID: "agy-create-" + string(req.SessionID),
			OrphanNativeID: orphan,
		})
		return err
	})
	return episode, err
}

func (a *AgyAdapter) bindingFor(req adapter.CreateSessionRequest, nativeID string) adapter.SessionBinding {
	return adapter.SessionBinding{SessionID: req.SessionID, Contributor: req.Contributor,
		NativeSessionID: nativeID, Config: req.Config}
}

func (a *AgyAdapter) compareStoredBinding(sessionID adapter.SessionID, cfg adapter.SessionConfig, b *storage.AgySessionBinding) error {
	if strings.TrimSpace(cfg.Model) != "" && cfg.Model != b.Model {
		return &ErrSessionConfigMismatch{SessionID: sessionID, Field: "model", Want: cfg.Model, Have: b.Model}
	}
	if strings.TrimSpace(cfg.WorkspaceRoot) != "" && cfg.WorkspaceRoot != b.Workspace {
		return &ErrSessionConfigMismatch{SessionID: sessionID, Field: "workspace", Want: cfg.WorkspaceRoot, Have: b.Workspace}
	}
	if b.ProfileDigest != a.profileDigest {
		return &ErrSessionConfigMismatch{SessionID: sessionID, Field: "profile_digest", Want: a.profileDigest, Have: b.ProfileDigest}
	}
	return nil
}

// checkEligibility applies the §3.2 gate step 1 before any child starts
// (creation and every turn launch). A nil lookup fails closed.
func (a *AgyAdapter) checkEligibility(sessionID adapter.SessionID) error {
	if a.fixtureScope {
		return nil
	}
	// The full Task 6 gate (eligibility.go) — platform, sealed image,
	// inventories, covering attestation — re-checked before every
	// creation and turn launch (durable lookups only, no process).
	err := checkProductionEligibility(a.policy, a.image, a.attestation)
	var missing *ErrProductionEligibilityMissing
	if errors.As(err, &missing) {
		missing.SessionID = sessionID
	}
	return err
}

// runPreLaunchCheck runs the per-launch toolkit seam (nil = none).
func (a *AgyAdapter) runPreLaunchCheck(ctx context.Context, sessionID adapter.SessionID) error {
	if a.preLaunchCheck == nil {
		return nil
	}
	return a.preLaunchCheck(ctx, sessionID)
}

// validateLaunch is the adapter's closing check on a launch request: the
// held sealed image, the pinned command, the frozen profile digest and
// model, the exact frozen argv, and the launch matrix.
func (a *AgyAdapter) validateLaunch(ctx context.Context, req execpolicy.LaunchRequest, sessionID adapter.SessionID, kind LaunchKind, nativeID, model, pdig string) error {
	if err := checkLaunchMatrix(req.Profile, a.fixtureScope); err != nil {
		return err
	}
	if req.SessionID != string(sessionID) {
		return fmt.Errorf("launch request is for session %q, not %q", req.SessionID, sessionID)
	}
	if err := a.checkSealedImage(req.SealedImage); err != nil {
		return err
	}
	home, err := agyHomeDir(a.policy.ExpectedHome)
	if err != nil {
		return err
	}
	if req.HomeDir != home {
		return fmt.Errorf("launch HOME %q is not the operator home %q derived from the frozen expected_home", req.HomeDir, home)
	}
	meta, err := a.store.GetSessionMetadata(ctx, string(sessionID))
	if err != nil {
		return fmt.Errorf("session metadata lookup: %w", err)
	}
	if req.RunID != meta.RunID {
		return fmt.Errorf("launch request run %q is not the session's run %q", req.RunID, meta.RunID)
	}
	alloc, ok := a.allocations.GetPaths(meta.RunID, string(sessionID))
	if !ok {
		return fmt.Errorf("no AC-005 allocation for run %s session %s", meta.RunID, sessionID)
	}
	if req.Paths.Root != alloc.Root || req.Paths.Scratch != alloc.Scratch {
		return fmt.Errorf("launch paths (root %q, scratch %q) are not the AC-005 allocation (root %q, scratch %q)",
			req.Paths.Root, req.Paths.Scratch, alloc.Root, alloc.Scratch)
	}
	wantCmd := a.policy.BinaryPath
	if a.image != nil {
		wantCmd = a.image.ArgV0
	}
	if req.Command != wantCmd {
		return fmt.Errorf("launch command %q is not the pinned binary %q", req.Command, wantCmd)
	}
	if req.ProfileDigest != a.profileDigest {
		return fmt.Errorf("launch profile digest %q is not the frozen %q", req.ProfileDigest, a.profileDigest)
	}
	if req.Model != model || model != a.policy.Model {
		return &ErrProfileDrift{Field: "model", Want: a.policy.Model, Have: req.Model}
	}
	if kind == LaunchTurn && req.PromptDigest != pdig {
		return errors.New("launch prompt digest does not match the attempt's pdig")
	}
	if strings.TrimSpace(req.Paths.Root) == "" {
		return errors.New("launch request has no AC-005 workspace root")
	}
	return validateLaunchArgv(req.Args, argvSpec{kind: kind, model: model, policy: a.policy,
		nativeID: nativeID, logRoot: alloc.Scratch})
}

// checkSealedImage requires the request's image to be the adapter's held
// image: the same object AND the same pinned digest and argv0 (digest
// inequality is the real refusal; pointer identity alone is fragile).
func (a *AgyAdapter) checkSealedImage(img *execpolicy.SealedImage) error {
	if a.image == nil {
		if img != nil {
			return errors.New("launch request carries a sealed image the adapter does not hold")
		}
		return nil
	}
	switch {
	case img == nil:
		return errors.New("launch request does not carry the adapter's held sealed image")
	case img.Digest != a.image.Digest || img.Digest != a.policy.BinaryDigest:
		return fmt.Errorf("launch sealed image digest %s is not the pinned %s", img.Digest, a.image.Digest)
	case img.ArgV0 != a.image.ArgV0:
		return fmt.Errorf("launch sealed image argv0 %q is not the held %q", img.ArgV0, a.image.ArgV0)
	case img != a.image:
		return errors.New("launch request does not carry the adapter's held sealed image")
	}
	return nil
}

// verifyInit applies the §3.1 pre-transmission checks, in order:
// conversation id, permission mode, model, cwd, tools.
func (a *AgyAdapter) verifyInit(ev Event, kind LaunchKind, nativeID, model, root string) error {
	if ev.Kind != EventKindInit || ev.Init == nil {
		return &ErrProtocolDrift{Event: string(ev.Kind), Reason: "the first event is not init"}
	}
	if kind == LaunchCreate {
		if !isValidUUIDv4(ev.ConversationID) {
			return &ErrConversationDrift{Observed: ev.ConversationID}
		}
	} else if ev.ConversationID != nativeID {
		return &ErrConversationDrift{Requested: nativeID, Observed: ev.ConversationID}
	}
	if ev.Init.PermissionMode == "" || ev.Init.PermissionMode != a.policy.PermissionMode {
		return &ErrProfileDrift{Field: "permission_mode", Want: a.policy.PermissionMode, Have: ev.Init.PermissionMode}
	}
	if ev.Init.Model != model {
		return &ErrProfileDrift{Field: "model", Want: model, Have: ev.Init.Model}
	}
	if !sameDir(ev.Init.CWD, root) {
		return &ErrProfileDrift{Field: "cwd", Want: root, Have: ev.Init.CWD}
	}
	return compareToolInventory(ev.Init.Tools, a.policy.ExpectedTools)
}

func compareToolInventory(observed, expected []string) error {
	if len(observed) == 0 {
		return &ErrToolInventoryDrift{Reason: "init.tools is empty or missing"}
	}
	seen := make(map[string]struct{}, len(observed))
	for _, t := range observed {
		if _, dup := seen[t]; dup {
			return &ErrToolInventoryDrift{Reason: fmt.Sprintf("init.tools lists %q twice", t)}
		}
		seen[t] = struct{}{}
	}
	want := make(map[string]struct{}, len(expected))
	var missing, unexpected []string
	for _, t := range expected {
		want[t] = struct{}{}
		if _, ok := seen[t]; !ok {
			missing = append(missing, t)
		}
	}
	for _, t := range observed {
		if _, ok := want[t]; !ok {
			unexpected = append(unexpected, t)
		}
	}
	if len(missing) > 0 || len(unexpected) > 0 {
		sort.Strings(missing)
		sort.Strings(unexpected)
		return &ErrToolInventoryDrift{Missing: missing, Unexpected: unexpected}
	}
	return nil
}

// sameDir compares two directories by their resolved physical paths
// (the child's getwd resolves symlinked ancestors). Unresolvable ⇒ not
// the same.
func sameDir(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}

// ── ResumeSession ───────────────────────────────────────────────────────

// ResumeSession is local inspection only (spec §3.4): the binding is
// well-formed and matches, and a materialized binding's conversation
// file still exists at the recorded path, 0600, current uid, recorded
// identity. Native verification is the next turn's init check.
func (a *AgyAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	// A resumed (parked) session re-proves sign-in: the next dispatch
	// runs the models auth gate again (spec §3.2 gate step 2).
	a.invalidateAuth(binding.SessionID)
	// An empty config would skip the model/workspace comparison: refuse.
	if strings.TrimSpace(binding.Config.Model) == "" || strings.TrimSpace(binding.Config.WorkspaceRoot) == "" {
		return fmt.Errorf("resume of %s requires the bound model and workspace root", binding.SessionID)
	}
	stored, err := a.store.GetAgySessionBinding(ctx, string(binding.SessionID))
	if err != nil {
		return err
	}
	if stored == nil {
		return fmt.Errorf("resume requires a persisted agy binding for %s", binding.SessionID)
	}
	if !isValidUUIDv4(stored.NativeID) {
		return fmt.Errorf("persisted native id %q is not a UUIDv4", stored.NativeID)
	}
	if binding.NativeSessionID != stored.NativeID {
		return &ErrSessionConfigMismatch{SessionID: binding.SessionID, Field: "native_session_id",
			Want: binding.NativeSessionID, Have: stored.NativeID}
	}
	if err := a.compareStoredBinding(binding.SessionID, binding.Config, stored); err != nil {
		return err
	}
	if pending := a.pendingMaterializeErr(binding.SessionID); pending != nil {
		// A post-acceptance materialization record failed: retry it; a
		// store that still cannot record it fails the resume.
		if err := a.materializeIfPresent(ctx, binding.SessionID, stored.NativeID); err != nil {
			return fmt.Errorf("materialization of %s could not be recorded: %w", binding.SessionID, err)
		}
		a.setMaterializeErr(binding.SessionID, nil)
		if stored, err = a.store.GetAgySessionBinding(ctx, string(binding.SessionID)); err != nil {
			return err
		}
	}
	if !stored.Materialized {
		return nil
	}
	want := conversationPath(a.policy.ExpectedHome, stored.NativeID)
	if stored.ConversationPath == nil || *stored.ConversationPath != want {
		return fmt.Errorf("materialized binding %s does not record the conversation path %s", binding.SessionID, want)
	}
	if stored.FileIdentity == nil || strings.TrimSpace(*stored.FileIdentity) == "" {
		return fmt.Errorf("materialized binding %s has no recorded file identity", binding.SessionID)
	}
	return verifyConversationFile(want, *stored.FileIdentity)
}

// materializeIfPresent records the conversation file after the first
// accepted turn when it is observable (stat only). An absent or
// ill-formed file is a gap, not an error (the binding stays
// unmaterialized; the next init re-verifies); a store failure is
// returned.
func (a *AgyAdapter) materializeIfPresent(ctx context.Context, sessionID adapter.SessionID, nativeID string) error {
	b, err := a.store.GetAgySessionBinding(ctx, string(sessionID))
	if err != nil {
		return err
	}
	if b == nil || b.Materialized {
		return nil
	}
	path := conversationPath(a.policy.ExpectedHome, nativeID)
	identity, err := conversationFileIdentity(path)
	if err != nil || verifyConversationFile(path, identity) != nil {
		return nil
	}
	return a.durable(opMaterialize, func() error {
		return a.store.MarkAgySessionMaterialized(ctx, string(sessionID), path, identity)
	})
}

func (a *AgyAdapter) pendingMaterializeErr(sessionID adapter.SessionID) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.materializeErrs[sessionID]
}

func (a *AgyAdapter) setMaterializeErr(sessionID adapter.SessionID, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		delete(a.materializeErrs, sessionID)
		return
	}
	a.materializeErrs[sessionID] = err
}

// ── Dispatch ────────────────────────────────────────────────────────────

// Dispatch submits one turn (spec §3.5). Pre-write failures are
// DispatchRejected with the attempt recorded missing; a failure after
// the first stdin byte is DispatchUnknown; a fully written and closed
// prompt is DispatchAccepted (transmitted — the durable acceptance
// evidence is the user_input DONE step the turn goroutine records).
func (a *AgyAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	reject := func(reason string, err error) (adapter.DispatchOutcome, error) {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: reason}, err
	}
	if err := ref.Validate(); err != nil {
		return reject(err.Error(), err)
	}
	attempt, ok := a.identity.AttemptFor(ctx, ref)
	if !ok || strings.TrimSpace(attempt) == "" {
		err := fmt.Errorf("no attempt identity for %s/%s", ref.SessionID, ref.TurnKey)
		return reject(err.Error(), err)
	}
	binding, err := a.store.GetAgySessionBinding(ctx, string(ref.SessionID))
	if err != nil {
		return reject(err.Error(), err)
	}
	if binding == nil {
		err := fmt.Errorf("no agy session binding for %s", ref.SessionID)
		return reject(err.Error(), err)
	}
	nativeID := binding.NativeID
	if !isValidUUIDv4(nativeID) {
		err := fmt.Errorf("binding native id %q is not a UUIDv4", nativeID)
		return reject(err.Error(), err)
	}
	if binding.ProfileDigest != a.profileDigest {
		err := &ErrSessionConfigMismatch{SessionID: ref.SessionID, Field: "profile_digest", Want: a.profileDigest, Have: binding.ProfileDigest}
		return reject(err.Error(), err)
	}
	required, err := a.requiredTools(ctx, ref)
	if err != nil {
		return reject(err.Error(), err)
	}
	// A turn that already reached a verified terminal is never
	// redispatched (no redispatch branch, §3.5).
	if latest, err := a.store.GetLatestAgyTurnAttempt(ctx, string(ref.SessionID), ref.TurnKey); err != nil {
		return reject(err.Error(), err)
	} else if latest != nil && latest.Terminal {
		err := fmt.Errorf("turn %s/%s already reached a verified terminal (%s); redispatch refused",
			ref.SessionID, ref.TurnKey, latest.ObservedStatus)
		return reject(err.Error(), err)
	}

	// Single flight per native conversation (in-process), then the
	// durable block (an unresolved attempt blocks across restarts).
	if err := a.acquireSlot(nativeID, attempt); err != nil {
		return reject(err.Error(), err)
	}
	held := true
	release := func() {
		if held {
			a.releaseSlot(nativeID, attempt)
			held = false
		}
	}
	rejectRelease := func(reason string, err error) (adapter.DispatchOutcome, error) {
		release()
		return reject(reason, err)
	}
	blocked, err := a.store.HasAgyUnresolvedAttempts(ctx, nativeID)
	if err != nil {
		return rejectRelease(err.Error(), err)
	}
	if blocked {
		return rejectRelease("native conversation "+nativeID+" has an unresolved attempt; blocked until the controller records a disposition", nil)
	}
	if err := a.checkEligibility(ref.SessionID); err != nil {
		return rejectRelease(err.Error(), err)
	}

	pdig, err := promptDigest(nativeID, ref.TurnKey, attempt, prompt)
	if err != nil {
		return rejectRelease(err.Error(), err)
	}
	line, err := EncodeUserMessage(prompt)
	if err != nil {
		return rejectRelease(err.Error(), err)
	}
	req, err := a.launch.AgyTurnLaunch(ctx, ref.SessionID, nativeID, pdig, LaunchTurn)
	if err != nil {
		return rejectRelease("turn launch: "+err.Error(), err)
	}
	if err := a.validateLaunch(ctx, req, ref.SessionID, LaunchTurn, nativeID, binding.Model, pdig); err != nil {
		return rejectRelease(err.Error(), err)
	}
	if !sameDir(req.Paths.Root, binding.Workspace) {
		err := &ErrSessionConfigMismatch{SessionID: ref.SessionID, Field: "workspace", Want: req.Paths.Root, Have: binding.Workspace}
		return rejectRelease(err.Error(), err)
	}
	// Task 6 gates, before anything durable and before any prompt
	// exists: toolkit configured state (every launch), then the models
	// auth gate (first dispatch of the session / after resume / TTL).
	if err := a.runPreLaunchCheck(ctx, ref.SessionID); err != nil {
		return rejectRelease(err.Error(), err)
	}
	if err := a.authGate(ctx, ref.SessionID); err != nil {
		return rejectRelease(err.Error(), err)
	}
	req.TurnKey = ref.TurnKey
	req.AttemptID = attempt

	// §3.5/§3.11 durable ordering: attempt + pdig AND the launch
	// reservation (launch_count 0→1) in ONE transaction, BEFORE the
	// executor starts. A failure leaves nothing durable.
	bg := context.Background()
	seq, err := a.store.InsertAgyTurnAttemptAndReserveLaunch(ctx, storage.AgyTurnAttempt{
		AttemptID: attempt, SessionID: string(ref.SessionID), TurnKey: ref.TurnKey,
		PromptDigest: pdig, RequiredTools: required, CreatedAt: time.Now().UTC(),
	}, executorIdentity)
	if err != nil {
		return rejectRelease(err.Error(), err)
	}

	proc, err := a.executor.Start(bg, req)
	if err != nil {
		if recErr := a.store.RecordAgyLaunchState(bg, attempt, seq, "start_failed", nil, nil); recErr != nil {
			release()
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
				Reason: "start failed and the launch state could not be recorded: " + recErr.Error()}, recErr
		}
		return rejectRelease("child start failed: "+err.Error(), err)
	}
	var exe *storage.AgyExeIdentity
	if id := proc.ExecutableIdentity(); id.DevIno != "" || id.Digest != "" {
		exe = &storage.AgyExeIdentity{DevIno: id.DevIno, Digest: id.Digest}
	}
	p := watchProcess(proc)
	// abort terminates the child before any prompt byte crossed stdin.
	// The rejection is reported only when the pre-acceptance evidence
	// (attempt missing) is durable; otherwise the outcome is Unknown.
	abort := func(reason string, cause error) (adapter.DispatchOutcome, error) {
		missingErr, deadErr := a.abortPreWrite(p, attempt, seq)
		release()
		if missingErr != nil {
			unknown := adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
				Reason: reason + "; the pre-write rejection could not be recorded (attempt stays uncertain): " + missingErr.Error()}
			return unknown, errors.Join(cause, missingErr)
		}
		if deadErr != nil {
			reason += "; launch dead state not recorded: " + deadErr.Error()
		}
		return reject(reason, cause)
	}
	if err := a.store.RecordAgyLaunchState(bg, attempt, seq, "started", nil, exe); err != nil {
		return abort("launch state could not be recorded: "+err.Error(), err)
	}

	// Pre-transmission verification (§3.1): init BEFORE any byte.
	it, err := p.awaitInit(initTimeout)
	if err != nil {
		return abort("init not observed before the write: "+err.Error(), err)
	}
	if err := a.verifyInit(it.ev, LaunchTurn, nativeID, binding.Model, req.Paths.Root); err != nil {
		var drift *ErrConversationDrift
		if errors.As(err, &drift) && drift.Observed != "" {
			// The orphan id is durable on the attempt BEFORE the child
			// is terminated.
			if recErr := a.durable(opOrphan, func() error {
				return a.store.RecordAgyOrphanConversation(bg, attempt, drift.Observed)
			}); recErr != nil {
				err = fmt.Errorf("%w (orphan id could not be recorded: %v)", err, recErr)
			}
		}
		return abort("pre-transmission verification failed: "+err.Error(), err)
	}

	run := &agyTurnRun{
		ref: ref, attemptID: attempt, nativeID: nativeID, seq: seq, required: required,
		p: p, subs: make(map[*adapter.BufferedStream]struct{}), done: make(chan struct{}),
	}

	// The first stdin byte is the ONLY ambiguity boundary (§3.5).
	bw := &boundaryWriter{w: proc.Stdin(), store: a.store, attemptID: attempt, seq: seq}
	_, werr := bw.Write(line[:1])
	if werr == nil {
		_, werr = bw.Write(line[1:])
	}
	if werr != nil && !bw.transmitted.Load() {
		return abort("stdin write failed before the first byte: "+werr.Error(), werr)
	}
	// From here nothing is pre-acceptance: the run owns the outcome.
	var closeErr error
	if werr == nil {
		closeErr = proc.Stdin().Close()
	} else {
		_ = proc.Stdin().Close()
	}
	held = false // the turn goroutine releases the slot
	a.mu.Lock()
	a.turns[ref] = run
	a.mu.Unlock()
	go a.runTurn(run)

	switch {
	case werr != nil:
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "prompt transmission began but failed: " + werr.Error()}, nil
	case bw.recordErr != nil:
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "first-byte boundary could not be persisted: " + bw.recordErr.Error()}, nil
	case closeErr != nil:
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown,
			Reason: "stdin close failed after the prompt was written: " + closeErr.Error()}, nil
	}
	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
}

// requiredTools resolves the attempt's required set: the journaled
// dispatch intent (an unreadable or missing intent is refused typed —
// never replaced by the defaults), else, for an empty journaled set,
// the frozen default; re-validated ⊆ expected_tools
// with no duplicates (defense in depth behind queue-time validation).
func (a *AgyAdapter) requiredTools(ctx context.Context, ref adapter.TurnRef) ([]string, error) {
	tools := a.policy.DefaultRequiredTools
	if a.required != nil {
		t, ok, err := a.required.RequiredToolsFor(ctx, ref)
		if err != nil {
			return nil, &ErrRequiredToolsUnavailable{SessionID: ref.SessionID, TurnKey: ref.TurnKey, Err: err}
		}
		if ok {
			tools = t
		}
	}
	expected := make(map[string]struct{}, len(a.policy.ExpectedTools))
	for _, t := range a.policy.ExpectedTools {
		expected[t] = struct{}{}
	}
	seen := make(map[string]struct{}, len(tools))
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if _, ok := expected[t]; !ok {
			return nil, fmt.Errorf("required tool %q is not in the frozen expected_tools", t)
		}
		if _, dup := seen[t]; dup {
			return nil, fmt.Errorf("required tool %q is listed twice", t)
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out, nil
}

// abortPreWrite terminates a child before any prompt byte crossed stdin
// and records the positive pre-acceptance evidence: launch dead, attempt
// missing (never uncertain-blocking). Both record failures are returned:
// a missing record that failed leaves the attempt uncertain (the caller
// reports Unknown); a failed dead record is surfaced in the reason.
func (a *AgyAdapter) abortPreWrite(p *agyProcess, attempt string, seq int64) (missingErr, deadErr error) {
	p.kill()
	code := p.wait()
	bg := context.Background()
	deadErr = a.durable(opLaunchDead, func() error {
		return a.store.RecordAgyLaunchState(bg, attempt, seq, "dead", &code, nil)
	})
	missingErr = a.durable(opAttemptMissing, func() error {
		return a.store.SetAgyAttemptObservedStatus(bg, attempt, "missing")
	})
	return missingErr, deadErr
}

// boundaryWriter persists the first-byte transmission boundary at the
// write boundary itself (AC-008 idiom). A storage failure is surfaced by
// the dispatcher as ambiguity, never dropped.
type boundaryWriter struct {
	w         io.Writer
	store     *storage.Store
	attemptID string
	seq       int64

	transmitted atomic.Bool
	recordErr   error
}

func (b *boundaryWriter) Write(p []byte) (int, error) {
	n, err := b.w.Write(p)
	if n > 0 && b.transmitted.CompareAndSwap(false, true) {
		if rerr := b.store.RecordAgyStdinTransmitted(context.Background(), b.attemptID, b.seq); rerr != nil {
			b.recordErr = rerr
		}
	}
	return n, err
}

// ── The turn run ────────────────────────────────────────────────────────

type agyTurnRun struct {
	ref       adapter.TurnRef
	attemptID string
	nativeID  string
	seq       int64
	required  []string
	p         *agyProcess

	mu              sync.Mutex
	subs            map[*adapter.BufferedStream]struct{}
	history         []adapter.Event
	finished        bool
	finishErr       error
	cancelRequested bool
	termStatus      string // completed|failed|cancelled once committed
	denialSeq       int

	done chan struct{}
}

func (r *agyTurnRun) publish(ev adapter.Event) {
	ev.Ref = r.ref
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	ev.Payload = clampPayload(ev.Payload)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.history) >= historyCap {
		r.history = append(r.history[:0:0], r.history[1:]...)
	}
	r.history = append(r.history, ev)
	for s := range r.subs {
		if err := s.SendOrOverflow(ev); err != nil {
			delete(r.subs, s)
		}
	}
}

// subscribe returns a detachable tap: history replayed, then live
// events; cancelling ctx closes only this tap (never the turn).
func (r *agyTurnRun) subscribe(ctx context.Context) *adapter.BufferedStream {
	s := adapter.NewBufferedStream(r.ref, adapter.DefaultBufferCapacity)
	r.mu.Lock()
	for _, ev := range r.history {
		_ = s.SendOrOverflow(ev)
	}
	if r.finished {
		err := r.finishErr
		r.mu.Unlock()
		closeTap(s, err)
		return s
	}
	r.subs[s] = struct{}{}
	r.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			delete(r.subs, s)
			r.mu.Unlock()
			_ = s.Close()
		case <-s.Done():
		}
	}()
	return s
}

func closeTap(s *adapter.BufferedStream, err error) {
	if err == nil {
		_ = s.Close()
		return
	}
	_ = s.CloseWithErr(err)
}

// finish ends every tap and signals classification complete.
func (r *agyTurnRun) finish(status string, err error) {
	r.mu.Lock()
	r.finished = true
	r.finishErr = err
	r.termStatus = status
	subs := r.subs
	r.subs = nil
	r.mu.Unlock()
	for s := range subs {
		closeTap(s, err)
	}
	close(r.done)
}

func (r *agyTurnRun) status() (string, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.termStatus, r.finished, r.cancelRequested
}

func clampPayload(s string) string {
	const marker = "…[truncated]"
	if len(s) <= adapter.MaxEventPayloadBytes {
		return s
	}
	return s[:adapter.MaxEventPayloadBytes-len(marker)] + marker
}

// runTurn owns a transmitted turn until classification: acceptance on
// user_input DONE, tool steps observed, progress mirrored, exactly one
// result, then (after stdout EOF, the joined stderr drain, and the exit)
// either the verified terminal with its verification evidence or
// Uncertain. It is the only consumer of the process's events.
func (a *AgyAdapter) runTurn(run *agyTurnRun) {
	bg := context.Background()
	var (
		accepted  bool
		observed  []string
		result    *ResultEvent
		resultRaw []byte
		drift     error
		acceptErr error
	)
	bound := time.NewTimer(turnBoundFor(a.policy))
	defer bound.Stop()

loop:
	for {
		select {
		case it, ok := <-run.p.items:
			if !ok {
				break loop
			}
			if drift != nil {
				continue // poisoned: drain until the killed child's EOF
			}
			if result != nil {
				drift = &ErrProtocolDrift{Event: string(it.ev.Kind), Reason: "event after the terminal result"}
				run.p.kill()
				continue
			}
			if it.ev.ConversationID != run.nativeID {
				drift = &ErrConversationDrift{Requested: run.nativeID, Observed: it.ev.ConversationID}
				// The orphan id is durable BEFORE the child is terminated.
				observedID := it.ev.ConversationID
				if observedID == "" {
					run.p.kill()
					continue
				}
				if err := a.durable(opOrphan, func() error {
					return a.store.RecordAgyOrphanConversation(bg, run.attemptID, observedID)
				}); err != nil {
					drift = fmt.Errorf("%w (orphan id could not be recorded: %v)", drift, err)
				}
				run.p.kill()
				continue
			}
			switch it.ev.Kind {
			case EventKindStepUpdate:
				s := it.ev.Step
				if s.Type == StepTypeUserInput && s.State == "DONE" && !accepted {
					accepted = true
					idx := s.Index
					if err := a.durable(opNativeStepIndex, func() error {
						return a.store.RecordAgyNativeStepIndex(bg, run.attemptID, idx)
					}); err != nil {
						// No durable acceptance evidence: the turn can
						// never be reported as a verified terminal.
						acceptErr = err
					} else if err := a.materializeIfPresent(bg, run.ref.SessionID, run.nativeID); err != nil {
						a.setMaterializeErr(run.ref.SessionID, err)
					}
				}
				if s.Type == StepTypeTool && s.State == "DONE" && s.ToolName != "" {
					observed = append(observed, s.ToolName)
				}
				run.publish(progressEvent(s))
			case EventKindResult:
				result = it.ev.Result
				resultRaw = it.raw
			default:
				drift = &ErrProtocolDrift{Event: string(it.ev.Kind), Reason: "unexpected event after init"}
				run.p.kill()
			}
		case <-bound.C:
			_ = a.interruptRun(run) // Council turn bound (§3.5)
		}
	}

	readErr := run.p.readErr
	if readErr != nil {
		run.p.kill() // poisoned stream: never a best-effort parse
	}
	code := run.p.settle(postResultExitGrace)
	deadErr := a.durable(opLaunchDead, func() error {
		return a.store.RecordAgyLaunchState(bg, run.attemptID, run.seq, "dead", &code, nil)
	})

	var reason string
	switch {
	case drift != nil:
		reason = "protocol drift: " + drift.Error()
	case acceptErr != nil:
		reason = "acceptance evidence (user_input DONE) could not be recorded: " + acceptErr.Error()
	case readErr != nil:
		reason = "stream poisoned: " + readErr.Error()
	case run.p.printTimeout.Load():
		reason = "stderr print-timeout marker: partial output is not a verified terminal"
	case run.p.ignoredInput.Load():
		reason = "stderr ignored-input marker: the input message was dropped"
	case run.p.stderrOverflow.Load():
		reason = "stderr scan inconclusive"
	case result == nil:
		reason = "process exited without a result"
	}
	if reason != "" {
		if deadErr != nil {
			reason += "; launch dead state not recorded: " + deadErr.Error()
		}
		a.finishRun(run, "", errors.New("attempt uncertain: "+reason))
		return
	}

	observedStatus, turnStatus, payload := "completed", council.TurnCompleted, result.Response
	if result.Status == "ERROR" {
		_, _, cancelRequested := run.status()
		switch {
		case result.Error == "interrupted" && cancelRequested:
			observedStatus, turnStatus, payload = "cancelled", council.TurnCancelled, ""
		case result.Error == "interrupted":
			// No Council-sent SIGINT: an external interrupt is a failure,
			// never a Council cancellation.
			observedStatus, turnStatus, payload = "failed", council.TurnFailed,
				"interrupted (external: no Council cancel request)"
		default:
			observedStatus, turnStatus, payload = "failed", council.TurnFailed, result.Error
		}
	}
	for _, c := range classifyDenials(observed, result.DeniedActions, a.policy.Coverage) {
		run.denialSeq++
		text := c.Pair
		if len(c.Tools) > 0 {
			text = strings.Join(c.Tools, ",") + " (" + c.Pair + ")"
		}
		run.publish(adapter.Event{Type: adapter.EventToolDenied, Status: council.TurnRunning,
			ApprovalID: fmt.Sprintf("agy-denial-%s-%d", run.attemptID, run.denialSeq),
			Payload:    "tool denied: " + text})
	}
	verification := ComputeVerification(run.required, observed, result.DeniedActions, a.policy.Coverage)
	usageJSON := encodeUsage(result.Usage)
	if err := a.store.SetAgyAttemptTerminal(bg, run.attemptID, observedStatus, string(resultRaw), usageJSON, verification); err != nil {
		a.finishRun(run, "", fmt.Errorf("terminal persistence failed; attempt uncertain: %w", err))
		return
	}
	run.publish(adapter.Event{Type: adapter.EventTerminal, Status: turnStatus, Payload: payload,
		Usage: usageFromJSON(usageJSON)})
	var finishErr error
	if deadErr != nil {
		finishErr = fmt.Errorf("verified %s terminal recorded, but the launch dead state was not: %w", observedStatus, deadErr)
	}
	a.finishRun(run, observedStatus, finishErr)
}

func progressEvent(s *StepUpdate) adapter.Event {
	payload := string(s.Type) + " " + s.State
	switch {
	case s.Type == StepTypeAgentResponse && s.TextDelta != "":
		payload = s.TextDelta
	case s.Type == StepTypeTool && s.ToolName != "":
		payload = "tool " + s.ToolName + " " + s.State
	}
	ev := adapter.Event{Type: adapter.EventProgress, Status: council.TurnRunning, Payload: payload}
	if s.Usage != nil {
		ev.Usage = adapter.ExecutionUsage{
			InputTokens:  adapter.UsageMetric[int64]{Value: int64(s.Usage.InputTokens), Available: true},
			OutputTokens: adapter.UsageMetric[int64]{Value: int64(s.Usage.OutputTokens), Available: true},
		}
	}
	return ev
}

// finishRun ends the run's taps, retires it from the live map (kept for
// in-memory replay), and releases the single-flight slot. The durable
// attempt state carries any block.
func (a *AgyAdapter) finishRun(run *agyTurnRun, status string, err error) {
	a.mu.Lock()
	if a.turns[run.ref] == run {
		delete(a.turns, run.ref)
	}
	if _, seen := a.finished[run.ref]; !seen {
		a.finishedOrder = append(a.finishedOrder, run.ref)
	}
	a.finished[run.ref] = run
	for len(a.finishedOrder) > finishedCap {
		delete(a.finished, a.finishedOrder[0])
		a.finishedOrder = a.finishedOrder[1:]
	}
	if a.slots[run.nativeID] == run.attemptID {
		delete(a.slots, run.nativeID)
	}
	a.mu.Unlock()
	run.finish(status, err)
}

type usageRecord struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

func encodeUsage(u Usage) string {
	raw, err := json.Marshal(usageRecord{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		ThinkingTokens: u.ThinkingTokens, CacheReadTokens: u.CacheReadTokens, TotalTokens: u.TotalTokens})
	if err != nil {
		return ""
	}
	return string(raw)
}

// usageFromJSON renders the recorded usage; cost is never fabricated.
func usageFromJSON(raw string) adapter.ExecutionUsage {
	usage := adapter.ExecutionUsage{TotalCostUSD: adapter.UsageMetric[float64]{Available: false}}
	var u usageRecord
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &u) != nil {
		return usage
	}
	usage.InputTokens = adapter.UsageMetric[int64]{Value: int64(u.InputTokens), Available: true}
	usage.OutputTokens = adapter.UsageMetric[int64]{Value: int64(u.OutputTokens), Available: true}
	return usage
}

// ── Observe ─────────────────────────────────────────────────────────────

// Observe returns a bounded, detachable tap of the turn's event stream.
// A live (or, in this process, finished) turn replays its bounded
// history then streams; after a restart a terminal turn replays the
// recorded terminal from durable state. Cancelling ctx detaches only the
// tap (client disconnect is not cancellation).
func (a *AgyAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	a.mu.Lock()
	run := a.turns[ref]
	if run == nil {
		run = a.finished[ref]
	}
	a.mu.Unlock()
	if run != nil {
		return run.subscribe(ctx), nil
	}
	attempt, err := a.store.GetLatestAgyTurnAttempt(ctx, string(ref.SessionID), ref.TurnKey)
	if err != nil {
		return nil, err
	}
	if attempt == nil || !attempt.Terminal {
		return nil, fmt.Errorf("turn %s/%s is not live and has no recorded terminal", ref.SessionID, ref.TurnKey)
	}
	res, _ := a.resultFromAttempt(ref, attempt)
	s := adapter.NewBufferedStream(ref, 1)
	_ = s.SendOrOverflow(adapter.Event{Ref: ref, Type: adapter.EventTerminal, Status: res.Status,
		Payload: clampPayload(res.Output), Usage: res.Usage, Timestamp: attempt.UpdatedAt})
	_ = s.Close()
	return s, nil
}

// ── Cancel ──────────────────────────────────────────────────────────────

// interruptRun sends SIGINT once and arms the grace escalation
// (terminate ⇒ no terminal ⇒ Uncertain).
func (a *AgyAdapter) interruptRun(run *agyTurnRun) error {
	run.mu.Lock()
	first := !run.cancelRequested
	run.cancelRequested = true
	run.mu.Unlock()
	if !first {
		return nil
	}
	if err := run.p.proc.Interrupt(); err != nil {
		run.mu.Lock()
		run.cancelRequested = false
		run.mu.Unlock()
		return err
	}
	run.publish(adapter.Event{Type: adapter.EventProgress, Status: council.TurnCancelling, Payload: "SIGINT sent"})
	go func() {
		t := time.NewTimer(cancelGrace)
		defer t.Stop()
		select {
		case <-run.done:
		case <-t.C:
			run.p.kill()
		}
	}()
	return nil
}

// Cancel applies spec §3.6.
func (a *AgyAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	a.mu.Lock()
	run := a.turns[ref]
	a.mu.Unlock()
	if run == nil {
		attempt, err := a.store.GetLatestAgyTurnAttempt(ctx, string(ref.SessionID), ref.TurnKey)
		if err == nil && attempt != nil && attempt.Terminal {
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelAlreadyTerminal,
				Reason: "turn already reached a verified terminal (" + attempt.ObservedStatus + ")"}, nil
		}
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown, Reason: "turn is not live"}, nil
	}
	if status, finished, _ := run.status(); finished {
		return finishedCancelOutcome(ref, status), nil
	}
	if err := a.interruptRun(run); err != nil {
		if errors.Is(err, execpolicy.ErrInterruptUnsupported) {
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnsupported,
				Reason: "SIGINT is unsupported on this platform"}, nil
		}
		// The child may already be gone; the run's classification decides.
		t := time.NewTimer(postResultExitGrace)
		defer t.Stop()
		select {
		case <-run.done:
			status, _, _ := run.status()
			return finishedCancelOutcome(ref, status), nil
		case <-ctx.Done():
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
				Reason: "interrupt failed and the caller stopped waiting: " + err.Error()}, nil
		case <-t.C:
			return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
				Reason: "interrupt failed: " + err.Error()}, nil
		}
	}
	select {
	case <-run.done:
		status, _, _ := run.status()
		return finishedCancelOutcome(ref, status), nil
	case <-ctx.Done():
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelRequested,
			Reason: "SIGINT sent; awaiting the verified interrupted result"}, nil
	}
}

func finishedCancelOutcome(ref adapter.TurnRef, status string) adapter.CancelOutcome {
	switch status {
	case "cancelled":
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed,
			Reason: `verified result{status:"ERROR",error:"interrupted"}`}
	case "":
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown,
			Reason: "the process ended without a verified terminal; attempt uncertain"}
	default:
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelAlreadyTerminal,
			Reason: "turn reached a verified terminal (" + status + ")"}
	}
}

// ── Collect / Verification ──────────────────────────────────────────────

type verificationRecord struct {
	RequiredTools        []string `json:"required_tools"`
	ExecutedTools        []string `json:"executed_tools"`
	MissingRequiredTools []string `json:"missing_required_tools"`
	DeniedTools          []string `json:"denied_tools"`
	AmbiguousDenials     []string `json:"ambiguous_denials"`
	UnattributedDenials  []string `json:"unattributed_denials"`
	UnmappedDenials      []string `json:"unmapped_denials"`
	Incomplete           bool     `json:"verification_incomplete"`
}

type rawEvidenceRecord struct {
	Result json.RawMessage `json:"result,omitempty"`
	// ResultOmitted replaces Result when the evidence would exceed
	// adapter.MaxRawEvidenceBytes: the verbatim line stays durable on the
	// attempt; its size and digest identify it.
	ResultOmitted *omittedResult     `json:"result_omitted,omitempty"`
	Verification  verificationRecord `json:"verification"`
}

type omittedResult struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
	Reason string `json:"reason"`
}

// clampRawEvidence bounds RawEvidence to adapter.MaxRawEvidenceBytes by
// omitting the verbatim result line (never by cutting JSON mid-value).
func clampRawEvidence(rec rawEvidenceRecord) ([]byte, error) {
	raw, err := json.Marshal(rec)
	if err != nil || len(raw) <= adapter.MaxRawEvidenceBytes {
		return raw, err
	}
	sum := sha256.Sum256(rec.Result)
	rec.ResultOmitted = &omittedResult{Bytes: len(rec.Result), SHA256: hex.EncodeToString(sum[:]),
		Reason: fmt.Sprintf("raw evidence exceeds %d bytes; the verbatim result line is retained on the attempt", adapter.MaxRawEvidenceBytes)}
	rec.Result = nil
	raw, err = json.Marshal(rec)
	if err != nil || len(raw) <= adapter.MaxRawEvidenceBytes {
		return raw, err
	}
	return json.Marshal(rawEvidenceRecord{ResultOmitted: rec.ResultOmitted})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Collect reports from durable state only: the recorded terminal of the
// turn's latest attempt. Output is result.response (the error text for a
// failed terminal); RawEvidence is {"result": <verbatim result line>,
// "verification": {...}}. Cost is never fabricated.
func (a *AgyAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	attempt, err := a.store.GetLatestAgyTurnAttempt(ctx, string(ref.SessionID), ref.TurnKey)
	if err != nil {
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultUnavailable}, err
	}
	if attempt == nil {
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultUnavailable},
			errors.New("turn not dispatched")
	}
	if !attempt.Terminal {
		switch {
		case attempt.ObservedStatus == "missing":
			return adapter.TurnResult{Ref: ref, Status: council.TurnFailed, ResultStatus: adapter.ResultUnavailable}, nil
		case a.isLive(ref):
			return adapter.TurnResult{Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultPending}, nil
		default:
			return adapter.TurnResult{Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultUnavailable}, nil
		}
	}
	return a.resultFromAttempt(ref, attempt)
}

func (a *AgyAdapter) resultFromAttempt(ref adapter.TurnRef, attempt *storage.AgyTurnAttempt) (adapter.TurnResult, error) {
	malformed := adapter.TurnResult{Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultMalformed}
	if attempt.ResultPayload == nil {
		return malformed, nil
	}
	ev, err := DecodeEvent([]byte(*attempt.ResultPayload))
	if err != nil || ev.Kind != EventKindResult || ev.Result == nil {
		return malformed, nil
	}
	raw, err := clampRawEvidence(rawEvidenceRecord{
		Result: json.RawMessage(*attempt.ResultPayload),
		Verification: verificationRecord{
			RequiredTools: nonNil(attempt.RequiredTools), ExecutedTools: nonNil(attempt.ExecutedTools),
			MissingRequiredTools: nonNil(attempt.MissingRequiredTools), DeniedTools: nonNil(attempt.DeniedTools),
			AmbiguousDenials: nonNil(attempt.AmbiguousDenials), UnattributedDenials: nonNil(attempt.UnattributedDenials),
			UnmappedDenials: nonNil(attempt.UnmappedDenials), Incomplete: attempt.VerificationIncomplete,
		},
	})
	if err != nil {
		return malformed, nil
	}
	usage := ptrUsage(attempt.ResultUsage)
	out := adapter.TurnResult{Ref: ref, RawEvidence: raw, Usage: usage, CompletedAt: attempt.UpdatedAt}
	switch attempt.ObservedStatus {
	case "completed":
		out.Status, out.ResultStatus, out.Output = council.TurnCompleted, adapter.ResultAvailable, ev.Result.Response
	case "failed":
		out.Status, out.ResultStatus, out.Output = council.TurnFailed, adapter.ResultFailed, ev.Result.Error
	case "cancelled":
		out.Status, out.ResultStatus = council.TurnCancelled, adapter.ResultUnavailable
	default:
		return malformed, nil
	}
	return out, nil
}

func ptrUsage(raw *string) adapter.ExecutionUsage {
	if raw == nil {
		return usageFromJSON("")
	}
	return usageFromJSON(*raw)
}

// Verification returns the recorded required-tool verification of the
// turn's latest attempt; ok is false until that attempt is terminal.
func (a *AgyAdapter) Verification(ctx context.Context, ref adapter.TurnRef) (AgyVerification, bool, error) {
	attempt, err := a.store.GetLatestAgyTurnAttempt(ctx, string(ref.SessionID), ref.TurnKey)
	if err != nil || attempt == nil || !attempt.Terminal {
		return AgyVerification{}, false, err
	}
	return AgyVerification{
		Executed: attempt.ExecutedTools, MissingRequired: attempt.MissingRequiredTools,
		Denied: attempt.DeniedTools, Ambiguous: attempt.AmbiguousDenials,
		Unattributed: attempt.UnattributedDenials, Unmapped: attempt.UnmappedDenials,
		Incomplete: attempt.VerificationIncomplete,
	}, true, nil
}

// ── Reconcile (§3.10, advisory) ─────────────────────────────────────────

// Reconcile classifies a recovered turn from evidence: a live run in
// this process is ReachableActive; a committed terminal is
// ReachableTerminal; recorded pre-acceptance evidence (start_failed or
// pre-write rejection ⇒ missing, or an attempt that never reserved a
// launch, so no process can exist) is DefinitivelyMissing; everything
// else stays Uncertain until a controller disposition. A present
// conversation file or a dead process never establishes an outcome.
// RecoveryRef is echoed back verbatim.
func (a *AgyAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	uncertain := adapter.ReconciliationOutcome{Ref: ref, Reachability: council.VisibilityHostLost,
		Status: adapter.ReconciliationUncertain, Observed: council.TurnRunning}
	if err := ref.Validate(); err != nil {
		return uncertain, err
	}
	a.mu.Lock()
	run := a.turns[ref.TurnRef]
	a.mu.Unlock()
	if run != nil {
		observed := council.TurnRunning
		if _, _, cancelling := run.status(); cancelling {
			observed = council.TurnCancelling
		}
		return adapter.ReconciliationOutcome{Ref: ref, Reachability: council.VisibilityReachable,
			Status: adapter.ReconciliationReachableActive, Observed: observed}, nil
	}
	attempt, err := a.store.GetLatestAgyTurnAttempt(ctx, string(ref.SessionID), ref.TurnKey)
	if err != nil {
		return uncertain, fmt.Errorf("reconcile attempt lookup: %w", err)
	}
	if attempt == nil {
		return uncertain, nil
	}
	missing := adapter.ReconciliationOutcome{Ref: ref, Reachability: council.VisibilityReachable,
		Status: adapter.ReconciliationDefinitivelyMissing, Observed: council.TurnFailed}
	switch {
	case attempt.Terminal:
		res, _ := a.resultFromAttempt(ref.TurnRef, attempt)
		observed := council.TurnCompleted
		switch attempt.ObservedStatus {
		case "failed":
			observed = council.TurnFailed
		case "cancelled":
			observed = council.TurnCancelled
		}
		return adapter.ReconciliationOutcome{Ref: ref, Reachability: council.VisibilityReachable,
			Status: adapter.ReconciliationReachableTerminal, Observed: observed, Result: res.Output}, nil
	case attempt.ObservedStatus == "missing":
		return missing, nil
	case attempt.LaunchCount == 0:
		// No reservation ⇒ no process can have started. Unless THIS
		// process is mid-dispatch on it, record the positive pre-start
		// evidence so the conversation is no longer blocked.
		if a.slotOwnedBy(attempt.AttemptID) {
			return adapter.ReconciliationOutcome{Ref: ref, Reachability: council.VisibilityReachable,
				Status: adapter.ReconciliationReachableActive, Observed: council.TurnRunning}, nil
		}
		if err := a.durable(opAttemptMissing, func() error {
			return a.store.SetAgyAttemptObservedStatus(ctx, attempt.AttemptID, "missing")
		}); err != nil {
			return uncertain, fmt.Errorf("record missing: %w", err)
		}
		return missing, nil
	}
	return uncertain, nil
}

// ── Single flight ───────────────────────────────────────────────────────

func (a *AgyAdapter) acquireSlot(nativeID, attempt string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if owner, held := a.slots[nativeID]; held && owner != attempt {
		return ErrConversationBusy
	} else if held {
		return fmt.Errorf("attempt %s is already dispatching on %s", attempt, nativeID)
	}
	a.slots[nativeID] = attempt
	return nil
}

func (a *AgyAdapter) releaseSlot(nativeID, attempt string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.slots[nativeID] == attempt {
		delete(a.slots, nativeID)
	}
}

func (a *AgyAdapter) slotOwnedBy(attempt string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, owner := range a.slots {
		if owner == attempt {
			return true
		}
	}
	return false
}

// isLive reports whether this process holds a live run for ref.
func (a *AgyAdapter) isLive(ref adapter.TurnRef) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.turns[ref]
	return ok
}
