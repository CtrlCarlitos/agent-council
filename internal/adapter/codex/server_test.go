//go:build unix

package codex

// POSIX-scoped process evidence for the codex app-server child: the exact
// argv template through the real PolicyExecutor, the initialize handshake
// gate (attestation mismatch terminates the child, fail closed),
// pump-before-first-notification ordering at the process level, the
// detached launch context, graceful termination with drained pipes, and
// the creation-reservation route end to end over the fixture binary.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ── Test fixtures ───────────────────────────────────────────────────────

// lifecycleLaunchSource builds the exact `codex app-server` LaunchRequest
// rooted at the given neutral scratch directory (the child's cwd; the
// workspace root stays out of the child process plumbing).
type lifecycleLaunchSource struct {
	scratch string
	args    []string // override for template-violation evidence
}

func codexLifecycleProfile() storage.CanonicalProfile {
	return storage.CanonicalProfile{
		AlgoVersion:         "cprof-v3",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"codex"},
	}
}

func (s lifecycleLaunchSource) CodexAppServerLaunch(_ context.Context, sessionID adapter.SessionID) (execpolicy.LaunchRequest, error) {
	args := s.args
	if args == nil {
		args = append([]string(nil), CodexAppServerArgs...)
	}
	return execpolicy.LaunchRequest{
		RunID:     "run-codex-lc",
		SessionID: string(sessionID),
		Command:   "codex",
		Args:      args,
		Paths: workspace.WorkspacePaths{
			Root:   s.scratch,
			Config: s.scratch,
		},
		Profile: codexLifecycleProfile(),
	}, nil
}

// newLifecycleServer prepares PATH with the fixture binary and returns a
// CodexServer whose frozen policy expects exactly what the fixture
// reports (codexHome = $HOME/.codex with HOME=scratch; the host platform).
func newLifecycleServer(t *testing.T, scratch string, mutate ...func(*CodexLaunchPolicy, *lifecycleLaunchSource)) *CodexServer {
	t.Helper()
	binDir := compileCodexFixture(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))
	policy := CodexLaunchPolicy{
		ExpectedCodexHome: filepath.Join(scratch, ".codex"),
		PlatformOS:        runtime.GOOS,
		PlatformFamily:    "unix",
	}
	src := lifecycleLaunchSource{scratch: scratch}
	for _, fn := range mutate {
		if fn != nil {
			fn(&policy, &src)
		}
	}
	return NewCodexServer(execpolicy.New(), src, policy)
}

// startOK starts a child and guarantees teardown. starts a child and guarantees teardown.
func startOK(t *testing.T, m *CodexServer, sessionID adapter.SessionID) *codexChild {
	t.Helper()
	t.Cleanup(func() { m.stopAll(context.Background()) })
	child, err := m.start(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("start codex child: %v", err)
	}
	return child
}

// assertChildGone polls until the fixture's SIGTERM markers appear and the
// latest pid is reaped.
func assertChildGone(t *testing.T, scratch string, wantCount int) {
	t.Helper()
	lines := waitForFile(t, scratch, ".codex-fixture-terminated", 10*time.Second)
	if len(lines) < wantCount {
		t.Fatalf("expected %d terminated markers, got %v", wantCount, lines)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil {
		t.Fatalf("marker must carry the pid: %q", lines)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return // reaped: the process is gone
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d is still alive after termination", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ── Launch template ─────────────────────────────────────────────────────

// The launched child must observe the exact pinned argv template, and a
// mutated template must be rejected before any process starts.
func TestCodexServer_ExactArgvTemplate(t *testing.T) {
	scratch := t.TempDir()
	m := newLifecycleServer(t, scratch)
	startOK(t, m, "ses_cx_argv")

	got := waitForFile(t, scratch, ".codex-fixture-args", 10*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected exactly one child launch, got %v", got)
	}
	parts := strings.Split(got[0], "\x1f")
	if len(parts) != 3 || parts[0] != "app-server" || parts[1] != "--listen" || parts[2] != "stdio://" {
		t.Fatalf("argv template violation: %v", parts)
	}

	// A mutated shape fails closed before any process starts: the
	// forbidden ws:// listener shape is structurally rejected.
	scratch2 := t.TempDir()
	m2 := newLifecycleServer(t, scratch2, func(p *CodexLaunchPolicy, s *lifecycleLaunchSource) {
		s.args = []string{"app-server", "--listen", "ws://127.0.0.1:9999"}
	})
	_, err := m2.start(context.Background(), "ses_cx_bad")
	var tmpl *ErrLaunchTemplate
	if !errors.As(err, &tmpl) {
		t.Fatalf("expected ErrLaunchTemplate, got %T: %v", err, err)
	}
	if lines := readFixtureFile(t, scratch2, ".codex-fixture-args"); lines != nil {
		t.Fatalf("no child may start with a mutated template, launches: %v", lines)
	}
}

// ── Handshake gate ──────────────────────────────────────────────────────

// A platform attestation mismatch terminates the child and fails closed.
func TestCodexServer_HandshakePlatformMismatchTerminates(t *testing.T) {
	scratch := t.TempDir()
	m := newLifecycleServer(t, scratch, func(p *CodexLaunchPolicy, s *lifecycleLaunchSource) {
		if err := os.WriteFile(filepath.Join(scratch, ".codex-fixture-platform"), []byte("plan9"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	orig := handshakeTimeout
	handshakeTimeout = 5 * time.Second
	defer func() { handshakeTimeout = orig }()

	_, err := m.start(context.Background(), "ses_cx_plat")
	var attest *ErrHandshakeAttestation
	if !errors.As(err, &attest) {
		t.Fatalf("expected ErrHandshakeAttestation, got %T: %v", err, err)
	}
	if attest.Field != "platformOs" {
		t.Fatalf("mismatched field: %q", attest.Field)
	}
	m.mu.Lock()
	registered := len(m.children)
	m.mu.Unlock()
	if registered != 0 {
		t.Fatalf("failed handshake must not register the child, children=%d", registered)
	}
	assertChildGone(t, scratch, 1)
}

// A codexHome attestation mismatch terminates the child and fails closed.
func TestCodexServer_HandshakeCodexHomeMismatchTerminates(t *testing.T) {
	scratch := t.TempDir()
	m := newLifecycleServer(t, scratch, func(p *CodexLaunchPolicy, s *lifecycleLaunchSource) {
		p.ExpectedCodexHome = "/operator/provisioned/.codex"
	})
	orig := handshakeTimeout
	handshakeTimeout = 5 * time.Second
	defer func() { handshakeTimeout = orig }()

	_, err := m.start(context.Background(), "ses_cx_home")
	var attest *ErrHandshakeAttestation
	if !errors.As(err, &attest) {
		t.Fatalf("expected ErrHandshakeAttestation, got %T: %v", err, err)
	}
	if attest.Field != "codexHome" {
		t.Fatalf("mismatched field: %q", attest.Field)
	}
	assertChildGone(t, scratch, 1)
}

// ── Pump-before-first-notification, at the process level ────────────────

// The connection-setup notification (remoteControl/status/changed, emitted
// by the fixture BEFORE any request) must already be captured when start
// returns: the pump is armed before the handshake is sent.
func TestCodexServer_PumpArmedBeforeHandshake(t *testing.T) {
	scratch := t.TempDir()
	stageFixture(t, scratch, "handshake")
	m := newLifecycleServer(t, scratch)
	child := startOK(t, m, "ses_cx_arm")

	got := child.pump.GlobalNotifications()
	if len(got) != 1 || got[0].Method != "remoteControl/status/changed" {
		t.Fatalf("connection-setup notification must be captured by the armed pump, got %+v", got)
	}
	// The handshake itself was seen by the fixture exactly once.
	reqs := waitForFile(t, scratch, ".codex-fixture-requests", 10*time.Second)
	if len(reqs) != 1 || reqs[0] != "initialize" {
		t.Fatalf("handshake request log: %v", reqs)
	}
}

// ── Detached launch context ─────────────────────────────────────────────

// Cancelling the caller's context after start must not stop the child or
// the pump: the canceled call abandons its creation reservation (uncertain
// — recreation stays blocked), and the child keeps serving and routing.
func TestCodexServer_DetachedContext(t *testing.T) {
	scratch := t.TempDir()
	writeScenario(t, scratch,
		`{"emit_on_request": {"method":"thread/start","line":{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"01900000-0000-7000-8000-000000000001","sessionId":"01900000-0000-7000-8000-000000000001","status":{"type":"idle"}},"emittedAtMs":1760000000000}}}}`,
		`{"respond": {"method":"thread/start","delay_ms":500,"result":{"id":"01900000-0000-7000-8000-000000000001","sessionId":"01900000-0000-7000-8000-000000000001","status":{"type":"idle"},"model":"gpt-6-astra"}}}`,
		`{"respond": {"method":"thread/resume","result":{"id":"01900000-0000-7000-8000-00000000000f","status":{"type":"idle"},"cwd":"/fixture/workspace","approvalPolicy":"on-request","sandbox":{"type":"workspace-write"},"approvalsReviewer":"user","model":"gpt-6-astra","modelProvider":"openai","instructionSources":["user"]}}}`,
	)
	m := newLifecycleServer(t, scratch)
	child := startOK(t, m, "ses_cx_detach")

	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// A call bounded by the canceled caller ctx fails at the ack wait —
	// the write happened, so the creation reservation is abandoned
	// (uncertain) and recreation stays blocked on this child.
	_, err := child.client.ThreadStart(callerCtx, ThreadStartParams{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller ctx must bound the call, got %v", err)
	}
	if got := child.pump.CreationOutcome(); !errors.Is(got, ErrCreationAbandoned) {
		t.Fatalf("canceled creation must be recorded as abandoned/uncertain, got %v", got)
	}
	if _, err := child.client.ThreadStart(context.Background(), ThreadStartParams{}); !errors.Is(err, ErrCreationPending) {
		t.Fatalf("recreation after an uncertain creation must stay blocked, got %v", err)
	}

	// The child and pump are fully alive: the provider-free resume probe
	// completes and notifications still route through the pump.
	cfg, err := child.client.ResumeProbe("01900000-0000-7000-8000-00000000000f")
	if err != nil {
		t.Fatalf("resume probe after caller cancel: %v", err)
	}
	if cfg.Model != "gpt-6-astra" {
		t.Fatalf("resume config: %+v", cfg)
	}
	if poisoned, perr := child.pump.Poisoned(); poisoned {
		t.Fatalf("pump poisoned by the canceled caller: %v", perr)
	}
}

// ── Park / Close / drain ────────────────────────────────────────────────

// Close terminates the child gracefully (SIGTERM observed by the fixture),
// pipes stay drained under heavy stderr, and the child is not registered.
func TestCodexServer_CloseGracefulTermination(t *testing.T) {
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, ".codex-fixture-heavy-stderr"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newLifecycleServer(t, scratch)
	startOK(t, m, "ses_cx_close")

	m.stop(context.Background(), "ses_cx_close")
	assertChildGone(t, scratch, 1)
	m.mu.Lock()
	registered := len(m.children)
	parked := len(m.parked)
	m.mu.Unlock()
	if registered != 0 || parked != 0 {
		t.Fatalf("stop must deregister without parking: children=%d parked=%d", registered, parked)
	}

	// Park records the session for resume.
	child := startOK(t, m, "ses_cx_park")
	if err := m.park(context.Background(), "ses_cx_park"); err != nil {
		t.Fatalf("park: %v", err)
	}
	assertChildGone(t, scratch, 2)
	if !m.isParked("ses_cx_park") {
		t.Fatal("parked session must be recorded")
	}
	if child.client == nil || child.conn == nil || child.pump == nil {
		t.Fatal("child must expose the conn/pump/client seams")
	}
}

// ── Creation-reservation route, end to end over the fixture ─────────────

// thread/start with a notification-before-response fixture: the binding
// publishes only after response-id equality, the pending reservation is
// exclusive while in flight, and a late duplicate thread/started dedups.
func TestCodexServer_ThreadStartCreationReservation(t *testing.T) {
	scratch := t.TempDir()
	stageFixture(t, scratch, "thread-start")
	m := newLifecycleServer(t, scratch)
	child := startOK(t, m, "ses_cx_create")

	thread, err := child.client.ThreadStart(context.Background(), ThreadStartParams{})
	if err != nil {
		t.Fatalf("thread/start: %v", err)
	}
	const fixtureThread = "01900000-0000-7000-8000-000000000001"
	if thread.ID != fixtureThread || thread.SessionID != fixtureThread {
		t.Fatalf("thread identity: %+v", thread)
	}
	if thread.Status.Type != "idle" || thread.Model != "gpt-6-astra" {
		t.Fatalf("thread body: %+v", thread)
	}

	// The binding is published in the pump: a late duplicate
	// thread/started dedups instead of drifting.
	child.pump.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThread)))
	if poisoned, err := child.pump.Poisoned(); poisoned {
		t.Fatalf("late duplicate must dedup: %v", err)
	}

	// After confirmation the reservation is cleared: the next creation may
	// reserve (exclusivity during a pending creation is covered in the
	// portable pump evidence). Release it so teardown stays clean.
	if err := child.pump.ReserveCreation(child.conn.AllocateID()); err != nil {
		t.Fatalf("reservation after a confirmed creation must be possible, got %v", err)
	}
	child.pump.AbandonCreation()

	// Request evidence: exactly initialize + thread/start hit the child.
	reqs := waitForFile(t, scratch, ".codex-fixture-requests", 10*time.Second)
	if len(reqs) != 2 || reqs[0] != "initialize" || reqs[1] != "thread/start" {
		t.Fatalf("request log: %v", reqs)
	}
}

// The resume probe against the fixture returns the recorded effective
// config; the verbatim missing-thread error maps to the typed missing
// error through a live child.
func TestCodexServer_ResumeProbeLive(t *testing.T) {
	scratch := t.TempDir()
	stageFixture(t, scratch, "resume-positive")
	m := newLifecycleServer(t, scratch)
	child := startOK(t, m, "ses_cx_resume")

	cfg, err := child.client.ResumeProbe("01900000-0000-7000-8000-000000000002")
	if err != nil {
		t.Fatalf("resume probe: %v", err)
	}
	if cfg.ThreadID != "01900000-0000-7000-8000-000000000002" ||
		cfg.Model != "gpt-6-astra" || cfg.ModelProvider != "openai" ||
		cfg.ApprovalsReviewer != "user" || cfg.CWD != "/fixture/workspace" {
		t.Fatalf("effective config: %+v", cfg)
	}

	// Negative: a replacement child loads the recorded verbatim
	// missing-thread error and the typed mapping holds end to end.
	stageFixture(t, scratch, "resume-missing")
	child2 := startOK(t, m, "ses_cx_resume_neg")
	_, err = child2.client.ResumeProbe("00000000-0000-0000-0000-000000000001")
	var missing *ErrNativeSessionMissing
	if !errors.As(err, &missing) {
		t.Fatalf("expected ErrNativeSessionMissing, got %T: %v", err, err)
	}
}
