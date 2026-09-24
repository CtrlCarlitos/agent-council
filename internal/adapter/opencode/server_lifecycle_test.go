//go:build unix

package opencode

// POSIX-scoped process fixtures: these tests launch real stub children
// through PolicyExecutor and inspect signal-based termination. The portable
// HTTP/state-machine contract evidence lives in review_gate1_test.go with
// no build tags.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ── Stub `opencode` binary ──────────────────────────────────────────────

const stubServeSource = `package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func stateFile(name string) string { return name }

func appendLine(name, line string) {
	f, err := os.OpenFile(stateFile(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func readLines(name string) []string {
	b, err := os.ReadFile(stateFile(name))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func main() {
	user := os.Getenv("OPENCODE_SERVER_USERNAME")
	pass := os.Getenv("OPENCODE_SERVER_PASSWORD")

	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("opencode 1.18.31-stub")
		return
	}

	appendLine(".stub-launches", fmt.Sprintf("%d", os.Getpid()))

	if _, err := os.Stat(stateFile(".stub-unhealthy")); err == nil {
		fmt.Fprint(os.Stderr, "unhealthy stub starting\n")
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
		<-ch
		appendLine(".stub-terminated", fmt.Sprintf("%d", os.Getpid()))
		return
	}

	// Drain evidence: write far more than the 64KiB pipe buffer to stderr
	// before listening. An undrained parent blocks the child here forever.
	blob := strings.Repeat("x", 200<<10)
	fmt.Fprint(os.Stderr, blob)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Printf("listening on http://127.0.0.1:%d\n", ln.Addr().(*net.TCPAddr).Port)

	// In-memory message store: native session ID -> messages.
	var msgMu sync.Mutex
	messages := map[string][]map[string]any{}

	appendMessage := func(sessionID string, m map[string]any) {
		msgMu.Lock()
		defer msgMu.Unlock()
		messages[sessionID] = append(messages[sessionID], m)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"healthy": true})
	})
	mux.HandleFunc("GET /api/model", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "model", "providerID": "stub"}},
		})
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		n := len(readLines(".stub-sessions")) + 1
		id := fmt.Sprintf("ses_stub_%d", n)
		appendLine(".stub-sessions", id)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": id})
	})
	mux.HandleFunc("GET /session/{sessionID}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("sessionID")
		for _, l := range readLines(".stub-sessions") {
			if l == id {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"id": id})
				return
			}
		}
		http.Error(w, "not found", 404)
	})
	mux.HandleFunc("POST /session/{sessionID}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		// Drain the body first so a decode failure cannot mask the real
		// status behind a connection reset.
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			MessageID string   "json:\"messageID\""
			Parts     []struct {
				Type string "json:\"type\""
				Text string "json:\"text\""
			} "json:\"parts\""
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			appendLine(".stub-badjson", string(raw))
			http.Error(w, "bad json", 400)
			return
		}
		sessionID := r.PathValue("sessionID")
		parts := make([]map[string]any, 0, len(body.Parts))
		for _, p := range body.Parts {
			parts = append(parts, map[string]any{"type": p.Type, "text": p.Text})
		}
		appendMessage(sessionID, map[string]any{
			"info": map[string]any{"id": body.MessageID, "role": "user"}, "parts": parts,
		})
		// Script an assistant reply correlated by parentID.
		appendMessage(sessionID, map[string]any{
			"info": map[string]any{
				"id": "msg_asst_" + body.MessageID, "role": "assistant", "parentID": body.MessageID,
			},
			"parts": []map[string]any{{"type": "text", "text": "stub assistant response"}},
		})
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /session/{sessionID}/message", func(w http.ResponseWriter, r *http.Request) {
		msgMu.Lock()
		msgs := messages[r.PathValue("sessionID")]
		msgMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if msgs == nil {
			msgs = []map[string]any{}
		}
		json.NewEncoder(w).Encode(msgs)
	})
	mux.HandleFunc("GET /session/{sessionID}/message/{messageID}", func(w http.ResponseWriter, r *http.Request) {
		sessionID, messageID := r.PathValue("sessionID"), r.PathValue("messageID")
		msgMu.Lock()
		msgs := messages[sessionID]
		msgMu.Unlock()
		for _, m := range msgs {
			info := m["info"].(map[string]any)
			if info["id"] == messageID {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(m)
				return
			}
		}
		http.Error(w, "not found", 404)
	})

	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				appendLine(".stub-panic", fmt.Sprintf("%s %s: %v", r.Method, r.URL.Path, rec))
				panic(rec)
			}
		}()
		appendLine(".stub-hits", r.Method+" "+r.URL.Path)
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			appendLine(".stub-authfail", r.URL.Path)
			w.Header().Set("WWW-Authenticate", "Basic realm=\"opencode\"")
			http.Error(w, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{Handler: authed, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(ln); err != nil {
		os.Exit(0)
	}
}
`

var (
	stubOnce     sync.Once
	stubBinDir   string
	stubBuildErr error
)

// compileStubOpencode builds the stub `opencode` binary once per test
// binary and returns the directory to prepend to PATH.
func compileStubOpencode(t *testing.T) string {
	t.Helper()
	stubOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ac007-stub-*")
		if err != nil {
			stubBuildErr = err
			return
		}
		srcDir := filepath.Join(dir, "src")
		if err := os.MkdirAll(srcDir, 0o700); err != nil {
			stubBuildErr = err
			return
		}
		src := filepath.Join(srcDir, "main.go")
		if err := os.WriteFile(src, []byte(stubServeSource), 0o600); err != nil {
			stubBuildErr = err
			return
		}
		name := "opencode"
		if runtime.GOOS == "windows" {
			name = "opencode.exe"
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, name), src)
		build.Dir = srcDir
		if out, err := build.CombinedOutput(); err != nil {
			stubBuildErr = fmt.Errorf("build stub: %v: %s", err, out)
			return
		}
		stubBinDir = dir
	})
	if stubBuildErr != nil {
		t.Fatalf("stub build: %v", stubBuildErr)
	}
	return stubBinDir
}

// ── Test fixtures ───────────────────────────────────────────────────────

// lifecycleLaunchSource is a SessionLaunchSource producing a valid
// `opencode serve` LaunchRequest rooted at the given workspace directory.
type lifecycleLaunchSource struct{ root string }

func lifecycleProfile() storage.CanonicalProfile {
	return storage.CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"opencode"},
	}
}

func (s lifecycleLaunchSource) OpenCodeServeLaunch(_ context.Context, sessionID adapter.SessionID) (execpolicy.LaunchRequest, error) {
	return execpolicy.LaunchRequest{
		RunID:     "run-lc",
		SessionID: string(sessionID),
		Command:   "opencode",
		Args:      []string{"serve", "--hostname", "127.0.0.1", "--port", "0"},
		Paths: workspace.WorkspacePaths{
			Root:   s.root,
			Config: s.root,
		},
		Profile: lifecycleProfile(),
	}, nil
}

// newLifecycleManager prepares PATH with the stub binary and returns a
// server manager backed by a real PolicyExecutor.
func newLifecycleManager(t *testing.T, root string) *serverManager {
	t.Helper()
	binDir := compileStubOpencode(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))
	return newServerManager(execpolicy.New(), lifecycleLaunchSource{root: root})
}

func readStubFile(t *testing.T, root, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
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

// ── Lifecycle evidence ──────────────────────────────────────────────────

// Concurrent starts for one session must produce exactly one native child
// and every caller must receive the same endpoint.
func TestServerLifecycle_ConcurrentStartsSingleChild(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManager(t, root)
	defer m.stopAll(context.Background())

	const callers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		endpoints = make([]string, callers)
		errs      = make([]error, callers)
	)
	ctx := context.Background()
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sp, err := m.start(ctx, "ses_lc_conc")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[i] = err
				return
			}
			endpoints[i] = sp.endpoint
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if endpoints[i] != endpoints[0] {
			t.Fatalf("caller %d: endpoint mismatch %q != %q", i, endpoints[i], endpoints[0])
		}
	}

	launches := readStubFile(t, root, ".stub-launches")
	if len(launches) != 1 {
		t.Fatalf("expected exactly 1 native child process, got %d launches (%v)", len(launches), launches)
	}
}

// Generated credentials must be retained on the child record and actually
// accepted by the server: the stub enforces Basic auth on every endpoint
// and records every authentication failure.
func TestServerLifecycle_CredentialsRetainedAndUsed(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManager(t, root)
	defer m.stopAll(context.Background())

	sp, err := m.start(context.Background(), "ses_lc_auth")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if sp.username == "" || sp.password == "" {
		t.Fatal("generated credentials must be retained on the server process record")
	}
	if !strings.HasPrefix(sp.username, "opencode-") {
		t.Fatalf("username should carry the generated prefix, got %q", sp.username)
	}
	if sp.password == sp.username {
		t.Fatal("username and password must be independent values")
	}
	if fails := readStubFile(t, root, ".stub-authfail"); len(fails) != 0 {
		t.Fatalf("health check must authenticate, auth failures: %v", fails)
	}
}

// Stderr must be drained for the child's lifetime: the stub writes 200KiB
// to stderr before listening, which would deadlock an undrained pipe and
// prevent the health check from ever succeeding.
func TestServerLifecycle_StderrDrainedChildBecomesHealthy(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManager(t, root)
	defer m.stopAll(context.Background())

	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = m.start(context.Background(), "ses_lc_drain")
	}()
	select {
	case <-done:
	case <-time.After(healthTimeout + 10*time.Second):
		t.Fatal("start blocked: stderr pipe was not drained")
	}
	if err != nil {
		t.Fatalf("child with heavy stderr output must become healthy: %v", err)
	}
}

// Park must terminate the child and record the session; resume must start a
// replacement child against the same workspace and verify the exact native
// session still exists.
func TestServerLifecycle_ParkResumeReplacementVerifiesNativeSession(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManager(t, root)
	defer m.stopAll(context.Background())
	ctx := context.Background()

	sp1, err := m.start(ctx, "ses_lc_park")
	if err != nil {
		t.Fatalf("initial start: %v", err)
	}
	// Native session persisted server-side across the park.
	if err := os.WriteFile(filepath.Join(root, ".stub-sessions"), []byte("ses_lc_park\n"), 0o600); err != nil {
		t.Fatalf("seed native session: %v", err)
	}

	if err := m.park(ctx, "ses_lc_park"); err != nil {
		t.Fatalf("park: %v", err)
	}
	m.mu.Lock()
	parkedLen := len(m.parked)
	childrenLen := len(m.children)
	m.mu.Unlock()
	if parkedLen != 1 || childrenLen != 0 {
		t.Fatalf("after park: parked=%d children=%d, want 1/0", parkedLen, childrenLen)
	}

	sp2, err := m.start(ctx, "ses_lc_park")
	if err != nil {
		t.Fatalf("resume start: %v", err)
	}
	if sp2.endpoint == sp1.endpoint {
		t.Fatal("resume must start a replacement server, not reuse the parked one")
	}
	if sp2.workspace != sp1.workspace {
		t.Fatalf("replacement server must target the same workspace: %q != %q", sp2.workspace, sp1.workspace)
	}
	m.mu.Lock()
	childrenLen = len(m.children)
	parkedLen = len(m.parked)
	m.mu.Unlock()
	if childrenLen != 1 || parkedLen != 0 {
		t.Fatalf("after resume: children=%d parked=%d, want 1/0", childrenLen, parkedLen)
	}
}

// Resume must fail closed when the native session did not survive: the
// replacement child is terminated and never registered.
func TestServerLifecycle_ResumeFailsClosedWhenNativeSessionMissing(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManager(t, root)
	defer m.stopAll(context.Background())
	ctx := context.Background()

	if _, err := m.start(ctx, "ses_lc_gone"); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	if err := m.park(ctx, "ses_lc_gone"); err != nil {
		t.Fatalf("park: %v", err)
	}
	// Native session lost across the park.
	if err := os.WriteFile(filepath.Join(root, ".stub-sessions"), []byte("other-session\n"), 0o600); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	_, err := m.start(ctx, "ses_lc_gone")
	if err == nil {
		t.Fatal("resume with a missing native session must fail closed")
	}
	if !strings.Contains(err.Error(), "missing after resume") {
		t.Fatalf("expected missing-native-session error, got %v", err)
	}
	m.mu.Lock()
	childrenLen := len(m.children)
	m.mu.Unlock()
	if childrenLen != 0 {
		t.Fatalf("failed resume must not register a child, children=%d", childrenLen)
	}
}

// A child that never becomes healthy must be terminated and never
// registered. Termination is observed through the stub's SIGTERM handler
// marker and by confirming the process is gone.
func TestServerLifecycle_FailedHealthTerminatesChildAndDoesNotRegister(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".stub-unhealthy"), []byte("1"), 0o600); err != nil {
		t.Fatalf("seed unhealthy marker: %v", err)
	}
	m := newLifecycleManager(t, root)
	defer m.stopAll(context.Background())

	origTimeout := healthTimeout
	healthTimeout = 1200 * time.Millisecond
	defer func() { healthTimeout = origTimeout }()

	_, err := m.start(context.Background(), "ses_lc_dead")
	if err == nil {
		t.Fatal("start must fail when the child never becomes healthy")
	}
	m.mu.Lock()
	childrenLen := len(m.children)
	m.mu.Unlock()
	if childrenLen != 0 {
		t.Fatalf("failed health must not register a child, children=%d", childrenLen)
	}

	// The stub records its PID before doing anything else.
	launches := readStubFile(t, root, ".stub-launches")
	if len(launches) != 1 {
		t.Fatalf("expected exactly 1 child launch, got %v", launches)
	}
	var pid int
	if _, err := fmt.Sscanf(launches[0], "%d", &pid); err != nil {
		t.Fatalf("parse pid: %v", err)
	}

	// Graceful termination: the stub's SIGTERM handler writes a marker.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, ".stub-terminated")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child was not gracefully terminated after failed health check")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The process must be gone.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("child pid %d is still alive after failed health start", pid)
	}
}

// The endpoint scan and authenticated health check work against the real
// loopback listener: guard that the manager reports the endpoint through
// its accessor.
func TestServerLifecycle_EndpointAccessor(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManager(t, root)
	defer m.stopAll(context.Background())

	sp, err := m.start(context.Background(), "ses_lc_ep")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ep, err := m.endpoint("ses_lc_ep")
	if err != nil {
		t.Fatalf("endpoint accessor: %v", err)
	}
	if ep != sp.endpoint {
		t.Fatalf("endpoint mismatch: %q != %q", ep, sp.endpoint)
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(ep, "http://"))
	if err != nil || host != "127.0.0.1" || port == "" {
		t.Fatalf("endpoint must be a loopback host:port, got %q", ep)
	}

	// Unauthenticated requests to the live stub must fail with 401 and be
	// recorded, proving the served endpoint is genuinely credentialed.
	resp, err := http.Get(ep + "/api/health")
	if err != nil {
		t.Fatalf("plain GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated health must be 401, got %d", resp.StatusCode)
	}
	if fails := readStubFile(t, root, ".stub-authfail"); len(fails) == 0 {
		t.Fatal("unauthenticated request must be recorded as an auth failure")
	}
}
