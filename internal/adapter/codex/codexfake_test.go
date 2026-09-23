//go:build unix

package codex

// The fixture `codex` executable: a compiled stub speaking JSON-RPC 2.0
// over stdio (one object per line), launched through the real
// PolicyExecutor exactly like the native app-server child. It replays the
// recorded, sanitized fixture scenarios from testdata/*.jsonl, staged into
// the child's working directory as .codex-fixture-scenario.jsonl.
//
// Built-in behavior (no scenario needed):
//   - initialize is always answered with the recorded handshake shape,
//     codexHome derived from CODEX_HOME (never set by the adapter) or
//     $HOME/.codex, platform from runtime (overridable for mismatch
//     evidence via the .codex-fixture-platform knob)
//   - unknown methods are answered with the -32600 unknown-variant error
//
// Scenario directives (JSONL, sanitized from the 0.154.0 research
// evidence):
//   {"respond": {"method": M, "result": R}}            answer every M request with R
//   {"respond_error": {"method": M, "code": C, "message": S}}
//   {"emit_on_request": {"method": M, "line": L}}      emit L BEFORE answering M (consume once)
//   {"emit_after_response": {"method": M, "line": L}}  emit L AFTER answering M (consume once)
//   {"emit_many_on_request": {"method": M, "lines": [...]}}
//                                                      emit all L BEFORE answering M (consume once)
//   {"emit_many_after_response": {"method": M, "lines": [...]}}
//                                                      emit all L AFTER answering M (consume once)
//   {"append_on_request": {"method": M, "path": P, "lines": [...]}}
//                                                      append lines to file P BEFORE answering M (consume once)
//   {"emit": L} / {"emit_raw": S} / {"emit_oversized": N}   immediate output
//   {"stderr": S} / {"delay_ms": N}                    immediate side effects
//
// Evidence knobs (resolved in the child's CWD):
//   .codex-fixture-args         append-mode argv log (template evidence)
//   .codex-fixture-requests     append-mode request log (method evidence)
//   .codex-fixture-replies      verbatim reply frames (server→client
//                               approval answers — payload evidence)
//   .codex-fixture-terminated   written on graceful SIGTERM (with pid)
//   .codex-fixture-platform     overrides platformOs (handshake mismatch)
//   .codex-fixture-heavy-stderr 200KiB stderr before serving (drain evidence)

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const codexFixtureSource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type respondRule struct {
	Method  string          ` + "`json:\"method\"`" + `
	Result  json.RawMessage ` + "`json:\"result\"`" + `
	DelayMs int64           ` + "`json:\"delay_ms\"`" + `
}

type respondErrorRule struct {
	Method  string ` + "`json:\"method\"`" + `
	Code    int    ` + "`json:\"code\"`" + `
	Message string ` + "`json:\"message\"`" + `
	DelayMs int64  ` + "`json:\"delay_ms\"`" + `
}

type lineRule struct {
	Method string          ` + "`json:\"method\"`" + `
	Line   json.RawMessage ` + "`json:\"line\"`" + `
	used   bool
}

type appendRule struct {
	Method string   ` + "`json:\"method\"`" + `
	Path   string   ` + "`json:\"path\"`" + `
	Lines  []string ` + "`json:\"lines\"`" + `
	used   bool
}

type multiLineRule struct {
	Method string            ` + "`json:\"method\"`" + `
	Lines  []json.RawMessage ` + "`json:\"lines\"`" + `
	used   bool
}

type directive struct {
	Respond           *respondRule      ` + "`json:\"respond\"`" + `
	RespondError      *respondErrorRule ` + "`json:\"respond_error\"`" + `
	EmitOnRequest     *lineRule         ` + "`json:\"emit_on_request\"`" + `
	EmitAfterResponse *lineRule         ` + "`json:\"emit_after_response\"`" + `
	AppendOnRequest   *appendRule       ` + "`json:\"append_on_request\"`" + `
	EmitManyOnRequest *multiLineRule    ` + "`json:\"emit_many_on_request\"`" + `
	EmitManyAfterResp *multiLineRule    ` + "`json:\"emit_many_after_response\"`" + `
	Emit              json.RawMessage   ` + "`json:\"emit\"`" + `
	EmitRaw           string            ` + "`json:\"emit_raw\"`" + `
	EmitOversized     int64             ` + "`json:\"emit_oversized\"`" + `
	Stderr            string            ` + "`json:\"stderr\"`" + `
	DelayMs           int64             ` + "`json:\"delay_ms\"`" + `
}

func appendLine(name, line string) {
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func main() {
	for _, a := range os.Args[1:] {
		_ = a
	}
	appendLine(".codex-fixture-args", strings.Join(os.Args[1:], "\x1f"))

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-ch
		appendLine(".codex-fixture-terminated", strconv.Itoa(os.Getpid()))
		os.Exit(0)
	}()

	if _, err := os.Stat(".codex-fixture-heavy-stderr"); err == nil {
		fmt.Fprint(os.Stderr, strings.Repeat("x", 200<<10))
	} else {
		fmt.Fprint(os.Stderr, "codex fixture stderr noise\n")
	}

	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home := os.Getenv("HOME")
		codexHome = home + "/.codex"
	}
	platformOS := runtime.GOOS
	if b, err := os.ReadFile(".codex-fixture-platform"); err == nil {
		platformOS = strings.TrimSpace(string(b))
	}
	platformFamily := "unix"

	initResult := map[string]any{
		"userAgent":      "agent-council/0.154.0 (fixture; x86_64)",
		"codexHome":      codexHome,
		"platformFamily": platformFamily,
		"platformOs":     platformOS,
	}

	// Load scenario directives.
	var (
		responds  []respondRule
		respErrs  []respondErrorRule
		onReq     []*lineRule
		afterResp []*lineRule
		appends   []*appendRule
		manyReq   []*multiLineRule
		manyAfter []*multiLineRule
	)
	if raw, err := os.ReadFile(".codex-fixture-scenario.jsonl"); err == nil {
		for _, ln := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(ln) == "" {
				continue
			}
			var d directive
			if err := json.Unmarshal([]byte(ln), &d); err != nil {
				continue
			}
			switch {
			case d.Respond != nil:
				responds = append(responds, *d.Respond)
			case d.RespondError != nil:
				respErrs = append(respErrs, *d.RespondError)
			case d.EmitOnRequest != nil:
				onReq = append(onReq, &lineRule{Method: d.EmitOnRequest.Method, Line: d.EmitOnRequest.Line})
			case d.EmitAfterResponse != nil:
				afterResp = append(afterResp, &lineRule{Method: d.EmitAfterResponse.Method, Line: d.EmitAfterResponse.Line})
			case d.AppendOnRequest != nil:
				appends = append(appends, &appendRule{Method: d.AppendOnRequest.Method, Path: d.AppendOnRequest.Path, Lines: d.AppendOnRequest.Lines})
			case d.EmitManyOnRequest != nil:
				manyReq = append(manyReq, &multiLineRule{Method: d.EmitManyOnRequest.Method, Lines: d.EmitManyOnRequest.Lines})
			case d.EmitManyAfterResp != nil:
				manyAfter = append(manyAfter, &multiLineRule{Method: d.EmitManyAfterResp.Method, Lines: d.EmitManyAfterResp.Lines})
			case len(d.Emit) > 0:
				fmt.Fprintln(os.Stdout, string(d.Emit))
			case d.EmitRaw != "":
				fmt.Fprintln(os.Stdout, d.EmitRaw)
			case d.EmitOversized > 0:
				fmt.Fprintln(os.Stdout, strings.Repeat("x", int(d.EmitOversized)))
			case d.Stderr != "":
				fmt.Fprint(os.Stderr, d.Stderr)
			case d.DelayMs > 0:
				time.Sleep(time.Duration(d.DelayMs) * time.Millisecond)
			}
		}
	}

	replyResult := func(id json.RawMessage, result any) {
		r, _ := json.Marshal(result)
		fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n", string(id), string(r))
	}
	replyError := func(id json.RawMessage, code int, msg string) {
		fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":%d,\"message\":%q}}\n", string(id), code, msg)
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     json.RawMessage ` + "`json:\"id\"`" + `
			Method string          ` + "`json:\"method\"`" + `
			Params json.RawMessage ` + "`json:\"params\"`" + `
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.Method == "" {
			// A response frame (Council's reply to a server→client
			// request): log verbatim as approval-payload evidence.
			appendLine(".codex-fixture-replies", line)
			continue
		}
		appendLine(".codex-fixture-requests", req.Method)

		if req.Method == "initialize" {
			replyResult(req.ID, initResult)
			continue
		}

		for _, r := range onReq {
			if !r.used && r.Method == req.Method {
				fmt.Fprintln(os.Stdout, string(r.Line))
				r.used = true
				break
			}
		}

		for _, r := range manyReq {
			if !r.used && r.Method == req.Method {
				for _, l := range r.Lines {
					fmt.Fprintln(os.Stdout, string(l))
				}
				r.used = true
				break
			}
		}

		for _, r := range appends {
			if !r.used && r.Method == req.Method {
				f, err := os.OpenFile(r.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
				if err == nil {
					for _, l := range r.Lines {
						fmt.Fprintln(f, l)
					}
					f.Close()
				}
				r.used = true
				break
			}
		}

		answered := false
		for _, r := range responds {
			if r.Method == req.Method {
				if r.DelayMs > 0 {
					time.Sleep(time.Duration(r.DelayMs) * time.Millisecond)
				}
				replyResult(req.ID, json.RawMessage(r.Result))
				answered = true
				break
			}
		}
		if !answered {
			for _, r := range respErrs {
				if r.Method == req.Method {
					if r.DelayMs > 0 {
						time.Sleep(time.Duration(r.DelayMs) * time.Millisecond)
					}
					replyError(req.ID, r.Code, r.Message)
					answered = true
					break
				}
			}
		}
		if !answered {
			replyError(req.ID, -32600, "Invalid request: unknown variant <"+req.Method+">")
			answered = true
		}

		for _, r := range manyAfter {
			if !r.used && r.Method == req.Method {
				for _, l := range r.Lines {
					fmt.Fprintln(os.Stdout, string(l))
				}
				r.used = true
				break
			}
		}
		for _, r := range afterResp {
			if !r.used && r.Method == req.Method {
				fmt.Fprintln(os.Stdout, string(r.Line))
				r.used = true
				break
			}
		}
	}
	// stdin closed: parent is gone or the connection ended; park until
	// signaled so lifecycle evidence stays deterministic.
	select {}
}
`

var (
	codexFixtureOnce   sync.Once
	codexFixtureBinDir string
	codexFixtureErr    error
)

// compileCodexFixture builds the fake `codex` binary once per test binary
// and returns the directory to prepend to PATH.
func compileCodexFixture(t *testing.T) string {
	t.Helper()
	codexFixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ac009-fake-*")
		if err != nil {
			codexFixtureErr = err
			return
		}
		srcDir := filepath.Join(dir, "src")
		if err := os.MkdirAll(srcDir, 0o700); err != nil {
			codexFixtureErr = err
			return
		}
		src := filepath.Join(srcDir, "main.go")
		if err := os.WriteFile(src, []byte(codexFixtureSource), 0o600); err != nil {
			codexFixtureErr = err
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "codex"), src)
		build.Dir = srcDir
		if out, err := build.CombinedOutput(); err != nil {
			codexFixtureErr = fmt.Errorf("build codex fixture: %v: %s", err, out)
			return
		}
		codexFixtureBinDir = dir
	})
	if codexFixtureErr != nil {
		t.Fatalf("codex fixture build: %v", codexFixtureErr)
	}
	return codexFixtureBinDir
}

// stageFixture copies a committed testdata scenario into the child's
// working directory as the active scenario.
func stageFixture(t *testing.T, cwd, name string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatalf("read fixture scenario %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".codex-fixture-scenario.jsonl"), raw, 0o600); err != nil {
		t.Fatalf("stage scenario: %v", err)
	}
}

// writeScenario composes a scenario from raw directive lines (for tests
// that need a combination the committed files do not carry).
func writeScenario(t *testing.T, cwd string, lines ...string) {
	t.Helper()
	joined := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(cwd, ".codex-fixture-scenario.jsonl"), []byte(joined), 0o600); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
}

// readFixtureFile reads a child evidence file, returning nil lines when it
// does not exist.
func readFixtureFile(t *testing.T, cwd, name string) []string {
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

// waitForFile polls for a child evidence file to appear.
func waitForFile(t *testing.T, cwd, name string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if lines := readFixtureFile(t, cwd, name); lines != nil {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("evidence file %s never appeared in %s", name, cwd)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
