package claude

import (
	"strings"
	"testing"
)

func validAttestation() ProtectionAttestation {
	return ProtectionAttestation{
		ClaudeVersion:  "2.1.278",
		Platform:       "linux/amd64",
		ManifestDigest: "cprof-v2:sha256:def",
		TemplateDigest: "ctmpl-v1:sha256:abc",
		Records: []ProbeRecord{
			{ToolClass: ProbeBash, ToolName: "Bash", Denied: true,
				EnforcingCapability: CapGuardrailHook, DenialText: "guardrail denied"},
			{ToolClass: ProbeRead, ToolName: "Read", Denied: true,
				EnforcingCapability: CapCwdBoundary, DenialText: "outside allowed directories"},
			{ToolClass: ProbeMCP, ToolName: "mcp__serena__find_symbol", Denied: true,
				EnforcingCapability: CapPermissionDenial, DenialText: "permission denied by policy"},
		},
		ProbedAt: "2026-09-22T00:00:00Z",
		Actor:    "operator",
	}
}

// Independently computed fixed vector (python3 framing, spec §3.6 field
// order: version, platform, manifest digest, template digest, framed
// records with class/name/target/denied/capability/excerpt, probed_at,
// actor). The encoder must reproduce THIS digest, not merely a stable
// one.
func TestProtectionAttestation_FixedGoldenVector(t *testing.T) {
	// Independently recomputed for the numeric enum sort order
	// (read, bash, mcp).
	want := "cprot-v1:sha256:dda4ff20431e3890d4a9203c6b6fb7866629e4550e1321e3d98ae439065a1402"
	a := validAttestation()
	got, err := a.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if got != want {
		t.Fatalf("fixed golden vector mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestProtectionAttestation_CanonicalOrderAndDeterminism(t *testing.T) {
	a := validAttestation()
	d1, err := a.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	// Reordering the input records must not change the digest.
	reordered := a
	reordered.Records = []ProbeRecord{a.Records[2], a.Records[0], a.Records[1]}
	d2, err := reordered.Digest()
	if err != nil {
		t.Fatalf("reordered digest: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("record order must be canonicalized: %q vs %q", d1, d2)
	}

	// Any binding-field change changes the digest.
	changedVersion := a
	changedVersion.ClaudeVersion = "2.1.279"
	if d, _ := changedVersion.Digest(); d == d1 {
		t.Fatal("version change must change the attestation digest")
	}
	changedTemplate := a
	changedTemplate.TemplateDigest = "ctmpl-v1:sha256:different"
	if d, _ := changedTemplate.Digest(); d == d1 {
		t.Fatal("template change must change the attestation digest")
	}
	changedPlatform := a
	changedPlatform.Platform = "windows/amd64"
	if d, _ := changedPlatform.Digest(); d == d1 {
		t.Fatal("platform change must change the attestation digest")
	}
}

func TestProtectionAttestation_ExcerptNormalizationAndTruncation(t *testing.T) {
	long := strings.Repeat("é", 200) // 400 bytes of valid UTF-8

	// Raw inputs: untrimmed whitespace and an over-length excerpt whose
	// 256-byte truncation lands mid-code-point.
	raw := validAttestation()
	raw.Records[1].ToolName = " Read "
	raw.Records[1].DenialText = "  outside   allowed\tdirectories  "
	raw.Records[2].DenialText = long
	d1, err := raw.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	// The equivalent normalized inputs must produce the same digest.
	normalized := validAttestation()
	normalized.Records[1].ToolName = "Read"
	normalized.Records[1].DenialText = "outside allowed directories"
	normalized.Records[2].DenialText = strings.Repeat("é", 128) // exactly 256 bytes
	d2, err := normalized.Digest()
	if err != nil {
		t.Fatalf("normalized digest: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("normalized inputs must be canonical: %q vs %q", d1, d2)
	}
}

func TestProtectionAttestation_Rejections(t *testing.T) {
	notDenied := validAttestation()
	notDenied.Records[0].Denied = false

	duplicate := validAttestation()
	duplicate.Records = append(duplicate.Records, ProbeRecord{
		ToolClass: ProbeRead, ToolName: "Read", Denied: true,
		EnforcingCapability: CapCwdBoundary, DenialText: "dup",
	})

	cases := []struct {
		name    string
		mutate  func(*ProtectionAttestation)
		wantErr string
	}{
		{"not denied", func(a *ProtectionAttestation) { a.Records[0].Denied = false }, "invalidates protected evidence"},
		{"duplicate class+name", func(a *ProtectionAttestation) {
			a.Records = append(a.Records, ProbeRecord{ToolClass: ProbeRead, ToolName: "Read",
				Denied: true, EnforcingCapability: CapCwdBoundary, DenialText: "dup"})
		}, "duplicate"},
		{"empty tool name", func(a *ProtectionAttestation) {
			a.Records[0].ToolName = "  "
		}, "empty tool name"},
		{"empty excerpt", func(a *ProtectionAttestation) {
			a.Records[0].DenialText = "   "
		}, "empty denial text"},
		{"unknown capability", func(a *ProtectionAttestation) {
			a.Records[0].EnforcingCapability = "vibes"
		}, "unknown enforcing capability"},
		{"unknown tool class", func(a *ProtectionAttestation) {
			a.Records[0].ToolClass = ProbeToolClass(99)
		}, "unknown probe tool class"},
		{"missing version", func(a *ProtectionAttestation) { a.ClaudeVersion = " " }, "probed CLI version"},
		{"missing actor", func(a *ProtectionAttestation) { a.Actor = "" }, "operator actor"},
		{"missing probed time", func(a *ProtectionAttestation) { a.ProbedAt = "" }, "probe timestamp"},
		{"non-RFC3339 probed time", func(a *ProtectionAttestation) { a.ProbedAt = "yesterday" }, "RFC3339"},
		{"non-UTC probed time", func(a *ProtectionAttestation) { a.ProbedAt = "2026-09-22T00:00:00+02:00" }, "UTC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validAttestation()
			tc.mutate(&a)
			_, err := a.Digest()
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q in error, got %v", tc.wantErr, err)
			}
		})
	}
}
