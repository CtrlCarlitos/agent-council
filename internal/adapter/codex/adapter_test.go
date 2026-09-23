//go:build unix

package codex

// Task 5 adapter contract evidence (POSIX — real executor + fixture
// child): the enforceable §3.4 creation gate ordering (attestation
// pre-child, auth gate post-child-start with no thread or turn created,
// creation-reservation idempotency/concurrency, lost response), §3.4
// local-only resume, and §3.5 dispatch (pre-transmission verification
// BEFORE any prompt for EVERY turn — one drift case per compared field,
// accepted/rejected/unknown acceptance semantics, single-flight,
// baseline persistence, crash boundaries through the adapter).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const (
	testSessionID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6f"
	testThreadID  = "01900000-0000-7000-8000-000000000001"
	testTurnID    = "01900000-0000-7000-8000-0000000000aa"
)

// testAttestationID is a well-formed cprot-v2 attestation id for the
// harness's eligibility lookup.
func testAttestationID() string {
	return "cprot-v2:sha256:" + strings.Repeat("ab", 32)
}

type adapterHarness struct {
	store         *storage.Store
	server        *CodexServer
	adapter       *CodexAdapter
	scratch       string
	wsRoot        string
	policy        CodexLaunchPolicy
	profileDigest string
	model         string
	eligible      atomic.Bool
}

// codexIdentity maps a TurnRef to its deterministic attempt identity.
type codexIdentity struct {
	fn func(ref adapter.TurnRef) (string, bool)
}

func (i codexIdentity) AttemptFor(_ context.Context, ref adapter.TurnRef) (string, bool) {
	return i.fn(ref)
}

func defaultIdentity(ref adapter.TurnRef) (string, bool) {
	return "att-" + ref.TurnKey, true
}

func newAdapterHarness(t *testing.T) *adapterHarness {
	return newAdapterHarnessScenario(t, nil)
}

// newAdapterHarnessScenario stages exactly the given scenario lines for
// the (single) child this harness will launch.
func newAdapterHarnessScenario(t *testing.T, lines []string) *adapterHarness {
	t.Helper()
	scratch := t.TempDir()
	wsRoot := t.TempDir()

	profile, evidenceRoot := evidenceRootForCodex(t, v3CodexProfile())
	cx := profile.Harnesses["codex"].Codex
	cx.ExpectedCodexHome = filepath.Join(scratch, ".codex")
	cx.Platform = storage.CodexPlatformSpec{OS: runtime.GOOS, Family: "unix"}
	cx.SandboxPolicy.WritableRoots = []string{wsRoot}
	cx.SandboxPolicy.Type = "workspace-write"
	cx.SandboxPolicy.NetworkAccess = false

	policy, err := ValidateCodexHarness(profile, evidenceRoot)
	if err != nil {
		t.Fatalf("validate codex harness: %v", err)
	}
	digest, _, err := storage.ComputeProfileDigest(profile)
	if err != nil {
		t.Fatalf("profile digest: %v", err)
	}

	binDir := compileCodexFixture(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	store, err := storage.Open(storage.StoreOptions{StateDir: filepath.Join(scratch, "state")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const lease = "lease-codex-adapter"
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-codex", ControllerLease: lease, RunID: "run-codex",
		Brief: "adapter test", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      profile,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-codex", lease, storage.SessionRecord{
		ID: testSessionID, RunID: "run-codex", Contributor: "codex", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	model := profile.Harnesses["codex"].Model
	server := NewCodexServer(execpolicy.New(), lifecycleLaunchSource{scratch: scratch}, policy)
	h := &adapterHarness{
		store: store, server: server,
		scratch: scratch, wsRoot: wsRoot,
		policy: policy, profileDigest: digest, model: model,
	}
	h.eligible.Store(true)

	att := func() (string, bool) {
		if !h.eligible.Load() {
			return "", false
		}
		return testAttestationID(), true
	}
	h.adapter = NewCodexAdapter(store, server, policy, digest, codexIdentity{fn: defaultIdentity}, att)

	if lines == nil {
		lines = append([]string{authOKLine()}, threadStartRules(testThreadID, wsRoot, model)...)
		lines = append(lines, resumeRule(testThreadID, wsRoot, model, nil))
	}
	writeScenario(t, scratch, lines...)
	t.Cleanup(func() { server.stopAll(context.Background()) })
	return h
}

// ── Scenario line builders ──────────────────────────────────────────────

func authOKLine() string {
	return `{"respond": {"method":"account/read","result":{"account":{"accountId":"acc"},"requiresOpenaiAuth":false}}}`
}

func threadStartRules(threadID, wsRoot, model string) []string {
	notif := `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"` + threadID + `","sessionId":"` + threadID + `","status":{"type":"idle"}}}}`
	result := `{"id":"` + threadID + `","sessionId":"` + threadID + `","environments":[{"environmentId":"local","cwd":"` + wsRoot + `"}],"status":{"type":"idle"},"model":"` + model + `","historyMode":"paginated","path":"rollout-fixture.jsonl"}`
	return []string{
		`{"emit_on_request": {"method":"thread/start","line":` + notif + `}}`,
		`{"respond": {"method":"thread/start","result":` + result + `}}`,
	}
}

// resumeRule builds the thread/resume response matching the frozen
// policy, with drift applied per field for the §3.5 rejection matrix.
func resumeRule(threadID, wsRoot, model string, drift func(map[string]any)) string {
	cfg := map[string]any{
		"id":                 threadID,
		"sessionId":          threadID,
		"status":             map[string]any{"type": "idle"},
		"cwd":                wsRoot,
		"approvalPolicy":     "on-request",
		"sandbox":            map[string]any{"type": "workspace-write", "writable_roots": []string{wsRoot}, "network_access": false},
		"approvalsReviewer":  "user",
		"model":              model,
		"modelProvider":      "openai",
		"instructionSources": []string{"~/.codex/AGENTS.md"},
	}
	if drift != nil {
		drift(cfg)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return `{"respond": {"method":"thread/resume","result":` + string(raw) + `}}`
}

func turnAcceptedRules(threadID, turnID string, complete bool) []string {
	// The fixture consumes at most ONE on-request and ONE after-response
	// rule per request: turn/started rides emit_on_request (before the
	// response), turn/completed rides emit_after_response (after it).
	out := []string{
		`{"respond": {"method":"turn/start","result":{"id":"` + turnID + `","threadId":"` + threadID + `","status":{"type":"inProgress"}}}}`,
		`{"emit_on_request": {"method":"turn/start","line":{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"` + threadID + `","turnId":"` + turnID + `"}}}}`,
	}
	if complete {
		out = append(out, `{"emit_after_response": {"method":"turn/start","line":{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"`+threadID+`","turn":{"id":"`+turnID+`","items":[],"status":"completed","durationMs":100}}}}}`)
	}
	return out
}

// ── Harness operations ──────────────────────────────────────────────────

func (h *adapterHarness) create(t *testing.T, sessionID string) adapter.SessionBinding {
	t.Helper()
	binding, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID(sessionID),
		Contributor: "codex",
		Config: adapter.SessionConfig{
			WorkspaceRoot: h.wsRoot,
			Model:         h.model,
		},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return binding
}

// persist is the §3.4 service/storage seam: the adapter returned the
// binding, storage records it.
func (h *adapterHarness) persist(t *testing.T, sessionID string, binding adapter.SessionBinding) {
	t.Helper()
	if err := h.store.InsertCodexSessionBinding(context.Background(), storage.CodexSessionBinding{
		SessionID: sessionID, NativeID: binding.NativeSessionID,
		Model: h.model, Workspace: h.wsRoot,
		ProfileDigest: h.profileDigest, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("persist binding: %v", err)
	}
}

func (h *adapterHarness) createAndPersist(t *testing.T) adapter.SessionBinding {
	t.Helper()
	binding := h.create(t, testSessionID)
	h.persist(t, testSessionID, binding)
	return binding
}

func (h *adapterHarness) dispatch(t *testing.T, turnKey, prompt string) (adapter.DispatchOutcome, error) {
	t.Helper()
	return h.adapter.Dispatch(context.Background(), adapter.TurnRef{
		SessionID: testSessionID, TurnKey: turnKey,
	}, prompt)
}

func requestLog(t *testing.T, scratch string) []string {
	t.Helper()
	return readFixtureFile(t, scratch, ".codex-fixture-requests")
}

// readReplies reads the verbatim reply frames the fixture logged
// (Council's answers to server→client approval requests).
func readReplies(t *testing.T, scratch string) []string {
	t.Helper()
	return readFixtureFile(t, scratch, ".codex-fixture-replies")
}

// waitForReply polls the reply log until a frame carrying the given
// JSON-encoded id appears, returning the whole log.
func waitForReply(t *testing.T, scratch, id string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, line := range readReplies(t, scratch) {
			if strings.Contains(line, `"id":`+id+",") || strings.Contains(line, `"id":`+id+"}") {
				return readReplies(t, scratch)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reply frame with id %s ever appeared; replies=%v", id, readReplies(t, scratch))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func terminatedCount(t *testing.T, scratch string) int {
	t.Helper()
	return len(readFixtureFile(t, scratch, ".codex-fixture-terminated"))
}

// waitAttempt polls the latest attempt row until pred holds.
func waitAttempt(t *testing.T, h *adapterHarness, turnKey string, pred func(*storage.CodexTurnAttempt) bool) *storage.CodexTurnAttempt {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a, err := h.store.GetLatestCodexTurnAttempt(context.Background(), testSessionID, turnKey)
		if err == nil && a != nil && pred(a) {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempt %s never reached the expected state; last=%+v err=%v", turnKey, a, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func withTimeoutVar(t *testing.T, target *time.Duration, d time.Duration) {
	t.Helper()
	old := *target
	*target = d
	t.Cleanup(func() { *target = old })
}

// seedRollout writes a rollout fixture whose session_meta matches the
// native thread, at the date-path shape the native home uses.
func seedRollout(t *testing.T, h *adapterHarness, threadID string, extraLines ...string) string {
	t.Helper()
	dir := filepath.Join(h.scratch, ".codex", "sessions", "2026", "09", "23")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	path := filepath.Join(dir, "rollout-2026-09-23T00-00-00-"+threadID+".jsonl")
	lines := append([]string{
		`{"timestamp":"2026-09-23T00:00:00Z","type":"session_meta","payload":{"session_id":"` + threadID + `"}}`,
		`{"timestamp":"2026-09-23T00:00:01Z","type":"turn_context","payload":{"permission_profile":{"type":"managed"}}}`,
	}, extraLines...)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

// ── §3.4 CreateSession gate ordering ────────────────────────────────────

// The full enforceable ordering on success: attestation (no-op to
// observe directly — the child exists) → child start → handshake →
// account/read → creation-reservation → thread/start. The request log
// proves the ORDER: initialize, account/read, thread/start — and the
// binding carries the native UUIDv7 with materialized=false, persisted
// by nobody but the service layer.
func TestCodexAdapter_CreateSessionGateOrdering(t *testing.T) {
	h := newAdapterHarness(t)
	binding := h.create(t, testSessionID)

	if binding.NativeSessionID != testThreadID {
		t.Fatalf("native id: %q", binding.NativeSessionID)
	}
	if binding.Config.Model != h.model || binding.Config.WorkspaceRoot != h.wsRoot {
		t.Fatalf("binding config: %+v", binding.Config)
	}
	got := requestLog(t, h.scratch)
	want := []string{"initialize", "account/read", "thread/start"}
	if len(got) != len(want) {
		t.Fatalf("request order must be %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request order must be %v, got %v", want, got)
		}
	}
	if existing, err := h.store.GetCodexSessionBinding(context.Background(), testSessionID); err != nil || existing != nil {
		t.Fatalf("adapter must not persist; store saw %+v err=%v", existing, err)
	}
}

// §3.3 ordering step 1: a missing eligibility attestation fails closed
// BEFORE any process starts — no launch evidence at all.
func TestCodexAdapter_CreateSessionEligibilityMissingPreChild(t *testing.T) {
	h := newAdapterHarness(t)
	h.eligible.Store(false)

	_, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   testSessionID,
		Contributor: "codex",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	})
	var missing *ErrProductionEligibilityMissing
	if !errors.As(err, &missing) {
		t.Fatalf("expected ErrProductionEligibilityMissing, got %T: %v", err, err)
	}
	if lines := readFixtureFile(t, h.scratch, ".codex-fixture-args"); lines != nil {
		t.Fatalf("no process may start without eligibility, launches: %v", lines)
	}
	if lines := requestLog(t, h.scratch); lines != nil {
		t.Fatalf("no requests may reach a child without eligibility, got %v", lines)
	}
}

// The production constructor has no path around the gate: a nil
// attestation lookup behaves exactly like a missing attestation.
func TestCodexAdapter_CreateSessionNilAttestationFailsClosed(t *testing.T) {
	h := newAdapterHarness(t)
	bare := NewCodexAdapter(h.store, h.server, h.policy, h.profileDigest, codexIdentity{fn: defaultIdentity}, nil)
	_, err := bare.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   testSessionID,
		Contributor: "codex",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	})
	var missing *ErrProductionEligibilityMissing
	if !errors.As(err, &missing) {
		t.Fatalf("nil lookup must fail closed, got %T: %v", err, err)
	}
	if lines := readFixtureFile(t, h.scratch, ".codex-fixture-args"); lines != nil {
		t.Fatalf("no process may start without eligibility, launches: %v", lines)
	}
}

// §3.3 ordering step 2: the auth gate fires AFTER the child starts but
// BEFORE any thread or turn exists. Unauthenticated ⇒ typed rejection,
// child terminated, and thread/start never reached the child.
func TestCodexAdapter_CreateSessionAuthGatePostChild(t *testing.T) {
	h := newAdapterHarnessScenario(t, []string{
		`{"respond": {"method":"account/read","result":{"account":null,"requiresOpenaiAuth":true}}}`,
	})
	_, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   testSessionID,
		Contributor: "codex",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	})
	var authErr *ErrCodexAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("expected ErrCodexAuthRequired, got %T: %v", err, err)
	}
	got := requestLog(t, h.scratch)
	if len(got) != 2 || got[0] != "initialize" || got[1] != "account/read" {
		t.Fatalf("auth gate must fire after the handshake and before any thread, got %v", got)
	}
	if terminatedCount(t, h.scratch) < 1 {
		t.Fatal("the auth-gate rejection must terminate the child")
	}
}

// An INCONCLUSIVE auth check (error response) fails closed the same way.
func TestCodexAdapter_CreateSessionAuthInconclusiveFailsClosed(t *testing.T) {
	h := newAdapterHarness(t)
	writeScenario(t, h.scratch,
		`{"respond_error": {"method":"account/read","code":-32600,"message":"Invalid request: unknown variant <account/read>"}}`,
		`{"respond": {"method":"thread/start","result":{"id":"never-reached"}}}`)
	_, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   testSessionID,
		Contributor: "codex",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	})
	var authErr *ErrCodexAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("inconclusive auth must fail closed, got %T: %v", err, err)
	}
	for _, r := range requestLog(t, h.scratch) {
		if r == "thread/start" {
			t.Fatal("no thread may be created behind a failed auth gate")
		}
	}
}

// Repeated identical config is idempotent off the persisted binding (no
// new child); changed config fails closed with the typed mismatch.
func TestCodexAdapter_CreateSessionIdempotentAndMismatch(t *testing.T) {
	h := newAdapterHarness(t)
	binding := h.createAndPersist(t)

	again, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   testSessionID,
		Contributor: "codex",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	})
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if again.NativeSessionID != binding.NativeSessionID {
		t.Fatalf("idempotency broken: %q vs %q", again.NativeSessionID, binding.NativeSessionID)
	}

	_, err = h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   testSessionID,
		Contributor: "codex",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: "gpt-6-astra"},
	})
	var mismatch *ErrSessionConfigMismatch
	if !errors.As(err, &mismatch) || mismatch.Field != "model" {
		t.Fatalf("changed config must fail closed, got %T: %v", err, err)
	}

	// The second/third calls must not have started another child.
	if args := readFixtureFile(t, h.scratch, ".codex-fixture-args"); len(args) != 1 {
		t.Fatalf("idempotent replay must not launch a child, launches: %v", args)
	}
}

// Concurrent duplicate CreateSession shares one native identity: the
// creator performs the creation, waiters receive the same binding, and
// exactly one thread/start reaches the child.
func TestCodexAdapter_CreateSessionConcurrentShared(t *testing.T) {
	h := newAdapterHarness(t)
	const n = 8
	bindings := make([]adapter.SessionBinding, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			bindings[i], errs[i] = h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
				SessionID:   testSessionID,
				Contributor: "codex",
				Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
			})
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent caller %d: %v", i, errs[i])
		}
		if bindings[i].NativeSessionID != testThreadID {
			t.Fatalf("caller %d received %q", i, bindings[i].NativeSessionID)
		}
	}
	threads := 0
	for _, r := range requestLog(t, h.scratch) {
		if r == "thread/start" {
			threads++
		}
	}
	if threads != 1 {
		t.Fatalf("exactly one thread/start may execute, got %d (%v)", threads, requestLog(t, h.scratch))
	}
}

// A lost thread/start response (ack never observed) is creation
// UNCERTAIN — the native thread may exist; recreation is not automatic.
func TestCodexAdapter_CreateSessionLostResponseUncertain(t *testing.T) {
	h := newAdapterHarness(t)
	writeScenario(t, h.scratch,
		authOKLine(),
		`{"emit_on_request": {"method":"thread/start","line":{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}}}`,
		`{"respond": {"method":"thread/start","delay_ms":2000,"result":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}`)
	withTimeoutVar(t, &creationAckTimeout, 200*time.Millisecond)

	_, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   testSessionID,
		Contributor: "codex",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	})
	var uncertain *adapter.ErrSessionCreationUncertain
	if !errors.As(err, &uncertain) {
		t.Fatalf("expected ErrSessionCreationUncertain, got %T: %v", err, err)
	}
	if terminatedCount(t, h.scratch) < 1 {
		t.Fatal("an uncertain creation must terminate the child (never silently reused)")
	}
}

// The confirmed thread id must be a canonical UUIDv7; anything else is
// id drift — uncertain, never bound.
func TestCodexAdapter_CreateSessionDriftedIdFailsClosed(t *testing.T) {
	h := newAdapterHarness(t)
	writeScenario(t, h.scratch,
		authOKLine(),
		`{"emit_on_request": {"method":"thread/start","line":{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"definitely-not-a-real-id","status":{"type":"idle"}}}}}}`,
		`{"respond": {"method":"thread/start","result":{"id":"definitely-not-a-real-id","status":{"type":"idle"}}}}`)

	_, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   testSessionID,
		Contributor: "codex",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	})
	var uncertain *adapter.ErrSessionCreationUncertain
	if !errors.As(err, &uncertain) || uncertain.PartialNativeID != "definitely-not-a-real-id" {
		t.Fatalf("non-canonical id must fail closed as uncertain, got %T: %v", err, err)
	}
}

// §3.4: automatic recreation is blocked after an uncertain creation. The
// second CreateSession for the same logical session returns the typed
// uncertain error WITHOUT starting a new child or thread; only the
// explicit ResolveCreationUncertainty seam clears the tombstone.
func TestCodexAdapter_CreationUncertainTombstoneBlocksRetry(t *testing.T) {
	h := newAdapterHarness(t)
	writeScenario(t, h.scratch,
		authOKLine(),
		`{"emit_on_request": {"method":"thread/start","line":{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}}}`,
		`{"respond": {"method":"thread/start","delay_ms":2000,"result":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}`)
	withTimeoutVar(t, &creationAckTimeout, 200*time.Millisecond)

	req := adapter.CreateSessionRequest{
		SessionID:   testSessionID,
		Contributor: "codex",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	}
	_, firstErr := h.adapter.CreateSession(context.Background(), req)
	var firstUnc *adapter.ErrSessionCreationUncertain
	if !errors.As(firstErr, &firstUnc) {
		t.Fatalf("first creation must be uncertain, got %T: %v", firstErr, firstErr)
	}

	// The automatic retry is refused by the tombstone — no new child.
	if _, err := h.adapter.CreateSession(context.Background(), req); err == nil {
		t.Fatal("retried creation must fail closed on the unresolved uncertainty")
	} else {
		var uncertain *adapter.ErrSessionCreationUncertain
		if !errors.As(err, &uncertain) {
			t.Fatalf("retry must carry the typed uncertain error, got %T: %v", err, err)
		}
	}
	if args := readFixtureFile(t, h.scratch, ".codex-fixture-args"); len(args) != 1 {
		t.Fatalf("the blocked retry must not start a child, launches: %v", args)
	}

	// Only the explicit resolution seam clears the tombstone.
	if !h.adapter.ResolveCreationUncertainty(testSessionID) {
		t.Fatal("resolve must report a cleared tombstone")
	}
	if h.adapter.ResolveCreationUncertainty(testSessionID) {
		t.Fatal("a second resolve must report nothing to clear")
	}
}

// ── §3.4 ResumeSession (local inspection only) ──────────────────────────

func TestCodexAdapter_ResumeSessionLocalOnly(t *testing.T) {
	h := newAdapterHarness(t)
	binding := h.createAndPersist(t)

	if err := h.adapter.ResumeSession(context.Background(), binding); err != nil {
		t.Fatalf("resume unmaterialized binding: %v", err)
	}
	// No native call, no child: the request log holds only what the
	// creation consumed, and exactly one child was ever launched.
	if args := readFixtureFile(t, h.scratch, ".codex-fixture-args"); len(args) != 1 {
		t.Fatalf("resume must not launch children, launches: %v", args)
	}

	// A materialized binding must still find its rollout, with
	// session_meta matching the native id.
	path := seedRollout(t, h, testThreadID)
	if err := h.store.MarkCodexSessionMaterialized(context.Background(), testSessionID, path); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if err := h.adapter.ResumeSession(context.Background(), binding); err != nil {
		t.Fatalf("resume materialized binding: %v", err)
	}

	// A missing rollout fails the local check.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove rollout: %v", err)
	}
	if err := h.adapter.ResumeSession(context.Background(), binding); err == nil {
		t.Fatal("materialized binding with a missing rollout must fail locally")
	}
}

func TestCodexAdapter_ResumeSessionMismatchFailsClosed(t *testing.T) {
	h := newAdapterHarness(t)
	binding := h.createAndPersist(t)

	wrongNative := binding
	wrongNative.NativeSessionID = "01900000-0000-7000-8000-0000000000ff"
	var mismatch *ErrSessionConfigMismatch
	if err := h.adapter.ResumeSession(context.Background(), wrongNative); !errors.As(err, &mismatch) {
		t.Fatalf("native id mismatch must fail closed, got %v", err)
	}

	wrongModel := binding
	wrongModel.Config.Model = "gpt-6-astra"
	if err := h.adapter.ResumeSession(context.Background(), wrongModel); !errors.As(err, &mismatch) {
		t.Fatalf("model mismatch must fail closed, got %v", err)
	}

	// A drifted frozen profile must refuse to resume the binding.
	other := NewCodexAdapter(h.store, h.server, h.policy, "cprof-v3:sha256:"+strings.Repeat("be", 32), codexIdentity{fn: defaultIdentity}, func() (string, bool) { return testAttestationID(), true })
	if err := other.ResumeSession(context.Background(), binding); !errors.As(err, &mismatch) || mismatch.Field != "profile_digest" {
		t.Fatalf("profile digest drift must fail closed, got %v", err)
	}
}

// ── §3.5 Dispatch ───────────────────────────────────────────────────────

// Accepted dispatch: resume verification runs BEFORE turn/start even on
// the FIRST turn (request order proves it), turn/started binds the
// native turn id, the launch boundary is recorded, the rollout baseline
// is taken and the binding materializes at first acceptance, and the
// turn completes to a verified terminal.
func TestCodexAdapter_DispatchAcceptedBindsNativeTurnID(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, true)...)
	writeScenario(t, h.scratch, scenario...)

	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	out, err := h.dispatch(t, "t-accept", "review the diff and summarize")
	if err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%s err=%v", out.Status, err)
	}

	// Request order: resume verification precedes turn/start (§3.5 step
	// 1, first turn included).
	got := requestLog(t, h.scratch)
	want := []string{"initialize", "account/read", "thread/start", "thread/resume", "turn/start"}
	if len(got) != len(want) {
		t.Fatalf("request order must be %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request order must be %v, got %v", want, got)
		}
	}

	att := waitAttempt(t, h, "t-accept", func(a *storage.CodexTurnAttempt) bool {
		return a.NativeTurnID != nil && *a.NativeTurnID == testTurnID && a.Terminal
	})
	if att.ObservedStatus != "completed" {
		t.Fatalf("terminal classification: %q", att.ObservedStatus)
	}
	if att.PromptDigest == "" {
		t.Fatal("prompt digest must be recorded at launch")
	}

	// Binding materialized at first acceptance with the rollout baseline.
	b, err := h.store.GetCodexSessionBinding(context.Background(), testSessionID)
	if err != nil || b == nil || !b.Materialized || b.RolloutPath == nil {
		t.Fatalf("first acceptance must materialize the binding: %+v err=%v", b, err)
	}
	if b.FirstPromptDigest == nil || *b.FirstPromptDigest != att.PromptDigest {
		t.Fatalf("first_prompt_digest must equal the attempt digest: %v", b.FirstPromptDigest)
	}
	if !att.BaselineMaterialized || att.BaselineEntries != 2 || att.BaselineSize == 0 || att.BaselineIdentity == "" {
		t.Fatalf("baseline persistence: mat=%v entries=%d size=%d identity=%q",
			att.BaselineMaterialized, att.BaselineEntries, att.BaselineSize, att.BaselineIdentity)
	}
}

// A definitive error response to turn/start is DispatchRejected — the
// native side definitively refused.
func TestCodexAdapter_DispatchRejectedDefinitiveError(t *testing.T) {
	h := newAdapterHarness(t)
	writeScenario(t, h.scratch,
		authOKLine(),
		`{"emit_on_request": {"method":"thread/start","line":{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}}}`,
		`{"respond": {"method":"thread/start","result":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}`,
		resumeRule(testThreadID, h.wsRoot, h.model, nil),
		`{"respond_error": {"method":"turn/start","code":-32600,"message":"thread not found: `+testThreadID+`"}}`)

	h.createAndPersist(t)
	out, err := h.dispatch(t, "t-rej", "prompt")
	if out.Status != adapter.DispatchRejected {
		t.Fatalf("definitive native refusal must reject, got %+v err=%v", out, err)
	}
	if !strings.Contains(out.Reason, "thread not found") {
		t.Fatalf("outcome: %+v", out)
	}

	att := waitAttempt(t, h, "t-rej", func(a *storage.CodexTurnAttempt) bool { return a.ObservedStatus == "missing" })
	if att.Terminal {
		t.Fatal("a refused turn is not terminal evidence")
	}

	// The slot was released: a second dispatch is NOT "thread busy".
	out2, _ := h.dispatch(t, "t-rej-2", "prompt")
	if out2.Status == adapter.DispatchRejected && strings.Contains(out2.Reason, "busy") {
		t.Fatal("the slot must be released after a definitive rejection")
	}
}

// A lost turn/start response (write completed, ack never observed) is
// DispatchUnknown; the transmission boundary is durable; the uncertain
// attempt blocks subsequent turns until disposition.
func TestCodexAdapter_DispatchUnknownLostResponse(t *testing.T) {
	h := newAdapterHarness(t)
	writeScenario(t, h.scratch,
		authOKLine(),
		`{"emit_on_request": {"method":"thread/start","line":{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}}}`,
		`{"respond": {"method":"thread/start","result":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}`,
		resumeRule(testThreadID, h.wsRoot, h.model, nil),
		`{"respond": {"method":"turn/start","delay_ms":2000,"result":{"id":"`+testTurnID+`","status":{"type":"inProgress"}}}}`)
	withTimeoutVar(t, &dispatchAckTimeout, 250*time.Millisecond)

	h.createAndPersist(t)
	out, err := h.dispatch(t, "t-unk", "prompt")
	if err != nil {
		t.Fatalf("unknown is an outcome, not a transport error: %v", err)
	}
	if out.Status != adapter.DispatchUnknown {
		t.Fatalf("outcome: %+v", out)
	}

	// Crash boundary: the boundary was recorded (write provably happened).
	att := waitAttempt(t, h, "t-unk", func(a *storage.CodexTurnAttempt) bool { return a.LaunchCount == 1 })
	row, err := h.store.GetCodexTurnAttempt(context.Background(), att.AttemptID)
	if err != nil || row == nil {
		t.Fatalf("attempt row: %v", err)
	}
	var firstByte interface{}
	if err := h.store.DB().QueryRowContext(context.Background(),
		`SELECT first_stdin_byte_at FROM codex_attempt_launches WHERE attempt_id = ? AND reservation_seq = 1`,
		att.AttemptID).Scan(&firstByte); err != nil || firstByte == nil {
		t.Fatalf("first_stdin_byte_at must be recorded after a written-but-unacked turn: %v %v", firstByte, err)
	}

	// The uncertain attempt durably blocks the native session.
	out2, err2 := h.dispatch(t, "t-unk-2", "prompt")
	if err2 != nil || out2.Status != adapter.DispatchRejected {
		t.Fatalf("subsequent turn must be durably blocked, got %+v err=%v", out2, err2)
	}
	if !strings.Contains(out2.Reason, "unresolved attempt") {
		t.Fatalf("block reason: %q", out2.Reason)
	}
}

// Pre-transmission drift rejection for EVERY compared field — the typed
// ErrProfileDrift fires BEFORE the prompt is ever written (no turn/start
// in the request log), the child is terminated, and the dispatch is
// rejected pre-acceptance.
func TestCodexAdapter_DriftMatrix(t *testing.T) {
	cases := []struct {
		name  string
		drift func(map[string]any)
	}{
		{"model", func(c map[string]any) { c["model"] = "gpt-6-astra" }},
		{"modelProvider", func(c map[string]any) { c["modelProvider"] = "azure" }},
		{"approvalPolicy", func(c map[string]any) { c["approvalPolicy"] = "never" }},
		{"sandbox", func(c map[string]any) {
			c["sandbox"] = map[string]any{"type": "workspace-write", "writable_roots": []string{"/somewhere/else"}, "network_access": false}
		}},
		{"sandboxNetwork", func(c map[string]any) {
			c["sandbox"] = map[string]any{"type": "workspace-write", "writable_roots": []string{c["cwd"].(string)}, "network_access": true}
		}},
		{"approvalsReviewer", func(c map[string]any) { c["approvalsReviewer"] = "auto_review" }},
		{"instructionSources", func(c map[string]any) { c["instructionSources"] = []string{"~/.codex/AGENTS.md", "/opt/extra.md"} }},
		{"cwd", func(c map[string]any) { c["cwd"] = "/not/the/workspace" }},
		{"threadID", func(c map[string]any) { c["id"] = "01900000-0000-7000-8000-0000000000ff" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAdapterHarness(t)
			scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
			scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, tc.drift))
			scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, true)...)
			writeScenario(t, h.scratch, scenario...)
			h.createAndPersist(t)

			out, err := h.dispatch(t, "t-drift", "prompt that must never be written")
			var drift *ErrProfileDrift
			if !errors.As(err, &drift) {
				t.Fatalf("expected ErrProfileDrift, got %T: %v (outcome %+v)", err, err, out)
			}
			if out.Status != adapter.DispatchRejected {
				t.Fatalf("drift is a pre-acceptance rejection, got %s", out.Status)
			}
			for _, r := range requestLog(t, h.scratch) {
				if r == "turn/start" {
					t.Fatal("the prompt was transmitted despite verified drift")
				}
			}
			if terminatedCount(t, h.scratch) < 1 {
				t.Fatal("drift must terminate the child")
			}
		})
	}
}

// Single-flight per native thread: a second dispatch while a turn is
// in flight is rejected as busy, never queued.
func TestCodexAdapter_SingleFlightPerNativeThread(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, false)...) // never completes
	writeScenario(t, h.scratch, scenario...)

	h.createAndPersist(t)
	if out, err := h.dispatch(t, "t-sf-1", "first"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("first dispatch: %+v err=%v", out, err)
	}

	out, err := h.dispatch(t, "t-sf-2", "second")
	if out.Status != adapter.DispatchRejected {
		t.Fatalf("second dispatch must be rejected while a turn is in flight, got %+v err=%v", out, err)
	}
	// The in-flight turn is uncertain in durable state until terminal, so
	// the durable block (§3.5) is the single-flight guarantee: the second
	// turn is rejected, never queued, and no attempt row is inserted.
	if !strings.Contains(out.Reason, "busy") && !strings.Contains(out.Reason, "unresolved attempt") {
		t.Fatalf("second dispatch must be rejected as busy/blocked, got %+v", out)
	}
	if a, _ := h.store.GetLatestCodexTurnAttempt(context.Background(), testSessionID, "t-sf-2"); a != nil {
		t.Fatalf("single-flight rejection must not insert an attempt row: %+v", a)
	}
}

// Crash boundary re-run through the adapter: a launch that was reserved
// but never started (crash between reservation and the write) leaves an
// uncertain attempt that durably blocks redispatch — the adapter refuses
// a new turn without starting any child.
func TestCodexAdapter_CrashBoundaryReservedNotStarted(t *testing.T) {
	h := newAdapterHarness(t)
	h.createAndPersist(t)

	// Simulate the crash: attempt + reservation durable, no launch state.
	ctx := context.Background()
	if err := h.store.InsertCodexTurnAttempt(ctx, storage.CodexTurnAttempt{
		AttemptID: "att-crash", SessionID: testSessionID, TurnKey: "t-crash",
		PromptDigest: "pdig-v1:sha256:fixed", RolloutProtection: "advisory",
	}); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	if _, err := h.store.ReserveCodexLaunch(ctx, "att-crash", "codex-app-server", 0); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}

	// The adapter must refuse the new turn from DURABLE state, with no
	// child started and no redispatch of the uncertain attempt.
	out, err := h.dispatch(t, "t-after-crash", "prompt")
	if err != nil || out.Status != adapter.DispatchRejected {
		t.Fatalf("reserved-not-started must reject: %+v err=%v", out, err)
	}
	if !strings.Contains(out.Reason, "unresolved attempt") {
		t.Fatalf("block reason: %q", out.Reason)
	}
	if args := readFixtureFile(t, h.scratch, ".codex-fixture-args"); len(args) != 1 {
		t.Fatalf("blocked dispatch must not start children, launches: %v", args)
	}
}

// §3.9: the eligibility gate fires at every launch. A dispatch that
// needs a replacement child (park, crash, restart — no live child)
// re-checks the attestation BEFORE any process starts.
func TestCodexAdapter_DispatchEligibilityGateOnChildReplacement(t *testing.T) {
	h := newAdapterHarness(t)
	h.createAndPersist(t)

	// Child-replacement situation: the running child is gone.
	h.server.stop(context.Background(), testSessionID)
	h.eligible.Store(false)

	out, err := h.dispatch(t, "t-elig", "prompt")
	var missing *ErrProductionEligibilityMissing
	if !errors.As(err, &missing) {
		t.Fatalf("expected ErrProductionEligibilityMissing, got %T: %v", err, err)
	}
	if out.Status != adapter.DispatchRejected {
		t.Fatalf("eligibility loss is a pre-acceptance rejection, got %s", out.Status)
	}
	// Exactly the creation launch remains: no replacement child started.
	if args := readFixtureFile(t, h.scratch, ".codex-fixture-args"); len(args) != 1 {
		t.Fatalf("no child may start without eligibility, launches: %v", args)
	}
}

// Resume of a missing native session: the deterministic verbatim error
// maps to the typed ErrNativeSessionMissing as a pre-acceptance
// rejection, and the child is terminated.
func TestCodexAdapter_DispatchNativeSessionMissing(t *testing.T) {
	h := newAdapterHarness(t)
	writeScenario(t, h.scratch,
		authOKLine(),
		`{"emit_on_request": {"method":"thread/start","line":{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}}}`,
		`{"respond": {"method":"thread/start","result":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}`,
		`{"respond_error": {"method":"thread/resume","code":-32600,"message":"no rollout found for thread id `+testThreadID+`"}}`)

	h.createAndPersist(t)
	out, err := h.dispatch(t, "t-missing", "prompt")
	var missing *ErrNativeSessionMissing
	if !errors.As(err, &missing) {
		t.Fatalf("expected ErrNativeSessionMissing, got %T: %v", err, err)
	}
	if out.Status != adapter.DispatchRejected {
		t.Fatalf("missing session is a pre-acceptance rejection, got %s", out.Status)
	}
	for _, r := range requestLog(t, h.scratch) {
		if r == "turn/start" {
			t.Fatal("no turn may start against a provably missing native session")
		}
	}
	if terminatedCount(t, h.scratch) < 1 {
		t.Fatal("the missing-session rejection must terminate the child")
	}
}

// An invalid prompt (NUL byte) is rejected before ANY durable or native
// effect: no attempt row, no turn/start, slot released.
func TestCodexAdapter_PromptNULRejectedPreWrite(t *testing.T) {
	h := newAdapterHarness(t)
	h.createAndPersist(t)
	out, err := h.dispatch(t, "t-nul", "bad\x00prompt")
	if err == nil || out.Status != adapter.DispatchRejected {
		t.Fatalf("NUL prompt must reject, got %+v err=%v", out, err)
	}
	if a, _ := h.store.GetLatestCodexTurnAttempt(context.Background(), testSessionID, "t-nul"); a != nil {
		t.Fatalf("no attempt row may exist for an invalid prompt: %+v", a)
	}
	for _, r := range requestLog(t, h.scratch) {
		if r == "turn/start" {
			t.Fatal("an invalid prompt must never be transmitted")
		}
	}
}

// ── Idle-grace park scheduling (controller ruling: default 30s,
// config-driven) ─────────────────────────────────────────────────────────

func TestCodexAdapter_IdleGraceParksChild(t *testing.T) {
	h := newAdapterHarness(t)
	h.server.SetIdleGrace(80 * time.Millisecond)
	h.create(t, testSessionID)

	// Busy holds the child: markBusy during the grace prevents parking.
	h.server.markBusy(adapter.SessionID(testSessionID))
	time.Sleep(250 * time.Millisecond)
	if terminatedCount(t, h.scratch) != 0 {
		t.Fatal("a busy child must not be parked")
	}

	h.server.markIdle(adapter.SessionID(testSessionID))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if terminatedCount(t, h.scratch) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle child was not parked within the grace")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !h.server.isParked(adapter.SessionID(testSessionID)) {
		t.Fatal("parked session must be recorded")
	}
}

// ── Task 6 surfaces: honesty for undispatched turns ─────────────────────
//
// (The live-turn behavior of Observe/Cancel/Collect/Reconcile is covered
// by rollout_test.go and reconcile_test.go.)

func TestCodexAdapter_PhasedSurfacesHonest(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, false)...)
	writeScenario(t, h.scratch, scenario...)

	h.createAndPersist(t)

	// Not-dispatched turn: Observe fails, Cancel is CancelUnknown,
	// Collect is unavailable, Reconcile stays Uncertain.
	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-phase"}
	if _, err := h.adapter.Observe(context.Background(), ref); err == nil {
		t.Fatal("observe must fail honestly for an undispatched turn")
	}
	if co, err := h.adapter.Cancel(context.Background(), ref); err != nil || co.Disposition != adapter.CancelUnknown {
		t.Fatalf("cancel must be CancelUnknown, got %+v err=%v", co, err)
	}
	if cr, err := h.adapter.Collect(context.Background(), ref); err == nil || cr.ResultStatus != adapter.ResultUnavailable {
		t.Fatalf("collect must be unavailable before dispatch, got %+v err=%v", cr, err)
	}
	if ro, err := h.adapter.Reconcile(context.Background(), adapter.RecoveryRef{
		TurnRef: ref, Generation: 1,
	}); err != nil || ro.Status != adapter.ReconciliationUncertain {
		t.Fatalf("reconcile must stay uncertain, got %+v err=%v", ro, err)
	}

	// Live turn: Observe hands out the bounded stream; the accepted turn
	// keeps the slot until terminal.
	if out, err := h.dispatch(t, "t-phase", "prompt"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	stream, err := h.adapter.Observe(context.Background(), ref)
	if err != nil || stream == nil {
		t.Fatalf("live observe must return the turn stream, got %v", err)
	}
	waitAttempt(t, h, "t-phase", func(a *storage.CodexTurnAttempt) bool { return a.NativeTurnID != nil })
}

// §6.1 scenario 1 — concurrent duplicate dispatch: N callers, ONE
// TurnRef, one shared verdict. Exactly one dispatch is accepted, the
// others are rejected (never queued), exactly one turn/start reaches the
// wire, and exactly one durable attempt row exists.
func TestCodexAdapter_ConcurrentDispatchScenarioOne(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, false)...)
	writeScenario(t, h.scratch, scenario...)
	h.createAndPersist(t)

	const n = 8
	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-sc1"}
	outcomes := make([]adapter.DispatchOutcome, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i], errs[i] = h.adapter.Dispatch(context.Background(), ref, "shared prompt")
		}(i)
	}
	wg.Wait()

	accepted := 0
	for i := 0; i < n; i++ {
		switch outcomes[i].Status {
		case adapter.DispatchAccepted:
			accepted++
		case adapter.DispatchRejected:
			// Expected for the losers; the reason differs by which gate
			// observed the duplicate (durable block, busy, or duplicate
			// attempt insert) — all are rejections, none queued.
		default:
			t.Fatalf("caller %d: unexpected verdict %+v err=%v", i, outcomes[i], errs[i])
		}
	}
	if accepted != 1 {
		t.Fatalf("exactly one caller may be accepted, got %d", accepted)
	}

	turns := 0
	for _, r := range requestLog(t, h.scratch) {
		if r == "turn/start" {
			turns++
		}
	}
	if turns != 1 {
		t.Fatalf("exactly one turn/start may reach the wire, got %d (%v)", turns, requestLog(t, h.scratch))
	}

	var rows int
	if err := h.store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM codex_turn_attempts WHERE session_id = ? AND turn_key = ?`,
		testSessionID, ref.TurnKey).Scan(&rows); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if rows != 1 {
		t.Fatalf("exactly one attempt row must exist, got %d", rows)
	}
}

// compile-time contract check: a drifted adapter contract fails loudly.
var _ adapter.Adapter = (*CodexAdapter)(nil)
