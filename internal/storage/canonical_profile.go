package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
	ApprovalsReviewer          string                 `json:"approvals_reviewer"`
	ExpectedMCPServers         []string               `json:"expected_mcp_servers"`
	ExpectedInstructionSources []string               `json:"expected_instruction_sources"`
	RulesEvidence              CodexRulesEvidenceSpec `json:"rules_evidence"`
	EventUniversePath          string                 `json:"event_universe_path"`
	EventUniverseDigest        string                 `json:"event_universe_digest"`
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

// ComputeProfileDigest computes the canonical profile digest and normalized JSON representation:
// "cprof-v1:sha256:" / "cprof-v2:sha256:" / "cprof-v3:sha256:" + hex(sha256(canonical_profile_json))
func ComputeProfileDigest(profile CanonicalProfile) (string, []byte, error) {
	switch profile.AlgoVersion {
	case "cprof-v1":
		if profile.ToolkitManifest != nil {
			return "", nil, fmt.Errorf("toolkit_manifest requires algo_version %q, not %q", "cprof-v2", profile.AlgoVersion)
		}
		if err := profile.validateNoCodexBlocks(); err != nil {
			return "", nil, err
		}
	case "cprof-v2", "cprof-v3":
		if profile.ToolkitManifest == nil {
			return "", nil, fmt.Errorf("algo_version %q requires toolkit_manifest", profile.AlgoVersion)
		}
		if err := profile.ValidateForClaude(); err != nil {
			return "", nil, err
		}
		if profile.AlgoVersion == "cprof-v2" {
			if err := profile.validateNoCodexBlocks(); err != nil {
				return "", nil, err
			}
		} else if err := profile.validateCodexBlocks(); err != nil {
			return "", nil, err
		}
	default:
		return "", nil, fmt.Errorf("unsupported profile algo_version: %q (expected %q, %q or %q)", profile.AlgoVersion, "cprof-v1", "cprof-v2", "cprof-v3")
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

	if profile.AlgoVersion == "cprof-v2" || profile.AlgoVersion == "cprof-v3" {
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
		"expected_instruction_sources": normalizeStringSlice(c.ExpectedInstructionSources, false, false),
		"rules_evidence": map[string]any{
			"verified":     normalizeStringSlice(c.RulesEvidence.Verified, false, false),
			"unverifiable": normalizeStringSlice(c.RulesEvidence.Unverifiable, false, false),
		},
		"event_universe_path":   codexScalar(c.EventUniversePath),
		"event_universe_digest": strings.ToLower(strings.TrimSpace(c.EventUniverseDigest)),
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
	case "cprof-v2", "cprof-v3":
		if prof.ToolkitManifest == nil {
			return CanonicalProfile{}, fmt.Errorf("algo_version %q requires toolkit_manifest", prof.AlgoVersion)
		}
		if prof.AlgoVersion == "cprof-v2" {
			if err := prof.validateNoCodexBlocks(); err != nil {
				return CanonicalProfile{}, err
			}
		}
	default:
		return CanonicalProfile{}, fmt.Errorf("unsupported profile algo_version: %q (expected %q, %q or %q)", prof.AlgoVersion, "cprof-v1", "cprof-v2", "cprof-v3")
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
