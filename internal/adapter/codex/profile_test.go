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

// stageToolInventory writes the Council-shaped native tool-inventory
// capture under the evidence root and pins the profile to it.
func stageToolInventory(t *testing.T, p storage.CanonicalProfile, root string, inv NativeToolInventory) storage.CanonicalProfile {
	t.Helper()
	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	rel := "docs/superpowers/evidence/ac009-native-tool-inventory-0.154.0.json"
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, raw, 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	p.Harnesses["codex"].Codex.ToolInventoryPath = rel
	p.Harnesses["codex"].Codex.ToolInventoryDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	return p
}

// Inventory-evidence gate: no committed evidence path proves a
// non-empty native tool inventory (the mcpServerStatus/list shape and
// the plugin inventory surface are not schema-pinned), so a profile
// enabling ANY MCP server or plugin tool is not launchable — even with
// a digest-bound, agreeing Council-shaped capture, which is operator-
// edited JSON rather than native evidence. Empty inventories proceed.
func TestValidateCodexHarness_NonEmptyInventoriesAreNotLaunchable(t *testing.T) {
	cases := map[string]func(c *storage.CodexHarnessSpec){
		"mcp server": func(c *storage.CodexHarnessSpec) { c.ExpectedMCPServers = []string{"context7"} },
		"mcp tool": func(c *storage.CodexHarnessSpec) {
			c.ExpectedMCPServers = []string{"context7"}
			c.ExpectedMCPTools = []string{"context7/toolA"}
		},
		"plugin tool": func(c *storage.CodexHarnessSpec) { c.ExpectedPluginTools = []string{"skill:review"} },
	}
	for name, enable := range cases {
		t.Run(name, func(t *testing.T) {
			p, root := evidenceRootForCodex(t, v3CodexProfile())
			c := p.Harnesses["codex"].Codex
			enable(c)
			inv := NativeToolInventory{CodexCLIVersion: "0.154.0", MCPServers: map[string][]string{},
				PluginTools: append([]string{}, c.ExpectedPluginTools...)}
			for _, srv := range c.ExpectedMCPServers {
				inv.MCPServers[srv] = []string{}
			}
			for _, path := range c.ExpectedMCPTools {
				srv, tool, _ := strings.Cut(path, "/")
				inv.MCPServers[srv] = append(inv.MCPServers[srv], tool)
			}
			p = stageToolInventory(t, p, root, inv)
			_, err := ValidateCodexHarness(p, root)
			var unsupported *ErrUnsupportedProfile
			if err == nil || !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "only empty inventories are eligible") {
				t.Fatalf("a %s-enabled profile must be refused even with an agreeing capture, got %T: %v", name, err, err)
			}
		})
	}
	// Empty inventories: no capture needed; a present capture must agree
	// (one exposing servers the profile hides is refused); an agreeing
	// empty capture validates.
	p, root := evidenceRootForCodex(t, v3CodexProfile())
	if _, err := ValidateCodexHarness(p, root); err != nil {
		t.Fatalf("a profile enabling nothing needs no inventory evidence: %v", err)
	}
	p, root = evidenceRootForCodex(t, v3CodexProfile())
	p = stageToolInventory(t, p, root, NativeToolInventory{CodexCLIVersion: "0.154.0",
		MCPServers: map[string][]string{"fs": {"read"}}, PluginTools: []string{}})
	if _, err := ValidateCodexHarness(p, root); err == nil || !strings.Contains(err.Error(), "expected_mcp_servers") {
		t.Fatalf("an inventory exposing servers the profile hides must be refused, got %v", err)
	}
	p, root = evidenceRootForCodex(t, v3CodexProfile())
	p = stageToolInventory(t, p, root, NativeToolInventory{CodexCLIVersion: "0.154.0",
		MCPServers: map[string][]string{}, PluginTools: []string{}})
	if _, err := ValidateCodexHarness(p, root); err != nil {
		t.Fatalf("an empty capture agreeing with an empty profile must validate: %v", err)
	}
	// Duplicate frozen inventory entries are rejected at validation, not
	// canonicalized away (they are also unlaunchable, but the shape error
	// comes first).
	for name, mutate := range map[string]func(c *storage.CodexHarnessSpec){
		"mcp tool": func(c *storage.CodexHarnessSpec) {
			c.ExpectedMCPServers = []string{"context7"}
			c.ExpectedMCPTools = []string{"context7/a", "context7/a"}
		},
		"plugin tool": func(c *storage.CodexHarnessSpec) { c.ExpectedPluginTools = []string{"skill:review", " skill:review"} },
	} {
		p, root := evidenceRootForCodex(t, v3CodexProfile())
		mutate(p.Harnesses["codex"].Codex)
		if _, err := ValidateCodexHarness(p, root); err == nil || !strings.Contains(err.Error(), "is duplicated") {
			t.Fatalf("a duplicated %s entry must be refused, got %v", name, err)
		}
	}
}

// The equality rule the eventual evidence path is held to
// (verifyToolInventoryEvidence, unit-tested directly because the freeze
// gate refuses non-empty inventories before reaching it): the capture
// must re-hash, be for the pinned version, decode strictly, and its
// server, tool, and plugin sets must equal the frozen lists exactly.
func TestVerifyToolInventoryEvidence_EqualityAndStrictness(t *testing.T) {
	enabled := func() (storage.CanonicalProfile, string) {
		p, root := evidenceRootForCodex(t, v3CodexProfile())
		c := p.Harnesses["codex"].Codex
		c.ExpectedMCPServers = []string{"context7"}
		c.ExpectedMCPTools = []string{"context7/toolA", "context7/toolB"}
		c.ExpectedPluginTools = []string{"skill:review"}
		return p, root
	}
	full := NativeToolInventory{
		CodexCLIVersion: "0.154.0",
		MCPServers:      map[string][]string{"context7": {"toolB", "toolA"}},
		PluginTools:     []string{"skill:review"},
	}
	verify := func(p storage.CanonicalProfile, root string) error {
		return verifyToolInventoryEvidence(root, p.Harnesses["codex"].Codex)
	}
	writeRaw := func(p storage.CanonicalProfile, root string, raw []byte) storage.CanonicalProfile {
		rel := "docs/superpowers/evidence/ac009-native-tool-inventory-0.154.0.json"
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, raw, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		p.Harnesses["codex"].Codex.ToolInventoryPath = rel
		p.Harnesses["codex"].Codex.ToolInventoryDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
		return p
	}

	p, root := enabled()
	if err := verify(p, root); err == nil || !strings.Contains(err.Error(), "no tool_inventory_path/tool_inventory_digest") {
		t.Fatalf("enabled without evidence must be refused, got %v", err)
	}
	p, root = enabled()
	if err := verify(stageToolInventory(t, p, root, full), root); err != nil {
		t.Fatalf("matching evidence must verify: %v", err)
	}
	p, root = enabled()
	p.Harnesses["codex"].Codex.ExpectedMCPTools = []string{"context7/toolA"}
	if err := verify(stageToolInventory(t, p, root, full), root); err == nil || !strings.Contains(err.Error(), "expected_mcp_tools") {
		t.Fatalf("omitting a native tool must be refused, got %v", err)
	}
	p, root = enabled()
	p.Harnesses["codex"].Codex.ExpectedPluginTools = []string{}
	if err := verify(stageToolInventory(t, p, root, full), root); err == nil || !strings.Contains(err.Error(), "expected_plugin_tools") {
		t.Fatalf("omitting a plugin tool must be refused, got %v", err)
	}
	p, root = enabled()
	extra := full
	extra.MCPServers = map[string][]string{"context7": {"toolA", "toolB"}, "fs": {}}
	if err := verify(stageToolInventory(t, p, root, extra), root); err == nil || !strings.Contains(err.Error(), "expected_mcp_servers") {
		t.Fatalf("an unlisted native server must be refused, got %v", err)
	}
	p, root = enabled()
	stale := full
	stale.CodexCLIVersion = "0.153.0"
	if err := verify(stageToolInventory(t, p, root, stale), root); err == nil || !strings.Contains(err.Error(), "is for codex") {
		t.Fatalf("a capture for another version must be refused, got %v", err)
	}
	p, root = enabled()
	p = stageToolInventory(t, p, root, full)
	p.Harnesses["codex"].Codex.ToolInventoryDigest = "sha256:" + strings.Repeat("0", 64)
	if err := verify(p, root); err == nil || !strings.Contains(err.Error(), "tool inventory digest mismatch") {
		t.Fatalf("a digest mismatch must be refused, got %v", err)
	}
	p, root = enabled()
	p = stageToolInventory(t, p, root, full)
	p.Harnesses["codex"].Codex.ToolInventoryDigest = ""
	if err := verify(p, root); err == nil || !strings.Contains(err.Error(), "must both be set") {
		t.Fatalf("a half-set binding must be refused, got %v", err)
	}

	// Strict decoding: one unambiguous canonical meaning per file.
	base := `{"codex_cli_version":"0.154.0","mcp_servers":{"context7":["toolA","toolB"]},"plugin_tools":["skill:review"]}`
	for name, tc := range map[string]struct{ raw, want string }{
		"unknown key":              {`{"codex_cli_version":"0.154.0","mcp_servers":{"context7":["toolA","toolB"]},"plugin_tools":["skill:review"],"extra":1}`, "unknown key"},
		"trailing value":           {base + `{}`, "trailing content"},
		"trailing garbage":         {base + ` x`, "trailing content"},
		"duplicate top-level key":  {`{"codex_cli_version":"0.154.0","codex_cli_version":"0.154.0","mcp_servers":{"context7":["toolA","toolB"]},"plugin_tools":["skill:review"]}`, "is duplicated"},
		"duplicate server key":     {`{"codex_cli_version":"0.154.0","mcp_servers":{"context7":["toolA"],"context7":["toolB"]},"plugin_tools":["skill:review"]}`, "server \"context7\" is duplicated"},
		"duplicate tool in server": {`{"codex_cli_version":"0.154.0","mcp_servers":{"context7":["toolA","toolB","toolA"]},"plugin_tools":["skill:review"]}`, "entry \"toolA\" is duplicated"},
		"duplicate plugin":         {`{"codex_cli_version":"0.154.0","mcp_servers":{"context7":["toolA","toolB"]},"plugin_tools":["skill:review","skill:review"]}`, "entry \"skill:review\" is duplicated"},
		"empty tool name":          {`{"codex_cli_version":"0.154.0","mcp_servers":{"context7":["toolA"," "]},"plugin_tools":["skill:review"]}`, "must not be empty"},
		"missing key":              {`{"codex_cli_version":"0.154.0","mcp_servers":{"context7":["toolA","toolB"]}}`, "missing key"},
		"not an object":            {`[]`, "expected"},
		"wrong value type":         {`{"codex_cli_version":1,"mcp_servers":{},"plugin_tools":[]}`, "must be a string"},
	} {
		t.Run(name, func(t *testing.T) {
			p, root := enabled()
			p = writeRaw(p, root, []byte(tc.raw))
			if err := verify(p, root); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
	p, root = enabled()
	if err := verify(writeRaw(p, root, []byte(base)), root); err != nil {
		t.Fatalf("the canonical capture must verify: %v", err)
	}
}
