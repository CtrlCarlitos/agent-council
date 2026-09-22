package storage

import (
	"fmt"
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
// cprof-v2 algorithm with a complete toolkit manifest.
func (p CanonicalProfile) ValidateForClaude() error {
	if p.AlgoVersion != "cprof-v2" {
		return &ErrUnsupportedProfile{
			AlgoVersion: p.AlgoVersion,
			Reason:      "the Claude adapter requires cprof-v2 with a frozen toolkit manifest",
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
	if !strings.HasPrefix(m.UniverseEvidenceDigest, "sha256:") ||
		len(m.UniverseEvidenceDigest) != len("sha256:"+strings.Repeat("0", 64)) {
		return &ErrUnsupportedProfile{AlgoVersion: p.AlgoVersion, Reason: "universe_evidence_digest must be sha256:<64 lowercase hex>"}
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
