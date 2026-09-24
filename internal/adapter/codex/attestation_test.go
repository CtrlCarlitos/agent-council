package codex

// cprot-v2 canonical protection-attestation tests: fixed golden vectors
// (one per record class plus a mixed-class payload), canonical ordering
// and determinism, excerpt truncation at code-point boundaries, and the
// fail-closed rejections (spec §3.7).

import (
	"strings"
	"testing"
)

func validAttestation() ProtectionAttestation {
	return ProtectionAttestation{
		CodexVersion:   "0.154.0",
		PlatformOS:     "linux",
		PlatformFamily: "unix",
		ManifestDigest: "cprof-v2:sha256:def",
		ProfileDigest:  "cprof-v3:sha256:abc",
		ProbeRecords: []ProbeRecord{
			{Class: RecordSiblingRead, ToolClass: ToolRead, ToolName: "view_file",
				Operation: OpRead, Denied: true,
				EnforcingCapability: CapCwdBoundary, DenialText: "outside allowed directories"},
			{Class: RecordSiblingRead, ToolClass: ToolBashAbsolute, ToolName: "shell",
				Operation: OpRead, Denied: true,
				EnforcingCapability: CapSandboxRestrictedFS, DenialText: "Read-only file system: sibling rollout outside sandbox"},
			{Class: RecordSelfMutation, ToolClass: ToolBashAbsolute, ToolName: "shell",
				Operation: OpTruncate, Denied: true,
				EnforcingCapability: CapSandboxRestrictedFS, DenialText: "Read-only file system"},
		},
		ApprovalDenies: []ApprovalDenyRecord{
			{MethodName: "execCommandApproval", RefusalKind: RefusalNativeEnum},
			{MethodName: "item/commandExecution/requestApproval", RefusalKind: RefusalLiveVerifiedEquivalent},
		},
		ProbedAt: "2026-09-23T12:00:00Z",
		Actor:    "operator",
	}
}

// Independently computed fixed vectors (python3 framing, spec §3.7:
// cprot-v2 tag, codex_version, platform os, platform family, manifest
// digest, profile digest, framed records with class blocks in canonical
// order, probed_at, actor). The encoder must reproduce THESE digests,
// not merely stable ones.
func TestProtectionAttestation_FixedGoldenVectors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ProtectionAttestation)
		want   string
	}{
		{"sibling_read only", func(a *ProtectionAttestation) {
			a.ProbeRecords = a.ProbeRecords[1:2]
			a.ApprovalDenies = nil
		}, "cprot-v2:sha256:2f19e0af3cab84ad010725a042b028b3e77e6d03e1a9b921bdf22a8321c967ef"},
		{"self_mutation only", func(a *ProtectionAttestation) {
			a.ProbeRecords = a.ProbeRecords[2:]
			a.ApprovalDenies = nil
		}, "cprot-v2:sha256:30f8aa66a3ea20eefe19c17624208383aa2fcf5aa288c17e9b09c2e8fa98f45d"},
		{"approval_deny only", func(a *ProtectionAttestation) {
			a.ProbeRecords = nil
			a.ApprovalDenies = a.ApprovalDenies[:1]
		}, "cprot-v2:sha256:c8d6ef93c4ddd52566543f6d832a1271d4c0019883244d1ede86633300ee02a9"},
		{"mixed classes", func(a *ProtectionAttestation) {},
			"cprot-v2:sha256:4e7df779dc77000b33bc638645527898f463c5bcb7b32fe08b2feb8f937f4a66"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validAttestation()
			tc.mutate(&a)
			got, err := a.Digest()
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if got != tc.want {
				t.Fatalf("fixed golden vector mismatch:\n got %q\nwant %q", got, tc.want)
			}
			if len(got) != 80 {
				t.Fatalf("attestation id must be 80 chars for storage CHECK, got %d", len(got))
			}
		})
	}
}

func TestProtectionAttestation_CanonicalOrderAndDeterminism(t *testing.T) {
	a := validAttestation()
	d1, err := a.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	d1again, err := a.Digest()
	if err != nil {
		t.Fatalf("second digest: %v", err)
	}
	if d1 != d1again {
		t.Fatalf("identical payloads must produce identical digests: %q vs %q", d1, d1again)
	}

	// Reordering the input records must not change the digest.
	reordered := a
	reordered.ProbeRecords = []ProbeRecord{a.ProbeRecords[2], a.ProbeRecords[0], a.ProbeRecords[1]}
	reordered.ApprovalDenies = []ApprovalDenyRecord{a.ApprovalDenies[1], a.ApprovalDenies[0]}
	d2, err := reordered.Digest()
	if err != nil {
		t.Fatalf("reordered digest: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("record order must be canonicalized: %q vs %q", d1, d2)
	}

	// probed_at and actor are part of the payload: a re-run at a
	// different time or by a different actor intentionally produces a
	// different digest.
	changedTime := a
	changedTime.ProbedAt = "2026-09-23T12:00:01Z"
	if d, _ := changedTime.Digest(); d == d1 {
		t.Fatal("probed_at change must change the attestation digest")
	}
	changedActor := a
	changedActor.Actor = "operator-2"
	if d, _ := changedActor.Digest(); d == d1 {
		t.Fatal("actor change must change the attestation digest")
	}
	changedProfile := a
	changedProfile.ProfileDigest = "cprof-v3:sha256:zzz"
	if d, _ := changedProfile.Digest(); d == d1 {
		t.Fatal("profile change must change the attestation digest")
	}
}

func TestProtectionAttestation_ExcerptNormalizationAndTruncation(t *testing.T) {
	// 85×"明" (3 bytes each = 255 bytes) + "é" (2 bytes) = 257 bytes:
	// the 256-byte cut lands on the FIRST byte of "é", so truncation
	// must execute the partial-rune strip and keep exactly the
	// 255-byte whole-rune prefix (明 ×85).
	splitting := strings.Repeat("明", 85) + "é"

	// Raw inputs: untrimmed whitespace and the over-length excerpt
	// whose 256-byte cut splits a code point.
	raw := validAttestation()
	raw.ProbeRecords[1].ToolName = " shell "
	raw.ProbeRecords[1].DenialText = "  Read-only   file\tsystem  "
	raw.ProbeRecords[2].DenialText = splitting
	d1, err := raw.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	// The equivalent normalized inputs — with the truncated excerpt
	// pinned to exactly the expected 255-byte valid UTF-8 prefix —
	// must produce the same digest.
	normalized := validAttestation()
	normalized.ProbeRecords[1].ToolName = "shell"
	normalized.ProbeRecords[1].DenialText = "Read-only file system"
	normalized.ProbeRecords[2].DenialText = strings.Repeat("明", 85) // exactly 255 bytes
	d2, err := normalized.Digest()
	if err != nil {
		t.Fatalf("normalized digest: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("truncation must keep exactly the 255-byte whole-rune prefix: %q vs %q", d1, d2)
	}
}

func TestProtectionAttestation_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ProtectionAttestation)
		wantErr string
	}{
		{"sibling_read with non-read operation", func(a *ProtectionAttestation) {
			a.ProbeRecords[0].Operation = OpWrite
		}, "sibling_read requires operation read"},
		{"self_mutation with read operation", func(a *ProtectionAttestation) {
			a.ProbeRecords[2].Operation = OpRead
		}, "self_mutation requires a mutation operation"},
		{"self_mutation with unknown operation", func(a *ProtectionAttestation) {
			a.ProbeRecords[2].Operation = Operation(9)
		}, "self_mutation requires a mutation operation"},
		{"probe not denied", func(a *ProtectionAttestation) {
			a.ProbeRecords[0].Denied = false
		}, "invalidates protected evidence"},
		{"duplicate probe record", func(a *ProtectionAttestation) {
			a.ProbeRecords = append(a.ProbeRecords, a.ProbeRecords[0])
			a.ProbeRecords[3].DenialText = "dup"
		}, "duplicate"},
		{"duplicate approval method", func(a *ProtectionAttestation) {
			a.ApprovalDenies = append(a.ApprovalDenies, a.ApprovalDenies[0])
		}, "duplicate"},
		{"unknown record class", func(a *ProtectionAttestation) {
			a.ProbeRecords[0].Class = RecordClass(9)
		}, "unknown record class"},
		{"unknown tool class", func(a *ProtectionAttestation) {
			a.ProbeRecords[0].ToolClass = ToolClass(99)
		}, "unknown tool class"},
		{"unknown enforcing capability", func(a *ProtectionAttestation) {
			a.ProbeRecords[0].EnforcingCapability = EnforcingCapability(9)
		}, "unknown enforcing capability"},
		{"unknown refusal kind", func(a *ProtectionAttestation) {
			a.ApprovalDenies[0].RefusalKind = RefusalKind(9)
		}, "unknown refusal kind"},
		{"empty tool name", func(a *ProtectionAttestation) {
			a.ProbeRecords[0].ToolName = "  "
		}, "empty tool name"},
		{"empty denial text", func(a *ProtectionAttestation) {
			a.ProbeRecords[0].DenialText = "   "
		}, "empty denial text"},
		{"empty method name", func(a *ProtectionAttestation) {
			a.ApprovalDenies[0].MethodName = " "
		}, "empty method name"},
		{"non-UTF-8 codex version", func(a *ProtectionAttestation) {
			a.CodexVersion = "0.154.\xff"
		}, "not valid UTF-8"},
		{"non-UTF-8 tool name", func(a *ProtectionAttestation) {
			a.ProbeRecords[0].ToolName = "view_\xffile"
		}, "not valid UTF-8"},
		{"non-UTF-8 denial text", func(a *ProtectionAttestation) {
			a.ProbeRecords[0].DenialText = "denied \xff badly"
		}, "not valid UTF-8"},
		{"non-UTF-8 method name", func(a *ProtectionAttestation) {
			a.ApprovalDenies[0].MethodName = "exec\xffCommandApproval"
		}, "not valid UTF-8"},
		{"missing codex version", func(a *ProtectionAttestation) { a.CodexVersion = " " }, "probed codex version"},
		{"missing platform os", func(a *ProtectionAttestation) { a.PlatformOS = "" }, "platform os"},
		{"missing platform family", func(a *ProtectionAttestation) { a.PlatformFamily = "" }, "platform family"},
		{"missing digests", func(a *ProtectionAttestation) {
			a.ManifestDigest = ""
			a.ProfileDigest = " "
		}, "manifest and profile digests"},
		{"no records", func(a *ProtectionAttestation) {
			a.ProbeRecords = nil
			a.ApprovalDenies = nil
		}, "at least one record"},
		{"missing probed time", func(a *ProtectionAttestation) { a.ProbedAt = "" }, "probe timestamp"},
		{"non-RFC3339 probed time", func(a *ProtectionAttestation) { a.ProbedAt = "yesterday" }, "RFC3339"},
		{"non-UTC probed time", func(a *ProtectionAttestation) { a.ProbedAt = "2026-09-23T12:00:00+02:00" }, "UTC"},
		{"missing actor", func(a *ProtectionAttestation) { a.Actor = "" }, "operator actor"},
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
