package agy

// Provider-free strict decoding of the agy stream-json protocol (AC-010
// research §7.1-§7.5, verbatim event shapes): three output event kinds
// (init, step_update, terminal result) read from the child's stdout, one
// input envelope shape (user) written to its stdin, and the two stderr
// markers the adapter must recognize without parsing the model's output.
//
// Strictness rules (spec "unknown ⇒ Uncertain, never terminal success"):
// every typed envelope decode uses encoding/json's DisallowUnknownFields,
// so an unrecognized field ANYWHERE in the envelope — top-level or inside
// the nested init/step_update/result object — is protocol drift, exactly
// like an unrecognized "event" value or an out-of-vocabulary "state"/
// "status" enum. A line that is not valid JSON at all is a distinct,
// non-drift condition (ErrMalformedEvent): the two are kept separate
// because a caller may want to retry/reclassify a malformed line
// differently than a line that parsed but violated the frozen contract.
//
// This file has no storage import: agy's stream protocol is decoded the
// same way regardless of how (or whether) a caller persists it.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// maxEventLineBytes bounds a single stream-json line (stdout event or
// stdin message): 1 MiB. A line at or under the bound is accepted; a
// line strictly longer is ErrLineTooLong.
const maxEventLineBytes = 1 << 20

var (
	// ErrLineTooLong is returned when a stream-json line exceeds the 1
	// MiB bound before any JSON parsing is attempted.
	ErrLineTooLong = errors.New("agy stream event line exceeds the 1 MiB bound")

	// ErrMalformedEvent is returned when a stream-json line is not valid
	// JSON at all — distinct from ErrProtocolDrift, which reports a
	// line that parsed but violated the frozen event contract (unknown
	// event/step_type/state/status, or an unrecognized field anywhere
	// in the envelope).
	ErrMalformedEvent = errors.New("agy stream event line is not valid json")

	// ErrEmptyUserMessage is returned by EncodeUserMessage when handed
	// empty text: NUL bytes and newlines inside the text are not a
	// concern (JSON string encoding escapes both, so neither can break
	// out of the envelope), but an empty prompt is never meaningful.
	ErrEmptyUserMessage = errors.New("agy user message text is empty")
)

// ErrProtocolDrift reports agy stream-json behavior that contradicts the
// verified 1.2.9 contract (AC-010 research §7.1): an unrecognized event
// kind, step_type, state, or status, or an unrecognized field anywhere in
// a typed envelope. Per spec, drift is always Uncertain, never a silent
// best-effort parse.
type ErrProtocolDrift struct {
	// Event is the top-level "event" value the drifted line carried
	// (when known — it is empty when the drift is the event value
	// itself being unrecognized before any sub-object was inspected).
	Event string
	// Reason describes exactly what violated the frozen contract.
	Reason string
}

func (e *ErrProtocolDrift) Error() string {
	if e.Event == "" {
		return "agy protocol drift: " + e.Reason
	}
	return "agy protocol drift on " + e.Event + ": " + e.Reason
}

// EventKind is the closed vocabulary of agy stream-json output events.
type EventKind string

const (
	EventKindInit       EventKind = "init"
	EventKindStepUpdate EventKind = "step_update"
	EventKindResult     EventKind = "result"
)

// StepType is the closed step_update discriminator vocabulary
// (AC-010 research §7.1).
type StepType string

const (
	StepTypeUserInput     StepType = "user_input"
	StepTypeAgentResponse StepType = "agent_response"
	StepTypeSystemMessage StepType = "system_message"
	StepTypeTool          StepType = "tool"
	StepTypeErrorMessage  StepType = "error_message"
)

// Usage is the token-accounting object carried by both step_update
// (per-step) and result (conversation-cumulative).
type Usage struct {
	InputTokens     int
	OutputTokens    int
	ThinkingTokens  int
	CacheReadTokens int
	TotalTokens     int
}

// InitEvent is the payload of an "init" event: emitted once, before any
// stdin message is read.
type InitEvent struct {
	// Model is the --model value, present only when the launch set it.
	Model          string
	CWD            string
	Tools          []string
	PermissionMode string
}

// StepUpdate is the payload of a "step_update" event.
type StepUpdate struct {
	Index     int
	State     string // ACTIVE | DONE
	Type      StepType
	TextDelta string
	ToolName  string
	ToolInfo  json.RawMessage
	Duration  float64
	Usage     *Usage
}

// DeniedAction is one entry of a result's denied_actions list (AC-010
// research §7.3: a tool auto-denied under headless request-review).
type DeniedAction struct {
	Action      string
	DisplayName string
}

// ResultEvent is the payload of the terminal "result" event: exactly one
// per process, whether the turn succeeded, was denied, was interrupted,
// or the input envelope was rejected.
type ResultEvent struct {
	Status        string // SUCCESS | ERROR
	Response      string
	Error         string
	Duration      float64
	NumTurns      int
	Usage         Usage
	DeniedActions []DeniedAction
}

// Event is one decoded agy stream-json output event. Exactly one of
// Init, Step, Result is non-nil, selected by Kind. ConversationID is
// always populated regardless of Kind, even though the wire shape places
// it at the top level for "init" but nested inside "step_update"/
// "result" (AC-010 research §7.1, verbatim).
type Event struct {
	Kind           EventKind
	ConversationID string
	Init           *InitEvent
	Step           *StepUpdate
	Result         *ResultEvent
}

// ── wire shapes (strict decode targets) ─────────────────────────────────

type wireUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

func (w wireUsage) toUsage() Usage {
	return Usage{
		InputTokens:     w.InputTokens,
		OutputTokens:    w.OutputTokens,
		ThinkingTokens:  w.ThinkingTokens,
		CacheReadTokens: w.CacheReadTokens,
		TotalTokens:     w.TotalTokens,
	}
}

// wireProbe sniffs only the "event" discriminator. It is deliberately
// lenient (no DisallowUnknownFields): its only job is routing to the
// correct strict decode, which is where every field is actually
// validated.
type wireProbe struct {
	Event string `json:"event"`
}

type wireInitPayload struct {
	Model          string   `json:"model"`
	CWD            string   `json:"cwd"`
	Tools          []string `json:"tools"`
	PermissionMode string   `json:"permission_mode"`
}

type wireInitEnvelope struct {
	Event          string           `json:"event"`
	ConversationID string           `json:"conversation_id"`
	Init           *wireInitPayload `json:"init"`
}

type wireStepPayload struct {
	ConversationID  string          `json:"conversation_id"`
	StepIndex       int             `json:"step_index"`
	State           string          `json:"state"`
	StepType        string          `json:"step_type"`
	TextDelta       string          `json:"text_delta"`
	ToolName        string          `json:"tool_name"`
	ToolInfo        json.RawMessage `json:"tool_info"`
	DurationSeconds float64         `json:"duration_seconds"`
	Usage           *wireUsage      `json:"usage"`
}

type wireStepEnvelope struct {
	Event      string           `json:"event"`
	StepUpdate *wireStepPayload `json:"step_update"`
}

type wireDeniedAction struct {
	Action      string `json:"action"`
	DisplayName string `json:"display_name"`
}

type wireResultPayload struct {
	ConversationID  string             `json:"conversation_id"`
	Status          string             `json:"status"`
	Response        string             `json:"response"`
	Error           string             `json:"error"`
	DurationSeconds float64            `json:"duration_seconds"`
	NumTurns        int                `json:"num_turns"`
	Usage           wireUsage          `json:"usage"`
	DeniedActions   []wireDeniedAction `json:"denied_actions"`
}

type wireResultEnvelope struct {
	Event  string             `json:"event"`
	Result *wireResultPayload `json:"result"`
}

// DecodeEvent strictly decodes one stream-json line into an Event.
// Unknown "event" values, unknown "step_type" values, a "state" outside
// {ACTIVE, DONE}, a "status" outside {SUCCESS, ERROR}, and any
// unrecognized field anywhere in the envelope are all ErrProtocolDrift.
// A line that is not valid JSON is ErrMalformedEvent. A line over 1 MiB
// is ErrLineTooLong, checked before any parsing.
func DecodeEvent(line []byte) (Event, error) {
	if len(line) > maxEventLineBytes {
		return Event{}, fmt.Errorf("%w: got %d bytes", ErrLineTooLong, len(line))
	}

	var probe wireProbe
	if err := strictDecode(line, &probe, false); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrMalformedEvent, err)
	}

	switch probe.Event {
	case string(EventKindInit):
		return decodeInitEvent(line)
	case string(EventKindStepUpdate):
		return decodeStepEvent(line)
	case string(EventKindResult):
		return decodeResultEvent(line)
	default:
		return Event{}, &ErrProtocolDrift{Event: probe.Event, Reason: fmt.Sprintf("unrecognized stream event %q", probe.Event)}
	}
}

// strictDecode decodes exactly one JSON value from line into into. When
// strict is true it disallows unknown fields on the typed target;
// either way it rejects trailing content after the single value.
func strictDecode(line []byte, into any, strict bool) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(into); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing content after json value")
	}
	return nil
}

func decodeInitEvent(line []byte) (Event, error) {
	var env wireInitEnvelope
	if err := strictDecode(line, &env, true); err != nil {
		return Event{}, &ErrProtocolDrift{Event: "init", Reason: err.Error()}
	}
	if env.Init == nil {
		return Event{}, &ErrProtocolDrift{Event: "init", Reason: `missing "init" object`}
	}
	return Event{
		Kind:           EventKindInit,
		ConversationID: env.ConversationID,
		Init: &InitEvent{
			Model:          env.Init.Model,
			CWD:            env.Init.CWD,
			Tools:          env.Init.Tools,
			PermissionMode: env.Init.PermissionMode,
		},
	}, nil
}

func validStepType(t string) bool {
	switch StepType(t) {
	case StepTypeUserInput, StepTypeAgentResponse, StepTypeSystemMessage, StepTypeTool, StepTypeErrorMessage:
		return true
	default:
		return false
	}
}

func decodeStepEvent(line []byte) (Event, error) {
	var env wireStepEnvelope
	if err := strictDecode(line, &env, true); err != nil {
		return Event{}, &ErrProtocolDrift{Event: "step_update", Reason: err.Error()}
	}
	if env.StepUpdate == nil {
		return Event{}, &ErrProtocolDrift{Event: "step_update", Reason: `missing "step_update" object`}
	}
	su := env.StepUpdate
	if su.State != "ACTIVE" && su.State != "DONE" {
		return Event{}, &ErrProtocolDrift{Event: "step_update", Reason: fmt.Sprintf("unrecognized state %q", su.State)}
	}
	if !validStepType(su.StepType) {
		return Event{}, &ErrProtocolDrift{Event: "step_update", Reason: fmt.Sprintf("unrecognized step_type %q", su.StepType)}
	}
	var usage *Usage
	if su.Usage != nil {
		u := su.Usage.toUsage()
		usage = &u
	}
	return Event{
		Kind:           EventKindStepUpdate,
		ConversationID: su.ConversationID,
		Step: &StepUpdate{
			Index:     su.StepIndex,
			State:     su.State,
			Type:      StepType(su.StepType),
			TextDelta: su.TextDelta,
			ToolName:  su.ToolName,
			ToolInfo:  su.ToolInfo,
			Duration:  su.DurationSeconds,
			Usage:     usage,
		},
	}, nil
}

func decodeResultEvent(line []byte) (Event, error) {
	var env wireResultEnvelope
	if err := strictDecode(line, &env, true); err != nil {
		return Event{}, &ErrProtocolDrift{Event: "result", Reason: err.Error()}
	}
	if env.Result == nil {
		return Event{}, &ErrProtocolDrift{Event: "result", Reason: `missing "result" object`}
	}
	r := env.Result
	if r.Status != "SUCCESS" && r.Status != "ERROR" {
		return Event{}, &ErrProtocolDrift{Event: "result", Reason: fmt.Sprintf("unrecognized status %q", r.Status)}
	}
	var denied []DeniedAction
	for _, d := range r.DeniedActions {
		denied = append(denied, DeniedAction{Action: d.Action, DisplayName: d.DisplayName})
	}
	return Event{
		Kind:           EventKindResult,
		ConversationID: r.ConversationID,
		Result: &ResultEvent{
			Status:        r.Status,
			Response:      r.Response,
			Error:         r.Error,
			Duration:      r.DurationSeconds,
			NumTurns:      r.NumTurns,
			Usage:         r.Usage.toUsage(),
			DeniedActions: denied,
		},
	}, nil
}

// wireUserEnvelope is the accepted user-message input shape (AC-010
// research §7.1, live-verified): {"event":"user","message":{"content":
// "<text>"}}.
type wireUserEnvelope struct {
	Event   string `json:"event"`
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

// EncodeUserMessage encodes text as one agy stream-json input line
// (including its trailing newline, ready to write directly to the
// child's stdin). NUL bytes and newlines inside text cannot break out of
// the JSON string encoding, so the only rejected input is empty text.
func EncodeUserMessage(text string) ([]byte, error) {
	if text == "" {
		return nil, ErrEmptyUserMessage
	}
	var env wireUserEnvelope
	env.Event = "user"
	env.Message.Content = text
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encode agy user message: %w", err)
	}
	return append(raw, '\n'), nil
}

// Marker is a recognized agy stderr line classification.
type Marker int

const (
	// MarkerNone is any stderr line that carries neither recognized
	// marker.
	MarkerNone Marker = iota
	// MarkerPrintTimeout marks the "[agy] print timeout" line: the
	// --print-timeout backstop fired with the turn still in progress
	// (AC-010 research §7.2 — exit 0, result.status ERROR).
	MarkerPrintTimeout
	// MarkerIgnoredInput marks the "ignoring unsupported stream input
	// message event" warning: an unrecognized input "event" value was
	// silently dropped, never dispatched as a turn (AC-010 research
	// §0 hazard 4).
	MarkerIgnoredInput
)

const (
	printTimeoutMarkerText = "[agy] print timeout"
	ignoredInputMarkerText = "ignoring unsupported stream input message event"
)

// StderrMarker classifies one agy stderr line. Matching is substring
// containment (not full-line equality): the exact surrounding text
// (timing, the dropped event name) is not part of the frozen contract,
// only these two marker substrings are.
func StderrMarker(line string) Marker {
	switch {
	case strings.Contains(line, printTimeoutMarkerText):
		return MarkerPrintTimeout
	case strings.Contains(line, ignoredInputMarkerText):
		return MarkerIgnoredInput
	default:
		return MarkerNone
	}
}

// boundedBuffer keeps the most recent maxLen bytes written to it. Used
// to bound the agy child's stderr tail so a decode/sink failure can
// report recent diagnostics without unbounded memory (mirrors the
// codex/opencode adapters' identical helper).
type boundedBuffer struct {
	mu     sync.Mutex
	buf    []byte
	maxLen int
}

func newBoundedBuffer(maxLen int) *boundedBuffer {
	return &boundedBuffer{maxLen: maxLen}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.maxLen {
		b.buf = b.buf[len(b.buf)-b.maxLen:]
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// stderrTailBytes is the amount of trailing agy stderr kept for
// diagnostics (64 KiB, per the brief).
const stderrTailBytes = 64 << 10

// newStderrTail constructs the bounded stderr buffer ReadEvents accepts.
// Exported via the unexported boundedBuffer type: callers in this
// package (and Task 5's server, once it exists here) drain the child's
// Stderr() pipe into it concurrently with ReadEvents draining Stdout();
// ReadEvents itself never touches the child's stderr pipe.
func newStderrTail() *boundedBuffer {
	return newBoundedBuffer(stderrTailBytes)
}

// ReadEvents scans r (the agy child's stdout) line by line, decoding
// each with DecodeEvent and handing it to sink, stopping at the first
// decode error or the first error sink returns. Blank lines are
// skipped. stderrTail, when non-nil, is expected to be concurrently
// filled by the caller from the child's stderr pipe (ReadEvents does not
// read stderr itself); its current tail is attached to any returned
// error for diagnostics.
func ReadEvents(r io.Reader, sink func(Event) error, stderrTail *boundedBuffer) error {
	scanner := bufio.NewScanner(r)
	// The scan buffer must exceed maxEventLineBytes so an oversized line
	// surfaces DecodeEvent's typed ErrLineTooLong instead of bufio's own
	// (untyped) ErrTooLong.
	scanner.Buffer(make([]byte, 64<<10), maxEventLineBytes+4096)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		ev, err := DecodeEvent(line)
		if err != nil {
			return withStderrTail(err, stderrTail)
		}
		if err := sink(ev); err != nil {
			return withStderrTail(err, stderrTail)
		}
	}
	if err := scanner.Err(); err != nil {
		return withStderrTail(fmt.Errorf("read agy stream: %w", err), stderrTail)
	}
	return nil
}

func withStderrTail(err error, stderrTail *boundedBuffer) error {
	if stderrTail == nil {
		return err
	}
	tail := stderrTail.String()
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w (agy stderr tail: %s)", err, tail)
}
