//go:build unix

package service

// Task 10 acceptance story (Codex-only, provider-free): the full
// controller lifecycle through the service bridge with the codextest
// fixture adapter — the ONLY pre-attestation construction path — wired
// through configured service construction (NewServerWithAdapter with the
// fixture-scoped adapter, CodexBinaryPath configured), a REAL executor
// (the fixture manager launches the compiled fixture `codex` child via
// execpolicy), and the fixture codex child. Flow: adopt (secret once) →
// connect → CreateCodexSession (gate ordering + §3.4 binding
// persistence) → queue → release → gated execution to the fixture child
// → collect → disconnect (child survives) → reconnect → follow-up on
// the exact thread → durable outcomes. Spec §6.1 scenarios 1–12 as
// focused tests; adapter-level coverage is referenced in the evidence
// matrix (docs/superpowers/specs/2026-09-23-ac-009-codex-adapter-design.md
// §4/§6) and not duplicated here.
//
// The codextest fixture never touches production: production construction
// fail-closed evidence (ErrProductionEligibilityMissing pre-child, the
// cprot-v2 journal operation, and the durable §3.4 restart block through
// production wiring) lives in review_ac009_wiring_test.go. The restart
// test below re-asserts the RESTART-BLOCK ruling through the BRIDGE: the
// first (uncertain) creation runs through the codextest path, the restart
// server is PRODUCTION construction, and the refusal + no-second-child
// assertions are made over the same durable store.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex/codextest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// The codextest fixture scenario fixes the native ids it binds (the
// values are constants of the codextest package; they are mirrored here
// because the package exports only the harness, not its scenario ids).
// The native thread id is also recovered dynamically from the binding
// returned by CreateCodexSession; the constants are used only in staged
// scenario lines that must name the thread before it exists.
const (
	codexAcceptanceRunID     = "run-codextest"   // codextest fixtureRunID
	codexAcceptanceBootstrap = "lease-codextest" // codextest fixture controller lease
	codexAcceptanceSession   = "sess-ac009-acc"
	codexFixtureThreadID     = "01900000-0000-7000-8000-000000000001" // codextest fixtureThreadID
	codexFixtureTurnID       = "01900000-0000-7000-8000-0000000000aa" // codextest fixtureTurnID
	codexCouncilDenialText   = "denied by council: contributor approvals are never granted; the turn continues"
)

// codexAcceptanceProfile reconstructs the frozen cprof-v3 profile the
// codextest fixture freezes internally (same fields, same staged
// event-universe bytes, same digest derivation) so the service's
// CreateCodexSession persists a binding whose profile_digest equals the
// adapter's frozen digest. The equality is asserted at setup: any drift
// between this reconstruction and codextest.buildFixtureProfile fails
// the test loudly instead of corrupting the binding.
func codexAcceptanceProfile(t *testing.T, scratch, wsRoot string) storage.CanonicalProfile {
	t.Helper()
	universe, err := json.Marshal(map[string]any{
		"codex_cli_version": "0.154.0",
		"methods":           []string{"initialize", "thread/start"},
	})
	if err != nil {
		t.Fatalf("marshal universe: %v", err)
	}
	profile := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v3",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"codex", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"codex": {
				Model:          "gpt-5.6-sol",
				NativeAuthMode: "inherited_codex_home",
				Codex: &storage.CodexHarnessSpec{
					AppServerVersion:  "0.154.0",
					ModelProvider:     "openai",
					ExpectedCodexHome: filepath.Join(scratch, ".codex"),
					Platform:          storage.CodexPlatformSpec{OS: runtime.GOOS, Family: "unix"},
					SandboxPolicy: storage.CodexSandboxPolicySpec{
						Type:          "workspace-write",
						WritableRoots: []string{wsRoot},
						NetworkAccess: false,
					},
					ApprovalPolicy:    storage.CodexApprovalPolicy{Kind: "string", String: "on-request"},
					ApprovalsReviewer: "user",
					// The fixture child answers mcpServerStatus/list
					// with {"servers":[]}: the frozen inventory is
					// affirmatively empty (must equal the observed).
					ExpectedMCPServers:         []string{},
					ExpectedInstructionSources: []string{"~/.codex/AGENTS.md"},
					RulesEvidence: storage.CodexRulesEvidenceSpec{
						Verified:     []string{"sandbox workspace-write"},
						Unverifiable: []string{"~/.codex/rules/*.rules contents"},
					},
					EventUniversePath:   "docs/superpowers/evidence/ac009-native-event-universe-0.154.0.json",
					EventUniverseDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(universe)),
				},
			},
		},
		ToolkitManifest: &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
			ProbedCLIVersion:       "2.1.278",
			UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
			UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
			ApprovedTools:          []string{"Read", "Glob"},
			DeniedComplement:       []string{"Bash"},
			ExpectedHooks:          []string{"SessionStart:startup"},
			TurnsBound:             8,
		}},
	}
	return profile
}

// stageCodexRollout stages the rollout file the native side would have
// materialized: under the frozen CODEX_HOME sessions root, named with
// the native UUID suffix, carrying a verified session_meta. extraLines
// ride between session_meta and the (optional torn) tail.
func stageCodexRollout(t *testing.T, scratch, threadID string, extraLines ...string) string {
	t.Helper()
	dir := filepath.Join(scratch, ".codex", "sessions", "2026", "09", "23")
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

// ── Fixture evidence helpers (the child's cwd is the fixture scratch) ───

func codexRequestLog(t *testing.T, scratch string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(scratch, ".codex-fixture-requests"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read request log: %v", err)
	}
	var out []string
	for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

func codexCountRequests(t *testing.T, scratch, method string) int {
	t.Helper()
	n := 0
	for _, m := range codexRequestLog(t, scratch) {
		if m == method {
			n++
		}
	}
	return n
}

func codexLaunchCount(t *testing.T, scratch string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(scratch, ".codex-fixture-args"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read args log: %v", err)
	}
	n := 0
	for _, ln := range strings.Split(string(raw), "\n") {
		if ln != "" {
			n++
		}
	}
	return n
}

func codexTerminatedCount(t *testing.T, scratch string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(scratch, ".codex-fixture-terminated"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read terminated log: %v", err)
	}
	n := 0
	for _, ln := range strings.Split(string(raw), "\n") {
		if ln != "" {
			n++
		}
	}
	return n
}

// waitCodexReply polls the verbatim reply log until a frame naming the
// given JSON-RPC id appears (the fixture logs every Council response).
func waitCodexReply(t *testing.T, scratch, id string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	path := filepath.Join(scratch, ".codex-fixture-replies")
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			var hits []string
			for _, ln := range strings.Split(string(raw), "\n") {
				if strings.Contains(ln, `"id":`+id+",") {
					hits = append(hits, ln)
				}
			}
			if len(hits) > 0 {
				return hits
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reply frame with id %s within %v", id, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitCodexAttempt(t *testing.T, acc *codexAcceptance, turnKey string, until func(*storage.CodexTurnAttempt) bool) *storage.CodexTurnAttempt {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		att, err := acc.store.GetLatestCodexTurnAttempt(context.Background(), codexAcceptanceSession, turnKey)
		if err == nil && att != nil && until(att) {
			return att
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempt %s never reached the awaited state (last: %+v err=%v)", turnKey, att, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ── Scenario line builders (mirror the codextest directive grammar) ─────

func codexAuthOKLine() string {
	return `{"respond": {"method":"account/read","result":{"account":{"accountId":"acc"},"requiresOpenaiAuth":false}}}`
}

func codexThreadStartRules() []string {
	notif := `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"` + codexFixtureThreadID + `","sessionId":"` + codexFixtureThreadID + `","status":{"type":"idle"}}}}`
	result := `{"id":"` + codexFixtureThreadID + `","sessionId":"` + codexFixtureThreadID + `","status":{"type":"idle"},"model":"gpt-5.6-sol"}`
	return []string{
		`{"emit_on_request": {"method":"thread/start","line":` + notif + `}}`,
		`{"respond": {"method":"thread/start","result":` + result + `}}`,
	}
}

// codexResumeRule builds a thread/resume response matching the frozen
// fixture policy, with per-field drift for §3.5 rejection evidence.
func codexResumeRule(wsRoot string, drift func(map[string]any)) string {
	cfg := map[string]any{
		"id":                 codexFixtureThreadID,
		"sessionId":          codexFixtureThreadID,
		"status":             map[string]any{"type": "idle"},
		"cwd":                wsRoot,
		"approvalPolicy":     "on-request",
		"sandbox":            map[string]any{"type": "workspace-write", "writable_roots": []string{wsRoot}, "network_access": false},
		"approvalsReviewer":  "user",
		"model":              "gpt-5.6-sol",
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

func codexTurnStartedLine() string {
	return `{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"` + codexFixtureThreadID + `","turnId":"` + codexFixtureTurnID + `"}}`
}

func codexTurnCompletedLine() string {
	return `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"` + codexFixtureThreadID + `","turn":{"id":"` + codexFixtureTurnID + `","items":[],"status":"completed","durationMs":100}}}`
}

func codexTurnStartRespondRule() string {
	return `{"respond": {"method":"turn/start","result":{"id":"` + codexFixtureTurnID + `","threadId":"` + codexFixtureThreadID + `","status":{"type":"inProgress"}}}}`
}

// codexLifecycleScenario stages the §3.4/§3.5 happy path for ONE child
// serving TWO turns: the emit rules are one-shot per child, so the
// turn/started + turn/completed pair is staged once per expected turn
// while the respond rules (repeatable) are staged once.
func codexLifecycleScenario(wsRoot string, turns int) []string {
	lines := append([]string{codexAuthOKLine()}, codexThreadStartRules()...)
	lines = append(lines, codexResumeRule(wsRoot, nil), codexTurnStartRespondRule())
	for i := 0; i < turns; i++ {
		lines = append(lines,
			`{"emit_on_request": {"method":"turn/start","line":`+codexTurnStartedLine()+`}}`,
			`{"emit_after_response": {"method":"turn/start","line":`+codexTurnCompletedLine()+`}}`)
	}
	return lines
}

// ── Harness ─────────────────────────────────────────────────────────────

type codexAcceptance struct {
	t       *testing.T
	fx      *codextest.Fixture
	srv     *Server
	store   *storage.Store // the fixture store: adapter and service share it
	profile storage.CanonicalProfile
	token   string
	baseDir string       // parent of the service state dir (restart-test layout)
	lock    *ServiceLock // held for the first instance; the restart releases it
	bridge  *acceptanceBridge
}

// newCodexAcceptance builds the fixture harness and the configured
// service construction over it. scenario == nil stages the codextest
// default happy path.
func newCodexAcceptance(t *testing.T, scenario []string) *codexAcceptance {
	t.Helper()
	fx, err := codextest.NewFixtureAdapter(codextest.FixtureOptions{Scenario: scenario})
	if err != nil {
		t.Fatalf("fixture harness: %v", err)
	}
	t.Cleanup(func() { _ = fx.Close() }) // registered first: runs after the server cleanup

	profile := codexAcceptanceProfile(t, fx.ScratchDir, fx.WorkspaceRoot)
	digest, _, err := storage.ComputeProfileDigest(profile)
	if err != nil {
		t.Fatalf("profile digest: %v", err)
	}
	if digest != fx.ProfileDigest {
		t.Fatalf("service profile digest %q must equal the fixture's frozen digest %q; "+
			"codexAcceptanceProfile drifted from codextest's frozen profile", digest, fx.ProfileDigest)
	}

	// Park is config-driven; a long grace keeps the child-survival
	// assertions deterministic (the lifecycle finishes well inside it).
	fx.Server.SetIdleGrace(5 * time.Minute)

	dir := testStateDir(t)
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	for _, d := range []string{stateDir, wsBase} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	lock, err := AcquireServiceLock(stateDir)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release() })

	instanceID := fmt.Sprintf("inst-ac009-acc-%d", time.Now().UnixNano())
	token := "tok-ac009-acc-" + instanceID
	cfg := ServerConfig{
		StateDir:         stateDir,
		InstanceID:       instanceID,
		AuthToken:        token,
		WorkspaceBaseDir: wsBase,
		CodexBinaryPath:  "codex", // resolved on PATH to the compiled fixture
		CodexProfile:     profile,
	}
	srv, err := NewServerWithAdapter(fx.Store, lock, cfg, fx.Adapter)
	if err != nil {
		t.Fatalf("configured service construction with the fixture adapter: %v", err)
	}
	started := false
	t.Cleanup(func() {
		if started {
			_ = srv.Close()
		}
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	started = true

	if err := fx.SeedSession(context.Background(), codexAcceptanceSession); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return &codexAcceptance{
		t:       t,
		fx:      fx,
		srv:     srv,
		store:   fx.Store,
		profile: profile,
		token:   token,
		baseDir: dir,
		lock:    lock,
		bridge:  &acceptanceBridge{t: t, client: newTestClient(srv.SocketPath()), token: token},
	}
}

func (acc *codexAcceptance) createRequest() adapter.CreateSessionRequest {
	return adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID(codexAcceptanceSession),
		Contributor: "codex",
		Config: adapter.SessionConfig{
			WorkspaceRoot: acc.fx.WorkspaceRoot,
			Model:         acc.fx.Model,
		},
	}
}

// restage replaces the staged scenario before any child starts (the
// fixture launches children lazily at CreateSession, so restaging after
// construction but before birth is what takes effect).
func (acc *codexAcceptance) restage(lines ...string) {
	acc.t.Helper()
	if err := acc.fx.StageScenario(lines...); err != nil {
		acc.t.Fatalf("restage scenario: %v", err)
	}
}

// adoptAndConnect performs adopt-once + bridge connect; the lease secret
// is returned exactly once.
func (acc *codexAcceptance) adoptAndConnect() string {
	acc.t.Helper()
	code, resp := acc.bridge.do("POST", "/v1/runs/"+codexAcceptanceRunID+"/controller/adopt", fmt.Sprintf(
		`{"op_id": "op-adopt-acc-cx", "harness": "codex", "controller_ref": "conv-acc-cx",
		  "bootstrap_lease": %q}`, codexAcceptanceBootstrap))
	if code != http.StatusOK && code != http.StatusCreated {
		acc.t.Fatalf("adopt: %d %v", code, resp)
	}
	lease, _ := resp["lease_secret"].(string)
	if lease == "" {
		acc.t.Fatalf("adopt must return the lease secret once, got %v", resp)
	}
	code, resp = acc.bridge.do("POST", "/v1/runs/"+codexAcceptanceRunID+"/controller/connect", fmt.Sprintf(
		`{"op_id": "op-conn-acc-cx-1", "controller_lease": %q, "expected_generation": 1, "instance_id": %q}`,
		lease, acc.srv.InstanceID()))
	if code != http.StatusOK && code != http.StatusCreated {
		acc.t.Fatalf("connect: %d %v", code, resp)
	}
	return lease
}

func (acc *codexAcceptance) sessionVersion() int64 {
	acc.t.Helper()
	hydrated, err := acc.store.HydrateState(context.Background())
	if err != nil {
		acc.t.Fatalf("hydrate: %v", err)
	}
	sess, ok := hydrated.Sessions[codexAcceptanceSession]
	if !ok {
		acc.t.Fatalf("session %s missing from durable state", codexAcceptanceSession)
	}
	return sess.RowVersion
}

func (acc *codexAcceptance) queue(lease, opID, turnKey, prompt string) int64 {
	acc.t.Helper()
	ver := acc.sessionVersion()
	code, resp := acc.bridge.do("POST", "/v1/runs/"+codexAcceptanceRunID+"/sessions/"+codexAcceptanceSession+"/prompts/queue",
		fmt.Sprintf(`{"instance_id": %q, "op_id": %q, "controller_lease": %q, "expected_version": %d,
		  "turn_key": %q, "prompt": %q}`, acc.srv.InstanceID(), opID, lease, ver, turnKey, prompt))
	if code != http.StatusOK && code != http.StatusCreated {
		acc.t.Fatalf("queue %s: %d %v", turnKey, code, resp)
	}
	receipt, ok := resp["receipt"].(map[string]any)
	if !ok {
		acc.t.Fatalf("queue %s must return a receipt, got %v", turnKey, resp)
	}
	committed, _ := receipt["committed_version"].(float64)
	return int64(committed)
}

func (acc *codexAcceptance) release(lease, opID, turnKey string, version int64) int {
	acc.t.Helper()
	code, _ := acc.bridge.do("POST", "/v1/runs/"+codexAcceptanceRunID+"/sessions/"+codexAcceptanceSession+"/turns/"+turnKey+"/release",
		fmt.Sprintf(`{"op_id": %q, "controller_lease": %q, "expected_version": %d}`, opID, lease, version))
	return code
}

// releaseOK releases and requires acceptance.
func (acc *codexAcceptance) releaseOK(lease, opID, turnKey string, version int64) {
	acc.t.Helper()
	if code := acc.release(lease, opID, turnKey, version); code != http.StatusAccepted {
		acc.t.Fatalf("release %s: %d", turnKey, code)
	}
}

func (acc *codexAcceptance) collect(turnKey string) (int, map[string]any) {
	acc.t.Helper()
	return acc.bridge.do("GET", "/v1/runs/"+codexAcceptanceRunID+"/sessions/"+codexAcceptanceSession+"/turns/"+turnKey, "")
}

// requireCompleted collects the turn and asserts the bounded completed
// terminal with the correlated native turn payload.
func (acc *codexAcceptance) requireCompleted(turnKey string) map[string]any {
	acc.t.Helper()
	code, resp := acc.collect(turnKey)
	if code != http.StatusOK {
		acc.t.Fatalf("collect %s: %d %v", turnKey, code, resp)
	}
	if resp["status"] != string(council.TurnCompleted) {
		acc.t.Fatalf("turn %s must be completed, got %v (%v)", turnKey, resp["status"], resp)
	}
	result, _ := resp["result"].(string)
	if !strings.Contains(result, codexFixtureTurnID) || !strings.Contains(result, "completed") {
		acc.t.Fatalf("turn %s result must carry the native turn payload, got %q", turnKey, result)
	}
	return resp
}

// ── The bridge lifecycle ────────────────────────────────────────────────

// TestAcceptance_Codex_BridgeLifecycle drives the whole controller
// story through the service bridge: adopt -> connect -> authority
// pre-flight -> CreateCodexSession (native UUIDv7 binding persisted) ->
// queue -> release -> gated execution to the fixture child via the real
// executor -> collect -> disconnect (child survives) -> reconnect ->
// follow-up turn on the exact native thread with the SAME child ->
// durable outcomes. §6.1 scenario 4 (controller disconnect never
// touches the child) is asserted in steps 7–9.
func TestAcceptance_Codex_BridgeLifecycle(t *testing.T) {
	acc := newCodexAcceptance(t, nil)
	// One child serves both turns: stage one one-shot notification pair
	// per expected turn (see codexLifecycleScenario).
	acc.restage(codexLifecycleScenario(acc.fx.WorkspaceRoot, 2)...)
	ctx := context.Background()
	bridge := acc.bridge

	// 1+2. Adopt (secret once) and connect.
	lease := acc.adoptAndConnect()

	// 3. Authority pre-flight: a wrong credential is refused before any
	// native identity is minted and no binding appears.
	if _, _, err := acc.srv.CreateCodexSession(ctx, "op-bind-acc-cx-bad", "not-the-lease", acc.createRequest()); err == nil {
		t.Fatal("session birth without the controller credential must be refused")
	}
	if b, _ := acc.store.GetCodexSessionBinding(ctx, codexAcceptanceSession); b != nil {
		t.Fatal("no binding may exist after an authority refusal")
	}

	// 4. Birth through the bridge: CreateCodexSession persists the §3.4
	// binding under the controller credential; the native id is a
	// server-assigned canonical UUIDv7, deliberately distinct.
	binding, _, err := acc.srv.CreateCodexSession(ctx, "op-bind-acc-cx-1", lease, acc.createRequest())
	if err != nil {
		t.Fatalf("native session creation through the service path: %v", err)
	}
	nativeID := binding.NativeSessionID
	if nativeID == "" || nativeID == codexAcceptanceSession {
		t.Fatalf("native id must be server-assigned and distinct, got %q", nativeID)
	}
	stored, err := acc.store.GetCodexSessionBinding(ctx, codexAcceptanceSession)
	if err != nil || stored == nil {
		t.Fatalf("binding must be persisted by the service path: %v", err)
	}
	if stored.NativeID != string(nativeID) {
		t.Fatalf("persisted binding must carry the native id: %+v", stored)
	}
	if stored.ProfileDigest != acc.fx.ProfileDigest {
		t.Fatalf("binding profile digest %q must be the frozen digest %q", stored.ProfileDigest, acc.fx.ProfileDigest)
	}
	// Gate ordering evidence: the child handshake + auth gate ran (the
	// fixture request log records the §3.3 order) and no turn exists.
	want := []string{"initialize", "account/read", "thread/start"}
	if got := codexRequestLog(t, acc.fx.ScratchDir); len(got) != len(want) {
		t.Fatalf("creation request order must be %v, got %v", want, got)
	}

	// First acceptance materializes the binding from the rollout
	// baseline; stage the rollout the native side would have written.
	stageCodexRollout(t, acc.fx.ScratchDir, string(nativeID))

	// 5. Queue + release: the supervisor worker executes through the
	// wired adapter to the fixture child.
	ver := acc.queue(lease, "op-q-acc-cx-1", "t-acc-1", "acceptance prompt")
	acc.releaseOK(lease, "op-rel-acc-cx-1", "t-acc-1", ver)
	acc.srv.Coordinator().WaitWorkers()

	// 6. Collect: bounded completed terminal with the correlated native
	// turn payload.
	acc.requireCompleted("t-acc-1")

	// The dispatch addressed the EXACT native thread (resume
	// verification preceded turn/start, §3.5 step 1, first turn
	// included) and no second child was involved.
	if codexCountRequests(t, acc.fx.ScratchDir, "turn/start") != 1 {
		t.Fatalf("exactly one turn/start may have been transmitted, log: %v",
			codexRequestLog(t, acc.fx.ScratchDir))
	}
	mat, err := acc.store.GetCodexSessionBinding(ctx, codexAcceptanceSession)
	if err != nil || mat == nil || !mat.Materialized || mat.RolloutPath == nil {
		t.Fatalf("first acceptance must materialize the binding: %+v err=%v", mat, err)
	}
	att := waitCodexAttempt(t, acc, "t-acc-1", func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "completed" || att.NativeTurnID == nil || *att.NativeTurnID != codexFixtureTurnID {
		t.Fatalf("durable attempt must be terminal completed on the native turn id, got %+v", att)
	}

	// 7. Bridge disconnect: the child and the durable state survive —
	// a disconnected interactive controller cannot touch them.
	code, resp := bridge.do("POST", "/v1/runs/"+codexAcceptanceRunID+"/controller/disconnect", fmt.Sprintf(
		`{"op_id": "op-disc-acc-cx", "controller_lease": %q, "expected_generation": 1}`, lease))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("disconnect: %d %v", code, resp)
	}
	if launches := codexLaunchCount(t, acc.fx.ScratchDir); launches != 1 {
		t.Fatalf("the child must survive the disconnect (no relaunch), launches: %d", launches)
	}
	if codexTerminatedCount(t, acc.fx.ScratchDir) != 0 {
		t.Fatal("disconnect must never terminate the child")
	}

	// 8. Bridge reconnect.
	code, resp = bridge.do("POST", "/v1/runs/"+codexAcceptanceRunID+"/controller/connect", fmt.Sprintf(
		`{"op_id": "op-conn-acc-cx-2", "controller_lease": %q, "expected_generation": 1, "instance_id": %q}`,
		lease, acc.srv.InstanceID()))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("reconnect: %d %v", code, resp)
	}

	// 9. Follow-up turn on the EXACT thread: the same child serves it
	// (resume verification runs again provider-free; no new
	// initialize — no second child was ever launched).
	ver2 := acc.queue(lease, "op-q-acc-cx-2", "t-acc-2", "follow-up prompt")
	acc.releaseOK(lease, "op-rel-acc-cx-2", "t-acc-2", ver2)
	acc.srv.Coordinator().WaitWorkers()
	acc.requireCompleted("t-acc-2")

	if launches := codexLaunchCount(t, acc.fx.ScratchDir); launches != 1 {
		t.Fatalf("the follow-up must reuse the surviving child, launches: %d", launches)
	}
	if n := codexCountRequests(t, acc.fx.ScratchDir, "initialize"); n != 1 {
		t.Fatalf("exactly one child handshake may exist, got %d", n)
	}
	if n := codexCountRequests(t, acc.fx.ScratchDir, "thread/resume"); n != 2 {
		t.Fatalf("both dispatches must resume-verify the exact thread, got %d resumes", n)
	}
	if n := codexCountRequests(t, acc.fx.ScratchDir, "turn/start"); n != 2 {
		t.Fatalf("both turns must be transmitted once, got %d turn/starts", n)
	}

	// 10. Durable evidence: both turns persisted completed with native
	// results; both attempts terminal.
	hydrated, err := acc.store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	turns := hydrated.Sessions[codexAcceptanceSession].Turns
	for _, key := range []string{"t-acc-1", "t-acc-2"} {
		turn, ok := turns[key]
		if !ok {
			t.Fatalf("turn %s missing from durable state", key)
		}
		if turn.Status != string(council.TurnCompleted) {
			t.Fatalf("turn %s must be durably completed, got %v", key, turn.Status)
		}
		if !strings.Contains(turn.Result, codexFixtureTurnID) {
			t.Fatalf("durable result of %s must carry the native payload, got %q", key, turn.Result)
		}
	}
	waitCodexAttempt(t, acc, "t-acc-2", func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
}

// §6.1 scenario 1: N concurrent callers releasing one queued turn
// produce exactly one accepted execution and one native turn; the
// losers are rejected on durable turn state and share the verdict.
func TestAcceptance_Codex_ConcurrentDuplicateDispatch(t *testing.T) {
	acc := newCodexAcceptance(t, nil)
	lease := acc.adoptAndConnect()

	if _, _, err := acc.srv.CreateCodexSession(context.Background(), "op-bind-acc-cx-dup", lease, acc.createRequest()); err != nil {
		t.Fatalf("create: %v", err)
	}
	stageCodexRollout(t, acc.fx.ScratchDir, codexFixtureThreadID)

	ver := acc.queue(lease, "op-q-acc-cx-dup", "t-dup", "duplicate prompt")

	const n = 4
	accepted := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			if code := acc.release(lease, fmt.Sprintf("op-rel-acc-cx-dup-%d", slot), "t-dup", ver); code == http.StatusAccepted {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("exactly one release must be accepted, got %d", accepted)
	}

	acc.srv.Coordinator().WaitWorkers()
	acc.requireCompleted("t-dup")
	if got := codexCountRequests(t, acc.fx.ScratchDir, "turn/start"); got != 1 {
		t.Fatalf("one shared native turn must exist, got %d turn/starts", got)
	}
}

// §6.1 scenario 2 (bridge tail): a turn/start written but never
// acknowledged is DispatchUnknown — the attempt stays Uncertain with no
// fabricated terminal, and the unresolved attempt durably blocks the
// next turn through the bridge (the prompt of the blocked turn is never
// transmitted). The crash-boundary durability and the one-redispatch
// rule are adapter-level evidence (TestCodexAdapter_DispatchUnknownLostResponse,
// TestCodexAdapter_Reconcile_*).
func TestAcceptance_Codex_LostTurnBlocksNextThroughBridge(t *testing.T) {
	acc := newCodexAcceptance(t, nil)
	acc.restage(
		codexAuthOKLine(),
		codexThreadStartRules()[0],
		codexThreadStartRules()[1],
		codexResumeRule(acc.fx.WorkspaceRoot, nil),
		// The ack never arrives inside the adapter's dispatch window:
		// the response is delayed past dispatchAckTimeout (15s).
		`{"respond": {"method":"turn/start","delay_ms":16000,"result":{"id":"`+codexFixtureTurnID+`","threadId":"`+codexFixtureThreadID+`","status":{"type":"inProgress"}}}}`,
	)
	lease := acc.adoptAndConnect()
	if _, _, err := acc.srv.CreateCodexSession(context.Background(), "op-bind-acc-cx-lost", lease, acc.createRequest()); err != nil {
		t.Fatalf("create: %v", err)
	}
	stageCodexRollout(t, acc.fx.ScratchDir, codexFixtureThreadID)

	ver := acc.queue(lease, "op-q-acc-cx-lost", "t-lost", "prompt that will lose its ack")
	acc.releaseOK(lease, "op-rel-acc-cx-lost", "t-lost", ver)
	acc.srv.Coordinator().WaitWorkers() // returns only after the DispatchUnknown classification

	// No fabricated terminal: the lost turn is not completed or failed.
	code, resp := acc.collect("t-lost")
	if code != http.StatusOK {
		t.Fatalf("collect lost turn: %d %v", code, resp)
	}
	switch resp["status"] {
	case string(council.TurnCompleted), string(council.TurnFailed):
		t.Fatalf("the lost turn must stay unconfirmed, got %v", resp["status"])
	}

	// The next turn cannot even be released: the lost turn is still the
	// session's ACTIVE turn at the service layer (no fabricated
	// terminal), so the release is refused — and no second prompt is
	// transmitted (one turn/start in the log: the lost one).
	ver2 := acc.queue(lease, "op-q-acc-cx-lost-2", "t-after", "prompt that must never transmit")
	releaseCode := acc.release(lease, "op-rel-acc-cx-lost-2", "t-after", ver2)
	if releaseCode == http.StatusAccepted {
		t.Fatal("the release of a second turn while the lost turn stays active must be refused")
	}
	if got := codexCountRequests(t, acc.fx.ScratchDir, "turn/start"); got != 1 {
		t.Fatalf("the blocked prompt must never be transmitted, turn/starts: %d", got)
	}
	// The uncertain attempt is durable state: the ADAPTER's §3.5 block
	// is what a disposition (or the one verified-absence redispatch)
	// must clear; adapter-level evidence:
	// TestCodexAdapter_DispatchUnknownLostResponse,
	// TestCodexAdapter_Reconcile_*.
	att, err := acc.store.GetLatestCodexTurnAttempt(context.Background(), codexAcceptanceSession, "t-lost")
	if err != nil || att == nil {
		t.Fatalf("the lost attempt must be durable: %v", err)
	}
	if att.Terminal {
		t.Fatalf("the lost attempt must stay unconfirmed, got %+v", att)
	}
}

// §6.1 scenarios 7+8: effective-config drift (model) and toolkit drift
// (instruction sources) fail closed as typed ErrProfileDrift BEFORE any
// prompt is transmitted; the drifted child is terminated. The full
// per-field matrix is adapter-level evidence
// (TestCodexAdapter_DriftMatrix); the bridge proves the pre-transmission
// guarantee end to end through queue/release.
func TestAcceptance_Codex_ProfileDriftFailsClosedBeforeTransmission(t *testing.T) {
	cases := []struct {
		name  string
		drift func(map[string]any)
		field string
	}{
		{"config drift between turns (model)", func(c map[string]any) { c["model"] = "gpt-6-astra" }, "model"},
		{"toolkit drift (instruction sources)", func(c map[string]any) {
			c["instructionSources"] = []string{"~/.codex/AGENTS.md", "/opt/extra.md"}
		}, "instructionSources"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newCodexAcceptance(t, nil)
			acc.restage(
				codexAuthOKLine(),
				codexThreadStartRules()[0],
				codexThreadStartRules()[1],
				codexResumeRule(acc.fx.WorkspaceRoot, tc.drift),
			)
			lease := acc.adoptAndConnect()
			if _, _, err := acc.srv.CreateCodexSession(context.Background(), "op-bind-acc-cx-drift", lease, acc.createRequest()); err != nil {
				t.Fatalf("create: %v", err)
			}
			stageCodexRollout(t, acc.fx.ScratchDir, codexFixtureThreadID)

			ver := acc.queue(lease, "op-q-acc-cx-drift", "t-drift", "prompt that must never be written")
			acc.releaseOK(lease, "op-rel-acc-cx-drift", "t-drift", ver)
			acc.srv.Coordinator().WaitWorkers()

			code, resp := acc.collect("t-drift")
			if code != http.StatusOK || resp["status"] != string(council.TurnFailed) {
				t.Fatalf("drift must fail the turn closed, got %d %v", code, resp)
			}
			result, _ := resp["result"].(string)
			if !strings.Contains(result, "codex profile drift on "+tc.field) {
				t.Fatalf("the failure must carry the typed drift reason on %s, got %q", tc.field, result)
			}
			if got := codexCountRequests(t, acc.fx.ScratchDir, "turn/start"); got != 0 {
				t.Fatalf("the prompt was transmitted despite verified drift, turn/starts: %d", got)
			}
			if codexTerminatedCount(t, acc.fx.ScratchDir) < 1 {
				t.Fatal("drift must terminate the child")
			}
		})
	}
}

// §6.1 scenario 5 (bridge half): the verbatim deterministic missing-
// thread error on thread/resume raises the typed
// ErrNativeSessionMissing through the bridge and nothing is transmitted.
// Id-drift poisoning and non-UUID refusal are adapter-level evidence
// (TestCodexAdapter_CreateSessionDriftedIdFailsClosed,
// TestCodexAdapter_DispatchNativeSessionMissing).
func TestAcceptance_Codex_MissingNativeThreadRejected(t *testing.T) {
	acc := newCodexAcceptance(t, nil)
	acc.restage(
		codexAuthOKLine(),
		codexThreadStartRules()[0],
		codexThreadStartRules()[1],
		`{"respond_error": {"method":"thread/resume","code":-32600,"message":"no rollout found for thread id `+codexFixtureThreadID+`"}}`,
	)
	lease := acc.adoptAndConnect()
	if _, _, err := acc.srv.CreateCodexSession(context.Background(), "op-bind-acc-cx-miss", lease, acc.createRequest()); err != nil {
		t.Fatalf("create: %v", err)
	}

	ver := acc.queue(lease, "op-q-acc-cx-miss", "t-missing", "prompt for a thread that is gone")
	acc.releaseOK(lease, "op-rel-acc-cx-miss", "t-missing", ver)
	acc.srv.Coordinator().WaitWorkers()

	code, resp := acc.collect("t-missing")
	if code != http.StatusOK || resp["status"] != string(council.TurnFailed) {
		t.Fatalf("the missing thread must fail the turn, got %d %v", code, resp)
	}
	if result, _ := resp["result"].(string); !strings.Contains(result, "no rollout found for thread id") {
		t.Fatalf("the verbatim deterministic absence evidence must surface, got %q", result)
	}
	if got := codexCountRequests(t, acc.fx.ScratchDir, "turn/start"); got != 0 {
		t.Fatalf("nothing may be transmitted against a missing thread, turn/starts: %d", got)
	}
}

// §6.1 scenario 6: a server approval request under the frozen on-request
// policy is denied with the exact §3.6 payload, the denial never
// retires the turn, and the turn reaches a bounded completed terminal —
// no hang. The tool_requested/tool_denied mirroring with ApprovalID
// correlation is adapter-level evidence
// (TestCodexAdapter_ExecApprovalDenialMirrorsAndTurnContinues); the
// bridge asserts the wire payload and the bounded terminal.
func TestAcceptance_Codex_ApprovalDenyMirroredBoundedTerminal(t *testing.T) {
	approval := `{"jsonrpc":"2.0","id":911,"method":"execCommandApproval","params":{"callId":"call-acc-1","conversationId":"` + codexFixtureThreadID + `"}}`
	acc := newCodexAcceptance(t, nil)
	acc.restage(
		codexAuthOKLine(),
		codexThreadStartRules()[0],
		codexThreadStartRules()[1],
		codexResumeRule(acc.fx.WorkspaceRoot, nil),
		`{"emit_many_on_request": {"method":"turn/start","lines":[`+codexTurnStartedLine()+`,`+approval+`]}}`,
		`{"respond": {"method":"turn/start","result":{"id":"`+codexFixtureTurnID+`","threadId":"`+codexFixtureThreadID+`","status":{"type":"inProgress"}}}}`,
		`{"emit_after_response": {"method":"turn/start","line":`+codexTurnCompletedLine()+`}}`,
	)
	lease := acc.adoptAndConnect()
	if _, _, err := acc.srv.CreateCodexSession(context.Background(), "op-bind-acc-cx-appr", lease, acc.createRequest()); err != nil {
		t.Fatalf("create: %v", err)
	}
	stageCodexRollout(t, acc.fx.ScratchDir, codexFixtureThreadID)

	ver := acc.queue(lease, "op-q-acc-cx-appr", "t-approval", "use the approval-required tool")
	start := time.Now()
	acc.releaseOK(lease, "op-rel-acc-cx-appr", "t-approval", ver)
	acc.srv.Coordinator().WaitWorkers()

	// The exact §3.6 deny payload went over the wire (verbatim reply
	// log), and the turn still reached its own bounded terminal.
	want := `{"jsonrpc":"2.0","id":911,"result":{"decision":{"denied":{"rejection":"` + codexCouncilDenialText + `"}}}}`
	replies := waitCodexReply(t, acc.fx.ScratchDir, "911", 10*time.Second)
	found := false
	for _, ln := range replies {
		if ln == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("the deny payload must match the §3.6 table exactly\nwant %s\ngot  %v", want, replies)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("the denial must never hang the turn, took %v", elapsed)
	}
	acc.requireCompleted("t-approval")
	att := waitCodexAttempt(t, acc, "t-approval", func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "completed" {
		t.Fatalf("a denial must not retire the turn: the verified terminal stands, got %+v", att)
	}
}

// §6.1 scenario 9: a rollout with a torn final line still materializes
// the binding at first acceptance; the attempt stays in the advisory
// evidence class (no protected terminal claim beyond the class).
func TestAcceptance_Codex_TruncatedRolloutStaysAdvisory(t *testing.T) {
	acc := newCodexAcceptance(t, nil)
	lease := acc.adoptAndConnect()
	if _, _, err := acc.srv.CreateCodexSession(context.Background(), "op-bind-acc-cx-torn", lease, acc.createRequest()); err != nil {
		t.Fatalf("create: %v", err)
	}
	stageCodexRollout(t, acc.fx.ScratchDir, codexFixtureThreadID,
		`{"timestamp":"2026-09-23T00:00:02Z","type":"turn_`)

	ver := acc.queue(lease, "op-q-acc-cx-torn", "t-torn", "prompt under a torn rollout")
	acc.releaseOK(lease, "op-rel-acc-cx-torn", "t-torn", ver)
	acc.srv.Coordinator().WaitWorkers()

	acc.requireCompleted("t-torn")
	mat, err := acc.store.GetCodexSessionBinding(context.Background(), codexAcceptanceSession)
	if err != nil || mat == nil || !mat.Materialized || mat.RolloutPath == nil {
		t.Fatalf("the torn tail must not block materialization: %+v err=%v", mat, err)
	}
	att := waitCodexAttempt(t, acc, "t-torn", func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
	if att.RolloutProtection != "advisory" {
		t.Fatalf("without an attestation the attempt stays advisory, got %q", att.RolloutProtection)
	}
}

// §6.1 scenarios 2 (restart block) + 10 (shared creation failure): N
// concurrent bridge creations of a poisoned thread/start share the
// creator's typed failure, exactly one child starts, one durable
// uncertainty is recorded — and a SERVICE RESTART (a new server
// instance over the same durable store, PRODUCTION construction, fresh
// in-memory state) refuses to re-create the thread with no second
// child, until the explicit resolution seam clears the block. This is
// the RESTART-BLOCK ruling asserted through the bridge; the production
// wiring form is Task 9 evidence
// (TestServiceCodexSession_CreationUncertainBlocksAcrossRestart).
func TestAcceptance_Codex_CreationUncertainBlocksAcrossRestart(t *testing.T) {
	acc := newCodexAcceptance(t, codextest.PoisonThreadStartScenario())
	ctx := context.Background()
	lease := acc.adoptAndConnect()

	// Scenario 10: concurrent duplicate creations share one outcome.
	const n = 3
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			_, _, errs[slot] = acc.srv.CreateCodexSession(ctx,
				fmt.Sprintf("op-bind-acc-cx-share-%d", slot), lease, acc.createRequest())
		}(i)
	}
	wg.Wait()
	for slot, err := range errs {
		var unc *adapter.ErrSessionCreationUncertain
		if err == nil || !errors.As(err, &unc) {
			t.Fatalf("caller %d must share the creator's typed failure, got %v", slot, err)
		}
	}
	if launches := codexLaunchCount(t, acc.fx.ScratchDir); launches != 1 {
		t.Fatalf("the shared creation must start exactly one child, got %d", launches)
	}
	blocked, err := acc.store.HasCodexCreationUncertainty(ctx, codexAcceptanceSession)
	if err != nil || !blocked {
		t.Fatalf("the uncertainty must be recorded durably, blocked=%v err=%v", blocked, err)
	}
	var journalCount int
	if err := acc.store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM journal_entries WHERE op_id = ?`,
		"op-codex-creation-uncertain-"+codexAcceptanceSession).Scan(&journalCount); err != nil || journalCount != 1 {
		t.Fatalf("the durable record must exist exactly once, got %d err=%v", journalCount, err)
	}

	// Restart: same durable state directory, fresh server instance
	// (PRODUCTION construction — a real restart has no in-memory
	// tombstone), fresh lock and a fresh store handle over the same
	// durable rows. The durable record must refuse the re-creation
	// before any child can start.
	if err := acc.srv.Close(); err != nil { // closes the shared fixture store handle (teardown)
		t.Fatalf("close first instance: %v", err)
	}
	if err := acc.lock.Release(); err != nil { // idempotent with the harness cleanup
		t.Fatalf("release first lock: %v", err)
	}
	dir := acc.baseDir
	stateDir := filepath.Join(dir, "state")
	scratch2 := filepath.Join(dir, "codex-scratch-restart")
	if err := os.MkdirAll(scratch2, 0o700); err != nil {
		t.Fatalf("mkdir restart scratch: %v", err)
	}
	lock2, err := AcquireServiceLock(stateDir)
	if err != nil {
		t.Fatalf("second lock: %v", err)
	}
	t.Cleanup(func() { _ = lock2.Release() })
	store2, err := storage.Open(storage.StoreOptions{StateDir: filepath.Join(acc.fx.ScratchDir, "state")})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	cfg2 := ServerConfig{
		StateDir:          stateDir,
		InstanceID:        acc.srv.InstanceID() + "-restart",
		AuthToken:         acc.token,
		WorkspaceBaseDir:  filepath.Join(dir, "workspaces"),
		CodexBinaryPath:   "codex",
		CodexProfile:      acc.profile,
		CodexEvidenceRoot: filepath.Join(acc.fx.ScratchDir, "evidence"),
		CodexScratchRoot:  scratch2,
	}
	srv2, err := NewServerWithAdapter(store2, lock2, cfg2, nil)
	if err != nil {
		t.Fatalf("restart construction (production): %v", err)
	}
	t.Cleanup(func() { _ = srv2.Close() })
	if err := srv2.Start(); err != nil {
		t.Fatalf("restart start: %v", err)
	}

	if _, _, err := srv2.CreateCodexSession(ctx, "op-bind-acc-cx-restart", lease, acc.createRequest()); err == nil {
		t.Fatal("a restart must not re-create the thread for an uncertain session")
	} else {
		var unc *adapter.ErrSessionCreationUncertain
		if !errors.As(err, &unc) || !strings.Contains(err.Error(), "durable creation uncertainty") {
			t.Fatalf("the restart refusal must be the durable §3.4 block, got %T: %v", err, err)
		}
	}
	if launches := codexLaunchCount(t, acc.fx.ScratchDir); launches != 1 {
		t.Fatalf("the refused re-creation must not start a second child, launches: %d", launches)
	}

	// The bridge cannot breathe life into the blocked session either:
	// reconnect to the new instance, queue and release — the dispatch
	// fails closed on the missing binding, transmitting nothing.
	bridge2 := &acceptanceBridge{t: t, client: newTestClient(srv2.SocketPath()), token: acc.token}
	code, resp := bridge2.do("POST", "/v1/runs/"+codexAcceptanceRunID+"/controller/connect", fmt.Sprintf(
		`{"op_id": "op-conn-acc-cx-restart", "controller_lease": %q, "expected_generation": 1, "instance_id": %q}`,
		lease, srv2.InstanceID()))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("restart connect: %d %v", code, resp)
	}
	hydratedRestart, err := store2.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate after restart: %v", err)
	}
	ver := hydratedRestart.Sessions[codexAcceptanceSession].RowVersion
	code, resp = bridge2.do("POST", "/v1/runs/"+codexAcceptanceRunID+"/sessions/"+codexAcceptanceSession+"/prompts/queue",
		fmt.Sprintf(`{"instance_id": %q, "op_id": "op-q-acc-cx-restart", "controller_lease": %q, "expected_version": %d,
		  "turn_key": "t-blocked", "prompt": "blocked prompt"}`, srv2.InstanceID(), lease, ver))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("queue on the blocked session: %d %v", code, resp)
	}
	receipt := resp["receipt"].(map[string]any)
	committed := int64(receipt["committed_version"].(float64))
	code, resp = bridge2.do("POST", "/v1/runs/"+codexAcceptanceRunID+"/sessions/"+codexAcceptanceSession+"/turns/t-blocked/release",
		fmt.Sprintf(`{"op_id": "op-rel-acc-cx-restart", "controller_lease": %q, "expected_version": %d}`, lease, committed))
	if code != http.StatusAccepted {
		t.Fatalf("release on the blocked session: %d %v", code, resp)
	}
	srv2.Coordinator().WaitWorkers()
	code, resp = bridge2.do("GET", "/v1/runs/"+codexAcceptanceRunID+"/sessions/"+codexAcceptanceSession+"/turns/t-blocked", "")
	if code != http.StatusOK || resp["status"] != string(council.TurnFailed) {
		t.Fatalf("the blocked session's turn must fail closed, got %d %v", code, resp)
	}
	if result, _ := resp["result"].(string); !strings.Contains(result, "no codex session binding") {
		t.Fatalf("the blocked session must fail on the missing binding, got %q", result)
	}
	if got := codexCountRequests(t, acc.fx.ScratchDir, "turn/start"); got != 0 {
		t.Fatalf("no prompt may ever reach a child for the blocked session, turn/starts: %d", got)
	}

	// Explicit resolution (controller-visible seam) clears the durable
	// block; automatic retries never reach it.
	if _, err := srv2.ResolveCodexSessionCreationUncertainty(ctx, "op-resolve-acc-cx", codexAcceptanceSession, "controller-acc-cx"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if blocked, err := store2.HasCodexCreationUncertainty(ctx, codexAcceptanceSession); err != nil || blocked {
		t.Fatalf("the durable block must be cleared by the explicit resolution, blocked=%v err=%v", blocked, err)
	}
}
