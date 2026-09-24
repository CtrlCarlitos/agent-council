package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// HarnessProfileSpec defines the per-harness execution parameters within a canonical profile.
type HarnessProfileSpec struct {
	ExtraEnvAllowlist []string `json:"extra_env_allowlist"`
	Model             string   `json:"model"`
	NativeAuthMode    string   `json:"native_auth_mode"`
	// Codex is additive in cprof-v3 (absent in v1/v2 encodings, where a
	// codex block is a validation error). See CodexHarnessSpec and the
	// AC-009 spec §3.8 for the frozen encoding.
	Codex *CodexHarnessSpec `json:"codex,omitempty"`
	// Agy is additive in cprof-v4 (absent in v1/v2/v3 encodings, where an
	// agy block is a validation error). See AgyHarnessSpec and the
	// AC-010 spec §3.7 for the frozen encoding.
	Agy *AgyHarnessSpec `json:"agy,omitempty"`
}

// CodexPlatformSpec is the frozen platform identity compared against the
// native initialize attestation.
type CodexPlatformSpec struct {
	OS     string `json:"os"`
	Family string `json:"family"`
}

// CodexSandboxPolicySpec is the frozen sandbox policy compared on resume
// and turn_context. writable_roots are path-normalized in the canonical
// encoding; network_access is a plain boolean.
type CodexSandboxPolicySpec struct {
	Type          string   `json:"type"`
	WritableRoots []string `json:"writable_roots"`
	NetworkAccess bool     `json:"network_access"`
}

// CodexRulesEvidenceSpec records what Council verified about the native
// rules/hooks layer and what stays an explicit, unclaimed gap.
type CodexRulesEvidenceSpec struct {
	Verified     []string `json:"verified"`
	Unverifiable []string `json:"unverifiable"`
}

// CodexGranularApproval is the `granular` object kind of the codex
// approval policy: EXACTLY the five keys observed in the installed
// schema. Unknown keys are rejected at parse (DisallowUnknownFields on
// this typed struct); each value is re-encoded verbatim as canonical
// JSON at freeze. Equality of two granular policies is byte equality of
// their canonical encodings — no semantic interpretation of sub-values.
type CodexGranularApproval struct {
	McpElicitations    json.RawMessage `json:"mcp_elicitations"`
	Rules              json.RawMessage `json:"rules"`
	SandboxApproval    json.RawMessage `json:"sandbox_approval"`
	RequestPermissions json.RawMessage `json:"request_permissions"`
	SkillApproval      json.RawMessage `json:"skill_approval"`
}

// Granular approval policy kinds for CodexApprovalPolicy.Kind.
const (
	CodexApprovalKindString   = "string"
	CodexApprovalKindGranular = "granular"
)

// CodexApprovalPolicy is the string-or-granular tagged union of the
// native approval policy. The canonical enum for the string kind is
// {untrusted, on-request, never}; no bypass value exists natively.
type CodexApprovalPolicy struct {
	// Kind is CodexApprovalKindString or CodexApprovalKindGranular.
	Kind     string
	String   string
	Granular *CodexGranularApproval
}

func (p *CodexApprovalPolicy) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return errors.New("approval_policy is required")
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return fmt.Errorf("decode approval_policy string: %w", err)
		}
		p.Kind = CodexApprovalKindString
		p.String = s
		p.Granular = nil
		return nil
	case '{':
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		var g CodexGranularApproval
		if err := dec.Decode(&g); err != nil {
			return fmt.Errorf("decode granular approval_policy: %w", err)
		}
		if dec.More() {
			return errors.New("trailing characters after granular approval_policy")
		}
		p.Kind = CodexApprovalKindGranular
		p.String = ""
		p.Granular = &g
		return nil
	default:
		return fmt.Errorf("approval_policy must be a string or a granular object, got %s", trimmed)
	}
}

// granularApprovalKeys are the canonical granular keys in sorted order.
var granularApprovalKeys = []string{
	"mcp_elicitations",
	"request_permissions",
	"rules",
	"sandbox_approval",
	"skill_approval",
}

// granularShapeEvidence records, per granular approval key, the
// committed schema-derived evidence that pins the key's sub-value shape
// (generate-json-schema captures committed with digest under
// docs/superpowers/evidence/). Entries are added ONLY when the
// corresponding capture is committed to the repository. With no
// committed evidence for a key's shape, profile freeze REJECTS the
// granular policy entirely (fail-closed honest gap; string policies
// remain fully usable).
//
// The committed 0.154.0 capture
// docs/superpowers/evidence/ac009-schema-0.154.0/TurnStartParams.json
// pins the complete AskForApproval.granular shape: exactly the five keys
// below, each typed boolean (mcp_elicitations/rules/sandbox_approval
// required; request_permissions/skill_approval default false). All five
// entries are therefore wired to that capture (AC-009 Task 9).
var granularShapeEvidence = map[string]string{
	"mcp_elicitations":    "docs/superpowers/evidence/ac009-schema-0.154.0/TurnStartParams.json",
	"request_permissions": "docs/superpowers/evidence/ac009-schema-0.154.0/TurnStartParams.json",
	"rules":               "docs/superpowers/evidence/ac009-schema-0.154.0/TurnStartParams.json",
	"sandbox_approval":    "docs/superpowers/evidence/ac009-schema-0.154.0/TurnStartParams.json",
	"skill_approval":      "docs/superpowers/evidence/ac009-schema-0.154.0/TurnStartParams.json",
}

// CodexHarnessSpec is the frozen per-run codex block added by cprof-v3
// (AC-009 spec §3.8). Every value the design freezes and compares
// appears here and is covered by the profile digest.
type CodexHarnessSpec struct {
	AppServerVersion  string                 `json:"app_server_version"`
	ModelProvider     string                 `json:"model_provider"`
	ExpectedCodexHome string                 `json:"expected_codex_home"`
	Platform          CodexPlatformSpec      `json:"platform"`
	SandboxPolicy     CodexSandboxPolicySpec `json:"sandbox_policy"`
	ApprovalPolicy    CodexApprovalPolicy    `json:"approval_policy"`
	// ApprovalsReviewer MUST be "user": any other native value (e.g.
	// auto_review) would answer approvals outside Council's visibility
	// (§3.6). Enforced at profile freeze, not at runtime.
	ApprovalsReviewer  string   `json:"approvals_reviewer"`
	ExpectedMCPServers []string `json:"expected_mcp_servers"`
	// ExpectedMCPTools is the EXACT frozen inventory of MCP tool paths,
	// each "<server>/<tool>" with <server> in ExpectedMCPServers. It is
	// the MCP half of the attestation coverage universe (AC-009 §3.7):
	// every listed path must be probed, and nothing else may be.
	ExpectedMCPTools []string `json:"expected_mcp_tools"`
	// ExpectedPluginTools is the EXACT frozen inventory of skill/plugin-
	// contributed tool names — the plugin half of the coverage universe.
	ExpectedPluginTools        []string               `json:"expected_plugin_tools"`
	ExpectedInstructionSources []string               `json:"expected_instruction_sources"`
	RulesEvidence              CodexRulesEvidenceSpec `json:"rules_evidence"`
	EventUniversePath          string                 `json:"event_universe_path"`
	EventUniverseDigest        string                 `json:"event_universe_digest"`
	// ToolInventoryPath / ToolInventoryDigest are the binding slot for
	// the eventual native tool-inventory evidence (repo-relative under
	// the evidence root, sha256 of its bytes). The adapter re-hashes a
	// present capture and requires its server, MCP tool, and plugin tool
	// sets to EQUAL the frozen lists, but a Council-shaped capture is
	// operator-edited JSON, NOT native evidence: until a schema-pinned
	// derivation of the raw native response exists, profile freeze
	// REJECTS every non-empty inventory (validateCodexHarnessBlock), so
	// today the slot can only ever agree with empty lists.
	ToolInventoryPath   string `json:"tool_inventory_path"`
	ToolInventoryDigest string `json:"tool_inventory_digest"`
}

// AgyPlatformSpec is the frozen platform identity compared against the
// Agy install (AC-010 spec §3.7).
type AgyPlatformSpec struct {
	OS     string `json:"os"`
	Family string `json:"family"`
}

// AgyHooksEvidenceSpec records what Council verified about the Agy hooks
// layer and what stays an explicit, unclaimed gap (spec §3.2).
type AgyHooksEvidenceSpec struct {
	Verified     []string `json:"verified"`
	Unverifiable []string `json:"unverifiable"`
}

// AgyHarnessSpec is the frozen per-run agy block added by cprof-v4
// (AC-010 spec §3.7). Every value the design freezes and compares
// appears here and is covered by the profile digest.
type AgyHarnessSpec struct {
	CLIVersion   string          `json:"cli_version"`
	BinaryPath   string          `json:"binary_path"`
	BinaryDigest string          `json:"binary_digest"`
	ExpectedHome string          `json:"expected_home"`
	Platform     AgyPlatformSpec `json:"platform"`
	// PermissionMode ∈ {request-review, strict}: no bypass value is
	// representable (spec §3.7).
	PermissionMode string `json:"permission_mode"`
	// ExecutionMode ∈ {default, accept-edits, plan}.
	ExecutionMode               string               `json:"execution_mode"`
	Sandbox                     bool                 `json:"sandbox"`
	PrintTimeoutBackstopSeconds int                  `json:"print_timeout_backstop_seconds"`
	ExpectedTools               []string             `json:"expected_tools"`
	ExpectedMCPServers          []string             `json:"expected_mcp_servers"`
	ExpectedMCPTools            []string             `json:"expected_mcp_tools"`
	ExpectedPluginTools         []string             `json:"expected_plugin_tools"`
	DefaultRequiredTools        []string             `json:"default_required_tools"`
	HooksEvidence               AgyHooksEvidenceSpec `json:"hooks_evidence"`
	ExpectedSkills              []string             `json:"expected_skills"`
	PluginsEvidencePath         string               `json:"plugins_evidence_path"`
	PluginsEvidenceDigest       string               `json:"plugins_evidence_digest"`
	HooksConfigDigest           string               `json:"hooks_config_digest"`
	RequiredHooks               []string             `json:"required_hooks"`
	InitEvidencePath            string               `json:"init_evidence_path"`
	InitEvidenceDigest          string               `json:"init_evidence_digest"`
	ToolCoveragePath            string               `json:"tool_coverage_path"`
	ToolCoverageDigest          string               `json:"tool_coverage_digest"`
}

// CanonicalProfile defines the frozen execution profile parameters for a Council run.
type CanonicalProfile struct {
	AlgoVersion         string                        `json:"algo_version"`
	WorkspaceMode       string                        `json:"workspace_mode"`
	IsolationStrictness string                        `json:"isolation_strictness"`
	NetworkMode         string                        `json:"network_mode"`
	NetworkAllowlist    []string                      `json:"network_allowlist"`
	CodeIndexScope      []string                      `json:"code_index_scope"`
	Tooling             []string                      `json:"tooling"`
	Harnesses           map[string]HarnessProfileSpec `json:"harnesses"`
	// ToolkitManifest is additive in cprof-v2 (absent in v1 encodings).
	// See toolkit_manifest.go for the type and Claude eligibility rules.
	ToolkitManifest *ToolkitManifestSpec `json:"toolkit_manifest,omitempty"`
}

// ComputeBriefDigest calculates the canonical digest for the run brief:
// "cbrief-v1:sha256:" + hex(sha256(canonical_utf8_nfc_brief_bytes))
func ComputeBriefDigest(brief string) (string, error) {
	if !utf8.ValidString(brief) {
		return "", errors.New("brief contains invalid UTF-8 bytes")
	}
	trimmed := strings.Trim(brief, "\ufeff")
	canonical := norm.NFC.String(trimmed)
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("cbrief-v1:sha256:%x", sum), nil
}

func isHexLower(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ComputeSourceDigest calculates the canonical digest for the run source inputs:
// For workspace_mode == "none": "csource-v1:none"
// For readonly and isolated_branch: "csource-v1:sha256:" + hex(sha256(canonical_source_json))
func ComputeSourceDigest(mode string, repoIdentity, commit, tree string) (string, error) {
	switch mode {
	case "none":
		return "csource-v1:none", nil
	case "readonly", "isolated_branch":
		if repoIdentity == "" {
			return "", errors.New("repo_identity must not be empty for workspace mode " + mode)
		}
		if !utf8.ValidString(repoIdentity) {
			return "", errors.New("repo_identity contains invalid UTF-8 bytes")
		}
		if len(commit) != 40 || !isHexLower(commit) {
			return "", errors.New("commit must be a 40-character lowercase hexadecimal sha")
		}
		if len(tree) != 40 || !isHexLower(tree) {
			return "", errors.New("tree must be a 40-character lowercase hexadecimal sha")
		}

		cleanedRepo := filepath.ToSlash(filepath.Clean(strings.Trim(repoIdentity, "\ufeff")))
		cleanedRepo = norm.NFC.String(cleanedRepo)

		m := map[string]string{
			"algo_version":  "csource-v1",
			"commit":        commit,
			"repo_identity": cleanedRepo,
			"tree":          tree,
		}

		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(m); err != nil {
			return "", fmt.Errorf("encode canonical source json: %w", err)
		}
		rawJSON := bytes.TrimRight(buf.Bytes(), "\n")
		sum := sha256.Sum256(rawJSON)
		return fmt.Sprintf("csource-v1:sha256:%x", sum), nil
	default:
		return "", fmt.Errorf("unsupported workspace mode for source digest: %q", mode)
	}
}

// normalizeStringSlice trims BOM, normalizes to UTF-8 NFC, deduplicates, and sorts lexicographically.
func normalizeStringSlice(slice []string, lower bool, cleanPath bool) []string {
	if slice == nil {
		return []string{}
	}
	seen := make(map[string]struct{}, len(slice))
	out := make([]string, 0, len(slice))
	for _, item := range slice {
		s := strings.Trim(item, "\ufeff")
		if lower {
			s = strings.ToLower(s)
		}
		if cleanPath {
			s = filepath.ToSlash(filepath.Clean(s))
			if s != "/" {
				s = strings.TrimSuffix(s, "/")
			}
		}
		s = norm.NFC.String(s)
		if _, exists := seen[s]; !exists {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// normalizePathScalar applies the family's scalar path normalization
// (BOM trim, ToSlash/Clean, no trailing slash, NFC) to a single path
// field (e.g. binary_path, expected_home) — the scalar counterpart of
// normalizeStringSlice(slice, false, true).
func normalizePathScalar(p string) string {
	s := strings.Trim(p, "\ufeff")
	s = filepath.ToSlash(filepath.Clean(s))
	if s != "/" {
		s = strings.TrimSuffix(s, "/")
	}
	return norm.NFC.String(s)
}

// ComputeProfileDigest computes the canonical profile digest and normalized JSON representation:
// "cprof-v1:sha256:" / "cprof-v2:sha256:" / "cprof-v3:sha256:" / "cprof-v4:sha256:" + hex(sha256(canonical_profile_json))
func ComputeProfileDigest(profile CanonicalProfile) (string, []byte, error) {
	switch profile.AlgoVersion {
	case "cprof-v1":
		if profile.ToolkitManifest != nil {
			return "", nil, fmt.Errorf("toolkit_manifest requires algo_version %q, not %q", "cprof-v2", profile.AlgoVersion)
		}
		if err := profile.validateNoCodexBlocks(); err != nil {
			return "", nil, err
		}
		if err := profile.validateNoAgyBlocks(); err != nil {
			return "", nil, err
		}
	case "cprof-v2", "cprof-v3", "cprof-v4":
		if profile.ToolkitManifest == nil {
			return "", nil, fmt.Errorf("algo_version %q requires toolkit_manifest", profile.AlgoVersion)
		}
		if err := profile.ValidateForClaude(); err != nil {
			return "", nil, err
		}
		switch profile.AlgoVersion {
		case "cprof-v2":
			if err := profile.validateNoCodexBlocks(); err != nil {
				return "", nil, err
			}
			if err := profile.validateNoAgyBlocks(); err != nil {
				return "", nil, err
			}
		case "cprof-v3":
			if err := profile.validateCodexBlocks(); err != nil {
				return "", nil, err
			}
			if err := profile.validateNoAgyBlocks(); err != nil {
				return "", nil, err
			}
		case "cprof-v4":
			// cprof-v4 requires toolkit_manifest (inherited above) and,
			// when a codex block is present, validates it exactly as v3
			// does; the additive agy block is validated by its own gates.
			if err := profile.validateCodexBlocks(); err != nil {
				return "", nil, err
			}
			if err := profile.validateAgyBlocks(); err != nil {
				return "", nil, err
			}
		}
	default:
		return "", nil, fmt.Errorf("unsupported profile algo_version: %q (expected %q, %q, %q or %q)", profile.AlgoVersion, "cprof-v1", "cprof-v2", "cprof-v3", "cprof-v4")
	}

	switch profile.WorkspaceMode {
	case "none", "readonly", "isolated_branch":
	default:
		return "", nil, fmt.Errorf("unsupported workspace_mode: %q", profile.WorkspaceMode)
	}

	switch profile.IsolationStrictness {
	case "strict", "permissive_dev":
	default:
		return "", nil, fmt.Errorf("unsupported isolation_strictness: %q", profile.IsolationStrictness)
	}

	switch profile.NetworkMode {
	case "none", "allowlist", "unrestricted":
	default:
		return "", nil, fmt.Errorf("unsupported network_mode: %q", profile.NetworkMode)
	}

	normTooling := normalizeStringSlice(profile.Tooling, true, false)
	normNetworkAllowlist := normalizeStringSlice(profile.NetworkAllowlist, false, false)
	normCodeIndexScope := normalizeStringSlice(profile.CodeIndexScope, false, true)

	harnessesMap := make(map[string]any, len(profile.Harnesses))
	for hName, hSpec := range profile.Harnesses {
		cleanedHName := norm.NFC.String(strings.Trim(hName, "\ufeff"))
		entry := map[string]any{
			"extra_env_allowlist": normalizeStringSlice(hSpec.ExtraEnvAllowlist, false, false),
			"model":               norm.NFC.String(strings.Trim(hSpec.Model, "\ufeff")),
			"native_auth_mode":    norm.NFC.String(strings.Trim(hSpec.NativeAuthMode, "\ufeff")),
		}
		if hSpec.Codex != nil {
			block, err := canonicalCodexBlock(hSpec.Codex)
			if err != nil {
				return "", nil, err
			}
			entry["codex"] = block
		}
		if hSpec.Agy != nil {
			entry["agy"] = canonicalAgyBlock(hSpec.Agy)
		}
		harnessesMap[cleanedHName] = entry
	}

	canonicalMap := map[string]any{
		"algo_version":         profile.AlgoVersion,
		"code_index_scope":     normCodeIndexScope,
		"harnesses":            harnessesMap,
		"isolation_strictness": profile.IsolationStrictness,
		"network_allowlist":    normNetworkAllowlist,
		"network_mode":         profile.NetworkMode,
		"tooling":              normTooling,
		"workspace_mode":       profile.WorkspaceMode,
	}

	if profile.AlgoVersion == "cprof-v2" || profile.AlgoVersion == "cprof-v3" || profile.AlgoVersion == "cprof-v4" {
		canonicalMap["toolkit_manifest"] = canonicalToolkitManifestMap(profile.ToolkitManifest.ToolkitManifest)
	}

	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(canonicalMap); err != nil {
		return "", nil, fmt.Errorf("encode canonical profile json: %w", err)
	}
	canonicalJSON := bytes.TrimRight(buf.Bytes(), "\n")
	sum := sha256.Sum256(canonicalJSON)
	digest := fmt.Sprintf("%s:sha256:%x", profile.AlgoVersion, sum)
	return digest, canonicalJSON, nil
}

// canonicalToolkitManifestMap builds the normalized canonical sub-map
// for a toolkit manifest — EXACTLY the value ComputeProfileDigest
// embeds under "toolkit_manifest". Shared so the standalone manifest
// digest and the profile digest can never diverge.
func canonicalToolkitManifestMap(m ToolkitManifest) map[string]any {
	return map[string]any{
		"probed_cli_version":       norm.NFC.String(strings.Trim(m.ProbedCLIVersion, "\ufeff")),
		"universe_evidence_path":   norm.NFC.String(strings.Trim(m.UniverseEvidencePath, "\ufeff")),
		"universe_evidence_digest": strings.ToLower(strings.TrimSpace(m.UniverseEvidenceDigest)),
		"approved_tools":           normalizeStringSlice(m.ApprovedTools, false, false),
		"denied_complement":        normalizeStringSlice(m.DeniedComplement, false, false),
		"expected_hooks":           normalizeStringSlice(m.ExpectedHooks, false, false),
		"expected_skills":          normalizeStringSlice(m.ExpectedSkills, false, false),
		"expected_plugins":         normalizeStringSlice(m.ExpectedPlugins, false, false),
		"turns_bound":              m.TurnsBound,
	}
}

// ComputeToolkitManifestDigest returns the canonical toolkit-manifest
// digest — `sha256:<hex>` over the same normalized canonical JSON
// encoding ComputeProfileDigest embeds under "toolkit_manifest". This is
// THE manifest_digest value the codex launch policy freezes at
// validation and the codex protection-attestation journal operation
// stores: both call this one function, so the frozen launch tuple and
// the durable attestation rows can never disagree on manifest identity.
func ComputeToolkitManifestDigest(manifest ToolkitManifest) (string, error) {
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(canonicalToolkitManifestMap(manifest)); err != nil {
		return "", fmt.Errorf("encode canonical toolkit manifest json: %w", err)
	}
	canonicalJSON := bytes.TrimRight(buf.Bytes(), "\n")
	sum := sha256.Sum256(canonicalJSON)
	return fmt.Sprintf("sha256:%x", sum), nil
}

// canonicalJSONValue re-encodes a raw JSON value verbatim as canonical
// JSON: number literals are preserved (UseNumber), object keys are
// sorted by the encoder, and no insignificant whitespace survives.
func canonicalJSONValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing characters after json value")
	}
	return v, nil
}

// canonicalGranularApproval re-encodes the granular object canonically:
// all five keys present, each value re-encoded verbatim as canonical
// JSON. Equality of two granular policies is byte equality of the
// resulting encoding.
func canonicalGranularApproval(g *CodexGranularApproval) (map[string]any, error) {
	if g == nil {
		return nil, fmt.Errorf("approval_policy granular kind carries no granular object")
	}
	raws := map[string]json.RawMessage{
		"mcp_elicitations":    g.McpElicitations,
		"request_permissions": g.RequestPermissions,
		"rules":               g.Rules,
		"sandbox_approval":    g.SandboxApproval,
		"skill_approval":      g.SkillApproval,
	}
	out := make(map[string]any, len(granularApprovalKeys))
	for _, key := range granularApprovalKeys {
		raw := raws[key]
		if len(raw) == 0 || string(raw) == "null" {
			return nil, fmt.Errorf("granular approval_policy is missing required key %q", key)
		}
		v, err := canonicalJSONValue(raw)
		if err != nil {
			return nil, fmt.Errorf("granular approval_policy key %q: %w", key, err)
		}
		out[key] = v
	}
	return out, nil
}

// requireGranularShapeEvidence fails closed unless committed schema
// evidence pins the sub-value shape of every granular key.
func requireGranularShapeEvidence(g *CodexGranularApproval) error {
	for _, key := range granularApprovalKeys {
		if granularShapeEvidence[key] == "" {
			return fmt.Errorf(
				"granular approval_policy key %q has no committed schema-derived shape evidence; "+
					"granular policies are rejected at profile freeze (fail-closed)", key)
		}
	}
	return nil
}

// canonicalApprovalPolicy encodes the tagged union for the canonical map:
// the string kind yields the normalized enum string; the granular kind
// yields the canonical object.
func canonicalApprovalPolicy(p CodexApprovalPolicy) (any, error) {
	switch p.Kind {
	case CodexApprovalKindString:
		return norm.NFC.String(strings.Trim(p.String, "\ufeff")), nil
	case CodexApprovalKindGranular:
		return canonicalGranularApproval(p.Granular)
	default:
		return nil, fmt.Errorf("approval_policy must be a string or a granular object")
	}
}

func codexScalar(s string) string {
	return norm.NFC.String(strings.Trim(s, "\ufeff"))
}

// canonicalCodexBlock builds the canonical codex-block map (§3.8):
// scalars trimmed + NFC; arrays BOM-trimmed, NFC, deduped, byte-wise
// sorted without lowercasing; writable_roots path-normalized.
func canonicalCodexBlock(c *CodexHarnessSpec) (map[string]any, error) {
	policy, err := canonicalApprovalPolicy(c.ApprovalPolicy)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"app_server_version":  codexScalar(c.AppServerVersion),
		"model_provider":      codexScalar(c.ModelProvider),
		"expected_codex_home": codexScalar(c.ExpectedCodexHome),
		"platform": map[string]any{
			"family": codexScalar(c.Platform.Family),
			"os":     codexScalar(c.Platform.OS),
		},
		"sandbox_policy": map[string]any{
			"type":           codexScalar(c.SandboxPolicy.Type),
			"writable_roots": normalizeStringSlice(c.SandboxPolicy.WritableRoots, false, true),
			"network_access": c.SandboxPolicy.NetworkAccess,
		},
		"approval_policy":              policy,
		"approvals_reviewer":           codexScalar(c.ApprovalsReviewer),
		"expected_mcp_servers":         normalizeStringSlice(c.ExpectedMCPServers, false, false),
		"expected_mcp_tools":           normalizeStringSlice(c.ExpectedMCPTools, false, false),
		"expected_plugin_tools":        normalizeStringSlice(c.ExpectedPluginTools, false, false),
		"expected_instruction_sources": normalizeStringSlice(c.ExpectedInstructionSources, false, false),
		"rules_evidence": map[string]any{
			"verified":     normalizeStringSlice(c.RulesEvidence.Verified, false, false),
			"unverifiable": normalizeStringSlice(c.RulesEvidence.Unverifiable, false, false),
		},
		"event_universe_path":   codexScalar(c.EventUniversePath),
		"event_universe_digest": strings.ToLower(strings.TrimSpace(c.EventUniverseDigest)),
		"tool_inventory_path":   codexScalar(c.ToolInventoryPath),
		"tool_inventory_digest": strings.ToLower(strings.TrimSpace(c.ToolInventoryDigest)),
	}, nil
}

// validateNoCodexBlocks rejects a codex block under cprof-v1/v2.
func (p CanonicalProfile) validateNoCodexBlocks() error {
	for _, h := range p.Harnesses {
		if h.Codex != nil {
			return fmt.Errorf("a codex harness block requires algo_version %q, not %q", "cprof-v3", p.AlgoVersion)
		}
	}
	return nil
}

// validateCodexBlocks freezes every codex harness block: approval
// enum/shape gates and the §3.6 reviewer invariant.
func (p CanonicalProfile) validateCodexBlocks() error {
	for _, h := range p.Harnesses {
		if h.Codex == nil {
			continue
		}
		if err := validateCodexHarnessBlock(h.Codex); err != nil {
			return err
		}
	}
	return nil
}

// validateCodexHarnessBlock enforces the freeze-time codex gates:
// sandbox_policy.type enum {read-only, workspace-write} (spec §5 —
// danger-full-access is forbidden in any launch or code path),
// approval_policy enum {untrusted, on-request, never, granular object},
// granular shape evidence (fail-closed), and approvals_reviewer == user.
func validateCodexHarnessBlock(c *CodexHarnessSpec) error {
	switch codexScalar(c.SandboxPolicy.Type) {
	case "read-only", "workspace-write":
	default:
		return fmt.Errorf("unsupported sandbox_policy type %q (expected read-only or workspace-write; danger-full-access is forbidden)", c.SandboxPolicy.Type)
	}
	switch c.ApprovalPolicy.Kind {
	case CodexApprovalKindString:
		switch norm.NFC.String(strings.Trim(c.ApprovalPolicy.String, "\ufeff")) {
		case "untrusted", "on-request", "never":
		default:
			return fmt.Errorf("unsupported approval_policy %q (expected untrusted, on-request, never, or a granular object)", c.ApprovalPolicy.String)
		}
	case CodexApprovalKindGranular:
		if _, err := canonicalGranularApproval(c.ApprovalPolicy.Granular); err != nil {
			return err
		}
		if err := requireGranularShapeEvidence(c.ApprovalPolicy.Granular); err != nil {
			return err
		}
	default:
		return fmt.Errorf("approval_policy must be a string or a granular object")
	}
	if codexScalar(c.ApprovalsReviewer) != "user" {
		return fmt.Errorf("approvals_reviewer %q must be %q: only the user answers approvals inside Council's visibility", c.ApprovalsReviewer, "user")
	}
	// Inventory-evidence gate AT FREEZE (AC-009 §3.8 errata): no committed
	// evidence path proves a non-empty native MCP/plugin tool inventory,
	// so a run must never durably freeze a profile the production adapter
	// can never launch. Only empty inventories freeze.
	if len(c.ExpectedMCPServers) > 0 || len(c.ExpectedMCPTools) > 0 || len(c.ExpectedPluginTools) > 0 {
		return fmt.Errorf("expected_mcp_servers, expected_mcp_tools, and expected_plugin_tools must be empty: no committed evidence path proves a non-empty native tool inventory, so such a profile is not launchable and must not be frozen")
	}
	return nil
}

// canonicalAgyBlock builds the canonical agy-block map (§3.7): scalars
// trimmed + NFC; arrays BOM-trimmed, NFC, deduped, byte-wise sorted
// without lowercasing; binary_path/expected_home/plugins_evidence_path/
// init_evidence_path/tool_coverage_path path-normalized; digests
// lowercased; booleans/integers verbatim.
func canonicalAgyBlock(a *AgyHarnessSpec) map[string]any {
	return map[string]any{
		"cli_version":                    codexScalar(a.CLIVersion),
		"binary_path":                    normalizePathScalar(a.BinaryPath),
		"binary_digest":                  strings.ToLower(strings.TrimSpace(a.BinaryDigest)),
		"expected_home":                  normalizePathScalar(a.ExpectedHome),
		"platform":                       map[string]any{"family": codexScalar(a.Platform.Family), "os": codexScalar(a.Platform.OS)},
		"permission_mode":                codexScalar(a.PermissionMode),
		"execution_mode":                 codexScalar(a.ExecutionMode),
		"sandbox":                        a.Sandbox,
		"print_timeout_backstop_seconds": a.PrintTimeoutBackstopSeconds,
		"expected_tools":                 normalizeStringSlice(a.ExpectedTools, false, false),
		"expected_mcp_servers":           normalizeStringSlice(a.ExpectedMCPServers, false, false),
		"expected_mcp_tools":             normalizeStringSlice(a.ExpectedMCPTools, false, false),
		"expected_plugin_tools":          normalizeStringSlice(a.ExpectedPluginTools, false, false),
		"default_required_tools":         normalizeStringSlice(a.DefaultRequiredTools, false, false),
		"hooks_evidence": map[string]any{
			"verified":     normalizeStringSlice(a.HooksEvidence.Verified, false, false),
			"unverifiable": normalizeStringSlice(a.HooksEvidence.Unverifiable, false, false),
		},
		"expected_skills":         normalizeStringSlice(a.ExpectedSkills, false, false),
		"plugins_evidence_path":   normalizePathScalar(a.PluginsEvidencePath),
		"plugins_evidence_digest": strings.ToLower(strings.TrimSpace(a.PluginsEvidenceDigest)),
		"hooks_config_digest":     strings.ToLower(strings.TrimSpace(a.HooksConfigDigest)),
		"required_hooks":          normalizeStringSlice(a.RequiredHooks, false, false),
		"init_evidence_path":      normalizePathScalar(a.InitEvidencePath),
		"init_evidence_digest":    strings.ToLower(strings.TrimSpace(a.InitEvidenceDigest)),
		"tool_coverage_path":      normalizePathScalar(a.ToolCoveragePath),
		"tool_coverage_digest":    strings.ToLower(strings.TrimSpace(a.ToolCoverageDigest)),
	}
}

// validateNoAgyBlocks rejects an agy block under any algo_version other
// than cprof-v4.
func (p CanonicalProfile) validateNoAgyBlocks() error {
	for _, h := range p.Harnesses {
		if h.Agy != nil {
			return fmt.Errorf("an agy harness block requires algo_version %q, not %q", "cprof-v4", p.AlgoVersion)
		}
	}
	return nil
}

// validateAgyBlocks freezes every agy harness block present (an agy
// block is optional even under cprof-v4: one run profile serves all
// four harnesses, and a v4 run without an Agy contributor remains
// valid).
func (p CanonicalProfile) validateAgyBlocks() error {
	for _, h := range p.Harnesses {
		if h.Agy == nil {
			continue
		}
		if err := validateAgyHarnessBlock(h.Agy); err != nil {
			return err
		}
	}
	return nil
}

var semverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// isJSONPointer accepts the empty pointer (whole document) or a pointer
// starting with "/" (RFC 6901).
func isJSONPointer(s string) bool {
	return s == "" || strings.HasPrefix(s, "/")
}

func requireDistinctSHA256(field, digest string) error {
	if err := ValidateSHA256Digest(digest); err != nil {
		return fmt.Errorf("agy block %s: %w", field, err)
	}
	return nil
}

func requireNoDuplicates(field string, items []string) error {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, dup := seen[item]; dup {
			return fmt.Errorf("agy block %s contains duplicate entry %q", field, item)
		}
		seen[item] = struct{}{}
	}
	return nil
}

// validateAgyHarnessBlock enforces the freeze-time agy gates (AC-010
// spec §3.7): shape and enum rules only — evidence-root I/O (re-hashing
// and strictly decoding init_evidence_path/tool_coverage_path/
// plugins_evidence_path, resolving required_hooks against the live
// hooks.json) is the adapter's job (internal/adapter/agy.ValidateAgyHarness),
// mirroring how the codex family splits freeze-time shape checks from
// adapter-time evidence checks.
func validateAgyHarnessBlock(a *AgyHarnessSpec) error {
	if !semverPattern.MatchString(codexScalar(a.CLIVersion)) {
		return fmt.Errorf("agy block cli_version %q must be MAJOR.MINOR.PATCH", a.CLIVersion)
	}
	binaryPath := normalizePathScalar(a.BinaryPath)
	if binaryPath == "" || !strings.HasPrefix(binaryPath, "/") {
		return fmt.Errorf("agy block binary_path %q must be absolute", a.BinaryPath)
	}
	if err := requireDistinctSHA256("binary_digest", a.BinaryDigest); err != nil {
		return err
	}
	expectedHome := normalizePathScalar(a.ExpectedHome)
	if expectedHome == "" || !strings.HasPrefix(expectedHome, "/") {
		return fmt.Errorf("agy block expected_home %q must be absolute", a.ExpectedHome)
	}
	if codexScalar(a.Platform.OS) == "" {
		return fmt.Errorf("agy block lacks the platform os")
	}
	if codexScalar(a.Platform.Family) == "" {
		return fmt.Errorf("agy block lacks the platform family")
	}
	switch codexScalar(a.PermissionMode) {
	case "request-review", "strict":
	default:
		return fmt.Errorf("unsupported permission_mode %q (expected request-review or strict; no bypass value is representable)", a.PermissionMode)
	}
	switch codexScalar(a.ExecutionMode) {
	case "default", "accept-edits", "plan":
	default:
		return fmt.Errorf("unsupported execution_mode %q (expected default, accept-edits, or plan)", a.ExecutionMode)
	}
	if a.PrintTimeoutBackstopSeconds < 60 {
		return fmt.Errorf("agy block print_timeout_backstop_seconds %d must be >= 60", a.PrintTimeoutBackstopSeconds)
	}
	if len(a.ExpectedTools) == 0 {
		return fmt.Errorf("agy block requires a non-empty expected_tools inventory")
	}
	if err := requireNoDuplicates("expected_tools", a.ExpectedTools); err != nil {
		return err
	}
	if a.ExpectedMCPServers == nil || a.ExpectedMCPTools == nil || a.ExpectedPluginTools == nil {
		return fmt.Errorf("agy block requires expected_mcp_servers, expected_mcp_tools, and expected_plugin_tools ([] when none)")
	}
	// Inventory-evidence gate AT FREEZE (spec §3.7): no committed
	// evidence path proves a non-empty native MCP/plugin tool inventory
	// for Agy either, so only empty inventories freeze.
	if len(a.ExpectedMCPServers) > 0 || len(a.ExpectedMCPTools) > 0 || len(a.ExpectedPluginTools) > 0 {
		return fmt.Errorf("expected_mcp_servers, expected_mcp_tools, and expected_plugin_tools must be empty: no committed evidence path proves a non-empty native tool inventory, so such a profile is not launchable and must not be frozen")
	}
	if a.DefaultRequiredTools == nil {
		return fmt.Errorf("agy block requires default_required_tools ([] when none)")
	}
	if err := requireNoDuplicates("default_required_tools", a.DefaultRequiredTools); err != nil {
		return err
	}
	toolSet := make(map[string]struct{}, len(a.ExpectedTools))
	for _, t := range a.ExpectedTools {
		toolSet[t] = struct{}{}
	}
	for _, t := range a.DefaultRequiredTools {
		if _, ok := toolSet[t]; !ok {
			return fmt.Errorf("agy block default_required_tools entry %q is not in expected_tools", t)
		}
	}
	if a.ExpectedSkills == nil {
		return fmt.Errorf("agy block requires expected_skills ([] when none)")
	}
	if err := requireNoDuplicates("expected_skills", a.ExpectedSkills); err != nil {
		return err
	}
	for _, s := range a.ExpectedSkills {
		if s == "" || strings.ContainsAny(s, "/\\") {
			return fmt.Errorf("agy block expected_skills entry %q must be a bare directory name (no separators)", s)
		}
	}
	if strings.TrimSpace(a.InitEvidencePath) == "" {
		return fmt.Errorf("agy block requires init_evidence_path")
	}
	if err := requireDistinctSHA256("init_evidence_digest", a.InitEvidenceDigest); err != nil {
		return err
	}
	if strings.TrimSpace(a.ToolCoveragePath) == "" {
		return fmt.Errorf("agy block requires tool_coverage_path")
	}
	if err := requireDistinctSHA256("tool_coverage_digest", a.ToolCoverageDigest); err != nil {
		return err
	}
	if strings.TrimSpace(a.PluginsEvidencePath) == "" {
		return fmt.Errorf("agy block requires plugins_evidence_path")
	}
	if err := requireDistinctSHA256("plugins_evidence_digest", a.PluginsEvidenceDigest); err != nil {
		return err
	}
	if err := requireDistinctSHA256("hooks_config_digest", a.HooksConfigDigest); err != nil {
		return err
	}
	if len(a.RequiredHooks) == 0 {
		return fmt.Errorf("agy block requires a non-empty required_hooks list")
	}
	if err := requireNoDuplicates("required_hooks", a.RequiredHooks); err != nil {
		return err
	}
	for i, ptr := range a.RequiredHooks {
		if !isJSONPointer(ptr) {
			return fmt.Errorf("agy block required_hooks entry %q is not an RFC 6901 JSON pointer", ptr)
		}
		if i > 0 && a.RequiredHooks[i-1] > ptr {
			return fmt.Errorf("agy block required_hooks must be sorted, %q precedes %q", a.RequiredHooks[i-1], ptr)
		}
	}
	if len(a.HooksEvidence.Verified) == 0 && len(a.HooksEvidence.Unverifiable) == 0 {
		return fmt.Errorf("agy block hooks_evidence requires at least one of verified/unverifiable to be present")
	}
	return nil
}

// ParseCanonicalProfileJSON unmarshals and validates profile JSON, rejecting unknown fields.
func ParseCanonicalProfileJSON(data []byte) (CanonicalProfile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var prof CanonicalProfile
	if err := dec.Decode(&prof); err != nil {
		return CanonicalProfile{}, fmt.Errorf("decode canonical profile json: %w", err)
	}

	if dec.More() {
		return CanonicalProfile{}, errors.New("trailing characters after profile json")
	}

	switch prof.AlgoVersion {
	case "cprof-v1":
		if err := prof.validateNoCodexBlocks(); err != nil {
			return CanonicalProfile{}, err
		}
		if err := prof.validateNoAgyBlocks(); err != nil {
			return CanonicalProfile{}, err
		}
	case "cprof-v2", "cprof-v3":
		if prof.ToolkitManifest == nil {
			return CanonicalProfile{}, fmt.Errorf("algo_version %q requires toolkit_manifest", prof.AlgoVersion)
		}
		if prof.AlgoVersion == "cprof-v2" {
			if err := prof.validateNoCodexBlocks(); err != nil {
				return CanonicalProfile{}, err
			}
		}
		if err := prof.validateNoAgyBlocks(); err != nil {
			return CanonicalProfile{}, err
		}
	case "cprof-v4":
		if prof.ToolkitManifest == nil {
			return CanonicalProfile{}, fmt.Errorf("algo_version %q requires toolkit_manifest", prof.AlgoVersion)
		}
	default:
		return CanonicalProfile{}, fmt.Errorf("unsupported profile algo_version: %q (expected %q, %q, %q or %q)", prof.AlgoVersion, "cprof-v1", "cprof-v2", "cprof-v3", "cprof-v4")
	}
	switch prof.WorkspaceMode {
	case "none", "readonly", "isolated_branch":
	default:
		return CanonicalProfile{}, fmt.Errorf("unsupported workspace_mode: %q", prof.WorkspaceMode)
	}
	switch prof.IsolationStrictness {
	case "strict", "permissive_dev":
	default:
		return CanonicalProfile{}, fmt.Errorf("unsupported isolation_strictness: %q", prof.IsolationStrictness)
	}
	switch prof.NetworkMode {
	case "none", "allowlist", "unrestricted":
	default:
		return CanonicalProfile{}, fmt.Errorf("unsupported network_mode: %q", prof.NetworkMode)
	}

	return prof, nil
}
