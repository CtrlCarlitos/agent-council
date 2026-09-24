package codex

// cprot-v2 canonical protection-attestation encoding (spec §3.7):
// a tagged union of probe and approval-deny records with fixed enum
// ordering, byte-wise sorting, UTF-8-safe excerpt normalization,
// duplicate rejection, and a fixed-field-order length-prefixed
// attestation frame. Semantically identical probe runs produce
// identical digests.

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

func u32be(n uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return b
}

// RecordClass enumerates the cprot-v2 record classes in their fixed
// canonical order. Unknown values are rejected.
type RecordClass uint8

const (
	RecordSiblingRead RecordClass = iota + 1
	RecordSelfMutation
	RecordApprovalDeny
)

// ToolClass enumerates the probed tool classes in their fixed canonical
// order. Unknown values are rejected.
type ToolClass uint8

const (
	ToolRead ToolClass = iota + 1
	ToolGlob
	ToolGrep
	ToolBashAbsolute
	ToolMCP
	ToolPlugin
)

// Operation enumerates the probed read and mutation operations.
// Validity is class-bound: sibling_read records MUST be read;
// self_mutation records MUST be a mutation (write..delete).
type Operation uint8

const (
	OpRead Operation = iota + 1
	OpWrite
	OpAppend
	OpTruncate
	OpRename
	OpDelete
)

// Enforcing capabilities that may be credited for a verified denial.
type EnforcingCapability uint8

const (
	CapCwdBoundary EnforcingCapability = iota + 1
	CapSandboxRestrictedFS
	CapGuardrailHook
	CapDenyList
	CapPermissionDenial
)

// RefusalKind records how an approval variant's deny semantics are
// established: a schema-native refusal enum, or a live-verified
// deny-equivalent (spec §3.6).
type RefusalKind uint8

const (
	RefusalNativeEnum RefusalKind = iota + 1
	RefusalLiveVerifiedEquivalent
)

// ProbeRecord is one sibling-read or self-mutation probe result.
type ProbeRecord struct {
	Class               RecordClass // sibling_read or self_mutation
	ToolClass           ToolClass
	ToolName            string
	Operation           Operation
	Denied              bool
	EnforcingCapability EnforcingCapability
	DenialText          string
}

// ApprovalDenyRecord is one approval variant's established deny
// semantics.
type ApprovalDenyRecord struct {
	MethodName  string
	RefusalKind RefusalKind
}

// ProtectionAttestation is the typed cprot-v2 domain value binding the
// probed codex version, platform, digests in force, probe and approval
// records, time, and actor into one canonical evidence record.
type ProtectionAttestation struct {
	CodexVersion   string
	PlatformOS     string
	PlatformFamily string
	ManifestDigest string // toolkit manifest in force
	ProfileDigest  string // cprof-v3 profile in force
	ProbeRecords   []ProbeRecord
	ApprovalDenies []ApprovalDenyRecord
	ProbedAt       string // RFC3339 UTC
	Actor          string
}

const maxExcerptBytes = 256

// PlatformIdentity is the canonical platform binding string (§3.7): the
// profile manifest shape os + family joined with "/". The journal
// operation stores THIS string in the durable platform column and the
// launch-time lookup compares against the same derivation from the
// frozen policy — one canonical form on both sides of the tuple match.
func (a ProtectionAttestation) PlatformIdentity() string {
	return a.PlatformOS + "/" + a.PlatformFamily
}

// Validate enforces the attestation invariants: valid UTF-8 in every
// string field, non-empty binding fields, RFC3339-UTC probed_at, known
// enums, complete denial records
// (denied=true, named capability, non-empty excerpt), class-bound
// operations, and no duplicate record keys. A sibling_read record with
// operation ≠ read, a self_mutation record with a non-mutation
// operation, or any denied=false probe record invalidates the whole
// attestation.
func (a ProtectionAttestation) Validate() error {
	// The framing family is UTF-8 everywhere (spec §3.7): any string
	// field carrying invalid UTF-8 fails closed before encoding —
	// invalid bytes must never reach the canonical frame, and an
	// over-length invalid excerpt would otherwise truncate silently.
	binding := []struct {
		name  string
		value string
	}{
		{"codex version", a.CodexVersion},
		{"platform os", a.PlatformOS},
		{"platform family", a.PlatformFamily},
		{"manifest digest", a.ManifestDigest},
		{"profile digest", a.ProfileDigest},
		{"probed_at", a.ProbedAt},
		{"actor", a.Actor},
	}
	for _, f := range binding {
		if !utf8.ValidString(f.value) {
			return fmt.Errorf("attestation %s is not valid UTF-8", f.name)
		}
	}
	if strings.TrimSpace(a.CodexVersion) == "" {
		return fmt.Errorf("attestation requires the probed codex version")
	}
	if strings.TrimSpace(a.PlatformOS) == "" {
		return fmt.Errorf("attestation requires the platform os")
	}
	if strings.TrimSpace(a.PlatformFamily) == "" {
		return fmt.Errorf("attestation requires the platform family")
	}
	if strings.TrimSpace(a.ManifestDigest) == "" || strings.TrimSpace(a.ProfileDigest) == "" {
		return fmt.Errorf("attestation requires the manifest and profile digests")
	}
	if strings.TrimSpace(a.ProbedAt) == "" {
		return fmt.Errorf("attestation requires the probe timestamp")
	}
	ts, err := time.Parse(time.RFC3339, a.ProbedAt)
	if err != nil {
		return fmt.Errorf("attestation probed_at must be RFC3339: %w", err)
	}
	if ts.Location() != time.UTC {
		return fmt.Errorf("attestation probed_at must be UTC")
	}
	if strings.TrimSpace(a.Actor) == "" {
		return fmt.Errorf("attestation requires the operator actor")
	}
	return validateRecords(a.ProbeRecords, a.ApprovalDenies)
}

// validateRecords enforces the record-level invariants shared by the
// typed attestation and a decoded durable frame: at least one record,
// known enums, class-bound operations, complete denials, valid UTF-8,
// and no duplicate keys. Coverage against the frozen profile is a
// separate, stricter check (coverage.go).
func validateRecords(probes []ProbeRecord, denies []ApprovalDenyRecord) error {
	if len(probes)+len(denies) == 0 {
		return fmt.Errorf("attestation requires at least one record")
	}

	seen := make(map[string]struct{}, len(probes))
	for i, r := range probes {
		switch r.Class {
		case RecordSiblingRead, RecordSelfMutation:
		default:
			return fmt.Errorf("probe record %d: unknown record class %d", i, r.Class)
		}
		switch r.ToolClass {
		case ToolRead, ToolGlob, ToolGrep, ToolBashAbsolute, ToolMCP, ToolPlugin:
		default:
			return fmt.Errorf("probe record %d (%s): unknown tool class %d", i, r.ToolName, r.ToolClass)
		}
		if !utf8.ValidString(r.ToolName) {
			return fmt.Errorf("probe record %d: tool name is not valid UTF-8", i)
		}
		name := strings.TrimSpace(r.ToolName)
		if name == "" {
			return fmt.Errorf("probe record %d: empty tool name", i)
		}
		switch r.Class {
		case RecordSiblingRead:
			if r.Operation != OpRead {
				return fmt.Errorf("probe record %d (%s): sibling_read requires operation read", i, name)
			}
		case RecordSelfMutation:
			if r.Operation < OpWrite || r.Operation > OpDelete {
				return fmt.Errorf("probe record %d (%s): self_mutation requires a mutation operation", i, name)
			}
		}
		switch r.EnforcingCapability {
		case CapCwdBoundary, CapSandboxRestrictedFS, CapGuardrailHook, CapDenyList, CapPermissionDenial:
		default:
			return fmt.Errorf("probe record %d (%s): unknown enforcing capability %d", i, name, r.EnforcingCapability)
		}
		if !r.Denied {
			return fmt.Errorf("probe record %d (%s): a path that was not denied invalidates protected evidence", i, name)
		}
		if !utf8.ValidString(r.DenialText) {
			return fmt.Errorf("probe record %d (%s): denial text is not valid UTF-8", i, name)
		}
		if strings.TrimSpace(r.DenialText) == "" {
			return fmt.Errorf("probe record %d (%s): empty denial text", i, name)
		}
		key := fmt.Sprintf("%d\x00%d\x00%d\x00%s", r.Class, r.ToolClass, r.Operation, name)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("probe record %d: duplicate record class+tool class+operation+name %q", i, name)
		}
		seen[key] = struct{}{}
	}

	methods := make(map[string]struct{}, len(denies))
	for i, r := range denies {
		if !utf8.ValidString(r.MethodName) {
			return fmt.Errorf("approval deny record %d: method name is not valid UTF-8", i)
		}
		method := strings.TrimSpace(r.MethodName)
		if method == "" {
			return fmt.Errorf("approval deny record %d: empty method name", i)
		}
		switch r.RefusalKind {
		case RefusalNativeEnum, RefusalLiveVerifiedEquivalent:
		default:
			return fmt.Errorf("approval deny record %d (%s): unknown refusal kind %d", i, method, r.RefusalKind)
		}
		if _, dup := methods[method]; dup {
			return fmt.Errorf("approval deny record %d: duplicate approval method %q", i, method)
		}
		methods[method] = struct{}{}
	}
	return nil
}

// EncodeProbeRecords produces the canonical framed record list
// (uint32-BE count, then per record the class byte and its class body
// in fixed order, byte-wise sorted: sibling_read, then self_mutation by
// (tool_class, operation, tool_name); approval_deny by method_name).
// This is the cprot-v2 encoding persisted in
// codex_protection_attestations.probe_results — never free-form JSON.
func (a ProtectionAttestation) EncodeProbeRecords() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	probeCanonical := canonicalProbeRecords(a.ProbeRecords)
	denyCanonical := canonicalApprovalDenies(a.ApprovalDenies)
	buf := make([]byte, 0, 256)
	field := func(s string) {
		buf = append(buf, u32be(uint32(len(s)))...)
		buf = append(buf, s...)
	}
	buf = append(buf, u32be(uint32(len(probeCanonical)+len(denyCanonical)))...)
	for _, r := range probeCanonical {
		buf = append(buf, byte(r.Class)) // 1. record class
		buf = append(buf, byte(r.ToolClass))
		field(r.ToolName) // 2. tool_name
		buf = append(buf, byte(r.Operation))
		buf = append(buf, byte(r.EnforcingCapability))
		buf = append(buf, 1) // 3. denied (every probe record is denied)
		field(r.DenialText)  // 4. excerpt
	}
	for _, r := range denyCanonical {
		buf = append(buf, byte(RecordApprovalDeny)) // 1. record class
		field(r.MethodName)                         // 2. method_name
		buf = append(buf, byte(r.RefusalKind))      // 3. refusal_kind
	}
	return buf, nil
}

func canonicalProbeRecords(records []ProbeRecord) []ProbeRecord {
	canonical := make([]ProbeRecord, 0, len(records))
	for _, r := range records {
		canonical = append(canonical, ProbeRecord{
			Class:               r.Class,
			ToolClass:           r.ToolClass,
			ToolName:            strings.TrimSpace(r.ToolName),
			Operation:           r.Operation,
			Denied:              true,
			EnforcingCapability: r.EnforcingCapability,
			DenialText:          normalizeExcerpt(r.DenialText),
		})
	}
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Class != canonical[j].Class {
			return canonical[i].Class < canonical[j].Class
		}
		if canonical[i].ToolClass != canonical[j].ToolClass {
			return canonical[i].ToolClass < canonical[j].ToolClass
		}
		if canonical[i].Operation != canonical[j].Operation {
			return canonical[i].Operation < canonical[j].Operation
		}
		return canonical[i].ToolName < canonical[j].ToolName
	})
	return canonical
}

func canonicalApprovalDenies(records []ApprovalDenyRecord) []ApprovalDenyRecord {
	canonical := make([]ApprovalDenyRecord, 0, len(records))
	for _, r := range records {
		canonical = append(canonical, ApprovalDenyRecord{
			MethodName:  strings.TrimSpace(r.MethodName),
			RefusalKind: r.RefusalKind,
		})
	}
	sort.Slice(canonical, func(i, j int) bool {
		return canonical[i].MethodName < canonical[j].MethodName
	})
	return canonical
}

// Digest computes cprot-v2:sha256:<hex> over the canonical attestation
// frame. Identical canonical payloads produce identical digests;
// probed_at and actor are part of the payload, so a re-run at a
// different time or by a different actor intentionally produces a
// different digest.
func (a ProtectionAttestation) Digest() (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	records, err := a.EncodeProbeRecords()
	if err != nil {
		return "", err
	}

	buf := make([]byte, 0, 256)
	field := func(s string) {
		buf = append(buf, u32be(uint32(len(s)))...)
		buf = append(buf, s...)
	}
	// Fixed field order (spec §3.7): framing tag, codex version,
	// platform os, platform family, manifest digest, profile digest,
	// framed records, probed_at, actor.
	field("cprot-v2")
	field(a.CodexVersion)
	field(a.PlatformOS)
	field(a.PlatformFamily)
	field(a.ManifestDigest)
	field(a.ProfileDigest)
	buf = append(buf, records...)
	field(a.ProbedAt)
	field(a.Actor)

	return "cprot-v2:sha256:" + fmt.Sprintf("%x", sha256.Sum256(buf)), nil
}

// normalizeExcerpt trims, collapses internal whitespace runs to single
// spaces, and truncates at the last valid UTF-8 code-point boundary that
// keeps the result within 256 bytes (never splitting a code point).
func normalizeExcerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxExcerptBytes {
		return s
	}
	out := s[:maxExcerptBytes]
	for len(out) > 0 && !utf8.ValidString(out) {
		out = out[:len(out)-1] // drop the partial trailing rune
	}
	return out
}
