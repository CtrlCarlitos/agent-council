package storage

// cprof-v4: additive agy harness block, frozen per AC-010 spec §3.7.
// v1/v2/v3 encodings must remain byte-identical; the agy block is legal
// only under cprof-v4; when a codex block is also present under v4 it
// is validated exactly as v3 does.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func v4AgyBlock() *AgyHarnessSpec {
	return &AgyHarnessSpec{
		CLIVersion:                  "1.2.9",
		BinaryPath:                  "/home/op/.local/bin/agy",
		BinaryDigest:                "sha256:" + strings.Repeat("c", 64),
		ExpectedHome:                "/home/op/.gemini",
		Platform:                    AgyPlatformSpec{OS: "linux", Family: "unix"},
		PermissionMode:              "request-review",
		ExecutionMode:               "default",
		Sandbox:                     true,
		PrintTimeoutBackstopSeconds: 1800,
		ExpectedTools:               []string{"ask_permission", "run_command", "view_file"},
		ExpectedMCPServers:          []string{},
		ExpectedMCPTools:            []string{},
		ExpectedPluginTools:         []string{},
		DefaultRequiredTools:        []string{},
		HooksEvidence: AgyHooksEvidenceSpec{
			Verified:     []string{"guardrail hook present"},
			Unverifiable: []string{"hook execution at the Agy layer", "settings.json permissions.allow contents"},
		},
		ExpectedSkills:        []string{"research"},
		PluginsEvidencePath:   "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json",
		PluginsEvidenceDigest: "sha256:e44331c4cf02210fba9fa4c5223bce59a5659570e667b07721106cbdb2028ae7",
		HooksConfigDigest:     "sha256:" + strings.Repeat("d", 64),
		RequiredHooks:         []string{"/guardrail"},
		InitEvidencePath:      "docs/superpowers/evidence/ac010-agy-init-1.2.9.json",
		InitEvidenceDigest:    "sha256:071ffa3332db26c3b99706891af004344b437ed921ff8baf96f95c5d54889359",
		ToolCoveragePath:      "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json",
		ToolCoverageDigest:    "sha256:eb33c3ac04646a3485c84ef240939354af57ba06678d6f8ad44057fea682b563",
	}
}

func v4Profile() CanonicalProfile {
	p := CanonicalProfile{
		AlgoVersion:         "cprof-v4",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"agy", "git", "go"},
		Harnesses: map[string]HarnessProfileSpec{
			"agy": {Model: "gpt-oss-120b-medium", NativeAuthMode: "inherited_gemini_home", Agy: v4AgyBlock()},
		},
	}
	p.ToolkitManifest = &ToolkitManifestSpec{ToolkitManifest: v2Manifest()}
	return p
}

// Golden digest vector: the canonical v4 encoding is pinned byte-for-
// byte, computed independently with python3
// (json.dumps(doc, sort_keys=True, separators=(",",":"),
// ensure_ascii=False) over the equivalent structure), and the digest is
// sha256 over exactly those bytes.
func TestCanonicalProfileV4_GoldenDigestVector(t *testing.T) {
	p := v4Profile()
	digest, canon, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("v4 digest: %v", err)
	}

	golden := `{"algo_version":"cprof-v4","code_index_scope":[],"harnesses":{"agy":{"agy":{"binary_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","binary_path":"/home/op/.local/bin/agy","cli_version":"1.2.9","default_required_tools":[],"execution_mode":"default","expected_home":"/home/op/.gemini","expected_mcp_servers":[],"expected_mcp_tools":[],"expected_plugin_tools":[],"expected_skills":["research"],"expected_tools":["ask_permission","run_command","view_file"],"hooks_config_digest":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hooks_evidence":{"unverifiable":["hook execution at the Agy layer","settings.json permissions.allow contents"],"verified":["guardrail hook present"]},"init_evidence_digest":"sha256:071ffa3332db26c3b99706891af004344b437ed921ff8baf96f95c5d54889359","init_evidence_path":"docs/superpowers/evidence/ac010-agy-init-1.2.9.json","permission_mode":"request-review","platform":{"family":"unix","os":"linux"},"plugins_evidence_digest":"sha256:e44331c4cf02210fba9fa4c5223bce59a5659570e667b07721106cbdb2028ae7","plugins_evidence_path":"docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json","print_timeout_backstop_seconds":1800,"required_hooks":["/guardrail"],"sandbox":true,"tool_coverage_digest":"sha256:eb33c3ac04646a3485c84ef240939354af57ba06678d6f8ad44057fea682b563","tool_coverage_path":"docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json"},"extra_env_allowlist":[],"model":"gpt-oss-120b-medium","native_auth_mode":"inherited_gemini_home"}},"isolation_strictness":"permissive_dev","network_allowlist":[],"network_mode":"unrestricted","tooling":["agy","git","go"],"toolkit_manifest":{"approved_tools":["Glob","Grep","Read"],"denied_complement":["Bash","Write"],"expected_hooks":["PreToolUse","SessionStart:startup"],"expected_plugins":["superpowers"],"expected_skills":["research"],"probed_cli_version":"2.1.278","turns_bound":8,"universe_evidence_digest":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","universe_evidence_path":"docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json"},"workspace_mode":"none"}`

	if string(canon) != golden {
		t.Fatalf("canonical v4 JSON drifted from the golden vector:\n got: %s\nwant: %s", canon, golden)
	}
	sum := sha256.Sum256([]byte(golden))
	want := fmt.Sprintf("cprof-v4:sha256:%x", sum)
	if digest != want {
		t.Fatalf("v4 digest must be sha256 over the golden bytes: got %q want %q", digest, want)
	}
	// Independently pinned: python3 -c "import json,hashlib; ..." over the
	// equivalent structure produced
	// cprof-v4:sha256:e980c56880c53939bb644abd1c4cf01875757b7dab0f66ce16882989df2c9b69
	const independentlyComputed = "cprof-v4:sha256:e980c56880c53939bb644abd1c4cf01875757b7dab0f66ce16882989df2c9b69"
	if digest != independentlyComputed {
		t.Fatalf("v4 digest must match the value computed independently with python3: got %q want %q", digest, independentlyComputed)
	}
}

// v4 requires the toolkit manifest (inherits the v2/v3 rule).
func TestCanonicalProfileV4_RequiresToolkitManifest(t *testing.T) {
	p := v4Profile()
	p.ToolkitManifest = nil
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("cprof-v4 without toolkit_manifest must be rejected")
	}
	p.ToolkitManifest = &ToolkitManifestSpec{ToolkitManifest: v2Manifest()}
	p.ToolkitManifest.ToolkitManifest.TurnsBound = 0
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("cprof-v4 with an incomplete manifest must be rejected")
	}
}

// An agy block under cprof-v1/v2/v3 is a validation error, at freeze
// and at parse.
func TestCanonicalProfileV4_RejectsAgyBlockUnderEarlierVersions(t *testing.T) {
	for _, v := range []string{"cprof-v1", "cprof-v2", "cprof-v3"} {
		p := v4Profile()
		p.AlgoVersion = v
		if v == "cprof-v1" {
			p.ToolkitManifest = nil
		}
		if _, _, err := ComputeProfileDigest(p); err == nil {
			t.Fatalf("agy block under %s must be rejected at freeze", v)
		}

		// And at parse: a minimally-shaped (empty-object) agy block is
		// enough to set harnesses.agy.agy non-nil; validateNoAgyBlocks
		// rejects its mere presence regardless of content.
		canon := map[string]any{
			"algo_version":         v,
			"code_index_scope":     []any{},
			"harnesses":            map[string]any{"agy": map[string]any{"extra_env_allowlist": []any{}, "model": "m", "native_auth_mode": "a", "agy": map[string]any{}}},
			"isolation_strictness": "permissive_dev",
			"network_allowlist":    []any{},
			"network_mode":         "unrestricted",
			"tooling":              []any{"git"},
			"workspace_mode":       "none",
		}
		if v == "cprof-v2" || v == "cprof-v3" {
			canon["toolkit_manifest"] = map[string]any{
				"probed_cli_version": "2.1.278", "universe_evidence_path": "docs/superpowers/evidence/u.json",
				"universe_evidence_digest": "sha256:" + strings.Repeat("a", 64),
				"approved_tools":           []any{"Read"}, "denied_complement": []any{},
				"expected_hooks": []any{}, "expected_skills": []any{}, "expected_plugins": []any{},
				"turns_bound": 8,
			}
		}
		raw, err := json.Marshal(canon)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := ParseCanonicalProfileJSON(raw); err == nil {
			t.Fatalf("agy block under %s must be rejected at parse", v)
		}
	}
}

// A codex block under cprof-v4 is validated exactly as v3: the same
// gates (sandbox type enum, approvals_reviewer, inventory gate) apply.
func TestCanonicalProfileV4_CodexBlockValidatedAsV3(t *testing.T) {
	p := v4Profile()
	p.Harnesses["codex"] = HarnessProfileSpec{Model: "gpt-5.6-sol", NativeAuthMode: "inherited_codex_home", Codex: v3CodexBlock()}
	if _, _, err := ComputeProfileDigest(p); err != nil {
		t.Fatalf("v4 profile with a complete codex block must freeze: %v", err)
	}
	bad := v4Profile()
	badCodex := v3CodexBlock()
	badCodex.SandboxPolicy.Type = "danger-full-access"
	bad.Harnesses["codex"] = HarnessProfileSpec{Model: "gpt-5.6-sol", NativeAuthMode: "inherited_codex_home", Codex: badCodex}
	if _, _, err := ComputeProfileDigest(bad); err == nil {
		t.Fatal("v4 profile with an invalid codex block must be rejected exactly as v3 rejects it")
	}
}

// A v4 profile with no agy block at all still freezes: one run profile
// serves all four harnesses, and a v4 run without an Agy contributor
// remains valid.
func TestCanonicalProfileV4_AgyBlockOptionalUnderV4(t *testing.T) {
	p := v4Profile()
	p.Harnesses["agy"] = HarnessProfileSpec{Model: "gpt-oss-120b-medium", NativeAuthMode: "inherited_gemini_home"}
	if _, _, err := ComputeProfileDigest(p); err != nil {
		t.Fatalf("v4 profile without an agy block must still freeze: %v", err)
	}
}

// permission_mode and execution_mode enums at freeze: no bypass value
// is representable.
func TestCanonicalProfileV4_PermissionAndExecutionModeEnums(t *testing.T) {
	for _, ok := range []string{"request-review", "strict"} {
		p := v4Profile()
		p.Harnesses["agy"].Agy.PermissionMode = ok
		if _, _, err := ComputeProfileDigest(p); err != nil {
			t.Fatalf("permission_mode %q must freeze: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "always-proceed", "proceed-in-sandbox", "yolo"} {
		p := v4Profile()
		p.Harnesses["agy"].Agy.PermissionMode = bad
		if _, _, err := ComputeProfileDigest(p); err == nil {
			t.Fatalf("permission_mode %q must be rejected at freeze (no bypass value representable)", bad)
		}
	}
	for _, ok := range []string{"default", "accept-edits", "plan"} {
		p := v4Profile()
		p.Harnesses["agy"].Agy.ExecutionMode = ok
		if _, _, err := ComputeProfileDigest(p); err != nil {
			t.Fatalf("execution_mode %q must freeze: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "yolo", "auto"} {
		p := v4Profile()
		p.Harnesses["agy"].Agy.ExecutionMode = bad
		if _, _, err := ComputeProfileDigest(p); err == nil {
			t.Fatalf("execution_mode %q must be rejected at freeze", bad)
		}
	}
}

// print_timeout_backstop_seconds must be >= 60.
func TestCanonicalProfileV4_PrintTimeoutBackstopMinimum(t *testing.T) {
	p := v4Profile()
	p.Harnesses["agy"].Agy.PrintTimeoutBackstopSeconds = 60
	if _, _, err := ComputeProfileDigest(p); err != nil {
		t.Fatalf("60 seconds must freeze: %v", err)
	}
	p2 := v4Profile()
	p2.Harnesses["agy"].Agy.PrintTimeoutBackstopSeconds = 59
	if _, _, err := ComputeProfileDigest(p2); err == nil {
		t.Fatal("59 seconds must be rejected at freeze")
	}
}

// Inventory-evidence gate (spec §3.7, AC-009 §3.8 errata applied
// verbatim): no committed evidence path proves a non-empty native
// MCP/plugin tool inventory, so a run enabling any must never durably
// freeze.
func TestCanonicalProfileV4_NonEmptyInventoriesRejectedAtFreeze(t *testing.T) {
	cases := map[string]func(a *AgyHarnessSpec){
		"mcp server": func(a *AgyHarnessSpec) { a.ExpectedMCPServers = []string{"context7"} },
		"mcp tool": func(a *AgyHarnessSpec) {
			a.ExpectedMCPServers = []string{"context7"}
			a.ExpectedMCPTools = []string{"context7/toolA"}
		},
		"plugin tool": func(a *AgyHarnessSpec) { a.ExpectedPluginTools = []string{"skill:review"} },
	}
	for name, enable := range cases {
		t.Run(name, func(t *testing.T) {
			p := v4Profile()
			enable(p.Harnesses["agy"].Agy)
			if _, _, err := ComputeProfileDigest(p); err == nil || !strings.Contains(err.Error(), "must be empty") {
				t.Fatalf("a %s-enabled profile must be rejected at freeze, got %v", name, err)
			}
		})
	}
	if _, _, err := ComputeProfileDigest(v4Profile()); err != nil {
		t.Fatalf("empty inventories must freeze: %v", err)
	}
}

// default_required_tools must be a subset of expected_tools.
func TestCanonicalProfileV4_DefaultRequiredToolsMustBeSubset(t *testing.T) {
	p := v4Profile()
	p.Harnesses["agy"].Agy.DefaultRequiredTools = []string{"run_command"}
	if _, _, err := ComputeProfileDigest(p); err != nil {
		t.Fatalf("default_required_tools ⊆ expected_tools must freeze: %v", err)
	}
	p2 := v4Profile()
	p2.Harnesses["agy"].Agy.DefaultRequiredTools = []string{"not_a_tool"}
	if _, _, err := ComputeProfileDigest(p2); err == nil {
		t.Fatal("default_required_tools outside expected_tools must be rejected")
	}
}

// expected_skills entries must be bare directory names (no separators),
// duplicates rejected.
func TestCanonicalProfileV4_ExpectedSkillsShape(t *testing.T) {
	p := v4Profile()
	p.Harnesses["agy"].Agy.ExpectedSkills = []string{"research", "research"}
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("duplicate expected_skills entries must be rejected")
	}
	p2 := v4Profile()
	p2.Harnesses["agy"].Agy.ExpectedSkills = []string{"nested/skill"}
	if _, _, err := ComputeProfileDigest(p2); err == nil {
		t.Fatal("expected_skills with a separator must be rejected")
	}
}

// required_hooks: non-empty, sorted, duplicates rejected, each an RFC
// 6901 JSON pointer.
func TestCanonicalProfileV4_RequiredHooksShape(t *testing.T) {
	p := v4Profile()
	p.Harnesses["agy"].Agy.RequiredHooks = []string{}
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("empty required_hooks must be rejected")
	}
	p2 := v4Profile()
	p2.Harnesses["agy"].Agy.RequiredHooks = []string{"guardrail"}
	if _, _, err := ComputeProfileDigest(p2); err == nil {
		t.Fatal("required_hooks entry without a leading slash must be rejected")
	}
	p3 := v4Profile()
	p3.Harnesses["agy"].Agy.RequiredHooks = []string{"/z", "/a"}
	if _, _, err := ComputeProfileDigest(p3); err == nil {
		t.Fatal("unsorted required_hooks must be rejected")
	}
	p4 := v4Profile()
	p4.Harnesses["agy"].Agy.RequiredHooks = []string{"/guardrail", "/guardrail"}
	if _, _, err := ComputeProfileDigest(p4); err == nil {
		t.Fatal("duplicate required_hooks must be rejected")
	}
}

// Malformed digests (binary_digest, plugins_evidence_digest,
// hooks_config_digest, init_evidence_digest, tool_coverage_digest) are
// rejected: sha256:<64 lowercase hex> exactly.
func TestCanonicalProfileV4_DigestFormat(t *testing.T) {
	mutations := map[string]func(a *AgyHarnessSpec){
		"binary_digest":           func(a *AgyHarnessSpec) { a.BinaryDigest = "not-a-digest" },
		"plugins_evidence_digest": func(a *AgyHarnessSpec) { a.PluginsEvidenceDigest = "sha256:tooshort" },
		"hooks_config_digest":     func(a *AgyHarnessSpec) { a.HooksConfigDigest = "SHA256:" + strings.Repeat("a", 64) },
		"init_evidence_digest":    func(a *AgyHarnessSpec) { a.InitEvidenceDigest = "" },
		"tool_coverage_digest":    func(a *AgyHarnessSpec) { a.ToolCoverageDigest = "sha256:" + strings.Repeat("g", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := v4Profile()
			mutate(p.Harnesses["agy"].Agy)
			if _, _, err := ComputeProfileDigest(p); err == nil {
				t.Fatalf("malformed %s must be rejected at freeze", name)
			}
		})
	}
}

// binary_path and expected_home must be absolute.
func TestCanonicalProfileV4_PathsMustBeAbsolute(t *testing.T) {
	p := v4Profile()
	p.Harnesses["agy"].Agy.BinaryPath = "relative/path/agy"
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("relative binary_path must be rejected at freeze")
	}
	p2 := v4Profile()
	p2.Harnesses["agy"].Agy.ExpectedHome = "relative/home"
	if _, _, err := ComputeProfileDigest(p2); err == nil {
		t.Fatal("relative expected_home must be rejected at freeze")
	}
}

// cli_version must be MAJOR.MINOR.PATCH.
func TestCanonicalProfileV4_CLIVersionShape(t *testing.T) {
	for _, bad := range []string{"", "1.2", "1.2.x", "v1.2.9"} {
		p := v4Profile()
		p.Harnesses["agy"].Agy.CLIVersion = bad
		if _, _, err := ComputeProfileDigest(p); err == nil {
			t.Fatalf("cli_version %q must be rejected at freeze", bad)
		}
	}
}

// hooks_evidence requires at least one of verified/unverifiable.
func TestCanonicalProfileV4_HooksEvidenceRequiresOneList(t *testing.T) {
	p := v4Profile()
	p.Harnesses["agy"].Agy.HooksEvidence = AgyHooksEvidenceSpec{}
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("hooks_evidence with both lists empty must be rejected")
	}
}

// Compatibility: cprof-v4 remains eligible for Claude (manifest still
// required; agy/codex blocks ignored) — the v1 rejection is unchanged.
func TestCanonicalProfileV4_ClaudeEligibility(t *testing.T) {
	p := v4Profile()
	if err := p.ValidateForClaude(); err != nil {
		t.Fatalf("v4 profile must be eligible for Claude: %v", err)
	}
}

// Normalization per spec §3.7, matching the existing family: BOM trim,
// NFC, dedupe, byte-wise sort (no lowercasing) for array fields; path
// fields ToSlash/Clean with no trailing slash; digests lowercased.
func TestCanonicalProfileV4_Normalization(t *testing.T) {
	p := v4Profile()
	a := p.Harnesses["agy"].Agy
	// expected_tools rejects duplicates explicitly (spec §3.7), so the
	// dedup/sort demonstration uses hooks_evidence.verified instead
	// (not duplicate-checked at freeze), matching the codex family's
	// rules_evidence normalization pattern.
	a.ExpectedTools = []string{"\ufeffview_file", "run_command", "ask_permission"}
	a.HooksEvidence.Verified = []string{"\ufeffzeta", "Alpha", "Alpha", "zeta"}
	a.BinaryPath = "/home/op//.local/bin/../bin/agy/"
	p.Harnesses["agy"] = HarnessProfileSpec{Model: "\ufeffgpt-oss-120b-medium", NativeAuthMode: "inherited_gemini_home", Agy: a}

	_, canon, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	s := string(canon)
	if !strings.Contains(s, `"expected_tools":["ask_permission","run_command","view_file"]`) {
		t.Fatalf("expected_tools must be BOM-trimmed and sorted: %s", s)
	}
	if !strings.Contains(s, `"verified":["Alpha","zeta"]`) {
		t.Fatalf("hooks_evidence.verified must be deduped, BOM-trimmed, and byte-wise sorted (no lowercasing): %s", s)
	}
	if !strings.Contains(s, `"binary_path":"/home/op/.local/bin/agy"`) {
		t.Fatalf("binary_path must be Clean()ed: %s", s)
	}
	if !strings.Contains(s, `"model":"gpt-oss-120b-medium"`) {
		t.Fatalf("agy harness scalars must be BOM-trimmed: %s", s)
	}
}

// Round-trip: the v4 canonical JSON parses through the strict decoder
// and preserves the agy block.
func TestCanonicalProfileV4_RoundTripParse(t *testing.T) {
	p := v4Profile()
	_, canon, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	parsed, err := ParseCanonicalProfileJSON(canon)
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if parsed.AlgoVersion != "cprof-v4" {
		t.Fatalf("round-trip algo, got %q", parsed.AlgoVersion)
	}
	spec, ok := parsed.Harnesses["agy"]
	if !ok || spec.Agy == nil {
		t.Fatal("round-trip must preserve the agy block")
	}
	if spec.Agy.CLIVersion != "1.2.9" {
		t.Fatalf("round-trip cli_version, got %q", spec.Agy.CLIVersion)
	}
}

// Unknown fields inside the agy block are rejected at parse
// (DisallowUnknownFields).
func TestCanonicalProfileV4_UnknownFieldRejectedAtParse(t *testing.T) {
	p := v4Profile()
	_, canon, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	raw := strings.Replace(string(canon), `"cli_version":"1.2.9"`, `"cli_version":"1.2.9","bypass":"yolo"`, 1)
	if _, err := ParseCanonicalProfileJSON([]byte(raw)); err == nil {
		t.Fatal("unknown agy block key must be rejected at parse")
	}
}
