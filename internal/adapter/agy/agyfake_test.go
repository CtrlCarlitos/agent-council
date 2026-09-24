//go:build unix

package agy

// The fixture `agy` executable: a compiled stub speaking the agy
// stream-json protocol over stdio (one JSON object per line), exec'd
// through the real PolicyExecutor exactly like the native binary would
// be (on Linux, via a sealed-image launch built from this exact binary
//—Task 3's memfd-sealed, ptrace-verified launch path; off Linux, via
// an ordinary path launch gated by the explicit FixtureLaunch marker,
// since NewSealedImage is Linux-only).
//
// It reads .agy-fixture-scenario.jsonl from its cwd, logs argv to
// .agy-fixture-args (fields joined by U+001F), and — since the real agy
// process-per-turn protocol sends exactly one stdin line per invocation —
// reads and logs exactly that one stdin line to .agy-fixture-input before
// validating it and replaying the scripted behavior described by the
// directives below (AC-010 research doc
// docs/superpowers/evidence/ac010-agy-installed-interface-research.md
// §7.1-§7.5). This file is built with //go:build unix, exactly like
// codex/codexfake_test.go, because the generated fixture program relies
// on a Unix signal (SIGINT) discipline; on windows this whole file --
// helpers and tests alike—is excluded from the build (the GOOS=windows
// go vet run never attempts to type-check it).
//
// Built-in behavior (no scenario needed):
//   - init is always emitted before any stdin line is read; conversation
//     id resolution follows the live-verified 1.2.9 rule: a known
//     --conversation id is echoed back, an absent/unknown one silently
//     falls back to a NEW id with a stderr warning (never an error), and
//     with no --conversation flag a fresh id is generated (or the pinned
//     conversation_id directive's value, for determinism).
//   - --version prints the version directive (default "1.2.9").
//   - `models` prints the models_catalog directive (a small default
//     catalog) or, under models_not_signed_in, the not-signed-in text.
//   - `plugin list` prints the plugin_list_drift directive's raw text, or
//     else the canonical committed plugins JSON verbatim
//     (docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json).
//   - the one stdin line is validated exactly like 1.2.9 (research §3
//     line 213, §7.1 lines 337-344, evidence verbatim): a decode failure
//     is a stderr error and exit 1; a missing "event" field is the exact
//     stderr error text `error: stream input message is missing the
//     "event" field` (no "user" qualifier), exit 1; an "event" other than
//     "user" is the exact "ignoring unsupported..." warning (exit 0, no
//     result); for "event":"user", a missing "message" field, or a
//     message.content that is not a non-empty JSON string (numeric,
//     empty string, array, object, null), yields a terminal ERROR result
//     with error text `stream input "user" message is missing the
//     "message" field` and exit 1 — rejected input still yields a
//     result, never a silent no-op.
//
// Scenario directives (JSONL, one key set per line):
//   {"known_conversation": "<id>"}       a --conversation value this
//                                        process will echo back verbatim
//   {"conversation_id": "<id>"}          pins the FRESH id used when no
//                                        --conversation is given, or
//                                        when a given one is unknown
//   {"permission_mode": "<mode>"}        overrides "request-review"
//   {"tools": [...]}                     overrides the 57-name default
//   {"slow_init_ms": N}                  sleep N ms before emitting init
//   {"step": {...}}                      emit one step_update (state,
//                                        step_type, text_delta,
//                                        tool_name, tool_info,
//                                        duration_seconds, usage);
//                                        conversation_id/step_index are
//                                        auto-filled, in directive order
//   {"result": {...}}                    override the terminal result
//                                        entirely (status, response,
//                                        error, duration_seconds,
//                                        num_turns, usage,
//                                        denied_actions)
//   {"denied_actions": [...]}            shorthand: SUCCESS, response
//                                        "", these denied_actions
//   {"interrupt_on_sigint": true}        emit one ACTIVE agent_response
//                                        step, then wait for SIGINT and
//                                        emit result{ERROR,"interrupted"},
//                                        exit 1 (§7.4)
//   {"print_timeout_marker": true}       emit the "[agy] print timeout"
//                                        stderr marker, then an ERROR
//                                        result, exit 0 (§7.2)
//   {"exit_without_result": true}        exit 0 without ever emitting a
//                                        terminal result
//   {"models_catalog": [...]}            `agy models` catalog override
//   {"models_not_signed_in": true}       `agy models` not-signed-in text
//   {"plugin_list_drift": "<raw text>"}  `agy plugin list` drifted output
//   {"version": "<string>"}              --version output
//
// Evidence knobs (resolved in the child's CWD):
//   .agy-fixture-args    argv log (fields joined by U+001F), one line
//                        per invocation
//   .agy-fixture-input   the single stdin line read, verbatim

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const agyFixtureSource = `package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var defaultTools = []string{"ask_custom_permission", "ask_permission", "ask_question", "browser_click_element", "browser_drag_pixel_to_pixel", "browser_get_dom", "browser_get_network_request", "browser_input", "browser_list_network_requests", "browser_mouse_down", "browser_mouse_up", "browser_move_mouse", "browser_press_key", "browser_refresh_page", "browser_resize_window", "browser_scroll", "browser_scroll_dom", "browser_select_option", "browser_subagent", "call_mcp_tool", "capture_browser_console_logs", "capture_browser_screenshot", "click_browser_pixel", "command_status", "define_subagent", "delete_knowledge", "execute_browser_javascript", "find_by_name", "finish", "generate_image", "grep_search", "invoke_subagent", "list_browser_pages", "list_dir", "list_permissions", "list_resources", "manage_inbox", "manage_subagents", "manage_task", "multi_replace_file_content", "notebook_edit", "notebook_execution", "open_browser_url", "read_browser_page", "read_resource", "read_url_content", "replace_file_content", "run_command", "schedule", "search_web", "sed_file", "send_command_input", "send_message", "view_file", "wait", "wait_5_seconds", "write_to_file"}

const canonicalPluginList = ` + "`" + `{"imports":[{"components":["hooks","skills"],"importedAt":"2026-09-02T19:59:12Z","name":"superpowers","source":"gemini-cli"}]}` + "`" + `

type usageDirective struct {
	InputTokens     int ` + "`" + `json:"input_tokens"` + "`" + `
	OutputTokens    int ` + "`" + `json:"output_tokens"` + "`" + `
	ThinkingTokens  int ` + "`" + `json:"thinking_tokens"` + "`" + `
	CacheReadTokens int ` + "`" + `json:"cache_read_tokens"` + "`" + `
	TotalTokens     int ` + "`" + `json:"total_tokens"` + "`" + `
}

type deniedDirective struct {
	Action      string ` + "`" + `json:"action"` + "`" + `
	DisplayName string ` + "`" + `json:"display_name"` + "`" + `
}

type stepDirective struct {
	State           string          ` + "`" + `json:"state"` + "`" + `
	StepType        string          ` + "`" + `json:"step_type"` + "`" + `
	TextDelta       string          ` + "`" + `json:"text_delta"` + "`" + `
	ToolName        string          ` + "`" + `json:"tool_name"` + "`" + `
	ToolInfo        json.RawMessage ` + "`" + `json:"tool_info"` + "`" + `
	DurationSeconds float64         ` + "`" + `json:"duration_seconds"` + "`" + `
	Usage           *usageDirective ` + "`" + `json:"usage"` + "`" + `
}

type resultDirective struct {
	Status          string            ` + "`" + `json:"status"` + "`" + `
	Response        string            ` + "`" + `json:"response"` + "`" + `
	Error           string            ` + "`" + `json:"error"` + "`" + `
	DurationSeconds float64           ` + "`" + `json:"duration_seconds"` + "`" + `
	NumTurns        int               ` + "`" + `json:"num_turns"` + "`" + `
	Usage           *usageDirective   ` + "`" + `json:"usage"` + "`" + `
	DeniedActions   []deniedDirective ` + "`" + `json:"denied_actions"` + "`" + `
}

type directive struct {
	KnownConversation   string            ` + "`" + `json:"known_conversation"` + "`" + `
	ConversationID      string            ` + "`" + `json:"conversation_id"` + "`" + `
	PermissionMode      string            ` + "`" + `json:"permission_mode"` + "`" + `
	Tools               []string          ` + "`" + `json:"tools"` + "`" + `
	SlowInitMs          int64             ` + "`" + `json:"slow_init_ms"` + "`" + `
	Step                *stepDirective    ` + "`" + `json:"step"` + "`" + `
	Result              *resultDirective  ` + "`" + `json:"result"` + "`" + `
	DeniedActions       []deniedDirective ` + "`" + `json:"denied_actions"` + "`" + `
	InterruptOnSigint   bool              ` + "`" + `json:"interrupt_on_sigint"` + "`" + `
	PrintTimeoutMarker  bool              ` + "`" + `json:"print_timeout_marker"` + "`" + `
	ExitWithoutResult   bool              ` + "`" + `json:"exit_without_result"` + "`" + `
	ModelsCatalog       []string          ` + "`" + `json:"models_catalog"` + "`" + `
	ModelsNotSignedIn   bool              ` + "`" + `json:"models_not_signed_in"` + "`" + `
	PluginListDrift     string            ` + "`" + `json:"plugin_list_drift"` + "`" + `
	Version             string            ` + "`" + `json:"version"` + "`" + `
}

type scenarioState struct {
	knownConversations map[string]bool
	conversationID     string
	permissionMode     string
	tools              []string
	slowInitMs         int64
	steps              []stepDirective
	result             *resultDirective
	deniedActions      []deniedDirective
	interruptOnSigint  bool
	printTimeoutMarker bool
	exitWithoutResult  bool
	modelsCatalog      []string
	modelsNotSignedIn  bool
	pluginListDrift    string
	version            string
}

func appendLine(name, line string) {
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func loadScenario() *scenarioState {
	st := &scenarioState{
		knownConversations: map[string]bool{},
		permissionMode:     "request-review",
		tools:              defaultTools,
		version:            "1.2.9",
	}
	raw, err := os.ReadFile(".agy-fixture-scenario.jsonl")
	if err != nil {
		return st
	}
	for _, ln := range strings.Split(string(raw), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var d directive
		if err := json.Unmarshal([]byte(ln), &d); err != nil {
			fmt.Fprintf(os.Stderr, "error: fixture scenario: %v\n", err)
			os.Exit(2)
		}
		recognized := true
		switch {
		case d.KnownConversation != "":
			st.knownConversations[d.KnownConversation] = true
		case d.ConversationID != "":
			st.conversationID = d.ConversationID
		case d.PermissionMode != "":
			st.permissionMode = d.PermissionMode
		case d.Tools != nil:
			st.tools = d.Tools
		case d.SlowInitMs > 0:
			st.slowInitMs = d.SlowInitMs
		case d.Step != nil:
			st.steps = append(st.steps, *d.Step)
		case d.Result != nil:
			st.result = d.Result
		case len(d.DeniedActions) > 0:
			st.deniedActions = d.DeniedActions
		case d.InterruptOnSigint:
			st.interruptOnSigint = true
		case d.PrintTimeoutMarker:
			st.printTimeoutMarker = true
		case d.ExitWithoutResult:
			st.exitWithoutResult = true
		case len(d.ModelsCatalog) > 0:
			st.modelsCatalog = d.ModelsCatalog
		case d.ModelsNotSignedIn:
			st.modelsNotSignedIn = true
		case d.PluginListDrift != "":
			st.pluginListDrift = d.PluginListDrift
		case d.Version != "":
			st.version = d.Version
		default:
			recognized = false
		}
		if !recognized {
			fmt.Fprintf(os.Stderr, "error: fixture scenario: no recognized directive key in line %q\n", ln)
			os.Exit(2)
		}
	}
	return st
}

func newUUIDv4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func emitJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stdout, string(b))
}

func emitResult(obj map[string]any) {
	emitJSON(map[string]any{"event": "result", "result": obj})
}

func buildResultObj(convID, status, response, errText string, duration float64, numTurns int, usage *usageDirective, denied []deniedDirective) map[string]any {
	u := usage
	if u == nil {
		u = &usageDirective{}
	}
	obj := map[string]any{
		"conversation_id":  convID,
		"status":           status,
		"response":         response,
		"error":            errText,
		"duration_seconds": duration,
		"num_turns":        numTurns,
		"usage": map[string]any{
			"input_tokens":      u.InputTokens,
			"output_tokens":     u.OutputTokens,
			"thinking_tokens":   u.ThinkingTokens,
			"cache_read_tokens": u.CacheReadTokens,
			"total_tokens":      u.TotalTokens,
		},
	}
	if len(denied) > 0 {
		var da []map[string]any
		for _, d := range denied {
			da = append(da, map[string]any{"action": d.Action, "display_name": d.DisplayName})
		}
		obj["denied_actions"] = da
	}
	return obj
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func findConversationArg(args []string) (string, bool) {
	for i, a := range args {
		if a == "--conversation" && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func handleModels(st *scenarioState) {
	if st.modelsNotSignedIn {
		fmt.Println("You are not logged into Antigravity. Run ` + "`" + `agy` + "`" + ` to sign in.")
		return
	}
	catalog := st.modelsCatalog
	if len(catalog) == 0 {
		catalog = []string{"gemini-3.8-flash-low", "gemini-3.8-flash-medium", "gemini-3.1-pro-low", "claude-sonnet-4-6", "claude-opus-4-6-thinking", "gpt-oss-120b-medium"}
	}
	for _, m := range catalog {
		fmt.Println(m)
	}
}

func handlePluginList(st *scenarioState) {
	if st.pluginListDrift != "" {
		fmt.Println(st.pluginListDrift)
		return
	}
	fmt.Println(canonicalPluginList)
}

func runStreamJSON(args []string, st *scenarioState, sigCh chan os.Signal) {
	model := argValue(args, "--model")
	conversationArg, hasConversationArg := findConversationArg(args)

	var convID string
	if hasConversationArg {
		if st.knownConversations[conversationArg] {
			convID = conversationArg
		} else {
			fmt.Fprintf(os.Stderr, "warning: conversation %q not found\n", conversationArg)
			convID = st.conversationID
			if convID == "" {
				convID = newUUIDv4()
			}
		}
	} else {
		convID = st.conversationID
		if convID == "" {
			convID = newUUIDv4()
		}
	}

	if st.slowInitMs > 0 {
		time.Sleep(time.Duration(st.slowInitMs) * time.Millisecond)
	}

	cwd, _ := os.Getwd()
	initObj := map[string]any{"cwd": cwd, "tools": st.tools, "permission_mode": st.permissionMode}
	if model != "" {
		initObj["model"] = model
	}
	emitJSON(map[string]any{"event": "init", "conversation_id": convID, "init": initObj})

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	if !scanner.Scan() {
		return
	}
	line := append([]byte(nil), scanner.Bytes()...)
	appendLine(".agy-fixture-input", string(line))

	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(line, &rawFields); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to decode stream input: %v\n", err)
		os.Exit(1)
	}
	eventRaw, hasEvent := rawFields["event"]
	if !hasEvent {
		fmt.Fprintln(os.Stderr, ` + "`" + `error: stream input message is missing the "event" field` + "`" + `)
		os.Exit(1)
	}
	var eventName string
	if err := json.Unmarshal(eventRaw, &eventName); err != nil {
		fmt.Fprintln(os.Stderr, ` + "`" + `error: stream input message is missing the "event" field` + "`" + `)
		os.Exit(1)
	}
	if eventName != "user" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unsupported stream input message event %q\n", eventName)
		return
	}
	messageRaw, hasMessage := rawFields["message"]
	var msg struct {
		Content string ` + "`" + `json:"content"` + "`" + `
	}
	contentIsNonEmptyString := false
	if hasMessage {
		var rawMsgFields map[string]json.RawMessage
		if err := json.Unmarshal(messageRaw, &rawMsgFields); err == nil {
			if contentRaw, hasContent := rawMsgFields["content"]; hasContent {
				var contentStr string
				if err := json.Unmarshal(contentRaw, &contentStr); err == nil && contentStr != "" {
					msg.Content = contentStr
					contentIsNonEmptyString = true
				}
			}
		}
	}
	if !hasMessage || !contentIsNonEmptyString {
		emitResult(buildResultObj(convID, "ERROR", "", ` + "`" + `stream input "user" message is missing the "message" field` + "`" + `, 0, 0, nil, nil))
		os.Exit(1)
	}

	if st.interruptOnSigint {
		emitJSON(map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": convID, "step_index": 0, "state": "ACTIVE", "step_type": "agent_response"}})
		<-sigCh
		emitResult(buildResultObj(convID, "ERROR", "", "interrupted", 0, 1, &usageDirective{}, nil))
		os.Exit(1)
	}

	if st.printTimeoutMarker {
		fmt.Fprintln(os.Stderr, "[agy] print timeout after 30s with turn in progress; returning partial output")
		emitResult(buildResultObj(convID, "ERROR", "", "", 0, 1, &usageDirective{}, nil))
		os.Exit(0)
	}

	for i, s := range st.steps {
		stepObj := map[string]any{"conversation_id": convID, "step_index": i, "state": s.State, "step_type": s.StepType}
		if s.TextDelta != "" {
			stepObj["text_delta"] = s.TextDelta
		}
		if s.ToolName != "" {
			stepObj["tool_name"] = s.ToolName
		}
		if len(s.ToolInfo) > 0 {
			stepObj["tool_info"] = s.ToolInfo
		}
		if s.DurationSeconds != 0 {
			stepObj["duration_seconds"] = s.DurationSeconds
		}
		if s.Usage != nil {
			stepObj["usage"] = s.Usage
		}
		emitJSON(map[string]any{"event": "step_update", "step_update": stepObj})
	}

	if len(st.deniedActions) > 0 {
		emitResult(buildResultObj(convID, "SUCCESS", "", "", 0, 1, &usageDirective{}, st.deniedActions))
		return
	}
	if st.result != nil {
		emitResult(buildResultObj(convID, st.result.Status, st.result.Response, st.result.Error, st.result.DurationSeconds, st.result.NumTurns, st.result.Usage, st.result.DeniedActions))
		return
	}
	if st.exitWithoutResult {
		return
	}
	emitResult(buildResultObj(convID, "SUCCESS", msg.Content+"\n", "", 0.1, 1, &usageDirective{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}, nil))
}

func main() {
	// Registered before any output (including argv/scenario logging): a
	// SIGINT arriving the instant the driver sees the ACTIVE step_update
	// must never race process startup.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)

	args := os.Args[1:]
	appendLine(".agy-fixture-args", strings.Join(args, "\x1f"))

	st := loadScenario()

	for _, a := range args {
		if a == "--version" {
			fmt.Println(st.version)
			return
		}
	}
	if len(args) > 0 && args[0] == "models" {
		handleModels(st)
		return
	}
	if len(args) > 1 && args[0] == "plugin" && args[1] == "list" {
		handlePluginList(st)
		return
	}
	runStreamJSON(args, st, sigCh)
}
`

var (
	agyFixtureOnce   sync.Once
	agyFixtureBinDir string
	agyFixtureErr    error
)

// compileAgyFixture builds the fake agy binary once per test binary and
// returns the directory holding it (mirrors codex's compileCodexFixture,
// internal/adapter/codex/codexfake_test.go:375).
func compileAgyFixture(t *testing.T) string {
	t.Helper()
	agyFixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ac010-agy-fake-*")
		if err != nil {
			agyFixtureErr = err
			return
		}
		srcDir := filepath.Join(dir, "src")
		if err := os.MkdirAll(srcDir, 0o700); err != nil {
			agyFixtureErr = err
			return
		}
		src := filepath.Join(srcDir, "main.go")
		if err := os.WriteFile(src, []byte(agyFixtureSource), 0o600); err != nil {
			agyFixtureErr = err
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "agy"), src)
		build.Dir = srcDir
		if out, err := build.CombinedOutput(); err != nil {
			agyFixtureErr = fmt.Errorf("build agy fixture: %v: %s", err, out)
			return
		}
		agyFixtureBinDir = dir
	})
	if agyFixtureErr != nil {
		t.Fatalf("agy fixture build: %v", agyFixtureErr)
	}
	return agyFixtureBinDir
}

// writeAgyScenario composes a scenario from raw directive lines.
func writeAgyScenario(t *testing.T, cwd string, lines ...string) {
	t.Helper()
	joined := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(cwd, ".agy-fixture-scenario.jsonl"), []byte(joined), 0o600); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
}

// readAgyFixtureFile reads a child evidence file, returning nil lines
// when it does not exist.
func readAgyFixtureFile(t *testing.T, cwd, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cwd, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", name, err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
