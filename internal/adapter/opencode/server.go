package opencode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
)

// SessionLaunchSource is a service-owned seam that builds the complete
// session-specific LaunchRequest (frozen profile, validated workspace
// paths, run ID, session ID) from the persisted frozen run profile and the
// AC-005 workspace allocation. The adapter verifies the exact opencode
// serve shape and appends GeneratedServerEnv but never constructs policy
// inputs itself.
type SessionLaunchSource interface {
	OpenCodeServeLaunch(ctx context.Context, sessionID adapter.SessionID) (execpolicy.LaunchRequest, error)
}

// serverProcess tracks one running `opencode serve` child.
type serverProcess struct {
	proc       execpolicy.ManagedProcess
	endpoint   string
	workspace  string
	username   string
	password   string
	startedAt  time.Time
	stderrTail *boundedBuffer // last stderr bytes, for failure diagnostics
}

// serverManager owns all live `opencode serve` children for this adapter
// instance, keyed by native session ID. It provides:
//   - Start: launch a serve child via SessionLaunchSource + executor.
//   - Stop: graceful terminate → force kill (pipes drained).
//   - StopAll: service-shutdown cleanup.
//   - Endpoint: the HTTP endpoint for HTTP client calls.
type serverManager struct {
	mu       sync.Mutex
	executor execpolicy.PolicyExecutor
	launch   SessionLaunchSource
	children map[string]*serverProcess // child key (logical session) → child
	starting map[string]chan struct{}  // session ID → in-flight launch
	parked   map[string]parkedServer   // child key → parked record
	// nativeFor resolves the NATIVE session ID for a child key when a
	// persisted binding exists; resume verification must check the exact
	// native session, never the logical one.
	nativeFor func(adapter.SessionID) string
}

// parkedServer records a session whose serve child was parked after idle
// grace. Resume relaunches against the same workspace and verifies the
// exact native session survived.
type parkedServer struct {
	workspace string
	parkedAt  time.Time
}

// healthTimeout bounds how long a freshly launched serve child has to
// become healthy. Package-level so tests can shorten it.
var healthTimeout = 15 * time.Second

func newServerManager(executor execpolicy.PolicyExecutor, launch SessionLaunchSource) *serverManager {
	return &serverManager{
		executor: executor,
		launch:   launch,
		children: make(map[string]*serverProcess),
		starting: make(map[string]chan struct{}),
		parked:   make(map[string]parkedServer),
	}
}

// start launches a new `opencode serve` child for the session. It:
//  1. Asks the SessionLaunchSource for the complete LaunchRequest.
//  2. Verifies the exact `opencode serve` shape.
//  3. Appends GeneratedServerEnv.
//  4. Starts via PolicyExecutor.
//  5. Waits for the health endpoint.
//  6. Records the child.
func (m *serverManager) start(ctx context.Context, sessionID adapter.SessionID) (*serverProcess, error) {
	m.mu.Lock()
	if existing, ok := m.children[string(sessionID)]; ok {
		m.mu.Unlock()
		return existing, nil
	}
	if launching, ok := m.starting[string(sessionID)]; ok {
		m.mu.Unlock()
		<-launching // wait for the in-flight launch to complete
		m.mu.Lock()
		sp, ok := m.children[string(sessionID)]
		m.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("server launch for %s failed", sessionID)
		}
		return sp, nil
	}
	launchDone := make(chan struct{})
	m.starting[string(sessionID)] = launchDone
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.starting, string(sessionID))
		m.mu.Unlock()
		close(launchDone)
	}()

	launchReq, err := m.launch.OpenCodeServeLaunch(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session launch source: %w", err)
	}

	// Verify the exact opencode serve shape: the adapter does not construct
	// policy inputs, but it does validate the launch it is about to start.
	if !execpolicy.IsOpenCodeServeLaunch(launchReq) {
		return nil, fmt.Errorf("%w: session launch shape is %q %v", execpolicy.ErrServerEnvShape, launchReq.Command, launchReq.Args)
	}

	// Generate ephemeral server credentials.
	username, password, err := generateServerCredentials()
	if err != nil {
		return nil, fmt.Errorf("generate server credentials: %w", err)
	}
	launchReq.GeneratedServerEnv = &execpolicy.GeneratedServerEnv{Username: username, Password: password}

	proc, err := m.executor.Start(ctx, launchReq)
	if err != nil {
		return nil, fmt.Errorf("start opencode serve: %w", err)
	}

	// Drain stderr for the child's lifetime: an undrained pipe eventually
	// blocks the child. The tail is kept for failure diagnostics.
	stderrTail := newBoundedBuffer(8 << 10)
	go func() { _, _ = io.Copy(stderrTail, proc.Stderr()) }()

	// Wait for the health endpoint (authenticated with the generated
	// credentials, mirroring the real server's Basic-auth requirement).
	endpoint, err := m.waitForHealthy(ctx, proc, healthTimeout, username, password)
	if err != nil {
		termCtx, termCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer termCancel()
		_ = proc.Terminate(termCtx)
		return nil, fmt.Errorf("opencode serve health: %w", err)
	}

	// Resume semantics: when this session was parked, the replacement
	// server must still expose the exact native session — resolved
	// through the binding, never assumed to equal the logical ID.
	m.mu.Lock()
	_, wasParked := m.parked[string(sessionID)]
	nativeFor := m.nativeFor
	m.mu.Unlock()
	if wasParked {
		verifyID := string(sessionID)
		if nativeFor != nil {
			verifyID = nativeFor(sessionID)
		}
		if err := m.verifyNativeSession(ctx, endpoint, username, password, verifyID); err != nil {
			termCtx, termCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer termCancel()
			_ = proc.Terminate(termCtx)
			return nil, fmt.Errorf("resume verification for %s: %w", sessionID, err)
		}
		m.mu.Lock()
		delete(m.parked, string(sessionID))
		m.mu.Unlock()
	}

	sp := &serverProcess{
		proc:       proc,
		endpoint:   endpoint,
		workspace:  launchReq.Paths.Root,
		username:   username,
		password:   password,
		startedAt:  time.Now().UTC(),
		stderrTail: stderrTail,
	}

	m.mu.Lock()
	m.children[string(sessionID)] = sp
	m.mu.Unlock()

	return sp, nil
}

// park stops the session's serve child after idle grace and records the
// parked session so the next start relaunches against the same workspace
// and verifies the native session still exists.
func (m *serverManager) park(ctx context.Context, sessionID adapter.SessionID) error {
	m.mu.Lock()
	sp, ok := m.children[string(sessionID)]
	if ok {
		delete(m.children, string(sessionID))
		m.parked[string(sessionID)] = parkedServer{
			workspace: sp.workspace,
			parkedAt:  time.Now().UTC(),
		}
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no serve child to park for session %s", sessionID)
	}
	termCtx, termCancel := context.WithTimeout(ctx, 5*time.Second)
	defer termCancel()
	if sp.proc == nil {
		// A child without a managed process (injected fixture) has
		// nothing to terminate.
		return nil
	}
	return sp.proc.Terminate(termCtx)
}

// verifyNativeSession checks that the resumed server still exposes the
// exact native session. A verified 404 is a hard resume failure.
func (m *serverManager) verifyNativeSession(ctx context.Context, endpoint, username, password, nativeID string) error {
	vctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// The resumed server's session metadata is not bound to a specific
	// directory here: the manager keys children by the session's own
	// child key and the adapter verifies project context at
	// ResumeSession. Existence is the lifecycle fact this check owns.
	_, exists, err := newNativeClient(endpoint, username, password).GetSession(vctx, nativeID)
	if err != nil {
		return fmt.Errorf("native session lookup: %w", err)
	}
	if !exists {
		return fmt.Errorf("native session %s is missing after resume", nativeID)
	}
	return nil
}

// stop terminates a specific session's serve child.
func (m *serverManager) stop(ctx context.Context, sessionID adapter.SessionID) error {
	m.mu.Lock()
	sp, ok := m.children[string(sessionID)]
	if ok {
		delete(m.children, string(sessionID))
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	termCtx, termCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer termCancel()
	return sp.proc.Terminate(termCtx)
}

// stopAll terminates all serve children (service shutdown).
func (m *serverManager) stopAll(ctx context.Context) {
	m.mu.Lock()
	children := make(map[string]*serverProcess, len(m.children))
	for k, v := range m.children {
		children[k] = v
		delete(m.children, k)
	}
	m.mu.Unlock()
	for _, sp := range children {
		termCtx, termCancel := context.WithTimeout(ctx, 5*time.Second)
		_ = sp.proc.Terminate(termCtx)
		termCancel()
	}
}

// child returns the server process for a session.
func (m *serverManager) child(sessionID string) (*serverProcess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sp, ok := m.children[sessionID]
	if !ok {
		return nil, fmt.Errorf("no serve child for session %s", sessionID)
	}
	return sp, nil
}

// endpoint returns the HTTP endpoint for a session's serve child.
func (m *serverManager) endpoint(sessionID adapter.SessionID) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sp, ok := m.children[string(sessionID)]
	if !ok {
		return "", fmt.Errorf("no serve child for session %s", sessionID)
	}
	return sp.endpoint, nil
}

// isParked reports whether the session has a parked (stopped but
// resumable) serve child.
func (m *serverManager) isParked(sessionID adapter.SessionID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.parked[string(sessionID)]
	return ok
}

// waitForHealthy polls the child's stdout for the printed listen address,
// then verifies GET /api/health with the generated credentials. Returns the
// endpoint.
func (m *serverManager) waitForHealthy(ctx context.Context, proc execpolicy.ManagedProcess, timeout time.Duration, username, password string) (string, error) {
	var buf syncBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(proc.Stdout())
		for sc.Scan() {
			buf.append(sc.Text() + "\n")
		}
	}()

	deadline := time.After(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		out := buf.string()
		ep := scanEndpoint(out)
		if ep != "" {
			if m.checkHealth(ctx, ep, username, password) {
				return ep, nil
			}
		}
		select {
		case <-done:
			// Child exited; check one last time.
			if ep := scanEndpoint(buf.string()); ep != "" && m.checkHealth(ctx, ep, username, password) {
				return ep, nil
			}
			return "", fmt.Errorf("opencode serve exited before becoming healthy")
		case <-deadline:
			return "", fmt.Errorf("opencode serve did not become healthy in %v", timeout)
		case <-ticker.C:
		}
	}
}

func (m *serverManager) checkHealth(ctx context.Context, endpoint, username, password string) bool {
	hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return newNativeClient(endpoint, username, password).Health(hctx) == nil
}

// generateServerCredentials generates an ephemeral username/password pair
// for the opencode serve child.
func generateServerCredentials() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate server credentials: %w", err)
	}
	h := hex.EncodeToString(buf)
	return "opencode-" + h[:16], h[16:], nil
}

// syncBuffer is a thread-safe string buffer for stdout capture.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
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

func (b *syncBuffer) append(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.WriteString(s)
}

func (b *syncBuffer) string() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
