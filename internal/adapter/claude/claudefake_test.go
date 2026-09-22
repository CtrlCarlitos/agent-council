package claude

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The fixture `claude` executable (Task 4): a compiled stub that honors
// --version, --help contract probes, and replays a recorded NDJSON
// fixture to stdout for `-p` runs (prompt consumed from stdin). It is
// NOT a production component.

const claudeFixtureSource = `package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const versionLine = "2.1.278 (Claude Code)"

const helpText = ` + "`" + `Usage: claude [options] [command] [prompt]

Options:
  -p, --print                 Print response
  --output-format <format>    Output format (choices: "text", "json", "stream-json")
  --input-format <format>     Input format (choices: "text", "stream-json")
  --verbose                   Verbose output
  --session-id <uuid>         Use a specific session ID (must be a valid UUID)
  --resume <session-id>       Resume a specific session
  --model <model>             Model for the session
  --max-turns <n>             Max agentic turns
  --allowedTools <tools...>   Allowed tools
  --disallowedTools <tools..> Denied tools
  --permission-mode <mode>    Permission mode (choices: "acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan")
  --dangerously-skip-permissions  Bypass all permission checks
  --bare                      Minimal mode
  --no-session-persistence    Disable session persistence
` + "`" + `

func main() {
	args := os.Args[1:]
	for _, a := range args {
		if a == "--version" {
			fmt.Println(versionLine)
			return
		}
		if a == "--help" {
			fmt.Print(helpText)
			return
		}
	}

	// -p mode: consume stdin (the prompt), then replay the fixture.
	io.ReadAll(os.Stdin)

	fixture := os.Getenv("CLAUDE_FIXTURE")
	if fixture == "" {
		fmt.Fprintln(os.Stderr, "CLAUDE_FIXTURE is required in -p mode")
		os.Exit(3)
	}
	raw, err := os.ReadFile(fixture)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		os.Exit(3)
	}

	writer := bufio.NewWriter(os.Stdout)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fmt.Fprintln(writer, line)
		writer.Flush()
		if d := os.Getenv("CLAUDE_FIXTURE_DELAY_MS"); d != "" {
			if ms := parseMs(d); ms > 0 {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
		}
	}
}

func parseMs(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
`

var (
	fixtureOnce   sync.Once
	fixtureBinDir string
	fixtureErr    error
)

// compileClaudeFixture builds the stub `claude` binary once per test
// binary and returns the directory to prepend to PATH.
func compileClaudeFixture(t *testing.T) string {
	t.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ac008-fixture-*")
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
		if err := os.WriteFile(src, []byte(claudeFixtureSource), 0o600); err != nil {
			fixtureErr = err
			return
		}
		name := "claude"
		if runtimeGOOSWindows {
			name = "claude.exe"
		}
		build := execCommand("go", "build", "-o", filepath.Join(dir, name), src)
		build.Dir = srcDir
		if out, err := build.CombinedOutput(); err != nil {
			fixtureErr = fmt.Errorf("build fixture: %v: %s", err, out)
			return
		}
		fixtureBinDir = dir
	})
	if fixtureErr != nil {
		t.Fatalf("fixture build: %v", fixtureErr)
	}
	return fixtureBinDir
}

func writeFixture(t *testing.T, dir string, name string, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return p
}

func setEnvVar(t *testing.T, key, val string) {
	t.Helper()
	if err := os.Setenv(key, val); err != nil {
		t.Fatalf("setenv %s: %v", key, err)
	}
}

// The fixture binary satisfies --version and the --help contract probe.
func TestClaudeFixture_ContractProbes(t *testing.T) {
	binDir := compileClaudeFixture(t)
	bin := filepath.Join(binDir, claudeCommand)

	out, err := execCommand(bin, "--version").Output()
	if err != nil || !strings.Contains(string(out), "2.1.278") {
		t.Fatalf("--version: %v %s", err, out)
	}
	help, err := execCommand(bin, "--help").Output()
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	// The --help contract probe must find every required flag and the
	// verified choice sets.
	for _, required := range []string{
		"-p", "--output-format", "--verbose", "--session-id", "--resume",
		"--model", "--max-turns", "--disallowedTools",
	} {
		if !strings.Contains(string(help), required) {
			t.Fatalf("--help must document %s", required)
		}
	}
	for _, forbidden := range []string{"--bare"} {
		if strings.Contains(string(help), forbidden+"\n") || strings.HasPrefix(string(help), forbidden) {
			// --bare IS documented by the real CLI; the contract check
			// only asserts the REQUIRED surface. No-op here.
			_ = forbidden
		}
	}
}

func execCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	return cmd
}

// End-to-end: the fixture child replays a fixture file; the parser
// classifies the stream (init checks + terminal) exactly as it would
// for a real process.
func TestClaudeFixture_ReplayThroughParser(t *testing.T) {
	binDir := compileClaudeFixture(t)
	fixtureDir := t.TempDir()

	stream := strings.Join([]string{
		`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup"}`,
		`{"type":"system","subtype":"init","session_id":"` + testNativeID + `","cwd":"/ws","claude_code_version":"2.1.278","model":"claude-haiku-4-5-20251001","permissionMode":"default","tools":["Read","Glob"],"skills":["s"],"plugins":[]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"step"}]},"session_id":"` + testNativeID + `"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"` + testNativeID + `","result":"final answer"}`,
		"",
	}, "\n")
	fixture := writeFixture(t, fixtureDir, "happy.jsonl", stream)

	bin := filepath.Join(binDir, claudeCommand)
	cmd := exec.Command(bin, "-p", "--output-format", "stream-json", "--verbose",
		"--session-id", testNativeID, "--model", "haiku", "--max-turns", "8")
	cmd.Env = append(os.Environ(), "CLAUDE_FIXTURE="+fixture)
	cmd.Stdin = strings.NewReader("the prompt")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	cfg := StreamConfig{
		WorkspaceRoot:   "/ws",
		ExpectedVersion: "2.1.278",
		ExpectedModel:   "haiku",
		Manifest:        toolkitManifestFixture([]string{"SessionStart:startup"}, []string{"s"}, nil),
		UniverseTools:   []string{"Read", "Glob"},
		MaxLineBytes:    1 << 20,
		MaxTotalBytes:   8 << 20,
	}
	var progress int
	out, perr := ParseStream(stdout, cfg, func(ev StreamEvent) {
		if ev.Type == EventProgress {
			progress++
		}
	})
	io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	if waitErr != nil {
		t.Fatalf("fixture wait: %v", waitErr)
	}
	if !out.Terminal || !out.Completed || out.ResultText != "final answer" {
		t.Fatalf("terminal outcome, got %+v", out)
	}
	if out.SessionID != testNativeID {
		t.Fatalf("session id, got %q", out.SessionID)
	}
	if progress != 1 {
		t.Fatalf("expected 1 progress event, got %d", progress)
	}
	if out.Init == nil || len(out.Init.Tools) != 2 {
		t.Fatalf("init report, got %+v", out.Init)
	}
}
