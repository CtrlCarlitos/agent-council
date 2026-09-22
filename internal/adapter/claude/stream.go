package claude

// NDJSON stream parser and §3.9 failure state machine (AC-008 spec):
// one pass over the process stdout producing typed events and a terminal
// outcome. Any started process without exactly one verified terminal
// result remains Uncertain.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Stream event types surfaced to observer taps.
type StreamEventType string

const (
	EventProgress      StreamEventType = "progress"
	EventToolRequested StreamEventType = "tool_requested"
	EventToolDenied    StreamEventType = "tool_denied"
	EventTerminal      StreamEventType = "terminal"
)

// Denial classes (spec §3.8): each maps to a distinct verified native
// denial text.
const (
	DenialApprovalDenied string = "approval_denied"
	DenialGuardrail      string = "guardrail_denied"
	DenialToolDisabled   string = "tool_disabled"
)

// StreamConfig carries the frozen expectations the stream is validated
// against.
type StreamConfig struct {
	WorkspaceRoot         string
	ExpectedVersion       string // probed CLI version from the manifest
	ExpectedModelIdentity string // frozen resolved native model id
	ExpectedSessionID     string // the native session this stream must belong to
	Manifest              storage.ToolkitManifest
	UniverseTools         []string // pinned native tool universe
	MaxLineBytes          int
	MaxTotalBytes         int
}

// StreamEvent is one observable event on the turn stream.
type StreamEvent struct {
	Type        StreamEventType
	DeniedClass string // for ToolDenied: approval_denied|guardrail_denied|tool_disabled
	ToolName    string
	Text        string
}

// InitReport is the toolkit evidence captured from system/init plus the
// hooks observed on the stream.
type InitReport struct {
	SessionID      string
	Version        string
	Model          string
	PermissionMode string
	Cwd            string
	Tools          []string
	Skills         []string
	Plugins        []string
	HooksSeen      []string
	APIKeySource   string
}

// StreamOutcome is the parser's terminal verdict for one invocation.
// ResultUsage carries the verified terminal usage/cost payload for
// persistence (AC-008 §3.11 result_usage).
type ResultUsage struct {
	Subtype       string
	IsError       bool
	DurationAPIms int64
	TotalCostUSD  float64
	Raw           string // canonical JSON of the usage block
}

type StreamOutcome struct {
	Terminal             bool
	Completed            bool
	ResultText           string
	ResultUsage          *ResultUsage
	SessionID            string
	Poisoned             bool
	PoisonReason         string
	Init                 *InitReport
	TerminalEventEmitted bool
}

// ParsedToolResult is a structured tool_result from the native stream.
type ParsedToolResult struct {
	ToolName string
	IsError  bool
	Text     string
}

// nativeEvent is the raw per-line envelope.
type nativeEvent struct {
	Type          string          `json:"type"`
	Subtype       string          `json:"subtype"`
	SessionID     string          `json:"session_id"`
	HookName      string          `json:"hook_name"`
	Cwd           string          `json:"cwd"`
	Version       string          `json:"claude_code_version"`
	Model         string          `json:"model"`
	PermMode      string          `json:"permissionMode"`
	APIKeySrc     string          `json:"apiKeySource"`
	Tools         []string        `json:"tools"`
	Skills        []string        `json:"skills"`
	Plugins       []pluginRef     `json:"plugins"`
	Message       messageEnvelope `json:"message"`
	Result        string          `json:"result"`
	IsError       bool            `json:"is_error"`
	DurationAPIMs int64           `json:"duration_api_ms"`
	TotalCostUSD  float64         `json:"total_cost_usd"`
}

type pluginRef struct {
	Name string `json:"name"`
}

// messageEnvelope covers assistant and user messages.
type messageEnvelope struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// ParseStream reads one invocation's NDJSON stdout, validates it against
// the frozen expectations, and returns the terminal outcome. onEvent is
// called for observable events (may be nil). A nil error does NOT imply
// success: inspect the outcome.
func ParseStream(r io.Reader, cfg StreamConfig, onEvent func(StreamEvent)) (StreamOutcome, error) {
	if strings.TrimSpace(cfg.ExpectedSessionID) == "" {
		return StreamOutcome{}, fmt.Errorf("production stream parsing requires an ExpectedSessionID")
	}
	out := StreamOutcome{}
	emit := func(ev StreamEvent) {
		if onEvent != nil {
			onEvent(ev)
		}
	}

	var initReport *InitReport
	var hooksSeen []string
	pendingTool := make(map[string]string) // tool_use_id -> tool name
	sawEvidence := false                   // assistant/user/result observed
	resultCount := 0
	totalBytes := 0
	reader := bufio.NewReader(r)

	// orderingAndCorrelation enforces: evidence events require init
	// first and a matching session id.
	check := func(ev nativeEvent) bool {
		if initReport == nil {
			out.Poisoned, out.PoisonReason = true, fmt.Sprintf(
				"%s event before system/init", ev.Type)
			return false
		}
		if cfg.ExpectedSessionID != "" && ev.SessionID != cfg.ExpectedSessionID {
			out.Poisoned, out.PoisonReason = true, fmt.Sprintf(
				"%s session_id %q does not match the expected native session %q",
				ev.Type, ev.SessionID, cfg.ExpectedSessionID)
			return false
		}
		return true
	}

	for {
		line, more, readErr := readBoundedLine(reader, cfg.MaxLineBytes, &totalBytes)
		if totalBytes > cfg.MaxTotalBytes {
			out.Poisoned, out.PoisonReason = true, "stream exceeded total byte budget"
			return out, nil
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			out.Poisoned, out.PoisonReason = true, fmt.Sprintf("stream read: %v", readErr)
			return out, nil
		}
		if !more {
			// Unterminated final line: torn tail, tolerated as a
			// non-evidence tail. The partial line is discarded.
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		var ev nativeEvent
		if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
			out.Poisoned, out.PoisonReason = true, fmt.Sprintf("malformed NDJSON mid-stream: %v", err)
			return out, nil
		}

		switch ev.Type {
		case "system":
			switch ev.Subtype {
			case "init":
				if sawEvidence {
					out.Poisoned, out.PoisonReason = true, "system/init after turn evidence"
					return out, nil
				}
				if cfg.ExpectedSessionID != "" && ev.SessionID != cfg.ExpectedSessionID {
					out.Poisoned, out.PoisonReason = true, fmt.Sprintf(
						"init session_id %q does not match the expected native session %q",
						ev.SessionID, cfg.ExpectedSessionID)
					return out, nil
				}
				rep, reason := validateInit(ev, cfg, hooksSeen)
				if reason != "" {
					out.Poisoned, out.PoisonReason = true, reason
					return out, nil
				}
				initReport = rep
				out.SessionID = rep.SessionID
			case "hook_started":
				if ev.HookName != "" {
					hooksSeen = append(hooksSeen, ev.HookName)
				}
			}
		case "assistant":
			sawEvidence = true
			if !check(ev) {
				return out, nil
			}
			var blocks []contentBlock
			if len(ev.Message.Content) > 0 {
				if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
					out.Poisoned, out.PoisonReason = true, fmt.Sprintf("malformed assistant content: %v", err)
					return out, nil
				}
				for _, b := range blocks {
					if b.Type == "text" && b.Text != "" {
						emit(StreamEvent{Type: EventProgress, Text: b.Text})
					}
					if b.Type == "tool_use" {
						// Correlate tool_use_id -> tool name so the
						// matching tool_result can name the actual tool.
						pendingTool[b.ID] = b.Name
						emit(StreamEvent{Type: EventToolRequested, ToolName: b.Name})
					}
				}
			}
		case "user":
			sawEvidence = true
			if !check(ev) {
				return out, nil
			}
			var blocks []contentBlock
			if len(ev.Message.Content) > 0 {
				if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
					out.Poisoned, out.PoisonReason = true, fmt.Sprintf("malformed user content: %v", err)
					return out, nil
				}
				for _, b := range blocks {
					if b.Type != "tool_result" {
						continue
					}
					toolName, correlated := pendingTool[b.ToolUseID]
					if !correlated {
						out.Poisoned, out.PoisonReason = true, fmt.Sprintf(
							"tool_result references uncorrelated tool_use_id %q", b.ToolUseID)
						return out, nil
					}
					if b.IsError {
						if class, isDenial := classifyDenial(textOf(b.Content)); isDenial {
							emit(StreamEvent{Type: EventToolDenied, DeniedClass: class,
								ToolName: toolName, Text: textOf(b.Content)})
							continue
						}
						emit(StreamEvent{Type: EventProgress,
							Text: "tool error: " + textOf(b.Content)})
						continue
					}
					emit(StreamEvent{Type: EventProgress, ToolName: toolName,
						Text: textOf(b.Content)})
				}
			}
		case "result":
			sawEvidence = true
			if initReport == nil {
				out.Poisoned, out.PoisonReason = true, "result before system/init"
				return out, nil
			}
			if cfg.ExpectedSessionID != "" && ev.SessionID != cfg.ExpectedSessionID {
				out.Poisoned, out.PoisonReason = true, fmt.Sprintf(
					"result session_id %q does not match the expected native session %q",
					ev.SessionID, cfg.ExpectedSessionID)
				return out, nil
			}
			resultCount++
			if resultCount > 1 {
				out.Poisoned, out.PoisonReason = true, "multiple result events (protocol drift)"
				return out, nil
			}
			out.Terminal = true
			out.SessionID = ev.SessionID
			out.ResultText = ev.Result
			out.Completed = !ev.IsError && ev.Subtype == "success"
			out.ResultUsage = &ResultUsage{
				Subtype:       ev.Subtype,
				IsError:       ev.IsError,
				DurationAPIms: ev.DurationAPIMs,
				TotalCostUSD:  ev.TotalCostUSD,
				Raw:           trimmed,
			}
		case "rate_limit_event":
			// advisory; ignored
		}
	}

	if initReport == nil {
		out.Poisoned, out.PoisonReason = true, "stream ended without a system/init event"
		return out, nil
	}
	out.Init = initReport
	out.Init.HooksSeen = hooksSeen
	// The terminal event is emitted ONLY when exactly one verified
	// result was observed: a clean EOF without a result is an
	// uncertain end, never a terminal.
	if resultCount == 1 && out.Terminal {
		emit(StreamEvent{Type: EventTerminal, Text: out.ResultText})
		out.TerminalEventEmitted = true
	}
	return out, nil
}

// validateInit checks the init event against the frozen expectations.
// Returns a toolkit-evidence poison reason on mismatch.
func validateInit(ev nativeEvent, cfg StreamConfig, hooksSeen []string) (*InitReport, string) {
	rep := &InitReport{
		SessionID:      ev.SessionID,
		Version:        ev.Version,
		Model:          ev.Model,
		PermissionMode: ev.PermMode,
		Cwd:            ev.Cwd,
		Tools:          ev.Tools,
		Skills:         ev.Skills,
		APIKeySource:   ev.APIKeySrc,
	}
	for _, p := range ev.Plugins {
		rep.Plugins = append(rep.Plugins, p.Name)
	}
	rep.HooksSeen = hooksSeen

	if ev.Cwd != cfg.WorkspaceRoot {
		return rep, fmt.Sprintf("init cwd %q does not match the workspace root %q", ev.Cwd, cfg.WorkspaceRoot)
	}
	if ev.Version != cfg.ExpectedVersion {
		return rep, fmt.Sprintf("init claude_code_version %q does not match the probed version %q", ev.Version, cfg.ExpectedVersion)
	}
	// Frozen model identity: the manifest pins the exact resolved native
	// model id; a fuzzy alias match is never accepted.
	if cfg.ExpectedModelIdentity == "" || ev.Model != cfg.ExpectedModelIdentity {
		return rep, fmt.Sprintf("init model %q does not match the frozen native model identity %q",
			ev.Model, cfg.ExpectedModelIdentity)
	}
	if ev.PermMode != "" && ev.PermMode != "default" {
		return rep, fmt.Sprintf("init permissionMode %q is not the native default", ev.PermMode)
	}

	// Enabled tools must be EXACTLY universe - denied complement: any
	// unknown tool, any missing enabled tool, any denied tool enabled.
	denied := make(map[string]struct{}, len(cfg.Manifest.DeniedComplement))
	for _, d := range cfg.Manifest.DeniedComplement {
		denied[d] = struct{}{}
	}
	expectedEnabled := make(map[string]struct{}, len(cfg.UniverseTools))
	for _, tool := range cfg.UniverseTools {
		if _, isDenied := denied[tool]; !isDenied {
			expectedEnabled[tool] = struct{}{}
		}
	}
	enabled := make(map[string]struct{}, len(ev.Tools))
	for _, tool := range ev.Tools {
		enabled[tool] = struct{}{}
		if _, known := universeLookup(cfg.UniverseTools, tool); !known {
			return rep, fmt.Sprintf("init reports unknown tool %q (not in the pinned universe)", tool)
		}
		if _, isDenied := denied[tool]; isDenied {
			return rep, fmt.Sprintf("init reports denied tool %q as enabled", tool)
		}
	}
	for tool := range expectedEnabled {
		if _, on := enabled[tool]; !on {
			return rep, fmt.Sprintf("init is missing expected enabled tool %q", tool)
		}
	}

	// Skills/plugins are exact canonical set comparisons: missing AND
	// unexpected entries both fail.
	if reason := exactSetMismatch("skills", setOf(cfg.Manifest.ExpectedSkills), setOf(ev.Skills)); reason != "" {
		return rep, fmt.Errorf("%s", reason).Error()
	}
	if reason := exactSetMismatch("plugins", setOf(cfg.Manifest.ExpectedPlugins), setOf(rep.Plugins)); reason != "" {
		return rep, fmt.Errorf("%s", reason).Error()
	}
	for _, hook := range cfg.Manifest.ExpectedHooks {
		found := false
		for _, h := range hooksSeen {
			if h == hook {
				found = true
				break
			}
		}
		if !found {
			return rep, fmt.Sprintf("init stream is missing expected hook %q", hook)
		}
	}
	return rep, ""
}

func setOf(list []string) map[string]struct{} {
	m := make(map[string]struct{}, len(list))
	for _, s := range list {
		m[s] = struct{}{}
	}
	return m
}

// exactSetMismatch returns a reason string when expected and actual
// differ in either direction; "" when they are exact canonical sets.
func exactSetMismatch(field string, expected, actual map[string]struct{}) string {
	for v := range expected {
		if _, ok := actual[v]; !ok {
			return fmt.Sprintf("init is missing expected %s %q", field, v)
		}
	}
	for v := range actual {
		if _, ok := expected[v]; !ok {
			return fmt.Sprintf("init reports unexpected %s %q", field, v)
		}
	}
	return ""
}

func universeLookup(universe []string, tool string) (struct{}, bool) {
	for _, u := range universe {
		if u == tool {
			return struct{}{}, true
		}
	}
	return struct{}{}, false
}

// classifyDenial maps structured denial texts to the three denial
// classes. Returns ("", false) for genuine tool errors.
func classifyDenial(text string) (string, bool) {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "requested permissions") && strings.Contains(lower, "haven't granted"):
		return DenialApprovalDenied, true
	case strings.Contains(lower, "guardrail denied") ||
		strings.Contains(lower, "pretooluse:") && strings.Contains(lower, "hook error"):
		return DenialGuardrail, true
	case strings.Contains(lower, "no such tool available") ||
		strings.Contains(lower, "disabled for this session"):
		return DenialToolDisabled, true
	default:
		return "", false
	}
}

func textOf(content json.RawMessage) string {
	// content may be a string or an array of typed blocks.
	var asText string
	if err := json.Unmarshal(content, &asText); err == nil {
		return asText
	}
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return string(content)
}

// readBoundedLine reads one newline-terminated line enforcing the
// per-line and total byte budgets. A torn final line (EOF without \n)
// is returned as-is: the parse step tolerates or poisons it, per §3.9.
// The returned hasMore flag reports whether more stream remains.
func readBoundedLine(r *bufio.Reader, maxLine int, total *int) (line string, more bool, err error) {
	var acc []byte
	for {
		chunk, err := r.ReadByte()
		if err != nil {
			if err == io.EOF {
				if len(acc) > 0 {
					*total += len(acc)
					if len(acc) > maxLine {
						return "", false, fmt.Errorf("final line exceeds %d bytes", maxLine)
					}
					return string(acc), false, nil
				}
				return "", false, io.EOF
			}
			return "", false, err
		}
		acc = append(acc, chunk)
		*total++
		if len(acc) > maxLine {
			return "", false, fmt.Errorf("line exceeds %d bytes", maxLine)
		}
		if chunk == '\n' {
			return strings.TrimSuffix(string(acc), "\n"), true, nil
		}
	}
}

// utf8 is used for torn-tail validity.
var _ = utf8.RuneLen
