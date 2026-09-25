//go:build unix

package agy

// Shared adapter-contract harness (AC-010 Task 5): a real storage.Store,
// a real AC-005 workspace allocation, the storage-backed launch source,
// and the compiled fixture `agy` executable from agytest launched through
// the real PolicyExecutor (sealed on Linux). The only test seam is
// testExecutor, which wraps execpolicy.New() to (a) authorize the
// fixture launch off Linux through the explicit FixtureLaunch marker
// (test files only — production code never names it), and (b) inject
// crash gaps between durable phases. NEVER the real agy binary.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/agy/agytest"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const (
	testRunID     = "run-agy"
	testSessionID = "sess-agy-1"
	testNativeID  = "3f2c1a9e-8b7d-4c6e-9f01-23456789abcd"
	otherNativeID = "7a6b5c4d-3e2f-4a1b-8c9d-0e1f2a3b4c5d"
	testLease     = "lease-agy-adapter"
)

var (
	sharedFixtureOnce sync.Once
	sharedFixture     *agytest.Fixture
	sharedFixtureErr  error
)

func agyFixture(t *testing.T) *agytest.Fixture {
	t.Helper()
	sharedFixtureOnce.Do(func() {
		sharedFixture, sharedFixtureErr = agytest.NewFixture()
	})
	if sharedFixtureErr != nil {
		t.Fatalf("agytest fixture: %v", sharedFixtureErr)
	}
	return sharedFixture
}

// ── test executor ───────────────────────────────────────────────────────

type testExecutor struct {
	inner execpolicy.PolicyExecutor
	// starts counts create/turn (stream-json) launches only; gateStarts
	// counts the provider-free gate launches (`models`, `plugin list`,
	// Task 6), which the beforeStart/wrapProc/lastReq seams never see.
	starts     atomic.Int32
	gateStarts atomic.Int32

	mu          sync.Mutex
	beforeStart func(req execpolicy.LaunchRequest) error
	wrapProc    func(p execpolicy.ManagedProcess) execpolicy.ManagedProcess
	lastReq     execpolicy.LaunchRequest
}

// isGateLaunch reports a provider-free gate launch (`models` or
// `plugin list`).
func isGateLaunch(req execpolicy.LaunchRequest) bool {
	return len(req.Args) > 0 && (req.Args[0] == "models" || req.Args[0] == "plugin")
}

func (e *testExecutor) Start(ctx context.Context, req execpolicy.LaunchRequest) (execpolicy.ManagedProcess, error) {
	if isGateLaunch(req) {
		if req.SealedImage == nil && runtime.GOOS != "linux" {
			req.FixtureLaunch = true
		}
		e.gateStarts.Add(1)
		return e.inner.Start(ctx, req)
	}
	e.mu.Lock()
	before, wrap := e.beforeStart, e.wrapProc
	e.lastReq = req
	e.mu.Unlock()
	if before != nil {
		if err := before(req); err != nil {
			return nil, err
		}
	}
	if req.SealedImage == nil && runtime.GOOS != "linux" {
		// Off Linux NewSealedImage is unsupported: the fixture launch is
		// authorized by the explicit test-only marker (agytest's rule).
		req.FixtureLaunch = true
	}
	e.starts.Add(1)
	p, err := e.inner.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	if wrap != nil {
		return wrap(p), nil
	}
	return p, nil
}

func (e *testExecutor) setBeforeStart(fn func(req execpolicy.LaunchRequest) error) {
	e.mu.Lock()
	e.beforeStart = fn
	e.mu.Unlock()
}

func (e *testExecutor) setWrapProc(fn func(p execpolicy.ManagedProcess) execpolicy.ManagedProcess) {
	e.mu.Lock()
	e.wrapProc = fn
	e.mu.Unlock()
}

func (e *testExecutor) last() execpolicy.LaunchRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastReq
}

// wrappedProc lets a test replace the child's stdin (crash gaps, input
// rewriting) while every other surface stays the real process.
type wrappedProc struct {
	execpolicy.ManagedProcess
	stdin io.WriteCloser
}

func (w *wrappedProc) Stdin() io.WriteCloser { return w.stdin }

// ── identity / required tools ───────────────────────────────────────────

type fnIdentity struct {
	fn func(ref adapter.TurnRef) (string, bool)
}

func (i fnIdentity) AttemptFor(_ context.Context, ref adapter.TurnRef) (string, bool) {
	return i.fn(ref)
}

// mapRequired is the harness RequiredToolsSource: a turn key absent
// from m stands for a present intent with an EMPTY set (defaults apply);
// fail injects the seam's error channel (a read failure or a missing
// intent) per turn key.
type mapRequired struct {
	mu   sync.Mutex
	m    map[string][]string
	fail map[string]error
}

func (r *mapRequired) RequiredToolsFor(_ context.Context, ref adapter.TurnRef) ([]string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail[ref.TurnKey]; err != nil {
		return nil, false, err
	}
	tools, ok := r.m[ref.TurnKey]
	return tools, ok && len(tools) > 0, nil
}

func (r *mapRequired) failWith(turnKey string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail == nil {
		r.fail = map[string]error{}
	}
	r.fail[turnKey] = err
}

func (r *mapRequired) set(turnKey string, tools []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[turnKey] = tools
}

// ── harness ─────────────────────────────────────────────────────────────

type agyHarness struct {
	t             *testing.T
	stateDir      string
	store         *storage.Store
	wm            *workspace.WorkspaceManager
	fx            *agytest.Fixture
	evidenceRoot  string
	exec          *testExecutor
	source        *StorageLaunchSource
	policy        AgyLaunchPolicy
	profile       storage.CanonicalProfile
	profileDigest string
	model         string
	wsRoot        string
	home          string
	required      *mapRequired
	adapter       *AgyAdapter
}

func newAgyHarness(t *testing.T) *agyHarness {
	return newAgyHarnessProfile(t, nil)
}

func newAgyHarnessProfile(t *testing.T, mutate func(*storage.CanonicalProfile)) *agyHarness {
	t.Helper()
	fx := agyFixture(t)

	profile, evidenceRoot := acceptedProfileAndRoot(t)
	// expected_home names the operator's .gemini directory (spec §3.7);
	// the launch HOME is its parent. Never the real home.
	home := filepath.Join(t.TempDir(), ".gemini")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir expected home: %v", err)
	}
	a := profile.Harnesses["agy"].Agy
	a.BinaryPath = fx.BinaryPath
	a.BinaryDigest = fx.Digest
	a.ExpectedHome = home
	a.Platform = storage.AgyPlatformSpec{OS: runtime.GOOS, Family: "unix"}
	// The configured toolkit state the Task 6 checks verify before every
	// launch, staged in the temp home: the hooks capture (its canonical
	// digest frozen) and the one expected skill directory.
	a.HooksConfigDigest = stageHooks(t, home, defaultHooksJSON)
	for _, skill := range a.ExpectedSkills {
		if err := os.MkdirAll(filepath.Join(home, "antigravity-cli", "skills", skill), 0o700); err != nil {
			t.Fatalf("mkdir skill: %v", err)
		}
	}
	if mutate != nil {
		mutate(&profile)
	}
	policy, err := ValidateAgyHarness(profile, evidenceRoot)
	if err != nil {
		t.Fatalf("validate agy harness: %v", err)
	}
	digest, _, err := storage.ComputeProfileDigest(profile)
	if err != nil {
		t.Fatalf("profile digest: %v", err)
	}

	scratch := t.TempDir()
	stateDir := filepath.Join(scratch, "state")
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-agy", ControllerLease: testLease, RunID: testRunID,
		Brief: "agy adapter test", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      profile,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-agy", testLease, storage.SessionRecord{
		ID: testSessionID, RunID: testRunID, Contributor: "agy", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	wm, err := workspace.NewWorkspaceManager(filepath.Join(scratch, "wm-state"), filepath.Join(scratch, "ws"))
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}
	paths, err := wm.AllocateWorkspace(testRunID, testSessionID, profile.WorkspaceMode, "", "")
	if err != nil {
		t.Fatalf("allocate workspace: %v", err)
	}

	h := &agyHarness{
		t: t, stateDir: stateDir, store: store, wm: wm, fx: fx, evidenceRoot: evidenceRoot,
		exec:   &testExecutor{inner: execpolicy.New()},
		policy: policy, profile: profile, profileDigest: digest,
		model:    profile.Harnesses["agy"].Model,
		wsRoot:   paths.Root,
		home:     home,
		required: &mapRequired{m: map[string][]string{}},
	}
	h.source = NewAgyTurnLaunchSource(store, wm, policy, fx.SealedImage)
	h.adapter = h.newAdapter()
	t.Cleanup(func() { _ = h.store.Close() })
	return h
}

func (h *agyHarness) newAdapter() *AgyAdapter {
	h.t.Helper()
	a, err := NewFixtureScopedAdapter(h.store, h.exec, h.source, h.wm, h.policy, h.profileDigest,
		h.fx.SealedImage, fnIdentity{fn: defaultAttempt}, h.required, FixtureMode{})
	if err != nil {
		h.t.Fatalf("NewFixtureScopedAdapter: %v", err)
	}
	return a
}

// defaultHooksJSON is the staged operator hooks capture: one guardrail
// entry with a non-empty command and no disabled marker.
const defaultHooksJSON = `{"guardrail": {"command": "guardrail.sh", "enabled": true, "event": "PreToolUse"}}`

// stageHooks writes raw as <home>/config/hooks.json and returns its
// canonical digest (what the profile freezes as hooks_config_digest).
func stageHooks(t *testing.T, home, raw string) string {
	t.Helper()
	dir := filepath.Join(home, "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir hooks dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write hooks.json: %v", err)
	}
	digest, err := CanonicalHooksConfigDigest([]byte(raw))
	if err != nil {
		t.Fatalf("hooks digest: %v", err)
	}
	return digest
}

// gateLines returns the gate-launch argv log lines of kind ("models" or
// "plugin\x1flist").
func (h *agyHarness) gateLines(kind string) []string {
	h.t.Helper()
	var out []string
	for _, l := range h.fixtureFile(".agy-fixture-gate-args") {
		if l == kind {
			out = append(out, l)
		}
	}
	return out
}

func defaultAttempt(ref adapter.TurnRef) (string, bool) {
	return "att-" + ref.TurnKey, true
}

// reopen simulates a service restart: the store is closed and reopened
// at the same state dir and a fresh adapter is built over it.
func (h *agyHarness) reopen() {
	h.t.Helper()
	_ = h.store.Close()
	store, err := storage.Open(storage.StoreOptions{StateDir: h.stateDir})
	if err != nil {
		h.t.Fatalf("reopen store: %v", err)
	}
	h.store = store
	h.source = NewAgyTurnLaunchSource(store, h.wm, h.policy, h.fx.SealedImage)
	h.adapter = h.newAdapter()
}

// scenario stages the child's directives: the frozen tool inventory
// first (the fixture's default is the unmodified 57-tool install), then
// the given lines (a later "tools" line overrides).
func (h *agyHarness) scenario(lines ...string) {
	h.t.Helper()
	toolsLine := `{"tools": ["` + strings.Join(nonUncoveredTools, `","`) + `"]}`
	all := append([]string{toolsLine}, lines...)
	joined := strings.Join(all, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(h.wsRoot, ".agy-fixture-scenario.jsonl"), []byte(joined), 0o600); err != nil {
		h.t.Fatalf("write scenario: %v", err)
	}
}

// turnScenario stages a turn on the persisted conversation: the fixture
// echoes the known id back.
func (h *agyHarness) turnScenario(lines ...string) {
	h.t.Helper()
	h.scenario(append([]string{`{"known_conversation": "` + testNativeID + `"}`}, lines...)...)
}

func (h *agyHarness) fixtureFile(name string) []string {
	h.t.Helper()
	return readAgyFixtureFile(h.t, h.wsRoot, name)
}

func (h *agyHarness) createRequest() adapter.CreateSessionRequest {
	return adapter.CreateSessionRequest{
		SessionID:   testSessionID,
		Contributor: "agy",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	}
}

func (h *agyHarness) persist(nativeID string) {
	h.t.Helper()
	if err := h.store.InsertAgySessionBinding(context.Background(), storage.AgySessionBinding{
		SessionID: testSessionID, NativeID: nativeID,
		Model: h.model, Workspace: h.wsRoot, ProfileDigest: h.profileDigest,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		h.t.Fatalf("persist binding: %v", err)
	}
}

func (h *agyHarness) ref(turnKey string) adapter.TurnRef {
	return adapter.TurnRef{SessionID: testSessionID, TurnKey: turnKey}
}

func (h *agyHarness) dispatch(turnKey, prompt string) (adapter.DispatchOutcome, error) {
	return h.adapter.Dispatch(context.Background(), h.ref(turnKey), prompt)
}

// waitAttempt polls the durable attempt until pred holds.
func (h *agyHarness) waitAttempt(attemptID string, pred func(*storage.AgyTurnAttempt) bool) *storage.AgyTurnAttempt {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, err := h.store.GetAgyTurnAttempt(context.Background(), attemptID)
		if err == nil && a != nil && pred(a) {
			return a
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("attempt %s never reached the expected state; last=%+v err=%v", attemptID, a, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitIdle polls until the adapter no longer holds a live run for ref
// (the turn goroutine finished its classification and released).
func (h *agyHarness) waitIdle(ref adapter.TurnRef) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.adapter.isLive(ref) {
		if time.Now().After(deadline) {
			h.t.Fatalf("turn %v never finished", ref)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *agyHarness) launchStates(attemptID string) []string {
	h.t.Helper()
	states, err := h.store.AgyAttemptLaunchStates(context.Background(), attemptID)
	if err != nil {
		h.t.Fatalf("launch states: %v", err)
	}
	return states
}

// withShortTimers shortens the adapter-owned bounds for one test.
func withShortTimers(t *testing.T, init, grace time.Duration) {
	t.Helper()
	oldInit, oldGrace := initTimeout, cancelGrace
	initTimeout, cancelGrace = init, grace
	t.Cleanup(func() { initTimeout, cancelGrace = oldInit, oldGrace })
}

func isUncertainCreation(err error) (*adapter.ErrSessionCreationUncertain, bool) {
	var unc *adapter.ErrSessionCreationUncertain
	ok := errors.As(err, &unc)
	return unc, ok
}

// waitAllIdle waits (bounded) until the adapter holds no live run.
func (a *AgyAdapter) waitAllIdle(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		n := len(a.turns)
		a.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
