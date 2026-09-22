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
	"unicode/utf8"
)

// ProbeToolClass enumerates the transcript-path probe classes in their
// fixed canonical order.
type ProbeToolClass uint8

const (
	ProbeRead ProbeToolClass = iota + 1
	ProbeGlob
	ProbeGrep
	ProbeBash
	ProbeMCP
	ProbePlugin
)

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

// AttestationDigest computes cprot-v1:sha256:<hex> over the canonical
// attestation payload. Any probe record with denied=false invalidates the
// attestation (protected evidence requires every path proven denied).
func AttestationDigest(claudeVersion, platform, manifestDigest, templateDigest string, records []ProbeRecord, probedAtRFC3339, actor string) (string, error) {
	if strings.TrimSpace(claudeVersion) == "" {
		return "", fmt.Errorf("attestation requires the probed CLI version")
	}
	if strings.TrimSpace(platform) == "" {
		return "", fmt.Errorf("attestation requires the platform")
	}
	if strings.TrimSpace(manifestDigest) == "" || strings.TrimSpace(templateDigest) == "" {
		return "", fmt.Errorf("attestation requires the manifest and template digests")
	}
	if strings.TrimSpace(actor) == "" {
		return "", fmt.Errorf("attestation requires the operator actor")
	}
	if len(records) == 0 {
		return "", fmt.Errorf("attestation requires at least one probe record")
	}

	canonical := make([]ProbeRecord, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for i, r := range records {
		name := strings.TrimSpace(r.ToolName)
		if name == "" {
			return "", fmt.Errorf("probe record %d: empty tool name", i)
		}
		text := normalizeExcerpt(r.DenialText)
		if text == "" {
			return "", fmt.Errorf("probe record %d: empty denial text", i)
		}
		if !r.Denied {
			return "", fmt.Errorf("probe record %d (%s): a path that was not denied invalidates protected evidence", i, name)
		}
		switch r.EnforcingCapability {
		case CapCwdBoundary, CapGuardrailHook, CapDenyList, CapPermissionDenial:
		default:
			return "", fmt.Errorf("probe record %d (%s): unknown enforcing capability %q", i, name, r.EnforcingCapability)
		}
		key := fmt.Sprintf("%d\x00%s", r.ToolClass, name)
		if _, dup := seen[key]; dup {
			return "", fmt.Errorf("probe record %d: duplicate tool class+name %q", i, name)
		}
		seen[key] = struct{}{}
		canonical = append(canonical, ProbeRecord{
			ToolClass:           r.ToolClass,
			ToolName:            name,
			Denied:              true,
			EnforcingCapability: r.EnforcingCapability,
			DenialText:          text,
		})
	}
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].ToolClass != canonical[j].ToolClass {
			return canonical[i].ToolClass < canonical[j].ToolClass
		}
		return canonical[i].ToolName < canonical[j].ToolName
	})

	buf := make([]byte, 0, 256)
	appendField := func(s string) {
		buf = append(buf, u32be(uint32(len(s)))...)
		buf = append(buf, s...)
	}
	appendField("cprot-v1")
	appendField(claudeVersion)
	appendField(platform)
	appendField(manifestDigest)
	appendField(templateDigest)
	buf = append(buf, u32be(uint32(len(canonical)))...)
	for _, r := range canonical {
		appendField(r.ToolName)
		buf = append(buf, byte(r.ToolClass))
		if r.Denied {
			buf = append(buf, 1)
		} else {
			buf = append(buf, 0)
		}
		appendField(string(r.EnforcingCapability))
		appendField(r.DenialText)
	}
	appendField(probedAtRFC3339)
	appendField(actor)

	return "cprot-v1:sha256:" + fmt.Sprintf("%x", sha256.Sum256(buf)), nil
}

// normalizeExcerpt trims, collapses internal whitespace runs to single
// spaces, and truncates at the last valid UTF-8 code-point boundary that
// keeps the result within 256 bytes (never splitting a code point).
func normalizeExcerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 256
	if len(s) <= max {
		return s
	}
	out := s[:max]
	for len(out) > 0 && !utf8.ValidString(out) {
		out = out[:len(out)-1] // drop the partial trailing rune
	}
	return out
}
