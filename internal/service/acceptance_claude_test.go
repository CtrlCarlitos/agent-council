//go:build unix

package service

// Task 7 acceptance story (Claude-only, provider-free): the full
// controller lifecycle through the production service wiring reaching a
// controlled stub `claude` child through the real PolicyExecutor —
// configured construction, adopt (secret once), connect, CreateSession +
// §3.3 binding persistence, queue, release, gated execution, collect,
// disconnect, reconnect, follow-up via the exact native session
// (--resume), and durable outcomes. Plus §6.1 focused scenarios.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/claude"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const claudeAcceptanceSession = "c1aude11-aaaa-4bbb-8ccc-dddddddddd01"

// claudeStubScript writes the sanitized `claude` stub: --version/--help
// contract surfaces (with the v8-verified choice sets) and a -p turn
// mode that logs argv, writes the bound transcript under
// CLAUDE_CONFIG_DIR, honors a .claude-fixture replay file, and emits
// the happy stream correlated to the exact native session.
func claudeStubScript(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir stub dir: %v", err)
	}
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
sid="00000000-0000-4000-8000-000000000000"
model="claude-haiku-4-5-20251001"
prev=""
for a in "$@"; do
  case "$prev" in
    --session-id|--resume) sid="$a" ;;
    --model) model="$a" ;;
  esac
  case "$a" in
    --session-id|--resume|--model) prev="$a" ;;
    *) prev="" ;;
  esac
done
umask 077
case " $* " in
  *" --version"*) echo "2.1.278 (Claude Code)"; exit 0 ;;
  *" --help"*)
    cat <<'HELP'
Usage: claude [options] [prompt]
  -p, --print                 Print response
  --output-format <format>    Output format (choices: text, json, stream-json)
  --verbose                   Verbose
  --session-id <uuid>         Session ID (must be valid UUID)
  --resume <session-id>       Resume a session
  --model <model>             Model
  --max-turns <n>             Max turns
  --permission-mode <mode>    Permission mode (choices: acceptEdits, auto, bypassPermissions, manual, dontAsk, plan)
  --disallowedTools <tools>   Denied tools
  --allowedTools <tools>      Allowed tools
HELP
    exit 0 ;;
esac
prompt=$(cat)
printf '%s\n' "$*" >> .claude-fixture-args
cfg="${CLAUDE_CONFIG_DIR:-}"
if [ -n "$cfg" ]; then
  cwd=$(pwd)
  munged=$(printf '%s' "$cwd" | sed 's/[^a-zA-Z0-9]/-/g')
  mkdir -p "$cfg/projects/$munged"
  printf '{"type":"user","message":{"role":"user","content":"%s"}}\n{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}\n' "$prompt" > "$cfg/projects/$munged/$sid.jsonl"
fi
if [ -f .claude-fixture ]; then
  cat .claude-fixture
  exit 0
fi
cwd=$(pwd)
emit() { printf '%s\n' "$1"; }
emit '{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup"}'
emit "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"$sid\",\"cwd\":\"$cwd\",\"claude_code_version\":\"2.1.278\",\"model\":\"$model\",\"permissionMode\":\"default\",\"tools\":[\"Read\",\"Glob\",\"Grep\"],\"skills\":[],\"plugins\":[]}"
emit "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"working\"}]},\"session_id\":\"$sid\"}"
emit "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"$sid\",\"result\":\"fixture response\"}"
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write claude stub: %v", err)
	}
	return dir
}

type claudeAcceptance struct {
	srv      *Server
	store    *storage.Store
	wsRoot   string
	bindings string // config base
	token    string
}

func newClaudeAcceptance(t *testing.T) *claudeAcceptance {
	t.Helper()
	dir := testStateDir(t)
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	configBase := filepath.Join(dir, "claude-config")
	templateDir := filepath.Join(dir, "claude-template")
	evidenceRoot := filepath.Join(dir, "claude-evidence")
	scratchRoot := filepath.Join(dir, "claude-probe-scratch")
	for _, d := range []string{stateDir, wsBase, configBase, templateDir, evidenceRoot, scratchRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Frozen config template (operator-provisioned).
	if err := os.WriteFile(filepath.Join(templateDir, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("template settings: %v", err)
	}
	// Pinned universe evidence (the committed 2.1.278 universe).
	os.MkdirAll(filepath.Join(evidenceRoot, "docs", "superpowers", "evidence"), 0o700)
	universeDoc := `{"claude_code_version":"2.1.278","tools":["Read","Glob","Grep","Bash","Write","WebSearch"]}`
	if err := os.WriteFile(filepath.Join(evidenceRoot, "docs", "superpowers", "evidence", "ac008-native-tool-universe-2.1.278.json"), []byte(universeDoc), 0o600); err != nil {
		t.Fatalf("universe evidence: %v", err)
	}

	binDir := claudeStubScript(t, filepath.Join(dir, "bin"))
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	bootstrap := "bootstrap-claude-acc"
	universeDigest := "sha256:" + sha256SumService(universeDoc)
	profile := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v2",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"claude", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"claude": {Model: "claude-haiku-4-5-20251001", NativeAuthMode: "inherited_host_keychain"},
		},
	}
	profile.ToolkitManifest = &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
		ProbedCLIVersion:       "2.1.278",
		UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
		UniverseEvidenceDigest: universeDigest,
		ApprovedTools:          []string{"Read", "Glob", "Grep"},
		DeniedComplement:       []string{"Bash", "Write", "WebSearch"},
		TurnsBound:             8,
	}}
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-cacc", ControllerLease: bootstrap, RunID: "run-cacc",
		Brief: "claude acceptance story", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      profile,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-cacc", bootstrap, storage.SessionRecord{
		ID: claudeAcceptanceSession, RunID: "run-cacc", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	instanceID := fmt.Sprintf("inst-cacc-%d", time.Now().UnixNano())
	authToken := "tok-cacc-" + instanceID
	lock, err := AcquireServiceLock(stateDir)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release() })

	srv, err := NewServer(store, lock, ServerConfig{
		StateDir:               stateDir,
		InstanceID:             instanceID,
		AuthToken:              authToken,
		WorkspaceBaseDir:       wsBase,
		ClaudeBinaryPath:       filepath.Join(binDir, "claude"),
		ClaudeConfigBaseDir:    configBase,
		ClaudeTemplateDir:      templateDir,
		ClaudeEvidenceRoot:     evidenceRoot,
		ClaudeProbeProfile:     probeProfileForClaudeAcceptance(),
		ClaudeProbeScratchRoot: scratchRoot,
	})
	if err != nil {
		t.Fatalf("configured service construction: %v", err)
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

	ws, err := srv.WorkspaceManager().AllocateWorkspace("run-cacc", claudeAcceptanceSession, "none", "example/repo",
		"0123456789012345678901234567890123456789")
	if err != nil {
		t.Fatalf("workspace allocation: %v", err)
	}
	return &claudeAcceptance{srv: srv, store: store, wsRoot: ws.Root, bindings: configBase, token: authToken}
}

func probeProfileForClaudeAcceptance() storage.CanonicalProfile {
	return storage.CanonicalProfile{
		AlgoVersion:         "cprof-v2",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"claude", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"claude": {Model: "claude-haiku-4-5-20251001", NativeAuthMode: "inherited_host_keychain"},
		},
	}
}

func sha256SumService(data string) string {
	sum := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", sum)
}

// adoptAndConnect performs the adopt-once + bridge connect steps and
// returns the lease secret.
func adoptAndConnect(t *testing.T, acc *claudeAcceptance, bridge *acceptanceBridge) string {
	t.Helper()
	code, resp := bridge.do("POST", "/v1/runs/run-cacc/controller/adopt", `{
		"op_id": "op-adopt-cacc", "harness": "claude", "controller_ref": "conv-cacc",
		"bootstrap_lease": "bootstrap-claude-acc"}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("adopt: %d %v", code, resp)
	}
	lease, _ := resp["lease_secret"].(string)
	if lease == "" {
		t.Fatalf("adopt must return the lease secret once, got %v", resp)
	}
	code, resp = bridge.do("POST", "/v1/runs/run-cacc/controller/connect", fmt.Sprintf(
		`{"op_id": "op-conn-cacc-1", "controller_lease": %q, "expected_generation": 1, "instance_id": %q}`,
		lease, acc.srv.InstanceID()))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("connect: %d %v", code, resp)
	}
	return lease
}

// createAndBind drives the production CreateSession through the wired
// adapter and performs the §3.3 service/storage persistence.
func (acc *claudeAcceptance) createAndBind(t *testing.T, ctx context.Context) adapter.SessionBinding {
	t.Helper()
	binding, err := acc.srv.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   claudeAcceptanceSession,
		Contributor: "claude",
		Config: adapter.SessionConfig{
			WorkspaceRoot: acc.wsRoot,
			Model:         "claude-haiku-4-5-20251001",
		},
	})
	if err != nil {
		t.Fatalf("native session creation: %v", err)
	}
	runID, err := acc.store.GetSessionRunID(ctx, claudeAcceptanceSession)
	if err != nil {
		t.Fatalf("run lookup: %v", err)
	}
	templateDigest, err := claude.TemplateDigest(filepath.Join(acc.bindings, "..", "claude-template"))
	if err != nil {
		t.Fatalf("template digest: %v", err)
	}
	if err := acc.store.InsertClaudeSessionBinding(ctx, storage.ClaudeSessionBinding{
		SessionID:      claudeAcceptanceSession,
		NativeID:       binding.NativeSessionID,
		Model:          "claude-haiku-4-5-20251001",
		Workspace:      acc.wsRoot,
		ConfigRoot:     claude.ConfigRootPath(acc.bindings, runID, claudeAcceptanceSession),
		TemplateDigest: templateDigest,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("persist binding: %v", err)
	}
	return binding
}

// TestAcceptance_Claude_BridgeLifecycle drives the whole controller
// story through production wiring: adopt -> connect -> CreateSession +
// binding persistence -> queue -> release -> gated execution to the
// stub child via the real executor -> collect -> disconnect ->
// reconnect -> follow-up turn dispatched with --resume on the exact
// native session -> durable outcomes.
func TestAcceptance_Claude_BridgeLifecycle(t *testing.T) {
	acc := newClaudeAcceptance(t)
	ctx := context.Background()
	bridge := &acceptanceBridge{t: t, client: newTestClient(acc.srv.SocketPath()), token: acc.token}

	// 1+2. Adopt (secret returned exactly once) and bridge connect.
	lease := adoptAndConnect(t, acc, bridge)
	var code int
	var resp map[string]any

	// 3. Native session birth through the production adapter; the
	// service/storage layer persists the returned binding (§3.3).
	binding := acc.createAndBind(t, ctx)
	if binding.NativeSessionID == claudeAcceptanceSession {
		t.Fatal("native id must be freshly generated and distinct")
	}

	// 4. Bridge queue.
	hydrated0, err := acc.store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	ver0 := hydrated0.Sessions[claudeAcceptanceSession].RowVersion
	code, resp = bridge.do("POST", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/prompts/queue", fmt.Sprintf(
		`{"instance_id": %q, "op_id": "op-q-cacc-1", "controller_lease": %q, "expected_version": %d,
		  "turn_key": "t-cacc-1", "prompt": "acceptance prompt"}`, acc.srv.InstanceID(), lease, ver0))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("queue: %d %v", code, resp)
	}
	receipt := resp["receipt"].(map[string]any)
	verAfterQueue := int64(receipt["committed_version"].(float64))

	// 5. Bridge release: the supervisor worker executes through the
	// production adapter to the stub child (first turn: --session-id).
	code, resp = bridge.do("POST", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/turns/t-cacc-1/release", fmt.Sprintf(
		`{"op_id": "op-rel-cacc-1", "controller_lease": %q, "expected_version": %d}`, lease, verAfterQueue))
	if code != http.StatusAccepted {
		t.Fatalf("release: %d %v", code, resp)
	}
	acc.srv.Coordinator().WaitWorkers()

	// 6. Bridge collect: terminal with the correlated native output.
	code, resp = bridge.do("GET", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/turns/t-cacc-1", "")
	if code != http.StatusOK {
		t.Fatalf("collect: %d %v", code, resp)
	}
	if resp["status"] != string(council.TurnCompleted) {
		t.Fatalf("turn must be completed, got %v (%v)", resp["status"], resp)
	}
	if result, _ := resp["result"].(string); !strings.Contains(result, "fixture response") {
		t.Fatalf("expected correlated native output, got %q", result)
	}

	// The first turn launched with --session-id; the verified terminal
	// materialized the binding; the transcript carries the accepted
	// user entry, so resume passes local inspection end to end.
	argLines := readStubState(t, acc.wsRoot, ".claude-fixture-args")
	if len(argLines) != 1 || !strings.Contains(argLines[0], "--session-id") {
		t.Fatalf("first turn must launch with --session-id, got %v", argLines)
	}
	stored, err := acc.store.GetClaudeSessionBinding(ctx, claudeAcceptanceSession)
	if err != nil || stored == nil || !stored.Materialized {
		t.Fatalf("binding must be materialized after the verified first turn: %+v %v", stored, err)
	}
	if err := acc.srv.adapter.ResumeSession(ctx, adapter.SessionBinding{
		SessionID:       claudeAcceptanceSession,
		NativeSessionID: stored.NativeID,
		Config:          adapter.SessionConfig{Model: "claude-haiku-4-5-20251001", WorkspaceRoot: acc.wsRoot},
	}); err != nil {
		t.Fatalf("resume local inspection with transcript correlation: %v", err)
	}

	// 7. Bridge disconnect, 8. reconnect (Claude has no persistent
	// server to park: the process per turn is already gone).
	code, _ = bridge.do("POST", "/v1/runs/run-cacc/controller/disconnect", fmt.Sprintf(
		`{"op_id": "op-disc-cacc", "controller_lease": %q, "expected_generation": 1}`, lease))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("disconnect: %d", code)
	}
	code, resp = bridge.do("POST", "/v1/runs/run-cacc/controller/connect", fmt.Sprintf(
		`{"op_id": "op-conn-cacc-2", "controller_lease": %q, "expected_generation": 1, "instance_id": %q}`,
		lease, acc.srv.InstanceID()))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("reconnect: %d %v", code, resp)
	}

	// 9. Follow-up turn: dispatched with --resume on the EXACT native
	// session.
	hydrated, err := acc.store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	verNow := hydrated.Sessions[claudeAcceptanceSession].RowVersion
	code, resp = bridge.do("POST", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/prompts/queue", fmt.Sprintf(
		`{"instance_id": %q, "op_id": "op-q-cacc-2", "controller_lease": %q, "expected_version": %d,
		  "turn_key": "t-cacc-2", "prompt": "follow-up prompt"}`, acc.srv.InstanceID(), lease, verNow))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("queue 2: %d %v", code, resp)
	}
	receipt2 := resp["receipt"].(map[string]any)
	verAfterQueue2 := int64(receipt2["committed_version"].(float64))
	code, resp = bridge.do("POST", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/turns/t-cacc-2/release", fmt.Sprintf(
		`{"op_id": "op-rel-cacc-2", "controller_lease": %q, "expected_version": %d}`, lease, verAfterQueue2))
	if code != http.StatusAccepted {
		t.Fatalf("release 2: %d %v", code, resp)
	}
	acc.srv.Coordinator().WaitWorkers()

	code, resp = bridge.do("GET", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/turns/t-cacc-2", "")
	if code != http.StatusOK {
		t.Fatalf("collect 2: %d %v", code, resp)
	}
	if resp["status"] != string(council.TurnCompleted) {
		t.Fatalf("follow-up must complete, got %v (%v)", resp["status"], resp)
	}

	argLines = readStubState(t, acc.wsRoot, ".claude-fixture-args")
	if len(argLines) != 2 || !strings.Contains(argLines[1], "--resume") {
		t.Fatalf("follow-up must resume with --resume on the exact native session, got %v", argLines)
	}

	// 10. Durable evidence: both turns persisted with native results.
	hydrated, err = acc.store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	turns := hydrated.Sessions[claudeAcceptanceSession].Turns
	for _, key := range []string{"t-cacc-1", "t-cacc-2"} {
		turn, ok := turns[key]
		if !ok {
			t.Fatalf("turn %s missing from durable state", key)
		}
		if turn.Status != string(council.TurnCompleted) {
			t.Fatalf("turn %s must be durably completed, got %v", key, turn.Status)
		}
	}
}

// §6.1 scenario 1: N concurrent callers releasing one turn produce
// exactly one accepted execution and one native process; the losers are
// rejected on durable turn state.
func TestAcceptance_Claude_ConcurrentDuplicateDispatch(t *testing.T) {
	acc := newClaudeAcceptance(t)
	ctx := context.Background()
	bridge := &acceptanceBridge{t: t, client: newTestClient(acc.srv.SocketPath()), token: acc.token}
	lease := adoptAndConnect(t, acc, bridge)
	acc.createAndBind(t, ctx)

	hydrated, err := acc.store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	ver := hydrated.Sessions[claudeAcceptanceSession].RowVersion
	code, resp := bridge.do("POST", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/prompts/queue", fmt.Sprintf(
		`{"instance_id": %q, "op_id": "op-q-cacc-dup", "controller_lease": %q, "expected_version": %d,
		  "turn_key": "t-dup", "prompt": "duplicate prompt"}`, acc.srv.InstanceID(), lease, ver))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("queue: %d %v", code, resp)
	}
	receipt := resp["receipt"].(map[string]any)
	verAfterQueue := int64(receipt["committed_version"].(float64))

	const n = 4
	accepted := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			code, _ := bridge.do("POST", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/turns/t-dup/release",
				fmt.Sprintf(`{"op_id": "op-rel-cacc-dup-%d", "controller_lease": %q, "expected_version": %d}`,
					slot, lease, verAfterQueue))
			if code == http.StatusAccepted {
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
	argLines := readStubState(t, acc.wsRoot, ".claude-fixture-args")
	if len(argLines) != 1 {
		t.Fatalf("exactly one native process may run, got %d invocations", len(argLines))
	}
	code, resp = bridge.do("GET", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/turns/t-dup", "")
	if code != http.StatusOK || resp["status"] != string(council.TurnCompleted) {
		t.Fatalf("the shared verdict must be completed, got %d %v", code, resp)
	}
}

// §6.1 scenario 6: a would-prompt tool under -p produces the
// approval_denied classification and a bounded FAILED terminal (via the
// error_max_turns result) — never a hang.
func TestAcceptance_Claude_ApprovalRequiredBoundedRun(t *testing.T) {
	acc := newClaudeAcceptance(t)
	ctx := context.Background()
	binding := acc.createAndBind(t, ctx)

	bridge := &acceptanceBridge{t: t, client: newTestClient(acc.srv.SocketPath()), token: acc.token}
	lease := adoptAndConnect(t, acc, bridge)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	sid := binding.NativeSessionID
	fixture := strings.Join([]string{
		`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup"}`,
		fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"cwd":%q,"claude_code_version":"2.1.278","model":"claude-haiku-4-5-20251001","permissionMode":"default","tools":["Read","Glob","Grep"],"skills":[],"plugins":[]}`, sid, acc.wsRoot),
		fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Write","input":{"path":%q}}]},"session_id":%q}`, filepath.Join(cwd, "outside.txt"), sid),
		fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"Claude requested permissions to write to %s, but you haven't granted it yet"}]},"session_id":%q}`, filepath.Join(cwd, "outside.txt"), sid),
		fmt.Sprintf(`{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":%q,"result":"reached max turns"}`, sid),
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(acc.wsRoot, ".claude-fixture"), []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture stream: %v", err)
	}

	start := time.Now()
	hydrated, err := acc.store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	ver := hydrated.Sessions[claudeAcceptanceSession].RowVersion
	code, resp := bridge.do("POST", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/prompts/queue", fmt.Sprintf(
		`{"instance_id": %q, "op_id": "op-q-cacc-appr", "controller_lease": %q, "expected_version": %d,
		  "turn_key": "t-approval", "prompt": "write the file"}`, acc.srv.InstanceID(), lease, ver))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("queue: %d %v", code, resp)
	}
	receipt := resp["receipt"].(map[string]any)
	verAfterQueue := int64(receipt["committed_version"].(float64))
	code, resp = bridge.do("POST", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/turns/t-approval/release", fmt.Sprintf(
		`{"op_id": "op-rel-cacc-appr", "controller_lease": %q, "expected_version": %d}`, lease, verAfterQueue))
	if code != http.StatusAccepted {
		t.Fatalf("release: %d %v", code, resp)
	}

	// The run is bounded: the worker reaches the terminal promptly.
	acc.srv.Coordinator().WaitWorkers()

	code, resp = bridge.do("GET", "/v1/runs/run-cacc/sessions/"+claudeAcceptanceSession+"/turns/t-approval", "")
	if code != http.StatusOK {
		t.Fatalf("collect: %d %v", code, resp)
	}
	if resp["status"] != string(council.TurnFailed) {
		t.Fatalf("approval-bounded run must end FAILED, got %v (%v)", resp["status"], resp)
	}
	if result, _ := resp["result"].(string); !strings.Contains(result, "reached max turns") {
		t.Fatalf("verified failure text must be retained, got %q", result)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("approval-required run must be bounded, took %v", elapsed)
	}

	// The durable attempt records the verified failed terminal.
	attempt, err := acc.store.GetLatestClaudeTurnAttempt(ctx, claudeAcceptanceSession, "t-approval")
	if err != nil || attempt == nil {
		t.Fatalf("get attempt: %v", err)
	}
	if !attempt.Terminal || attempt.ObservedStatus != "failed" {
		t.Fatalf("attempt must be terminal failed, got %+v", attempt)
	}
}
