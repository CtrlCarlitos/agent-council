package agy

// cprot-v2 coverage binding tests for Agy (spec §3.2): the expected
// record set is DERIVED from expected_tools ∩ the coverage map, several
// tool NAMES may share one cprot-v2 tool class (unlike the codex
// validator), and the single approval record is method denied_actions.

import (
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
)

// smallPolicy is a compact AgyLaunchPolicy fixture exercising: two
// sibling_read_path-only tools sharing the SAME cprot-v2 class (read),
// one tool with both capabilities, and one own_mutation_path-only tool.
func smallPolicy() AgyLaunchPolicy {
	return AgyLaunchPolicy{
		CLIVersion:     "1.2.9",
		PlatformOS:     "linux",
		PlatformFamily: "unix",
		ExpectedTools:  []string{"view_file", "read_resource", "run_command", "write_to_file", "ask_permission"},
		Coverage: CoverageMap{
			CLIVersion: "1.2.9",
			Tools: map[string][]Capability{
				"view_file":      {CapabilitySiblingReadPath},
				"read_resource":  {CapabilitySiblingReadPath},
				"run_command":    {CapabilitySiblingReadPath, CapabilityOwnMutationPath},
				"write_to_file":  {CapabilityOwnMutationPath},
				"ask_permission": {CapabilityControl},
			},
		},
	}
}

func probe(class codex.RecordClass, tc codex.ToolClass, name string, op codex.Operation) codex.ProbeRecord {
	return codex.ProbeRecord{
		Class: class, ToolClass: tc, ToolName: name, Operation: op, Denied: true,
		EnforcingCapability: codex.CapSandboxRestrictedFS,
		DenialText:          "denied: " + name,
	}
}

// coveringAttestation builds the minimal attestation that covers cov,
// derived from smallPolicy(): view_file and read_resource both under
// the `read` class (several names, one class — the Agy-specific rule),
// run_command under bash_absolute with both a read and all five
// mutation records, write_to_file under bash_absolute with the five
// mutation records only, and the single denied_actions approval.
func coveringAttestation(cov ProtectionCoverage) codex.ProtectionAttestation {
	att := codex.ProtectionAttestation{
		CodexVersion: cov.AgyVersion, PlatformOS: cov.PlatformOS, PlatformFamily: cov.PlatformFamily,
		ManifestDigest: "sha256:" + strings.Repeat("ab", 32),
		ProfileDigest:  "cprof-v4:sha256:" + strings.Repeat("cd", 32),
		ProbedAt:       "2026-09-24T00:00:00Z", Actor: "operator",
	}
	att.ProbeRecords = append(att.ProbeRecords,
		probe(codex.RecordSiblingRead, codex.ToolRead, "view_file", codex.OpRead),
		probe(codex.RecordSiblingRead, codex.ToolRead, "read_resource", codex.OpRead),
		probe(codex.RecordSiblingRead, codex.ToolBashAbsolute, "run_command", codex.OpRead),
	)
	for _, op := range allMutationOps {
		att.ProbeRecords = append(att.ProbeRecords, probe(codex.RecordSelfMutation, codex.ToolBashAbsolute, "run_command", op))
	}
	for _, op := range allMutationOps {
		att.ProbeRecords = append(att.ProbeRecords, probe(codex.RecordSelfMutation, codex.ToolBashAbsolute, "write_to_file", op))
	}
	att.ApprovalDenies = append(att.ApprovalDenies,
		codex.ApprovalDenyRecord{MethodName: approvalDeniedActionsMethod, RefusalKind: codex.RefusalNativeEnum})
	return att
}

func TestExpectedCoverage_DerivesSeveralNamesPerClass(t *testing.T) {
	cov, err := ExpectedCoverage(smallPolicy())
	if err != nil {
		t.Fatalf("ExpectedCoverage: %v", err)
	}
	if len(cov.siblingReads) != 3 {
		t.Fatalf("expected 3 sibling_read expectations (view_file, read_resource, run_command), got %d: %+v", len(cov.siblingReads), cov.siblingReads)
	}
	if len(cov.mutations) != 2 {
		t.Fatalf("expected 2 mutation-capable tools (run_command, write_to_file), got %d: %+v", len(cov.mutations), cov.mutations)
	}
}

func TestCoverage_FullSuiteValidates(t *testing.T) {
	policy := smallPolicy()
	cov, err := ExpectedCoverage(policy)
	if err != nil {
		t.Fatalf("ExpectedCoverage: %v", err)
	}
	att := coveringAttestation(cov)
	if err := ValidateCoverage(att, cov); err != nil {
		t.Fatalf("the covering suite must validate: %v", err)
	}

	// The durable frame round-trips through the same coverage check.
	raw, err := att.EncodeProbeRecords()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := ValidateFrame(raw, cov); err != nil {
		t.Fatalf("ValidateFrame must accept the covering frame: %v", err)
	}
}

func TestCoverage_UncoveredExpectedToolRejected(t *testing.T) {
	policy := smallPolicy()
	policy.ExpectedTools = append(policy.ExpectedTools, "invoke_subagent")
	policy.Coverage.Tools["invoke_subagent"] = []Capability{CapabilityUncovered}
	if _, err := ExpectedCoverage(policy); err == nil {
		t.Fatal("an expected_tools entry classified uncovered must be rejected")
	}
}

func TestCoverage_UnclassifiedExpectedToolRejected(t *testing.T) {
	policy := smallPolicy()
	policy.ExpectedTools = append(policy.ExpectedTools, "mystery_tool")
	if _, err := ExpectedCoverage(policy); err == nil {
		t.Fatal("an expected_tools entry absent from the coverage map must be rejected")
	}
}

func TestCoverage_TupleMismatchRejected(t *testing.T) {
	policy := smallPolicy()
	cov, err := ExpectedCoverage(policy)
	if err != nil {
		t.Fatalf("ExpectedCoverage: %v", err)
	}
	for _, mutate := range []func(*codex.ProtectionAttestation){
		func(a *codex.ProtectionAttestation) { a.CodexVersion = "9.9.9" },
		func(a *codex.ProtectionAttestation) { a.PlatformOS = "windows" },
		func(a *codex.ProtectionAttestation) { a.PlatformFamily = "nt" },
	} {
		att := coveringAttestation(cov)
		mutate(&att)
		if err := ValidateCoverage(att, cov); err == nil {
			t.Fatal("a tuple mismatch must be rejected")
		}
	}
}

func TestCoverage_MissingRecordRejected(t *testing.T) {
	policy := smallPolicy()
	cov, err := ExpectedCoverage(policy)
	if err != nil {
		t.Fatalf("ExpectedCoverage: %v", err)
	}
	// Drop the read_resource sibling_read record.
	att := coveringAttestation(cov)
	filtered := att.ProbeRecords[:0]
	for _, r := range att.ProbeRecords {
		if r.Class == codex.RecordSiblingRead && r.ToolName == "read_resource" {
			continue
		}
		filtered = append(filtered, r)
	}
	att.ProbeRecords = filtered
	if err := ValidateCoverage(att, cov); err == nil {
		t.Fatal("coverage missing a required sibling_read record must be rejected")
	}

	// Drop one mutation op for write_to_file.
	att2 := coveringAttestation(cov)
	filtered2 := att2.ProbeRecords[:0]
	for _, r := range att2.ProbeRecords {
		if r.Class == codex.RecordSelfMutation && r.ToolName == "write_to_file" && r.Operation == codex.OpDelete {
			continue
		}
		filtered2 = append(filtered2, r)
	}
	att2.ProbeRecords = filtered2
	if err := ValidateCoverage(att2, cov); err == nil {
		t.Fatal("coverage missing a required self_mutation op must be rejected")
	}
}

func TestCoverage_UnexpectedRecordRejected(t *testing.T) {
	policy := smallPolicy()
	cov, err := ExpectedCoverage(policy)
	if err != nil {
		t.Fatalf("ExpectedCoverage: %v", err)
	}
	// An extra sibling_read record for a tool outside expected_tools.
	att := coveringAttestation(cov)
	att.ProbeRecords = append(att.ProbeRecords, probe(codex.RecordSiblingRead, codex.ToolGrep, "grep_search", codex.OpRead))
	if err := ValidateCoverage(att, cov); err == nil {
		t.Fatal("an unexpected sibling_read record must be rejected")
	}

	// A self_mutation record for a read-only tool (view_file has no
	// own_mutation_path capability).
	att2 := coveringAttestation(cov)
	for _, op := range allMutationOps {
		att2.ProbeRecords = append(att2.ProbeRecords, probe(codex.RecordSelfMutation, codex.ToolRead, "view_file", op))
	}
	if err := ValidateCoverage(att2, cov); err == nil {
		t.Fatal("an unexpected self_mutation record for a read-only tool must be rejected")
	}
}

func TestCoverage_ApprovalRecordRules(t *testing.T) {
	policy := smallPolicy()
	cov, err := ExpectedCoverage(policy)
	if err != nil {
		t.Fatalf("ExpectedCoverage: %v", err)
	}

	// Missing the approval record entirely.
	att := coveringAttestation(cov)
	att.ApprovalDenies = nil
	if err := ValidateCoverage(att, cov); err == nil {
		t.Fatal("coverage missing the denied_actions approval record must be rejected")
	}

	// Wrong method name.
	att2 := coveringAttestation(cov)
	att2.ApprovalDenies = []codex.ApprovalDenyRecord{{MethodName: "some_other_method", RefusalKind: codex.RefusalNativeEnum}}
	if err := ValidateCoverage(att2, cov); err == nil {
		t.Fatal("an approval_deny record for an unknown method must be rejected")
	}

	// Wrong refusal kind.
	att3 := coveringAttestation(cov)
	att3.ApprovalDenies = []codex.ApprovalDenyRecord{{MethodName: approvalDeniedActionsMethod, RefusalKind: codex.RefusalLiveVerifiedEquivalent}}
	if err := ValidateCoverage(att3, cov); err == nil {
		t.Fatal("denied_actions must carry refusal_kind=native_refusal_enum")
	}
}

func TestValidateFrame_StructuralAndTruncationRejections(t *testing.T) {
	policy := smallPolicy()
	cov, err := ExpectedCoverage(policy)
	if err != nil {
		t.Fatalf("ExpectedCoverage: %v", err)
	}
	att := coveringAttestation(cov)
	raw, err := att.EncodeProbeRecords()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := ValidateFrame(raw[:len(raw)-1], cov); err == nil {
		t.Fatal("a truncated frame must be rejected")
	}
	if err := ValidateFrame(append(append([]byte{}, raw...), 0xff), cov); err == nil {
		t.Fatal("trailing bytes after the frame must be rejected")
	}
}

// structuralValidate reuses codex.ProtectionAttestation.Validate for
// enum/shape checks without forking codex's unexported validateRecords.
func TestStructuralValidate_RejectsUnknownEnumsAndIncompleteDenials(t *testing.T) {
	bad := []codex.ProbeRecord{{
		Class: codex.RecordSiblingRead, ToolClass: codex.ToolRead, ToolName: "view_file",
		Operation: codex.OpRead, Denied: false, EnforcingCapability: codex.CapSandboxRestrictedFS,
		DenialText: "x",
	}}
	if err := structuralValidate(bad, nil); err == nil {
		t.Fatal("a probe record that was not denied must invalidate the whole attestation")
	}
}
