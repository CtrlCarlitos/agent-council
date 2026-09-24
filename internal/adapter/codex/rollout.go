package codex

// Rollout trust model for the codex adapter (AC-009 spec §3.7) and the
// §3.5 step-3 post-launch turn_context confirmation. The rollout is the
// durable turn-correlation evidence: the path is NOT derivable from the
// thread id alone (date components), so first acceptance resolves it by
// a bounded scan constrained to the exact UUID suffix, verifies
// session_meta.session_id == the native id, and records the baseline
// (file-identity, byte size, entry count). Reads past the baseline are
// tail-only and size-capped; a torn final line is tolerated; a mid-file
// parse failure invalidates the read. Entry correlation (protected mode)
// is ORDERED: turn_context (frozen pins match) → user response_item
// (pdig match) → task_complete (error absent = completed, populated =
// failed). Advisory mode treats the same reads as diagnostics only.
// Windows is an unverified platform: integrity is reported unverified
// and every decision degrades to Uncertain (fail-closed honest gap).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
)

// maxRolloutScanEntries bounds the UUID-suffix scan: the sessions tree
// grows without bound in the operator home, so the walk aborts (fail
// closed) rather than scanning the whole tree.
const maxRolloutScanEntries = 4096

// MaxRolloutTailBytes bounds a tail-only read past a recorded baseline;
// growth beyond the bound fails closed instead of being parsed. A var so
// tests can shrink it.
var MaxRolloutTailBytes = int64(8 << 20)

// rolloutTrustSupported reports whether POSIX-grade rollout integrity
// checks (symlink/containment, ownership, mode identity) are verified on
// the running platform. Windows is the fail-closed honest gap (§3.7):
// callers degrade to integrity=unverified and Uncertain decisions.
func rolloutTrustSupported() bool { return runtime.GOOS != "windows" }

// codexPlatformIdentity is the attestation binding string for the frozen
// launch platform (§3.7: platform is the profile manifest shape os +
// family). Both sides of the protection tuple — the launch-time lookup
// and the journal operation's durable rows — derive the string from the
// frozen policy/attestation pair, so the tuple match can never disagree
// on platform identity.
func codexPlatformIdentity(policy CodexLaunchPolicy) string {
	return policy.PlatformOS + "/" + policy.PlatformFamily
}

// ResolveRollout resolves the rollout path for a native thread id by the
// bounded UUID-suffix scan under $CODEX_HOME/sessions, verifies
// session_meta.session_id == the native id, and enforces the
// path-integrity rules (no symlinks, contained under the sessions root).
// The returned path is the PHYSICAL path (the scan walks the symlink-
// resolved sessions root, so a home behind a symlinked ancestor such as
// macOS /var → /private/var resolves to the spelling the native child
// experiences); it is what first acceptance records on the binding, and
// checkRolloutPath accepts it against either spelling of the home.
func ResolveRollout(codexHome, nativeID string) (string, error) {
	path, err := locateRollout(codexHome, nativeID)
	if err != nil {
		return "", err
	}
	if _, _, _, err := scanRolloutBaseline(path, nativeID); err != nil {
		return "", err
	}
	return path, nil
}

// ── Rollout entries ─────────────────────────────────────────────────────

// rolloutEntry is the decoded subset of one rollout JSONL line the
// trust model reads: session identity, the effective per-turn context
// (turn_context), the user input response_item, and the terminal
// event_msg task_complete. Unknown line types decode with empty fields
// and are skipped by correlation.
type rolloutEntry struct {
	Timestamp string `json:"timestamp,omitempty"`
	Type      string `json:"type"`
	Payload   struct {
		SessionID         string          `json:"session_id"`
		Type              string          `json:"type"`
		Role              string          `json:"role"`
		Content           json.RawMessage `json:"content"`
		TurnID            string          `json:"turn_id"`
		CWD               string          `json:"cwd"`
		Model             string          `json:"model"`
		ApprovalPolicy    json.RawMessage `json:"approval_policy"`
		ApprovalsReviewer string          `json:"approvals_reviewer"`
		SandboxPolicy     json.RawMessage `json:"sandbox_policy"`
		LastAgentMessage  *string         `json:"last_agent_message"`
		Error             json.RawMessage `json:"error"`
	} `json:"payload"`
}

// parseRolloutLine strictly decodes one rollout line. A torn final line
// never reaches this function through the bounded readers; a decode
// failure here is a mid-file parse failure and invalidates the read.
func parseRolloutLine(line []byte) (rolloutEntry, error) {
	var e rolloutEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return rolloutEntry{}, err
	}
	if strings.TrimSpace(e.Type) == "" {
		return rolloutEntry{}, fmt.Errorf("rollout entry carries no type")
	}
	return e, nil
}

// rolloutUserText extracts the exact user input text from a
// response_item message payload: a plain string content, or the first
// text block of a content array.
func rolloutUserText(e rolloutEntry) string {
	var text string
	if err := json.Unmarshal(e.Payload.Content, &text); err == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(e.Payload.Content, &blocks); err == nil {
		for _, b := range blocks {
			if strings.TrimSpace(b.Text) != "" {
				return b.Text
			}
		}
	}
	return ""
}

// ── Tail-only reads past the baseline ───────────────────────────────────

// readRolloutTail reads the rollout entries STRICTLY AFTER the recorded
// baseline byte size (tail-only, size-capped). A partial line at the
// baseline boundary is treated as the already-recorded torn tail and
// skipped; a torn FINAL line is tolerated (dropped); a mid-file parse
// failure invalidates the whole read. Growth beyond MaxRolloutTailBytes
// fails closed.
func readRolloutTail(path string, afterBytes int64) ([]rolloutEntry, error) {
	if !rolloutTrustSupported() {
		return nil, fmt.Errorf("rollout reads are not verified on %s; failing closed (§3.7)", runtime.GOOS)
	}
	if afterBytes < 0 {
		return nil, fmt.Errorf("negative rollout baseline offset")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rollout: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat rollout: %w", err)
	}
	if afterBytes > st.Size() {
		return nil, fmt.Errorf("rollout %s shrank below the recorded baseline", path)
	}
	if st.Size()-afterBytes > MaxRolloutTailBytes {
		return nil, fmt.Errorf("rollout %s grew beyond the %d-byte tail bound", path, MaxRolloutTailBytes)
	}

	// Line alignment: afterBytes may sit inside the torn final line that
	// was already counted at baseline time. Step back one byte and skip
	// to the next newline unless the offset is exactly line-aligned.
	offset := afterBytes
	if offset > 0 {
		buf := make([]byte, 1)
		if _, err := f.ReadAt(buf, offset-1); err != nil {
			return nil, fmt.Errorf("rollout boundary check: %w", err)
		}
		if buf[0] != '\n' {
			// Skip the remainder of the partially-recorded line.
			aligned := make([]byte, 1)
			for {
				if _, err := f.ReadAt(aligned, offset); err != nil {
					if err == io.EOF {
						// Nothing but the torn remnant exists.
						return nil, nil
					}
					return nil, fmt.Errorf("rollout alignment: %w", err)
				}
				if aligned[0] == '\n' {
					offset++
					break
				}
				offset++
			}
		}
	}

	var entries []rolloutEntry
	sc := bufio.NewScanner(io.LimitReader(io.NewSectionReader(f, offset, st.Size()-offset), MaxRolloutTailBytes))
	sc.Buffer(make([]byte, 64<<10), MaxFrameBytes)
	for {
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		e, perr := parseRolloutLine([]byte(line))
		if perr != nil {
			if sc.Scan() {
				// Content follows the bad line: mid-file parse failure
				// invalidates the read (spec §3.7).
				return nil, fmt.Errorf("rollout %s tail is corrupt past offset %d: %w", path, offset, perr)
			}
			// Nothing followed: a torn final line is ignored.
			break
		}
		entries = append(entries, e)
	}
	if serr := sc.Err(); serr != nil {
		return nil, fmt.Errorf("read rollout %s tail: %w", path, serr)
	}
	return entries, nil
}

// ── Protected entry correlation (§3.7) ──────────────────────────────────

// RolloutPins are the frozen per-turn pins a turn_context entry is
// compared against (§3.5 step 2/3): model, cwd, canonical approval
// policy, frozen reviewer, and canonical sandbox policy.
type RolloutPins struct {
	Model                   string
	CWD                     string
	ApprovalPolicyCanonical string
	ApprovalsReviewer       string
	SandboxPolicyCanonical  string
}

// RolloutPinDrift reports the first turn_context field that disagreed
// with the frozen pins.
type RolloutPinDrift struct {
	Field string
	Want  string
	Have  string
}

// RolloutTaskComplete is the decoded terminal record of a task_complete
// entry: error absent ⇒ completed, populated ⇒ failed.
type RolloutTaskComplete struct {
	Failed           bool
	ErrorMessage     string
	LastAgentMessage string
}

// RolloutTurnEvidence is the ordered-correlation result for THIS turn's
// rollout entries past the baseline (§3.7): the effective-profile
// confirmation, the durable prompt acceptance, and the terminal record.
type RolloutTurnEvidence struct {
	TurnContextMatch  bool
	Drift             *RolloutPinDrift
	PromptDigestMatch bool
	TaskComplete      *RolloutTaskComplete
}

// CorrelateRolloutTurn applies the §3.7 ordered correlation over the
// entries read past THIS attempt's baseline: (a) the turn_context whose
// effective-profile fields match the frozen pins, (b) the user input
// response_item whose pdig framing equals wantDigest, then (c) the
// matching task_complete. Entries that belong to the correlation are
// matched in order; unrelated entries between them are skipped. A turn
// context that appears out of order (after the user item) fails the
// correlation for the turn.
func CorrelateRolloutTurn(entries []rolloutEntry, pins RolloutPins, nativeThreadID, turnKey, attemptID, wantDigest string) (RolloutTurnEvidence, error) {
	ev := RolloutTurnEvidence{}
	stage := 0 // 0 = want turn_context, 1 = want response_item, 2 = want task_complete
	for _, e := range entries {
		switch stage {
		case 0:
			if e.Type != "turn_context" {
				continue
			}
			drift := turnContextDrift(e, pins)
			if drift != nil {
				ev.Drift = drift
				return ev, nil
			}
			ev.TurnContextMatch = true
			stage = 1
		case 1:
			if e.Type != "response_item" {
				continue
			}
			if !strings.EqualFold(e.Payload.Role, "user") {
				// Assistant/tool items between the context and the user
				// input are not the correlation item.
				continue
			}
			text := rolloutUserText(e)
			if strings.TrimSpace(text) == "" {
				continue
			}
			digest, err := PromptDigest(nativeThreadID, turnKey, attemptID, text)
			if err != nil {
				return ev, fmt.Errorf("correlate rollout user item: %w", err)
			}
			if digest != wantDigest {
				// The first user item past the context must carry THIS
				// turn's prompt; anything else is a mismatch, not a skip.
				return ev, nil
			}
			ev.PromptDigestMatch = true
			stage = 2
		case 2:
			if e.Type != "event_msg" || e.Payload.Type != "task_complete" {
				continue
			}
			tc := &RolloutTaskComplete{
				Failed:       len(e.Payload.Error) > 0 && string(e.Payload.Error) != "null",
				ErrorMessage: strings.Trim(string(e.Payload.Error), `"`),
			}
			if e.Payload.LastAgentMessage != nil {
				tc.LastAgentMessage = *e.Payload.LastAgentMessage
			}
			ev.TaskComplete = tc
			return ev, nil
		}
	}
	return ev, nil
}

// turnContextDrift compares one turn_context entry's effective-profile
// fields against the frozen pins and returns the FIRST disagreement
// (§3.5 step 3). Canonical byte comparison for the policy fields; exact
// match for scalars.
func turnContextDrift(e rolloutEntry, pins RolloutPins) *RolloutPinDrift {
	drift := func(field, want, have string) *RolloutPinDrift {
		return &RolloutPinDrift{Field: field, Want: want, Have: have}
	}
	if strings.TrimSpace(e.Payload.Model) != strings.TrimSpace(pins.Model) {
		return drift("model", pins.Model, e.Payload.Model)
	}
	if strings.TrimSpace(e.Payload.CWD) != strings.TrimSpace(pins.CWD) {
		return drift("cwd", pins.CWD, e.Payload.CWD)
	}
	if e.Payload.ApprovalPolicy != nil {
		native, err := canonicalJSON(e.Payload.ApprovalPolicy)
		if err != nil {
			return drift("approval_policy", pins.ApprovalPolicyCanonical, "unparseable: "+err.Error())
		}
		if native != pins.ApprovalPolicyCanonical {
			return drift("approval_policy", pins.ApprovalPolicyCanonical, native)
		}
	}
	if strings.TrimSpace(e.Payload.ApprovalsReviewer) != strings.TrimSpace(pins.ApprovalsReviewer) {
		return drift("approvals_reviewer", pins.ApprovalsReviewer, e.Payload.ApprovalsReviewer)
	}
	if e.Payload.SandboxPolicy != nil {
		native, err := canonicalJSON(e.Payload.SandboxPolicy)
		if err != nil {
			return drift("sandbox_policy", pins.SandboxPolicyCanonical, "unparseable: "+err.Error())
		}
		if native != pins.SandboxPolicyCanonical {
			return drift("sandbox_policy", pins.SandboxPolicyCanonical, native)
		}
	}
	return nil
}

// RolloutPinsFor builds the frozen pin set for a turn from the launch
// policy and the binding's frozen model/workspace.
func RolloutPinsFor(policy CodexLaunchPolicy, model, workspaceRoot string) (RolloutPins, error) {
	sandbox, err := CanonicalSandboxPolicy(policy)
	if err != nil {
		return RolloutPins{}, err
	}
	return RolloutPins{
		Model:                   model,
		CWD:                     workspaceRoot,
		ApprovalPolicyCanonical: policy.ApprovalPolicyCanonical,
		ApprovalsReviewer:       policy.ApprovalsReviewer,
		SandboxPolicyCanonical:  string(sandbox),
	}, nil
}

// ── Protection classification (§3.7 protected-evidence upgrade) ─────────

// rolloutProtectionClass freezes the rollout protection class for an
// attempt at launch: protected ONLY while a valid isolation attestation
// is in force for the launch — the seam reports one AND the attestation
// row exists for the frozen (codex version, platform, profile) tuple —
// and the platform's integrity checks are verified. Advisory is the
// default; the unverified platform degrades to integrity=unverified and
// every downstream decision to Uncertain. The returned id is the
// attestation frozen on the attempt.
func rolloutProtectionClass(platform string, seamOK bool, seamID, rowID string) (class, id string) {
	if platform == "windows" || !rolloutTrustSupported() {
		return "unverified", ""
	}
	if !seamOK || strings.TrimSpace(seamID) == "" {
		return "advisory", ""
	}
	if rowID == "" || rowID != seamID {
		// The seam's attestation has no matching durable row for the
		// frozen tuple: never freeze protected evidence on it.
		return "advisory", ""
	}
	return "protected", seamID
}

// ── Terminal classification helpers ─────────────────────────────────────

// turnStatusFromBody extracts the raw turn status string from a turn
// notification body: both the plain-string spelling
// ({"turn":{"status":"completed"}}) and the object spelling
// ({"turn":{"status":{"type":"inProgress"}}}) parse; ok is false only
// for an unparseable or turn-less body.
func turnStatusFromBody(params json.RawMessage) (status string, ok bool) {
	if len(params) == 0 {
		return "", false
	}
	var p struct {
		Turn *struct {
			Status json.RawMessage `json:"status"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Turn == nil {
		return "", false
	}
	if len(p.Turn.Status) > 0 && p.Turn.Status[0] == '{' {
		var st ThreadStatus
		if err := json.Unmarshal(p.Turn.Status, &st); err != nil {
			return "", false
		}
		return st.Type, true
	}
	return strings.Trim(string(p.Turn.Status), `" `), true
}

// isVerifiedTerminalStatus reports whether a native turn status is a
// verified terminal for the adapter: completed or failed. interrupted is
// the verified CANCELLED terminal handled by the cancel path.
func isVerifiedTerminalStatus(status string) bool {
	return status == "completed" || status == "failed"
}

// truncateEventPayload clamps an event payload to the AC-006 stream
// bound with a visible truncation marker — bounded delivery, never a
// silent evidence drop.
func truncateEventPayload(s string) string {
	if len(s) <= adapter.MaxEventPayloadBytes {
		return s
	}
	out := s[:adapter.MaxEventPayloadBytes-32]
	for len(out) > 0 && !utf8.ValidString(out) {
		out = out[:len(out)-1]
	}
	return out + "\n…(truncated)"
}
