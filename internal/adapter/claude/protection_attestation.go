package claude

// cprot-v1 canonical protection-attestation encoding (spec §3.6):
// a typed probe-record list with fixed enum ordering, byte-wise sorting,
// UTF-8-safe excerpt normalization, duplicate rejection, and a fixed-
// field-order length-prefixed attestation frame. Semantically identical
// probe runs produce identical digests.

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ProbeToolClass enumerates the transcript-path probe classes in their
// fixed canonical order. Unknown values are rejected.
type ProbeToolClass uint8

const (
	ProbeRead ProbeToolClass = iota + 1
	ProbeGlob
	ProbeGrep
	ProbeBash
	ProbeMCP
	ProbePlugin
)

// canonicalName is the length-prefixed string form used in the frame.
func (c ProbeToolClass) canonicalName() (string, error) {
	switch c {
	case ProbeRead:
		return "read", nil
	case ProbeGlob:
		return "glob", nil
	case ProbeGrep:
		return "grep", nil
	case ProbeBash:
		return "bash", nil
	case ProbeMCP:
		return "mcp", nil
	case ProbePlugin:
		return "plugin", nil
	default:
		return "", fmt.Errorf("unknown probe tool class %d", c)
	}
}

// Enforcing capabilities that may be credited for a verified denial.
type EnforcingCapability string

const (
	CapCwdBoundary      EnforcingCapability = "cwd_boundary"
	CapGuardrailHook    EnforcingCapability = "guardrail_hook"
	CapDenyList         EnforcingCapability = "deny_list"
	CapPermissionDenial EnforcingCapability = "permission_denial"
)

// ProbeRecord is one transcript-path denial probe result.
type ProbeRecord struct {
	ToolClass           ProbeToolClass
	ToolName            string
	Denied              bool
	EnforcingCapability EnforcingCapability
	DenialText          string
}

// ProtectionAttestation is the typed cprot-v1 domain value binding the
// probed CLI version, platform, digests in force, probe results, time,
// and actor into one canonical evidence record.
type ProtectionAttestation struct {
	ClaudeVersion  string
	Platform       string
	ManifestDigest string // manifest in force (e.g. cprof-v2:sha256:…)
	TemplateDigest string // template in force (e.g. ctmpl-v1:sha256:…)
	Records        []ProbeRecord
	ProbedAt       string // RFC3339 UTC
	Actor          string
}

const (
	probeTargetLiteral = "sibling-transcript"
	maxExcerptBytes    = 256
)

// Validate enforces the attestation invariants: non-empty binding fields,
// complete denial records (denied=true, named capability, non-empty
// excerpt), known tool classes, no duplicate (class, name) pairs.
func (a ProtectionAttestation) Validate() error {
	if strings.TrimSpace(a.ClaudeVersion) == "" {
		return fmt.Errorf("attestation requires the probed CLI version")
	}
	if strings.TrimSpace(a.Platform) == "" {
		return fmt.Errorf("attestation requires the platform")
	}
	if strings.TrimSpace(a.ManifestDigest) == "" || strings.TrimSpace(a.TemplateDigest) == "" {
		return fmt.Errorf("attestation requires the manifest and template digests")
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
	if len(a.Records) == 0 {
		return fmt.Errorf("attestation requires at least one probe record")
	}
	seen := make(map[string]struct{}, len(a.Records))
	for i, r := range a.Records {
		className, err := r.ToolClass.canonicalName()
		if err != nil {
			return fmt.Errorf("probe record %d: %w", i, err)
		}
		name := strings.TrimSpace(r.ToolName)
		if name == "" {
			return fmt.Errorf("probe record %d: empty tool name", i)
		}
		if !r.Denied {
			return fmt.Errorf("probe record %d (%s): a path that was not denied invalidates protected evidence", i, name)
		}
		switch r.EnforcingCapability {
		case CapCwdBoundary, CapGuardrailHook, CapDenyList, CapPermissionDenial:
		default:
			return fmt.Errorf("probe record %d (%s): unknown enforcing capability %q", i, name, r.EnforcingCapability)
		}
		if strings.TrimSpace(r.DenialText) == "" {
			return fmt.Errorf("probe record %d (%s): empty denial text", i, name)
		}
		key := className + "\x00" + name
		if _, dup := seen[key]; dup {
			return fmt.Errorf("probe record %d: duplicate tool class+name %q", i, name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// Digest computes cprot-v1:sha256:<hex> over the canonical attestation
// frame. Any probe record with denied=false invalidates the attestation
// (protected evidence requires every path proven denied).
func (a ProtectionAttestation) Digest() (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}

	canonical := make([]ProbeRecord, 0, len(a.Records))
	for _, r := range a.Records {
		canonical = append(canonical, ProbeRecord{
			ToolClass:           r.ToolClass,
			ToolName:            strings.TrimSpace(r.ToolName),
			Denied:              true,
			EnforcingCapability: r.EnforcingCapability,
			DenialText:          normalizeExcerpt(r.DenialText),
		})
	}
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].ToolClass != canonical[j].ToolClass {
			return canonical[i].ToolClass < canonical[j].ToolClass
		}
		return canonical[i].ToolName < canonical[j].ToolName
	})

	buf := make([]byte, 0, 256)
	field := func(s string) {
		buf = append(buf, u32be(uint32(len(s)))...)
		buf = append(buf, s...)
	}
	// Fixed field order (spec §3.6): version, platform, manifest digest,
	// template digest, framed records, probed_at, actor.
	field("cprot-v1")
	field(a.ClaudeVersion)
	field(a.Platform)
	field(a.ManifestDigest)
	field(a.TemplateDigest)
	buf = append(buf, u32be(uint32(len(canonical)))...)
	for _, r := range canonical {
		className, _ := r.ToolClass.canonicalName()
		field(className)          // 1. tool_class (canonical name)
		field(r.ToolName)         // 2. tool_name
		field(probeTargetLiteral) // 3. target (fixed literal)
		if r.Denied {             // 4. denied (one byte)
			buf = append(buf, 1)
		} else {
			buf = append(buf, 0)
		}
		field(string(r.EnforcingCapability)) // 5. enforcing capability
		field(r.DenialText)                  // 6. excerpt
	}
	field(a.ProbedAt)
	field(a.Actor)

	return "cprot-v1:sha256:" + fmt.Sprintf("%x", sha256.Sum256(buf)), nil
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
