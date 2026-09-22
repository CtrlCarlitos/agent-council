package claude

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The fixture `claude` executable: a compiled stub that honors
// --version and --help contract probes, and replays a stream for -p
// mode (consuming stdin). The default happy stream uses the session-id
// from args; a .claude-fixture file in the CWD overrides the stream.

var claudeFixtureSourceLines = []string{
	"package main",
	"",
	"import (",
	"\t\"fmt\"",
	"\t\"io\"",
	"\t\"os\"",
	"\t\"strings\"",
	")",
	"",
	"const versionLine = \"2.1.278 (Claude Code)\"",
	"",
	"const helpText = \"Usage: claude [options] [prompt]\\n\" +",
	"\t\"  -p, --print                 Print response\\n\" +",
	"\t\"  --output-format <format>    Output format (choices: text, json, stream-json)\\n\" +",
	"\t\"  --verbose                   Verbose\\n\" +",
	"\t\"  --session-id <uuid>         Session ID (must be valid UUID)\\n\" +",
	"\t\"  --resume <session-id>       Resume a session\\n\" +",
	"\t\"  --model <model>             Model\\n\" +",
	"\t\"  --max-turns <n>             Max turns\\n\" +",
	"\t\"  --disallowedTools <tools>   Denied tools\\n\" +",
	"\t\"  --allowedTools <tools>      Allowed tools\\n\"",
	"",
	"func main() {",
	"\targs := os.Args[1:]",
	"\tfor _, a := range args {",
	"\t\tif a == \"--version\" {",
	"\t\t\tfmt.Println(versionLine)",
	"\t\t\treturn",
	"\t\t}",
	"\t\tif a == \"--help\" {",
	"\t\t\tfmt.Print(helpText)",
	"\t\t\treturn",
	"\t\t}",
	"\t}",
	"",
	"\tio.ReadAll(os.Stdin)",
	"",
	"\tif raw, err := os.ReadFile(\".claude-fixture\"); err == nil {",
	"\t\tfor _, line := range strings.Split(string(raw), \"\\n\") {",
	"\t\t\tif strings.TrimSpace(line) != \"\" {",
	"\t\t\t\tfmt.Fprintln(os.Stdout, line)",
	"\t\t\t}",
	"\t\t}",
	"\t\treturn",
	"\t}",
	"",
	"\tsid := \"00000000-0000-4000-8000-000000000000\"",
	"\tcwd, _ := os.Getwd()",
	"\tfor i, a := range args {",
	"\t\tif (a == \"--session-id\" || a == \"--resume\") && i+1 < len(args) {",
	"\t\t\tsid = args[i+1]",
	"\t\t}",
	"\t}",
	"",
	"\temit := func(format string, vals ...interface{}) {",
	"\t\tfmt.Fprintf(os.Stdout, format, vals...)",
	"\t\tfmt.Fprintln(os.Stdout)",
	"\t}",
	"\temit(\"{\\\"type\\\":\\\"system\\\",\\\"subtype\\\":\\\"hook_started\\\",\\\"hook_name\\\":\\\"SessionStart:startup\\\"}\")",
	"\temit(\"{\\\"type\\\":\\\"system\\\",\\\"subtype\\\":\\\"init\\\",\\\"session_id\\\":%q,\\\"cwd\\\":%q,\\\"claude_code_version\\\":\\\"2.1.278\\\",\\\"model\\\":\\\"claude-haiku-4-5-20251001\\\",\\\"permissionMode\\\":\\\"default\\\",\\\"tools\\\":[\\\"Read\\\",\\\"Glob\\\",\\\"Grep\\\"],\\\"skills\\\":[],\\\"plugins\\\":[]}\", sid, cwd)",
	"\temit(\"{\\\"type\\\":\\\"assistant\\\",\\\"message\\\":{\\\"content\\\":[{\\\"type\\\":\\\"text\\\",\\\"text\\\":\\\"working\\\"}]},\\\"session_id\\\":%q}\", sid)",
	"\temit(\"{\\\"type\\\":\\\"result\\\",\\\"subtype\\\":\\\"success\\\",\\\"is_error\\\":false,\\\"session_id\\\":%q,\\\"result\\\":\\\"fixture response\\\"}\", sid)",
	"}",
}

var (
	fixtureOnce   sync.Once
	fixtureBinDir string
	fixtureErr    error
)

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
		if err := os.WriteFile(src, []byte(strings.Join(claudeFixtureSourceLines, "\n")), 0o600); err != nil {
			fixtureErr = err
			return
		}
		name := "claude"
		if runtime.GOOS == "windows" {
			name = "claude.exe"
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, name), src)
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

func writeFixtureFile(t *testing.T, dir string, content string) string {
	t.Helper()
	p := filepath.Join(dir, ".claude-fixture")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

// The fixture binary satisfies --version and --help contract probes.
func TestClaudeFixture_ContractProbes(t *testing.T) {
	binDir := compileClaudeFixture(t)
	bin := filepath.Join(binDir, "claude")

	out, err := exec.Command(bin, "--version").Output()
	if err != nil || !strings.Contains(string(out), "2.1.278") {
		t.Fatalf("--version: %v %s", err, out)
	}
	help, err := exec.Command(bin, "--help").Output()
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	for _, required := range []string{
		"-p", "--output-format", "--verbose", "--session-id", "--resume",
		"--model", "--max-turns", "--disallowedTools",
	} {
		if !strings.Contains(string(help), required) {
			t.Fatalf("--help must document %s", required)
		}
	}
}

// End-to-end: the fixture child replays a happy stream; the parser
// produces a terminal outcome with the correct session id.
func TestClaudeFixture_ReplayThroughParser(t *testing.T) {
	binDir := compileClaudeFixture(t)
	bin := filepath.Join(binDir, "claude")

	cmd := exec.Command(bin, "-p", "--output-format", "stream-json", "--verbose",
		"--session-id", testNativeID, "--model", "claude-haiku-4-5-20251001", "--max-turns", "8")
	cmd.Stdin = strings.NewReader("the prompt")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	realCwd, _ := os.Getwd()
	cfg := StreamConfig{
		WorkspaceRoot:         realCwd,
		ExpectedVersion:       "2.1.278",
		ExpectedModelIdentity: "claude-haiku-4-5-20251001",
		ExpectedSessionID:     testNativeID,
		Manifest:              toolkitManifestFixture([]string{"SessionStart:startup"}, []string{"s"}, nil),
		UniverseTools:         []string{"Read", "Glob", "Grep"},
		MaxLineBytes:          1 << 20,
		MaxTotalBytes:         8 << 20,
	}
	out, perr := ParseStream(stdout, cfg, nil)
	io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	if waitErr != nil {
		t.Fatalf("wait: %v", waitErr)
	}
	if !out.Terminal || !out.Completed || out.ResultText != "fixture response" {
		t.Fatalf("terminal outcome, got %+v", out)
	}
	if out.SessionID != testNativeID {
		t.Fatalf("session id, got %q", out.SessionID)
	}
}
