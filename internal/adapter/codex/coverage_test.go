package codex

// cprot-v2 coverage binding tests (spec §3.3/§3.7): an attestation
// unlocks nothing unless it enumerates every enabled path of the frozen
// profile — every built-in class, every expected MCP server and plugin
// tool, every mutation operation on every mutation-capable path, and
// every pinned approval method with a native refusal enum. Unexpected
// or duplicate coverage is rejected. The decoded durable frame is held
// to the same rule.

import (
	"strings"
	"testing"
)

func testCoverage() ProtectionCoverage {
	return ProtectionCoverage{
		CodexVersion:   "0.154.0",
		PlatformOS:     "linux",
		PlatformFamily: "unix",
		ManifestDigest: "sha256:" + strings.Repeat("ab", 32),
		ProfileDigest:  "cprof-v3:sha256:" + strings.Repeat("cd", 32),
	}
}

// coveringAttestation builds the minimal attestation that covers c.
func coveringAttestation(c ProtectionCoverage) ProtectionAttestation {
	probe := func(class RecordClass, tc ToolClass, name string, op Operation) ProbeRecord {
		return ProbeRecord{Class: class, ToolClass: tc, ToolName: name, Operation: op, Denied: true,
			EnforcingCapability: CapSandboxRestrictedFS, DenialText: "denied: " + name + " " + operationName(op)}
	}
	att := ProtectionAttestation{
		CodexVersion: c.CodexVersion, PlatformOS: c.PlatformOS, PlatformFamily: c.PlatformFamily,
		ManifestDigest: c.ManifestDigest, ProfileDigest: c.ProfileDigest,
		ProbedAt: "2026-09-23T00:00:00Z", Actor: "operator",
	}
	att.ProbeRecords = append(att.ProbeRecords,
		probe(RecordSiblingRead, ToolRead, "Read", OpRead),
		probe(RecordSiblingRead, ToolGlob, "Glob", OpRead),
		probe(RecordSiblingRead, ToolGrep, "Grep", OpRead),
	)
	mutationPath := func(tc ToolClass, name string) {
		att.ProbeRecords = append(att.ProbeRecords, probe(RecordSiblingRead, tc, name, OpRead))
		for _, op := range allMutationOps {
			att.ProbeRecords = append(att.ProbeRecords, probe(RecordSelfMutation, tc, name, op))
		}
	}
	mutationPath(ToolBashAbsolute, "Bash")
	for _, s := range c.MCPTools {
		mutationPath(ToolMCP, s)
	}
	for _, p := range c.PluginTools {
		mutationPath(ToolPlugin, p)
	}
	for method, denyEquivalent := range pinnedApprovalMethods() {
		if !denyEquivalent {
			att.ApprovalDenies = append(att.ApprovalDenies, ApprovalDenyRecord{MethodName: method, RefusalKind: RefusalNativeEnum})
		}
	}
	return att
}

func TestCoverage_FullSuiteValidates(t *testing.T) {
	c := testCoverage()
	if err := coveringAttestation(c).ValidateCoverage(c); err != nil {
		t.Fatalf("the covering suite must validate: %v", err)
	}
	// Inventory classes present in the frozen profile are required and
	// satisfied by the exact frozen paths.
	c.MCPTools = []string{"context7/resolve-library-id", "fs/read_file"}
	c.PluginTools = []string{"skill:review"}
	if err := coveringAttestation(c).ValidateCoverage(c); err != nil {
		t.Fatalf("the covering suite with inventories must validate: %v", err)
	}
	// Optional live-verified records for the two deny-equivalent
	// variants are accepted.
	att := coveringAttestation(c)
	att.ApprovalDenies = append(att.ApprovalDenies,
		ApprovalDenyRecord{MethodName: "item/permissions/requestApproval", RefusalKind: RefusalLiveVerifiedEquivalent},
		ApprovalDenyRecord{MethodName: "item/tool/requestUserInput", RefusalKind: RefusalLiveVerifiedEquivalent})
	if err := att.ValidateCoverage(c); err != nil {
		t.Fatalf("live-verified deny-equivalent records must be accepted: %v", err)
	}
	// CoverageFor derives the set from the frozen policy: MCP tool paths
	// and plugin tools are trimmed, deduplicated, and sorted.
	got := CoverageFor(CodexLaunchPolicy{
		AppServerVersion: "0.154.0", PlatformOS: "linux", PlatformFamily: "unix",
		ManifestDigest: c.ManifestDigest, ExpectedMCPTools: []string{" fs/read", "context7/x", "fs/read"},
		PluginTools: []string{"b", "a", "a"},
	}, c.ProfileDigest)
	if strings.Join(got.MCPTools, ",") != "context7/x,fs/read" || strings.Join(got.PluginTools, ",") != "a,b" {
		t.Fatalf("derived coverage must be canonical: %+v", got)
	}
	if got.ProfileDigest != c.ProfileDigest || got.ManifestDigest != c.ManifestDigest {
		t.Fatalf("derived coverage must carry the frozen digests: %+v", got)
	}
}

func TestCoverage_Rejections(t *testing.T) {
	dropWhere := func(a *ProtectionAttestation, keep func(ProbeRecord) bool) {
		var out []ProbeRecord
		for _, r := range a.ProbeRecords {
			if keep(r) {
				out = append(out, r)
			}
		}
		a.ProbeRecords = out
	}
	cases := []struct {
		name    string
		cov     func(*ProtectionCoverage)
		mutate  func(*ProtectionAttestation)
		wantErr string
	}{
		{"minimal one-record suite", nil, func(a *ProtectionAttestation) {
			a.ProbeRecords = a.ProbeRecords[:1]
			a.ApprovalDenies = a.ApprovalDenies[:1]
		}, "coverage missing"},
		{"missing glob class", nil, func(a *ProtectionAttestation) {
			dropWhere(a, func(r ProbeRecord) bool { return r.ToolClass != ToolGlob })
		}, "tool class glob has no probe record"},
		{"duplicate class coverage through two tool names", nil, func(a *ProtectionAttestation) {
			extra := a.ProbeRecords[0]
			extra.ToolName = "view_file"
			a.ProbeRecords = append(a.ProbeRecords, extra)
		}, "duplicate coverage: tool class read"},
		{"self_mutation on a read-only class", nil, func(a *ProtectionAttestation) {
			a.ProbeRecords = append(a.ProbeRecords, ProbeRecord{Class: RecordSelfMutation, ToolClass: ToolGrep,
				ToolName: "Grep", Operation: OpWrite, Denied: true, EnforcingCapability: CapDenyList, DenialText: "x"})
		}, "unexpected coverage: self_mutation records for read-only grep"},
		{"missing shell delete mutation", nil, func(a *ProtectionAttestation) {
			dropWhere(a, func(r ProbeRecord) bool {
				return !(r.Class == RecordSelfMutation && r.ToolClass == ToolBashAbsolute && r.Operation == OpDelete)
			})
		}, "no self_mutation delete record for bash_absolute path \"Bash\""},
		{"shell sibling_read missing", nil, func(a *ProtectionAttestation) {
			dropWhere(a, func(r ProbeRecord) bool {
				return !(r.Class == RecordSiblingRead && r.ToolClass == ToolBashAbsolute)
			})
		}, "no sibling_read record for bash_absolute path"},
		{"mcp record when no server is enabled", nil, func(a *ProtectionAttestation) {
			a.ProbeRecords = append(a.ProbeRecords, ProbeRecord{Class: RecordSiblingRead, ToolClass: ToolMCP,
				ToolName: "context7/read", Operation: OpRead, Denied: true, EnforcingCapability: CapDenyList, DenialText: "x"})
		}, "enables no MCP tool"},
		{"mcp path outside the frozen inventory", func(c *ProtectionCoverage) { c.MCPTools = []string{"context7/x"} },
			func(a *ProtectionAttestation) {
				for i := range a.ProbeRecords {
					if a.ProbeRecords[i].ToolClass == ToolMCP {
						a.ProbeRecords[i].ToolName = "context7/other"
					}
				}
			}, "not in the frozen MCP tool inventory"},
		{"sibling tool under a covered server is not covered by its sibling",
			func(c *ProtectionCoverage) { c.MCPTools = []string{"context7/toolA", "context7/toolB"} },
			func(a *ProtectionAttestation) {
				dropWhere(a, func(r ProbeRecord) bool { return r.ToolName != "context7/toolB" })
			}, "frozen MCP tool \"context7/toolB\" has no probe record"},
		{"expected mcp tool uncovered", func(c *ProtectionCoverage) { c.MCPTools = []string{"context7/x", "fs/read"} },
			func(a *ProtectionAttestation) {
				dropWhere(a, func(r ProbeRecord) bool { return r.ToolName != "fs/read" })
			}, "frozen MCP tool \"fs/read\" has no probe record"},
		{"mcp path lacks mutation coverage", func(c *ProtectionCoverage) { c.MCPTools = []string{"context7/x"} },
			func(a *ProtectionAttestation) {
				dropWhere(a, func(r ProbeRecord) bool {
					return !(r.ToolClass == ToolMCP && r.Class == RecordSelfMutation && r.Operation == OpRename)
				})
			}, "no self_mutation rename record for mcp path"},
		{"plugin tool uncovered", func(c *ProtectionCoverage) { c.PluginTools = []string{"skill:review"} },
			func(a *ProtectionAttestation) {
				dropWhere(a, func(r ProbeRecord) bool { return r.ToolClass != ToolPlugin })
			}, "frozen skill/plugin tool \"skill:review\" has no probe record"},
		{"plugin record when none enabled", nil, func(a *ProtectionAttestation) {
			a.ProbeRecords = append(a.ProbeRecords, ProbeRecord{Class: RecordSiblingRead, ToolClass: ToolPlugin,
				ToolName: "skill:x", Operation: OpRead, Denied: true, EnforcingCapability: CapDenyList, DenialText: "x"})
		}, "enables no skill/plugin tool"},
		{"missing native approval method", nil, func(a *ProtectionAttestation) {
			var out []ApprovalDenyRecord
			for _, d := range a.ApprovalDenies {
				if d.MethodName != "applyPatchApproval" {
					out = append(out, d)
				}
			}
			a.ApprovalDenies = out
		}, "no approval_deny record for method \"applyPatchApproval\""},
		{"native method claimed live-verified", nil, func(a *ProtectionAttestation) {
			for i := range a.ApprovalDenies {
				if a.ApprovalDenies[i].MethodName == "execCommandApproval" {
					a.ApprovalDenies[i].RefusalKind = RefusalLiveVerifiedEquivalent
				}
			}
		}, "must be native_refusal_enum"},
		{"deny-equivalent claimed native", nil, func(a *ProtectionAttestation) {
			a.ApprovalDenies = append(a.ApprovalDenies,
				ApprovalDenyRecord{MethodName: "item/permissions/requestApproval", RefusalKind: RefusalNativeEnum})
		}, "must be live_verified_equivalent"},
		{"unknown approval method", nil, func(a *ProtectionAttestation) {
			a.ApprovalDenies = append(a.ApprovalDenies,
				ApprovalDenyRecord{MethodName: "item/whatever/requestApproval", RefusalKind: RefusalNativeEnum})
		}, "unknown method"},
		{"tuple mismatch on profile digest", nil, func(a *ProtectionAttestation) {
			a.ProfileDigest = "cprof-v3:sha256:" + strings.Repeat("ee", 32)
		}, "profile digest"},
		{"tuple mismatch on platform", nil, func(a *ProtectionAttestation) { a.PlatformOS = "windows" }, "platform os"},
		{"structural invalidity still rejected", nil, func(a *ProtectionAttestation) {
			a.ProbeRecords[0].Denied = false
		}, "invalidates protected evidence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testCoverage()
			if tc.cov != nil {
				tc.cov(&c)
			}
			att := coveringAttestation(c)
			tc.mutate(&att)
			err := att.ValidateCoverage(c)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// The durable frame is decoded losslessly and held to the same coverage
// rule; a frame that decodes but does not cover the profile is refused,
// and a corrupt frame fails closed.
func TestCoverage_DurableFrameRoundTrip(t *testing.T) {
	c := testCoverage()
	c.MCPTools = []string{"context7/resolve-library-id"}
	att := coveringAttestation(c)
	raw, err := att.EncodeProbeRecords()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	probes, denies, err := DecodeProbeRecords(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(probes) != len(att.ProbeRecords) || len(denies) != len(att.ApprovalDenies) {
		t.Fatalf("decoded %d/%d records, want %d/%d", len(probes), len(denies), len(att.ProbeRecords), len(att.ApprovalDenies))
	}
	want := canonicalProbeRecords(att.ProbeRecords)
	for i := range want {
		if probes[i] != want[i] {
			t.Fatalf("record %d decoded as %+v, want %+v", i, probes[i], want[i])
		}
	}
	if err := c.ValidateFrame(raw); err != nil {
		t.Fatalf("covering frame must validate: %v", err)
	}

	stricter := c
	stricter.MCPTools = []string{"context7/resolve-library-id", "fs/read"}
	if err := stricter.ValidateFrame(raw); err == nil || !strings.Contains(err.Error(), "\"fs/read\" has no probe record") {
		t.Fatalf("a frame that does not cover the profile must be refused, got %v", err)
	}
	if err := c.ValidateFrame(raw[:len(raw)-3]); err == nil {
		t.Fatal("a truncated frame must fail closed")
	}
	if err := c.ValidateFrame(append(append([]byte(nil), raw...), 0)); err == nil {
		t.Fatal("trailing bytes must fail closed")
	}
	if err := c.ValidateFrame([]byte("[]")); err == nil {
		t.Fatal("a non-frame row must fail closed")
	}
	// A decoded denied=false byte is structurally invalid even though
	// the encoder can never produce it.
	// Canonical order puts sibling_read/read/"Read" first: count(4),
	// class(1), tool_class(1), len(4), "Read"(4), operation(1),
	// capability(1), then the denied byte.
	forged := append([]byte(nil), raw...)
	if forged[16] != 1 {
		t.Fatalf("fixture invariant: byte 16 must be the first record's denied flag, got %d", forged[16])
	}
	forged[16] = 0
	if err := c.ValidateFrame(forged); err == nil || !strings.Contains(err.Error(), "invalidates protected evidence") {
		t.Fatalf("a forged denied=false byte must fail closed, got %v", err)
	}
}
