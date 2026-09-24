package codex

// Task 9 probe-template and production-constructor evidence (spec §3.8,
// §3.3): the operator-owned probe template validates its configuration
// fail-closed, the probe child's argv is the pinned app-server template,
// and the §3.8 probe surfaces run provider-free against a scripted stub
// — version, daemon version, the NEGATIVE thread/resume contract
// (canonical zero UUID ⇒ verbatim missing-thread error), model/list,
// mcpServerStatus/list, and login status. The probe NEVER sends a turn.
// The production constructor fails closed on every missing dependency
// and on frozen-evidence drift, and wires the eligibility lookup to the
// durable rows — the §3.3 gate fires before any child can start.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func probeTemplateFor(t *testing.T, binaryPath, scratchRoot string, profile storage.CanonicalProfile) CodexProbeLaunchTemplate {
	t.Helper()
	tmpl := NewCodexProbeLaunchTemplate(binaryPath, scratchRoot, profile)
	if tmpl == nil {
		t.Fatal("nil probe template")
	}
	return tmpl
}

// The probe template fails closed on every missing configuration piece:
// binary path, scratch root, and a non-empty canonical profile.
func TestProbeTemplate_ConfigurationFailClosed(t *testing.T) {
	profile := v3CodexProfile()
	cases := []struct {
		name                    string
		binaryPath, scratchRoot string
		profile                 storage.CanonicalProfile
		wantErr                 string
	}{
		{"missing binary", "", "/scratch", profile, "binary path is required"},
		{"missing scratch", "codex", "", profile, "scratch root is required"},
		{"empty profile", "codex", "/scratch", storage.CanonicalProfile{}, "non-empty canonical profile"},
		{"harnessless profile", "codex", "/scratch", storage.CanonicalProfile{AlgoVersion: "cprof-v3"}, "non-empty canonical profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := probeTemplateFor(t, tc.binaryPath, tc.scratchRoot, tc.profile)
			for _, launch := range []struct {
				name string
				fn   func(ctx context.Context) (execpolicy.LaunchRequest, error)
			}{
				{"version", tmpl.VersionLaunch},
				{"daemon version", tmpl.DaemonVersionLaunch},
				{"login status", tmpl.LoginStatusLaunch},
				{"probe child", tmpl.ProbeChildLaunch},
			} {
				if _, err := launch.fn(context.Background()); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("%s launch: expected failure containing %q, got %v", launch.name, tc.wantErr, err)
				}
			}
		})
	}
}

// The probe child's argv is EXACTLY the pinned app-server template: the
// probe child is the same launch shape as a session child, never a
// mutated variant.
func TestProbeTemplate_ProbeChildPinsAppServerArgv(t *testing.T) {
	tmpl := probeTemplateFor(t, "/usr/bin/codex", "/scratch", v3CodexProfile())
	req, err := tmpl.ProbeChildLaunch(context.Background())
	if err != nil {
		t.Fatalf("probe child launch: %v", err)
	}
	if !IsCodexAppServerLaunch(req) {
		t.Fatalf("probe child must use the pinned app-server argv, got %q %v", req.Command, req.Args)
	}
	if req.Paths.Root != "/scratch" || req.Paths.Config != "/scratch" {
		t.Fatalf("probe child cwd must be the neutral scratch, got %+v", req.Paths)
	}
}

// RunLaunchProbe requires its dependencies.
func TestRunLaunchProbe_RequiresDependencies(t *testing.T) {
	tmpl := probeTemplateFor(t, "codex", "/scratch", v3CodexProfile())
	if _, err := RunLaunchProbe(context.Background(), nil, tmpl); err == nil {
		t.Fatal("nil executor must fail")
	}
	if _, err := RunLaunchProbe(context.Background(), execpolicy.New(), nil); err == nil {
		t.Fatal("nil template must fail")
	}
}

// parseCodexVersion requires the native identity marker.
func TestParseCodexVersion(t *testing.T) {
	cases := []struct {
		out   string
		want  string
		fails bool
	}{
		{"codex-cli 0.154.0\n", "0.154.0", false},
		{"codex-cli 1.2.3-beta.1 some build info", "1.2.3-beta.1", false},
		{"0.154.0", "", true},
		{"", "", true},
		{"codex 0.154.0", "", true},
		{"codex-cli", "", true},
	}
	for _, tc := range cases {
		got, err := parseCodexVersion(tc.out)
		if tc.fails {
			if err == nil {
				t.Fatalf("parseCodexVersion(%q) must fail", tc.out)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("parseCodexVersion(%q) = %q, %v; want %q", tc.out, got, err, tc.want)
		}
	}
}

// ── POSIX probe e2e against the scripted stub ───────────────────────────

// codexStubDir writes the POSIX probe stub `codex` executable and returns
// its directory (to prepend to PATH). The stub answers the version
// surfaces with fixed lines and runs a minimal JSON-RPC loop for the
// app-server child, logging every request method to
// .codex-probe-requests in its working directory (the never-a-turn
// evidence).
func codexStubDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("posix probe stub")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --version) echo \"codex-cli 0.154.0\" ;;\n" +
		"  app-server)\n" +
		"    if [ \"$2\" = \"daemon\" ]; then echo \"app-server daemon 0.154.0\"; exit 0; fi\n" +
		"    while IFS= read -r line; do\n" +
		"      method=$(printf '%s' \"$line\" | grep -o '\"method\":\"[^\"]*\"' | head -1 | cut -d'\"' -f4)\n" +
		"      [ -n \"$method\" ] && printf '%s\\n' \"$method\" >> .codex-probe-requests\n" +
		"      id=$(printf '%s' \"$line\" | grep -o '\"id\":[0-9]*' | head -1 | grep -o '[0-9]*')\n" +
		"      case \"$line\" in\n" +
		"        *'\"initialize\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"userAgent\":\"stub/0.154.0\",\"codexHome\":\"%s/.codex\",\"platformFamily\":\"unix\",\"platformOs\":\"%s\"}}\\n' \"$id\" \"${HOME:-/tmp}\" \"$(uname -s | tr 'A-Z' 'a-z')\" ;;\n" +
		"        *'\"thread/resume\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":-32600,\"message\":\"no rollout found for thread id 00000000-0000-0000-0000-000000000000\"}}\\n' \"$id\" ;;\n" +
		"        *'\"model/list\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"models\":[{\"id\":\"gpt-5.6-sol\",\"displayName\":\"stub model\"}]}}\\n' \"$id\" ;;\n" +
		"        *'\"mcpServerStatus/list\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"servers\":[]}}\\n' \"$id\" ;;\n" +
		"      esac\n" +
		"    done\n" +
		"    ;;\n" +
		"  login) echo \"Not logged in\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write codex stub: %v", err)
	}
	return dir
}

func withStubPath(t *testing.T, binDir string) {
	t.Helper()
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))
}

// The complete §3.8 probe template runs provider-free against the stub:
// every surface answers, the negative-resume contract carries the
// verbatim missing-thread error, and NO turn was ever sent.
func TestRunLaunchProbe_StubContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix probe stub")
	}
	withStubPath(t, codexStubDir(t))
	scratch := t.TempDir()

	profile, evidenceRoot := evidenceRootForCodex(t, v3CodexProfile())
	policy, err := ValidateCodexHarness(profile, evidenceRoot)
	if err != nil {
		t.Fatalf("validate codex harness: %v", err)
	}
	tmpl := probeTemplateFor(t, "codex", scratch, profile)

	report, err := RunLaunchProbe(context.Background(), execpolicy.New(), tmpl)
	if err != nil {
		t.Fatalf("probe run: %v", err)
	}
	if report.CodexVersion != "0.154.0" {
		t.Fatalf("codex version = %q, want 0.154.0", report.CodexVersion)
	}
	if report.RawVersionOutput != "codex-cli 0.154.0" {
		t.Fatalf("raw version output = %q", report.RawVersionOutput)
	}
	if !strings.Contains(report.DaemonVersionOutput, "daemon") {
		t.Fatalf("daemon version output = %q", report.DaemonVersionOutput)
	}
	if !strings.Contains(report.Models, "gpt-5.6-sol") {
		t.Fatalf("model inventory = %q", report.Models)
	}
	if report.MCPServerStatus == "" {
		t.Fatal("mcpServerStatus/list produced no output")
	}
	if report.LoginStatusOutput != "Not logged in" {
		t.Fatalf("login status capture = %q", report.LoginStatusOutput)
	}
	if report.NegativeResume == nil {
		t.Fatal("negative resume contract missing")
	}
	if report.NegativeResume.ThreadID != probeZeroThreadID ||
		report.NegativeResume.Code != -32600 ||
		!strings.HasPrefix(report.NegativeResume.Message, "no rollout found for thread id "+probeZeroThreadID) {
		t.Fatalf("negative resume must be the verbatim missing-thread contract, got %+v", *report.NegativeResume)
	}
	_ = policy

	// The probe NEVER sends a turn — and never creates a thread.
	raw, err := os.ReadFile(filepath.Join(scratch, ".codex-probe-requests"))
	if err != nil {
		t.Fatalf("probe request evidence: %v", err)
	}
	methods := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := map[string]bool{
		"initialize": false, "thread/resume": false, "model/list": false, "mcpServerStatus/list": false,
	}
	for _, m := range methods {
		if _, ok := want[m]; !ok {
			t.Fatalf("probe sent unexpected request %q (all: %v)", m, methods)
		}
		want[m] = true
	}
	for m, seen := range want {
		if !seen {
			t.Fatalf("probe never sent %q (all: %v)", m, methods)
		}
	}
	if len(methods) != len(want) {
		t.Fatalf("unexpected probe request count %d (all: %v)", len(methods), methods)
	}
}

// A negative-resume probe that SUCCEEDS is protocol drift: the canonical
// zero UUID cannot have a rollout, so the probe fails closed instead of
// accepting an impossible thread.
func TestRunLaunchProbe_NegativeResumeSuccessFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix probe stub")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --version) echo \"codex-cli 0.154.0\" ;;\n" +
		"  app-server)\n" +
		"    if [ \"$2\" = \"daemon\" ]; then echo \"daemon 0.154.0\"; exit 0; fi\n" +
		"    while IFS= read -r line; do\n" +
		"      id=$(printf '%s' \"$line\" | grep -o '\"id\":[0-9]*' | head -1 | grep -o '[0-9]*')\n" +
		"      case \"$line\" in\n" +
		"        *'\"initialize\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"userAgent\":\"stub\",\"codexHome\":\"/tmp/.codex\",\"platformFamily\":\"unix\",\"platformOs\":\"linux\"}}\\n' \"$id\" ;;\n" +
		"        *'\"thread/resume\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"id\":\"%s\",\"status\":{\"type\":\"idle\"},\"cwd\":\"/ws\",\"approvalPolicy\":\"on-request\",\"sandbox\":{\"type\":\"workspace-write\",\"writable_roots\":[\"/ws\"],\"network_access\":false},\"approvalsReviewer\":\"user\",\"model\":\"m\",\"modelProvider\":\"openai\",\"instructionSources\":[]}}\\n' \"$id\" \"$id\" ;;\n" +
		"      esac\n" +
		"    done\n" +
		"    ;;\n" +
		"  login) echo \"Not logged in\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	withStubPath(t, dir)

	tmpl := probeTemplateFor(t, "codex", t.TempDir(), v3CodexProfile())
	_, err := RunLaunchProbe(context.Background(), execpolicy.New(), tmpl)
	if err == nil || !strings.Contains(err.Error(), "unexpectedly SUCCEEDED") {
		t.Fatalf("a successful negative-resume probe must fail closed, got %v", err)
	}
}

// A thread/resume answer with a DIFFERENT error shape is not the
// verbatim missing-thread contract: the probe fails closed.
func TestRunLaunchProbe_NegativeResumeWrongShapeFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix probe stub")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --version) echo \"codex-cli 0.154.0\" ;;\n" +
		"  app-server)\n" +
		"    if [ \"$2\" = \"daemon\" ]; then echo \"daemon 0.154.0\"; exit 0; fi\n" +
		"    while IFS= read -r line; do\n" +
		"      id=$(printf '%s' \"$line\" | grep -o '\"id\":[0-9]*' | head -1 | grep -o '[0-9]*')\n" +
		"      case \"$line\" in\n" +
		"        *'\"initialize\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"userAgent\":\"stub\",\"codexHome\":\"/tmp/.codex\",\"platformFamily\":\"unix\",\"platformOs\":\"linux\"}}\\n' \"$id\" ;;\n" +
		"        *'\"thread/resume\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":-32000,\"message\":\"server error\"}}\\n' \"$id\" ;;\n" +
		"      esac\n" +
		"    done\n" +
		"    ;;\n" +
		"  login) echo \"Not logged in\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	withStubPath(t, dir)

	tmpl := probeTemplateFor(t, "codex", t.TempDir(), v3CodexProfile())
	_, err := RunLaunchProbe(context.Background(), execpolicy.New(), tmpl)
	if err == nil || !strings.Contains(err.Error(), "verbatim missing-thread contract") {
		t.Fatalf("a wrong-shaped negative-resume answer must fail closed, got %v", err)
	}
}

// ── Production constructor fail-closed ──────────────────────────────────

// openProbeStore opens a throwaway durable store for constructor tests.
func openProbeStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(storage.StoreOptions{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// The production constructor requires every dependency and validates the
// frozen evidence at construction: missing pieces or universe drift fail
// the WIRING.
func TestNewProductionCodexAdapter_FailClosed(t *testing.T) {
	profile, evidenceRoot := evidenceRootForCodex(t, v3CodexProfile())
	tmpl := probeTemplateFor(t, "codex", t.TempDir(), profile)

	if _, err := NewProductionCodexAdapter(nil, execpolicy.New(), tmpl, profile, evidenceRoot); err == nil ||
		!strings.Contains(err.Error(), "storage store is required") {
		t.Fatalf("nil store must fail, got %v", err)
	}
	if _, err := NewProductionCodexAdapter(openProbeStore(t), nil, tmpl, profile, evidenceRoot); err == nil ||
		!strings.Contains(err.Error(), "policy executor is required") {
		t.Fatalf("nil executor must fail, got %v", err)
	}
	if _, err := NewProductionCodexAdapter(openProbeStore(t), execpolicy.New(), nil, profile, evidenceRoot); err == nil ||
		!strings.Contains(err.Error(), "probe launch template is required") {
		t.Fatalf("nil template must fail, got %v", err)
	}
	if _, err := NewProductionCodexAdapter(openProbeStore(t), execpolicy.New(), tmpl, profile, ""); err == nil ||
		!strings.Contains(err.Error(), "evidence root is required") {
		t.Fatalf("empty evidence root must fail, got %v", err)
	}
	if _, err := NewProductionCodexAdapter(openProbeStore(t), execpolicy.New(), tmpl, storage.CanonicalProfile{}, evidenceRoot); err == nil ||
		!strings.Contains(err.Error(), "requires the frozen run profile") {
		t.Fatalf("empty profile must fail, got %v", err)
	}

	// Universe-evidence drift: the re-hash runs at construction.
	_, drifted := evidenceRootForCodex(t, profile)
	full := filepath.Join(drifted, filepath.FromSlash(profile.Harnesses["codex"].Codex.EventUniversePath))
	if err := os.WriteFile(full, []byte(`{"tampered":true}`), 0o600); err != nil {
		t.Fatalf("tamper universe: %v", err)
	}
	if _, err := NewProductionCodexAdapter(openProbeStore(t), execpolicy.New(), tmpl, profile, drifted); err == nil ||
		!strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("universe drift must fail the wiring, got %v", err)
	}

	// An incomplete codex block fails with the typed profile error.
	incomplete := v3CodexProfile()
	spec := incomplete.Harnesses["codex"]
	spec.Codex = nil
	incomplete.Harnesses["codex"] = spec
	if _, err := NewProductionCodexAdapter(openProbeStore(t), execpolicy.New(), tmpl, incomplete, evidenceRoot); err == nil {
		var unsupported *ErrUnsupportedProfile
		if !errors.As(err, &unsupported) {
			t.Fatalf("incomplete codex block must fail typed ErrUnsupportedProfile, got %v", err)
		}
	}
}
