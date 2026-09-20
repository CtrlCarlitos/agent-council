# AC-003 Service Lifetime & Local Control Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the Council Go service independently of CLI/MCP client connection lifetime over an authenticated local control boundary, ensuring client disconnection never terminates authorized work and service restart honestly preserves execution uncertainty without fabricating completion or redispatching unprompted.

**Architecture:** A foreground service core (`council service run`) manages state directory exclusivity via `syscall.Flock`, initializes SQLite storage, binds an owner-restricted UNIX domain socket (`council.sock`, mode `0600`), and serves HTTP/1.1 JSON commands and SSE observations with Bearer token authentication (`auth.token`). A detached background launcher (`council service start`) invokes the core with `setsid` on Linux/WSL and verifies authenticated readiness.

**Tech Stack:** Go 1.25.0, SQLite (modernc.org/sqlite v1.59.0 via database/sql), `net/http`, `syscall.Flock`, `crypto/rand`.

**Spec:** [`docs/superpowers/specs/2026-09-19-ac-003-service-lifetime-design.md`](file:///home/carlitos/projects/CtrlCarlitos/agent-council/docs/superpowers/specs/2026-09-19-ac-003-service-lifetime-design.md)

## Global Constraints

- Go 1.25.0 baseline across `go.mod`, CI workflows, and documentation.
- Dual CI verification: pure-Go (`CGO_ENABLED=0`) and race detector (`CGO_ENABLED=1 -race -count=3`).
- Linux/WSL first; native Windows service commands return clear unsupported-platform errors.
- Strict filesystem permissions: state directory `0700`; `service.lock`, `service.json`, `auth.token`, `council.sock` mode `0600`.
- `service.lock` is never unlinked or replaced on cleanup; handle closed last.
- Client disconnect never terminates accepted workers or blocks persistence.
- Restart detects unfinished work without inventing completion or redispatching unprompted.
- No "fake" adapter registered in production registry (use test-only assembly).

## Review Focus

1. **Concurrent Duplicate Release During Draining**: An authorized release retry arriving while the service is draining must return the original committed receipt without dispatching a second worker.
2. **Crash Between Reservation Commit & Worker Dispatch**: Terminating the service process immediately after SQLite reservation commit leaves the turn in an unresolved state upon restart, without fabricating worker survival or redispatching.
3. **Slow / Disconnected SSE Observer**: A client dropping connection or stalling on the SSE stream must be disconnected via write timeout without cancelling the worker or delaying database commits.
4. **Signal Drain Grace Expiry & Forced Termination**: Expiration of the 15-second signal drain grace period triggers bounded termination handling, preserving unconfirmed outcomes as unresolved in SQLite without early lock release or fabricated outcomes.
5. **Losing Startup Process Cleanliness**: A competing startup attempt failing to acquire `service.lock` must fail immediately and touch none of the winning service instance's runtime files (`service.json`, `auth.token`, `council.sock`).

---

### Task 1: Storage Bridge, Exclusivity Locking, and Discovery Metadata

**Files:**
- Modify: `internal/storage/transitions.go`
- Modify: `internal/storage/session_store.go`
- Create: `internal/service/lock.go`
- Create: `internal/service/discovery.go`
- Test: `internal/service/lock_test.go`
- Test: `internal/service/discovery_test.go`

**Interfaces:**
- Consumes: `storage.Store`, `storage.ReleaseReceipt`
- Produces:
  - `storage.ReleaseDisposition`: `ReleaseDispositionNew`, `ReleaseDispositionReplayed`
  - `storage.ReleaseResult`: `{ Receipt ReleaseReceipt, Disposition ReleaseDisposition }`
  - `service.AcquireServiceLock(stateDir string) (*ServiceLock, error)`
  - `service.ServiceLock.Release() error`
  - `service.PublishDiscovery(stateDir string, meta DiscoveryMeta, token string) error`
  - `service.CleanupDiscovery(stateDir string) error`

- [ ] **Step 1: Write failing tests for ReleaseDisposition and ServiceLock**

Create `internal/service/lock_test.go`:
```go
package service

import (
	"path/filepath"
	"testing"
)

func TestServiceLock_Exclusivity(t *testing.T) {
	dir := t.TempDir()
	l1, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("first lock acquisition failed: %v", err)
	}
	defer l1.Release()

	// Second acquisition must fail with ErrServiceAlreadyRunning
	_, err = AcquireServiceLock(dir)
	if err != ErrServiceAlreadyRunning {
		t.Fatalf("expected ErrServiceAlreadyRunning, got: %v", err)
	}

	// Release first lock, then second must succeed
	if err := l1.Release(); err != nil {
		t.Fatalf("first release failed: %v", err)
	}

	l2, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("second lock acquisition failed after release: %v", err)
	}
	defer l2.Release()

	// Ensure lock file is never deleted
	lockPath := filepath.Join(dir, "service.lock")
	if !fileExists(lockPath) {
		t.Fatal("service.lock must remain on disk after release")
	}
}
```

Create `internal/service/discovery_test.go`:
```go
package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscovery_SeparationAndPermissions(t *testing.T) {
	dir := t.TempDir()
	meta := DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      "inst-test-1",
		PID:             os.Getpid(),
		Transport:       "unix",
		Endpoint:        filepath.Join(dir, "council.sock"),
		StateDir:        dir,
	}
	token := "0123456789abcdef0123456789abcdef"

	if err := PublishDiscovery(dir, meta, token); err != nil {
		t.Fatalf("publish discovery failed: %v", err)
	}

	// Check auth.token exists with mode 0600
	tokenPath := filepath.Join(dir, "auth.token")
	info, err := os.Stat(tokenPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("auth.token permissions not 0600: %v", info.Mode().Perm())
	}

	// Check service.json exists with mode 0600 and does NOT contain token
	metaPath := filepath.Join(dir, "service.json")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read service.json: %v", err)
	}
	if string(metaBytes) == "" || bytesContains(metaBytes, []byte(token)) {
		t.Fatal("service.json must not leak auth.token")
	}

	// Cleanup removes service.json, auth.token, council.sock
	if err := CleanupDiscovery(dir); err != nil {
		t.Fatalf("cleanup discovery failed: %v", err)
	}
	if fileExists(tokenPath) || fileExists(metaPath) {
		t.Fatal("expected discovery and token files to be unlinked")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/service`
Expected: FAIL (compilation error, types not implemented).

- [ ] **Step 3: Implement ReleaseDisposition in storage and ServiceLock/Discovery in service**

Modify `internal/storage/types.go` and `internal/storage/transitions.go`:
Add:
```go
type ReleaseDisposition string

const (
	ReleaseDispositionNew      ReleaseDisposition = "new"
	ReleaseDispositionReplayed ReleaseDisposition = "replayed"
)

type ReleaseResult struct {
	Receipt     ReleaseReceipt
	Disposition ReleaseDisposition
}
```
Update `ReleaseTurn` to return `(ReleaseResult, error)` (or `ReleaseTurnResult`), checking if the receipt was retrieved from idempotent lookup (`ReleaseDispositionReplayed`) versus newly inserted (`ReleaseDispositionNew`).

Create `internal/service/lock.go` implementing `flock(LOCK_EX|LOCK_NB)` with `FD_CLOEXEC` on Unix.
Create `internal/service/discovery.go` implementing token generation, atomic JSON writing, path validation, and cleanup.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/service ./internal/storage`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage internal/service
git commit -m "feat(service): implement storage release disposition, exclusivity locking, and discovery metadata"
```

---

### Task 2: HTTP Service Core, Auth Middleware, and Diagnostic Endpoints

**Files:**
- Create: `internal/service/server.go`
- Create: `internal/service/auth.go`
- Create: `internal/service/error.go`
- Test: `internal/service/server_test.go`

**Interfaces:**
- Consumes: `storage.Store`, `ServiceLock`, `DiscoveryMeta`
- Produces:
  - `service.NewServer(store *storage.Store, lock *ServiceLock, cfg ServerConfig) (*Server, error)`
  - `service.Server.Start() error`
  - `service.Server.Close() error`
  - Endpoints: `GET /v1/readiness`, `GET /v1/status`

- [ ] **Step 1: Write failing tests for HTTP server, Bearer auth, readiness, and status**

Create `internal/service/server_test.go`:
```go
package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestServer_ReadinessAndStatus(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	srv, err := NewServer(store, dir)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := srv.Client()

	// 1. Unauthenticated request must return 401 with WWW-Authenticate
	resp, err := client.Get("/v1/readiness")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %v, err: %v", resp.StatusCode, err)
	}
	if resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("missing WWW-Authenticate header")
	}

	// 2. Authenticated readiness request must return 200 ready
	req, _ := http.NewRequest("GET", "/v1/readiness", nil)
	req.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got: %v", resp.StatusCode)
	}

	var r ReadinessResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if r.Status != "ready" || r.InstanceID != srv.InstanceID() {
		t.Fatalf("unexpected readiness response: %+v", r)
	}

	// 3. Authenticated status request returns diagnostics
	req, _ = http.NewRequest("GET", "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got: %v", resp.StatusCode)
	}
	var s StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if s.Status != "ready" || s.LiveWorkers != 0 {
		t.Fatalf("unexpected status response: %+v", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestServer_ReadinessAndStatus ./internal/service`
Expected: FAIL (compilation error, types not implemented).

- [ ] **Step 3: Implement HTTP Server, Auth Middleware, and Error Envelope**

Implement `internal/service/auth.go`:
- Bearer token validation middleware comparing `Authorization: Bearer <token>` with cached in-memory token.
- Returns standard JSON error envelope on failure.

Implement `internal/service/error.go`:
- Helper `writeError(w http.ResponseWriter, status int, code, message, opID string)` writing uniform JSON envelope.
- `http.MaxBytesReader` bounding.

Implement `internal/service/server.go`:
- Binds `net.Listen("unix", socketPath)`.
- Handles `GET /v1/readiness` and `GET /v1/status`.
- Supervizes `server.Serve(listener)` in a background goroutine.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/service`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service
git commit -m "feat(service): implement HTTP service core, auth middleware, and diagnostic endpoints"
```

---

### Task 3: Release Admission Gate & Retry-Safe Dispatch Coordinator

**Files:**
- Create: `internal/service/coordinator.go`
- Create: `internal/service/release.go`
- Test: `internal/service/release_test.go`

**Interfaces:**
- Consumes: `storage.Store`, `adapter.Adapter`
- Produces:
  - `service.Coordinator` (manages admission gate, live workers, execution contexts)
  - `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/release`
  - Worker execution decoupling: worker runs under `service.Context`, not `r.Context()`.

- [ ] **Step 1: Write failing test for release hand-off, retry safety, and draining admission**

Create `internal/service/release_test.go`:
```go
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestRelease_IdempotentRetryAndDraining(t *testing.T) {
	dir := t.TempDir()
	store, _ := storage.Open(storage.StoreOptions{StateDir: dir})
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "run-1", "sess-1", "claude", "lease-1", 1)
	_, _ = store.QueuePrompt(ctx, "op-q-1", "run-1", "sess-1", "t-1", "Hello", "lease-1", 1)

	srv, _ := NewServer(store, dir)
	_ = srv.Start()
	defer srv.Close()

	client := srv.Client()
	releaseBody := ReleaseRequest{
		InstanceID:      srv.InstanceID(),
		OpID:            "op-rel-1",
		ControllerLease: "lease-1",
		ExpectedVersion: 1,
	}
	bodyBytes, _ := json.Marshal(releaseBody)

	// 1. Initial release succeeds with 202 Accepted
	req, _ := http.NewRequest("POST", "/v1/runs/run-1/sessions/sess-1/turns/t-1/release", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %v", resp.StatusCode)
	}

	// 2. Retry with same op_id succeeds with 200 OK and replayed: true
	req2, _ := http.NewRequest("POST", "/v1/runs/run-1/sessions/sess-1/turns/t-1/release", bytes.NewReader(bodyBytes))
	req2.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	resp2, err := client.Do(req2)
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on replay, got %v", resp2.StatusCode)
	}
	var relResp ReleaseResponse
	_ = json.NewDecoder(resp2.Body).Decode(&relResp)
	if !relResp.Replayed {
		t.Fatal("expected replayed == true on idempotent release retry")
	}

	// 3. Enter draining mode: new release is rejected with 503, but previous op_id retry still succeeds
	srv.Coordinator().SetState(ServiceStateDraining)

	// New release rejected with 503
	newReleaseBody, _ := json.Marshal(ReleaseRequest{
		InstanceID:      srv.InstanceID(),
		OpID:            "op-rel-2",
		ControllerLease: "lease-1",
		ExpectedVersion: 1,
	})
	reqNew, _ := http.NewRequest("POST", "/v1/runs/run-1/sessions/sess-1/turns/t-2/release", bytes.NewReader(newReleaseBody))
	reqNew.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	respNew, _ := client.Do(reqNew)
	if respNew.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while draining, got %v", respNew.StatusCode)
	}

	// Retry of op-rel-1 still returns 200 OK
	reqRetry, _ := http.NewRequest("POST", "/v1/runs/run-1/sessions/sess-1/turns/t-1/release", bytes.NewReader(bodyBytes))
	reqRetry.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	respRetry, _ := client.Do(reqRetry)
	if respRetry.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on retry during drain, got %v", respRetry.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestRelease_IdempotentRetryAndDraining ./internal/service`
Expected: FAIL.

- [ ] **Step 3: Implement Coordinator and Release Handler**

Implement `internal/service/coordinator.go`:
- Track lifecycle states: `ServiceStateRunning`, `ServiceStateDraining`, `ServiceStateStopping`.
- Admission lock for checking and transitioning state.
- Register live workers and execution contexts detached from `r.Context()`.

Implement `internal/service/release.go`:
- Pre-flight adapter availability check (rejects `503` if adapter unavailable).
- Draining check: allows retries of known `op_id`s, blocks new releases with `503`.
- Calls `store.ReleaseTurn` and dispatches worker goroutine **only** if `Disposition == ReleaseDispositionNew`.
- Returns `202 Accepted` for new release, `200 OK` (`replayed: true`) for retries.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/service`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service
git commit -m "feat(service): implement release admission gate and retry-safe dispatch coordinator"
```

---

### Task 4: Command Routing, Recovery Episodes, and Authoritative Read

**Files:**
- Create: `internal/service/commands.go`
- Create: `internal/service/turns.go`
- Test: `internal/service/commands_test.go`

**Interfaces:**
- Consumes: `storage.Store`, `adapter.Adapter`, `service.Coordinator`
- Produces:
  - `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}`
  - `POST /v1/runs/{run_id}/controller/connect`
  - `POST /v1/runs/{run_id}/sessions/{session_id}/prompts/queue`
  - `POST /v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/replace`
  - `POST /v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/discard`
  - `POST /v1/runs/{run_id}/decisions`
  - `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/cancel`
  - `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/reconcile`

- [ ] **Step 1: Write failing test for turn read, cancel, and reconcile**

Create `internal/service/commands_test.go`:
```go
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestCommands_TurnReadAndCancellation(t *testing.T) {
	dir := t.TempDir()
	store, _ := storage.Open(storage.StoreOptions{StateDir: dir})
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "run-1", "sess-1", "claude", "lease-1", 1)
	_, _ = store.QueuePrompt(ctx, "op-q-1", "run-1", "sess-1", "t-1", "Hello", "lease-1", 1)

	srv, _ := NewServer(store, dir)
	_ = srv.Start()
	defer srv.Close()

	client := srv.Client()

	// 1. Authoritative Turn Read: GET turn returns full details
	req, _ := http.NewRequest("GET", "/v1/runs/run-1/sessions/sess-1/turns/t-1", nil)
	req.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on turn read, got %v", resp.StatusCode)
	}

	// 2. Cancellation Request
	cancelBody, _ := json.Marshal(CancelRequest{
		OpID:            "op-cancel-1",
		ControllerLease: "lease-1",
		ExpectedVersion: 1,
		Reason:          "user requested",
	})
	reqCancel, _ := http.NewRequest("POST", "/v1/runs/run-1/sessions/sess-1/turns/t-1/cancel", bytes.NewReader(cancelBody))
	reqCancel.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	respCancel, err := client.Do(reqCancel)
	if err != nil || respCancel.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on cancel request, got %v", respCancel.StatusCode)
	}
	var cResp CancelResponse
	_ = json.NewDecoder(respCancel.Body).Decode(&cResp)
	if cResp.CancellationStatus != "requested" && cResp.CancellationStatus != "confirmed" {
		t.Fatalf("unexpected cancellation status: %s", cResp.CancellationStatus)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestCommands_TurnReadAndCancellation ./internal/service`
Expected: FAIL.

- [ ] **Step 3: Implement Command Handlers and Authoritative Turn Read**

Implement `internal/service/turns.go`:
- Handles `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}`: returns serialized turn, dispatch intent, receipts, and terminal outcome.

Implement `internal/service/commands.go`:
- Handles prompt queueing, replacement, discarding, and decisions via `store`.
- Handles `POST /v1/runs/{run_id}/controller/connect`.
- Handles `POST .../cancel`: validates lease and version, calls `store.RequestCancel(..., op_id+":req", ...)`, invokes adapter Cancel on an independent control context, records outcome if confirmed.
- Handles `POST .../reconcile`: checks if recovery episode is open; if not, calls `store.RecordHostLoss(..., op_id+":host_loss", ...)`. Then invokes adapter `Reconcile` and records outcome.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/service`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service
git commit -m "feat(service): implement command routing, recovery episodes, and authoritative turn state read"
```

---

### Task 5: Server-Sent Events (SSE) Live Observation

**Files:**
- Create: `internal/service/events.go`
- Test: `internal/service/events_test.go`

**Interfaces:**
- Consumes: `service.Coordinator`, `storage.Store`
- Produces:
  - `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/events`
  - SSE framing: `id: <seq>\nevent: <name>\ndata: <json>\n\n`

- [ ] **Step 1: Write failing test for SSE snapshot on connect and client disconnection**

Create `internal/service/events_test.go`:
```go
package service

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestEvents_SnapshotAndCleanDisconnect(t *testing.T) {
	dir := t.TempDir()
	store, _ := storage.Open(storage.StoreOptions{StateDir: dir})
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "run-1", "sess-1", "claude", "lease-1", 1)
	_, _ = store.QueuePrompt(ctx, "op-q-1", "run-1", "sess-1", "t-1", "Hello", "lease-1", 1)

	srv, _ := NewServer(store, dir)
	_ = srv.Start()
	defer srv.Close()

	client := srv.Client()

	req, _ := http.NewRequest("GET", "/v1/runs/run-1/sessions/sess-1/turns/t-1/events", nil)
	req.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK SSE stream, got %v", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "id:") {
		t.Fatalf("expected id line, got: %q, err: %v", line, err)
	}

	// Close client connection immediately; verify server doesn't panic or leak
	resp.Body.Close()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestEvents_SnapshotAndCleanDisconnect ./internal/service`
Expected: FAIL.

- [ ] **Step 3: Implement SSE Endpoint**

Implement `internal/service/events.go`:
- Sets headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`.
- Flushes initial snapshot of turn state. If turn is already terminal, emit terminal event and return immediately.
- Subscribes to coordinator's event channel for that turn with bounded buffer (64).
- Listens on `r.Context().Done()`: when client disconnects, unregisters subscriber and exits without affecting execution.
- Emits terminal events only after SQLite transaction commit.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/service`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service
git commit -m "feat(service): implement SSE live observation with snapshot-on-connect and bounded buffers"
```

---

### Task 6: Shutdown Coordinator, Signal Handling & Forced-Exit Invariants

**Files:**
- Create: `internal/service/shutdown.go`
- Test: `internal/service/shutdown_test.go`

**Interfaces:**
- Consumes: `service.Coordinator`, `service.Server`, `storage.Store`
- Produces:
  - `POST /v1/service/stop`
  - OS Signal listener (`SIGTERM`, `SIGINT`)
  - Bounded graceful drain (15s) and teardown (5s) deadlines

- [ ] **Step 1: Write failing test for idle stop, drain stop, and signal grace**

Create `internal/service/shutdown_test.go`:
```go
package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestShutdown_IdleAndDrainingContracts(t *testing.T) {
	dir := t.TempDir()
	store, _ := storage.Open(storage.StoreOptions{StateDir: dir})
	defer store.Close()

	srv, _ := NewServer(store, dir)
	_ = srv.Start()
	defer srv.Close()

	client := srv.Client()

	// 1. Instance mismatch returns 409 instance_mismatch
	badBody, _ := json.Marshal(StopRequest{InstanceID: "wrong-id", Drain: false})
	req, _ := http.NewRequest("POST", "/v1/service/stop", bytes.NewReader(badBody))
	req.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	resp, _ := client.Do(req)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 instance mismatch, got %v", resp.StatusCode)
	}

	// 2. Idle stop on empty service returns 202 Accepted and stops
	stopBody, _ := json.Marshal(StopRequest{InstanceID: srv.InstanceID(), Drain: false})
	reqStop, _ := http.NewRequest("POST", "/v1/service/stop", bytes.NewReader(stopBody))
	reqStop.Header.Set("Authorization", "Bearer "+srv.AuthToken())
	respStop, err := client.Do(reqStop)
	if err != nil || respStop.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on idle stop, got %v", respStop.StatusCode)
	}

	// Wait for server shutdown
	srv.Wait()

	// Verify discovery files unlinked and lock file remains
	if fileExists(srv.SocketPath()) || fileExists(srv.TokenPath()) {
		t.Fatal("runtime socket and token must be unlinked on clean shutdown")
	}
	if !fileExists(srv.LockPath()) {
		t.Fatal("service.lock must remain on disk after shutdown")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestShutdown_IdleAndDrainingContracts ./internal/service`
Expected: FAIL.

- [ ] **Step 3: Implement Shutdown Coordinator and Stop Handler**

Implement `internal/service/shutdown.go`:
- Handles `POST /v1/service/stop`.
- Validates `instance_id`.
- If `drain: false`: checks `live_workers > 0` or unresolved blockers; if busy returns `409 service_busy`. If idle, atomically sets `stopping` and initiates teardown.
- If `drain: true`: sets `draining`, closes release admission gate, returns `202 Accepted`.
- Manages OS signal listeners (`SIGTERM`, `SIGINT`): initiates bounded drain (15s grace).
- Teardown sequence:
  1. Reject new mutations and subscriptions.
  2. Terminate active SSE streams.
  3. `http.Server.Shutdown(ctx)` (5s deadline).
  4. Join remaining tasks.
  5. Close `storage.Store`.
  6. Unlink `service.json`, `auth.token`, `council.sock`.
  7. Release `service.lock` handle last.
- If teardown deadline expires, execute forced exit without releasing lock prematurely.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/service`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service
git commit -m "feat(service): implement shutdown coordinator, signal handling, and teardown invariants"
```

---

### Task 7: Detached Background Launcher & CLI Command Suite

**Files:**
- Modify: `cmd/council/main.go`
- Create: `cmd/council/service.go`
- Create: `internal/client/client.go`
- Test: `cmd/council/service_test.go`

**Interfaces:**
- Consumes: `internal/service`, `internal/client`
- Produces:
  - `council service run [--state-dir <path>]`
  - `council service start [--state-dir <path>]`
  - `council service status [--state-dir <path>]`
  - `council service stop [--state-dir <path>] [--drain] [--timeout <duration>]`

- [ ] **Step 1: Write failing tests for CLI subcommands and detached launcher**

Create `cmd/council/service_test.go`:
```go
package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCLI_ServiceStartStatusStopLifecycle(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t)

	// 1. council service start launches background service
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmdStart := exec.CommandContext(ctx, bin, "service", "start", "--state-dir", dir)
	out, err := cmdStart.CombinedOutput()
	if err != nil {
		t.Fatalf("council service start failed: %v, out: %s", err, string(out))
	}

	// 2. council service status reports ready
	cmdStatus := exec.Command(bin, "service", "status", "--state-dir", dir)
	out, err = cmdStatus.CombinedOutput()
	if err != nil {
		t.Fatalf("council service status failed: %v, out: %s", err, string(out))
	}

	// 3. council service stop stops the background service
	cmdStop := exec.Command(bin, "service", "stop", "--state-dir", dir, "--timeout", "5s")
	out, err = cmdStop.CombinedOutput()
	if err != nil {
		t.Fatalf("council service stop failed: %v, out: %s", err, string(out))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./cmd/council`
Expected: FAIL.

- [ ] **Step 3: Implement CLI Subcommands and Client**

Implement `internal/client/client.go`:
- Discovers `service.json` and `auth.token`.
- Configures HTTP client with custom UDS dialer (`net.Dial("unix", socketPath)`).
- Issues authenticated requests and handles JSON error envelopes.

Implement `cmd/council/service.go` and update `cmd/council/main.go`:
- `service run`: starts foreground server.
- `service start`: executes `service run` with `setsid` and `DevNull` stdio; polls `GET /v1/readiness` with timeout; verifies matching instance, protocol version, and state dir.
- `service status`: reads discovery and calls `GET /v1/status`.
- `service stop`: calls `POST /v1/service/stop` with `--drain` flag and waits up to `--timeout`.
- Native Windows: returns explicit unsupported-platform error.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./cmd/council`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/council internal/client
git commit -m "feat(cli): implement detached background launcher and service command suite"
```

---

### Task 8: End-to-End Integration, Recovery Scenarios & Concurrency Acceptance Suite

**Files:**
- Create: `internal/service/acceptance_test.go`
- Create: `internal/service/testassembly_test.go`

**Interfaces:**
- Consumes: full `internal/service`, `internal/client`, `internal/adapter/adaptertest`
- Produces: Comprehensive acceptance evidence covering all spec scenarios.

- [ ] **Step 1: Write acceptance tests for two-client disconnect, crash recovery, and startup races**

Create `internal/service/acceptance_test.go`:
1. `TestAcceptance_TwoClientDisconnect`:
   - Independent service process running.
   - Client A releases a turn through fake adapter with simulated execution delay.
   - Client A process terminates immediately after release.
   - Worker completes execution and commits terminal outcome to SQLite.
   - Client B connects, queries `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}`, and verifies completed result.
   - A queued follow-up prompt remains queued and unexecuted.
2. `TestAcceptance_CrashRecovery_WithoutAccessibleNativeEvidence`:
   - Service process killed (`kill -9`) mid-execution.
   - Restart service against same state directory.
   - Service hydrates state: turn reservation is preserved as unresolved.
   - Service reports `live_workers: 0`, `reserved_turns: 1`, `unresolved_turns: 1`.
   - Turn is not redispatched.
3. `TestAcceptance_RestartWithIndependentlyRetainedEvidence`:
   - External helper process retains test execution evidence independent of service memory.
   - Restarted service calls `reconcile`: allocates recovery episode via `RecordHostLoss`, attaches to original binding, probes independent evidence, and authoritatively resolves.
4. `TestAcceptance_ConcurrentStartupRace`:
   - Two concurrent `service start` invocations against the same state directory.
   - Exactly one owner succeeds; loser fails with `already running` and leaves winner's files untouched.
5. `TestAcceptance_ReleaseRetryDuringDrain`:
   - Service enters `draining`.
   - Matching retry of already-committed release returns 200 OK without additional dispatch.
6. `TestAcceptance_SignalGraceExpiry`:
   - Signal grace expires while worker is active; service preserves unresolved outcome in SQLite without early lock release.

- [ ] **Step 2: Run tests to verify execution**

Run: `go test -race -count=1 -timeout=120s -v -run TestAcceptance_ ./internal/service`
Expected: PASS.

- [ ] **Step 3: Run full local repository verification**

```bash
CGO_ENABLED=0 go test ./...
CGO_ENABLED=1 go test -race -count=3 ./...
test -z "$(gofmt -l cmd internal)" && go vet ./... && python3 scripts/verify_seed.py && python3 -m unittest discover -s scripts/tests -v
```
Expected: All tests PASS, zero data races, clean linting and formatting.

- [ ] **Step 4: Commit**

```bash
git add internal/service
git commit -m "test(service): add end-to-end integration and concurrency acceptance test suite"
```
