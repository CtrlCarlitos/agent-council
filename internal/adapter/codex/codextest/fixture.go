// Package codextest is the test-only construction mode for the codex
// adapter (AC-009 spec §3.3, mirroring the AC-006 adaptertest guard): it
// builds adapters with production eligibility SKIPPED and every protocol
// validation retained, over the same compiled fixture `codex` executable
// the codex package tests use. It is usable ONLY from tests and the
// operator-invoked manual evidence executable — never registered as a
// production adapter, and production packages must not import it (the
// build guard in guard_test.go enforces this).
package codextest

// The fixture `codex` executable below is the harness copy of the
// scripted JSON-RPC 2.0 stub in internal/adapter/codex/codexfake_test.go
// (the codex package's internal test files cannot import this package —
// import cycle — so the source is carried here verbatim; both copies are
// continuously exercised by their respective suites). It speaks JSON-RPC
// 2.0 over stdio (one object per line), launched through the real
// PolicyExecutor exactly like the native app-server child, replaying
// staged scenario directives.
//
// Built-in behavior (no scenario needed):
//   - initialize is always answered with the recorded handshake shape,
//     codexHome derived from CODEX_HOME (never set by the adapter) or
//     $HOME/.codex, platform from runtime (overridable via the
//     .codex-fixture-platform knob)
//   - unknown methods are answered with the -32600 unknown-variant error
//
// Scenario directives (JSONL):
//   {"respond": {"method": M, "result": R}}            answer every M request with R
//   {"respond_error": {"method": M, "code": C, "message": S}}
//   {"emit_on_request": {"method": M, "line": L}}      emit L BEFORE answering M (consume once)
//   {"emit_after_response": {"method": M, "line": L}}  emit L AFTER answering M (consume once)
//   {"append_on_request": {"method": M, "path": P, "lines": [...]}}
//                                                      append lines to file P BEFORE answering M
//   {"emit": L} / {"emit_raw": S} / {"emit_oversized": N}   immediate output
//   {"stderr": S} / {"delay_ms": N}                    immediate side effects
//
// Evidence knobs (resolved in the child's CWD):
//   .codex-fixture-args         append-mode argv log (template evidence)
//   .codex-fixture-requests     append-mode request log (method evidence)
//   .codex-fixture-replies      verbatim reply frames
//   .codex-fixture-terminated   written on graceful SIGTERM (with pid)
//   .codex-fixture-platform     overrides platformOs (handshake mismatch)

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const fixtureSource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type respondRule struct {
	Method  string          ` + "`json:\"method\"`" + `
	Result  json.RawMessage ` + "`json:\"result\"`" + `
	DelayMs int64           ` + "`json:\"delay_ms\"`" + `
}

type respondErrorRule struct {
	Method  string ` + "`json:\"method\"`" + `
	Code    int    ` + "`json:\"code\"`" + `
	Message string ` + "`json:\"message\"`" + `
	DelayMs int64  ` + "`json:\"delay_ms\"`" + `
}

type lineRule struct {
	Method string          ` + "`json:\"method\"`" + `
	Line   json.RawMessage ` + "`json:\"line\"`" + `
	used   bool
}

type appendRule struct {
	Method string   ` + "`json:\"method\"`" + `
	Path   string   ` + "`json:\"path\"`" + `
	Lines  []string ` + "`json:\"lines\"`" + `
	used   bool
}

type directive struct {
	Respond           *respondRule      ` + "`json:\"respond\"`" + `
	RespondError      *respondErrorRule ` + "`json:\"respond_error\"`" + `
	EmitOnRequest     *lineRule         ` + "`json:\"emit_on_request\"`" + `
	EmitAfterResponse *lineRule         ` + "`json:\"emit_after_response\"`" + `
	AppendOnRequest   *appendRule       ` + "`json:\"append_on_request\"`" + `
	Emit              json.RawMessage   ` + "`json:\"emit\"`" + `
	EmitRaw           string            ` + "`json:\"emit_raw\"`" + `
	EmitOversized     int64             ` + "`json:\"emit_oversized\"`" + `
	Stderr            string            ` + "`json:\"stderr\"`" + `
	DelayMs           int64             ` + "`json:\"delay_ms\"`" + `
}

func appendLine(name, line string) {
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func main() {
	appendLine(".codex-fixture-args", strings.Join(os.Args[1:], "\x1f"))

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-ch
		appendLine(".codex-fixture-terminated", strconv.Itoa(os.Getpid()))
		os.Exit(0)
	}()

	fmt.Fprint(os.Stderr, "codex fixture stderr noise\n")

	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home := os.Getenv("HOME")
		codexHome = home + "/.codex"
	}
	platformOS := runtime.GOOS
	if b, err := os.ReadFile(".codex-fixture-platform"); err == nil {
		platformOS = strings.TrimSpace(string(b))
	}
	platformFamily := "unix"

	initResult := map[string]any{
		"userAgent":      "agent-council/0.154.0 (fixture; x86_64)",
		"codexHome":      codexHome,
		"platformFamily": platformFamily,
		"platformOs":     platformOS,
	}

	var (
		responds  []respondRule
		respErrs  []respondErrorRule
		onReq     []*lineRule
		afterResp []*lineRule
		appends   []*appendRule
	)
	if raw, err := os.ReadFile(".codex-fixture-scenario.jsonl"); err == nil {
		for _, ln := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(ln) == "" {
				continue
			}
			var d directive
			if err := json.Unmarshal([]byte(ln), &d); err != nil {
				continue
			}
			switch {
			case d.Respond != nil:
				responds = append(responds, *d.Respond)
			case d.RespondError != nil:
				respErrs = append(respErrs, *d.RespondError)
			case d.EmitOnRequest != nil:
				onReq = append(onReq, &lineRule{Method: d.EmitOnRequest.Method, Line: d.EmitOnRequest.Line})
			case d.EmitAfterResponse != nil:
				afterResp = append(afterResp, &lineRule{Method: d.EmitAfterResponse.Method, Line: d.EmitAfterResponse.Line})
			case d.AppendOnRequest != nil:
				appends = append(appends, &appendRule{Method: d.AppendOnRequest.Method, Path: d.AppendOnRequest.Path, Lines: d.AppendOnRequest.Lines})
			case len(d.Emit) > 0:
				fmt.Fprintln(os.Stdout, string(d.Emit))
			case d.EmitRaw != "":
				fmt.Fprintln(os.Stdout, d.EmitRaw)
			case d.EmitOversized > 0:
				fmt.Fprintln(os.Stdout, strings.Repeat("x", int(d.EmitOversized)))
			case d.Stderr != "":
				fmt.Fprint(os.Stderr, d.Stderr)
			case d.DelayMs > 0:
				time.Sleep(time.Duration(d.DelayMs) * time.Millisecond)
			}
		}
	}

	replyResult := func(id json.RawMessage, result any) {
		r, _ := json.Marshal(result)
		fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n", string(id), string(r))
	}
	replyError := func(id json.RawMessage, code int, msg string) {
		fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":%d,\"message\":%q}}\n", string(id), code, msg)
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     json.RawMessage ` + "`json:\"id\"`" + `
			Method string          ` + "`json:\"method\"`" + `
			Params json.RawMessage ` + "`json:\"params\"`" + `
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.Method == "" {
			appendLine(".codex-fixture-replies", line)
			continue
		}
		appendLine(".codex-fixture-requests", req.Method)

		if req.Method == "initialize" {
			replyResult(req.ID, initResult)
			continue
		}

		for _, r := range onReq {
			if !r.used && r.Method == req.Method {
				fmt.Fprintln(os.Stdout, string(r.Line))
				r.used = true
				break
			}
		}
		for _, r := range appends {
			if !r.used && r.Method == req.Method {
				f, err := os.OpenFile(r.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
				if err == nil {
					for _, l := range r.Lines {
						fmt.Fprintln(f, l)
					}
					f.Close()
				}
				r.used = true
				break
			}
		}

		answered := false
		for _, r := range responds {
			if r.Method == req.Method {
				if r.DelayMs > 0 {
					time.Sleep(time.Duration(r.DelayMs) * time.Millisecond)
				}
				replyResult(req.ID, json.RawMessage(r.Result))
				answered = true
				break
			}
		}
		if !answered {
			for _, r := range respErrs {
				if r.Method == req.Method {
					if r.DelayMs > 0 {
						time.Sleep(time.Duration(r.DelayMs) * time.Millisecond)
					}
					replyError(req.ID, r.Code, r.Message)
					answered = true
					break
				}
			}
		}
		if !answered {
			replyError(req.ID, -32600, "Invalid request: unknown variant <"+req.Method+">")
			answered = true
		}

		for _, r := range afterResp {
			if !r.used && r.Method == req.Method {
				fmt.Fprintln(os.Stdout, string(r.Line))
				r.used = true
				break
			}
		}
	}
	// stdin closed: parent is gone or the connection ended; park until
	// signaled so lifecycle evidence stays deterministic.
	select {}
}
`

var (
	fixtureOnce   sync.Once
	fixtureBinDir string
	fixtureErr    error
)

// compileFixtureExecutable builds the fake `codex` binary once per
// process and returns the directory to prepend to PATH.
func compileFixtureExecutable() (string, error) {
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ac009-codextest-fixture-*")
		if err != nil {
			fixtureErr = err
			return
		}
		srcDir := filepath.Join(dir, "src")
		if err := os.MkdirAll(srcDir, 0o700); err != nil {
			fixtureErr = err
			return
		}
		src := filepath.Join(srcDir, "main.go")
		if err := os.WriteFile(src, []byte(fixtureSource), 0o600); err != nil {
			fixtureErr = err
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "codex"), src)
		build.Dir = srcDir
		if out, err := build.CombinedOutput(); err != nil {
			fixtureErr = fmt.Errorf("build codex fixture: %v: %s", err, out)
			return
		}
		fixtureBinDir = dir
	})
	return fixtureBinDir, fixtureErr
}

// Fixed native ids the default scenario binds (canonical UUIDv7 shapes,
// same values the codex package harness uses).
const (
	fixtureThreadID = "01900000-0000-7000-8000-000000000001"
	fixtureTurnID   = "01900000-0000-7000-8000-0000000000aa"
	fixtureRunID    = "run-codextest"
)

// FixtureOptions configures the test-only construction. The zero value
// stages the default happy-path scenario and uses the deterministic
// default attempt identity.
type FixtureOptions struct {
	// Scenario are JSONL fixture directive lines staged into the child
	// working directory before any child starts. Empty/nil stages the
	// default happy-path scenario (auth, thread/start, thread/resume,
	// accepted turn that completes).
	Scenario []string
	// Identity overrides the attempt-identity source. Nil uses the
	// deterministic default ("att-" + turn key).
	Identity codex.DispatchIdentitySource
}

// Fixture is the live test-only harness around one fixture-scoped
// adapter: the neutral scratch directory (the child's working directory
// and evidence root), the workspace root, the durable store, the child
// manager, and the adapter built with production eligibility skipped.
type Fixture struct {
	// ScratchDir is the child working directory: scenario staging and
	// the .codex-fixture-* evidence files live here.
	ScratchDir string
	// WorkspaceRoot is the frozen workspace the profile pins.
	WorkspaceRoot string
	// Store is the durable store seeded with the fixture run record.
	Store *storage.Store
	// Server owns the fixture children.
	Server *codex.CodexServer
	// Adapter is the fixture-scoped adapter: production eligibility
	// skipped, every protocol validation retained.
	Adapter *codex.CodexAdapter
	// Policy is the frozen launch policy (ValidateCodexHarness output).
	Policy codex.CodexLaunchPolicy
	// ProfileDigest is the frozen profile digest.
	ProfileDigest string
	// Model is the frozen profile model.
	Model string

	dirs []string
}

// fixtureIdentity is the deterministic default attempt identity.
type fixtureIdentity struct{}

func (fixtureIdentity) AttemptFor(_ context.Context, ref adapter.TurnRef) (string, bool) {
	return "att-" + ref.TurnKey, true
}

// fixtureLaunchSource builds the exact `codex app-server` LaunchRequest
// rooted at the scratch directory (the child's cwd; the workspace root
// stays out of the child process plumbing).
type fixtureLaunchSource struct {
	scratch string
}

func (s fixtureLaunchSource) CodexAppServerLaunch(_ context.Context, sessionID adapter.SessionID) (execpolicy.LaunchRequest, error) {
	return execpolicy.LaunchRequest{
		RunID:     fixtureRunID,
		SessionID: string(sessionID),
		Command:   "codex",
		Args:      append([]string(nil), codex.CodexAppServerArgs...),
		Paths: workspace.WorkspacePaths{
			Root:   s.scratch,
			Config: s.scratch,
		},
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v3",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"codex"},
		},
	}, nil
}

// buildFixtureProfile mirrors the cprof-v3 codex harness profile the
// codex package tests freeze: it is validated through the production
// ValidateCodexHarness (the fixture path keeps the whole frozen-policy
// discipline) and pinned to the scratch codex home and workspace.
func buildFixtureProfile(scratch, wsRoot string) (storage.CanonicalProfile, codex.CodexLaunchPolicy, string, string, error) {
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
					ApprovalPolicy:             storage.CodexApprovalPolicy{Kind: "string", String: "on-request"},
					ApprovalsReviewer:          "user",
					ExpectedMCPServers:         []string{"context7"},
					ExpectedInstructionSources: []string{"~/.codex/AGENTS.md"},
					RulesEvidence: storage.CodexRulesEvidenceSpec{
						Verified:     []string{"sandbox workspace-write"},
						Unverifiable: []string{"~/.codex/rules/*.rules contents"},
					},
					EventUniversePath:   "docs/superpowers/evidence/ac009-native-event-universe-0.154.0.json",
					EventUniverseDigest: "",
				},
			},
		},
	}
	profile.ToolkitManifest = &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
		ProbedCLIVersion:       "2.1.278",
		UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
		UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
		ApprovedTools:          []string{"Read", "Glob"},
		DeniedComplement:       []string{"Bash"},
		ExpectedHooks:          []string{"SessionStart:startup"},
		TurnsBound:             8,
	}}

	// Stage the frozen event universe and pin its digest (validation
	// re-hashes it; drift fails closed — the production discipline).
	universe := map[string]any{"codex_cli_version": "0.154.0", "methods": []string{"initialize", "thread/start"}}
	raw, err := json.Marshal(universe)
	if err != nil {
		return profile, codex.CodexLaunchPolicy{}, "", "", fmt.Errorf("marshal universe: %w", err)
	}
	root := filepath.Join(scratch, "evidence")
	full := filepath.Join(root, filepath.FromSlash(profile.Harnesses["codex"].Codex.EventUniversePath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return profile, codex.CodexLaunchPolicy{}, "", "", fmt.Errorf("evidence dir: %w", err)
	}
	if err := os.WriteFile(full, raw, 0o600); err != nil {
		return profile, codex.CodexLaunchPolicy{}, "", "", fmt.Errorf("write universe: %w", err)
	}
	profile.Harnesses["codex"].Codex.EventUniverseDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))

	policy, err := codex.ValidateCodexHarness(profile, root)
	if err != nil {
		return profile, codex.CodexLaunchPolicy{}, "", "", fmt.Errorf("validate codex harness: %w", err)
	}
	digest, _, err := storage.ComputeProfileDigest(profile)
	if err != nil {
		return profile, codex.CodexLaunchPolicy{}, "", "", fmt.Errorf("profile digest: %w", err)
	}
	return profile, policy, digest, profile.Harnesses["codex"].Model, nil
}

// NewFixtureAdapter builds the test-only fixture harness: fixture
// executable compiled and put on PATH, frozen profile validated through
// the production validation, durable store opened and seeded with the
// run record, and the adapter constructed through the explicit
// fixture-scoped constructor (production eligibility skipped, marker
// required — never inferred). No child starts here: children launch
// lazily at CreateSession, so the scenario can still be restaged.
func NewFixtureAdapter(opts FixtureOptions) (*Fixture, error) {
	scratch, err := os.MkdirTemp("", "ac009-codextest-*")
	if err != nil {
		return nil, fmt.Errorf("scratch dir: %w", err)
	}
	wsRoot, err := os.MkdirTemp("", "ac009-codextest-ws-*")
	if err != nil {
		_ = os.RemoveAll(scratch)
		return nil, fmt.Errorf("workspace dir: %w", err)
	}
	f := &Fixture{
		ScratchDir:    scratch,
		WorkspaceRoot: wsRoot,
		dirs:          []string{scratch, wsRoot},
	}

	profile, policy, digest, model, err := buildFixtureProfile(scratch, wsRoot)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	f.Policy, f.ProfileDigest, f.Model = policy, digest, model

	binDir, err := compileFixtureExecutable()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := os.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH")); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stage fixture on PATH: %w", err)
	}

	ctx := context.Background()
	store, err := storage.Open(storage.StoreOptions{StateDir: filepath.Join(scratch, "state")})
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("open store: %w", err)
	}
	f.Store = store
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID:               "op-run-codextest",
		ControllerLease:    "lease-codextest",
		RunID:              fixtureRunID,
		Brief:              "codextest fixture harness",
		SourceRepoIdentity: "example/repo",
		SourceCommit:       strings.Repeat("0", 40),
		SourceTree:         strings.Repeat("a", 40),
		Profile:            profile,
	}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("create run: %w", err)
	}

	f.Server = codex.NewCodexServer(execpolicy.New(), fixtureLaunchSource{scratch: scratch}, policy)
	identity := opts.Identity
	if identity == nil {
		identity = fixtureIdentity{}
	}
	f.Adapter = codex.NewFixtureScopedAdapter(store, f.Server, policy, digest, identity, codex.FixtureMode{})

	lines := opts.Scenario
	if len(lines) == 0 {
		lines = DefaultScenario(f.WorkspaceRoot, f.Model)
	}
	if err := f.StageScenario(lines...); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// SeedSession records the logical session row the §3.4 creation flow
// requires (the adapter validates the contributor against it). Call it
// once per session id before CreateSession.
func (f *Fixture) SeedSession(ctx context.Context, sessionID string) error {
	_, err := f.Store.CreateSession(ctx, "op-sess-"+sessionID, "lease-codextest", storage.SessionRecord{
		ID: sessionID, RunID: fixtureRunID, Contributor: "codex", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		return fmt.Errorf("seed session %s: %w", sessionID, err)
	}
	return nil
}

// NewCreateRequest builds a CreateSessionRequest against the frozen
// fixture profile (contributor codex, frozen model and workspace).
func (f *Fixture) NewCreateRequest(sessionID string) adapter.CreateSessionRequest {
	return adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID(sessionID),
		Contributor: "codex",
		Config: adapter.SessionConfig{
			WorkspaceRoot: f.WorkspaceRoot,
			Model:         f.Model,
		},
	}
}

// PersistBinding is the §3.4 service/storage seam: the adapter returned
// the binding, storage records it (required before the first dispatch).
func (f *Fixture) PersistBinding(ctx context.Context, binding adapter.SessionBinding) error {
	if err := f.Store.InsertCodexSessionBinding(ctx, storage.CodexSessionBinding{
		SessionID:     string(binding.SessionID),
		NativeID:      binding.NativeSessionID,
		Model:         f.Model,
		Workspace:     f.WorkspaceRoot,
		ProfileDigest: f.ProfileDigest,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("persist binding: %w", err)
	}
	return nil
}

// StageScenario replaces the staged scenario file. Directives are read
// when a child STARTS, so restaging before the first CreateSession (or
// before a replacement-child launch) is what takes effect.
func (f *Fixture) StageScenario(lines ...string) error {
	joined := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(f.ScratchDir, ".codex-fixture-scenario.jsonl"), []byte(joined), 0o600); err != nil {
		return fmt.Errorf("stage scenario: %w", err)
	}
	return nil
}

// Close terminates every fixture child gracefully, closes the store, and
// removes the scratch directories. Safe to call more than once.
func (f *Fixture) Close() error {
	if f.Server != nil {
		f.Server.Close(context.Background())
	}
	if f.Store != nil {
		_ = f.Store.Close()
	}
	for _, dir := range f.dirs {
		_ = os.RemoveAll(dir)
	}
	return nil
}

// ── Default scenario builders ───────────────────────────────────────────

// DefaultScenario stages the §3.4/§3.5 happy path: authenticated
// account/read, thread/start reserving the canonical fixture thread,
// drift-free thread/resume, and an accepted turn that completes.
func DefaultScenario(wsRoot, model string) []string {
	lines := append([]string{authOKLine()}, threadStartRules(fixtureThreadID, wsRoot, model)...)
	lines = append(lines, resumeRule(fixtureThreadID, wsRoot, model, nil))
	lines = append(lines, turnAcceptedRules(fixtureThreadID, fixtureTurnID, true)...)
	return lines
}

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
// policy, with drift applied per field for §3.5 rejection evidence.
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
	out := []string{
		`{"respond": {"method":"turn/start","result":{"id":"` + turnID + `","threadId":"` + threadID + `","status":{"type":"inProgress"}}}}`,
		`{"emit_on_request": {"method":"turn/start","line":{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"` + threadID + `","turnId":"` + turnID + `"}}}}`,
	}
	if complete {
		out = append(out, `{"emit_after_response": {"method":"turn/start","line":{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"`+threadID+`","turn":{"id":"`+turnID+`","items":[],"status":"completed","durationMs":100}}}}}`)
	}
	return out
}

// PoisonThreadStartScenario stages an authenticated creation whose
// thread/start returns a non-canonical id: protocol validation must
// classify the creation UNCERTAIN even in the fixture scope.
func PoisonThreadStartScenario() []string {
	return []string{
		authOKLine(),
		`{"emit_on_request": {"method":"thread/start","line":{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"definitely-not-a-real-id","status":{"type":"idle"}}}}}}`,
		`{"respond": {"method":"thread/start","result":{"id":"definitely-not-a-real-id","status":{"type":"idle"}}}}`,
	}
}
