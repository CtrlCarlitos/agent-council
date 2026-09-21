//go:build !windows

package opencode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
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
	proc      execpolicy.ManagedProcess
	endpoint  string
	workspace string
	startedAt time.Time
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
	children map[string]*serverProcess // native session ID → child
}

func newServerManager(executor execpolicy.PolicyExecutor, launch SessionLaunchSource) *serverManager {
	return &serverManager{
		executor: executor,
		launch:   launch,
		children: make(map[string]*serverProcess),
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
	m.mu.Unlock()

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

	// Wait for the health endpoint.
	endpoint, err := m.waitForHealthy(ctx, proc, 15*time.Second)
	if err != nil {
		termCtx, termCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer termCancel()
		_ = proc.Terminate(termCtx)
		return nil, fmt.Errorf("opencode serve health: %w", err)
	}

	sp := &serverProcess{
		proc:      proc,
		endpoint:  endpoint,
		workspace: launchReq.Paths.Root,

		startedAt: time.Now().UTC(),
	}

	m.mu.Lock()
	m.children[string(sessionID)] = sp
	m.mu.Unlock()

	return sp, nil
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

// waitForHealthy polls the child's stdout for the printed listen address,
// then verifies GET /api/health. Returns the endpoint and PID.
func (m *serverManager) waitForHealthy(ctx context.Context, proc execpolicy.ManagedProcess, timeout time.Duration) (string, error) {
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
			if healthy := m.checkHealth(ctx, ep); healthy {
				return ep, nil
			}
		}
		select {
		case <-done:
			// Child exited; check one last time.
			if ep := scanEndpoint(buf.string()); ep != "" {
				return ep, nil
			}
			return "", fmt.Errorf("opencode serve exited before printing its endpoint")
		case <-deadline:
			return "", fmt.Errorf("opencode serve did not become healthy in %v", timeout)
		case <-ticker.C:
		}
	}
}

func (m *serverManager) checkHealth(ctx context.Context, endpoint string) bool {
	hc := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/health", nil)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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
