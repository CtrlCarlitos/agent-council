# AC-003: Service Lifetime, Local IPC, and Process Decoupling Design

- **Status**: Proposed (Amended)
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

## 2. Process Architecture, Exclusivity & Platform Boundaries

### 2.1 Process Models
- **Foreground Core (`council service run`)**:
  - The direct service execution engine.
  - Used for local interactive development, container entrypoints, and process supervisors (`systemd --user`).
  - Closing the terminal running `service run` will terminate the service; persistence of background work requires running via a supervisor or the detached launcher.
- **Detached Launcher (`council service start`)**:
  - Launches `council service run` as an independent, detached operating system process.
  - Linux/WSL: executes via `os/exec` with a fresh session (`SysProcAttr.Setsid = true`) and redirects standard streams (`os.DevNull` or configured log sinks). The launcher does not tie child process lifetime to its own context.
  - Distinguishes two startup timeout outcomes:
    1. Known early child exit: captures process exit code and stderr diagnostics, reporting immediate startup failure.
    2. Readiness deadline expiration while child may remain alive: reports startup outcome unknown without claiming failure-to-start or automatically relaunching a competing owner.
  - Native Windows: returns an explicit unsupported-platform error until native Windows named-pipe IPC and process detachment are implemented and verified.
- **Client Commands (`council service status`, `council service stop`, council turn operations)**:
  - Ephemeral client utilities that read discovery metadata (`service.json`) and credentials (`auth.token`), communicate over local IPC, and exit.
  - Ordinary client commands **never** implicitly spawn a background service.

### 2.2 Platform Scope & Native Windows Boundary
- **Linux/WSL (Initial Target)**: Full support using standard-library `net.Listen("unix", ...)` and `syscall.Flock`.
- **Native Windows (Explicitly Unsupported Initially)**:
  - Service commands (`run`, `start`, `status`, `stop`) return clear unsupported-platform errors.
  - No silent fallback to localhost TCP or unauthenticated transports.
  - Windows support requires implementing named pipes with explicit security descriptors via `github.com/Microsoft/go-winio` and `LockFileEx` exclusivity in a dedicated, verified task.
  - Cross-platform compilation of the repository is preserved via build tags (`//go:build !windows`).

### 2.3 Exclusivity & State Directory Locking
Exclusivity is anchored by an operating-system-held lock on `service.lock` in the root of the state directory:
1. `service.lock` is created with mode `0600` inside the `0700` state directory.
2. The service process acquires an exclusive, non-blocking lock:
   - Linux/WSL: `syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)`.
   - The file descriptor is marked `FD_CLOEXEC` to prevent inheritance by worker subprocesses.
3. If contention occurs, startup immediately fails with `ErrServiceAlreadyRunning`.
4. **Lock Invariant**: `service.lock` is **never unlinked or deleted** during normal cleanup. Unlinking an open file on Unix leaves the inode active while giving subsequent processes a new inode to lock, breaking exclusivity. The lock handle is closed and released last upon exit.
5. A stale PID or timestamp in discovery files never authorizes breaking an active OS lock.

### 2.4 Runtime Discovery & Credential Separation
To prevent credential leakage during diagnostic collection, metadata and secret tokens are strictly separated:

1. **`service.lock`**:
   - Empty or minimal header file; authority is the OS flock, not the file content.
2. **`auth.token` (Mode `0600`)**:
   - Generated freshly on every service start using 32 bytes from `crypto/rand` encoded as hex.
   - Written as a complete protected file before `service.json` is published.
   - Cached in-memory by the service; never reread from disk during serving.
   - Never printed in `service status`, logs, diagnostics, or model prompts.
3. **`service.json` (Mode `0600`)**:
   - Published only after successful startup, storage hydration, listener binding, and credential creation.
   - Non-secret discovery payload:
     ```json
     {
       "protocol_version": 1,
       "instance_id": "550e8400-e29b-41d4-a716-446655440000",
       "pid": 12345,
       "transport": "unix",
       "endpoint": "/path/to/state/council.sock",
       "state_dir": "/path/to/state",
       "started_at": "2026-09-19T20:00:00Z"
     }
     ```
4. **`council.sock` (Mode `0600`)**:
   - UNIX domain socket located in the `0700` state directory.
   - Before binding, the service validates that the endpoint path matches expectations and unlinks only a stale socket file belonging to this runtime location. An unexpected directory, regular file, or symlink is a fatal error.
5. **Startup Failure & Loser Cleanliness**:
   - A losing startup process (failing to acquire `service.lock`) touches none of the winner's files (`service.json`, `auth.token`, `council.sock`).
   - If an owning startup fails mid-initialization (e.g. storage error, listener error), it unwinds only the runtime resources it created, closes storage, and releases `service.lock`.

---

## 3. Communication Protocol, Endpoints & Contracts

The local control interface uses HTTP/1.1 with JSON requests/responses for commands and Server-Sent Events (SSE) for live observation.

### 3.1 Authentication, Decoding & Error Envelopes
- **Service Authentication**: Every incoming HTTP request must include `Authorization: Bearer <auth.token>`. Mismatches return `401 Unauthorized` with `WWW-Authenticate: Bearer`. The token is a trusted local operator capability.
- **Council Authorization**: Mutations additionally require valid controller credentials (`controller_lease`), expected version tags (`expected_version`), and resource correlation. Mismatches return `403 Forbidden` or `409 Conflict`.
- **Request Bounding & Strict Decoding**:
  - Request bodies: wrapped with `http.MaxBytesReader` (max 10MB).
  - Request headers: bounded by `http.Server.MaxHeaderBytes` (1MB).
  - Strict JSON: `json.Decoder.DisallowUnknownFields()`, followed by verifying EOF (rejecting trailing data).
- **Error Envelope**:
  All errors return a uniform structure with sanitized diagnostics (no raw SQL, internal paths, or tokens):
  ```json
  {
    "error": {
      "code": "stale_version",
      "message": "The session version has advanced; refresh state before retrying.",
      "op_id": "op-release-123"
    }
  }
  ```

### 3.2 Command Concurrency & Correlation Inputs
Controller commands follow a strict contract mapping:

| Operation | Route | Method | Required Concurrency & Correlation Inputs |
|---|---|---|---|
| Connect Controller | `/v1/runs/{run_id}/sessions/{session_id}/controller/connect` | POST | `op_id`, `controller_lease`, `expected_version` (session-scoped version check) |
| Queue Prompt | `/v1/runs/{run_id}/sessions/{session_id}/prompts/queue` | POST | `op_id`, `controller_lease`, `expected_version`, `prompt`, `turn_key` |
| Replace Prompt | `/v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/replace` | POST | `op_id`, `controller_lease`, `expected_version`, `prompt` |
| Discard Prompt | `/v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/discard` | POST | `op_id`, `controller_lease`, `expected_version` |
| Record Decision | `/v1/runs/{run_id}/decisions` | POST | `op_id`, `controller_lease`, `artifact_id`, `revision`, `decision_payload` |
| Release Turn | `/v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/release` | POST | `op_id`, `controller_lease`, `expected_version` |
| Read Turn State | `/v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}` | GET | Read-only; queries turn, intent, receipts, and execution status |
| Cancel Request | `/v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/cancel` | POST | `op_id`, `controller_lease`, `expected_version`, `reason` |
| Reconcile | `/v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/reconcile` | POST | `op_id`, `controller_lease` (validates against current durable turn state & recovery generation) |

- **Controller Reattachment**: Explicitly performed via `/v1/runs/{run_id}/sessions/{session_id}/controller/connect` targeting a specific session and validating against that session's version. Readiness probes, status queries, and SSE subscriptions **never** implicitly renew leases or reattach controllers.

---

### 3.3 Endpoints Specification

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
  - The launcher validates that `instance_id`, `protocol_version`, and `state_dir` match expectations.
  - Returns `200 OK` when storage is initialized, hydration succeeded, recovery restrictions are installed, and control endpoints can respond. Unresolved turns from previous runs do not block readiness.
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
- **Request Body**: `{"instance_id": "<uuid>", "drain": false | true}` (CLI wait `--timeout` is client-side).
- **Semantics**:
  - `instance_id` must match current running instance; mismatches return `409 Conflict` (`error.code: "instance_mismatch"`).
  - **Idle Stop (`drain: false`)**:
    - Under the admission lock, checks if `live_workers > 0`, in-flight commits exist, or unresolved recovery blockers exist.
    - If busy: returns `409 Conflict` (`error.code: "service_busy"`) without modifying service state.
    - If idle: atomically transitions to `stopping`, closes admission, returns `202 Accepted`, and signals asynchronous teardown.
  - **Draining Stop (`drain: true`)**:
    - Atomically closes admission for new turn releases.
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
- **Storage Release Contract & Disposition**:
  1. **Authentication & Scope Validation**: Validates Bearer token, controller lease, and resource scope.
  2. **Idempotent Retry Resolution**: Resolves whether `op_id` corresponds to an already-committed release operation for this turn. If found, returns the existing receipt immediately (`200 OK`, `replayed: true`) without checking harness availability or admission draining state.
  3. **Admission Coordination & Pre-flight Availability Check (New Releases Only)**:
     - Under the admission lock, if the service is `draining` or `stopping`, rejects new release requests with `503 Service Unavailable` (`error.code: "service_draining"`).
     - Verifies that the required native harness adapter and saved session binding are available; if not, rejects immediately with `503 Service Unavailable` (`error.code: "harness_unavailable"`) without releasing the turn or consuming the prompt.
  4. **Atomic Execution Hand-off**:
     - `storage.Store.ReleaseTurn(...)` transaction returns both the immutable `ReleaseReceipt` and a distinct disposition:
       - `ReleaseDispositionNew`: Newly committed reservation. The coordinator registers execution responsibility and dispatches the worker outside the DB transaction. Returns `202 Accepted` with receipt and authoritative turn URL.
       - `ReleaseDispositionReplayed`: Idempotent replay of an existing operation (e.g. concurrent race resolved by database transaction). **Does not dispatch a worker**. Returns `200 OK` with `replayed: true`.
     - Registration does not depend on writing the HTTP response; if the client disconnects immediately after commit, the coordinator still owns the execution.
  5. **Context Detachment**: The worker executes under a **service-owned turn context**. It does **not** inherit `r.Context()`. Disconnecting the HTTP request has zero effect on the worker.

#### `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}`
Retrieves authoritative durable state for a turn, session, and receipts. Used by Client B upon reconnection.
- **Headers**: `Authorization: Bearer <token>`
- **Response `200 OK`**: Returns full serialized `Turn` record, associated `DispatchIntent`, latest receipt, and outcome if terminal.

#### `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/events` (SSE Observation)
Streams live observation events for an active turn.
- **Headers**: `Authorization: Bearer <token>`, `Accept: text/event-stream`
- **Stream Rules**:
  - Best-effort live progress; no durable replay promise. Unsupported `Last-Event-ID` requests are reset/rejected.
  - Synchronized snapshot on connect: the service reads current authoritative turn status and registers the subscriber. If the turn is already terminal (or finishes between snapshot and subscription), it emits the terminal snapshot event immediately and cleanly closes the stream.
  - Bounded buffers: observer channel has bounded capacity (64 events) with write timeouts. Slow observers are disconnected without blocking workers or database commits.
  - Client disconnect: closing the SSE connection terminates the handler cleanly. Workers continue unaffected.
  - **Commit-Before-Delivery**: A terminal event claiming completion, cancellation, or failure is emitted to SSE **only after** the corresponding storage transaction has committed.

#### `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/cancel`
Requests cancellation of an active turn.
- **Headers**: `Authorization: Bearer <token>`
- **Request Body**:
  ```json
  {
    "op_id": "op-cancel-123",
    "controller_lease": "lease-abc",
    "expected_version": 4,
    "reason": "operator requested cancel"
  }
  ```
- **Semantics**:
  - Validates controller lease and expected version.
  - Invokes `storage.Store.RequestCancel(...)` using internal stage ID `<op_id>:req` to record the intent.
  - Signals worker context cancellation and invokes adapter `Cancel` using an independently bounded control context. Cancellation of execution does not disable observation or persistence.
  - Returns `200 OK` containing both `receipt` (original committed request acceptance) and `cancellation_status` (latest verified status: `requested`, `confirmed`, `unsupported`, or `uncertain`). A cancellation request is **not** confirmation of termination.

#### `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/reconcile`
Requests reconciliation of an uncertain or interrupted turn.
- **Headers**: `Authorization: Bearer <token>`
- **Request Body**:
  ```json
  {
    "op_id": "op-reconcile-123",
    "controller_lease": "lease-abc"
  }
  ```
- **Composite Command Convention**:
  - Public operation ID `op_id` links multi-stage durable records.
  - If no recovery episode is open, the service durably opens one via `storage.Store.RecordHostLoss(..., op_id+":host_loss", ...)` to obtain a fresh monotonic generation.
  - Invokes adapter `Reconcile` using the original recovery reference and generation.
  - Authoritatively records the reconciled outcome via `storage.Store.ReconcileSession(..., op_id+":reconcile", ...)` and returns the committed receipt.
  - Reconnection retry with the same `op_id` returns the committed result without opening fresh episodes.

---

### 3.4 Adapter Lifecycle & Outcome Mapping

| Adapter Result | Required Service Treatment |
|---|---|
| Dispatch accepted (`DispatchAccepted`) | Record acknowledgement intent; retain reservation; supervise outcome collection |
| Dispatch acceptance unknown (`DispatchUnknown`) | Record uncertainty intent; do not retry dispatch; retain reservation |
| Dispatch definitively rejected (`DispatchRejected`) | Record non-execution resolution without manufacturing a successful run; release reservation cleanly |
| Observation ends or errors | End/recover observation stream; do not infer task termination |
| Authoritative terminal outcome obtained | Persist to SQLite via `RecordTerminalOutcome` before announcing durable completion |
| Result persistence fails | Retain unresolved persistence responsibility; do not report successful drain |

- **Native Session Binding**: Explicit recovery loads the saved logical/native binding and approved configuration, invoking the adapter's supported resume/recovery mechanism against the original execution. It **never** calls `CreateSession` as a silent substitute for a missing session.
- **Post-Terminal Recovery Blockers**: A parked contributor with an outstanding reachability episode (`visibility == host_lost`) remains a recovery and shutdown blocker, even though its turn is already terminal.

---

## 4. Lifecycle Coordination, Draining, and Teardown Synchronization

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
- **STOPPING**: Final teardown. All command admission closed. Existing SSE connections terminated. Storage closed. Runtime files deleted. Lock released last. Read-only inspection or receipt retrieval may finish while HTTP remains available, but clients are not promised availability after listener shutdown begins.

### 4.2 Three Timeout Domains
1. **CLI Wait Timeout (`--timeout`)**:
   - Evaluated strictly on the client side.
   - If the CLI wait timeout expires while draining, the client reports `still draining (active: N)` or unknown status. The accepted service drain operation continues unaffected.
2. **Signal Drain Grace Period (Default: 15s)**:
   - Evaluated by the service upon receiving `SIGTERM` or `SIGINT`.
   - Starts a bounded countdown for accepted work to finish naturally. Repeated signals retain the original deadline.
   - If the grace period expires before executions complete: the service initiates bounded termination handling, requesting cancellation where supported, capturing confirmed outcomes, and preserving unconfirmed outcomes as unresolved.
3. **Final Teardown Deadline (Default: 5s)**:
   - Evaluated during final teardown in `STOPPING`.
   - Limits the time to wait for HTTP `Shutdown()`, observer termination, and joining active tasks.
   - If orderly quiescence cannot be established within the teardown deadline, the service exits through a forced-exit path without fabricating completion and **without releasing `service.lock` prematurely** while old tasks may still mutate state.

### 4.3 Shutdown Eligibility Definition
The coordinator considers the service eligible for final teardown only when all of the following reach zero:
1. Accepted-but-not-started release handoffs.
2. Active live worker executions.
3. Pending terminal outcome database commits.
4. Admitted in-flight cancellation or reconciliation jobs.
5. Outstanding unresolved recovery blockers (including post-terminal reachability episodes).

### 4.4 Orderly Teardown Sequence
1. Close release admission gate.
2. Drain accepted executions and their outcome commits (or signal grace expires).
3. Transition to `STOPPING`: reject all new incoming HTTP requests, mutations, and subscriptions.
4. Close existing SSE observation subscriptions.
5. Invoke `http.Server.Shutdown(ctx)` with bounded deadline to wait for in-flight handlers.
6. Join all service-owned execution, persistence, and observation tasks to reach total quiescence.
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
- Implement failed-start cleanup and verify loser startup cannot alter winner's runtime files.

### Task 2: HTTP Service Core, Auth Middleware, and Diagnostic Endpoints
- Implement `internal/service/server.go`: `net/http` server bound to UDS `council.sock`.
- Implement Bearer token authentication middleware with `WWW-Authenticate` challenge.
- Implement standard error envelope, `http.MaxBytesReader` (10MB), and header limits (1MB).
- Implement `GET /v1/readiness` and `GET /v1/status` distinguishing `live_workers`, `reserved_turns`, and `unresolved_turns`.

### Task 3: Release Admission Gate & Retry-Safe Dispatch Coordinator
- Extend `storage.Store.ReleaseTurn` to return `ReleaseResult{Receipt: receipt, Disposition: ReleaseDisposition}`.
- Implement `internal/service/coordinator.go`: admission gate state machine (`running`, `draining`, `stopping`).
- Implement `POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/release`:
  - Pre-flight check: reject if adapter or native binding is unavailable.
  - Draining check: reject new releases (`503`), permit matching retries.
  - Distinguish newly committed vs replayed storage receipts:
    - Newly committed: register execution, dispatch worker in detached service context.
    - Replayed: return existing receipt with `replayed: true` (200 OK) without dispatching.

### Task 4: Command Routing, Recovery Episodes, and Authoritative Read
- Implement `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}` (authoritative read used by reconnecting clients).
- Implement `POST /v1/runs/{run_id}/controller/connect` for explicit controller connection.
- Implement prompt queue commands (`/queue`, `/replace`, `/discard`) and `/decisions`.
- Implement `POST .../cancel`: record request, call adapter Cancel with independent context, record confirmed/uncertain outcome.
- Implement `POST .../reconcile`: allocate recovery episode via `RecordHostLoss` if needed, call adapter Reconcile, persist outcome.
- Validate controller lease, `expected_version`, and reference correlation across all mutations.

### Task 5: Server-Sent Events (SSE) Live Observation
- Implement `GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/events`.
- Synchronize observer registration and initial state snapshot: emit terminal event immediately if already finished (no hanging).
- Bounded event buffers (64 capacity), write timeouts, explicit disconnection on slow consumption.
- Emit durable terminal events only after SQLite transaction commit.
- Client disconnect does not cancel workers.

### Task 6: Shutdown Coordinator & Signal Handling
- Implement `POST /v1/service/stop` (`drain: false` with atomic idle check; `drain: true` with admission closure).
- Implement OS signal handler (`SIGTERM`/`SIGINT`) initiating bounded drain grace period (15s); grace expiry starts bounded termination handling.
- Implement strict teardown sequence: quiesce command admission -> close SSE -> HTTP shutdown -> join tasks -> close store -> delete runtime files -> release lock last.
- Implement forced termination path if orderly quiescence cannot be reached within the final teardown deadline (retaining lock until process termination).

### Task 7: Detached Background Launcher & CLI Command Suite
- Update `cmd/council`:
  - `council service run`: runs foreground core.
  - `council service start`: launches detached process via `setsid` on Linux/WSL, redirects stdio, polls `GET /v1/readiness` until ready or error (distinguishes early child exit from timeout).
  - `council service status`: reads discovery and displays authenticated status.
  - `council service stop [--drain]`: authenticated stop request with CLI `--timeout` wait.
- Explicit unsupported error on native Windows for all service commands.

### Task 8: End-to-End Integration & Concurrency Acceptance Suite
- **Acceptance Evidence Partitioning**:
  - Test 1: Independent client lifetime (Client A releases turn and terminates; service completes work and commits result; Client B connects, reads result; queued prompt remains queued).
  - Test 2: Crash and restart recovery without accessible native evidence (kill -9 during execution; restart preserves unresolved reservation without redispatch).
  - Test 3: Restart with independently retained test-harness evidence (explicit recovery attaches to original binding and resolves from independently retained evidence via test helper process).
  - Test 4: Concurrent startup race (two `service start` attempts; exactly one owner; loser leaves winner intact).
  - Test 5: Release retry safety (duplicate release during drain returns receipt without dispatch).
  - Test 6: Idle stop vs release race (atomic rejection or tracking).
  - Test 7: Signal grace expiry and forced exit preservation without early lock release.
  - Test 8: Test-only assembly with fake adapter (no fake adapter in production registry).
