# AC-003: Service Lifetime, Local IPC, and Process Decoupling Design

- **Status**: Proposed
- **Date**: 2026-09-19
- **Issue**: [#3 (AC-003)](https://github.com/CtrlCarlitos/agent-council/issues/3)
- **Dependencies**: AC-001 (Session & Turn Contracts), AC-002 (Durable State & Recovery)
- **Target Package**: `cmd/council`, `internal/service`, `internal/client`

---

## 1. Problem Statement & Core Principles

Closing a terminal window, dropping an SSH session, or disconnecting a client tool (CLI, IDE, or future MCP bridge) currently risks killing the job owner process and terminating in-flight contributor executions.

The Agent Council service must run **independently of client connection lifetime**. 

### Core Principles
1. **Client Disconnect is Not Cancellation**: Terminating or disconnecting a client process (CLI or MCP) closes that client's connection only. Accepted turn executions belong to the service and run to completion within their authorized budget and limits.
2. **Honest Failure Recovery**: Terminating the service process while a turn's outcome is unresolved must not cause the restarted service to fabricate worker survival, invent a completion or cancellation status, or automatically redispatch the assignment without controller instruction. Unresolved reservations survive restart and continue blocking conflicting releases until explicitly reconciled.
3. **Authenticated Local Control Boundary**: Local connectivity alone does not authorize operations. The service requires an ephemeral, owner-scoped Bearer token for every request, and enforces existing controller leases and version checks on all council mutations.
4. **Single Service Owner per State Directory**: Concurrent startup attempts against the same state directory must deterministically converge on exactly one owner holding an exclusive OS lock. A losing startup attempt must fail fast without clobbering the winner's runtime endpoints, credentials, or discovery files.
5. **No Second Lifecycle State Machine**: The service layer coordinates IPC and process lifetimes; it reuses `internal/storage` authoritative relational state, receipts, idempotency keys, and recovery generations verbatim.

---

## 2. Process Architecture & Exclusivity

### 2.1 Process Models
- **Foreground Core (`council service run`)**:
  - The direct service execution engine.
  - Used for local interactive development, container entrypoints, and process supervisors (`systemd --user`).
  - Closing the terminal running `service run` will terminate the service; persistence of background work requires running via a supervisor or the detached launcher.
- **Detached Launcher (`council service start`)**:
  - Launches `council service run` as an independent, detached operating system process.
  - Linux/WSL: executes via `os/exec` with a fresh session (`SysProcAttr.Setsid = true`) and redirects standard streams (`os.DevNull` or configured log sinks). The launcher does not tie child process lifetime to its own context.
  - Monitors the child process for early exit and polls authenticated readiness (`GET /v1/readiness`) up to a bounded startup timeout.
  - Returns success only after verified readiness. If the child exits early or the deadline expires, reports sanitized diagnostic failure without claiming success or relaunching.
  - Native Windows: returns an explicit unsupported-platform error until native Windows named-pipe IPC and process detachment are implemented and verified.
- **Client Commands (`council service status`, `council service stop`, council turn operations)**:
  - Ephemeral client utilities that read discovery metadata (`service.json`) and credentials (`auth.token`), communicate over local IPC, and exit.
  - Ordinary client commands **never** implicitly spawn a background service.

### 2.2 Exclusivity & State Directory Locking
Exclusivity is anchored by an operating-system-held lock on `service.lock` in the root of the state directory:
1. `service.lock` is created with mode `0600` inside the `0700` state directory.
2. The service process acquires an exclusive, non-blocking lock:
   - Linux/WSL: `syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)`.
   - The file descriptor is marked `FD_CLOEXEC` to prevent inheritance by worker subprocesses.
3. If contention occurs, startup immediately fails with `ErrServiceAlreadyRunning`.
4. **Lock Invariant**: `service.lock` is **never unlinked or deleted** during normal cleanup. Unlinking an open file on Unix leaves the inode active while giving subsequent processes a new inode to lock, breaking exclusivity. The lock handle is closed and released last upon exit.
5. A stale PID or timestamp in discovery files never authorizes breaking an active OS lock.

### 2.3 Runtime Discovery & Credential Separation
To prevent credential leakage during diagnostic collection, metadata and secret tokens are strictly separated:

1. **`service.lock`**:
   - Empty or minimal header file; authority is the OS flock, not the file content.
2. **`service.json` (Mode `0600`)**:
   - Published only after successful startup, storage hydration, listener binding, and credential creation.
   - Non-secret discovery payload:
     ```json
     {
       "protocol_version": 1,
       "instance_id": "550e8400-e29b-41d4-a716-446655440000",
       "pid": 12345,
       "transport": "unix",
       "endpoint": "/path/to/state/council.sock",
       "started_at": "2026-09-19T20:00:00Z"
     }
     ```
3. **`auth.token` (Mode `0600`)**:
   - Generated freshly on every service start using 32 bytes from `crypto/rand` encoded as hex.
   - Written as a complete file before `service.json` is published.
   - Cached in-memory by the service; never reread from disk during serving.
   - Never printed in `service status`, logs, diagnostics, or model prompts.
4. **`council.sock` (Mode `0600`)**:
   - UNIX domain socket located in the `0700` state directory.
   - Before binding, the service validates that the endpoint path matches expectations and unlinks only a stale socket file belonging to this runtime location. An unexpected directory, regular file, or symlink is a fatal error.

---

## 3. Communication Protocol & HTTP API

The local control interface uses standard HTTP/1.1 with JSON requests/responses for commands and Server-Sent Events (SSE) for live observation.

### 3.1 Authentication & Authorization
- **Service Authentication**: Every incoming HTTP request must include `Authorization: Bearer <auth.token>`. Mismatches return `401 Unauthorized` with `WWW-Authenticate: Bearer`.
- **Council Authorization**: Mutations additionally require valid controller credentials (`controller_lease`), expected version tags (`expected_version`), and resource correlation. Mismatches return `403 Forbidden` or `409 Conflict`.
- **Request Bounding**: All request bodies are wrapped with `http.MaxBytesReader` (max 10MB) to protect against memory exhaustion.

### 3.2 Error Envelope
All error responses adhere to a uniform structure:
```json
{
  "error": {
    "code": "stale_version",
    "message": "The session version has advanced; refresh state before retrying.",
    "op_id": "op-release-123"
  }
}
```

Standard Status Code Mapping:
- `400 Bad Request`: Malformed JSON, missing required fields, invalid identifiers.
- `401 Unauthorized`: Missing or invalid Bearer token.
- `403 Forbidden`: Insufficient controller authority or invalid controller lease.
- `404 Not Found`: Target run, session, turn, or prompt does not exist in scope.
- `409 Conflict`: Idempotency conflict, stale version, instance mismatch, or service busy.
- `413 Request Entity Too Large`: Payload exceeds body limit.
- `415 Unsupported Media Type`: Non-JSON content type where JSON is expected.
- `503 Service Unavailable`: New release requested while service is draining or stopping.

---

### 3.3 Endpoints

#### `GET /v1/readiness`
Verifies that the service is operational.
- **Headers**: `Authorization: Bearer <token>`
- **Response `200 OK`**:
  ```json
  {
    "status": "ready",
    "instance_id": "550e8400-e29b-41d4-a716-446655440000",
    "protocol_version": 1,
    "state_dir": "/path/to/state",
    "live_workers": 0,
    "reserved_turns": 1,
    "unresolved_turns": 1
  }
  ```
- **Rules**:
  - Returns `200 OK` when storage is initialized, hydration succeeded, recovery restrictions are in place, and control endpoints can respond. Unresolved turns from previous runs do not block readiness.
  - Returns `503 Service Unavailable` when the service is `draining` or `stopping`.

#### `GET /v1/status`
Returns high-level status of the council and service diagnostics.
- **Headers**: `Authorization: Bearer <token>`
- **Response `200 OK`**:
  ```json
  {
    "instance_id": "550e8400-e29b-41d4-a716-446655440000",
    "pid": 12345,
    "status": "ready",
    "started_at": "2026-09-19T20:00:00Z",
    "active_runs": ["run-1"],
    "live_workers": 0,
    "reserved_turns": 1,
    "unresolved_turns": 1
  }
  ```
- **Diagnostics**: `live_workers` counts verified active execution goroutines. `reserved_turns` counts SQLite-persisted nonterminal turns. Reopening a running database record does not indicate a live worker.

#### `POST /v1/service/stop`
Requests controlled shutdown of the service instance.
- **Headers**: `Authorization: Bearer <token>`
- **Request Body**:
  ```json
  {
    "instance_id": "550e8400-e29b-41d4-a716-446655440000",
    "drain": false
  }
  ```
- **Semantics**:
  - `instance_id` must match the current running instance; mismatches return `409 Conflict` (`error.code: "instance_mismatch"`).
  - **Idle Stop (`drain: false`)**:
    - Under the admission lock, checks if `live_workers > 0`, in-flight commits exist, or unresolved recovery blockers exist.
    - If busy: returns `409 Conflict` (`error.code: "service_busy"`) without modifying service state.
    - If idle: atomically transitions to `stopping`, closes admission, returns `202 Accepted`, and signals asynchronous teardown.
  - **Draining Stop (`drain: true`)**:
    - Atomically closes the admission gate for new releases.
    - Returns `202 Accepted` (`status: "draining"`).
    - Status, inspection, cancellation, and reconciliation endpoints remain operational.
    - Active executions finish and commit outcomes before final teardown.
    - Monotonic: repeated stop requests for the same instance return the current state without reopening admission.

#### `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/release`
Releases an approved queued prompt to execution.
- **Headers**: `Authorization: Bearer <token>`
- **Request Body**:
  ```json
  {
    "instance_id": "550e8400-e29b-41d4-a716-446655440000",
    "op_id": "op-release-123",
    "controller_lease": "lease-abc",
    "expected_version": 4
  }
  ```
- **Execution Hand-off & Retry Rules**:
  1. If service is in `draining` or `stopping` mode, check if `op_id` is an already-committed release:
     - If newly requested: return `503 Service Unavailable` (`error.code: "service_draining"`).
     - If matching retry: return the existing receipt (`200 OK`, `replayed: true`).
  2. Coordinate with release admission gate under lock.
  3. Invoke `storage.Store.ReleaseTurn(...)`:
     - Newly committed: returns receipt, registers execution responsibility with service lifecycle coordinator, and dispatches the worker outside the DB transaction. Returns `202 Accepted` with receipt and turn URL.
     - Idempotent replay: returns existing receipt. **Does not dispatch another worker**. Returns `200 OK` with `replayed: true`.
     - Idempotency conflict: returns `409 Conflict`.
  4. **Context Detachment**: The worker goroutine executes under a **service-owned turn context**. It does **not** inherit `r.Context()`. Disconnecting the HTTP request has zero effect on the worker.

#### `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}`
Retrieves authoritative durable state for a turn, session, and receipts. Used by Client B upon reconnection.
- **Headers**: `Authorization: Bearer <token>`
- **Response `200 OK`**: Returns full serialized `Turn` record, associated `DispatchIntent`, latest receipt, and outcome if terminal.

#### `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/events` (SSE Observation)
Streams live observation events for an active turn.
- **Headers**: `Authorization: Bearer <token>`, `Accept: text/event-stream`
- **Stream Rules**:
  - Snapshot on connect: the service reads current authoritative turn status and emits an initial snapshot event. If the turn is already terminal, it emits the terminal event and cleanly closes the stream.
  - Bounded buffers: observer channel has bounded capacity (64 events) with write timeouts. Slow observers are disconnected without blocking workers or database commits.
  - Client disconnect: closing the SSE connection terminates the handler cleanly. Workers continue unaffected.
  - Terminal event ordering: a terminal event claiming completion or failure is emitted to SSE **only after** the corresponding storage transaction has committed.

#### `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/cancel`
Requests cancellation of an active turn.
- **Headers**: `Authorization: Bearer <token>`
- **Request Body**: `{"op_id": "...", "controller_lease": "...", "reason": "..."}`
- **Semantics**:
  - Invokes `storage.Store.RequestCancel(...)` to record the intent.
  - Signals worker context cancellation and invokes adapter `Cancel`.
  - Returns `200 OK` with receipt indicating whether cancellation was confirmed, requested, or uncertain. A cancellation request is **not** confirmation of termination.

#### `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/reconcile`
Requests reconciliation of an uncertain or interrupted turn.
- **Headers**: `Authorization: Bearer <token>`
- **Request Body**: `{"op_id": "...", "controller_lease": "..."}`
- **Semantics**:
  - The client requests reconciliation; it cannot manufacture adapter observations.
  - If no host-loss episode is open, the service durably opens one via `storage.Store.RecordHostLoss(...)` to obtain a fresh generation.
  - Invokes adapter `Reconcile` using the original recovery reference and generation.
  - Authoritatively records the reconciled outcome in storage and returns the receipt.

---

## 4. Lifecycle Coordination, Draining, and Shutdown

### 4.1 Lifecycle States
```
   [ RUNNING ] --(stop idle accepted)-----------------------------> [ STOPPING ]
        |                                                                 ^
        |                                                                 |
        +-----(stop --drain OR SIGTERM/SIGINT)----> [ DRAINING ] ---------+
                                                        (all work done OR
                                                         signal grace expired)
```

- **RUNNING**: Normal operations. Release admission gate is open.
- **DRAINING**: Admission gate closed to new releases. Active executions continue. Mutation endpoints (cancel, reconcile) and inspection stay open. Existing receipts remain retrievable.
- **STOPPING**: Final teardown. All command admission closed. Existing SSE connections terminated. Storage closed. Runtime files deleted. Lock released last.

### 4.2 Three Timeout Domains
1. **CLI Wait Timeout (`--timeout`)**:
   - Evaluated strictly on the client side.
   - If the CLI wait timeout expires while draining, the client reports `still draining (active: N)` or unknown status. The service continues draining unaffected.
2. **Signal Drain Grace Period (Default: 15s)**:
   - Evaluated by the service upon receiving `SIGTERM` or `SIGINT`.
   - Starts a bounded countdown for accepted work to finish naturally. Repeated signals do not reset the grace period.
   - If the grace period expires before executions complete: the service initiates bounded termination, requesting cancellation where supported, capturing confirmed outcomes, and preserving unconfirmed outcomes as unresolved.
3. **Teardown Deadline (Default: 5s)**:
   - Evaluated during final teardown in `STOPPING`.
   - Bounded context for HTTP `Shutdown()`, observer termination, and task joining.
   - If quiescence cannot be established within the teardown deadline, the service exits through a forced-exit path without fabricating completion or releasing `service.lock` prematurely.

### 4.3 Orderly Teardown Sequence
1. Close release admission gate.
2. Allow accepted executions and their outcome commits to finish (or signal grace to expire).
3. Transition to `STOPPING`: reject all new incoming HTTP requests and mutations.
4. Close existing SSE observation subscriptions.
5. Invoke `http.Server.Shutdown(ctx)` with bounded deadline to wait for in-flight handlers.
6. Join all service-owned persistence and observation tasks to reach total quiescence.
7. Close `storage.Store`.
8. Unlink `service.json`, `auth.token`, and `council.sock`.
9. Close file descriptor and release `service.lock` **last** (file remains on disk).

---

## 5. Crash Recovery & Restart Reconciliation

When the service starts up against a state directory containing pre-existing records:

1. **Hydration**:
   - `storage.Store.HydrateState(ctx)` reconstructs relational state without mutations or side effects.
2. **Uncertainty Preservation**:
   - Any turn in a nonterminal state (`running`, `cancelling`) with an unresolved dispatch intent is recognized as an **unresolved persisted reservation**.
   - The service does **not** assume the previous worker survived.
   - The service does **not** write a fake completion, failure, or cancellation row.
   - The service does **not** automatically redispatch the prompt.
3. **Episode Initiation on Reconcile**:
   - If an open recovery episode exists in hydrated state (`visibility == host_lost`, positive `active_recovery_gen`), explicit reconciliation reuses that exact generation.
   - If no recovery episode is open (e.g., hard crash while `visibility == reachable`), the service calls `storage.Store.RecordHostLoss(...)` to allocate an episode and obtain a monotonic recovery generation before probing the adapter.
   - If native bindings or adapter recovery are unavailable, the turn remains unresolved.
4. **Diagnostic Integrity**:
   - `/v1/status` reports `live_workers: 0`, `reserved_turns: 1`, `unresolved_turns: 1`.
   - The reservation blocks new releases on that session until explicitly resolved.

---

## 6. Implementation Plan (8 Tasks)

### Task 1: Exclusivity Locking, Token Credential, and Discovery Metadata
- Implement `internal/service/lock.go`: `flock(LOCK_EX|LOCK_NB)` with `FD_CLOEXEC`, stable file retention, and clean release.
- Implement `internal/service/discovery.go`: atomic writing of `service.json` (mode `0600`) and random 32-byte `auth.token` (mode `0600`).
- Implement stale socket cleanup: check path, verify not a directory/symlink, unlink only stale socket file.
- Verify loser startup cannot alter winner's runtime files.

### Task 2: HTTP Service Core, Auth Middleware, and Diagnostic Endpoints
- Implement `internal/service/server.go`: `net/http` server bound to UDS `council.sock`.
- Implement Bearer token authentication middleware with `WWW-Authenticate` challenge.
- Implement standard error envelope and `http.MaxBytesReader` request bounding.
- Implement `GET /v1/readiness` and `GET /v1/status` distinguishing `live_workers`, `reserved_turns`, and `unresolved_turns`.

### Task 3: Release Admission Gate & Retry-Safe Dispatch Coordinator
- Implement `internal/service/coordinator.go`: admission gate state machine (`running`, `draining`, `stopping`).
- Implement `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/release`:
  - Draining check: reject new releases (`503`), permit matching retries.
  - Distinguish newly committed vs replayed storage receipts.
  - Newly committed: register active execution, dispatch worker in detached service context.
  - Replayed: return existing receipt with `replayed: true` (200 OK) without dispatching.

### Task 4: Command Routing, Recovery Episodes, and Authoritative Read
- Implement `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}` (authoritative read used by reconnecting clients).
- Implement prompt queue commands (`/queue`, `/replace`, `/discard`) and `/decisions`.
- Implement `POST .../cancel`: record request, call adapter Cancel, record confirmed/uncertain outcome.
- Implement `POST .../reconcile`: allocate recovery episode via `RecordHostLoss` if needed, call adapter Reconcile, persist outcome.
- Validate controller lease, `expected_version`, and reference correlation across all mutations.

### Task 5: Server-Sent Events (SSE) Live Observation
- Implement `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/events`.
- Synchronize observer registration and initial state snapshot: emit terminal event immediately if already finished.
- Bounded event buffers (64 capacity), write timeouts, explicit disconnection on slow consumption.
- Emit durable terminal events only after SQLite transaction commit.
- Client disconnect does not cancel workers.

### Task 6: Shutdown Coordinator & Signal Handling
- Implement `POST /v1/service/stop` (`drain: false` with atomic idle check; `drain: true` with admission closure).
- Implement OS signal handler (`SIGTERM`/`SIGINT`) initiating bounded drain grace period.
- Implement strict teardown sequence: quiesce command admission -> close SSE -> HTTP shutdown -> join tasks -> close store -> delete runtime files -> release lock last.
- Implement forced termination path if grace period or teardown deadline expires.

### Task 7: Detached Background Launcher & CLI Command Suite
- Update `cmd/council`:
  - `council service run`: runs foreground core.
  - `council service start`: launches detached process via `setsid` on Linux/WSL, redirects stdio, polls `GET /v1/readiness` until ready or error.
  - `council service status`: reads discovery and displays authenticated status.
  - `council service stop [--drain]`: authenticated stop request with CLI `--timeout` wait.
- Explicit unsupported error on native Windows for background launcher.

### Task 8: End-to-End Integration & Concurrency Acceptance Suite
- Test 1: Independent client lifetime (Client A releases turn and terminates; service completes work and commits result; Client B connects, reads result; queued prompt remains queued).
- Test 2: Crash and restart recovery (kill -9 during execution; restart preserves unresolved reservation without redispatch; explicit reconcile probes and resolves).
- Test 3: Concurrent startup race (two `service start` attempts; exactly one owner; loser leaves winner intact).
- Test 4: Release retry safety (duplicate release during drain returns receipt without dispatch).
- Test 5: Idle stop vs release race (atomic rejection or tracking).
- Test 6: Signal grace expiry and forced exit preservation.
- Test 7: Test-only assembly with fake adapter (no fake adapter in production registry).
