package codex

// Child lifecycle for the codex app-server contributor session (AC-009
// spec §3.2, AC-007 pattern): one `codex app-server` child per logical
// session, launched ONLY through the AC-005 PolicyExecutor with the exact
// pinned argv template, running on an adapter-owned detached context so
// caller cancellation never kills the child or the pump. The initialize
// handshake gates readiness and compares codexHome/platform against the
// frozen profile — mismatch terminates the child, fail closed. Park after
// idle grace; Close terminates gracefully then kills with pipes drained
// before the process is waited on.

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
)

// CouncilClientName is the initialize clientInfo.name Council presents.
const CouncilClientName = "agent-council"

// CodexAppServerArgs is the exact argv template pinned by the frozen
// launch contract (spec §3.2): the explicit stdio listen form so the
// executor's template validation has one shape. ws:// and unix://
// listeners and auth-token flags are structurally absent.
var CodexAppServerArgs = []string{"app-server", "--listen", "stdio://"}

// ErrLaunchTemplate reports a launch request whose argv deviates from the
// pinned template. The adapter never starts a child with a mutated shape.
type ErrLaunchTemplate struct {
	Command string
	Args    []string
}

func (e *ErrLaunchTemplate) Error() string {
	return fmt.Sprintf("codex app-server launch must be exactly [app-server --listen stdio://], got %q %v", e.Command, e.Args)
}

// ErrHandshakeAttestation reports an initialize result whose environment
// attestation (codexHome / platformOs / platformFamily) disagrees with the
// frozen profile. The child is terminated: fail closed, pre-session.
type ErrHandshakeAttestation struct {
	Field string
	Want  string
	Have  string
}

func (e *ErrHandshakeAttestation) Error() string {
	return fmt.Sprintf("initialize attestation mismatch for %s: frozen profile expects %q, child reports %q", e.Field, e.Want, e.Have)
}

// ChildLaunchSource is the service-owned seam that builds the complete
// session-specific LaunchRequest from the persisted frozen run profile and
// the AC-005 allocation. The adapter verifies the exact app-server shape
// but never constructs policy inputs itself. The child's working directory
// is the Council-owned neutral scratch directory carried in Paths.Root
// (the executor pins cmd.Dir there) — never a workspace root. CODEX_HOME
// is never set by the adapter: the environment is the inherited allowlist.
type ChildLaunchSource interface {
	CodexAppServerLaunch(ctx context.Context, sessionID adapter.SessionID) (execpolicy.LaunchRequest, error)
}

// IsCodexAppServerLaunch verifies the exact pinned launch shape.
func IsCodexAppServerLaunch(req execpolicy.LaunchRequest) bool {
	if req.Command == "" || filepath.Base(req.Command) != "codex" {
		return false
	}
	if len(req.Args) != len(CodexAppServerArgs) {
		return false
	}
	for i := range CodexAppServerArgs {
		if req.Args[i] != CodexAppServerArgs[i] {
			return false
		}
	}
	return true
}

var (
	// handshakeTimeout bounds the initialize gate on a freshly launched
	// child. Package-level so tests can shorten it.
	handshakeTimeout = 15 * time.Second
	// terminateGrace bounds the graceful phase of child termination
	// before the force-kill split (AC-005 terminate split).
	terminateGrace = 5 * time.Second
)

// codexChild tracks one running app-server child: its process, the
// JSON-RPC connection, the armed pump, and the typed client.
type codexChild struct {
	key        string
	proc       execpolicy.ManagedProcess
	conn       *Conn
	pump       *EventPump
	client     *CodexClient
	handshake  InitializeResult
	stderrTail *boundedBuffer // last stderr bytes, for failure diagnostics
	startedAt  time.Time
	stderrDone chan struct{}
}

// CodexServer owns all live app-server children for this adapter
// instance, keyed by the logical session ID.
type CodexServer struct {
	executor execpolicy.PolicyExecutor
	launch   ChildLaunchSource
	policy   CodexLaunchPolicy

	mu       sync.Mutex
	children map[string]*codexChild
	starting map[string]chan struct{}
	parked   map[string]time.Time
}

// NewCodexServer wires the child manager to an executor, a launch source,
// and the frozen launch policy (handshake attestation compare).
func NewCodexServer(executor execpolicy.PolicyExecutor, launch ChildLaunchSource, policy CodexLaunchPolicy) *CodexServer {
	return &CodexServer{
		executor: executor,
		launch:   launch,
		policy:   policy,
		children: make(map[string]*codexChild),
		starting: make(map[string]chan struct{}),
		parked:   make(map[string]time.Time),
	}
}

// start launches (or returns the already-running) child for the session.
// The caller's context is detached before anything long-lived is bound to
// it: the child process and the pump belong to the adapter's session
// lifecycle, never to a Dispatch/Observe call.
func (m *CodexServer) start(ctx context.Context, sessionID adapter.SessionID) (*codexChild, error) {
	key := string(sessionID)
	m.mu.Lock()
	if existing, ok := m.children[key]; ok {
		m.mu.Unlock()
		return existing, nil
	}
	if launching, ok := m.starting[key]; ok {
		m.mu.Unlock()
		<-launching
		m.mu.Lock()
		sp, ok := m.children[key]
		m.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("codex child launch for %s failed", sessionID)
		}
		return sp, nil
	}
	launchDone := make(chan struct{})
	m.starting[key] = launchDone
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.starting, key)
		m.mu.Unlock()
		close(launchDone)
	}()

	launchReq, err := m.launch.CodexAppServerLaunch(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("child launch source: %w", err)
	}
	if !IsCodexAppServerLaunch(launchReq) {
		return nil, &ErrLaunchTemplate{Command: launchReq.Command, Args: launchReq.Args}
	}

	// Detached adapter-owned launch context (spec §3.2): a canceled
	// caller ctx must not kill a healthy child. Everything long-lived —
	// the process and the pump — hangs off this context family only.
	startCtx := context.WithoutCancel(ctx)

	proc, err := m.executor.Start(startCtx, launchReq)
	if err != nil {
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}

	// stderr is drained once from process start into a bounded tail and
	// never parsed: an undrained pipe eventually blocks the child.
	stderrTail := newBoundedBuffer(8 << 10)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderrTail, proc.Stderr())
	}()

	// Pump-before-first-notification ordering (spec §3.2): the framing
	// reader, route tables, and buffers are armed BEFORE the handshake —
	// the first request — so connection-setup notifications (e.g.
	// remoteControl/status/changed) are never lost.
	conn := NewConn(proc.Stdin(), proc.Stdout())
	pump := NewEventPump()
	conn.SetNotificationHandler(pump.HandleNotification)
	onFatal := func(cause error) { m.poison(key, cause) }
	conn.SetFatalHandler(onFatal)
	pump.SetFatalHandler(onFatal)
	conn.Start()

	child := &codexChild{
		key:        key,
		proc:       proc,
		conn:       conn,
		pump:       pump,
		client:     NewCodexClient(conn, pump),
		stderrTail: stderrTail,
		startedAt:  time.Now().UTC(),
		stderrDone: stderrDone,
	}

	// Handshake gate: initialize with the Council clientInfo; the result's
	// codexHome/platform are compared against the frozen profile.
	if err := m.handshake(ctx, child); err != nil {
		m.terminate(key, child, err)
		return nil, err
	}

	m.mu.Lock()
	m.children[key] = child
	delete(m.parked, key)
	m.mu.Unlock()
	return child, nil
}

// handshake runs the initialize gate and freezes the attestation result on
// the child. Any failure or mismatch fails closed with a typed error.
func (m *CodexServer) handshake(ctx context.Context, child *codexChild) error {
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handshakeTimeout)
	defer cancel()
	init, err := child.client.Initialize(hctx, ClientInfo{
		Name:    CouncilClientName,
		Version: buildVersion(),
	})
	if err != nil {
		return fmt.Errorf("codex initialize handshake: %w (stderr tail: %q)", err, child.stderrTail.String())
	}
	child.handshake = init
	if init.CodexHome != m.policy.ExpectedCodexHome {
		return &ErrHandshakeAttestation{Field: "codexHome", Want: m.policy.ExpectedCodexHome, Have: init.CodexHome}
	}
	if init.PlatformOS != m.policy.PlatformOS {
		return &ErrHandshakeAttestation{Field: "platformOs", Want: m.policy.PlatformOS, Have: init.PlatformOS}
	}
	if init.PlatformFamily != m.policy.PlatformFamily {
		return &ErrHandshakeAttestation{Field: "platformFamily", Want: m.policy.PlatformFamily, Have: init.PlatformFamily}
	}
	return nil
}

// poison reacts to protocol drift (pump or conn fatal): the child is
// terminated and never silently resumable — the adapter layer records the
// uncertainty (spec §3.9).
func (m *CodexServer) poison(key string, cause error) {
	m.mu.Lock()
	child, ok := m.children[key]
	delete(m.children, key)
	m.mu.Unlock()
	if !ok {
		return
	}
	m.terminate(key, child, cause)
}

// terminate stops one child: close the connection (fail pending calls),
// graceful terminate → force kill, pipes drained before the process is
// considered done.
func (m *CodexServer) terminate(key string, child *codexChild, cause error) {
	child.conn.Close()
	termCtx, cancel := context.WithTimeout(context.Background(), terminateGrace)
	defer cancel()
	_ = child.proc.Terminate(termCtx)
	<-child.stderrDone
	_ = cause // recorded by callers that surface it
}

// park stops the session's child after idle grace and records the parked
// session; resume starts a replacement child and re-verifies the binding
// provider-free (spec §3.2/§3.4 — the adapter layer owns ResumeSession).
func (m *CodexServer) park(ctx context.Context, sessionID adapter.SessionID) error {
	key := string(sessionID)
	m.mu.Lock()
	child, ok := m.children[key]
	if ok {
		delete(m.children, key)
		m.parked[key] = time.Now().UTC()
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no codex child to park for session %s", sessionID)
	}
	m.terminate(key, child, nil)
	return nil
}

// stop terminates a specific session's child without a parked record.
func (m *CodexServer) stop(ctx context.Context, sessionID adapter.SessionID) {
	key := string(sessionID)
	m.mu.Lock()
	child, ok := m.children[key]
	if ok {
		delete(m.children, key)
	}
	m.mu.Unlock()
	if ok {
		m.terminate(key, child, nil)
	}
}

// stopAll terminates every child (service shutdown).
func (m *CodexServer) stopAll(ctx context.Context) {
	m.mu.Lock()
	children := make([]*codexChild, 0, len(m.children))
	for k, v := range m.children {
		children = append(children, v)
		delete(m.children, k)
	}
	m.mu.Unlock()
	for _, child := range children {
		m.terminate(child.key, child, nil)
	}
}

// child returns the running child for a session.
func (m *CodexServer) child(sessionID adapter.SessionID) (*codexChild, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	child, ok := m.children[string(sessionID)]
	if !ok {
		return nil, fmt.Errorf("no codex child for session %s", sessionID)
	}
	return child, nil
}

// isParked reports whether the session has a parked (stopped but
// resumable) child.
func (m *CodexServer) isParked(sessionID adapter.SessionID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.parked[string(sessionID)]
	return ok
}

// buildVersion reports the build version presented as clientInfo.version.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		v := strings.TrimSpace(info.Main.Version)
		if v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// boundedBuffer keeps the most recent maxLen bytes. Used for the child's
// stderr tail so failures carry diagnostics without unbounded memory.
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
