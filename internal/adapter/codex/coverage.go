package codex

// cprot-v2 coverage binding (spec §3.3/§3.7): an attestation is valid
// ONLY when its record set enumerates every enabled tool path of the
// frozen profile — "unprobed tool classes invalidate it by
// construction". The expected coverage set is DERIVED from the frozen
// launch policy (ValidateCodexHarness output) and the pinned protocol
// surface (the §3.6 approval table), never from the evidence itself:
//
//   - sibling_read: exactly one record for each built-in path class
//     (Read, Glob, Grep, shell-with-absolute-path), plus one per enabled
//     MCP server tool and per skill/plugin-contributed tool.
//   - self_mutation: every mutation-capable path (shell, MCP, plugin)
//     covers write, append, truncate, rename, and delete. Read-only
//     classes (Read/Glob/Grep) carry no mutation records.
//   - approval_deny: every pinned approval method with a schema-native
//     refusal enum carries a native_refusal_enum record; the two
//     deny-EQUIVALENT variants (permissions, requestUserInput) MAY carry
//     a live_verified_equivalent record — absence keeps that variant
//     fail-closed on receipt (§3.6), it does not invalidate the suite.
//   - No unexpected coverage: a record for a tool class, server, plugin,
//     or method the frozen profile does not enable is rejected; a class
//     probed through more than one tool name is duplicate coverage.
//
// Recording (service journal operation) and lookup (adapter eligibility
// seam and launch-time freeze) both enforce this: a durable row that
// does not cover the frozen profile never unlocks production dispatch
// and never freezes protected evidence.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// ProtectionCoverage is the expected cprot-v2 coverage set for one
// frozen launch tuple.
type ProtectionCoverage struct {
	CodexVersion   string
	PlatformOS     string
	PlatformFamily string
	ManifestDigest string
	ProfileDigest  string
	// MCPServers are the frozen expected MCP server names; each must be
	// covered by at least one MCP-class path (tool name equal to the
	// server name or "<server>/<tool>").
	MCPServers []string
	// PluginTools are the frozen skill/plugin-contributed tool names
	// (toolkit manifest expected_plugins ∪ expected_skills); each must
	// be covered by at least one plugin-class path.
	PluginTools []string
}

// CoverageFor derives the expected coverage set from the frozen launch
// policy and the frozen profile digest — the SAME values the launch
// tuple freezes, so coverage and tuple can never disagree.
func CoverageFor(policy CodexLaunchPolicy, profileDigest string) ProtectionCoverage {
	return ProtectionCoverage{
		CodexVersion:   policy.AppServerVersion,
		PlatformOS:     policy.PlatformOS,
		PlatformFamily: policy.PlatformFamily,
		ManifestDigest: policy.ManifestDigest,
		ProfileDigest:  profileDigest,
		MCPServers:     sortedUniqueTrimmed(policy.ExpectedMCPServers),
		PluginTools:    sortedUniqueTrimmed(policy.PluginTools),
	}
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
// exact binding tuple, and full coverage of the expected set.
func (a ProtectionAttestation) ValidateCoverage(c ProtectionCoverage) error {
	if err := a.Validate(); err != nil {
		return err
	}
	for _, f := range []struct{ name, have, want string }{
		{"codex version", a.CodexVersion, c.CodexVersion},
		{"platform os", a.PlatformOS, c.PlatformOS},
		{"platform family", a.PlatformFamily, c.PlatformFamily},
		{"manifest digest", a.ManifestDigest, c.ManifestDigest},
		{"profile digest", a.ProfileDigest, c.ProfileDigest},
	} {
		if f.have != f.want {
			return fmt.Errorf("attestation %s %q does not match the frozen %s %q", f.name, f.have, f.name, f.want)
		}
	}
	return c.coverRecords(a.ProbeRecords, a.ApprovalDenies)
}

// ValidateFrame decodes a durable cprot-v2 record frame (the
// probe_results column) and enforces structural validity plus full
// coverage of the expected set. The tuple columns are matched by the
// storage lookup; this closes the record-coverage half.
func (c ProtectionCoverage) ValidateFrame(raw []byte) error {
	probes, denies, err := DecodeProbeRecords(raw)
	if err != nil {
		return err
	}
	if err := validateRecords(probes, denies); err != nil {
		return err
	}
	return c.coverRecords(probes, denies)
}

type coveragePath struct {
	class ToolClass
	name  string
}

var allMutationOps = []Operation{OpWrite, OpAppend, OpTruncate, OpRename, OpDelete}

func operationName(op Operation) string {
	switch op {
	case OpRead:
		return "read"
	case OpWrite:
		return "write"
	case OpAppend:
		return "append"
	case OpTruncate:
		return "truncate"
	case OpRename:
		return "rename"
	case OpDelete:
		return "delete"
	}
	return fmt.Sprintf("operation(%d)", op)
}

func toolClassName(tc ToolClass) string {
	switch tc {
	case ToolRead:
		return "read"
	case ToolGlob:
		return "glob"
	case ToolGrep:
		return "grep"
	case ToolBashAbsolute:
		return "bash_absolute"
	case ToolMCP:
		return "mcp"
	case ToolPlugin:
		return "plugin"
	}
	return fmt.Sprintf("tool_class(%d)", tc)
}

// coverRecords applies the coverage rules to structurally valid records
// (validateRecords has already rejected unknown enums, denied=false,
// class-bound operation violations, and duplicate keys).
func (c ProtectionCoverage) coverRecords(probes []ProbeRecord, denies []ApprovalDenyRecord) error {
	reads := make(map[coveragePath]bool)
	muts := make(map[coveragePath]map[Operation]bool)
	names := make(map[ToolClass]map[string]struct{})
	for _, r := range probes {
		key := coveragePath{class: r.ToolClass, name: strings.TrimSpace(r.ToolName)}
		if names[key.class] == nil {
			names[key.class] = make(map[string]struct{})
		}
		names[key.class][key.name] = struct{}{}
		switch r.Class {
		case RecordSiblingRead:
			reads[key] = true
		case RecordSelfMutation:
			if muts[key] == nil {
				muts[key] = make(map[Operation]bool)
			}
			muts[key][r.Operation] = true
		}
	}
	sortedNames := func(tc ToolClass) []string {
		out := make([]string, 0, len(names[tc]))
		for n := range names[tc] {
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}
	requirePath := func(key coveragePath, mutation bool) error {
		if !reads[key] {
			return fmt.Errorf("coverage missing: no sibling_read record for %s path %q",
				toolClassName(key.class), key.name)
		}
		if !mutation {
			if len(muts[key]) > 0 {
				return fmt.Errorf("unexpected coverage: self_mutation records for read-only %s path %q",
					toolClassName(key.class), key.name)
			}
			return nil
		}
		for _, op := range allMutationOps {
			if !muts[key][op] {
				return fmt.Errorf("coverage missing: no self_mutation %s record for %s path %q",
					operationName(op), toolClassName(key.class), key.name)
			}
		}
		return nil
	}

	// Built-in classes: exactly one path each. Read/Glob/Grep are
	// read-only; shell-with-absolute-path is mutation-capable.
	for _, tc := range []ToolClass{ToolRead, ToolGlob, ToolGrep, ToolBashAbsolute} {
		paths := sortedNames(tc)
		switch len(paths) {
		case 0:
			return fmt.Errorf("coverage missing: tool class %s has no probe record", toolClassName(tc))
		case 1:
		default:
			return fmt.Errorf("duplicate coverage: tool class %s probed through %d tool names %v",
				toolClassName(tc), len(paths), paths)
		}
		if err := requirePath(coveragePath{class: tc, name: paths[0]}, tc == ToolBashAbsolute); err != nil {
			return err
		}
	}

	// Inventory classes: every recorded path must belong to an expected
	// owner; every expected owner must be covered; every path is
	// mutation-capable (a foreign tool can write through its own
	// process).
	for _, inv := range []struct {
		class    ToolClass
		expected []string
		label    string
	}{
		{ToolMCP, c.MCPServers, "MCP server"},
		{ToolPlugin, c.PluginTools, "skill/plugin tool"},
	} {
		paths := sortedNames(inv.class)
		if len(inv.expected) == 0 {
			if len(paths) > 0 {
				return fmt.Errorf("unexpected coverage: %s records %v but the frozen profile enables no %s",
					toolClassName(inv.class), paths, inv.label)
			}
			continue
		}
		covered := make(map[string]bool, len(inv.expected))
		for _, p := range paths {
			owner, ok := coverageOwner(p, inv.expected)
			if !ok {
				return fmt.Errorf("unexpected coverage: %s path %q is not an expected %s (%v)",
					toolClassName(inv.class), p, inv.label, inv.expected)
			}
			covered[owner] = true
			if err := requirePath(coveragePath{class: inv.class, name: p}, true); err != nil {
				return err
			}
		}
		for _, e := range inv.expected {
			if !covered[e] {
				return fmt.Errorf("coverage missing: expected %s %q has no probe record", inv.label, e)
			}
		}
	}

	// Approval methods: the pinned §3.6 table is the universe.
	pinned := pinnedApprovalMethods()
	seen := make(map[string]bool, len(denies))
	for _, d := range denies {
		method := strings.TrimSpace(d.MethodName)
		denyEquivalent, ok := pinned[method]
		if !ok {
			return fmt.Errorf("unexpected coverage: approval_deny record for unknown method %q", method)
		}
		seen[method] = true
		if denyEquivalent && d.RefusalKind != RefusalLiveVerifiedEquivalent {
			return fmt.Errorf("approval method %q has no native refusal enum; its record must be live_verified_equivalent", method)
		}
		if !denyEquivalent && d.RefusalKind != RefusalNativeEnum {
			return fmt.Errorf("approval method %q has a schema-native refusal enum; its record must be native_refusal_enum", method)
		}
	}
	methods := make([]string, 0, len(pinned))
	for m := range pinned {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	for _, m := range methods {
		if !pinned[m] && !seen[m] {
			return fmt.Errorf("coverage missing: no approval_deny record for method %q", m)
		}
	}
	return nil
}

// coverageOwner resolves the expected owner of an inventory path: the
// exact owner name, or the longest owner prefix followed by "/".
func coverageOwner(path string, expected []string) (string, bool) {
	best, ok := "", false
	for _, e := range expected {
		if path == e || strings.HasPrefix(path, e+"/") {
			if !ok || len(e) > len(best) {
				best, ok = e, true
			}
		}
	}
	return best, ok
}

// ── cprot-v2 record frame decoding ──────────────────────────────────────

// DecodeProbeRecords decodes a cprot-v2 record frame
// (ProtectionAttestation.EncodeProbeRecords output) into its probe and
// approval_deny records. Truncation, an unknown class or enum, invalid
// UTF-8, or trailing bytes is an error (callers fail closed).
func DecodeProbeRecords(raw []byte) ([]ProbeRecord, []ApprovalDenyRecord, error) {
	if len(raw) < 4 {
		return nil, nil, errors.New("cprot record frame too short")
	}
	pos := 0
	count := int(binary.BigEndian.Uint32(raw[:4]))
	pos = 4
	take := func(n int) ([]byte, error) {
		if n < 0 || pos+n > len(raw) {
			return nil, errors.New("truncated cprot record frame")
		}
		b := raw[pos : pos+n]
		pos += n
		return b, nil
	}
	takeU32 := func() (int, error) {
		b, err := take(4)
		if err != nil {
			return 0, err
		}
		return int(binary.BigEndian.Uint32(b)), nil
	}
	takeString := func(what string) (string, error) {
		n, err := takeU32()
		if err != nil {
			return "", err
		}
		b, err := take(n)
		if err != nil {
			return "", err
		}
		if !utf8.Valid(b) {
			return "", fmt.Errorf("%s is not valid UTF-8", what)
		}
		return string(b), nil
	}
	var probes []ProbeRecord
	var denies []ApprovalDenyRecord
	for i := 0; i < count; i++ {
		classB, err := take(1)
		if err != nil {
			return nil, nil, err
		}
		switch RecordClass(classB[0]) {
		case RecordApprovalDeny:
			name, err := takeString("approval_deny method_name")
			if err != nil {
				return nil, nil, err
			}
			kindB, err := take(1)
			if err != nil {
				return nil, nil, err
			}
			switch RefusalKind(kindB[0]) {
			case RefusalNativeEnum, RefusalLiveVerifiedEquivalent:
			default:
				return nil, nil, fmt.Errorf("unknown refusal kind %d", kindB[0])
			}
			denies = append(denies, ApprovalDenyRecord{MethodName: name, RefusalKind: RefusalKind(kindB[0])})
		case RecordSiblingRead, RecordSelfMutation:
			tcB, err := take(1)
			if err != nil {
				return nil, nil, err
			}
			name, err := takeString("probe record tool_name")
			if err != nil {
				return nil, nil, err
			}
			tail, err := take(3) // operation, enforcing capability, denied
			if err != nil {
				return nil, nil, err
			}
			text, err := takeString("probe record denial excerpt")
			if err != nil {
				return nil, nil, err
			}
			probes = append(probes, ProbeRecord{
				Class:               RecordClass(classB[0]),
				ToolClass:           ToolClass(tcB[0]),
				ToolName:            name,
				Operation:           Operation(tail[0]),
				EnforcingCapability: EnforcingCapability(tail[1]),
				Denied:              tail[2] == 1,
				DenialText:          text,
			})
		default:
			return nil, nil, fmt.Errorf("unknown cprot record class %d", classB[0])
		}
	}
	if pos != len(raw) {
		return nil, nil, errors.New("trailing bytes after cprot record frame")
	}
	return probes, denies, nil
}
