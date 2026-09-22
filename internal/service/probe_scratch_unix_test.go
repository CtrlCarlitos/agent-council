//go:build unix

package service

// POSIX construction-level and subprocess evidence for the probe scratch
// root: service construction enforces the configured root, and Probe
// runs end-to-end against a controlled stub binary. The portable
// validation evidence lives in probe_scratch_test.go.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Construction through the service must enforce the configured root:
// a missing root fails closed at NewServerWithAdapter.
func TestProbeScratchRoot_ConstructionRequiresRoot(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	for _, d := range []string{stateDir, wsBase} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lock, lockErr := AcquireServiceLock(stateDir)
	if lockErr != nil {
		t.Fatalf("lock: %v", lockErr)
	}
	t.Cleanup(func() { _ = lock.Release() })

	cfg := ServerConfig{
		StateDir:           stateDir,
		InstanceID:         "inst-probe-scratch-construction",
		AuthToken:          "tok",
		WorkspaceBaseDir:   wsBase,
		OpenCodeBinaryPath: "opencode",
	}
	if _, err := NewServerWithAdapter(store, lock, cfg, nil); err == nil {
		t.Fatal("construction without a configured probe scratch root must fail")
	}
}

// The configured scratch root must be usable end-to-end: construction
// through the production wiring path, then Probe against a controlled
// stub binary, with no probe artifacts left in the configured root.
func TestProbeScratchRoot_ProbeThroughConfiguredRoot(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	for _, d := range []string{stateDir, wsBase} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lock, lockErr := AcquireServiceLock(stateDir)
	if lockErr != nil {
		t.Fatalf("lock: %v", lockErr)
	}
	t.Cleanup(func() { _ = lock.Release() })

	binDir := compileStubOpencodeBinary(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	profile := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"opencode"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"opencode": {Model: "stub/model", NativeAuthMode: "managed_by_council"},
		},
	}

	scratchRoot := filepath.Join(dir, "probe-scratch")
	cfg := ServerConfig{
		StateDir:                 stateDir,
		InstanceID:               "inst-probe-e2e",
		AuthToken:                "tok-probe-e2e",
		WorkspaceBaseDir:         wsBase,
		OpenCodeBinaryPath:       "opencode",
		OpenCodeProbeScratchRoot: scratchRoot,
		OpenCodeProbeProfile:     profile,
	}
	srv, err := NewServerWithAdapter(store, lock, cfg, nil)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	report, err := srv.adapter.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe through configured root: %v", err)
	}
	if !report.HarnessVersion.Available || !strings.Contains(report.HarnessVersion.Value, "1.18.31-stub") {
		t.Fatalf("probe must capture the stub version, got %+v", report.HarnessVersion)
	}

	entries, err := os.ReadDir(scratchRoot)
	if err != nil {
		t.Fatalf("read configured root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("probe artifacts must be cleaned from the configured root, remaining: %v", names)
	}
}

// ── Controlled stub `opencode` binary (test fixture, mirrored from the
// adapter package's lifecycle tests; test-only, never production). ──

const opencodeStubServeSource = `package main

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
	opencodeStubOnce   sync.Once
	opencodeStubBinDir string
	opencodeStubErr    error
)

func compileStubOpencodeBinary(t *testing.T) string {
	t.Helper()
	opencodeStubOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ac007-svc-stub-*")
		if err != nil {
			opencodeStubErr = err
			return
		}
		srcDir := filepath.Join(dir, "src")
		if err := os.MkdirAll(srcDir, 0o700); err != nil {
			opencodeStubErr = err
			return
		}
		src := filepath.Join(srcDir, "main.go")
		if err := os.WriteFile(src, []byte(opencodeStubServeSource), 0o600); err != nil {
			opencodeStubErr = err
			return
		}
		name := "opencode"
		if runtime.GOOS == "windows" {
			name = "opencode.exe"
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, name), src)
		build.Dir = srcDir
		if out, err := build.CombinedOutput(); err != nil {
			opencodeStubErr = fmt.Errorf("build stub: %v: %s", err, out)
			return
		}
		opencodeStubBinDir = dir
	})
	if opencodeStubErr != nil {
		t.Fatalf("stub build: %v", opencodeStubErr)
	}
	return opencodeStubBinDir
}
