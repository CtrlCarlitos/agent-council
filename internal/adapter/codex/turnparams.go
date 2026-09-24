package codex

// Frozen thread/turn parameter encoding from the profile (AC-009 spec
// §3.4/§3.5): creation pins only the thread cwd; EVERY turn pins model,
// sandboxPolicy, approvalPolicy, and cwd from the frozen
// CodexLaunchPolicy — never values re-derived from ambient config
// (verified resume-drift hazard, §2.2). The turn input is framed as the
// pinned 0.154.0 text-item array; pdig-v1 correlates the exact prompt
// text durably, and live confirmation of the frame shape is the §4
// integration-evidence obligation.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// ThreadStartParamsFor builds the frozen creation params (spec §3.4):
// the thread cwd is the workspace root; model/sandbox/approval are
// pinned per turn at dispatch because resume re-derives them.
func ThreadStartParamsFor(workspaceRoot string) ThreadStartParams {
	return ThreadStartParams{CWD: workspaceRoot}
}

// EncodeTurnInput frames the prompt as the native turn input item array
// ([{"type":"text","text":<prompt>}]) for TurnStartParams.Input. NUL
// bytes are rejected and the prompt must be valid UTF-8 — the same
// fail-closed discipline as pdig-v1 framing.
func EncodeTurnInput(prompt string) (json.RawMessage, error) {
	if strings.IndexByte(prompt, 0) >= 0 {
		return nil, fmt.Errorf("turn input contains a NUL byte")
	}
	if !utf8.ValidString(prompt) {
		return nil, fmt.Errorf("turn input is not valid UTF-8")
	}
	item := []map[string]string{{"type": "text", "text": prompt}}
	raw, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("encode turn input: %w", err)
	}
	return raw, nil
}

// TurnStartParamsFor builds the per-turn pinned params from the frozen
// policy (spec §3.5 step 2): model, sandboxPolicy, approvalPolicy, and
// cwd come only from the frozen profile; input is the framed prompt.
func TurnStartParamsFor(policy CodexLaunchPolicy, model, workspaceRoot, threadID, prompt string) (TurnStartParams, error) {
	input, err := EncodeTurnInput(prompt)
	if err != nil {
		return TurnStartParams{}, err
	}
	sandbox, err := CanonicalSandboxPolicy(policy)
	if err != nil {
		return TurnStartParams{}, err
	}
	return TurnStartParams{
		ThreadID:       threadID,
		Input:          input,
		Model:          model,
		SandboxPolicy:  sandbox,
		ApprovalPolicy: json.RawMessage(policy.ApprovalPolicyCanonical),
		CWD:            workspaceRoot,
	}, nil
}

// CanonicalSandboxPolicy renders the frozen sandbox policy as canonical
// JSON (sorted keys, no insignificant whitespace) — the byte-exact
// comparison form for the effective-config check and the per-turn pin.
func CanonicalSandboxPolicy(policy CodexLaunchPolicy) (json.RawMessage, error) {
	roots := make([]string, 0, len(policy.WritableRoots))
	for _, r := range policy.WritableRoots {
		roots = append(roots, r)
	}
	raw, err := canonicalJSONValue(map[string]any{
		"type":           policy.SandboxType,
		"writable_roots": roots,
		"network_access": policy.NetworkAccess,
	})
	if err != nil {
		return nil, fmt.Errorf("encode sandbox policy: %w", err)
	}
	return json.RawMessage(raw), nil
}

// canonicalJSON re-encodes raw JSON in canonical form: decoded with
// exact number preservation and NFC-normalized string values (the
// frozen-profile normalization family), re-marshaled with sorted keys
// and no insignificant whitespace. Any byte difference against the
// frozen canonical encoding is drift (spec §3.5).
func canonicalJSON(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("decode native config: %w", err)
	}
	return canonicalJSONValue(normalizeJSONStrings(v))
}

// canonicalJSONValue marshals v with sorted map keys and no insignificant
// whitespace (encoding/json sorts map[string]any keys deterministically).
func canonicalJSONValue(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// normalizeJSONStrings applies trim + NFC to every string value in a
// decoded JSON tree, mirroring the freeze-time normalization family.
func normalizeJSONStrings(v any) any {
	switch t := v.(type) {
	case string:
		return norm.NFC.String(strings.TrimSpace(t))
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeJSONStrings(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalizeJSONStrings(val)
		}
		return t
	default:
		return v
	}
}

// canonicalStringSet normalizes a set-valued effective-config field:
// trim + NFC, dedupe, byte-wise lexicographic sort (case-sensitive) —
// the frozen-profile array normalization.
func canonicalStringSet(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = norm.NFC.String(strings.TrimSpace(s))
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
