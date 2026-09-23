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
// "cprof-v1:sha256:" + hex(sha256(canonical_profile_json))
func ComputeProfileDigest(profile CanonicalProfile) (string, []byte, error) {
	switch profile.AlgoVersion {
	case "cprof-v1":
		if profile.ToolkitManifest != nil {
			return "", nil, fmt.Errorf("toolkit_manifest requires algo_version %q, not %q", "cprof-v2", profile.AlgoVersion)
		}
	case "cprof-v2":
		if profile.ToolkitManifest == nil {
			return "", nil, fmt.Errorf("algo_version %q requires toolkit_manifest", profile.AlgoVersion)
		}
		if err := profile.ValidateForClaude(); err != nil {
			return "", nil, err
		}
	default:
		return "", nil, fmt.Errorf("unsupported profile algo_version: %q (expected %q or %q)", profile.AlgoVersion, "cprof-v1", "cprof-v2")
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
		harnessesMap[cleanedHName] = map[string]any{
			"extra_env_allowlist": normalizeStringSlice(hSpec.ExtraEnvAllowlist, false, false),
			"model":               norm.NFC.String(strings.Trim(hSpec.Model, "\ufeff")),
			"native_auth_mode":    norm.NFC.String(strings.Trim(hSpec.NativeAuthMode, "\ufeff")),
		}
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

	if profile.AlgoVersion == "cprof-v2" {
		m := profile.ToolkitManifest.ToolkitManifest
		canonicalMap["toolkit_manifest"] = map[string]any{
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

	if prof.AlgoVersion != "cprof-v1" && prof.AlgoVersion != "cprof-v2" {
		return CanonicalProfile{}, fmt.Errorf("unsupported profile algo_version: %q (expected %q or %q)", prof.AlgoVersion, "cprof-v1", "cprof-v2")
	}
	if prof.AlgoVersion == "cprof-v2" && prof.ToolkitManifest == nil {
		return CanonicalProfile{}, fmt.Errorf("algo_version %q requires toolkit_manifest", prof.AlgoVersion)
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
