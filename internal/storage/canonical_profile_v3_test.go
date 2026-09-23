package storage

// cprof-v3: additive codex harness block, frozen per AC-009 spec §3.8.
// v1/v2 encodings must remain byte-identical; the codex block is legal
// only under cprof-v3.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func v3CodexBlock() *CodexHarnessSpec {
	return &CodexHarnessSpec{
		AppServerVersion:  "0.154.0",
		ModelProvider:     "openai",
		ExpectedCodexHome: "/home/op/.codex",
		Platform:          CodexPlatformSpec{OS: "linux", Family: "unix"},
		SandboxPolicy: CodexSandboxPolicySpec{
			Type:          "workspace-write",
			WritableRoots: []string{"/home/op/ws"},
			NetworkAccess: false,
		},
		ApprovalPolicy:             CodexApprovalPolicy{Kind: "string", String: "on-request"},
		ApprovalsReviewer:          "user",
		ExpectedMCPServers:         []string{"context7"},
		ExpectedInstructionSources: []string{"~/.codex/AGENTS.md"},
		RulesEvidence: CodexRulesEvidenceSpec{
			Verified:     []string{"sandbox workspace-write"},
			Unverifiable: []string{"~/.codex/rules/*.rules contents"},
		},
		EventUniversePath:   "docs/superpowers/evidence/ac009-native-event-universe-0.154.0.json",
		EventUniverseDigest: "sha256:" + strings.Repeat("a", 64),
	}
}

func v3Profile() CanonicalProfile {
	p := CanonicalProfile{
		AlgoVersion:         "cprof-v3",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"codex", "git", "go"},
		Harnesses: map[string]HarnessProfileSpec{
			"codex": {Model: "gpt-5.6-sol", NativeAuthMode: "inherited_codex_home", Codex: v3CodexBlock()},
		},
	}
	p.ToolkitManifest = &ToolkitManifestSpec{ToolkitManifest: v2Manifest()}
	return p
}

// Golden digest vector: the canonical v3 encoding is pinned byte-for-byte
// and the digest is sha256 over exactly those bytes.
func TestCanonicalProfileV3_GoldenDigestVector(t *testing.T) {
	p := v3Profile()
	digest, canon, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("v3 digest: %v", err)
	}

	hex64 := strings.Repeat("a", 64)
	golden := fmt.Sprintf(`{"algo_version":"cprof-v3","code_index_scope":[],"harnesses":{"codex":{"codex":{"app_server_version":"0.154.0","approval_policy":"on-request","approvals_reviewer":"user","event_universe_digest":"sha256:%s","event_universe_path":"docs/superpowers/evidence/ac009-native-event-universe-0.154.0.json","expected_codex_home":"/home/op/.codex","expected_instruction_sources":["~/.codex/AGENTS.md"],"expected_mcp_servers":["context7"],"model_provider":"openai","platform":{"family":"unix","os":"linux"},"rules_evidence":{"unverifiable":["~/.codex/rules/*.rules contents"],"verified":["sandbox workspace-write"]},"sandbox_policy":{"network_access":false,"type":"workspace-write","writable_roots":["/home/op/ws"]}},"extra_env_allowlist":[],"model":"gpt-5.6-sol","native_auth_mode":"inherited_codex_home"}},"isolation_strictness":"permissive_dev","network_allowlist":[],"network_mode":"unrestricted","tooling":["codex","git","go"],"toolkit_manifest":{"approved_tools":["Glob","Grep","Read"],"denied_complement":["Bash","Write"],"expected_hooks":["PreToolUse","SessionStart:startup"],"expected_plugins":["superpowers"],"expected_skills":["research"],"probed_cli_version":"2.1.278","turns_bound":8,"universe_evidence_digest":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","universe_evidence_path":"docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json"},"workspace_mode":"none"}`, hex64)

	if string(canon) != golden {
		t.Fatalf("canonical v3 JSON drifted from the golden vector:\n got: %s\nwant: %s", canon, golden)
	}
	sum := sha256.Sum256([]byte(golden))
	want := fmt.Sprintf("cprof-v3:sha256:%x", sum)
	if digest != want {
		t.Fatalf("v3 digest must be sha256 over the golden bytes: got %q want %q", digest, want)
	}
}

// v3 requires the toolkit manifest (inherits the v2 rule).
func TestCanonicalProfileV3_RequiresToolkitManifest(t *testing.T) {
	p := v3Profile()
	p.ToolkitManifest = nil
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("cprof-v3 without toolkit_manifest must be rejected")
	}
	// An incomplete manifest is rejected too.
	p.ToolkitManifest = &ToolkitManifestSpec{ToolkitManifest: v2Manifest()}
	p.ToolkitManifest.ToolkitManifest.TurnsBound = 0
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("cprof-v3 with an incomplete manifest must be rejected")
	}
}

// A codex block under cprof-v1 or cprof-v2 is a validation error, at
// freeze and at parse.
func TestCanonicalProfileV3_RejectsCodexBlockUnderV1AndV2(t *testing.T) {
	for _, v := range []string{"cprof-v1", "cprof-v2"} {
		p := v3Profile()
		p.AlgoVersion = v
		if v == "cprof-v1" {
			p.ToolkitManifest = nil
		}
		if _, _, err := ComputeProfileDigest(p); err == nil {
			t.Fatalf("codex block under %s must be rejected at freeze", v)
		}
		canon := map[string]any{
			"algo_version":         v,
			"code_index_scope":     []any{},
			"harnesses":            map[string]any{"codex": map[string]any{"extra_env_allowlist": []any{}, "model": "m", "native_auth_mode": "a", "codex": map[string]any{"approval_policy": "never", "approvals_reviewer": "user"}}},
			"isolation_strictness": "permissive_dev",
			"network_allowlist":    []any{},
			"network_mode":         "unrestricted",
			"tooling":              []any{"git"},
			"workspace_mode":       "none",
		}
		if v == "cprof-v2" {
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
			t.Fatalf("codex block under %s must be rejected at parse", v)
		}
	}
}

// v1/v2 canonical encodings are byte-identical to the pre-v3 implementation.
func TestCanonicalProfileV1V2_ByteIdenticalEncodings(t *testing.T) {
	v1 := CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"git", "go"},
		Harnesses: map[string]HarnessProfileSpec{
			"claude": {Model: "m", NativeAuthMode: "inherited_host_keychain"},
		},
	}
	_, canonV1, err := ComputeProfileDigest(v1)
	if err != nil {
		t.Fatalf("v1 digest: %v", err)
	}
	wantV1 := `{"algo_version":"cprof-v1","code_index_scope":[],"harnesses":{"claude":{"extra_env_allowlist":[],"model":"m","native_auth_mode":"inherited_host_keychain"}},"isolation_strictness":"permissive_dev","network_allowlist":[],"network_mode":"unrestricted","tooling":["git","go"],"workspace_mode":"none"}`
	if string(canonV1) != wantV1 {
		t.Fatalf("v1 encoding changed:\n got: %s\nwant: %s", canonV1, wantV1)
	}

	_, canonV2, err := ComputeProfileDigest(v2Profile())
	if err != nil {
		t.Fatalf("v2 digest: %v", err)
	}
	wantV2 := `{"algo_version":"cprof-v2","code_index_scope":[],"harnesses":{"claude":{"extra_env_allowlist":[],"model":"haiku","native_auth_mode":"inherited_host_keychain"}},"isolation_strictness":"permissive_dev","network_allowlist":[],"network_mode":"unrestricted","tooling":["git","go","opencode"],"toolkit_manifest":{"approved_tools":["Glob","Grep","Read"],"denied_complement":["Bash","Write"],"expected_hooks":["PreToolUse","SessionStart:startup"],"expected_plugins":["superpowers"],"expected_skills":["research"],"probed_cli_version":"2.1.278","turns_bound":8,"universe_evidence_digest":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","universe_evidence_path":"docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json"},"workspace_mode":"none"}`
	if string(canonV2) != wantV2 {
		t.Fatalf("v2 encoding changed:\n got: %s\nwant: %s", canonV2, wantV2)
	}
}

// Normalization per spec §3.8, matching the existing family: BOM trim,
// NFC, dedupe, byte-wise sort (no lowercasing), scalar BOM-trim + NFC,
// writable_roots path-cleaned.
func TestCanonicalProfileV3_Normalization(t *testing.T) {
	p := v3Profile()
	c := p.Harnesses["codex"].Codex
	c.ExpectedMCPServers = []string{"\ufeffZeta", "zeta", "Alpha", "Alpha"}
	c.ExpectedInstructionSources = []string{"\ufeff~/.codex/AGENTS.md"}
	c.SandboxPolicy.WritableRoots = []string{"/home/op/ws///", "\ufeff/home/op/WS"}
	p.Harnesses["codex"] = HarnessProfileSpec{Model: "\ufeffgpt-5.6-sol", NativeAuthMode: "inherited_codex_home", Codex: c}

	_, canon, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	s := string(canon)
	// Dedupe + case-sensitive byte-wise sort (no lowercasing).
	for _, want := range []string{`"expected_mcp_servers":["Alpha","Zeta","zeta"]`} {
		if !strings.Contains(s, want) {
			t.Fatalf("normalized array %s missing from %s", want, s)
		}
	}
	if !strings.Contains(s, `"expected_instruction_sources":["~/.codex/AGENTS.md"]`) {
		t.Fatalf("array entries must be BOM-trimmed: %s", s)
	}
	if !strings.Contains(s, `"writable_roots":["/home/op/WS","/home/op/ws"]`) {
		t.Fatalf("writable_roots must be path-cleaned and sorted: %s", s)
	}
	if !strings.Contains(s, `"model":"gpt-5.6-sol"`) {
		t.Fatalf("codex scalars must be BOM-trimmed: %s", s)
	}
}

// approval_policy enum at freeze: only untrusted | on-request | never as
// string kind (plus the granular object kind).
func TestCanonicalProfileV3_ApprovalPolicyEnum(t *testing.T) {
	for _, ok := range []string{"untrusted", "on-request", "never"} {
		p := v3Profile()
		p.Harnesses["codex"].Codex.ApprovalPolicy = CodexApprovalPolicy{Kind: "string", String: ok}
		if _, _, err := ComputeProfileDigest(p); err != nil {
			t.Fatalf("approval_policy %q must freeze: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "yolo", "auto_review", "on_request"} {
		p := v3Profile()
		p.Harnesses["codex"].Codex.ApprovalPolicy = CodexApprovalPolicy{Kind: "string", String: bad}
		if _, _, err := ComputeProfileDigest(p); err == nil {
			t.Fatalf("approval_policy %q must be rejected at freeze", bad)
		}
	}
	// The JSON string form round-trips through the tagged union at parse;
	// the enum is a freeze gate, not a parse gate.
	raw := v3CanonicalJSONWithPolicy(`"on-request"`)
	parsed, err := ParseCanonicalProfileJSON(raw)
	if err != nil {
		t.Fatalf("string policy must parse: %v", err)
	}
	if got := parsed.Harnesses["codex"].Codex.ApprovalPolicy; got.Kind != "string" || got.String != "on-request" {
		t.Fatalf("string policy round-trip, got %+v", got)
	}
	unknown := v3CanonicalJSONWithPolicy(`"bypass-all"`)
	if _, err := ParseCanonicalProfileJSON(unknown); err != nil {
		t.Fatalf("parse is shape-only: %v", err)
	}
	if _, _, err := ComputeProfileDigest(parsed); err != nil {
		t.Fatalf("re-freeze: %v", err)
	}
	parsedUnknown, err := ParseCanonicalProfileJSON(unknown)
	if err != nil {
		t.Fatalf("parse unknown string policy: %v", err)
	}
	if _, _, err := ComputeProfileDigest(parsedUnknown); err == nil {
		t.Fatal("unknown string policy must be rejected at freeze")
	}
	// Neither kind missing.
	if _, err := ParseCanonicalProfileJSON(v3CanonicalJSONWithPolicy(`null`)); err == nil {
		t.Fatal("null approval_policy must be rejected at parse")
	}
}

// approvals_reviewer MUST be "user", enforced at freeze.
func TestCanonicalProfileV3_ApprovalsReviewerMustBeUser(t *testing.T) {
	for _, bad := range []string{"", "auto_review", "guardian_subagent", "User"} {
		p := v3Profile()
		p.Harnesses["codex"].Codex.ApprovalsReviewer = bad
		if _, _, err := ComputeProfileDigest(p); err == nil {
			t.Fatalf("approvals_reviewer %q must be rejected at freeze", bad)
		}
	}
}

// Granular approval policy: the committed 0.154.0 TurnStartParams.json
// capture pins the five-key all-boolean AskForApproval.granular shape,
// so a well-formed granular policy now freezes (Task 9 wired the shape
// evidence; Task 2's fail-closed gap is closed). The freeze must still
// enforce the shape registry: every canonical granular key carries a
// committed evidence path, and the granular policy participates in the
// canonical encoding.
func TestCanonicalProfileV3_GranularShapeEvidenceWired(t *testing.T) {
	// The registry pins exactly the canonical granular keys.
	if len(granularShapeEvidence) != len(granularApprovalKeys) {
		t.Fatalf("shape evidence registry must pin exactly the canonical granular keys, got %d entries", len(granularShapeEvidence))
	}
	for _, key := range granularApprovalKeys {
		path := granularShapeEvidence[key]
		if path == "" {
			t.Fatalf("granular key %q has no committed shape evidence", key)
		}
		if !strings.HasPrefix(path, "docs/superpowers/evidence/") {
			t.Fatalf("granular key %q evidence %q must be a committed repo-relative evidence path", key, path)
		}
	}
	p := v3Profile()
	p.Harnesses["codex"].Codex.ApprovalPolicy = CodexApprovalPolicy{Kind: "granular", Granular: granularFixture()}
	digest, _, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("granular approval_policy with committed shape evidence must freeze, got %v", err)
	}
	if digest == "" {
		t.Fatal("freeze must produce a digest")
	}

	// Fail-closed regression: with ANY one granular key's shape evidence
	// missing from the registry, freeze must REJECT the granular policy
	// (the registry is package state; the entry is restored before the
	// subtest returns so other tests are unaffected).
	t.Run("unpinnedKeyRejectedAtFreeze", func(t *testing.T) {
		key := granularApprovalKeys[0]
		saved, had := granularShapeEvidence[key]
		delete(granularShapeEvidence, key)
		t.Cleanup(func() {
			if had {
				granularShapeEvidence[key] = saved
			} else {
				delete(granularShapeEvidence, key)
			}
		})
		q := v3Profile()
		q.Harnesses["codex"].Codex.ApprovalPolicy = CodexApprovalPolicy{Kind: "granular", Granular: granularFixture()}
		_, _, err := ComputeProfileDigest(q)
		if err == nil {
			t.Fatalf("granular approval_policy with unpinned key %q must be rejected at freeze", key)
		}
		if !strings.Contains(err.Error(), "no committed schema-derived shape evidence") {
			t.Fatalf("rejection must name the missing shape evidence, got %v", err)
		}
	})
}

// An in-memory tagged union with the granular kind but no granular
// object fails closed with a validation error, not a nil dereference.
func TestCanonicalProfileV3_GranularNilObjectRejectedAtFreeze(t *testing.T) {
	p := v3Profile()
	p.Harnesses["codex"].Codex.ApprovalPolicy = CodexApprovalPolicy{Kind: "granular", Granular: nil}
	_, _, err := ComputeProfileDigest(p)
	if err == nil {
		t.Fatal("granular approval_policy without a granular object must be rejected at freeze")
	}
	if !strings.Contains(err.Error(), "granular") {
		t.Fatalf("rejection must name the missing granular object, got %v", err)
	}
}

// Granular canonical byte-equality: two granular policies whose canonical
// encodings are byte-identical are equal; any byte difference (key order
// folded away, value or number-literal differences kept) is a different
// policy. No semantic interpretation of sub-values.
func TestCanonicalProfileV3_GranularCanonicalByteEquality(t *testing.T) {
	a := `{"mcp_elicitations": true, "rules":false,"sandbox_approval":true,"request_permissions":false,"skill_approval":true}`
	b := `{"skill_approval":true,"request_permissions":false,"sandbox_approval":true, "rules":false,"mcp_elicitations":true}`
	pa, pb := mustGranular(t, a), mustGranular(t, b)
	ca, err := canonicalGranularApproval(pa)
	if err != nil {
		t.Fatalf("canonicalize a: %v", err)
	}
	cb, err := canonicalGranularApproval(pb)
	if err != nil {
		t.Fatalf("canonicalize b: %v", err)
	}
	ba, err1 := json.Marshal(ca)
	bb, err2 := json.Marshal(cb)
	if err1 != nil || err2 != nil {
		t.Fatalf("marshal canonical granular: %v %v", err1, err2)
	}
	if string(ba) != string(bb) {
		t.Fatalf("key order and whitespace must fold to identical canonical bytes: %s vs %s", ba, bb)
	}
	if string(ba) != `{"mcp_elicitations":true,"request_permissions":false,"rules":false,"sandbox_approval":true,"skill_approval":true}` {
		t.Fatalf("canonical granular must be sorted and compact, got %s", ba)
	}

	// A different number literal is a different policy (no folding).
	c := `{"mcp_elicitations": true, "rules":false,"sandbox_approval":true,"request_permissions":1.0,"skill_approval":true}`
	pc := mustGranular(t, c)
	cc, err := canonicalGranularApproval(pc)
	if err != nil {
		t.Fatalf("canonicalize c: %v", err)
	}
	bc, err := json.Marshal(cc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(bc) == string(ba) {
		t.Fatal("different sub-values must not collapse to the same canonical bytes")
	}
	if !strings.Contains(string(bc), `"request_permissions":1.0`) {
		t.Fatalf("number literals must be re-encoded verbatim, got %s", bc)
	}
}

// Unknown keys inside the granular object are rejected at parse
// (DisallowUnknownFields on the typed struct).
func TestCanonicalProfileV3_GranularUnknownKeyRejectedAtParse(t *testing.T) {
	granular := `{"mcp_elicitations":true,"rules":false,"sandbox_approval":true,"request_permissions":false,"skill_approval":true,"bypass":true}`
	if _, err := ParseCanonicalProfileJSON(v3CanonicalJSONWithPolicy(granular)); err == nil {
		t.Fatal("unknown granular key must be rejected at parse")
	}
	// Unknown keys anywhere in the codex block are rejected too.
	raw := strings.Replace(string(v3GoldenRaw()),
		`"approvals_reviewer":"user"`,
		`"approvals_reviewer":"user","approval_bypass":"yolo"`, 1)
	if _, err := ParseCanonicalProfileJSON([]byte(raw)); err == nil {
		t.Fatal("unknown codex block key must be rejected at parse")
	}
}

// Compatibility matrix, Claude leg: cprof-v3 is additive — Claude accepts
// v2|v3 (manifest still required) and still rejects v1.
func TestCanonicalProfileV3_ClaudeEligibility(t *testing.T) {
	p := v3Profile()
	if err := p.ValidateForClaude(); err != nil {
		t.Fatalf("v3 profile must remain eligible for Claude: %v", err)
	}
	v1 := v3Profile()
	v1.AlgoVersion = "cprof-v1"
	v1.ToolkitManifest = nil
	err := v1.ValidateForClaude()
	var unsupported *ErrUnsupportedProfile
	if err == nil || !asErrUnsupported(err, &unsupported) {
		t.Fatalf("v1 must stay ineligible for Claude, got %T: %v", err, err)
	}
}

// Round-trip: the v3 canonical JSON parses through the strict decoder
// and preserves the codex block.
func TestCanonicalProfileV3_RoundTripParse(t *testing.T) {
	p := v3Profile()
	_, canon, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	parsed, err := ParseCanonicalProfileJSON(canon)
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if parsed.AlgoVersion != "cprof-v3" {
		t.Fatalf("round-trip algo, got %q", parsed.AlgoVersion)
	}
	spec, ok := parsed.Harnesses["codex"]
	if !ok || spec.Codex == nil {
		t.Fatal("round-trip must preserve the codex block")
	}
	if got := spec.Codex.ApprovalPolicy; got.Kind != "string" || got.String != "on-request" {
		t.Fatalf("round-trip approval policy, got %+v", got)
	}
	if spec.Codex.SandboxPolicy.NetworkAccess {
		t.Fatal("round-trip network_access must be preserved")
	}
}

// granularFixture returns a structurally complete granular object.
func granularFixture() *CodexGranularApproval {
	return &CodexGranularApproval{
		McpElicitations:    json.RawMessage(`true`),
		Rules:              json.RawMessage(`false`),
		SandboxApproval:    json.RawMessage(`true`),
		RequestPermissions: json.RawMessage(`false`),
		SkillApproval:      json.RawMessage(`true`),
	}
}

func mustGranular(t *testing.T, raw string) *CodexGranularApproval {
	t.Helper()
	var policy CodexApprovalPolicy
	if err := policy.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatalf("decode granular %s: %v", raw, err)
	}
	if policy.Kind != "granular" || policy.Granular == nil {
		t.Fatalf("expected granular kind, got %+v", policy)
	}
	return policy.Granular
}

// v3GoldenRaw returns the canonical v3 JSON of v3Profile() as produced by
// the golden vector test.
func v3GoldenRaw() []byte {
	p := v3Profile()
	_, canon, err := ComputeProfileDigest(p)
	if err != nil {
		panic(err)
	}
	return canon
}

// v3CanonicalJSONWithPolicy builds a strict-decodable v3 profile JSON
// whose codex approval_policy is the given raw JSON.
func v3CanonicalJSONWithPolicy(policy string) []byte {
	base := string(v3GoldenRaw())
	old := `"approval_policy":"on-request"`
	if !strings.Contains(base, old) {
		panic("golden vector lost its approval_policy")
	}
	return []byte(strings.Replace(base, old, `"approval_policy":`+policy, 1))
}
