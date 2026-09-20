# AC-003 Service Lifetime & Local Control Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native inline TDD with 1 implementation owner and 2 internal verification gates). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the Council Go service independently of CLI/MCP client connection lifetime over an authenticated local control boundary, ensuring client disconnection never terminates authorized work and service restart honestly preserves execution uncertainty without fabricating completion or redispatching unprompted.

**Architecture:** A foreground service core (`council service run`) manages state directory exclusivity via `syscall.Flock`, initializes SQLite storage, binds an owner-restricted UNIX domain socket (`council.sock`, mode `0600`), and serves HTTP/1.1 JSON commands and SSE observations with Bearer token authentication (`auth.token`). A detached background launcher (`council service start`) invokes the core with `setsid` on Linux/WSL and verifies authenticated readiness.

**Tech Stack:** Go 1.25.0, SQLite (modernc.org/sqlite v1.59.0 via database/sql), `net/http`, `syscall.Flock`, `crypto/rand`.

**Spec:** [`../specs/2026-09-19-ac-003-service-lifetime-design.md`](file:///home/carlitos/projects/CtrlCarlitos/agent-council/docs/superpowers/specs/2026-09-19-ac-003-service-lifetime-design.md)

## Global Constraints

- Go 1.25.0 baseline across `go.mod`, CI workflows, and documentation.
- Dual CI verification: pure-Go (`CGO_ENABLED=0`) and race detector (`CGO_ENABLED=1 -race -count=3`).
- Linux/WSL first; native Windows service commands return clear unsupported-platform errors.
- Strict filesystem permissions: state directory `0700`; `service.lock`, `service.json`, `auth.token`, `council.sock` mode `0600`.
- `service.lock` is never unlinked or replaced on cleanup; handle closed last.
- Client disconnect never terminates accepted workers or blocks persistence.
- Restart detects unfinished work without inventing completion or redispatching unprompted.
- No "fake" adapter registered in production registry (use test-only assembly).

## Verification Gates
- **Gate 1 (after Tasks 1–3)**: Verify ownership exclusivity, authenticated startup, transaction-derived release disposition, adapter injection, and single-dispatch evidence. Full test pass with `CGO_ENABLED=0 go test ./...` and `CGO_ENABLED=1 go test -race -count=3 ./...`.
- **Gate 2 (after Tasks 4–6)**: Verify composite-command retries, durable outcome recording, race-free observation, and bounded shutdown against adversarial cases before CLI and final subprocess integration. Full test pass with `CGO_ENABLED=0 go test ./...` and `CGO_ENABLED=1 go test -race -count=3 ./...`.

## Review Focus

1. **Concurrent Duplicate Release During Draining**: An authorized release retry arriving while the service is draining must return the original committed receipt without dispatching a second worker.
2. **Crash Between Reservation Commit & Worker Dispatch**: Terminating the service process immediately after SQLite reservation commit leaves the turn in an unresolved state upon restart, without fabricating worker survival or redispatching.
3. **Slow / Disconnected SSE Observer**: A client dropping connection or stalling on the SSE stream must be disconnected via write timeout without cancelling the worker or delaying database commits.
4. **Signal Drain Grace Expiry & Forced Termination**: Expiration of the 15-second signal drain grace period triggers bounded termination handling, preserving unconfirmed outcomes as unresolved in SQLite without early lock release or fabricated outcomes.
5. **Losing Startup Process Cleanliness**: A competing startup attempt failing to acquire `service.lock` must fail immediately and touch none of the winning service instance's runtime files (`service.json`, `auth.token`, `council.sock`).

---

### Task 1: Storage Bridge, Exclusivity Locking, and Discovery Metadata

**Files:**
- Modify: `internal/storage/types.go`
- Modify: `internal/storage/transitions.go`
- Modify: `internal/storage/session_store.go`
- Create: `internal/storage/release_disposition_test.go`
- Create: `internal/service/lock.go`
- Create: `internal/service/lock_unix.go`
- Create: `internal/service/lock_windows.go`
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
  - `service.ServiceLock.CleanupDiscovery() error` (owner-bound receiver)
  - `service.PublishDiscovery(stateDir string, meta DiscoveryMeta, token string) error`

- [ ] **Step 1: Write failing tests for storage ReleaseDisposition, ServiceLock, and Discovery**

Create `internal/storage/release_disposition_test.go`:
```go
package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStorage_ReleaseDisposition_NewReplayedConflict(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, storage.SessionRecord{
		RunID:           "run-1",
		SessionID:       "sess-1",
		ContributorID:   "claude",
		ControllerLease: "lease-1",
		ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	qRec, err := store.QueuePrompt(ctx, storage.PendingPrompt{
		RunID:           "run-1",
		SessionID:       "sess-1",
		TurnKey:         "t-1",
		Prompt:          "Hello",
		ControllerLease: "lease-1",
		ExpectedVersion: sessRec.Receipt.CommittedVersion,
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	// 1. First release: returns ReleaseDispositionNew
	res1, err := store.ReleaseTurn(ctx, "op-rel-1", "run-1", "sess-1", "t-1", "lease-1", qRec.Receipt.CommittedVersion)
	if err != nil {
		t.Fatalf("first release failed: %v", err)
	}
	if res1.Disposition != storage.ReleaseDispositionNew {
		t.Fatalf("expected ReleaseDispositionNew, got %v", res1.Disposition)
	}
	if res1.Receipt.TurnKey != "t-1" {
		t.Fatalf("unexpected turn key in receipt: %s", res1.Receipt.TurnKey)
	}

	// 2. Idempotent replay: same op_id and parameters returns ReleaseDispositionReplayed with identical receipt
	res2, err := store.ReleaseTurn(ctx, "op-rel-1", "run-1", "sess-1", "t-1", "lease-1", qRec.Receipt.CommittedVersion)
	if err != nil {
		t.Fatalf("replay release failed: %v", err)
	}
	if res2.Disposition != storage.ReleaseDispositionReplayed {
		t.Fatalf("expected ReleaseDispositionReplayed, got %v", res2.Disposition)
	}
	if res2.Receipt != res1.Receipt {
		t.Fatalf("expected identical receipt, got %+v vs %+v", res2.Receipt, res1.Receipt)
	}

	// 3. Conflicting op_id: same op_id with different turn_key or parameters fails
	_, err = store.ReleaseTurn(ctx, "op-rel-1", "run-1", "sess-1", "t-other", "lease-1", qRec.Receipt.CommittedVersion)
	if err == nil {
		t.Fatal("expected error on conflicting release replay, got nil")
	}

	// 4. Replay after store reopen: preserve ReleaseDispositionReplayed and receipt
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	store2, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()

	res3, err := store2.ReleaseTurn(ctx, "op-rel-1", "run-1", "sess-1", "t-1", "lease-1", qRec.Receipt.CommittedVersion)
	if err != nil {
		t.Fatalf("replay after reopen failed: %v", err)
	}
	if res3.Disposition != storage.ReleaseDispositionReplayed || res3.Receipt != res1.Receipt {
		t.Fatalf("expected replayed receipt matching original after reopen, got %+v", res3)
	}
}
```

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
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscovery_SeparationAndPermissions(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

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
	if err != nil {
		t.Fatalf("stat auth.token: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("auth.token permissions not 0600: %v", info.Mode().Perm())
	}

	// Check service.json exists with mode 0600 and does NOT contain token
	metaPath := filepath.Join(dir, "service.json")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read service.json: %v", err)
	}
	if len(metaBytes) == 0 || bytes.Contains(metaBytes, []byte(token)) {
		t.Fatal("service.json must not leak auth.token")
	}

	// Owner-bound cleanup removes service.json, auth.token, council.sock
	if err := lock.CleanupDiscovery(); err != nil {
		t.Fatalf("cleanup discovery failed: %v", err)
	}
	if fileExists(tokenPath) || fileExists(metaPath) {
		t.Fatal("expected discovery and token files to be unlinked")
	}
}

func TestDiscovery_FailedStartAndSuccessorSafety(t *testing.T) {
	dir := t.TempDir()
	winnerLock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("winner lock failed: %v", err)
	}
	defer winnerLock.Release()

	meta := DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      "winner-1",
		PID:             os.Getpid(),
		Transport:       "unix",
		Endpoint:        filepath.Join(dir, "council.sock"),
		StateDir:        dir,
	}
	token := "winner-token"
	if err := PublishDiscovery(dir, meta, token); err != nil {
		t.Fatalf("winner publish failed: %v", err)
	}

	// Loser attempts to acquire lock and fails
	loserLock, err := AcquireServiceLock(dir)
	if err != ErrServiceAlreadyRunning {
		t.Fatalf("expected ErrServiceAlreadyRunning, got %v", err)
	}
	if loserLock != nil {
		t.Fatal("loser lock must be nil")
	}

	// Unowned / unheld lock cannot remove winner's files
	var nilLock *ServiceLock
	if err := nilLock.CleanupDiscovery(); err == nil {
		t.Fatal("unowned cleanup must return error")
	}

	// Winner's discovery files remain intact
	metaPath := filepath.Join(dir, "service.json")
	if !fileExists(metaPath) {
		t.Fatal("winner's service.json must remain intact after loser failure")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v ./internal/storage -run TestStorage_ReleaseDisposition` and `go test -v ./internal/service`
Expected: FAIL (types and methods not implemented).

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
Update `ReleaseTurn` to return `(ReleaseResult, error)`, distinguishing when the receipt was retrieved via idempotent journal lookup (`ReleaseDispositionReplayed`) versus a newly committed transition (`ReleaseDispositionNew`). Update any existing callers in `session_store.go` and tests to receive `ReleaseResult`.

Create `internal/service/lock.go`:
- Define `ServiceLock` struct holding file handle and stateDir.
- Define `ErrServiceAlreadyRunning = errors.New("service already running in this state directory")`
- Define `ErrUnsupportedPlatform = errors.New("service lifetime management is not supported on this platform")`
- Define `lock.CleanupDiscovery() error` (removes `service.json`, `auth.token`, `council.sock` only if `lock.IsHeld()`).

Create `internal/service/lock_unix.go` (`//go:build !windows`):
- Implement `AcquireServiceLock(stateDir string) (*ServiceLock, error)` using `syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)` with `FD_CLOEXEC`.
- Implement `Release() error`: closes fd and marks unheld, leaving `service.lock` on disk.

Create `internal/service/lock_windows.go` (`//go:build windows`):
- Implement `AcquireServiceLock(stateDir string) (*ServiceLock, error)` returning `nil, ErrUnsupportedPlatform`.

Create `internal/service/discovery.go`:
- Implement `PublishDiscovery(stateDir string, meta DiscoveryMeta, token string) error`:
  - Validates `0700` state directory.
  - Atomically writes `auth.token` (mode `0600`) with 32 cryptographically random bytes formatted as hex.
  - Atomically writes `service.json` (mode `0600`) excluding `auth.token`.
  - Removes stale socket if one exists (validating it is a socket, not a dir or symlink).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/storage -run TestStorage_ReleaseDisposition` and `go test -v ./internal/service`
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
  - `service.ServerConfig`: `{ StateDir string, InstanceID string, AuthToken string }`
  - `service.NewServer(store *storage.Store, lock *ServiceLock, cfg ServerConfig) (*Server, error)`
  - `service.Server.Start() error`
  - `service.Server.Close() error`
  - Endpoints: `GET /v1/readiness`, `GET /v1/status`
- **Resource Ownership**:
  The foreground runner or launcher acquires `ServiceLock` and opens `storage.Store`. `NewServer` receives these resources and configuration. `Server` owns the HTTP listener, request routing, and coordinator lifecycle. `Server.Close()` cleanly shuts down the HTTP server and coordinator. The caller/runner closes `storage.Store` and releases `ServiceLock` upon process exit.

- [ ] **Step 1: Write failing tests for HTTP server, Bearer auth, readiness, and status**

Create `internal/service/server_test.go`:
```go
package service

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func newTestClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

func TestServer_ReadinessAndStatus(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-auth-token-secret-1234567890",
	}

	srv, err := NewServer(store, lock, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	// 1. Unauthenticated request must return 401 with WWW-Authenticate
	resp, err := client.Get("http://localhost/v1/readiness")
	if err != nil {
		t.Fatalf("unauthenticated GET failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %v", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("missing WWW-Authenticate header")
	}

	// 2. Authenticated readiness request must return 200 ready
	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://localhost/v1/readiness", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	respReady, err := client.Do(req)
	if err != nil {
		t.Fatalf("readiness request failed: %v", err)
	}
	defer respReady.Body.Close()
	if respReady.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got: %v", respReady.StatusCode)
	}

	var r ReadinessResponse
	if err := json.NewDecoder(respReady.Body).Decode(&r); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if r.Status != "ready" || r.InstanceID != cfg.InstanceID {
		t.Fatalf("unexpected readiness response: %+v", r)
	}

	// 3. Authenticated status request returns diagnostics
	reqStatus, err := http.NewRequestWithContext(context.Background(), "GET", "http://localhost/v1/status", nil)
	if err != nil {
		t.Fatalf("create status req: %v", err)
	}
	reqStatus.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	respStatus, err := client.Do(reqStatus)
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer respStatus.Body.Close()
	if respStatus.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got: %v", respStatus.StatusCode)
	}

	var s StatusResponse
	if err := json.NewDecoder(respStatus.Body).Decode(&s); err != nil {
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
- Create: `internal/service/supervisor.go`
- Test: `internal/service/release_test.go`

**Interfaces:**
- Consumes: `storage.Store`, `ServiceLock`, `adapter.Adapter`
- Produces:
  - `service.Coordinator` (manages admission gate, live workers, execution contexts)
  - `service.ExecutionSupervisor` (owns worker dispatch, observation, collection, outcome commit, and version advance resilience)
  - `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/release`
  - Worker execution decoupling: worker runs under detached `coordinator.Context()`, not `r.Context()`.

- [ ] **Step 1: Write failing test for release hand-off, retry safety, draining admission, and adapter dispatch counting**

Create `internal/service/release_test.go`:
```go
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestRelease_IdempotentRetryAndDraining(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, storage.SessionRecord{
		RunID:           "run-1",
		SessionID:       "sess-1",
		ContributorID:   "claude",
		ControllerLease: "lease-1",
		ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	qRec1, err := store.QueuePrompt(ctx, storage.PendingPrompt{
		RunID:           "run-1",
		SessionID:       "sess-1",
		TurnKey:         "t-1",
		Prompt:          "Hello",
		ControllerLease: "lease-1",
		ExpectedVersion: sessRec.Receipt.CommittedVersion,
	})
	if err != nil {
		t.Fatalf("queue prompt t-1: %v", err)
	}

	// Authentically queue a sibling turn t-2 for later drain testing
	qRec2, err := store.QueuePrompt(ctx, storage.PendingPrompt{
		RunID:           "run-1",
		SessionID:       "sess-1",
		TurnKey:         "t-2",
		Prompt:          "Followup",
		ControllerLease: "lease-1",
		ExpectedVersion: qRec1.Receipt.CommittedVersion,
	})
	if err != nil {
		t.Fatalf("queue prompt t-2: %v", err)
	}

	// Create test adapter assembly
	fakeAdapter := adaptertest.NewFakeAdapter("claude")

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-auth-token-12345",
	}

	srv, err := NewServerWithAdapter(store, lock, cfg, fakeAdapter)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())
	releaseBody := ReleaseRequest{
		InstanceID:      srv.InstanceID(),
		OpID:            "op-rel-1",
		ControllerLease: "lease-1",
		ExpectedVersion: qRec2.Receipt.CommittedVersion,
	}
	bodyBytes, err := json.Marshal(releaseBody)
	if err != nil {
		t.Fatalf("marshal release req: %v", err)
	}

	// 1. Initial release succeeds with 202 Accepted
	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1/release", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("release request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %v", resp.StatusCode)
	}

	// Assert adapter dispatch count is exactly 1
	if count := fakeAdapter.DispatchCount("sess-1"); count != 1 {
		t.Fatalf("expected 1 dispatch for initial release, got %d", count)
	}

	// 2. Retry with same op_id succeeds with 200 OK and replayed: true
	req2, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1/release", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("create req2: %v", err)
	}
	req2.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("retry release request failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on replay, got %v", resp2.StatusCode)
	}
	var relResp ReleaseResponse
	if err := json.NewDecoder(resp2.Body).Decode(&relResp); err != nil {
		t.Fatalf("decode replay response: %v", err)
	}
	if !relResp.Replayed {
		t.Fatal("expected replayed == true on idempotent release retry")
	}

	// Assert adapter dispatch count remains exactly 1 (no second worker dispatched)
	if count := fakeAdapter.DispatchCount("sess-1"); count != 1 {
		t.Fatalf("expected dispatch count to remain 1 after replay, got %d", count)
	}

	// 3. Enter draining mode: new release is rejected with 503, but previous op_id retry still succeeds
	srv.Coordinator().SetState(ServiceStateDraining)

	// New release of sibling prompt t-2 rejected with 503
	newReleaseBody, err := json.Marshal(ReleaseRequest{
		InstanceID:      srv.InstanceID(),
		OpID:            "op-rel-2",
		ControllerLease: "lease-1",
		ExpectedVersion: qRec2.Receipt.CommittedVersion,
	})
	if err != nil {
		t.Fatalf("marshal new release body: %v", err)
	}
	reqNew, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-2/release", bytes.NewReader(newReleaseBody))
	if err != nil {
		t.Fatalf("create reqNew: %v", err)
	}
	reqNew.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqNew.Header.Set("Content-Type", "application/json")
	respNew, err := client.Do(reqNew)
	if err != nil {
		t.Fatalf("new release during drain failed: %v", err)
	}
	defer respNew.Body.Close()
	if respNew.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while draining, got %v", respNew.StatusCode)
	}

	// Retry of op-rel-1 still returns 200 OK without checking adapter availability or drain state
	reqRetry, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1/release", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("create reqRetry: %v", err)
	}
	reqRetry.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqRetry.Header.Set("Content-Type", "application/json")
	respRetry, err := client.Do(reqRetry)
	if err != nil {
		t.Fatalf("retry during drain failed: %v", err)
	}
	defer respRetry.Body.Close()
	if respRetry.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on retry during drain, got %v", respRetry.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestRelease_IdempotentRetryAndDraining ./internal/service`
Expected: FAIL.

- [ ] **Step 3: Implement Coordinator, Release Handler, and Execution Supervisor**

Implement `internal/service/coordinator.go`:
- Track lifecycle states: `ServiceStateRunning`, `ServiceStateDraining`, `ServiceStateStopping`.
- Admission lock for checking and transitioning state.
- Live worker accounting with wait groups and tracking maps.
- Detached execution context for workers tied to coordinator lifetime, not `r.Context()`.

Implement `internal/service/supervisor.go`:
- Service-owned execution supervisor pipeline:
  1. Retains original release receipt and attempt identity throughout.
  2. Dispatches worker via `adapter.Dispatch(turnCtx, ...)`.
  3. Handles disposition: `DispatchAccepted`, `DispatchUnknown`, or `DispatchRejected`.
  4. Observes events and collects authoritative outcome from `adapter.Collect(...)`.
  5. Persists terminal outcome via `store.RecordTerminalOutcome`.
     - **Session Version Resilience**: If session version advanced during execution (e.g., concurrent prompt queue or decision), supervisor re-queries latest session version and retries outcome recording deterministically without changing execution identity.
  6. Releases coordinator worker accounting only after the terminal outcome is durably committed to SQLite.

Implement `internal/service/release.go`:
- Enforces strict release ordering:
  1. Authenticate Bearer token and validate resource scope.
  2. **Idempotent Retry Resolution**: Checks if `op_id` already matches a committed release operation for this turn. If found, returns existing receipt immediately (`200 OK`, `replayed: true`) without checking harness availability or admission draining state.
  3. **New Release Checks**:
     - Under admission lock, rejects if `draining` or `stopping` (`503 Service Unavailable`, `error.code: "service_draining"`).
     - Verifies required harness adapter is available; if unavailable, rejects (`503 Service Unavailable`, `error.code: "harness_unavailable"`).
  4. Calls `store.ReleaseTurn(...)` to commit reservation in SQLite:
     - `ReleaseDispositionNew`: Coordinator registers worker execution and launches `supervisor.Run(workerCtx)` in a detached goroutine. Returns `202 Accepted`.
     - `ReleaseDispositionReplayed`: Returns `200 OK` with `replayed: true` (does not dispatch worker).

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
  - `POST /v1/runs/{run_id}/sessions/{session_id}/controller/connect` (session-scoped controller reattachment)
  - `POST /v1/runs/{run_id}/sessions/{session_id}/prompts/queue`
  - `POST /v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/replace`
  - `POST /v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/discard`
  - `POST /v1/runs/{run_id}/decisions`
  - `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/cancel`
  - `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/reconcile`

- [ ] **Step 1: Write failing test for turn read, cancellation of active turn, and composite reconciliation**

Create `internal/service/commands_test.go`:
```go
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestCommands_TurnReadCancelAndReconcile(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, storage.SessionRecord{
		RunID:           "run-1",
		SessionID:       "sess-1",
		ContributorID:   "claude",
		ControllerLease: "lease-1",
		ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	qRec, err := store.QueuePrompt(ctx, storage.PendingPrompt{
		RunID:           "run-1",
		SessionID:       "sess-1",
		TurnKey:         "t-1",
		Prompt:          "Hello",
		ControllerLease: "lease-1",
		ExpectedVersion: sessRec.Receipt.CommittedVersion,
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	// Release turn first so it is running (cancellation requires an active running turn)
	relRes, err := store.ReleaseTurn(ctx, "op-rel-1", "run-1", "sess-1", "t-1", "lease-1", qRec.Receipt.CommittedVersion)
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	fakeAdapter := adaptertest.NewFakeAdapter("claude")
	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-456",
	}

	srv, err := NewServerWithAdapter(store, lock, cfg, fakeAdapter)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	// 1. Authoritative Turn Read: GET turn returns full details
	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1", nil)
	if err != nil {
		t.Fatalf("create read req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("turn read request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on turn read, got %v", resp.StatusCode)
	}

	// 2. Cancellation Request against active running turn
	cancelBody, err := json.Marshal(CancelRequest{
		OpID:            "op-cancel-1",
		ControllerLease: "lease-1",
		ExpectedVersion: relRes.Receipt.CommittedVersion,
		Reason:          "user requested",
	})
	if err != nil {
		t.Fatalf("marshal cancel body: %v", err)
	}
	reqCancel, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1/cancel", bytes.NewReader(cancelBody))
	if err != nil {
		t.Fatalf("create cancel req: %v", err)
	}
	reqCancel.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqCancel.Header.Set("Content-Type", "application/json")
	respCancel, err := client.Do(reqCancel)
	if err != nil {
		t.Fatalf("cancel request: %v", err)
	}
	defer respCancel.Body.Close()
	if respCancel.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on cancel request, got %v", respCancel.StatusCode)
	}
	var cResp CancelResponse
	if err := json.NewDecoder(respCancel.Body).Decode(&cResp); err != nil {
		t.Fatalf("decode cancel resp: %v", err)
	}
	if cResp.CancellationStatus != "requested" && cResp.CancellationStatus != "confirmed" {
		t.Fatalf("unexpected cancellation status: %s", cResp.CancellationStatus)
	}

	// 3. Controller Reattachment: session-scoped connect validates version
	connectBody, err := json.Marshal(ControllerConnectRequest{
		OpID:            "op-conn-1",
		ControllerLease: "lease-1",
		ExpectedVersion: cResp.Receipt.CommittedVersion,
	})
	if err != nil {
		t.Fatalf("marshal connect body: %v", err)
	}
	reqConn, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/controller/connect", bytes.NewReader(connectBody))
	if err != nil {
		t.Fatalf("create conn req: %v", err)
	}
	reqConn.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqConn.Header.Set("Content-Type", "application/json")
	respConn, err := client.Do(reqConn)
	if err != nil {
		t.Fatalf("connect req: %v", err)
	}
	defer respConn.Body.Close()
	if respConn.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on connect, got %v", respConn.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestCommands_TurnReadCancelAndReconcile ./internal/service`
Expected: FAIL.

- [ ] **Step 3: Implement Command Handlers, Authoritative Turn Read, and Composite-Operation Recovery**

Implement `internal/service/turns.go`:
- Handles `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}`: returns serialized turn record, dispatch intent, receipts, and terminal outcome.

Implement `internal/service/commands.go`:
- Handles prompt queueing, replacement, discarding, and decisions via `store`.
- Handles `POST /v1/runs/{run_id}/sessions/{session_id}/controller/connect`: session-scoped reattachment validating target session's `expected_version`.
- Stable composite operation ID derivation:
  - Host loss stage: `fmt.Sprintf("%s:host_loss", req.OpID)`
  - Reconcile stage: `fmt.Sprintf("%s:reconcile", req.OpID)`
  - Cancel stage: `fmt.Sprintf("%s:req", req.OpID)`
- Multi-stage retry control flow for `/cancel`:
  - Validates lease and version against running turn.
  - Calls `store.RequestCancel(..., stageOpID, ...)`. If already committed, returns existing receipt.
  - Signals adapter Cancel under bounded control context.
  - Returns `CancelResponse{ Receipt: origReceipt, CancellationStatus: status }`.
- Multi-stage retry control flow for `/reconcile`:
  - **Check Completed Stage First**: If `%s:reconcile` stage already committed in SQLite, retrieve and return the committed receipt immediately without opening another episode or calling adapter.
  - **Episode Initiation**: If no recovery episode is currently open, calls `store.RecordHostLoss(..., stageHostLossID, ...)` to allocate an episode and obtain a monotonic recovery generation. If `%s:host_loss` was already committed, reuse that existing episode generation.
  - Invokes `adapter.Reconcile(ctx, ref, gen)`.
  - Authoritatively records outcome via `store.ReconcileSession(..., stageReconcileID, ...)` and returns committed receipt.
  - Idempotency conflicts: if same `op_id` is supplied with differing target or command parameters, return `409 Conflict`.

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
  - SSE framing: `event: <name>\ndata: <json>\n\n` (omitting resumable `id:` lines for AC-003)

- [ ] **Step 1: Write failing tests for atomic SSE snapshot synchronization, clean disconnect, and slow-consumer write deadlines**

Create `internal/service/events_test.go`:
```go
package service

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestEvents_SynchronizedSnapshotAndCleanDisconnect(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, storage.SessionRecord{
		RunID:           "run-1",
		SessionID:       "sess-1",
		ContributorID:   "claude",
		ControllerLease: "lease-1",
		ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	qRec, err := store.QueuePrompt(ctx, storage.PendingPrompt{
		RunID:           "run-1",
		SessionID:       "sess-1",
		TurnKey:         "t-1",
		Prompt:          "Hello",
		ControllerLease: "lease-1",
		ExpectedVersion: sessRec.Receipt.CommittedVersion,
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	// Release turn
	_, err = store.ReleaseTurn(ctx, "op-rel-1", "run-1", "sess-1", "t-1", "lease-1", qRec.Receipt.CommittedVersion)
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	fakeAdapter := adaptertest.NewFakeAdapter("claude")
	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-789",
	}

	srv, err := NewServerWithAdapter(store, lock, cfg, fakeAdapter)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1/events", nil)
	if err != nil {
		t.Fatalf("create sse req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK SSE stream, got %v, err: %v", resp.StatusCode, err)
	}

	reader := bufio.NewReader(resp.Body)
	// First event must be the initial state snapshot event
	line1, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line1, "event:") {
		t.Fatalf("expected event: prefix, got: %q, err: %v", line1, err)
	}
	line2, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line2, "data:") {
		t.Fatalf("expected data: prefix, got: %q, err: %v", line2, err)
	}

	// Close client connection immediately; verify server doesn't panic or leak,
	// and verify worker execution completes and commits terminal outcome to SQLite
	resp.Body.Close()

	// Wait for worker completion
	select {
	case <-time.After(200 * time.Millisecond):
	}
	// Verify subscriber is cleanly deregistered
	if count := srv.Coordinator().SubscriberCount("t-1"); count != 0 {
		t.Fatalf("expected 0 subscribers after disconnect, got %d", count)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestEvents_SynchronizedSnapshotAndCleanDisconnect ./internal/service`
Expected: FAIL.

- [ ] **Step 3: Implement Synchronized SSE Endpoint and Coordinator Event Broadcasting**

Implement `internal/service/events.go`:
- Sets headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`.
- **Atomic Snapshot and Subscription Registration**:
  - Under coordinator synchronization, registers the subscriber channel AND retrieves the initial state snapshot of the turn.
  - Network writes occur *outside* the synchronization lock.
  - If the turn is already terminal, emits the terminal event and cleanly closes the stream immediately without waiting on the subscriber channel.
- **Event Framing**:
  - Formats events as `event: <name>\ndata: <json>\n\n` (no resumable `id:` line for AC-003).
- **Bounded Buffers & Write Timeouts**:
  - Subscriber channels have bounded capacity (64 events).
  - Writing to an SSE subscriber is bounded by a write deadline (e.g. 500ms). Slow or stalled consumers are dropped without blocking execution or database commits.
- **Disconnect Handling**:
  - Listens on `r.Context().Done()`: when the client disconnects, unregisters the subscriber channel and terminates the handler goroutine cleanly.
  - Detached workers continue execution unaffected.
- **Commit-Before-Delivery**:
  - Terminal events (completion, failure, cancellation) are broadcast to SSE observers only after the database transaction in `RecordTerminalOutcome` or `ReconcileSession` has committed.

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
- Consumes: `service.Coordinator`, `service.Server`, `storage.Store`, `service.ServiceLock`
- Produces:
  - `POST /v1/service/stop`
  - OS Signal listener (`SIGTERM`, `SIGINT`)
  - Three distinct timeout boundaries:
    1. CLI wait timeout (`--timeout`, evaluated client-side)
    2. Signal drain grace period (default 15s, service-side)
    3. Final teardown deadline (default 5s, service-side)
  - Complete shutdown eligibility verification:
    1. Accepted-but-not-started release handoffs == 0
    2. Active live worker executions == 0
    3. Pending terminal outcome database commits == 0
    4. Admitted in-flight cancellation or reconciliation jobs == 0
    5. Outstanding unresolved recovery blockers == 0

- [ ] **Step 1: Write failing tests for idle stop, draining stop, blocked commits, and signal grace expiry**

Create `internal/service/shutdown_test.go`:
```go
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestShutdown_IdleAndDrainingContracts(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-shutdown",
	}

	srv, err := NewServer(store, lock, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	// 1. Instance mismatch returns 409 instance_mismatch
	badBody, err := json.Marshal(StopRequest{InstanceID: "wrong-id", Drain: false})
	if err != nil {
		t.Fatalf("marshal bad stop body: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), "POST", "http://localhost/v1/service/stop", bytes.NewReader(badBody))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("mismatch stop req failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 instance mismatch, got %v", resp.StatusCode)
	}

	// 2. Idle stop on empty service returns 202 Accepted and reaches quiescence
	stopBody, err := json.Marshal(StopRequest{InstanceID: cfg.InstanceID, Drain: false})
	if err != nil {
		t.Fatalf("marshal stop body: %v", err)
	}
	reqStop, err := http.NewRequestWithContext(context.Background(), "POST", "http://localhost/v1/service/stop", bytes.NewReader(stopBody))
	if err != nil {
		t.Fatalf("create reqStop: %v", err)
	}
	reqStop.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqStop.Header.Set("Content-Type", "application/json")
	respStop, err := client.Do(reqStop)
	if err != nil {
		t.Fatalf("stop request failed: %v", err)
	}
	defer respStop.Body.Close()
	if respStop.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on idle stop, got %v", respStop.StatusCode)
	}

	// Bounded wait for server shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.WaitForShutdown(shutdownCtx); err != nil {
		t.Fatalf("server did not shut down within timeout: %v", err)
	}

	// Verify discovery files unlinked and lock file remains on disk
	if fileExists(srv.SocketPath()) || fileExists(srv.TokenPath()) {
		t.Fatal("runtime socket and token must be unlinked on clean shutdown")
	}
	if !fileExists(lock.Path()) {
		t.Fatal("service.lock must remain on disk after shutdown")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestShutdown_IdleAndDrainingContracts ./internal/service`
Expected: FAIL.

- [ ] **Step 3: Implement Shutdown Coordinator and Orderly Teardown Sequence**

Implement `internal/service/shutdown.go`:
- Complete shutdown eligibility checking under coordinator admission lock:
  1. Accepted-but-not-started handoffs == 0
  2. Active executions (`live_workers`) == 0
  3. Pending terminal commits == 0
  4. Admitted control jobs == 0
  5. Outstanding unresolved recovery blockers == 0
- Handles `POST /v1/service/stop`:
  - Validates `instance_id`. Mismatch returns `409 instance_mismatch`.
  - If `drain: false`: verifies complete shutdown eligibility. If any counter > 0, returns `409 service_busy`. If idle, sets `stopping` and triggers teardown.
  - If `drain: true`: sets `draining`, closes release admission gate, returns `202 Accepted`.
- Manages OS signal listeners (`SIGTERM`, `SIGINT`):
  - Transitions to `draining` with a 15-second signal grace countdown.
  - **Grace Expiry Rule**: When the 15s grace expires before executions complete, initiates bounded termination handling (requests worker cancellation, persists confirmed outcomes, leaves unconfirmed as unresolved in SQLite).
- **Strict Teardown Sequence**:
  1. Close command admission gate (rejecting new releases, mutations, and subscriptions).
  2. Quiesce or terminate SSE streams.
  3. `http.Server.Shutdown(ctx)` bounded by the 5-second final teardown deadline.
  4. Join service-owned execution and persistence tasks.
  5. Close `storage.Store`.
  6. Call `lock.CleanupDiscovery()` to unlink `service.json`, `auth.token`, `council.sock`.
  7. Release `ServiceLock` handle **last** upon exit (never deleted from disk).
- **Forced Termination Boundary**:
  - If orderly quiescence cannot be established within the 5-second final teardown deadline, triggers forced exit without fabricating completion and **without releasing `service.lock` early**.

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
- Create: `cmd/council/service_unix.go`
- Create: `cmd/council/service_windows.go`
- Create: `internal/client/client.go`
- Test: `cmd/council/service_test.go`

**Interfaces:**
- Consumes: `internal/service`, `internal/client`
- Produces:
  - `council service run [--state-dir <path>]`
  - `council service start [--state-dir <path>]`
  - `council service status [--state-dir <path>]`
  - `council service stop [--state-dir <path>] [--drain] [--timeout <duration>]`

- [ ] **Step 1: Write failing tests for CLI subcommands, detached launcher, and status decoding**

Create `cmd/council/service_test.go`:
```go
package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCLI_ServiceStartStatusStopLifecycle(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t)

	// 1. council service start launches detached background service
	ctxStart, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStart()

	cmdStart := exec.CommandContext(ctxStart, bin, "service", "start", "--state-dir", dir)
	out, err := cmdStart.CombinedOutput()
	if err != nil {
		t.Fatalf("council service start failed: %v, out: %s", err, string(out))
	}

	// Always ensure cleanup on test exit
	defer func() {
		ctxClean, cancelClean := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClean()
		_ = exec.CommandContext(ctxClean, bin, "service", "stop", "--state-dir", dir, "--timeout", "3s").Run()
	}()

	// 2. council service status reports ready and decodes expected diagnostics
	ctxStatus, cancelStatus := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStatus()
	cmdStatus := exec.CommandContext(ctxStatus, bin, "service", "status", "--state-dir", dir)
	outStatus, err := cmdStatus.CombinedOutput()
	if err != nil {
		t.Fatalf("council service status failed: %v, out: %s", err, string(outStatus))
	}

	// Verify status output contains required diagnostic fields
	var statusDiag struct {
		Status      string `json:"status"`
		InstanceID  string `json:"instance_id"`
		StateDir    string `json:"state_dir"`
		LiveWorkers int    `json:"live_workers"`
	}
	if err := json.Unmarshal(outStatus, &statusDiag); err != nil {
		t.Fatalf("failed to decode JSON status: %v, raw: %s", err, string(outStatus))
	}
	if statusDiag.Status != "ready" || statusDiag.StateDir != dir || statusDiag.LiveWorkers != 0 {
		t.Fatalf("unexpected status output: %+v", statusDiag)
	}

	// 3. council service stop stops the background service and cleans up
	ctxStop, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	cmdStop := exec.CommandContext(ctxStop, bin, "service", "stop", "--state-dir", dir, "--timeout", "5s")
	outStop, err := cmdStop.CombinedOutput()
	if err != nil {
		t.Fatalf("council service stop failed: %v, out: %s", err, string(outStop))
	}

	// Verify runtime discovery and socket are deleted, while service.lock remains on disk
	sockPath := filepath.Join(dir, "council.sock")
	tokenPath := filepath.Join(dir, "auth.token")
	lockPath := filepath.Join(dir, "service.lock")
	if fileExists(sockPath) || fileExists(tokenPath) {
		t.Fatal("runtime socket and token must be unlinked after CLI stop")
	}
	if !fileExists(lockPath) {
		t.Fatal("service.lock must remain on disk after CLI stop")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./cmd/council`
Expected: FAIL.

- [ ] **Step 3: Implement CLI Subcommands and Client with Context-Aware Dialing**

Implement `internal/client/client.go`:
- Reads `service.json` and `auth.token`.
- Custom UDS transport using `net.Dialer.DialContext` so socket connection respects operation deadlines.
- Dispatches authenticated requests with Bearer token header.
- Unmarshals standard JSON error envelopes.

Implement `cmd/council/service.go`:
- Common flags: `--state-dir` (defaults to current dir or env).
- `service run`: acquires lock, starts HTTP server, blocks until signal or stop.
- `service status`: reads status from client and prints formatted JSON.
- `service stop`: sends stop request (`--drain`, `--timeout`).

Implement `cmd/council/service_unix.go` (`//go:build !windows`):
- `startDetachedService`: launches `service run` with `syscall.SysProcAttr{Setsid: true}` and redirects stdio to `os.DevNull`.
- Polls `GET /v1/readiness` using context-aware client until ready or early child termination.

Implement `cmd/council/service_windows.go` (`//go:build windows`):
- `startDetachedService`: returns explicit unsupported-platform error `ErrUnsupportedPlatform`.

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

- [ ] **Step 1: Write acceptance tests using deterministic gates and boundary scenarios**

Create `internal/service/acceptance_test.go`:
1. `TestAcceptance_TwoClientDisconnect_Deterministic`:
   - Independent service process running.
   - Use deterministic gate (channel latch in test adapter) rather than simulated delay.
   - Client A releases a turn through adapter; releases succeeds with 202 Accepted.
   - Client A process terminates immediately while work is held at the latch.
   - Release the latch: worker completes execution and durably commits terminal outcome to SQLite.
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
   - Restarted service calls `reconcile`: allocates recovery episode via `RecordHostLoss` if not open, attaches to original binding, probes independent evidence, and authoritatively resolves.
4. `TestAcceptance_ReconcileRetry_LostCompositeResponse`:
   - Service performs reconciliation, commits outcome, but client response is lost.
   - Client retries with same `op_id`: service returns committed receipt without opening fresh host-loss episodes or re-probing adapter.
5. `TestAcceptance_IdleStop_ReleaseRace`:
   - Stop request races with release acceptance: under admission lock, release is either rejected (503) or admitted and tracked before stopping.
6. `TestAcceptance_ConcurrentStartupRace`:
   - Two concurrent `service start` invocations against the same state directory.
   - Exactly one owner succeeds; loser fails with `already running` and leaves winner's files untouched.
7. `TestAcceptance_ReleaseRetryDuringDrain`:
   - Service enters `draining`.
   - Matching retry of already-committed release returns 200 OK without additional dispatch.
8. `TestAcceptance_SignalGraceExpiry_PreservesUnresolved`:
   - Signal grace expires while worker is active; service preserves unresolved outcome in SQLite without early lock release or fabricated completion.

- [ ] **Step 2: Run acceptance tests to verify execution**

Run: `go test -race -count=1 -timeout=120s -v -run TestAcceptance_ ./internal/service`
Expected: PASS.

- [ ] **Step 3: Run full repository verification**

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
