package storage

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ToolkitManifest is the frozen toolkit policy added by cprof-v2. It is
// covered by the profile digest and governs Claude tool enforcement and
// toolkit evidence (AC-008 spec §3.8/§3.9).
type ToolkitManifest struct {
	ProbedCLIVersion       string   `json:"probed_cli_version"`
	UniverseEvidencePath   string   `json:"universe_evidence_path"`
	UniverseEvidenceDigest string   `json:"universe_evidence_digest"`
	ApprovedTools          []string `json:"approved_tools"`
	DeniedComplement       []string `json:"denied_complement"`
	ExpectedHooks          []string `json:"expected_hooks"`
	ExpectedSkills         []string `json:"expected_skills"`
	ExpectedPlugins        []string `json:"expected_plugins"`
	// TurnsBound is the frozen --max-turns bound for every Claude turn
	// invocation of this run. Covered by the profile digest; the ONLY
	// source of the bound at launch time.
	TurnsBound int `json:"turns_bound"`
}

// ToolkitManifestSpec attaches the manifest to the profile. The embedded
// struct's fields appear INLINE under "toolkit_manifest" (no wrapper
// level), matching the canonical v2 encoding; cprof-v1 profiles marshal
// without the field entirely (additive v1→v2 compatibility).
type ToolkitManifestSpec struct {
	ToolkitManifest
}

// ErrUnsupportedProfile reports a profile that is not eligible for the
// requested adapter (e.g. cprof-v1 for Claude production sessions).
type ErrUnsupportedProfile struct {
	AlgoVersion string
	Reason      string
}

func (e *ErrUnsupportedProfile) Error() string {
	return fmt.Sprintf("profile algorithm %q is unsupported for this adapter: %s", e.AlgoVersion, e.Reason)
}

// ValidateForClaude fails closed for any profile that does not carry the
// cprof-v2, cprof-v3, or cprof-v4 algorithm with a complete toolkit
// manifest. The additive codex block (cprof-v3+) and agy block
// (cprof-v4 only) are ignored here: one run profile serves all four
// harnesses, and Claude contributors on a v3 or v4 run remain valid
// (AC-009 spec §3.8 / AC-010 spec §3.7 compatibility matrix).
func (p CanonicalProfile) ValidateForClaude() error {
	if p.AlgoVersion != "cprof-v2" && p.AlgoVersion != "cprof-v3" && p.AlgoVersion != "cprof-v4" {
		return &ErrUnsupportedProfile{
			AlgoVersion: p.AlgoVersion,
			Reason:      "the Claude adapter requires cprof-v2, cprof-v3, or cprof-v4 with a frozen toolkit manifest",
		}
	}
	if p.ToolkitManifest == nil {
		return &ErrUnsupportedProfile{
			AlgoVersion: p.AlgoVersion,
			Reason:      "cprof-v2 profile lacks the required toolkit manifest",
		}
	}
	m := p.ToolkitManifest.ToolkitManifest
	if strings.TrimSpace(m.ProbedCLIVersion) == "" {
		return &ErrUnsupportedProfile{AlgoVersion: p.AlgoVersion, Reason: "manifest lacks the probed CLI version"}
	}
	if err := validateUniverseEvidencePath(m.UniverseEvidencePath); err != nil {
		return &ErrUnsupportedProfile{AlgoVersion: p.AlgoVersion, Reason: err.Error()}
	}
	if err := validateManifestList("approved_tools", m.ApprovedTools, true); err != nil {
		return &ErrUnsupportedProfile{AlgoVersion: p.AlgoVersion, Reason: err.Error()}
	}
	if err := validateManifestList("denied_complement", m.DeniedComplement, false); err != nil {
		return &ErrUnsupportedProfile{AlgoVersion: p.AlgoVersion, Reason: err.Error()}
	}
	if err := validateManifestList("expected_hooks", m.ExpectedHooks, false); err != nil {
		return &ErrUnsupportedProfile{AlgoVersion: p.AlgoVersion, Reason: err.Error()}
	}
	if err := validateManifestList("expected_skills", m.ExpectedSkills, false); err != nil {
		return &ErrUnsupportedProfile{AlgoVersion: p.AlgoVersion, Reason: err.Error()}
	}
	if err := validateManifestList("expected_plugins", m.ExpectedPlugins, false); err != nil {
		return &ErrUnsupportedProfile{AlgoVersion: p.AlgoVersion, Reason: err.Error()}
	}
	if err := ValidateSHA256Digest(m.UniverseEvidenceDigest); err != nil {
		return &ErrUnsupportedProfile{AlgoVersion: p.AlgoVersion, Reason: "universe_evidence_digest: " + err.Error()}
	}
	if m.TurnsBound <= 0 {
		return &ErrUnsupportedProfile{AlgoVersion: p.AlgoVersion, Reason: "manifest turns_bound must be positive"}
	}
	return nil
}

// validateUniverseEvidencePath enforces the repo-relative provenance
// path form: forward slashes, no leading ./ or /, no .. components,
// trimmed, non-empty.
func validateUniverseEvidencePath(p string) error {
	s := strings.TrimSpace(p)
	if s == "" {
		return fmt.Errorf("universe_evidence_path is required")
	}
	if filepath.IsAbs(s) || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") ||
		strings.HasPrefix(s, "../") || s == ".." || strings.Contains(s, `\`) {
		return fmt.Errorf("universe_evidence_path %q must be repo-relative with forward slashes", p)
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("universe_evidence_path %q contains invalid path components", p)
		}
	}
	return nil
}

// ValidateSHA256Digest requires the exact form sha256:<64 lowercase hex>.
func ValidateSHA256Digest(d string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(d, prefix) {
		return fmt.Errorf("must be %s<64 lowercase hex>", prefix)
	}
	hex := strings.TrimPrefix(d, prefix)
	if len(hex) != 64 {
		return fmt.Errorf("must be %s<64 lowercase hex>", prefix)
	}
	for _, c := range hex {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("must be %s<64 lowercase hex>, found %q", prefix, c)
		}
	}
	return nil
}

// validateManifestList normalizes a manifest array per spec §3.11: trim,
// reject empties, reject duplicates, byte-wise lexicographic order
// (case-sensitive). required lists must be non-empty.
func validateManifestList(field string, list []string, required bool) error {
	seen := make(map[string]struct{}, len(list))
	for i, item := range list {
		s := strings.TrimSpace(item)
		if s == "" {
			return fmt.Errorf("toolkit manifest %s[%d] is empty after trim", field, i)
		}
		if _, dup := seen[s]; dup {
			return fmt.Errorf("toolkit manifest %s contains duplicate entry %q", field, s)
		}
		seen[s] = struct{}{}
	}
	if required && len(list) == 0 {
		return fmt.Errorf("toolkit manifest %s must not be empty", field)
	}
	return nil
}
