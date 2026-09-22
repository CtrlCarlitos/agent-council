package claude

import (
	"strings"
	"testing"
)

func validRecords() []ProbeRecord {
	return append([]ProbeRecord{}, []ProbeRecord{
		{ToolClass: ProbeBash, ToolName: "Bash", Denied: true,
			EnforcingCapability: CapGuardrailHook, DenialText: "guardrail denied"},
		{ToolClass: ProbeRead, ToolName: "Read", Denied: true,
			EnforcingCapability: CapCwdBoundary, DenialText: "outside allowed directories"},
		{ToolClass: ProbeMCP, ToolName: "mcp__serena__find_symbol", Denied: true,
			EnforcingCapability: CapPermissionDenial, DenialText: "permission denied by policy"},
	}...)
}

func TestProtectionAttestation_DigestGoldenAndDeterminism(t *testing.T) {
	a, err := AttestationDigest("2.1.278", "linux/amd64",
		"ctmpl-v1:sha256:abc", "cprof-v2:sha256:def", validRecords(),
		"2026-09-22T00:00:00Z", "operator")
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if !strings.HasPrefix(a, "cprot-v1:sha256:") {
		t.Fatalf("prefix, got %q", a)
	}
	b, err := AttestationDigest("2.1.278", "linux/amd64",
		"ctmpl-v1:sha256:abc", "cprof-v2:sha256:def", validRecords(),
		"2026-09-22T00:00:00Z", "operator")
	if err != nil {
		t.Fatalf("digest 2: %v", err)
	}
	// Record ORDER in the input must not matter (canonical sort).
	reordered := []ProbeRecord{validRecords()[2], validRecords()[0], validRecords()[1]}
	c, err := AttestationDigest("2.1.278", "linux/amd64",
		"ctmpl-v1:sha256:abc", "cprof-v2:sha256:def", reordered,
		"2026-09-22T00:00:00Z", "operator")
	if err != nil {
		t.Fatalf("digest 3: %v", err)
	}
	if a != b || a != c {
		t.Fatalf("attestation digest must be canonical: %q %q %q", a, b, c)
	}

	// Any binding-field change changes the digest.
	if d, _ := AttestationDigest("2.1.279", "linux/amd64",
		"ctmpl-v1:sha256:abc", "cprof-v2:sha256:def", validRecords(),
		"2026-09-22T00:00:00Z", "operator"); d == a {
		t.Fatal("version change must change the attestation digest")
	}
	if d, _ := AttestationDigest("2.1.278", "linux/amd64",
		"ctmpl-v1:sha256:different", "cprof-v2:sha256:def", validRecords(),
		"2026-09-22T00:00:00Z", "operator"); d == a {
		t.Fatal("template change must change the attestation digest")
	}
}

func TestProtectionAttestation_ExcerptNormalizationAndTruncation(t *testing.T) {
	long := strings.Repeat("é", 200) // 400 bytes of valid UTF-8
	records := []ProbeRecord{
		{ToolClass: ProbeRead, ToolName: " Read ", Denied: true,
			EnforcingCapability: CapCwdBoundary,
			DenialText:          "  outside   allowed\tdirectories  "},
		{ToolClass: ProbeGrep, ToolName: "Grep", Denied: true,
			EnforcingCapability: CapGuardrailHook, DenialText: long},
	}
	a1, err := AttestationDigest("2.1.278", "linux/amd64",
		"t", "m", records, "2026-09-22T00:00:00Z", "op")
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	// The trimmed record must produce the same digest as the pre-trimmed
	// equivalent.
	normalized := []ProbeRecord{
		{ToolClass: ProbeRead, ToolName: "Read", Denied: true,
			EnforcingCapability: CapCwdBoundary,
			DenialText:          "outside allowed directories"},
		{ToolClass: ProbeGrep, ToolName: "Grep", Denied: true,
			EnforcingCapability: CapGuardrailHook,
			DenialText:          strings.Repeat("é", 128)}, // 256 bytes exactly
	}
	a2, err := AttestationDigest("2.1.278", "linux/amd64",
		"t", "m", normalized, "2026-09-22T00:00:00Z", "op")
	if err != nil {
		t.Fatalf("digest 2: %v", err)
	}
	if a1 != a2 {
		t.Fatalf("normalized inputs must be canonical: %q vs %q", a1, a2)
	}
}

func TestProtectionAttestation_Rejections(t *testing.T) {
	denied := false
	base := validRecords()

	cases := []struct {
		name    string
		records []ProbeRecord
		wantErr string
	}{
		{"not denied", func() []ProbeRecord {
			r := append([]ProbeRecord{}, base...) // deep copy: no shared backing array
			r[0].Denied = denied
			return r
		}(), "denied"},
		{"duplicate class+name", append(append([]ProbeRecord{}, base...), ProbeRecord{
			ToolClass: ProbeRead, ToolName: "Read", Denied: true,
			EnforcingCapability: CapCwdBoundary, DenialText: "dup"},
		), "duplicate"},
		{"empty tool name", []ProbeRecord{
			{ToolClass: ProbeRead, ToolName: "  ", Denied: true,
				EnforcingCapability: CapCwdBoundary},
		}, "tool name"},
		{"empty excerpt", []ProbeRecord{
			{ToolClass: ProbeRead, ToolName: "Read", Denied: true,
				EnforcingCapability: CapCwdBoundary, DenialText: "   "},
		}, "denial text"},
		{"unknown capability", []ProbeRecord{
			{ToolClass: ProbeRead, ToolName: "Read", Denied: true,
				EnforcingCapability: "vibes", DenialText: "x"},
		}, "enforcing capability"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AttestationDigest("2.1.278", "linux/amd64",
				"t", "m", tc.records, "2026-09-22T00:00:00Z", "op")
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q in error, got %v", tc.wantErr, err)
			}
		})
	}
}
