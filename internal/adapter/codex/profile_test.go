package codex

// The codex adapter requires cprof-v3 with a complete frozen codex
// harness block; anything else fails closed with the typed
// ErrUnsupportedProfile (AC-009 spec §3.8 compatibility matrix).

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func v3CodexProfile() storage.CanonicalProfile {
	p := storage.CanonicalProfile{
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
					ExpectedCodexHome: "/home/op/.codex",
					Platform:          storage.CodexPlatformSpec{OS: "linux", Family: "unix"},
					SandboxPolicy: storage.CodexSandboxPolicySpec{
						Type:          "workspace-write",
						WritableRoots: []string{"/home/op/ws"},
						NetworkAccess: false,
					},
					ApprovalPolicy:    storage.CodexApprovalPolicy{Kind: "string", String: "on-request"},
					ApprovalsReviewer: "user",
					// The fixture child answers mcpServerStatus/list with
					// {"servers":[]} (the .codex-fixture-mcp knob
					// overrides it for inventory drift/match evidence).
					ExpectedMCPServers:         []string{},
					ExpectedMCPTools:           []string{},
					ExpectedPluginTools:        []string{},
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
	p.ToolkitManifest = &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
		ProbedCLIVersion:       "2.1.278",
		UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
		UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
		ApprovedTools:          []string{"Read", "Glob"},
		DeniedComplement:       []string{"Bash"},
		ExpectedHooks:          []string{"SessionStart:startup"},
		TurnsBound:             8,
	}}
	return p
}

// evidenceRootForCodex writes the frozen event universe into a temp
// evidence root and pins its digest on the profile.
func evidenceRootForCodex(t *testing.T, p storage.CanonicalProfile) (storage.CanonicalProfile, string) {
	t.Helper()
	universe := map[string]any{"codex_cli_version": "0.154.0", "methods": []string{"initialize", "thread/start"}}
	raw, err := json.Marshal(universe)
	if err != nil {
		t.Fatalf("marshal universe: %v", err)
	}
	root := t.TempDir()
	rel := p.Harnesses["codex"].Codex.EventUniversePath
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, raw, 0o600); err != nil {
		t.Fatalf("write universe: %v", err)
	}
	p.Harnesses["codex"].Codex.EventUniverseDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	return p, root
}

// A profile that is not cprof-v3 — or a v3 profile without a complete
// codex harness block — is rejected with the typed ErrUnsupportedProfile.
func TestValidateCodexHarness_RequiresV3WithCompleteBlock(t *testing.T) {
	p, root := evidenceRootForCodex(t, v3CodexProfile())

	if _, err := ValidateCodexHarness(p, root); err != nil {
		t.Fatalf("complete v3 profile must validate: %v", err)
	}

	for _, version := range []string{"cprof-v1", "cprof-v2"} {
		legacy := p
		legacy.AlgoVersion = version
		_, err := ValidateCodexHarness(legacy, root)
		var unsupported *ErrUnsupportedProfile
		if err == nil || !errors.As(err, &unsupported) {
			t.Fatalf("%s must fail with typed ErrUnsupportedProfile, got %T: %v", version, err, err)
		}
	}

	noBlock := p
	noBlock.Harnesses = map[string]storage.HarnessProfileSpec{
		"codex": {Model: "gpt-5.6-sol", NativeAuthMode: "inherited_codex_home"},
	}
	_, err := ValidateCodexHarness(noBlock, root)
	var unsupported *ErrUnsupportedProfile
	if err == nil || !errors.As(err, &unsupported) {
		t.Fatalf("v3 without the codex block must fail typed, got %T: %v", err, err)
	}

	noCodexHarness := p
	noCodexHarness.Harnesses = map[string]storage.HarnessProfileSpec{
		"claude": {Model: "m", NativeAuthMode: "inherited_host_keychain"},
	}
	if _, err := ValidateCodexHarness(noCodexHarness, root); err == nil {
		t.Fatal("v3 without a codex harness entry must be rejected")
	}
}

// Every required field of the codex block is checked; blanks and wrong
// values fail closed.
func TestValidateCodexHarness_RequiresCompleteBlock(t *testing.T) {
	mutations := map[string]func(c *storage.CodexHarnessSpec){
		"app_server_version":  func(c *storage.CodexHarnessSpec) { c.AppServerVersion = "" },
		"model_provider":      func(c *storage.CodexHarnessSpec) { c.ModelProvider = "" },
		"expected_codex_home": func(c *storage.CodexHarnessSpec) { c.ExpectedCodexHome = "" },
		"platform_os":         func(c *storage.CodexHarnessSpec) { c.Platform.OS = "" },
		"platform_family":     func(c *storage.CodexHarnessSpec) { c.Platform.Family = "" },
		"sandbox_type":        func(c *storage.CodexHarnessSpec) { c.SandboxPolicy.Type = "" },
		"writable_roots":      func(c *storage.CodexHarnessSpec) { c.SandboxPolicy.WritableRoots = nil },
		"approval_policy":     func(c *storage.CodexHarnessSpec) { c.ApprovalPolicy = storage.CodexApprovalPolicy{} },
		"reviewer":            func(c *storage.CodexHarnessSpec) { c.ApprovalsReviewer = "auto_review" },
		"mcp_servers":         func(c *storage.CodexHarnessSpec) { c.ExpectedMCPServers = nil },
		"mcp_tools":           func(c *storage.CodexHarnessSpec) { c.ExpectedMCPTools = nil },
		"mcp_tool_shape":      func(c *storage.CodexHarnessSpec) { c.ExpectedMCPTools = []string{"no-slash"} },
		"mcp_tool_server": func(c *storage.CodexHarnessSpec) {
			c.ExpectedMCPServers = []string{"context7"}
			c.ExpectedMCPTools = []string{"rogue/read"}
		},
		"plugin_tools":        func(c *storage.CodexHarnessSpec) { c.ExpectedPluginTools = nil },
		"plugin_tool_empty":   func(c *storage.CodexHarnessSpec) { c.ExpectedPluginTools = []string{" "} },
		"instruction_sources": func(c *storage.CodexHarnessSpec) { c.ExpectedInstructionSources = nil },
		"rules_evidence":      func(c *storage.CodexHarnessSpec) { c.RulesEvidence = storage.CodexRulesEvidenceSpec{} },
		"universe_path":       func(c *storage.CodexHarnessSpec) { c.EventUniversePath = "" },
		"universe_digest":     func(c *storage.CodexHarnessSpec) { c.EventUniverseDigest = "not-a-digest" },
	}
	for name, mutate := range mutations {
		q, r := evidenceRootForCodex(t, v3CodexProfile())
		mutate(q.Harnesses["codex"].Codex)
		if _, err := ValidateCodexHarness(q, r); err == nil {
			t.Fatalf("incomplete codex block (%s) must be rejected", name)
		} else {
			var unsupported *ErrUnsupportedProfile
			if !errors.As(err, &unsupported) {
				t.Fatalf("incomplete codex block (%s) must fail typed, got %T: %v", name, err, err)
			}
		}
	}
}

// An in-memory tagged union with the granular kind but no granular
// object fails closed with the typed error, not a nil dereference.
func TestValidateCodexHarness_GranularNilObjectRejectedTyped(t *testing.T) {
	p, root := evidenceRootForCodex(t, v3CodexProfile())
	p.Harnesses["codex"].Codex.ApprovalPolicy = storage.CodexApprovalPolicy{Kind: "granular", Granular: nil}
	_, err := ValidateCodexHarness(p, root)
	var unsupported *ErrUnsupportedProfile
	if err == nil || !errors.As(err, &unsupported) {
		t.Fatalf("nil granular object must fail with typed ErrUnsupportedProfile, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "granular") {
		t.Fatalf("rejection must name the missing granular object, got %v", err)
	}
}

// The sandbox_policy.type enum is a freeze gate (AC-009 §5): only
// read-only and workspace-write are launchable; danger-full-access —
// which would otherwise flow verbatim into every turn/start pin and
// self-confirm in the echo compares — and any unknown value are
// rejected with the typed error before any child starts.
func TestValidateCodexHarness_SandboxTypeEnumRejected(t *testing.T) {
	for _, ok := range []string{"read-only", "workspace-write"} {
		p, root := evidenceRootForCodex(t, v3CodexProfile())
		p.Harnesses["codex"].Codex.SandboxPolicy.Type = ok
		if _, err := ValidateCodexHarness(p, root); err != nil {
			t.Fatalf("sandbox_policy type %q must validate: %v", ok, err)
		}
	}
	for _, bad := range []string{"danger-full-access", "yolo-full-access", "read_only", "Read-Only"} {
		p, root := evidenceRootForCodex(t, v3CodexProfile())
		p.Harnesses["codex"].Codex.SandboxPolicy.Type = bad
		_, err := ValidateCodexHarness(p, root)
		var unsupported *ErrUnsupportedProfile
		if err == nil || !errors.As(err, &unsupported) {
			t.Fatalf("sandbox_policy type %q must be rejected with typed ErrUnsupportedProfile, got %T: %v", bad, err, err)
		}
		if !strings.Contains(err.Error(), "sandbox_policy") {
			t.Fatalf("rejection must name sandbox_policy, got %v", err)
		}
	}
}

// The event universe file is re-hashed at validation and must match the
// frozen digest; a valid profile yields the launch policy with the
// frozen values, including the canonical approval-policy encoding.
func TestValidateCodexHarness_RehashesEventUniverse(t *testing.T) {
	p, root := evidenceRootForCodex(t, v3CodexProfile())

	policy, err := ValidateCodexHarness(p, root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if policy.AppServerVersion != "0.154.0" || policy.ModelProvider != "openai" ||
		policy.ExpectedCodexHome != "/home/op/.codex" || policy.PlatformOS != "linux" ||
		policy.PlatformFamily != "unix" || policy.SandboxType != "workspace-write" ||
		policy.NetworkAccess {
		t.Fatalf("launch policy lost frozen values: %+v", policy)
	}
	if len(policy.WritableRoots) != 1 || policy.WritableRoots[0] != "/home/op/ws" {
		t.Fatalf("writable roots, got %v", policy.WritableRoots)
	}
	if policy.ApprovalPolicyCanonical != `"on-request"` {
		t.Fatalf("canonical approval policy, got %q", policy.ApprovalPolicyCanonical)
	}
	if policy.ApprovalsReviewer != "user" || policy.EventUniversePath != p.Harnesses["codex"].Codex.EventUniversePath {
		t.Fatalf("reviewer/universe path, got %+v", policy)
	}

	// Digest drift is a hard failure.
	drifted, _ := evidenceRootForCodex(t, v3CodexProfile())
	drifted.Harnesses["codex"].Codex.EventUniverseDigest = "sha256:" + strings.Repeat("b", 64)
	if _, err := ValidateCodexHarness(drifted, root); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("universe digest drift must be rejected, got %v", err)
	}

	// A missing universe file is a hard failure.
	missing, _ := evidenceRootForCodex(t, v3CodexProfile())
	missing.Harnesses["codex"].Codex.EventUniversePath = "docs/superpowers/evidence/absent-universe.json"
	if _, err := ValidateCodexHarness(missing, root); err == nil {
		t.Fatal("missing universe file must be rejected")
	}
}
