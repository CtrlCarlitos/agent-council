package codextest

// Test-only cprot-v2 attestation builder: the minimal record set that
// COVERS a frozen profile (codex coverage.go), with fixture denial
// excerpts. It exists so service and acceptance tests can exercise the
// operator journal operation and the eligibility/protection lookups
// with a structurally covering suite. It is evidence-SHAPED, never
// evidence: production packages must not import codextest (guard_test.go),
// and no operator path ever records a row built here.

import (
	"sort"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
)

// CoveringAttestation returns an attestation covering cov: one
// sibling_read record per built-in class, sibling_read + the five
// self_mutation operations for the shell path and for every expected
// MCP server ("<server>/read_file") and plugin tool, and a
// native_refusal_enum approval_deny record for every pinned approval
// method that has a schema-native refusal enum. probedAt must be
// RFC3339 UTC.
func CoveringAttestation(cov codex.ProtectionCoverage, probedAt, actor string) codex.ProtectionAttestation {
	probe := func(class codex.RecordClass, tc codex.ToolClass, name string, op codex.Operation) codex.ProbeRecord {
		return codex.ProbeRecord{Class: class, ToolClass: tc, ToolName: name, Operation: op, Denied: true,
			EnforcingCapability: codex.CapSandboxRestrictedFS, DenialText: "fixture denial: " + name}
	}
	att := codex.ProtectionAttestation{
		CodexVersion: cov.CodexVersion, PlatformOS: cov.PlatformOS, PlatformFamily: cov.PlatformFamily,
		ManifestDigest: cov.ManifestDigest, ProfileDigest: cov.ProfileDigest,
		ProbedAt: probedAt, Actor: actor,
	}
	att.ProbeRecords = append(att.ProbeRecords,
		probe(codex.RecordSiblingRead, codex.ToolRead, "Read", codex.OpRead),
		probe(codex.RecordSiblingRead, codex.ToolGlob, "Glob", codex.OpRead),
		probe(codex.RecordSiblingRead, codex.ToolGrep, "Grep", codex.OpRead),
	)
	mutationPath := func(tc codex.ToolClass, name string) {
		att.ProbeRecords = append(att.ProbeRecords, probe(codex.RecordSiblingRead, tc, name, codex.OpRead))
		for _, op := range []codex.Operation{codex.OpWrite, codex.OpAppend, codex.OpTruncate, codex.OpRename, codex.OpDelete} {
			att.ProbeRecords = append(att.ProbeRecords, probe(codex.RecordSelfMutation, tc, name, op))
		}
	}
	mutationPath(codex.ToolBashAbsolute, "Bash")
	for _, s := range cov.MCPServers {
		mutationPath(codex.ToolMCP, s+"/read_file")
	}
	for _, p := range cov.PluginTools {
		mutationPath(codex.ToolPlugin, p)
	}
	methods := codex.PinnedApprovalMethods()
	names := make([]string, 0, len(methods))
	for m := range methods {
		names = append(names, m)
	}
	sort.Strings(names)
	for _, m := range names {
		if !methods[m] {
			att.ApprovalDenies = append(att.ApprovalDenies,
				codex.ApprovalDenyRecord{MethodName: m, RefusalKind: codex.RefusalNativeEnum})
		}
	}
	return att
}
