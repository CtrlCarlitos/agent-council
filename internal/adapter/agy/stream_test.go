package agy

// Strict stream-json protocol tests (AC-010 research doc
// docs/superpowers/evidence/ac010-agy-installed-interface-research.md
// §7.1-§7.5, verbatim event shapes). Golden lines are copied verbatim
// from the committed evidence captures where a literal capture exists
// (the init event, docs/superpowers/evidence/ac010-agy-init-1.2.9.json);
// the other event kinds have no committed byte-for-byte capture (only
// init and the plugin-list output were captured live), so their golden
// lines reproduce the exact key/nesting shape §7.1 documents with
// illustrative values, cited by doc line number at each test.

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// goldenInitCapture is byte-for-byte docs/superpowers/evidence/
// ac010-agy-init-1.2.9.json (57 tool names verbatim; conversation id and
// workspace path are the capture's own redaction placeholders, not
// decode-time substitutions).
const goldenInitCapture = `{"conversation_id":"<conversation-id>","event":"init","init":{"cwd":"<workspace>","permission_mode":"request-review","tools":["ask_custom_permission","ask_permission","ask_question","browser_click_element","browser_drag_pixel_to_pixel","browser_get_dom","browser_get_network_request","browser_input","browser_list_network_requests","browser_mouse_down","browser_mouse_up","browser_move_mouse","browser_press_key","browser_refresh_page","browser_resize_window","browser_scroll","browser_scroll_dom","browser_select_option","browser_subagent","call_mcp_tool","capture_browser_console_logs","capture_browser_screenshot","click_browser_pixel","command_status","define_subagent","delete_knowledge","execute_browser_javascript","find_by_name","finish","generate_image","grep_search","invoke_subagent","list_browser_pages","list_dir","list_permissions","list_resources","manage_inbox","manage_subagents","manage_task","multi_replace_file_content","notebook_edit","notebook_execution","open_browser_url","read_browser_page","read_resource","read_url_content","replace_file_content","run_command","schedule","search_web","sed_file","send_command_input","send_message","view_file","wait","wait_5_seconds","write_to_file"]}}`

func TestDecodeEvent_InitGoldenCapture(t *testing.T) {
	ev, err := DecodeEvent([]byte(goldenInitCapture))
	if err != nil {
		t.Fatalf("decode golden init capture: %v", err)
	}
	if ev.Kind != EventKindInit {
		t.Fatalf("kind = %q, want init", ev.Kind)
	}
	if ev.ConversationID != "<conversation-id>" {
		t.Fatalf("conversation id = %q", ev.ConversationID)
	}
	if ev.Init == nil {
		t.Fatalf("Init is nil")
	}
	if ev.Init.Model != "" {
		t.Fatalf("model = %q, want empty (not set in the capture)", ev.Init.Model)
	}
	if ev.Init.CWD != "<workspace>" {
		t.Fatalf("cwd = %q", ev.Init.CWD)
	}
	if ev.Init.PermissionMode != "request-review" {
		t.Fatalf("permission_mode = %q", ev.Init.PermissionMode)
	}
	if len(ev.Init.Tools) != 57 {
		t.Fatalf("tools count = %d, want 57", len(ev.Init.Tools))
	}
	if ev.Init.Tools[0] != "ask_custom_permission" || ev.Init.Tools[56] != "write_to_file" {
		t.Fatalf("tools not decoded verbatim: first=%q last=%q", ev.Init.Tools[0], ev.Init.Tools[56])
	}
}

// TestDecodeEvent_InitWithModel reproduces the §7.1 shape (research doc
// line 323) with --model set, which the golden capture above does not
// exercise (that capture predates any --model flag).
func TestDecodeEvent_InitWithModel(t *testing.T) {
	line := `{"event":"init","conversation_id":"11111111-1111-4111-8111-111111111111","init":{"model":"gpt-oss-120b-medium","cwd":"/tmp/ws","tools":["run_command"],"permission_mode":"request-review"}}`
	ev, err := DecodeEvent([]byte(line))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Init.Model != "gpt-oss-120b-medium" {
		t.Fatalf("model = %q", ev.Init.Model)
	}
	if ev.ConversationID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("conversation id = %q", ev.ConversationID)
	}
}

// TestDecodeEvent_StepUpdateAgentResponse reproduces the §7.1 shape
// (research doc line 327-330): agent_response carries text_delta, and
// the DONE update carries duration_seconds plus a per-step usage.
func TestDecodeEvent_StepUpdateAgentResponse(t *testing.T) {
	active := `{"event":"step_update","step_update":{"conversation_id":"c1","step_index":2,"state":"ACTIVE","step_type":"agent_response","text_delta":"OK"}}`
	ev, err := DecodeEvent([]byte(active))
	if err != nil {
		t.Fatalf("decode ACTIVE: %v", err)
	}
	if ev.Kind != EventKindStepUpdate || ev.ConversationID != "c1" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.Step.State != "ACTIVE" || ev.Step.Type != StepTypeAgentResponse || ev.Step.TextDelta != "OK" || ev.Step.Index != 2 {
		t.Fatalf("unexpected step: %+v", ev.Step)
	}

	done := `{"event":"step_update","step_update":{"conversation_id":"c1","step_index":2,"state":"DONE","step_type":"agent_response","text_delta":"OK\n","duration_seconds":0.842,"usage":{"input_tokens":10,"output_tokens":2,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":12}}}`
	ev, err = DecodeEvent([]byte(done))
	if err != nil {
		t.Fatalf("decode DONE: %v", err)
	}
	if ev.Step.State != "DONE" || ev.Step.Duration != 0.842 {
		t.Fatalf("unexpected done step: %+v", ev.Step)
	}
	if ev.Step.Usage == nil || ev.Step.Usage.TotalTokens != 12 {
		t.Fatalf("unexpected step usage: %+v", ev.Step.Usage)
	}
}

// TestDecodeEvent_StepUpdateTool reproduces the §7.1 tool step shape
// (research doc line 328-330): tool_name plus tool_info, e.g.
// {"CommandLine":"echo AC010-PROBE-3"} — the exact example bytes cited.
func TestDecodeEvent_StepUpdateTool(t *testing.T) {
	line := `{"event":"step_update","step_update":{"conversation_id":"c1","step_index":1,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"CommandLine":"echo AC010-PROBE-3"},"duration_seconds":0.1}}`
	ev, err := DecodeEvent([]byte(line))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Step.Type != StepTypeTool || ev.Step.ToolName != "run_command" {
		t.Fatalf("unexpected step: %+v", ev.Step)
	}
	var info struct {
		CommandLine string `json:"CommandLine"`
	}
	if err := json.Unmarshal(ev.Step.ToolInfo, &info); err != nil {
		t.Fatalf("tool_info not valid json: %v", err)
	}
	if info.CommandLine != "echo AC010-PROBE-3" {
		t.Fatalf("tool_info.CommandLine = %q", info.CommandLine)
	}
}

// TestDecodeEvent_ResultSuccess reproduces the §7.1 terminal result
// shape (research doc line 334-337).
func TestDecodeEvent_ResultSuccess(t *testing.T) {
	line := `{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"OK\n","error":"","duration_seconds":1.2,"num_turns":1,"usage":{"input_tokens":23548,"output_tokens":4,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":23552}}}`
	ev, err := DecodeEvent([]byte(line))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Kind != EventKindResult || ev.ConversationID != "c1" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	r := ev.Result
	if r.Status != "SUCCESS" || r.Response != "OK\n" || r.NumTurns != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if r.Usage.InputTokens != 23548 || r.Usage.TotalTokens != 23552 {
		t.Fatalf("unexpected usage: %+v", r.Usage)
	}
	if len(r.DeniedActions) != 0 {
		t.Fatalf("unexpected denied actions: %+v", r.DeniedActions)
	}
}

// TestDecodeEvent_ResultRejectedInput reproduces the §7.1 rejected-input
// result (research doc line 338-339, exact error text): a malformed
// envelope still yields a terminal result, never a silent no-op.
func TestDecodeEvent_ResultRejectedInput(t *testing.T) {
	line := `{"event":"result","result":{"conversation_id":"","status":"ERROR","response":"","error":"stream input \"user\" message is missing the \"message\" field","duration_seconds":0,"num_turns":0,"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0}}}`
	ev, err := DecodeEvent([]byte(line))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r := ev.Result
	if r.Status != "ERROR" || r.NumTurns != 0 {
		t.Fatalf("unexpected result: %+v", r)
	}
	const wantErr = `stream input "user" message is missing the "message" field`
	if r.Error != wantErr {
		t.Fatalf("error = %q, want %q", r.Error, wantErr)
	}
}

// TestDecodeEvent_ResultDeniedActions reproduces the §7.3 headless
// denial result shape (research doc line 366): SUCCESS with an empty
// response and a structured denied_actions entry, exit 0.
func TestDecodeEvent_ResultDeniedActions(t *testing.T) {
	line := `{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"","error":"","duration_seconds":0.5,"num_turns":1,"usage":{"input_tokens":1,"output_tokens":1,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":2},"denied_actions":[{"action":"command","display_name":"RunCommand"}]}}`
	ev, err := DecodeEvent([]byte(line))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r := ev.Result
	if r.Status != "SUCCESS" || r.Response != "" {
		t.Fatalf("unexpected result: %+v", r)
	}
	if len(r.DeniedActions) != 1 || r.DeniedActions[0].Action != "command" || r.DeniedActions[0].DisplayName != "RunCommand" {
		t.Fatalf("unexpected denied actions: %+v", r.DeniedActions)
	}
}

// TestDecodeEvent_ResultInterrupted reproduces the §7.4 cancellation
// result shape (research doc line 381-386).
func TestDecodeEvent_ResultInterrupted(t *testing.T) {
	line := `{"event":"result","result":{"conversation_id":"c1","status":"ERROR","response":"","error":"interrupted","duration_seconds":0.9,"num_turns":1,"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0}}}`
	ev, err := DecodeEvent([]byte(line))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Result.Status != "ERROR" || ev.Result.Error != "interrupted" {
		t.Fatalf("unexpected result: %+v", ev.Result)
	}
}

// ── drift matrix ─────────────────────────────────────────────────────

func TestDecodeEvent_DriftMatrix(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"unknown_event", `{"event":"bogus","bogus":{}}`},
		{"unknown_step_type", `{"event":"step_update","step_update":{"conversation_id":"c1","step_index":0,"state":"ACTIVE","step_type":"telekinesis"}}`},
		{"state_outside_enum", `{"event":"step_update","step_update":{"conversation_id":"c1","step_index":0,"state":"PENDING","step_type":"tool"}}`},
		{"status_outside_enum", `{"event":"result","result":{"conversation_id":"c1","status":"PARTIAL","response":"","error":"","duration_seconds":0,"num_turns":0,"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0}}}`},
		{"unknown_field_top_level_init", `{"event":"init","conversation_id":"c1","init":{"cwd":"/tmp","tools":[],"permission_mode":"request-review"},"extra":true}`},
		{"unknown_field_inside_init", `{"event":"init","conversation_id":"c1","init":{"cwd":"/tmp","tools":[],"permission_mode":"request-review","surprise":1}}`},
		{"unknown_field_inside_step_update", `{"event":"step_update","step_update":{"conversation_id":"c1","step_index":0,"state":"ACTIVE","step_type":"tool","surprise":1}}`},
		{"unknown_field_inside_result", `{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"","error":"","duration_seconds":0,"num_turns":0,"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0},"surprise":1}}`},
		{"missing_init_object", `{"event":"init","conversation_id":"c1"}`},
		{"missing_step_update_object", `{"event":"step_update"}`},
		{"missing_result_object", `{"event":"result"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeEvent([]byte(tc.line))
			var drift *ErrProtocolDrift
			if !errors.As(err, &drift) {
				t.Fatalf("expected *ErrProtocolDrift, got %T: %v", err, err)
			}
		})
	}
}

func TestDecodeEvent_MalformedJSONDistinctFromDrift(t *testing.T) {
	cases := []string{
		`{"event":`,
		`not json at all`,
		``,
	}
	for _, line := range cases {
		_, err := DecodeEvent([]byte(line))
		if !errors.Is(err, ErrMalformedEvent) {
			t.Fatalf("line %q: expected ErrMalformedEvent, got %v", line, err)
		}
		var drift *ErrProtocolDrift
		if errors.As(err, &drift) {
			t.Fatalf("line %q: malformed JSON must not classify as drift", line)
		}
	}
}

func TestDecodeEvent_LineTooLong(t *testing.T) {
	huge := `{"event":"init","conversation_id":"c1","init":{"cwd":"` + strings.Repeat("x", maxEventLineBytes) + `","tools":[],"permission_mode":"request-review"}}`
	_, err := DecodeEvent([]byte(huge))
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
}

func TestDecodeEvent_LineAtBoundIsNotTooLong(t *testing.T) {
	// A line exactly at the bound must not trip ErrLineTooLong (it may
	// of course still fail to parse/validate for other reasons — here
	// it is padded with cwd filler so it parses cleanly).
	base := `{"event":"init","conversation_id":"c1","init":{"cwd":"","tools":[],"permission_mode":"request-review"}}`
	pad := maxEventLineBytes - len(base)
	line := `{"event":"init","conversation_id":"c1","init":{"cwd":"` + strings.Repeat("x", pad) + `","tools":[],"permission_mode":"request-review"}}`
	if len(line) != maxEventLineBytes {
		t.Fatalf("test construction bug: line is %d bytes, want exactly %d", len(line), maxEventLineBytes)
	}
	_, err := DecodeEvent([]byte(line))
	if errors.Is(err, ErrLineTooLong) {
		t.Fatalf("line at exactly the bound must not be ErrLineTooLong")
	}
}

// ── stderr markers ───────────────────────────────────────────────────

func TestStderrMarker(t *testing.T) {
	cases := []struct {
		line string
		want Marker
	}{
		{`[agy] print timeout after 30s with turn in progress; returning partial output`, MarkerPrintTimeout},
		{`warning: ignoring unsupported stream input message event "bogus"`, MarkerIgnoredInput},
		{`warning: conversation "00000000-0000-0000-0000-000000000000" not found`, MarkerNone},
		{`some unrelated stderr noise`, MarkerNone},
		{``, MarkerNone},
	}
	for _, tc := range cases {
		if got := StderrMarker(tc.line); got != tc.want {
			t.Errorf("StderrMarker(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// ── user message encoding ────────────────────────────────────────────

func TestEncodeUserMessage(t *testing.T) {
	raw, err := EncodeUserMessage("hello")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("encoded message must end with a newline: %q", raw)
	}
	var env wireUserEnvelope
	if err := json.Unmarshal(bytes.TrimRight(raw, "\n"), &env); err != nil {
		t.Fatalf("encoded message is not valid json: %v", err)
	}
	if env.Event != "user" || env.Message.Content != "hello" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
}

func TestEncodeUserMessage_EmptyRejected(t *testing.T) {
	_, err := EncodeUserMessage("")
	if !errors.Is(err, ErrEmptyUserMessage) {
		t.Fatalf("expected ErrEmptyUserMessage, got %v", err)
	}
}

// TestEncodeUserMessage_NULAndNewlineAreEscapedNotRejected documents the
// brief's resolution: JSON string encoding escapes NUL and newline, so
// neither can break out of the envelope — EncodeUserMessage does not
// specially reject them, it just round-trips them faithfully.
func TestEncodeUserMessage_NULAndNewlineAreEscapedNotRejected(t *testing.T) {
	text := "line one\nline two\x00tail"
	raw, err := EncodeUserMessage(text)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// The envelope itself must still be exactly one line (no raw
	// newline byte outside of the trailing one EncodeUserMessage adds).
	if strings.Count(string(raw), "\n") != 1 {
		t.Fatalf("encoded envelope must be a single NDJSON line, got: %q", raw)
	}
	var env wireUserEnvelope
	if err := json.Unmarshal(bytes.TrimRight(raw, "\n"), &env); err != nil {
		t.Fatalf("encoded message is not valid json: %v", err)
	}
	if env.Message.Content != text {
		t.Fatalf("content round-trip = %q, want %q", env.Message.Content, text)
	}
}

// ── ReadEvents ────────────────────────────────────────────────────────

func TestReadEvents_StopsAtFirstSinkError(t *testing.T) {
	stream := strings.Join([]string{
		`{"event":"init","conversation_id":"c1","init":{"cwd":"/tmp","tools":[],"permission_mode":"request-review"}}`,
		`{"event":"step_update","step_update":{"conversation_id":"c1","step_index":0,"state":"ACTIVE","step_type":"agent_response","text_delta":"a"}}`,
		`{"event":"step_update","step_update":{"conversation_id":"c1","step_index":0,"state":"DONE","step_type":"agent_response","text_delta":"ab"}}`,
	}, "\n") + "\n"

	var got []EventKind
	sinkErr := errors.New("sink stop")
	err := ReadEvents(strings.NewReader(stream), func(ev Event) error {
		got = append(got, ev.Kind)
		if len(got) == 2 {
			return sinkErr
		}
		return nil
	}, nil)
	if !errors.Is(err, sinkErr) {
		t.Fatalf("expected sink error, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 events delivered before stop, got %d: %v", len(got), got)
	}
}

func TestReadEvents_BlankLinesSkipped(t *testing.T) {
	stream := "\n" + `{"event":"init","conversation_id":"c1","init":{"cwd":"/tmp","tools":[],"permission_mode":"request-review"}}` + "\n\n"
	var got int
	err := ReadEvents(strings.NewReader(stream), func(ev Event) error {
		got++
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if got != 1 {
		t.Fatalf("expected 1 event, got %d", got)
	}
}

func TestReadEvents_DecodeErrorIncludesStderrTail(t *testing.T) {
	tail := newBoundedBuffer(64 << 10)
	_, _ = tail.Write([]byte("some diagnostic stderr line\n"))

	err := ReadEvents(strings.NewReader(`{"event":"bogus"}`+"\n"), func(Event) error { return nil }, tail)
	if err == nil {
		t.Fatalf("expected error")
	}
	var drift *ErrProtocolDrift
	if !errors.As(err, &drift) {
		t.Fatalf("expected *ErrProtocolDrift, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "some diagnostic stderr line") {
		t.Fatalf("expected stderr tail attached to error, got: %v", err)
	}
}

func TestReadEvents_LineTooLongSurfacesTypedError(t *testing.T) {
	huge := `{"event":"init","conversation_id":"c1","init":{"cwd":"` + strings.Repeat("x", maxEventLineBytes) + `","tools":[],"permission_mode":"request-review"}}` + "\n"
	err := ReadEvents(strings.NewReader(huge), func(Event) error { return nil }, nil)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
}

func TestBoundedBuffer_KeepsOnlyTail(t *testing.T) {
	b := newBoundedBuffer(8)
	_, _ = b.Write([]byte("0123456789"))
	if got := b.String(); got != "23456789" {
		t.Fatalf("bounded buffer = %q, want last 8 bytes", got)
	}
}

// sanity: maxEventLineBytes is exactly 1 MiB, and stderrTailBytes is
// exactly 64 KiB, matching the brief's exact bounds.
func TestBounds(t *testing.T) {
	if maxEventLineBytes != 1<<20 {
		t.Fatalf("maxEventLineBytes = %d, want 1 MiB", maxEventLineBytes)
	}
	if stderrTailBytes != 64<<10 {
		t.Fatalf("stderrTailBytes = %d, want 64 KiB", stderrTailBytes)
	}
}
