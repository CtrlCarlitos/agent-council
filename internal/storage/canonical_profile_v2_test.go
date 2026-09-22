package storage

import (
	"strings"
	"testing"
)

func v2Manifest() ToolkitManifest {
	return ToolkitManifest{
		ProbedCLIVersion:       "2.1.278",
		UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
		UniverseEvidenceDigest: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		ApprovedTools:          []string{"Read", "Glob", "Grep"},
		DeniedComplement:       []string{"Bash", "Write"},
		ExpectedHooks:          []string{"SessionStart:startup", "PreToolUse"},
		ExpectedSkills:         []string{"research"},
		ExpectedPlugins:        []string{"superpowers"},
		TurnsBound:             8,
	}
}

func v2Profile() CanonicalProfile {
	p := CanonicalProfile{
		AlgoVersion:         "cprof-v2",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"opencode", "git", "go"},
		Harnesses: map[string]HarnessProfileSpec{
			"claude": {Model: "haiku", NativeAuthMode: "inherited_host_keychain"},
		},
	}
	p.ToolkitManifest = &ToolkitManifestSpec{ToolkitManifest: v2Manifest()}
	return p
}

// v2 digest is deterministic and distinct from the v1 encoding of the
// same core fields.
func TestCanonicalProfileV2_DeterministicDistinctFromV1(t *testing.T) {
	d1, canon1, err := ComputeProfileDigest(v2Profile())
	if err != nil {
		t.Fatalf("v2 digest: %v", err)
	}
	if !strings.HasPrefix(d1, "cprof-v2:sha256:") {
		t.Fatalf("v2 digest prefix, got %q", d1)
	}
	d2, _, err := ComputeProfileDigest(v2Profile())
	if err != nil || d2 != d1 {
		t.Fatalf("v2 digest must be deterministic: %q vs %q (%v)", d1, d2, err)
	}

	v1 := v2Profile()
	v1.AlgoVersion = "cprof-v1"
	v1.ToolkitManifest = nil
	dv1, canonV1, err := ComputeProfileDigest(v1)
	if err != nil {
		t.Fatalf("v1 digest: %v", err)
	}
	if !strings.HasPrefix(dv1, "cprof-v1:sha256:") {
		t.Fatalf("v1 digest prefix, got %q", dv1)
	}
	if d1 == dv1 {
		t.Fatal("v2 digest must differ from v1")
	}
	if strings.Contains(string(canon1), "\"toolkit_manifest\"") == false {
		t.Fatalf("v2 canonical JSON must carry the manifest: %s", canon1)
	}
	if strings.Contains(string(canonV1), "toolkit_manifest") {
		t.Fatalf("v1 canonical JSON must not carry a manifest: %s", canonV1)
	}
}

// Manifest arrays are normalized: trimmed, empty-rejected, duplicate-
// rejected, byte-wise sorted (case-sensitive).
func TestCanonicalProfileV2_ManifestNormalization(t *testing.T) {
	p := v2Profile()
	p.ToolkitManifest.ToolkitManifest.ApprovedTools = []string{"Glob", "Read", "Glob", "  ", ""}
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("empty manifest entries must be rejected")
	}

	p.ToolkitManifest.ToolkitManifest.ApprovedTools = []string{"Read", "Glob"}
	d1, _, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	// Order-insensitive: a different input order normalizes to the same digest.
	p.ToolkitManifest.ToolkitManifest.ApprovedTools = []string{"Glob", "Read"}
	d2, canon, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("manifest normalization must be order-insensitive: %q vs %q", d1, d2)
	}
	if !strings.Contains(string(canon), `["Glob","Read"]`) {
		t.Fatalf("normalized array must be sorted, got %s", canon)
	}
	// Case-sensitive: "read" is a different entry from "Read".
	p.ToolkitManifest.ToolkitManifest.ApprovedTools = []string{"Read", "read"}
	if _, _, err := ComputeProfileDigest(p); err != nil {
		t.Fatalf("case-distinct entries are legal: %v", err)
	}
}

// v2 requires a complete manifest; missing fields are rejected.
func TestCanonicalProfileV2_RequiresCompleteManifest(t *testing.T) {
	p := v2Profile()
	p.ToolkitManifest.ToolkitManifest.ProbedCLIVersion = ""
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("incomplete manifest must be rejected")
	}
	p.ToolkitManifest.ToolkitManifest.ProbedCLIVersion = "2.1.278"
	p.ToolkitManifest.ToolkitManifest.UniverseEvidenceDigest = "not-a-digest"
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("malformed universe digest must be rejected")
	}

	// Uppercase hex and non-hex content are rejected: the frozen form is
	// exactly sha256:<64 lowercase hex>.
	p.ToolkitManifest.ToolkitManifest.UniverseEvidenceDigest = "sha256:" + strings.Repeat("A", 64)
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("uppercase universe digest must be rejected")
	}
	p.ToolkitManifest.ToolkitManifest.UniverseEvidenceDigest = "sha256:" + strings.Repeat("g", 64)
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("non-hex universe digest must be rejected")
	}

	// The evidence path is required and must be repo-relative.
	p.ToolkitManifest.ToolkitManifest.UniverseEvidenceDigest = "sha256:" + strings.Repeat("a", 64)
	p.ToolkitManifest.ToolkitManifest.UniverseEvidencePath = ""
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("missing universe evidence path must be rejected")
	}
	p.ToolkitManifest.ToolkitManifest.UniverseEvidencePath = "../outside/universe.json"
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("escaping universe evidence path must be rejected")
	}
	// Restore a valid path for the remaining positive assertions.
	p.ToolkitManifest.ToolkitManifest.UniverseEvidencePath =
		"docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json"

	// The frozen turns bound must be positive and is digest-covered.
	p.ToolkitManifest.ToolkitManifest.TurnsBound = 0
	if _, _, err := ComputeProfileDigest(p); err == nil {
		t.Fatal("non-positive turns bound must be rejected")
	}
	p.ToolkitManifest.ToolkitManifest.TurnsBound = 8
	d, canon, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("valid turns bound must pass: %v", err)
	}
	if !strings.Contains(string(canon), `"turns_bound":8`) {
		t.Fatal("turns_bound must be part of the canonical profile JSON")
	}
	_ = d
}

// Claude eligibility: v2 + complete manifest passes; v1 (or v2 without a
// manifest) fails with the typed unsupported-profile error.
func TestCanonicalProfileV2_ClaudeEligibility(t *testing.T) {
	p := v2Profile()
	if err := p.ValidateForClaude(); err != nil {
		t.Fatalf("v2 profile must be eligible: %v", err)
	}

	v1 := v2Profile()
	v1.AlgoVersion = "cprof-v1"
	v1.ToolkitManifest = nil
	err := v1.ValidateForClaude()
	if err == nil {
		t.Fatal("v1 profile must be ineligible for Claude")
	}
	var unsupported *ErrUnsupportedProfile
	if !asErrUnsupported(err, &unsupported) {
		t.Fatalf("expected ErrUnsupportedProfile, got %T: %v", err, err)
	}

	v2NoManifest := v2Profile()
	v2NoManifest.ToolkitManifest = nil
	if err := v2NoManifest.ValidateForClaude(); err == nil {
		t.Fatal("v2 without a manifest must be ineligible for Claude")
	}
}

func asErrUnsupported(err error, target **ErrUnsupportedProfile) bool {
	if e, ok := err.(*ErrUnsupportedProfile); ok {
		*target = e
		return true
	}
	return false
}

// v1 profiles are unaffected: the existing golden expectations still hold.
func TestCanonicalProfileV1_UnchangedByV2(t *testing.T) {
	p := CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"git", "go"},
		Harnesses: map[string]HarnessProfileSpec{
			"claude": {Model: "m", NativeAuthMode: "inherited_host_keychain"},
		},
	}
	d, _, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("v1 digest: %v", err)
	}
	if !strings.HasPrefix(d, "cprof-v1:sha256:") {
		t.Fatalf("v1 prefix, got %q", d)
	}
}

// Round-trip: the v2 canonical JSON parses through the same strict
// decoder GetRunProfile uses.
func TestCanonicalProfileV2_RoundTripParse(t *testing.T) {
	p := v2Profile()
	_, canon, err := ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	parsed, err := ParseCanonicalProfileJSON(canon)
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if parsed.AlgoVersion != "cprof-v2" {
		t.Fatalf("round-trip algo, got %q", parsed.AlgoVersion)
	}
	if parsed.ToolkitManifest == nil {
		t.Fatal("round-trip must preserve the manifest")
	}
}
