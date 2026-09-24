// Package agytest is the test-only construction mode for the agy
// adapter (AC-010, mirroring internal/adapter/codex/codextest exactly):
// it carries a byte-identical copy of the compiled fixture `agy`
// executable's source (internal/adapter/agy/agyfake_test.go cannot be
// imported from here—it is a _test.go file, and even if it were not,
// the codex precedent forks this into a sibling package on purpose) and
// builds it once per test binary, then exposes the digest and, on
// Linux, a real execpolicy.SealedImage built from it—so external
// consumers (Task 5's fixture-scoped adapter construction, and any
// operator-invoked manual evidence executable) exercise the exact same
// Task 3 sealed-launch path this package's own protocol tests do.
//
// It is usable ONLY from tests and the operator-invoked manual evidence
// executable—never registered as a production adapter, and
// production packages must not import it (guard_test.go enforces this,
// mirroring codextest/guard_test.go).
package agytest

// The fixture `agy` executable below is the harness copy of the scripted
// stream-json stub in internal/adapter/agy/agyfake_test.go (that file's
// agyFixtureSource constant). The two copies are held byte-identical by
// TestAgyTest_FixtureSourceByteIdenticalToFakeChild (guard_test.go): the
// child silently drops unrecognized directives (json.Unmarshal into a
// struct plus a switch with no default), so one-sided drift would turn
// a staged scenario into a silent no-op on this side while the agy
// package's own suite kept passing.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const fixtureSource = `package main

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
	PluginListCanonical bool              ` + "`" + `json:"plugin_list_canonical"` + "`" + `
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
			continue
		}
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

func runStreamJSON(args []string, st *scenarioState) {
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
		fmt.Fprintln(os.Stderr, ` + "`" + `error: stream input "user" message is missing the "event" field` + "`" + `)
		os.Exit(1)
	}
	var eventName string
	if err := json.Unmarshal(eventRaw, &eventName); err != nil {
		fmt.Fprintln(os.Stderr, ` + "`" + `error: stream input "user" message is missing the "event" field` + "`" + `)
		os.Exit(1)
	}
	if eventName != "user" {
		fmt.Fprintf(os.Stderr, "warning: ignoring unsupported stream input message event %q\n", eventName)
		return
	}
	messageRaw, hasMessage := rawFields["message"]
	if !hasMessage {
		emitResult(buildResultObj(convID, "ERROR", "", ` + "`" + `stream input "user" message is missing the "message" field` + "`" + `, 0, 0, nil, nil))
		os.Exit(1)
	}
	var msg struct {
		Content string ` + "`" + `json:"content"` + "`" + `
	}
	_ = json.Unmarshal(messageRaw, &msg)
	if msg.Content == "" {
		emitResult(buildResultObj(convID, "ERROR", "", ` + "`" + `stream input "user" message has no content` + "`" + `, 0, 0, nil, nil))
		os.Exit(1)
	}

	if st.interruptOnSigint {
		emitJSON(map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": convID, "step_index": 0, "state": "ACTIVE", "step_type": "agent_response"}})
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT)
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
	runStreamJSON(args, st)
}
`

var (
	fixtureOnce   sync.Once
	fixtureBinDir string
	fixtureErr    error
)

// compileFixtureExecutable builds the fake `agy` binary once per process
// and returns the directory holding it.
func compileFixtureExecutable() (string, error) {
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ac010-agytest-fixture-*")
		if err != nil {
			fixtureErr = err
			return
		}
		srcDir := filepath.Join(dir, "src")
		if err := os.MkdirAll(srcDir, 0o700); err != nil {
			fixtureErr = err
			return
		}
		src := filepath.Join(srcDir, "main.go")
		if err := os.WriteFile(src, []byte(fixtureSource), 0o600); err != nil {
			fixtureErr = err
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "agy"), src)
		build.Dir = srcDir
		if out, err := build.CombinedOutput(); err != nil {
			fixtureErr = fmt.Errorf("build agy fixture: %v: %s", err, out)
			return
		}
		fixtureBinDir = dir
	})
	return fixtureBinDir, fixtureErr
}

// Fixture is the compiled fixture `agy` binary plus, on Linux, the
// execpolicy.SealedImage built from it. SealedImage is nil on platforms
// where NewSealedImage is unsupported (execpolicy.ErrSealedLaunchUnsupported);
// LaunchRequest reflects that automatically.
type Fixture struct {
	// BinaryPath is the absolute path to the compiled fixture binary.
	BinaryPath string
	// Digest is the sha256 content digest ("sha256:<64 lowercase hex>")
	// of the compiled fixture binary.
	Digest string
	// SealedImage is the memfd-sealed, ptrace-verified image built from
	// BinaryPath (Linux only; nil elsewhere).
	SealedImage *execpolicy.SealedImage
}

// NewFixture builds (once per process) the fixture `agy` binary, computes
// its digest, and, on Linux, builds a SealedImage from it. Off Linux
// (where NewSealedImage returns execpolicy.ErrSealedLaunchUnsupported),
// SealedImage stays nil and LaunchRequest instead authorizes the launch
// through the explicit FixtureLaunch marker—the one bypass this
// package (and only this package) is entitled to use.
func NewFixture() (*Fixture, error) {
	binDir, err := compileFixtureExecutable()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(binDir, "agy")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fixture binary: %w", err)
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	f := &Fixture{BinaryPath: path, Digest: digest}

	sealed, sealErr := execpolicy.NewSealedImage(path, digest)
	switch {
	case sealErr == nil:
		f.SealedImage = sealed
	case errors.Is(sealErr, execpolicy.ErrSealedLaunchUnsupported):
		// Expected off Linux: LaunchRequest falls back to FixtureLaunch.
	default:
		return nil, fmt.Errorf("build sealed image: %w", sealErr)
	}
	return f, nil
}

// Close releases the SealedImage's kernel resources. Safe to call on a
// nil *Fixture or when SealedImage is nil (off Linux).
func (f *Fixture) Close() error {
	if f == nil || f.SealedImage == nil {
		return nil
	}
	return f.SealedImage.Close()
}

// LaunchRequest builds an execpolicy.LaunchRequest that launches this
// fixture: on Linux, through SealedImage (Command == SealedImage.ArgV0,
// so Task 3's sealed, ptrace-verified path is exercised); off Linux,
// through an ordinary path launch of BinaryPath, authorized by the
// explicit FixtureLaunch marker. profile.Tooling must list "agy" (or
// its caller-supplied override) for the launch to pass the command
// allowlist check; callers needing a different profile shape can copy
// this and adjust rather than fight the default.
func (f *Fixture) LaunchRequest(sessionID, runID string, args []string, paths workspace.WorkspacePaths, profile storage.CanonicalProfile) execpolicy.LaunchRequest {
	req := execpolicy.LaunchRequest{
		RunID:         runID,
		SessionID:     sessionID,
		Command:       f.BinaryPath,
		Args:          args,
		Paths:         paths,
		Profile:       profile,
		FixtureLaunch: true,
	}
	if f.SealedImage != nil {
		req.SealedImage = f.SealedImage
		req.Command = f.SealedImage.ArgV0
	}
	return req
}

// DefaultProfile is a minimal cprof-v4 profile sufficient to pass
// execpolicy's command-allowlist and algo-version checks for a fixture
// launch: permissive isolation, unrestricted network, tooling limited to
// "agy".
func DefaultProfile() storage.CanonicalProfile {
	return storage.CanonicalProfile{
		AlgoVersion:         "cprof-v4",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"agy"},
	}
}
