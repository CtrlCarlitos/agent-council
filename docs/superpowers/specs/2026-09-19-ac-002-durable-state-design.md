# Design Specification: Durable Run Journals, Native Session Mappings, Artifacts, and Recovery State (AC-002)

Status: Draft  
Date: 2026-09-19  
Issue: [AC-002 (#2)](https://github.com/CtrlCarlitos/agent-council/issues/2)  
Target Branch: `feat/ac-002-durable-state`  

---

## 1. Problem and Architectural Context

Agent Council is a controller-led system where four independent native harness contributors execute under explicit, deterministic governance ([ARCHITECTURE.md](../../ARCHITECTURE.md)). In-memory domain invariants were established in [AC-001 (PR #26)](https://github.com/CtrlCarlitos/agent-council/pull/26) and native harness adapter contracts with observation streams were established in [AC-006 (PR #27)](https://github.com/CtrlCarlitos/agent-council/pull/27).

However, an in-memory domain kernel or native session ID alone cannot survive a process termination, machine reboot, or client disconnect. If the Council service terminates while:
1. Four contributor sessions have established native conversation bindings,
2. Several turns have completed while others are queued,
3. A prompt dispatch was issued whose acceptance by the external provider is uncertain, or
4. Proposals, ballots, and synthesized findings have been produced,

the state must not be lost, silently reset, or corrupted. Nor may a restarted process convert uncertain external execution into an unearned success or a reckless automatic duplicate dispatch.

### 1.1 The Anchor Acceptance Story
> **Process A** records a council run with four distinct contributor sessions, completed turn results, pending prompts, and one turn whose dispatch outcome is uncertain. **Process A terminates.**  
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
    store_test.go                  # Reopen, process termination, concurrency, and rollback tests
    artifact_test.go               # Artifact two-phase commit, tampering, and missing-file tests
    migration_test.go              # New database, checksum mismatch, and schema version tests
```

### 2.1 Dependency and Driver Pinning
- **Driver**: `modernc.org/sqlite` accessed strictly through Go standard library `database/sql`. No dual-driver or pluggable runtime driver abstraction is included.
- **SQLite Engine Version**: Must embed a patched SQLite engine addressing the WAL-reset corruption bug (fixed in SQLite 3.51.3+ on March 13, 2026).
  - Selected Driver Pin: `modernc.org/sqlite v1.59.0` (embedding patched SQLite 3.51.3+).
  - Required Matching Dependency: `modernc.org/libc v1.75.7`.
  - Go Baseline Requirement: `go 1.25.0` (explicitly recorded in `go.mod` to match the pinned driver requirements).
- **Dual CI Verification Path**:
  - **Distribution Compatibility**: `CGO_ENABLED=0` builds and tests verify pure-Go cross-platform compilation across Linux, macOS, and Windows.
  - **Concurrency Verification**: `CGO_ENABLED=1` race tests (`go test -race ./...`) verify absence of data races under Go's race detector.

---

## 3. Database Schema and Integrity Invariants

The schema is defined in `internal/storage/schema.sql`. SQLite foreign key enforcement is mandatory (`PRAGMA foreign_keys = ON;`).

```sql
-- Schema Migration Tracking
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL -- ISO 8601 UTC (RFC3339Nano)
);

-- Council Runs (Top-level governance scope)
CREATE TABLE IF NOT EXISTS runs (
    run_id TEXT PRIMARY KEY,
    brief_digest TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    profile_digest TEXT NOT NULL,
    controller_lease TEXT NOT NULL, -- Authoritative run-level controller lease
    created_at TEXT NOT NULL
);

-- Contributor Sessions
CREATE TABLE IF NOT EXISTS sessions (
    session_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    contributor TEXT NOT NULL CHECK (contributor IN ('opencode', 'claude', 'codex', 'agy')),
    state TEXT NOT NULL CHECK (state IN ('parked', 'running', 'archived')),
    lifecycle TEXT NOT NULL CHECK (lifecycle IN ('active', 'archived')),
    controller_status TEXT NOT NULL CHECK (controller_status IN ('connected', 'disconnected')),
    visibility TEXT NOT NULL CHECK (visibility IN ('reachable', 'host_lost')),
    active_key TEXT,
    recovery_context TEXT,
    recovery_gen INTEGER NOT NULL CHECK (recovery_gen >= 0),
    active_recovery_gen INTEGER NOT NULL CHECK (active_recovery_gen >= 0),
    row_version INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(run_id, contributor) -- Exactly one session per contributor per run
);

-- Native Harness Bindings (Mapping logical sessions to external harness conversations)
CREATE TABLE IF NOT EXISTS native_bindings (
    session_id TEXT PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
    native_session_id TEXT NOT NULL,
    harness TEXT NOT NULL,
    model TEXT NOT NULL,
    config_json TEXT NOT NULL,
    created_at TEXT NOT NULL
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
    created_at TEXT NOT NULL,
    completed_at TEXT,
    PRIMARY KEY (session_id, turn_key)
);

-- Dual-Phase Dispatch Intent Logging (Boundary between DB reservation and external harness)
CREATE TABLE IF NOT EXISTS dispatch_intents (
    session_id TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
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
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    op_id TEXT NOT NULL UNIQUE, -- Idempotency key
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    session_id TEXT REFERENCES sessions(session_id) ON DELETE RESTRICT,
    turn_key TEXT,
    event_kind TEXT NOT NULL,
    payload_version INTEGER NOT NULL DEFAULT 1,
    payload_json TEXT NOT NULL,
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

---

## 4. Transaction Boundaries and Atomic Operations

All database modifications follow a strict, transactional command pipeline using `BEGIN IMMEDIATE` on a connection-bound transaction. An in-memory `*council.Session` is never mutated prior to successful commit.

### 4.1 Stale Update Prevention & Optimistic Locking
Every update to `sessions` asserts the current `row_version`:
```sql
UPDATE sessions 
SET state = ?, active_key = ?, visibility = ?, recovery_gen = ?, 
    active_recovery_gen = ?, row_version = row_version + 1, updated_at = ?
WHERE session_id = ? AND row_version = ?;
```
If 0 rows are affected, the transaction rolls back and returns `ErrStaleUpdate`. This prevents an outdated in-memory view from clobbering a newer completion or reconciliation recorded by another process.

### 4.2 The Six Atomic Operations

| Operation | Atomic Transaction Scope | Journal Event Kind | Failure / Crash Semantics |
| :--- | :--- | :--- | :--- |
| **1. QueuePrompt** | Validate lease; insert into `pending_prompts` (idempotent submission preserved); insert into `journal_entries`. | `prompt_queued` | Pre-commit crash: prompt remains unqueued; retry succeeds. |
| **2. ReleaseTurn** | Validate lease & `State == Parked`; delete from `pending_prompts`; insert into `turns` (`status = 'running'`); insert into `dispatch_intents` (`phase = 'intent_recorded'`); update `sessions` (`active_key = turn_key, state = 'running'`); insert into `journal_entries`. | `turn_released` | Pre-commit crash: prompt remains in `pending_prompts`. Post-commit crash: prompt is running with `intent_recorded`. |
| **3. RecordDispatchObservation** | Update `dispatch_intents` phase to `receipt_acknowledged` or `acceptance_unknown`; insert into `journal_entries`. | `dispatch_observed` | Does NOT authorize another turn or retire execution. |
| **4. RecordTerminalOutcome** | Validate active turn; update `turns` (`status`, `result`, `completed_at`); update `dispatch_intents` (`phase = 'resolved'`); update `sessions` (`active_key = NULL, state = 'parked'`); insert into `journal_entries`. | `turn_completed` / `turn_failed` / `turn_cancelled` | If host was lost, parks contributor but preserves outstanding recovery generation. |
| **5. ReconcileSession** | Validate `RecoveryRef`; update `sessions.visibility = 'reachable'`; if terminal: retire turn and resolve intent; if nonterminal: preserve reservation; insert into `journal_entries`. | `session_reconciled` | Stale recovery generation rejected without state change. |
| **6. RecordDecision** | Validate lease; insert into `journal_entries` binding exact `(artifact_id, revision)` references or proposal manifest digest. | `controller_decision` | Decision binds exact immutable artifact revisions; older approvals cannot apply to new revisions. |

### 4.3 Idempotent Retries via `op_id`
Every command accepts a unique operation identifier `op_id`. `journal_entries.op_id` has a `UNIQUE` constraint. If a command is retried after a network timeout where the database commit previously succeeded, the transaction detects the existing `op_id`, returns the committed result, and avoids duplicate executions or duplicated journal entries.

---

## 5. Execution Uncertainty and Reopening Semantics

### 5.1 Consistent Read Hydration
When a Council process opens the storage directory, it executes a single read-only transaction:
1. Loads the `runs` record to establish the authoritative `controller_lease`.
2. Loads all four `sessions` records for the run.
3. Loads all `native_bindings`, `pending_prompts`, and `turns` associated with each session.
4. Reconstitutes each `*council.Session` domain struct in memory:
   - Sets `s.Pending` to match current `pending_prompts`.
   - Sets `s.Turns` to match all recorded `turns`.
   - If `s.Active != ""`, sets `s.ActiveTurn = s.Turns[s.Active]` (preserving exact pointer identity).
   - Verifies relational consistency: if `s.Active != ""` but no matching turn exists, or if `s.Active == ""` but an unresolved running turn exists, storage hydration fails with `ErrInconsistentStorage`.

### 5.2 Zero Side-Effect Guarantee on Reopen
Opening storage:
- Does NOT start or redispatch background workers.
- Does NOT reconnect the controller.
- Does NOT reset controller leases.
- Does NOT increment recovery generations or create host-loss episodes.

### 5.3 Lifecycle State Preservation Across Reopen
Reopen preserves exact turn statuses without flattening:
- A turn in `TurnCancelling` remains `TurnCancelling`.
- A session in `VisibilityHostLost` retains `Visibility = VisibilityHostLost` and its active `RecoveryGeneration`.
- A turn with `dispatch_intents.phase IN ('intent_recorded', 'acceptance_unknown')` remains reserved in `TurnRunning`. Its contributor slot remains occupied, and `ReleaseTurn` for subsequent prompts remains strictly blocked until authoritative reconciliation occurs.

---

## 6. Two-Phase Protected Artifact Store

Artifacts (proposals, ballots, synthesis docs, findings, patches) are stored in an immutable, content-addressed directory structure outside contributor checkouts:
`<state_dir>/artifacts/<digest_prefix>/<digest>` (where prefix is the first 2 hex characters of SHA-256).

### 6.1 Two-Phase Publication Sequence
```
1. Write Payload to Temp File (.council/artifacts/tmp/tmp_<uuid>)
2. Fsync Temp File (ensure bytes hit physical media)
3. Compute SHA-256 Digest & Byte Size
4. Check Existing Destination File:
   - If exists: verify existing bytes match SHA-256 digest. If corrupt, return ErrArtifactCorrupt.
   - If does not exist: atomically move/install temp file to final digest path.
5. Fsync Parent Directory (on supported POSIX systems)
6. Commit SQLite Transaction:
   - Insert row into artifact_revisions (artifact_id, revision, run_id, kind, digest, byte_size, created_at).
   - Insert row into journal_entries (op_id, kind='artifact_created').
```

### 6.2 Recovery Matrix for Incomplete Publication

| Failure Point | Storage State on Reopen | Consequence & Recovery Policy |
| :--- | :--- | :--- |
| **Crash during step 1 or 2** (temp file incomplete) | Partial file in `tmp/`; no DB row. | No artifact revision visible. Temp files older than threshold can be safely pruned on startup. |
| **Crash during step 4** (blob installed on disk, DB not committed) | Blob exists at `<digest>`; no DB row in `artifact_revisions`. | Unreferenced blob. It is NOT an approved revision and cannot be accessed by logical ID. (Harmless; reused if identical content published later). |
| **Crash during step 6** (DB rollback, file exists) | File on disk; no DB row. | Identical to above; unreferenced blob. |
| **File missing or altered after DB commit** | DB row exists with SHA-256 digest $D$; file at path is missing or hash $D' \neq D$. | **Visible Failure**: `Store.ReadArtifact(id, rev)` returns `ErrArtifactNotFound` or `ErrArtifactCorrupt`. Any ballot or approval referencing this revision is invalid. |
| **Destination already exists on step 4** | File exists at `<digest>`. | Verify content against digest before reuse. If corrupt, replace with verified temp file; if valid, unlink temp file and reuse existing blob. |

### 6.3 Verified Readers vs Raw Paths
`ArtifactStore` never returns a naked, unverified file path for consumption. It returns an `io.ReadCloser` that computes the digest while reading and verifies it against the recorded digest on `Close()`, or loads and returns pre-verified `[]byte`.

---

## 7. Storage Lifecycle, Security, and Redaction

### 7.1 State Directory Placement
The state directory (containing `state.db`, `state.db-wal`, `state.db-shm`, and `artifacts/`) must default to an isolated location outside agent-accessible Git checkouts (e.g. `~/.local/share/agent-council/runs/<run_id>` or a dedicated operator-provided `--state-dir`).  
While an operator may optionally configure `.council/` inside a repository root, it must never be assumed to provide agent isolation.

### 7.2 Filesystem Permissions
- **POSIX Systems**:
  - Directory: `0700` (`rwx------`).
  - Database and artifact files: `0600` (`rw-------`).
  - Pre-existing directories are validated; permissions broader than `0700` are rejected or restricted before use.
- **Windows Systems**:
  - Validates user ACLs to ensure restricted owner access.
  - Accommodates non-atomic `os.Rename` replacement semantics using atomic replace helpers.
- **Git Protection**:
  - Automatically installs `.gitignore` containing `*` in the state directory to ensure runtime data and database files cannot be accidentally staged into Git.

### 7.3 SQLite Durability Configuration
```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
```
- **Durability (`synchronous = FULL`)**: Required to guarantee that acknowledged dispatch reservations and turn transitions survive operating-system crashes and power loss.
- **Single Connection Discipline**: To eliminate writer lock contention and connection pool drift during early milestones, `Store` maintains a single read-write connection pool (`SetMaxOpenConns(1)`). All connection initialization hooks enforce `foreign_keys = ON` on connection creation.

### 7.4 Credentials and Redaction Contract
- **Allowlist Policy**: Council does not collect or persist provider credentials (e.g., `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`), authorization headers, or private token files. Only model names, logical IDs, native session IDs, and explicit configuration parameters are stored in `native_bindings`.
- **Sensitive Data Scanner**: Prompts, turn results, and artifact metadata pass through a pattern-based redactor checking for common API key signatures (`sk-ant-...`, `sk-...`, `Bearer ...`) before being written to `turns`, `journal_entries`, or temporary files.
- **Sanitized Content Hashing**: If any content is redacted prior to storage, the stored SHA-256 digest binds the *sanitized bytes* that actually hit disk—never the pre-sanitized input.

---

## 8. Verification Strategy and Acceptance Test Matrix

Testing must strictly adhere to inline test-driven development (TDD). Ordinary CI remains completely provider-free and uses mock/fake drivers.

### 8.1 Primary Acceptance Tests (`internal/storage`)

| Test Name | Verification Target | Invariant Enforced |
| :--- | :--- | :--- |
| `TestStore_SeparateProcessReopen_Anchor` | Process A writes 4 sessions, 1 completed turn, 1 pending prompt, 1 uncertain dispatch (`intent_recorded`), terminates. Process B reopens same dir. | Process B reconstructs all 4 sessions, exact pending prompts, completed results, and flags the uncertain turn without starting work. |
| `TestStore_TransactionRollback_CrashBeforeCommit` | Simulated failure during multi-table mutation (e.g. `ReleaseTurn`). | Zero partial updates in SQLite; session state and pending prompt remain unaltered. |
| `TestStore_UncertainDispatch_BlocksSubsequentRelease` | Turn released with `intent_recorded`; process terminates before ack. Storage reopened. | Reopened turn is `TurnRunning`; attempt to `Release` another prompt on same session is blocked. |
| `TestStore_PreservesCancellationAndRecoveryGenerations` | Session in `TurnCancelling` and `VisibilityHostLost` with generation 3 reopened. | Preserves `TurnCancelling`, `host_lost`, and generation 3; stale generation reply rejected. |
| `TestStore_StaleUpdateRejected_OptimisticLocking` | Two concurrent stores attempt update using same `row_version`. | Second update fails with `ErrStaleUpdate`; no clobbering. |
| `TestArtifactStore_TwoPhaseCrashRecovery` | Test crash points: (1) orphan temp file, (2) blob on disk without DB record, (3) DB record with missing/corrupted file. | (1) Pruned on startup; (2) ignored, not approved; (3) returns `ErrArtifactCorrupt` / `ErrArtifactNotFound`. |
| `TestArtifactStore_DigestTamperingDetected` | Single byte modified in artifact blob file after DB commit. | `ReadArtifact` detects mismatch with stored digest and fails visibly. |
| `TestStore_Security_PermissionsAndGitignore` | POSIX directory permissions `0700`, files `0600`, auto-generated `.gitignore`. | Verifies non-compliant permissions rejected; `.gitignore` contains `*`. |
| `TestStore_Redaction_CredentialPatterns` | Prompts and results containing `sk-ant-...` or Bearer tokens. | Credentials redacted before SQLite / artifact storage; digest matches sanitized bytes. |
| `TestStore_Migrations_VersionAndChecksum` | Migrations applied transactionally; checksum tampering detected. | Forward migrations pass; modified migration scripts fail startup. |

---

## 9. Next Step
Upon review and approval of this specification by the operator, invoke the **`writing-plans`** skill to draft the implementation plan.
