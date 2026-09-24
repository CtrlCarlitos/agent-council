package agy

// cprot-v2 coverage binding for Agy (AC-010 spec §3.2/§3.7), reusing the
// codex cprot-v2 encoding UNCHANGED: the frame's version slot carries
// the Agy CLI version, manifest_digest the toolkit manifest digest,
// profile_digest the cprof-v4 digest (spec §3.2). Unlike the codex
// validator, several tool NAMES may share one cprot-v2 tool class (e.g.
// view_file and read_resource both `read`), because the coverage MAP,
// not the class, is the unit of coverage: duplicate coverage means the
// same tool name probed twice, and unexpected coverage means a name
// outside expected_tools or a class the map does not assign to it.
//
// The expected coverage set is DERIVED, never asserted: expected_tools
// ∩ the pinned coverage map yields the exact record set — one
// sibling_read per sibling_read_path tool name, the five self_mutation
// operations per own_mutation_path tool name, and the single
// denied_actions approval record — and because freeze rejects non-empty
// MCP/plugin inventories (§3.7) the set contains built-in tools only.
//
// ManifestDigest/ProfileDigest tuple binding (the durable
// agy_protection_attestations lookup) is wired by a later task once the
// launch policy carries those digests; this task derives and validates
// the built-in record-coverage set from the frozen tool coverage map.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
)

// Capability is one entry of the closed coverage-map enum (spec §3.2):
// {sibling_read_path, own_mutation_path, network_only, control,
// uncovered}, encoded in that fixed order.
type Capability string

const (
	CapabilitySiblingReadPath Capability = "sibling_read_path"
	CapabilityOwnMutationPath Capability = "own_mutation_path"
	CapabilityNetworkOnly     Capability = "network_only"
	CapabilityControl         Capability = "control"
	CapabilityUncovered       Capability = "uncovered"
)

// capabilityOrder is the closed enum's fixed canonical order.
var capabilityOrder = []Capability{
	CapabilitySiblingReadPath,
	CapabilityOwnMutationPath,
	CapabilityNetworkOnly,
	CapabilityControl,
	CapabilityUncovered,
}

func capabilityRank(c Capability) int {
	for i, want := range capabilityOrder {
		if c == want {
			return i
		}
	}
	return -1
}

// DenialEntry is one entry of the coverage evidence file's denial_map:
// a native denial (settings.json rule) proven for the pinned CLI
// version, naming the tools it covers.
type DenialEntry struct {
	Action      string
	DisplayName string
	Tools       []string
}

// CoverageMap is the version-pinned tool-to-capability coverage map
// (spec §3.2/§3.7), decoded from docs/superpowers/evidence/
// ac010-agy-tool-coverage-<version>.json.
type CoverageMap struct {
	CLIVersion string
	Tools      map[string][]Capability
	DenialMap  []DenialEntry
}

func (m CoverageMap) hasCapability(tool string, want Capability) bool {
	for _, c := range m.Tools[tool] {
		if c == want {
			return true
		}
	}
	return false
}

// siblingReadClassFor maps a sibling_read_path tool NAME to its
// cprot-v2 tool class (spec §3.2 table, applied verbatim): browser,
// notebook execution, and file:// URL reads are recorded under
// bash_absolute semantics ("arbitrary code or URL with filesystem
// reach"). call_mcp_tool is handled separately (it multiplies per
// frozen MCP tool, always empty under the §3.7 inventory gate).
var siblingReadClassFor = map[string]codex.ToolClass{
	"view_file":                  codex.ToolRead,
	"read_resource":              codex.ToolRead,
	"list_dir":                   codex.ToolGlob,
	"find_by_name":               codex.ToolGlob,
	"grep_search":                codex.ToolGrep,
	"run_command":                codex.ToolBashAbsolute,
	"send_command_input":         codex.ToolBashAbsolute,
	"notebook_execution":         codex.ToolBashAbsolute,
	"open_browser_url":           codex.ToolBashAbsolute,
	"read_browser_page":          codex.ToolBashAbsolute,
	"execute_browser_javascript": codex.ToolBashAbsolute,
	"read_url_content":           codex.ToolBashAbsolute,
}

// ownMutationClassFor maps an own_mutation_path tool NAME to its
// cprot-v2 tool class. The cprot-v2 enum is reused unchanged (no new
// tool class), so every arbitrary-filesystem-write tool — whether or
// not it also carries sibling_read_path — is recorded under the same
// bash_absolute "arbitrary reach" bucket the spec already uses for
// browser/notebook/file:// sibling reads.
var ownMutationClassFor = map[string]codex.ToolClass{
	"write_to_file":              codex.ToolBashAbsolute,
	"replace_file_content":       codex.ToolBashAbsolute,
	"multi_replace_file_content": codex.ToolBashAbsolute,
	"sed_file":                   codex.ToolBashAbsolute,
	"notebook_edit":              codex.ToolBashAbsolute,
	"run_command":                codex.ToolBashAbsolute,
	"send_command_input":         codex.ToolBashAbsolute,
	"notebook_execution":         codex.ToolBashAbsolute,
	"execute_browser_javascript": codex.ToolBashAbsolute,
}

// approvalDeniedActionsMethod is the single Agy approval method (spec
// §3.2): Agy has no per-variant approval wire protocol, so
// approval_deny records carry exactly one method, denied_actions, with
// refusal_kind=native_refusal_enum.
const approvalDeniedActionsMethod = "denied_actions"

type coveragePath struct {
	class codex.ToolClass
	name  string
}

var allMutationOps = []codex.Operation{codex.OpWrite, codex.OpAppend, codex.OpTruncate, codex.OpRename, codex.OpDelete}

// ProtectionCoverage is the expected cprot-v2 coverage set for one
// frozen Agy launch policy.
type ProtectionCoverage struct {
	AgyVersion     string
	PlatformOS     string
	PlatformFamily string

	siblingReads []coveragePath
	mutations    []coveragePath
}

// ExpectedCoverage derives the expected cprot-v2 record set from the
// frozen launch policy's expected_tools ∩ coverage map (spec §3.2): one
// sibling_read record per sibling_read_path tool name (call_mcp_tool
// excluded — it multiplies per frozen MCP tool, always empty under the
// §3.7 inventory gate, so it contributes no built-in record), the five
// self_mutation operations per own_mutation_path tool name, and the
// single denied_actions approval record.
func ExpectedCoverage(policy AgyLaunchPolicy) (ProtectionCoverage, error) {
	cov := ProtectionCoverage{
		AgyVersion:     policy.CLIVersion,
		PlatformOS:     policy.PlatformOS,
		PlatformFamily: policy.PlatformFamily,
	}
	for _, tool := range sortedUniqueTrimmed(policy.ExpectedTools) {
		caps, ok := policy.Coverage.Tools[tool]
		if !ok || len(caps) == 0 {
			return ProtectionCoverage{}, fmt.Errorf("expected_tools entry %q has no coverage-map classification", tool)
		}
		if policy.Coverage.hasCapability(tool, CapabilityUncovered) {
			return ProtectionCoverage{}, fmt.Errorf("expected_tools entry %q is classified uncovered: production eligibility requires the operator to deny it natively first", tool)
		}
		if policy.Coverage.hasCapability(tool, CapabilitySiblingReadPath) && tool != "call_mcp_tool" {
			class, ok := siblingReadClassFor[tool]
			if !ok {
				return ProtectionCoverage{}, fmt.Errorf("sibling_read_path tool %q has no known cprot-v2 tool class mapping", tool)
			}
			cov.siblingReads = append(cov.siblingReads, coveragePath{class: class, name: tool})
		}
		if policy.Coverage.hasCapability(tool, CapabilityOwnMutationPath) {
			class, ok := ownMutationClassFor[tool]
			if !ok {
				return ProtectionCoverage{}, fmt.Errorf("own_mutation_path tool %q has no known cprot-v2 tool class mapping", tool)
			}
			cov.mutations = append(cov.mutations, coveragePath{class: class, name: tool})
		}
	}
	return cov, nil
}

func sortedUniqueTrimmed(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ValidateCoverage enforces the attestation invariants (Validate), the
// version/platform binding, and full coverage of the expected set.
func ValidateCoverage(att codex.ProtectionAttestation, cov ProtectionCoverage) error {
	if err := att.Validate(); err != nil {
		return err
	}
	for _, f := range []struct{ name, have, want string }{
		{"agy version", att.CodexVersion, cov.AgyVersion},
		{"platform os", att.PlatformOS, cov.PlatformOS},
		{"platform family", att.PlatformFamily, cov.PlatformFamily},
	} {
		if f.have != f.want {
			return fmt.Errorf("attestation %s %q does not match the frozen %s %q", f.name, f.have, f.name, f.want)
		}
	}
	return cov.coverRecords(att.ProbeRecords, att.ApprovalDenies)
}

// ValidateFrame decodes a durable cprot-v2 record frame (the
// probe_results column) and enforces structural validity plus full
// coverage of the expected set. The tuple columns (version, platform)
// are matched by the storage lookup; this closes the record-coverage
// half.
func ValidateFrame(raw []byte, cov ProtectionCoverage) error {
	probes, denies, err := codex.DecodeProbeRecords(raw)
	if err != nil {
		return err
	}
	if err := structuralValidate(probes, denies); err != nil {
		return err
	}
	return cov.coverRecords(probes, denies)
}

// structuralValidate reuses codex.ProtectionAttestation.Validate for the
// record-level structural checks (known enums, class-bound operations,
// complete denials, no duplicate record keys) without forking the
// unexported codex.validateRecords: it wraps the decoded records in an
// otherwise-valid attestation shell and calls Validate, whose binding
// fields (version/platform/digests/probed_at/actor) are irrelevant to
// this call and discarded.
func structuralValidate(probes []codex.ProbeRecord, denies []codex.ApprovalDenyRecord) error {
	shell := codex.ProtectionAttestation{
		CodexVersion:   "structural-check",
		PlatformOS:     "structural-check",
		PlatformFamily: "structural-check",
		ManifestDigest: "structural-check",
		ProfileDigest:  "structural-check",
		ProbeRecords:   probes,
		ApprovalDenies: denies,
		ProbedAt:       time.Now().UTC().Format(time.RFC3339),
		Actor:          "structural-check",
	}
	return shell.Validate()
}

func toolClassName(tc codex.ToolClass) string {
	switch tc {
	case codex.ToolRead:
		return "read"
	case codex.ToolGlob:
		return "glob"
	case codex.ToolGrep:
		return "grep"
	case codex.ToolBashAbsolute:
		return "bash_absolute"
	case codex.ToolMCP:
		return "mcp"
	case codex.ToolPlugin:
		return "plugin"
	}
	return fmt.Sprintf("tool_class(%d)", tc)
}

func operationName(op codex.Operation) string {
	switch op {
	case codex.OpRead:
		return "read"
	case codex.OpWrite:
		return "write"
	case codex.OpAppend:
		return "append"
	case codex.OpTruncate:
		return "truncate"
	case codex.OpRename:
		return "rename"
	case codex.OpDelete:
		return "delete"
	}
	return fmt.Sprintf("operation(%d)", op)
}

// coverRecords applies the coverage rules to structurally valid records:
// exactly one sibling_read per expected tool name (several names may
// share a class — unlike codex, the map's unit of coverage is the tool
// NAME, not the class), exactly the five self_mutation ops per expected
// mutation tool name, and exactly one denied_actions approval record.
func (c ProtectionCoverage) coverRecords(probes []codex.ProbeRecord, denies []codex.ApprovalDenyRecord) error {
	reads := make(map[coveragePath]bool)
	muts := make(map[coveragePath]map[codex.Operation]bool)
	for _, r := range probes {
		key := coveragePath{class: r.ToolClass, name: strings.TrimSpace(r.ToolName)}
		switch r.Class {
		case codex.RecordSiblingRead:
			if reads[key] {
				return fmt.Errorf("duplicate coverage: sibling_read record for %s tool %q probed more than once", toolClassName(key.class), key.name)
			}
			reads[key] = true
		case codex.RecordSelfMutation:
			if muts[key] == nil {
				muts[key] = make(map[codex.Operation]bool)
			}
			if muts[key][r.Operation] {
				return fmt.Errorf("duplicate coverage: self_mutation %s record for %s tool %q probed more than once", operationName(r.Operation), toolClassName(key.class), key.name)
			}
			muts[key][r.Operation] = true
		}
	}

	expectedReads := make(map[coveragePath]bool, len(c.siblingReads))
	for _, p := range c.siblingReads {
		expectedReads[p] = true
	}
	for key := range reads {
		if !expectedReads[key] {
			return fmt.Errorf("unexpected coverage: sibling_read record for %s tool %q is not in the frozen expected_tools coverage set", toolClassName(key.class), key.name)
		}
	}
	for _, p := range c.siblingReads {
		if !reads[p] {
			return fmt.Errorf("coverage missing: no sibling_read record for %s tool %q", toolClassName(p.class), p.name)
		}
	}

	expectedMuts := make(map[coveragePath]bool, len(c.mutations))
	for _, p := range c.mutations {
		expectedMuts[p] = true
	}
	for key := range muts {
		if !expectedMuts[key] {
			return fmt.Errorf("unexpected coverage: self_mutation records for %s tool %q is not in the frozen expected_tools coverage set", toolClassName(key.class), key.name)
		}
	}
	for _, p := range c.mutations {
		for _, op := range allMutationOps {
			if !muts[p][op] {
				return fmt.Errorf("coverage missing: no self_mutation %s record for %s tool %q", operationName(op), toolClassName(p.class), p.name)
			}
		}
	}

	// Approval: exactly one denied_actions record with a native refusal
	// enum (spec §3.2 — Agy has no per-variant approval wire protocol).
	var denyCount int
	for _, d := range denies {
		method := strings.TrimSpace(d.MethodName)
		if method != approvalDeniedActionsMethod {
			return fmt.Errorf("unexpected coverage: approval_deny record for unknown method %q (expected only %q)", method, approvalDeniedActionsMethod)
		}
		if d.RefusalKind != codex.RefusalNativeEnum {
			return fmt.Errorf("approval method %q must carry refusal_kind=native_refusal_enum", method)
		}
		denyCount++
	}
	if denyCount == 0 {
		return fmt.Errorf("coverage missing: no approval_deny record for method %q", approvalDeniedActionsMethod)
	}
	if denyCount > 1 {
		return fmt.Errorf("duplicate coverage: more than one approval_deny record for method %q", approvalDeniedActionsMethod)
	}
	return nil
}
