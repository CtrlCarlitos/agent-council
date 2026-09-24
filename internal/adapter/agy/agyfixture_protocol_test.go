//go:build unix

package agy

// Protocol tests for the compiled fixture `agy` executable
// (agyfake_test.go): every test here launches the real fixture binary
// through the real PolicyExecutor (execpolicy) exactly like a live
// adapter would—on Linux via the sealed, ptrace-verified path built
// from Task 3, off Linux via the explicit FixtureLaunch marker—and
// asserts on the decoded stream-json events and stderr the child
// actually produced. It carries //go:build unix for the same reason
// agyfake_test.go does: the fixture program's SIGINT handling.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// startAgyFixture builds the fixture binary (once per test binary),
// launches it through the real PolicyExecutor with an agy-shaped
// LaunchRequest rooted at scratch, and returns the running process.
// On Linux it builds a real execpolicy.SealedImage from the compiled
// binary and launches through the sealed path (Task 3 exercised by
// every fixture test here); off Linux, where NewSealedImage is
// unsupported, it falls back to an ordinary path launch under the
// explicit FixtureLaunch marker—the one bypass agytest (and this
// package's own fixture tests) is authorized to use.
func startAgyFixture(t *testing.T, scratch string, args []string) execpolicy.ManagedProcess {
	t.Helper()
	binDir := compileAgyFixture(t)
	fixturePath := filepath.Join(binDir, "agy")

	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	req := execpolicy.LaunchRequest{
		RunID:     "run-agyfixture",
		SessionID: "session-agyfixture",
		Command:   fixturePath,
		Args:      args,
		Paths:     workspace.WorkspacePaths{Root: scratch, Config: scratch},
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v4",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"agy"},
		},
		FixtureLaunch: true,
	}

	sealed, sealErr := execpolicy.NewSealedImage(fixturePath, digest)
	switch {
	case sealErr == nil:
		req.SealedImage = sealed
		req.Command = sealed.ArgV0
		t.Cleanup(func() { _ = sealed.Close() })
	case errors.Is(sealErr, execpolicy.ErrSealedLaunchUnsupported):
		// Off Linux: the ordinary path launch under the explicit marker.
	default:
		t.Fatalf("build sealed image: %v", sealErr)
	}

	proc, err := execpolicy.New().Start(context.Background(), req)
	if err != nil {
		t.Fatalf("start agy fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = proc.Terminate(context.Background())
	})
	return proc
}

// agyFrozenArgs builds the frozen-shaped argv IsAgyLaunch recognizes,
// with optional trailing extra flags (e.g. --conversation <id>).
func agyFrozenArgs(model string, extra ...string) []string {
	base := []string{
		"--print=",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--disable-slash-commands",
		"--model", model,
		"--print-timeout", "120s",
		"--log-file", "agy.log",
	}
	return append(base, extra...)
}

func sendRawStdinLine(t *testing.T, proc execpolicy.ManagedProcess, line string) {
	t.Helper()
	if _, err := io.WriteString(proc.Stdin(), line+"\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
}

// streamAgyEvents runs ReadEvents against proc's stdout in the
// background, delivering each decoded event on the returned channel
// (closed at end of stream). Used by every protocol test, directly by
// the ones that must act (e.g. send SIGINT) between two specific
// events, and via runAgyFixture for the ones that don't.
func streamAgyEvents(proc execpolicy.ManagedProcess) <-chan Event {
	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		_ = ReadEvents(proc.Stdout(), func(ev Event) error {
			ch <- ev
			return nil
		}, nil)
	}()
	return ch
}

// drainAgyStderr copies proc's stderr into a boundedBuffer in the
// background (safe to read from concurrently via String(), including
// while the child is still running).
func drainAgyStderr(proc execpolicy.ManagedProcess) *boundedBuffer {
	buf := newBoundedBuffer(1 << 20)
	go func() { _, _ = io.Copy(buf, proc.Stderr()) }()
	return buf
}

// runAgyFixture drains proc's stdout/stderr in the background, waits
// for the init event, THEN writes userLine to stdin (skipped when
// userLine is empty), drains every remaining event to completion, and
// waits for exit. Waiting for init before writing stdin mirrors the
// protocol's own rule (AC-010 research §7.5: "wait for init ...
// transmit only then") and, just as importantly, removes any startup
// race between the child being spawned and it actually being ready to
// read stdin—writing blind immediately after Start() returned was
// observed to be flaky under -race's heavier scheduling. Safe to use
// whenever the run does not need mid-stream intervention (see
// TestAgyFixture_Interrupt for the case that does, which drives
// streamAgyEvents/drainAgyStderr directly instead).
func runAgyFixture(t *testing.T, proc execpolicy.ManagedProcess, userLine string) (initEv Event, rest []Event, stderrText string, exitCode int) {
	t.Helper()
	events := streamAgyEvents(proc)
	stderrBuf := drainAgyStderr(proc)

	var ok bool
	initEv, ok = <-events
	if !ok || initEv.Kind != EventKindInit {
		t.Fatalf("expected the init event first, got %+v ok=%v", initEv, ok)
	}

	if userLine != "" {
		sendRawStdinLine(t, proc, userLine)
	}

	for ev := range events {
		rest = append(rest, ev)
	}

	code, waitErr := proc.Wait()
	if waitErr != nil {
		t.Fatalf("proc.Wait: %v", waitErr)
	}
	return initEv, rest, stderrBuf.String(), code
}

// ── protocol tests ──────────────────────────────────────────────────

func TestAgyFixture_Handshake(t *testing.T) {
	scratch := t.TempDir()
	knownID := "22222222-2222-4222-8222-000000000002"
	writeAgyScenario(t, scratch,
		`{"known_conversation":"`+knownID+`"}`,
		`{"step":{"state":"ACTIVE","step_type":"agent_response","text_delta":"OK"}}`,
		`{"step":{"state":"DONE","step_type":"agent_response","text_delta":"OK\n","duration_seconds":0.05}}`,
	)

	proc := startAgyFixture(t, scratch, agyFrozenArgs("gpt-oss-120b-medium", "--conversation", knownID))

	initEv, rest, stderrText, code := runAgyFixture(t, proc, `{"event":"user","message":{"content":"hi"}}`)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderrText)
	}
	if initEv.ConversationID != knownID {
		t.Fatalf("unexpected init event: %+v", initEv)
	}
	if initEv.Init.Model != "gpt-oss-120b-medium" {
		t.Fatalf("init.model = %q", initEv.Init.Model)
	}
	if len(rest) != 3 {
		t.Fatalf("expected 3 more events (2 steps, result), got %d: %+v", len(rest), rest)
	}
	if rest[0].Kind != EventKindStepUpdate || rest[0].Step.State != "ACTIVE" {
		t.Fatalf("unexpected step 1: %+v", rest[0])
	}
	if rest[1].Kind != EventKindStepUpdate || rest[1].Step.State != "DONE" {
		t.Fatalf("unexpected step 2: %+v", rest[1])
	}
	result := rest[2]
	if result.Kind != EventKindResult || result.Result.Status != "SUCCESS" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.ConversationID != knownID {
		t.Fatalf("resumed conversation id = %q, want exact resumption of %q", result.ConversationID, knownID)
	}

	argLines := readAgyFixtureFile(t, scratch, ".agy-fixture-args")
	if len(argLines) != 1 || !strings.Contains(argLines[0], knownID) {
		t.Fatalf("expected argv log to record the launch, got: %v", argLines)
	}
	inputLines := readAgyFixtureFile(t, scratch, ".agy-fixture-input")
	if len(inputLines) != 1 || !strings.Contains(inputLines[0], `"content":"hi"`) {
		t.Fatalf("expected stdin log to record the user message, got: %v", inputLines)
	}
}

func TestAgyFixture_EnvelopeValidation_MissingEventField(t *testing.T) {
	scratch := t.TempDir()
	proc := startAgyFixture(t, scratch, agyFrozenArgs("m"))

	_, rest, stderrText, code := runAgyFixture(t, proc, `{"foo":1}`)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if len(rest) != 0 {
		t.Fatalf("expected no further events after init, got %+v", rest)
	}
	const wantErr = `error: stream input "user" message is missing the "event" field`
	if !strings.Contains(stderrText, wantErr) {
		t.Fatalf("stderr = %q, want it to contain %q", stderrText, wantErr)
	}
}

func TestAgyFixture_EnvelopeValidation_MissingMessageField(t *testing.T) {
	scratch := t.TempDir()
	proc := startAgyFixture(t, scratch, agyFrozenArgs("m"))

	_, rest, _, code := runAgyFixture(t, proc, `{"event":"user"}`)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if len(rest) != 1 {
		t.Fatalf("expected exactly a terminal result after init (rejected input still yields a result), got %d: %+v", len(rest), rest)
	}
	result := rest[0]
	if result.Kind != EventKindResult || result.Result.Status != "ERROR" || result.Result.NumTurns != 0 {
		t.Fatalf("unexpected result: %+v", result.Result)
	}
	const wantErr = `stream input "user" message is missing the "message" field`
	if result.Result.Error != wantErr {
		t.Fatalf("result.error = %q, want %q", result.Result.Error, wantErr)
	}
}

func TestAgyFixture_EnvelopeValidation_UnsupportedEventIgnored(t *testing.T) {
	scratch := t.TempDir()
	proc := startAgyFixture(t, scratch, agyFrozenArgs("m"))

	_, rest, stderrText, code := runAgyFixture(t, proc, `{"event":"bogus"}`)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (ignored, not rejected); stderr: %s", code, stderrText)
	}
	if len(rest) != 0 {
		t.Fatalf("expected no further events after init (no turn ran), got %+v", rest)
	}
	found := false
	for _, line := range strings.Split(stderrText, "\n") {
		if StderrMarker(line) == MarkerIgnoredInput {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a stderr line classified MarkerIgnoredInput, got: %q", stderrText)
	}
}

func TestAgyFixture_DeniedActions(t *testing.T) {
	scratch := t.TempDir()
	writeAgyScenario(t, scratch, `{"denied_actions":[{"action":"command","display_name":"RunCommand"}]}`)
	proc := startAgyFixture(t, scratch, agyFrozenArgs("m"))

	_, rest, _, code := runAgyFixture(t, proc, `{"event":"user","message":{"content":"run echo AC010-PROBE"}}`)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (denial keeps exit 0, per §7.3)", code)
	}
	if len(rest) == 0 {
		t.Fatalf("expected at least a terminal result after init")
	}
	last := rest[len(rest)-1]
	if last.Kind != EventKindResult || last.Result.Status != "SUCCESS" || last.Result.Response != "" {
		t.Fatalf("unexpected result: %+v", last.Result)
	}
	if len(last.Result.DeniedActions) != 1 || last.Result.DeniedActions[0].Action != "command" || last.Result.DeniedActions[0].DisplayName != "RunCommand" {
		t.Fatalf("unexpected denied actions: %+v", last.Result.DeniedActions)
	}
}

func TestAgyFixture_Interrupt(t *testing.T) {
	scratch := t.TempDir()
	writeAgyScenario(t, scratch, `{"interrupt_on_sigint":true}`)
	proc := startAgyFixture(t, scratch, agyFrozenArgs("m"))
	go func() { _, _ = io.Copy(io.Discard, proc.Stderr()) }()

	events := streamAgyEvents(proc)

	initEv, ok := <-events
	if !ok || initEv.Kind != EventKindInit {
		t.Fatalf("expected init event, got %+v ok=%v", initEv, ok)
	}

	sendRawStdinLine(t, proc, `{"event":"user","message":{"content":"count to 400"}}`)

	stepEv, ok := <-events
	if !ok || stepEv.Kind != EventKindStepUpdate || stepEv.Step.State != "ACTIVE" {
		t.Fatalf("expected an ACTIVE step_update before interrupting, got %+v ok=%v", stepEv, ok)
	}

	if err := proc.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	resultEv, ok := <-events
	if !ok {
		t.Fatalf("expected a terminal result after SIGINT")
	}
	if resultEv.Kind != EventKindResult || resultEv.Result.Status != "ERROR" || resultEv.Result.Error != "interrupted" {
		t.Fatalf("unexpected result: %+v", resultEv.Result)
	}
	if _, stillOpen := <-events; stillOpen {
		t.Fatalf("expected no further events after the interrupted result")
	}

	code, err := proc.Wait()
	if err != nil {
		t.Fatalf("proc.Wait: %v", err)
	}
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (§7.4)", code)
	}
}

func TestAgyFixture_FallbackConversationID(t *testing.T) {
	scratch := t.TempDir()
	pinned := "33333333-3333-4333-8333-000000000003"
	requested := "00000000-0000-0000-0000-000000000000"
	writeAgyScenario(t, scratch, `{"conversation_id":"`+pinned+`"}`)
	proc := startAgyFixture(t, scratch, agyFrozenArgs("m", "--conversation", requested))

	initEv, _, stderrText, code := runAgyFixture(t, proc, `{"event":"user","message":{"content":"hi"}}`)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (silent fallback, never an error)", code)
	}
	if initEv.ConversationID == requested {
		t.Fatalf("init.conversation_id must NOT equal the unknown requested id %q", requested)
	}
	if initEv.ConversationID != pinned {
		t.Fatalf("init.conversation_id = %q, want the fresh fallback id %q", initEv.ConversationID, pinned)
	}
	wantWarn := `warning: conversation "` + requested + `" not found`
	if !strings.Contains(stderrText, wantWarn) {
		t.Fatalf("stderr = %q, want it to contain %q", stderrText, wantWarn)
	}
}

func TestAgyFixture_PrintTimeoutMarker(t *testing.T) {
	scratch := t.TempDir()
	writeAgyScenario(t, scratch, `{"print_timeout_marker":true}`)
	proc := startAgyFixture(t, scratch, agyFrozenArgs("m"))

	_, rest, stderrText, code := runAgyFixture(t, proc, `{"event":"user","message":{"content":"go"}}`)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (print-timeout expiry is exit 0, §7.2)", code)
	}
	found := false
	for _, line := range strings.Split(stderrText, "\n") {
		if StderrMarker(line) == MarkerPrintTimeout {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a stderr line classified MarkerPrintTimeout, got: %q", stderrText)
	}
	if len(rest) == 0 {
		t.Fatalf("expected at least a terminal result after init")
	}
	last := rest[len(rest)-1]
	if last.Kind != EventKindResult || last.Result.Status != "ERROR" {
		t.Fatalf("unexpected result: %+v", last.Result)
	}
}

func TestAgyFixture_VersionAndModelsAndPluginList(t *testing.T) {
	scratch := t.TempDir()
	binDir := compileAgyFixture(t)
	fixturePath := filepath.Join(binDir, "agy")

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(fixturePath, args...)
		cmd.Dir = scratch
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run agy %v: %v: %s", args, err, out)
		}
		return string(out)
	}

	if got := strings.TrimSpace(run("--version")); got != "1.2.9" {
		t.Fatalf("--version = %q, want 1.2.9", got)
	}

	catalog := run("models")
	if !strings.Contains(catalog, "gpt-oss-120b-medium") {
		t.Fatalf("models catalog missing expected entry, got: %q", catalog)
	}

	plugins := strings.TrimSpace(run("plugin", "list"))
	const wantPlugins = `{"imports":[{"components":["hooks","skills"],"importedAt":"2026-09-02T19:59:12Z","name":"superpowers","source":"gemini-cli"}]}`
	if plugins != wantPlugins {
		t.Fatalf("plugin list = %q, want the canonical committed bytes %q", plugins, wantPlugins)
	}
}
