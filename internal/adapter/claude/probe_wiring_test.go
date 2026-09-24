//go:build unix

package claude

// Task 6 wiring evidence (POSIX): fail-closed production construction,
// operator-owned probe template rejections, and the Probe contract
// checks against the real fixture child — plus protocol-drift
// detection when the help surface loses a required flag.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func probeProfile() storage.CanonicalProfile {
	return storage.CanonicalProfile{
		AlgoVersion:         "cprof-v2",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"claude", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"claude": {Model: primaryModel, NativeAuthMode: "inherited_host_keychain"},
		},
	}
}

// Construction fails closed on every missing dependency.
func TestNewProductionClaudeAdapter_FailClosed(t *testing.T) {
	h := newAdapterHarness(t)
	executor := execpolicy.New()
	template := NewOperatorProbeLaunchTemplate("claude", t.TempDir(), probeProfile())

	if _, err := NewProductionClaudeAdapter(nil, h.wm, executor, template, h.configBase, h.template, h.wsRoot); err == nil {
		t.Fatal("nil store must fail construction")
	}
	if _, err := NewProductionClaudeAdapter(h.store, nil, executor, template, h.configBase, h.template, h.wsRoot); err == nil {
		t.Fatal("nil workspace manager must fail construction")
	}
	if _, err := NewProductionClaudeAdapter(h.store, h.wm, nil, template, h.configBase, h.template, h.wsRoot); err == nil {
		t.Fatal("nil executor must fail construction")
	}
	if _, err := NewProductionClaudeAdapter(h.store, h.wm, executor, nil, h.configBase, h.template, h.wsRoot); err == nil {
		t.Fatal("nil probe template must fail construction")
	}
	if _, err := NewProductionClaudeAdapter(h.store, h.wm, executor, template, "  ", h.template, h.wsRoot); err == nil {
		t.Fatal("empty config base must fail construction")
	}
	if _, err := NewProductionClaudeAdapter(h.store, h.wm, executor, template, h.configBase, "", h.wsRoot); err == nil {
		t.Fatal("empty template dir must fail construction")
	}
	if _, err := NewProductionClaudeAdapter(h.store, h.wm, executor, template, h.configBase, h.template, ""); err == nil {
		t.Fatal("empty evidence root must fail construction")
	}
}

// The operator probe template rejects missing binary path, scratch
// root, or profile before any launch is built.
func TestOperatorProbeLaunchTemplate_Rejections(t *testing.T) {
	profile := probeProfile()
	scratch := t.TempDir()

	if _, err := NewOperatorProbeLaunchTemplate("  ", scratch, profile).VersionLaunch(context.Background()); err == nil {
		t.Fatal("empty binary path must be rejected")
	}
	if _, err := NewOperatorProbeLaunchTemplate("claude", "  ", profile).VersionLaunch(context.Background()); err == nil {
		t.Fatal("empty scratch root must be rejected")
	}
	if _, err := NewOperatorProbeLaunchTemplate("claude", scratch, storage.CanonicalProfile{}).HelpLaunch(context.Background()); err == nil {
		t.Fatal("empty profile must be rejected")
	}

	req, err := NewOperatorProbeLaunchTemplate("/bin/claude", scratch, profile).VersionLaunch(context.Background())
	if err != nil {
		t.Fatalf("valid version launch: %v", err)
	}
	if req.Command != "/bin/claude" || len(req.Args) != 1 || req.Args[0] != "--version" {
		t.Fatalf("version launch must be the exact contract shape, got %+v", req)
	}
	req, err = NewOperatorProbeLaunchTemplate("/bin/claude", scratch, profile).HelpLaunch(context.Background())
	if err != nil {
		t.Fatalf("valid help launch: %v", err)
	}
	if len(req.Args) != 1 || req.Args[0] != "--help" {
		t.Fatalf("help launch must be the exact contract shape, got %+v", req)
	}
}

// The probe runs the fixture child through the executor and reports the
// version; the help contract assertions pass against the frozen flag
// surface.
func TestClaudeAdapter_ProbeContractE2E(t *testing.T) {
	h := newAdapterHarness(t)
	binDir := compileClaudeFixture(t)

	template := NewOperatorProbeLaunchTemplate(filepath.Join(binDir, "claude"), t.TempDir(), probeProfile())
	adp, err := NewProductionClaudeAdapter(h.store, h.wm, execpolicy.New(), template, h.configBase, h.template, h.wsRoot)
	if err != nil {
		t.Fatalf("production construction: %v", err)
	}

	report, err := adp.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !report.HarnessVersion.Available || !strings.Contains(report.HarnessVersion.Value, "2.1.278") {
		t.Fatalf("probe must report the fixture version, got %+v", report.HarnessVersion)
	}
	if report.Capabilities.SessionResumption != adapter.CapabilitySupported {
		t.Fatalf("claude supports resumption, got %+v", report.Capabilities)
	}
	if report.Capabilities.ToolApprovalRouting != adapter.CapabilityUnsupported {
		t.Fatalf("claude denies instead of routing approvals, got %+v", report.Capabilities)
	}
}

// A child whose --help surface lost a required flag is protocol drift:
// the probe fails closed.
func TestClaudeAdapter_ProbeDetectsHelpDrift(t *testing.T) {
	h := newAdapterHarness(t)

	driftDir := t.TempDir()
	script := filepath.Join(driftDir, "claude")
	body := "#!/bin/sh\ncase \"$1\" in\n  --version) echo \"9.9.9 (Claude Code)\" ;;\n  --help) echo \"Usage: claude\"; echo \"  -p, --print\" ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write drift stub: %v", err)
	}

	template := NewOperatorProbeLaunchTemplate(script, t.TempDir(), probeProfile())
	adp, err := NewProductionClaudeAdapter(h.store, h.wm, execpolicy.New(), template, h.configBase, h.template, h.wsRoot)
	if err != nil {
		t.Fatalf("production construction: %v", err)
	}

	_, err = adp.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "missing required launch flag") {
		t.Fatalf("drifted help surface must fail the probe, got %v", err)
	}
}

// Flag matching is exact-token: a decoy flag like --resume-other does
// not satisfy the required --resume. Choice sets are parsed per flag
// and compared as COMPLETE sets against the v8-verified values:
// missing, unexpected, or reassigned choices are protocol drift.
func TestClaudeAdapter_ProbeRejectsDecoyFlagsAndMissingChoices(t *testing.T) {
	h := newAdapterHarness(t)

	driftDir := t.TempDir()
	script := filepath.Join(driftDir, "claude")
	body := "#!/bin/sh\ncase \"$1\" in\n" +
		"  --version) echo \"9.9.9 (Claude Code)\" ;;\n" +
		"  --help) echo \"Usage: claude\"; " +
		"echo \"  -p, --print --output-format (choices: text, json, stream-json) --verbose --session-id --resume-other --model --max-turns --permission-mode (choices: acceptEdits, auto, bypassPermissions, manual, dontAsk, plan) --allowedTools --disallowedTools\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write decoy stub: %v", err)
	}

	template := NewOperatorProbeLaunchTemplate(script, t.TempDir(), probeProfile())
	adp, err := NewProductionClaudeAdapter(h.store, h.wm, execpolicy.New(), template, h.configBase, h.template, h.wsRoot)
	if err != nil {
		t.Fatalf("production construction: %v", err)
	}

	_, err = adp.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"--resume"`) {
		t.Fatalf("a decoy --resume-other must not satisfy --resume, got %v", err)
	}

	// A reduced permission-mode set (the old fixture text advertising
	// "default") is unexpected-and-missing at once: rejected.
	reducedDir := t.TempDir()
	script2 := filepath.Join(reducedDir, "claude")
	body2 := "#!/bin/sh\ncase \"$1\" in\n" +
		"  --version) echo \"9.9.9 (Claude Code)\" ;;\n" +
		"  --help) echo \"Usage: claude\"; " +
		"echo \"  -p, --print --output-format (choices: text, json, stream-json) --verbose --session-id --resume --model --max-turns --permission-mode (choices: default, acceptEdits, plan) --allowedTools --disallowedTools\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(script2, []byte(body2), 0o755); err != nil {
		t.Fatalf("write reduced stub: %v", err)
	}

	template2 := NewOperatorProbeLaunchTemplate(script2, t.TempDir(), probeProfile())
	adp2, err := NewProductionClaudeAdapter(h.store, h.wm, execpolicy.New(), template2, h.configBase, h.template, h.wsRoot)
	if err != nil {
		t.Fatalf("production construction: %v", err)
	}

	_, err = adp2.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--permission-mode") ||
		!strings.Contains(err.Error(), "frozen verified set") {
		t.Fatalf("reduced permission-mode choices must fail the probe, got %v", err)
	}

	// A reassigned choice set — output-format advertising permission
	// choices — is rejected even though every value exists somewhere.
	reassignedDir := t.TempDir()
	script3 := filepath.Join(reassignedDir, "claude")
	body3 := "#!/bin/sh\ncase \"$1\" in\n" +
		"  --version) echo \"9.9.9 (Claude Code)\" ;;\n" +
		"  --help) echo \"Usage: claude\"; " +
		"echo \"  -p, --print --output-format (choices: acceptEdits, auto, bypassPermissions, manual, dontAsk, plan) --verbose --session-id --resume --model --max-turns --permission-mode (choices: acceptEdits, auto, bypassPermissions, manual, dontAsk, plan) --allowedTools --disallowedTools\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(script3, []byte(body3), 0o755); err != nil {
		t.Fatalf("write reassigned stub: %v", err)
	}

	template3 := NewOperatorProbeLaunchTemplate(script3, t.TempDir(), probeProfile())
	adp3, err := NewProductionClaudeAdapter(h.store, h.wm, execpolicy.New(), template3, h.configBase, h.template, h.wsRoot)
	if err != nil {
		t.Fatalf("production construction: %v", err)
	}

	_, err = adp3.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--output-format") ||
		!strings.Contains(err.Error(), "frozen verified set") {
		t.Fatalf("reassigned output-format choices must fail the probe, got %v", err)
	}
}
