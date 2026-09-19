# Design Specification: Durable Run Journals, Native Session Mappings, Artifacts, and Recovery State (AC-002)

Status: Draft (Amended)  
Date: 2026-09-19  
Issue: [AC-002 (#2)](https://github.com/CtrlCarlitos/agent-council/issues/2)  
Target Branch: `feat/ac-002-durable-state`  

---

## 1. Problem and Architectural Context

Agent Council is a controller-led system where four independent native harness contributors execute under explicit, deterministic governance ([ARCHITECTURE.md](../../ARCHITECTURE.md)). In-memory domain invariants were established in [AC-001 (PR #26)](https://github.com/CtrlCarlitos/agent-council/pull/26) and native harness adapter contracts with observation streams were established in [AC-006 (PR #27)](https://github.com/CtrlCarlitos/agent-council/pull/27).

However, an in-memory domain kernel or native session ID alone cannot survive a process termination, machine reboot, or client disconnect. If the Council service terminates while:
1. Contributor sessions have established native conversation bindings,
2. Several turns have completed while others are queued,
3. A prompt dispatch was issued whose acceptance by the external provider is uncertain, or
4. Proposals, ballots, and synthesized findings have been produced,

the state must not be lost, silently reset, or corrupted. Nor may a restarted process convert uncertain external execution into an unearned success or a reckless automatic duplicate dispatch.

### 1.1 The Anchor Acceptance Story
> **Process A** records a council run with distinct contributor sessions, completed turn results, pending prompts, and one turn whose dispatch outcome is uncertain. **Process A terminates abruptly.**  
> **Process B** opens the same storage directory and reconstructs that state with fidelity without starting any additional work or issuing duplicate prompts.

### 1.2 Core Architectural Rule
> **Database state is authoritative; journal entries accompany accepted transitions; artifacts are immutable and verified; and restart never converts uncertainty into permission to execute.**

---

## 2. Package Structure and External Dependencies

```
internal/
  council/                         # Core domain kernel (Session, TurnRecord, Contributor, State)
  adapter/                         # Harness adapter contract, Stream, TurnRef, RecoveryRef
  storage/                         # AC-002 Persistent Storage Subsystem
    store.go                       # Store interface, Open, Close, lifecycle, connection PRAGMAs
    types.go                       # Store models, error sentinels, row versions, intent phases
    schema.sql                     # Embedded SQL schema (v1)
    migrations.go                  # Transactional migration runner with checksum verification
    tx.go                          # Connection-bound transaction wrapper (BEGIN IMMEDIATE)
    session_store.go               # Authoritative CRUD & atomic transitions for sessions, turns, prompts
    journal.go                     # Append-only audit journal entries for state transitions
    artifact_store.go              # Protected content-addressed artifact repository
    recovery.go                    # Hydration into *council.Session and uncertain dispatch evaluation
    redaction.go                   # Sensitive data scanning and credential pattern exclusion
    store_test.go                  # Separate-process reopen, termination, concurrency, rollback tests
    artifact_test.go               # Artifact two-phase commit, tampering, and missing-file tests
    migration_test.go              # New database, checksum mismatch, and schema version tests
```

### 2.1 Dependency and Driver Pinning
- **Driver**: `modernc.org/sqlite` accessed strictly through Go standard library `database/sql`. No dual-driver or pluggable runtime driver abstraction is included.
- **SQLite Engine Version**: Must embed a patched SQLite engine addressing the WAL-reset corruption bug (fixed in SQLite 3.51.3+ on March 13, 2026).
  - Selected Driver Pin: `modernc.org/sqlite v1.59.0` (embedding **SQLite 3.53.4**, which is well past the fix boundary).
  - Required Matching Dependency: `modernc.org/libc v1.75.7`.
  - Go Baseline Requirement: `go 1.25.0` (explicitly recorded in `go.mod`, CI workflows, and developer documentation to match driver requirements).
  - Verification: Engine version must be verified on startup via `SELECT sqlite_version();`.
- **Dual CI Verification Path**:
  - **Distribution Compatibility**: `CGO_ENABLED=0` builds and tests verify pure-Go cross-platform compilation across Linux, macOS, and Windows.
  - **Concurrency Verification**: `CGO_ENABLED=1` race tests (`go test -race ./...`) verify absence of data races under Go's race detector.

---

## 3. Database Schema and Integrity Invariants

The schema is defined in `internal/storage/schema.sql`. SQLite foreign key enforcement is mandatory (`PRAGMA foreign_keys = ON;`). All primary key identifiers are explicitly declared `NOT NULL`.

```sql
-- Schema Migration Tracking
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER NOT NULL PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL -- ISO 8601 UTC (RFC3339Nano)
);

-- Council Runs (Top-level governance scope)
CREATE TABLE IF NOT EXISTS runs (
    run_id TEXT NOT NULL PRIMARY KEY,
    brief_digest TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    profile_digest TEXT NOT NULL,
    controller_lease TEXT NOT NULL, -- Sole authoritative run-level controller lease
    lifecycle TEXT NOT NULL CHECK (lifecycle IN ('active', 'archived')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Contributor Sessions (Logical session identities distinct from contributor roles)
CREATE TABLE IF NOT EXISTS sessions (
    session_id TEXT NOT NULL PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    contributor TEXT NOT NULL CHECK (contributor IN ('opencode', 'claude', 'codex', 'agy')),
    is_active_contributor INTEGER NOT NULL DEFAULT 0 CHECK (is_active_contributor IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('parked', 'running', 'archived')),
    lifecycle TEXT NOT NULL CHECK (lifecycle IN ('active', 'archived')),
    controller_status TEXT NOT NULL CHECK (controller_status IN ('connected', 'disconnected')),
    visibility TEXT NOT NULL CHECK (visibility IN ('reachable', 'host_lost')),
    active_key TEXT,
    recovery_context TEXT,
    recovery_gen INTEGER NOT NULL CHECK (recovery_gen >= 0 AND recovery_gen <= 9223372036854775807),
    active_recovery_gen INTEGER NOT NULL CHECK (active_recovery_gen >= 0 AND active_recovery_gen <= 9223372036854775807),
    row_version INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- At most one active primary session per contributor per run for active voting/participation
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_active_contributor 
    ON sessions(run_id, contributor) WHERE is_active_contributor = 1;

-- Native Harness Bindings (Mapping logical sessions to external harness conversations)
CREATE TABLE IF NOT EXISTS native_bindings (
    session_id TEXT NOT NULL PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
    native_session_id TEXT NOT NULL,
    harness TEXT NOT NULL,
    model TEXT NOT NULL,
    config_json TEXT NOT NULL, -- Allowlisted configuration; no provider credentials
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Pending Prompts (Queued prompts awaiting authorized controller release)
CREATE TABLE IF NOT EXISTS pending_prompts (
    session_id TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    turn_key TEXT NOT NULL,
    prompt TEXT NOT NULL,
    queued_at TEXT NOT NULL,
    PRIMARY KEY (session_id, turn_key)
);

-- Turn History and Active Turns
CREATE TABLE IF NOT EXISTS turns (
    session_id TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    turn_key TEXT NOT NULL,
    prompt TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'completed', 'interrupted', 'cancelling', 'cancelled', 'failed')),
    result TEXT NOT NULL DEFAULT '',
    attempt_id TEXT NOT NULL, -- Immutable execution-attempt identity for this released turn
    created_at TEXT NOT NULL,
    completed_at TEXT,
    PRIMARY KEY (session_id, turn_key)
);

-- Dual-Phase Dispatch Intent Logging (Boundary between DB reservation and external harness)
CREATE TABLE IF NOT EXISTS dispatch_intents (
    session_id TEXT NOT NULL,
    turn_key TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (phase IN ('intent_recorded', 'receipt_acknowledged', 'acceptance_unknown', 'resolved')),
    recorded_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (session_id, turn_key),
    FOREIGN KEY (session_id, turn_key) REFERENCES turns(session_id, turn_key) ON DELETE CASCADE
);

-- Append-Only Audit Journal (Accompanies accepted state transitions; not for event-sourcing replay)
CREATE TABLE IF NOT EXISTS journal_entries (
    seq INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    op_id TEXT NOT NULL UNIQUE, -- Idempotency key
    command_type TEXT NOT NULL, -- Command kind (e.g. 'release_turn', 'queue_prompt')
    command_fingerprint TEXT NOT NULL, -- Hash of canonical command input parameters
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    session_id TEXT REFERENCES sessions(session_id) ON DELETE RESTRICT,
    turn_key TEXT,
    event_kind TEXT NOT NULL,
    payload_version INTEGER NOT NULL DEFAULT 1,
    payload_json TEXT NOT NULL, -- Bounded JSON payload and committed receipt
    created_at TEXT NOT NULL
);

-- Immutable Artifact Revisions
CREATE TABLE IF NOT EXISTS artifact_revisions (
    artifact_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision >= 1),
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    kind TEXT NOT NULL, -- 'proposal', 'ballot', 'synthesis', 'finding', 'patch'
    digest TEXT NOT NULL, -- SHA-256 hex digest of file bytes
    byte_size INTEGER NOT NULL CHECK (byte_size >= 0),
    created_at TEXT NOT NULL,
    PRIMARY KEY (artifact_id, revision)
);

CREATE INDEX IF NOT EXISTS idx_turns_session_status ON turns(session_id, status);
CREATE INDEX IF NOT EXISTS idx_journal_run_seq ON journal_entries(run_id, seq);
CREATE INDEX IF NOT EXISTS idx_artifacts_digest ON artifact_revisions(digest);
```

### 3.1 Checked Integer & Time Conversions
- **Recovery Generations**: The domain kernel defines `RecoveryGeneration uint64`. In SQLite, integer values are signed 64-bit (`INTEGER`). All conversions are strictly checked: values exceeding `math.MaxInt64` (`9223372036854775807`) return an explicit `ErrRecoveryGenerationOverflow`. No wraparound or truncation is permitted.
- **Timestamps**: All timestamps are formatted in UTC ISO 8601 / RFC 3339 with nanosecond precision (`time.RFC3339Nano`).

---

## 4. Transaction Boundaries, Command Pipeline, and Idempotency

All database modifications execute as short, connection-bound transactions using `BEGIN IMMEDIATE`. Transactional commands enforce the complete set of domain guards established in `internal/council`.

### 4.1 Mutation Rules and Stale-Update Protection
- **Transaction-Local Mutation**: A transaction-local domain object (`*council.Session`) may be mutated to evaluate domain invariants before commit. Shared or caller-visible state is updated *only* after a successful database commit. Rollback discards all transaction-local state.
- **Optimistic Locking (`row_version`)**: Every mutation to a session or its associated child entities (`turns`, `pending_prompts`, `dispatch_intents`, `native_bindings`) increments `sessions.row_version`:
  ```sql
  UPDATE sessions 
  SET state = ?, active_key = ?, visibility = ?, recovery_context = ?,
      recovery_gen = ?, active_recovery_gen = ?, row_version = row_version + 1, updated_at = ?
  WHERE session_id = ? AND row_version = ?;
  ```
  If 0 rows are updated, the transaction rolls back and returns `ErrStaleUpdate`.

### 4.2 Strict Command Idempotency Pipeline
Every modifying command receives a unique operation identifier `op_id`. While an initial read check may serve as a fast path, the **authoritative idempotency evaluation occurs inside the write transaction**:
1. Begin connection-bound write transaction (`_txlock=immediate`).
2. Query `journal_entries` for an existing `op_id`:
   - **Matching Operation**: If `op_id` exists, verify that its `command_type` and `command_fingerprint` match the incoming request. If caller lease matches, extract and return the original committed receipt from `payload_json` immediately. No new mutations or journal entries are created. This inside-transaction evaluation ensures that genuine retries of committed operations return their original receipt without being falsely rejected as stale updates.
   - **Idempotency Conflict**: If `op_id` exists with a different `command_type`, target `session_id`, or mismatched `command_fingerprint`, return `ErrIdempotencyConflict`.
   - **Authority Check**: If caller lease or credentials do not match the original command authority, return `ErrUnauthorizedOperation`.
3. If `op_id` is absent:
   - Validate caller lease, expected `row_version`, and complete domain guards against current database records.
   - Apply mutations to transaction-local state.
   - Persist state mutations, dispatch intent, and append the operation's committed receipt in `journal_entries`.
   - Commit transaction and return the committed receipt.

### 4.3 Transaction Command Boundaries

| Command | Pre-Conditions & Domain Checks | Atomic DB Mutations | Journal Event |
| :--- | :--- | :--- | :--- |
| **`CreateRun`** | Non-empty digests, valid lease, unique `run_id`. | Insert `runs`. | `run_created` |
| **`CreateSession`** | Valid contributor, active run, lease match. | Insert `sessions`, set `is_active_contributor = 1`. | `session_created` |
| **`SetNativeBinding`** | Valid session, allowlisted config (no provider keys). | Insert / update `native_bindings`, bump session `row_version`. | `binding_updated` |
| **`QueuePrompt`** | Valid lease, `Lifecycle == active`, `ControllerStatus == connected`. Key not in `Turns` with different prompt, not currently active. | Insert `pending_prompts` (idempotent if identical), bump session `row_version`. | `prompt_queued` |
| **`ReplacePendingPrompt`** | Valid lease, key exists in `pending_prompts`. | Update `pending_prompts`, bump session `row_version`. | `prompt_replaced` |
| **`DiscardPendingPrompt`** | Valid lease, key exists in `pending_prompts`. | Delete from `pending_prompts`, bump session `row_version`. | `prompt_discarded` |
| **`ReleaseTurn`** | Valid lease, `State == Parked`, `Visibility == Reachable` (rejects if `VisibilityHostLost`!), `ActiveTurn == nil`. | Delete from `pending_prompts`; insert into `turns` (`status = 'running'`); insert into `dispatch_intents` (`phase = 'intent_recorded'`); update `sessions` (`active_key = turn_key, state = 'running'`, bump `row_version`). | `turn_released` |
| **`RecordDispatchObservation`** | Active turn matches, intent phase not `resolved`. Late arrival rule: if intent already `resolved`, observation has no effect. | Update `dispatch_intents.phase` (`receipt_acknowledged` or `acceptance_unknown`), bump session `row_version`. | `dispatch_observed` |
| **`RequestCancel`** | Valid lease, `State == Running`, `ActiveTurn.Status == TurnRunning`. | Update `turns.status = 'cancelling'`, bump session `row_version`. | `cancel_requested` |
| **`RecordTerminalOutcome`** | Turn matches `active_key`. Applies `CompleteWithResult`, `ConfirmCancel`, `Interrupt`, or `Fail`. | Update `turns` (`status`, `result`, `completed_at`); update `dispatch_intents.phase = 'resolved'`; update `sessions` (`active_key = NULL, state = 'parked'`, bump `row_version`). *If visibility is `VisibilityHostLost`, parks contributor but retains `recovery_context` and `active_recovery_gen`.* | `turn_completed` / `turn_cancelled` / `turn_interrupted` / `turn_failed` |
| **`RecordHostLoss`** | `State == Running`, `ActiveTurn != nil`. | Increment `recovery_gen`, set `active_recovery_gen = recovery_gen`, `visibility = 'host_lost'`, `recovery_context = active_key`, bump session `row_version`. | `host_loss_recorded` |
| **`ReconcileSession`** | Valid `RecoveryRef` (exact generation match). If malformed or uncertain: no visibility change. If valid nonterminal: `visibility = 'reachable'`, reservation preserved. If valid terminal: apply terminal outcome, resolve intent, park. | Update `sessions` (and `turns`/`intents` if terminal), bump `row_version`. | `session_reconciled` |
| **`SetControllerConnection`** | Valid lease. | Update `sessions.controller_status`, bump `row_version`. | `connection_updated` |
| **`ArchiveSession`** | Valid lease, `State == Parked`. | Update `sessions.lifecycle = 'archived', state = 'archived'`, bump `row_version`. | `session_archived` |
| **`RecordDecision`** | Valid lease. Validates that referenced `(artifact_id, revision)` exist in `artifact_revisions` for this run. | Insert `journal_entries` binding exact revision digests or proposal set manifest. | `controller_decision` |

---

## 5. Execution Uncertainty and Reopening Semantics

### 5.1 Consistent Read Hydration
When a Council process opens storage, it performs a single read-only transaction:
1. Loads `runs` to establish the authoritative `controller_lease`.
2. Loads all `sessions` records for the run keyed by logical `session_id`.
3. Loads associated `native_bindings`, `pending_prompts`, and `turns`.
4. Reconstructs each `*council.Session` domain instance:
   - Sets `s.Pending` to match current `pending_prompts`.
   - Sets `s.Turns` to match all recorded `turns`.
   - If `s.Active != ""`, sets `s.ActiveTurn = s.Turns[s.Active]` (preserving exact pointer identity).
   - Validates relational invariants: if `s.Active != ""` but no matching turn exists, or if `s.Active == ""` while a turn is `running` or `cancelling`, hydration returns `ErrInconsistentStorage`.

### 5.2 Zero Side-Effect Guarantee on Reopen
Opening storage:
- Does NOT start or redispatch background workers.
- Does NOT reconnect the controller.
- Does NOT reset controller leases.
- Does NOT increment recovery generations or create host-loss episodes.

### 5.3 Lifecycle State Preservation Across Reopen
Reopen preserves exact turn statuses without flattening:
- **Preserved Status**: A turn in `TurnCancelling` remains `TurnCancelling`. A turn in `TurnRunning` remains `TurnRunning`.
- **Reservation Guarantee**: Unresolved dispatch intent (`intent_recorded`, `receipt_acknowledged`, `acceptance_unknown`) preserves the execution reservation and the recorded turn status. It does NOT convert `TurnCancelling` into `TurnRunning`.
- **Host Loss Retention**: A session in `VisibilityHostLost` retains `Visibility = VisibilityHostLost` and its active `RecoveryGeneration`. `ReleaseTurn` remains blocked until reconciliation succeeds.

---

## 6. Two-Phase Protected Artifact Store

Artifacts (proposals, ballots, synthesis docs, findings, patches) are stored in an immutable, content-addressed directory structure outside contributor checkouts:
`<state_dir>/artifacts/<digest_prefix>/<digest>` (prefix is the first 2 hex characters of SHA-256).

### 6.1 Two-Phase Publication Sequence
```
1. Write Payload to Temp File (<state_dir>/artifacts/tmp/tmp_<uuid>)
2. Fsync Temp File (ensure byte content hits storage stack via FlushFileBuffers / fsync)
3. Compute SHA-256 Digest & Byte Size
4. Check Existing Destination File (<state_dir>/artifacts/<prefix>/<digest>):
   - If destination exists: read and verify its SHA-256 digest.
     * If valid: unlink temp file (converged duplicate content; safe reuse).
     * If corrupt: publication fails immediately with ErrArtifactCorrupt without modifying or repairing existing file.
   - If destination does not exist: atomically link temp file to final digest path via os.Link.
     (No partial-write fallback is permitted; unlinked staging file is cleaned up on link failure).
5. Directory Synchronization:
   - On POSIX (Linux, macOS): fsync parent directory to flush directory entry metadata before DB commit.
   - On Windows: User-mode directory handles cannot be flushed via FlushFileBuffers (ERROR_ACCESS_DENIED).
     Durability relies on synchronous temp file data flush prior to linking, backed by the fail-closed
     read verification barrier (Section 6.3): any post-crash uncommitted directory entry manifests as a missing
     blob returning ErrArtifactNotFound, strictly preventing execution on corrupt or unverified artifacts.
6. Commit SQLite Transaction:
   - Insert row into artifact_revisions (artifact_id, revision, run_id, kind, digest, byte_size, created_at).
   - Insert row into journal_entries (op_id, command_type='publish_artifact', ...).
```

### 6.2 Recovery Matrix for Incomplete Publication

| Failure Point | Storage State on Reopen | Consequence & Recovery Policy |
| :--- | :--- | :--- |
| **Crash during step 1 or 2** (temp file incomplete) | Partial file in `tmp/`; no DB row. | No artifact revision visible. Temp files are never automatically deleted on startup based on age alone (to avoid deleting files of active concurrent writers); orphan candidates are logged/reported. |
| **Crash during step 4** (blob installed on disk, DB not committed) | Blob exists at `<digest>`; no DB row in `artifact_revisions`. | Unreferenced blob. It is NOT an approved revision and cannot be accessed by logical ID. (Harmless; safely reused if identical content published later). |
| **Crash during step 6** (DB rollback, file exists) | File on disk; no DB row. | Identical to above; unreferenced blob. |
| **File missing or altered after DB commit** | DB row exists with digest $D$; file at path is missing or hash $D' \neq D$. | **Visible Failure**: `Store.ReadArtifact(id, rev)` returns `ErrArtifactNotFound` or `ErrArtifactCorrupt`. Any ballot or approval referencing this revision is invalid. |
| **Destination already exists with corrupt content on step 4** | Corrupt file exists at `<digest>`. | Publication fails immediately with `ErrArtifactCorrupt`. The corrupt file is NOT overwritten or silently repaired. |

### 6.3 Pre-Verification on Content Access
`ReadArtifact` enforces an explicit maximum size limit (e.g. 10 MiB), reads the complete payload, verifies that both the byte count and SHA-256 digest match the recorded `artifact_revisions` entry, and only then returns the verified `[]byte` (or reader backed by verified bytes). Missing, oversized, or mismatched content returns an error without exposing unverified artifact content.

---

## 7. Storage Lifecycle, Security, and Redaction

### 7.1 State Directory Placement
The state directory (containing `state.db`, `state.db-wal`, `state.db-shm`, and `artifacts/`) defaults to an isolated directory outside agent-accessible Git checkouts (e.g. `~/.local/share/agent-council/runs/<run_id>` or an operator-specified `--state-dir`).  
While an operator may optionally configure a project-local directory, it is never assumed to provide agent isolation.

### 7.2 Filesystem Permissions and Platform Protection
- **POSIX Systems**:
  - State and artifact directories: `0700` (`rwx------`).
  - Database, sidecars (`-wal`, `-shm`), and artifact files: `0600` (`rw-------`).
  - Pre-existing directories are inspected; permissions broader than `0700` are rejected or restricted.
  - Path traversal and symlinked destinations are explicitly rejected.
- **Windows Systems**:
  - Restricts file access to the executing user ACL.
  - Accommodates non-atomic `os.Rename` behavior using retry/replace helpers.
- **Git Protection**:
  - Automatically installs `.gitignore` containing `*` in the state directory to prevent accidental staging of runtime data.

### 7.3 SQLite Durability and Connection Settings
The SQLite URI is safely constructed using URL query escaping (`url.PathEscape`) to handle special characters (`?`, `#`, spaces) and platform-specific path formats (including Windows paths).
Write connections configure `_txlock=immediate` in their connection parameters, while hydration read transactions execute in deferred snapshot mode (`_txlock=deferred`).

On every opened database connection (including replacement connections opened by `database/sql`), connection initialization hooks execute and read back:
```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
```
- **Read-Back Verification**: Every setting is read back via `PRAGMA ...;` queries to confirm activation. If `foreign_keys` is not `1`, `journal_mode` is not `wal`, or `synchronous` is not `2` (`FULL`), connection setup fails immediately.
- **Durability (`synchronous = FULL`)**: Required to ensure that committed dispatch reservations and transitions survive OS crashes and power interruptions (subject to the filesystem and hardware storage stack honoring synchronization commands).
- **Single Connection Discipline**: To eliminate writer lock contention within the process, `Store` configures `SetMaxOpenConns(1)`. WAL allows concurrent external readers, but permits only one writer across all processes.
- **Connection-Bound Transactions**: All transactional queries execute strictly on `*sql.Tx`. Mixing `db.Exec` with `tx` operations is strictly prohibited.
- **Migration Safety**: Rejects schema versions newer than binary supports (`ErrUnsupportedSchemaVersion`). Tests verify clean rollback of interrupted migrations and concurrent migration attempts.

### 7.4 Credentials and Redaction Contract
- **Allowlist Policy**: Council does not collect or persist provider credentials (e.g., `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`), authorization headers, or private token files. Structured configuration in `native_bindings` is allowlisted; unknown or sensitive keys are rejected before insertion rather than indiscriminately string-replaced.
- **Sensitive Data Scanner**: Prompts, turn results, and artifact bodies pass through a pattern-based redactor checking for common API key signatures (`sk-ant-...`, `sk-...`, `Bearer ...`) before being written to disk or database.
- **Pre-Write Sanitization Guarantee**: If sensitive input is detected in a prompt, it is rejected or sanitized before dispatch reservation. A sanitized prompt stored in the database is the exact content dispatched.
- **Sanitized Content Hashing**: The stored artifact SHA-256 digest binds the *sanitized bytes* that actually hit disk—never pre-sanitized input.

---

## 8. Verification Strategy and Acceptance Test Matrix

Testing adheres strictly to inline test-driven development (TDD).

> **Durability Evidence Boundary**: Persistence and crash-recovery acceptance tests use the pinned production `modernc.org/sqlite` driver and real temporary database files. Native harness behavior is simulated with provider-free fixtures. SQL mocks may supplement error-path tests but do not constitute durability evidence.

### 8.1 Acceptance Tests (`internal/storage`)

| Test Name | Target Invariant & Scenario | Verification Evidence |
| :--- | :--- | :--- |
| `TestStore_SeparateProcessReopen_Anchor` | **Process A** records 4 sessions, 1 completed turn, 1 pending prompt, 1 uncertain turn (`intent_recorded`), terminates via `os.Exit` or subprocess exit. **Process B** opens same directory. | Process B reconstructs all 4 sessions, exact pending prompts, completed results, and flags the uncertain turn without starting work or redispatching. |
| `TestStore_TransactionRollback_PreCommitCrash` | Injected fault before transaction commit during `ReleaseTurn`. | Zero partial updates in SQLite; session state and pending prompt remain unaltered. |
| `TestStore_ReleaseGuards_HostLossBlocksRelease` | Attempt `ReleaseTurn` on a parked session whose visibility is `VisibilityHostLost`. | Release rejected; session remains parked; pending prompt remains queued. |
| `TestStore_UnresolvedIntent_PreservesCancellingStatus` | Turn is `TurnCancelling` when process terminates before cancel acknowledgement. Storage reopened. | Reopened turn remains `TurnCancelling` (NOT converted to `TurnRunning`); execution reservation preserved. |
| `TestStore_StaleUpdate_OptimisticLocking` | Two concurrent operations attempt mutation using same `row_version`. | Second update fails with `ErrStaleUpdate`; no clobbering. |
| `TestStore_Idempotency_ReceiptAndConflict` | (1) Retry identical release after commit -> returns original receipt without duplicate intent. (2) Reuse `op_id` with different parameters -> returns `ErrIdempotencyConflict`. | Original receipt returned idempotently; conflicting duplicate rejected. |
| `TestStore_Reopen_ZeroSideEffects` | Open store with 4 sessions, queued prompts, host loss episodes. | Lease unchanged, recovery generations unchanged, no worker dispatched. |
| `TestArtifactStore_VerifyBeforeExposure` | Corrupt artifact file (1 byte flipped) requested via `ReadArtifact`. | Call fails with `ErrArtifactCorrupt`; zero unverified bytes exposed to caller. |
| `TestArtifactStore_CorruptDestination_NoSilentRepair` | Destination file exists but has corrupt hash. Attempt publication. | Fails with `ErrArtifactCorrupt`; corrupt file NOT overwritten or repaired. |
| `TestArtifactStore_ConcurrentIdenticalPublication` | Two workers publish identical content concurrently. | Converge safely on same digest-addressed file without corruption. |
| `TestArtifactStore_OrphanTempFileNotPruned` | Reopen store containing temporary file in `artifacts/tmp/`. | In-progress temp file is not deleted based on age alone. |
| `TestStore_ConnectionPRAGMAs_Verified` | Inspect active connection and replacement connections. | `foreign_keys = 1`, `journal_mode = wal`, `synchronous = 2` verified via read-back. |
| `TestStore_Migrations_RejectNewerSchema` | Open database with schema version `999`. | Fails immediately with `ErrUnsupportedSchemaVersion`. |
| `TestStore_Redaction_ArtifactsAndPrompts` | Prompts, results, and artifact bodies containing `sk-ant-...` / `Bearer ...`. | Secrets redacted prior to DB/disk write; SHA-256 matches sanitized bytes. |
| `TestStore_Platform_PermissionsAndWindowsACL` | POSIX permissions `0700`/`0600`; Windows ACL access validation. | Validates owner restriction; auto-generates `.gitignore` with `*`. |

---

## 9. Next Step
Upon review and approval of this amended specification by the operator, invoke the **`writing-plans`** skill to draft the implementation plan.
