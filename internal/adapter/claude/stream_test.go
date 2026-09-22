package claude

import (
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func toolkitManifestFixture(hooks, skills, plugins []string) storage.ToolkitManifest {
	return storage.ToolkitManifest{
		ProbedCLIVersion: "2.1.278",
		ApprovedTools:    []string{"Read", "Glob", "Grep"},
		DeniedComplement: []string{"Bash", "Write", "WebSearch"},
		ExpectedHooks:    hooks,
		ExpectedSkills:   skills,
		ExpectedPlugins:  plugins,
		TurnsBound:       8,
	}
}

const baseInitJSON = `{"type":"system","subtype":"init","session_id":"` + testNativeID +
	`","cwd":"/ws/claude","claude_code_version":"2.1.278","model":"claude-haiku-4-5-20251001",` +
	`"permissionMode":"default","tools":["Task","Read","Glob","Grep"],` +
	`"skills":["research"],"plugins":[{"name":"superpowers"}],"apiKeySource":"none"}`

const hookStartedJSON = `{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup","session_id":"` + testNativeID + `"}`

// basePrefix is the verified head of every healthy stream: the operator
// hook fires, then init.
const basePrefix = hookStartedJSON + "\n" + baseInitJSON + "\n"

const okResultJSON = `{"type":"result","subtype":"success","is_error":false,` +
	`"session_id":"` + testNativeID + `","result":"done","total_cost_usd":0.01,` +
	`"duration_api_ms":500}`

const maxTurnsResultJSON = `{"type":"result","subtype":"error_max_turns","is_error":true,` +
	`"session_id":"` + testNativeID + `","result":"stopped"}`

func streamConfig(t *testing.T) StreamConfig {
	t.Helper()
	return StreamConfig{
		WorkspaceRoot:         "/ws/claude",
		ExpectedVersion:       "2.1.278",
		ExpectedModelIdentity: "claude-haiku-4-5-20251001",
		ExpectedSessionID:     testNativeID,
		Manifest: toolkitManifestFixture(
			[]string{"SessionStart:startup"},
			[]string{"research"},
			[]string{"superpowers"},
		),
		UniverseTools: []string{"Task", "Read", "Glob", "Grep", "Bash", "Write", "WebSearch"},
		MaxLineBytes:  1 << 20,
		MaxTotalBytes: 8 << 20,
	}
}

// Happy path: init + assistant + result ⇒ terminal completed with the
// result text and session id.
func TestStreamParser_CompletedTurn(t *testing.T) {
	cfg := streamConfig(t)
	stream := basePrefix + `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]},"session_id":"` + testNativeID + `"}` + "\n" + okResultJSON + "\n"

	out, err := ParseStream(strings.NewReader(stream), cfg, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !out.Terminal {
		t.Fatal("stream must reach terminal")
	}
	if out.Poisoned {
		t.Fatalf("healthy stream must not poison: %s", out.PoisonReason)
	}
	if !out.Completed {
		t.Fatalf("success result must complete, got %+v", out)
	}
	if out.ResultText != "done" {
		t.Fatalf("result text, got %q", out.ResultText)
	}
	if out.SessionID != testNativeID {
		t.Fatalf("session id, got %q", out.SessionID)
	}
	if out.Init == nil || out.Init.Version != "2.1.278" || out.Init.Cwd != "/ws/claude" {
		t.Fatalf("init report, got %+v", out.Init)
	}
}

// error_max_turns is a bounded non-completion: terminal + failed.
func TestStreamParser_MaxTurnsIsFailedNotCompleted(t *testing.T) {
	cfg := streamConfig(t)
	stream := basePrefix + maxTurnsResultJSON + "\n"
	out, err := ParseStream(strings.NewReader(stream), cfg, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !out.Terminal || out.Completed {
		t.Fatalf("error_max_turns must be terminal-failed, got %+v", out)
	}
}

// Multiple result events poison the stream.
func TestStreamParser_MultipleResultsPoison(t *testing.T) {
	cfg := streamConfig(t)
	stream := basePrefix + okResultJSON + "\n" + maxTurnsResultJSON + "\n"
	out, err := ParseStream(strings.NewReader(stream), cfg, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !out.Poisoned {
		t.Fatal("duplicate results must poison")
	}
	if !strings.Contains(out.PoisonReason, "multiple result") {
		t.Fatalf("poison reason, got %q", out.PoisonReason)
	}
}

// Malformed NDJSON mid-stream poisons; a torn final line is tolerated.
func TestStreamParser_MalformedLines(t *testing.T) {
	cfg := streamConfig(t)

	t.Run("mid-stream malformed poisons", func(t *testing.T) {
		stream := basePrefix + "{not json}\n" + okResultJSON + "\n"
		out, _ := ParseStream(strings.NewReader(stream), cfg, nil)
		if !out.Poisoned {
			t.Fatal("mid-stream malformed line must poison")
		}
	})

	t.Run("torn final line tolerated", func(t *testing.T) {
		stream := basePrefix + okResultJSON + "\n" + `{"type":"assi`
		out, _ := ParseStream(strings.NewReader(stream), cfg, nil)
		if out.Poisoned {
			t.Fatalf("torn tail must be tolerated: %s", out.PoisonReason)
		}
		if !out.Terminal {
			t.Fatal("result before the torn tail must still be terminal")
		}
	})
}

// Oversized lines and total-byte exhaustion poison.
func TestStreamParser_SizeBounds(t *testing.T) {
	cfg := streamConfig(t)
	cfg.MaxLineBytes = 256

	t.Run("oversized line", func(t *testing.T) {
		stream := basePrefix + `{"type":"assistant","blob":"` + strings.Repeat("x", 512) + `"}` + "\n"
		out, _ := ParseStream(strings.NewReader(stream), cfg, nil)
		if !out.Poisoned || !strings.Contains(out.PoisonReason, "line") {
			t.Fatalf("oversized line must poison, got %+v", out)
		}
	})

	t.Run("total budget", func(t *testing.T) {
		cfg2 := streamConfig(t)
		cfg2.MaxTotalBytes = 512
		// Advisory-only lines: the budget must poison before any
		// result-processing verdict can fire.
		stream := strings.Repeat(hookStartedJSON+"\n", 4)
		out, _ := ParseStream(strings.NewReader(stream), cfg2, nil)
		if !out.Poisoned || !strings.Contains(out.PoisonReason, "total") {
			t.Fatalf("total-byte exhaustion must poison, got %+v", out)
		}
	})
}

// The four denial classes map from tool_result content; genuine tool
// errors are progress, not denials.
func TestStreamParser_DenialClassification(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantClass  string
		wantDenied bool
	}{
		{"approval denied",
			`Claude requested permissions to write to /x, but you haven't granted it yet.`,
			"approval_denied", true},
		{"guardrail hook denial",
			`PreToolUse:Glob hook error: [guardrail]: Guardrail denied this action: path capability call`,
			"guardrail_denied", true},
		{"tool disabled",
			`Error: No such tool available: Bash. Bash is disabled for this session, in subagents as well as here`,
			"tool_disabled", true},
		{"plain tool error is not a denial",
			`Error: ls: no such file or directory`,
			"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := streamConfig(t)
			toolUse := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]},"session_id":"` + testNativeID + `"}`
			toolResult := `{"type":"user","message":{"content":[{"type":"tool_result","is_error":` +
				boolJSON(tc.wantDenied) + `,"content":[{"type":"text","text":` + jsonString(tc.text) + `}]}]},"session_id":"` + testNativeID + `"}`
			stream := basePrefix + toolUse + "\n" + toolResult + "\n" + okResultJSON + "\n"

			var denials []StreamEvent
			out, err := ParseStream(strings.NewReader(stream), cfg, func(ev StreamEvent) {
				if ev.Type == EventToolDenied {
					denials = append(denials, ev)
				}
			})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if tc.wantDenied {
				if len(denials) != 1 {
					t.Fatalf("expected 1 denial, got %d", len(denials))
				}
				if denials[0].DeniedClass != tc.wantClass {
					t.Fatalf("class = %q, want %q", denials[0].DeniedClass, tc.wantClass)
				}
			} else if len(denials) != 0 {
				t.Fatalf("plain tool error must not be a denial, got %+v", denials)
			}
			if !out.Terminal {
				t.Fatal("stream must reach terminal after tool result")
			}
		})
	}
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func jsonString(s string) string {
	q := strings.ReplaceAll(s, `\`, `\\`)
	q = strings.ReplaceAll(q, `"`, `\"`)
	q = strings.ReplaceAll(q, "\n", `\n`)
	return `"` + q + `"`
}

// Init-vs-manifest mismatches poison with a toolkit-evidence reason:
// wrong cwd, wrong version, denied tool present, unknown tool, missing
// manifest elements.
func TestStreamParser_InitManifestChecks(t *testing.T) {
	cases := []struct {
		name    string
		init    string
		wantSub string
	}{
		{"wrong cwd",
			strings.Replace(baseInitJSON, `"/ws/claude"`, `"/elsewhere"`, 1),
			"cwd"},
		{"wrong version",
			strings.Replace(baseInitJSON, `"2.1.278"`, `"9.9.9"`, 1),
			"version"},
		{"denied tool enabled",
			strings.Replace(baseInitJSON, `"Read",`, `"Read","Bash",`, 1),
			"denied"},
		{"unknown tool",
			strings.Replace(baseInitJSON, `"Read",`, `"Read","MysteryTool",`, 1),
			"unknown"},
		{"missing expected skill",
			strings.Replace(baseInitJSON, `"research"`, `"other-skill"`, 1),
			"skill"},
		{"empty model",
			strings.Replace(baseInitJSON, `"claude-haiku-4-5-20251001"`, `""`, 1),
			"model identity"},
		{"unrelated model containing alias substring",
			strings.Replace(baseInitJSON, `"claude-haiku-4-5-20251001"`, `"claude-haiku-unrelated"`, 1),
			"model identity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := streamConfig(t)
			stream := hookStartedJSON + "\n" + tc.init + "\n" + okResultJSON + "\n"
			out, _ := ParseStream(strings.NewReader(stream), cfg, nil)
			if !out.Poisoned {
				t.Fatal("init mismatch must poison")
			}
			if !strings.Contains(out.PoisonReason, tc.wantSub) {
				t.Fatalf("reason %q must mention %q", out.PoisonReason, tc.wantSub)
			}
		})
	}
}

// Hooks seen before init are recorded for toolkit evidence.
func TestStreamParser_HooksSeenRecorded(t *testing.T) {
	cfg := streamConfig(t)
	stream := basePrefix + okResultJSON + "\n"
	out, _ := ParseStream(strings.NewReader(stream), cfg, nil)
	if out.Init == nil {
		t.Fatal("init must be reported")
	}
	found := false
	for _, h := range out.Init.HooksSeen {
		if h == "SessionStart:startup" {
			found = true
		}
	}
	if !found {
		t.Fatalf("hook must be recorded, got %v", out.Init.HooksSeen)
	}
}

// Init-only clean EOF: exactly one init, no result. The stream ends
// without a terminal event and without a terminal outcome.
func TestStreamParser_InitOnlyCleanEoFEmitsNoTerminal(t *testing.T) {
	cfg := streamConfig(t)
	stream := basePrefix + "\n"
	var terminals []StreamEvent
	out, err := ParseStream(strings.NewReader(stream), cfg, func(ev StreamEvent) {
		if ev.Type == EventTerminal {
			terminals = append(terminals, ev)
		}
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Poisoned {
		t.Fatalf("init-only clean EOF is not a poison, got %q", out.PoisonReason)
	}
	if out.Terminal {
		t.Fatal("no result observed: outcome must not be terminal")
	}
	if out.TerminalEventEmitted || len(terminals) != 0 {
		t.Fatal("terminal event must NOT be emitted for an init-only stream")
	}
	if out.Init == nil || out.Init.SessionID != testNativeID {
		t.Fatalf("init must still be reported, got %+v", out.Init)
	}
}

// Empty model in init is a mismatch (covered above); a missing init
// entirely (no init before result) poisons.
func TestStreamParser_MissingInitPoisons(t *testing.T) {
	cfg := streamConfig(t)
	stream := okResultJSON + "\n"
	out, _ := ParseStream(strings.NewReader(stream), cfg, nil)
	if !out.Poisoned || !strings.Contains(out.PoisonReason, "init") {
		t.Fatalf("missing init must poison, got %+v", out)
	}
}

// Session correlation: every correlated event must carry the expected
// native session id; a result before init poisons.
func TestStreamParser_SessionCorrelation(t *testing.T) {
	wrong := strings.Replace(baseInitJSON, testNativeID, "99999999-9999-4999-8999-999999999999", 1)

	t.Run("wrong session id on init", func(t *testing.T) {
		cfg := streamConfig(t)
		cfg.ExpectedSessionID = testNativeID
		out, _ := ParseStream(strings.NewReader(hookStartedJSON+"\n"+wrong+"\n"+okResultJSON+"\n"), cfg, nil)
		if !out.Poisoned || !strings.Contains(out.PoisonReason, "does not match the expected native session") {
			t.Fatalf("wrong init session must poison, got %+v", out)
		}
	})
	t.Run("wrong session id on result", func(t *testing.T) {
		cfg := streamConfig(t)
		cfg.ExpectedSessionID = testNativeID
		wrongResult := strings.Replace(okResultJSON, testNativeID, "99999999-9999-4999-8999-999999999999", 1)
		out, _ := ParseStream(strings.NewReader(hookStartedJSON+"\n"+baseInitJSON+"\n"+wrongResult+"\n"), cfg, nil)
		if !out.Poisoned || !strings.Contains(out.PoisonReason, "does not match the expected native session") {
			t.Fatalf("wrong result session must poison, got %+v", out)
		}
	})
	t.Run("assistant before init", func(t *testing.T) {
		cfg := streamConfig(t)
		cfg.ExpectedSessionID = testNativeID
		assistant := `{"type":"assistant","message":{"content":[{"type":"text","text":"early"}]},"session_id":"` + testNativeID + `"}`
		out, _ := ParseStream(strings.NewReader(assistant+"\n"), cfg, nil)
		if !out.Poisoned || !strings.Contains(out.PoisonReason, "before system/init") {
			t.Fatalf("assistant before init must poison, got %+v", out)
		}
	})
	t.Run("result before init", func(t *testing.T) {
		cfg := streamConfig(t)
		cfg.ExpectedSessionID = testNativeID
		out, _ := ParseStream(strings.NewReader(okResultJSON+"\n"), cfg, nil)
		if !out.Poisoned || !strings.Contains(out.PoisonReason, "before system/init") {
			t.Fatalf("result before init must poison, got %+v", out)
		}
	})
	t.Run("init after evidence", func(t *testing.T) {
		cfg := streamConfig(t)
		cfg.ExpectedSessionID = testNativeID
		stream := hookStartedJSON + "\n" + baseInitJSON + "\n" + okResultJSON + "\n" + baseInitJSON + "\n"
		out, _ := ParseStream(strings.NewReader(stream), cfg, nil)
		if !out.Poisoned || !strings.Contains(out.PoisonReason, "after turn evidence") {
			t.Fatalf("init after evidence must poison, got %+v", out)
		}
	})
	t.Run("matching ids accepted", func(t *testing.T) {
		cfg := streamConfig(t)
		cfg.ExpectedSessionID = testNativeID
		stream := basePrefix + okResultJSON + "\n"
		out, _ := ParseStream(strings.NewReader(stream), cfg, nil)
		if out.Poisoned {
			t.Fatalf("matching ids must pass, got %s", out.PoisonReason)
		}
		if !out.Terminal {
			t.Fatal("stream must reach terminal")
		}
	})
}

// Terminal events are emitted only after the stream completes cleanly,
// and the usage/cost payload is retained for persistence.
func TestStreamParser_UsageCostRetained(t *testing.T) {
	cfg := streamConfig(t)
	usage := `{"input_tokens":10,"output_tokens":101,"cache_read_input_tokens":25106}`
	resultJSON := `{"type":"result","subtype":"success","is_error":false,` +
		`"session_id":"` + testNativeID + `","result":"done","total_cost_usd":0.0303,` +
		`"duration_api_ms":1958,"usage":` + usage + `}`
	stream := basePrefix + resultJSON + "\n"

	var terminals []StreamEvent
	out, err := ParseStream(strings.NewReader(stream), cfg, func(ev StreamEvent) {
		if ev.Type == EventTerminal {
			terminals = append(terminals, ev)
		}
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.ResultUsage == nil {
		t.Fatal("usage payload must be retained")
	}
	if out.ResultUsage.TotalCostUSD != 0.0303 || out.ResultUsage.DurationAPIms != 1958 {
		t.Fatalf("usage numbers must be retained, got %+v", out.ResultUsage)
	}
	if !strings.Contains(out.ResultUsage.Raw, "total_cost_usd") {
		t.Fatal("raw usage JSON must be retained")
	}
	if len(terminals) != 1 {
		t.Fatalf("exactly one terminal event expected, got %d", len(terminals))
	}
}

// Tool denial events carry the ACTUAL tool name via tool_use_id
// correlation, not an empty field.
func TestStreamParser_ToolNameCorrelation(t *testing.T) {
	cfg := streamConfig(t)
	toolUse := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_42","name":"WebSearch","input":{}}]},"session_id":"` + testNativeID + `"}`
	toolResult := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_42","is_error":true,` +
		`"content":[{"type":"text","text":"Claude requested permissions to use WebSearch, but you haven't granted it yet."}]}]},"session_id":"` + testNativeID + `"}`
	stream := hookStartedJSON + "\n" + baseInitJSON + "\n" + toolUse + "\n" + toolResult + "\n" + okResultJSON + "\n"

	var denials []StreamEvent
	out, err := ParseStream(strings.NewReader(stream), cfg, func(ev StreamEvent) {
		if ev.Type == EventToolDenied {
			denials = append(denials, ev)
		}
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(denials) != 1 {
		t.Fatalf("expected 1 denial, got %d", len(denials))
	}
	if denials[0].ToolName != "WebSearch" {
		t.Fatalf("denial must carry the actual tool name, got %q", denials[0].ToolName)
	}
	if !out.Terminal {
		t.Fatalf("stream should reach terminal, got %+v", out)
	}
}
