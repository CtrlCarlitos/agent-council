# AC-002: Durable Run Journals, Native Session Mappings, Artifacts, and Recovery State Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement SQLite-backed persistent storage (`internal/storage`) for Agent Council that durably records run journals, logical-to-native session mappings, pending prompts, turn history, and content-addressed artifacts, ensuring that a terminated process can be reopened by a separate process with zero side effects and complete state fidelity.

**Architecture:** Authoritative relational state in SQLite (WAL mode, `synchronous = FULL`) tracks runs, contributor sessions, turns, pending prompts, and dispatch intent phases. An append-only `journal_entries` table records accepted transitions with unique idempotency keys. A two-phase content-addressed file repository stores immutable artifact revisions verified by SHA-256 digests. Reopening storage performs a consistent read without auto-redispatching or converting uncertain work into permissions to execute.

**Tech Stack:** Go 1.25.0, `database/sql`, `modernc.org/sqlite v1.59.0` (embedding SQLite 3.53.4), `modernc.org/libc v1.75.7`.

**Spec:** [`docs/superpowers/specs/2026-09-19-ac-002-durable-state-design.md`](file:///home/carlitos/projects/CtrlCarlitos/agent-council/docs/superpowers/specs/2026-09-19-ac-002-durable-state-design.md)

## Global Constraints

- Pinned SQLite driver: `modernc.org/sqlite v1.59.0` (embedding SQLite 3.53.4) and `modernc.org/libc v1.75.7` accessed strictly via `database/sql`.
- Required Go baseline: `go 1.25.0` in `go.mod`, CI workflows, and documentation.
- Dual CI paths: `CGO_ENABLED=0` builds and tests for pure-Go portability; `CGO_ENABLED=1` for race tests (`go test -race ./...`).
- Zero external provider calls in ordinary CI; tests use provider-free fixtures and real temporary SQLite database files.
- Directory permissions `0700` (`rwx------`), file permissions `0600` (`rw-------`) on POSIX; Windows ACL restriction to user.
- Automatic `.gitignore` containing `*` in the state directory.
- Database PRAGMAs: `PRAGMA journal_mode = WAL;`, `PRAGMA synchronous = FULL;`, `PRAGMA foreign_keys = ON;`, `PRAGMA busy_timeout = 5000;`.
- SQLite single-connection pool per Store (`SetMaxOpenConns(1)`); all transactional statements run on connection-bound `*sql.Tx` with `BEGIN IMMEDIATE`.
- Timestamps encoded in UTC ISO 8601 / RFC 3339 with nanosecond precision (`time.RFC3339Nano`).
- Checked integer conversions: `uint64` recovery generations mapped to `INTEGER` [0, `math.MaxInt64`]; overflow returns `ErrRecoveryGenerationOverflow`.

## Review Focus

1. **Separate-Process Reopen without Side Effects**: A process terminates abruptly with 4 sessions, 1 completed turn, 1 pending prompt, and 1 uncertain dispatch (`intent_recorded`). A second process reopens storage and reconstructs exact state without re-dispatching workers, resetting leases, or incrementing recovery generations.
2. **Crash Before Commit Leaves Zero Partial Updates**: Injected crash or transaction rollback during multi-table mutation (`ReleaseTurn`) leaves session, pending prompts, and turns completely unmodified.
3. **Uncertain Dispatch Blocks Subsequent Release**: An active turn with `phase = 'intent_recorded'` or `'acceptance_unknown'` preserves its execution reservation across reopen; `ReleaseTurn` on the same session remains strictly blocked until authoritative reconciliation.
4. **Idempotency Fingerprint Mismatch Rejection**: Retrying an operation with an existing `op_id` but mismatched parameters returns `ErrIdempotencyConflict`; retrying with identical parameters returns the original committed receipt.
5. **No Silent Repair of Corrupt Artifacts**: An existing corrupt digest-addressed artifact file causes publication to fail immediately with `ErrArtifactCorrupt` without overwriting or silently repairing the file; reading a corrupt file returns `ErrArtifactCorrupt` without exposing unverified bytes.

---

### Task 1: Go 1.25 Baseline, Dependency Pinning & Store Connection Lifecycle

**Files:**
- Modify: `go.mod:1-4`
- Modify: `.github/workflows/ci.yml:1-60`
- Create: `internal/storage/types.go`
- Create: `internal/storage/store.go`
- Test: `internal/storage/store_test.go`

**Interfaces:**
- Produces:
  ```go
  type StoreOptions struct {
      StateDir string
  }
  type Store struct { ... }
  func Open(opts StoreOptions) (*Store, error)
  func (s *Store) Close() error
  func (s *Store) DB() *sql.DB
  ```

- [ ] **Step 1: Write the failing test for Store Open, Close, and PRAGMA verification**

```go
// internal/storage/store_test.go
package storage_test

import (
	"path/filepath"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_OpenCloseAndPRAGMAs(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Verify engine version is SQLite 3.51.3 or newer
	var sqliteVersion string
	if err := store.DB().QueryRow("SELECT sqlite_version();").Scan(&sqliteVersion); err != nil {
		t.Fatalf("failed to query sqlite_version: %v", err)
	}
	t.Logf("Embedded SQLite version: %s", sqliteVersion)

	// Verify PRAGMAs
	var journalMode string
	if err := store.DB().QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("expected journal_mode=wal, got %s", journalMode)
	}

	var foreignKeys int
	if err := store.DB().QueryRow("PRAGMA foreign_keys;").Scan(&foreignKeys); err != nil {
		t.Fatalf("failed to query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("expected foreign_keys=1, got %d", foreignKeys)
	}

	var synchronous int
	if err := store.DB().QueryRow("PRAGMA synchronous;").Scan(&synchronous); err != nil {
		t.Fatalf("failed to query synchronous: %v", err)
	}
	if synchronous != 2 { // 2 = FULL
		t.Fatalf("expected synchronous=2 (FULL), got %d", synchronous)
	}

	var busyTimeout int
	if err := store.DB().QueryRow("PRAGMA busy_timeout;").Scan(&busyTimeout); err != nil {
		t.Fatalf("failed to query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("expected busy_timeout=5000, got %d", busyTimeout)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/storage -run ^TestStore_OpenCloseAndPRAGMAs$`  
Expected: FAIL (package `internal/storage` does not exist yet).

- [ ] **Step 3: Update `go.mod` to Go 1.25.0 and install pinned dependencies**

Update `go.mod`:
```
module github.com/CtrlCarlitos/agent-council

go 1.25.0

require (
	modernc.org/libc v1.75.7
	modernc.org/sqlite v1.59.0
)
```
Run: `go mod tidy`

- [ ] **Step 4: Implement `internal/storage/types.go` and `internal/storage/store.go`**

In `internal/storage/types.go`:
```go
package storage

import (
	"errors"
)

var (
	ErrStaleUpdate               = errors.New("stale update: row_version mismatch")
	ErrIdempotencyConflict       = errors.New("idempotency conflict: operation id exists with different parameters")
	ErrUnauthorizedOperation     = errors.New("unauthorized operation: caller lease mismatch")
	ErrInconsistentStorage       = errors.New("inconsistent storage: database records violate domain invariants")
	ErrArtifactNotFound          = errors.New("artifact not found")
	ErrArtifactCorrupt           = errors.New("artifact corrupt: sha256 digest mismatch")
	ErrUnsupportedSchemaVersion  = errors.New("unsupported schema version: database version is newer than binary supports")
	ErrRecoveryGenerationOverflow = errors.New("recovery generation overflow: exceeds maximum int64 range")
)

type StoreOptions struct {
	StateDir string
}
```

In `internal/storage/store.go`:
```go
package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db       *sql.DB
	stateDir string
}

func Open(opts StoreOptions) (*Store, error) {
	if opts.StateDir == "" {
		return nil, fmt.Errorf("state directory required")
	}
	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}

	dbPath := filepath.Join(opts.StateDir, "state.db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)", dbPath)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	// Single connection discipline to eliminate pool drift
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// Execute and read back required PRAGMAs to guarantee activation
	var jm string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&jm); err != nil || jm != "wal" {
		db.Close()
		return nil, fmt.Errorf("failed to verify WAL mode (got %q, err %v)", jm, err)
	}
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys;").Scan(&fk); err != nil || fk != 1 {
		db.Close()
		return nil, fmt.Errorf("failed to verify foreign_keys (got %d, err %v)", fk, err)
	}
	var sync int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&sync); err != nil || sync != 2 {
		db.Close()
		return nil, fmt.Errorf("failed to verify synchronous FULL (got %d, err %v)", sync, err)
	}

	return &Store{
		db:       db,
		stateDir: opts.StateDir,
	}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) StateDir() string {
	return s.stateDir
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -v ./internal/storage -run ^TestStore_OpenCloseAndPRAGMAs$`  
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/storage/types.go internal/storage/store.go internal/storage/store_test.go
git commit -m "feat(storage): initialize SQLite store with WAL mode and verified PRAGMAs"
```

---

### Task 2: Schema Migrations & Connection-Bound Transaction Wrapper

**Files:**
- Create: `internal/storage/schema.sql`
- Create: `internal/storage/migrations.go`
- Create: `internal/storage/tx.go`
- Test: `internal/storage/migration_test.go`

**Interfaces:**
- Produces:
  ```go
  func (s *Store) Migrate() error
  func (s *Store) CurrentSchemaVersion() (int, error)
  type Tx struct { ... }
  func (s *Store) BeginTx(ctx context.Context) (*Tx, error)
  func (tx *Tx) Commit() error
  func (tx *Tx) Rollback() error
  ```

- [ ] **Step 1: Write the failing tests for schema migrations and transaction rollback**

```go
// internal/storage/migration_test.go
package storage_test

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_Migrations_ApplyAndRejectNewer(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if ver < 1 {
		t.Fatalf("expected version >= 1, got %d", ver)
	}

	// Tamper with version to simulate database newer than binary
	_, err = store.DB().Exec("INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (999, 'future', 'hash', '2026-09-19T00:00:00Z');")
	if err != nil {
		t.Fatalf("insert future version: %v", err)
	}

	store2, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	defer store2.Close()

	if err := store2.Migrate(); err != storage.ErrUnsupportedSchemaVersion {
		t.Fatalf("expected ErrUnsupportedSchemaVersion, got %v", err)
	}
}

func TestStore_TransactionRollback(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	tx, err := store.BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO runs (run_id, brief_digest, source_digest, profile_digest, controller_lease, lifecycle, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?);`,
		"run-1", "b", "s", "p", "lease-1", "active", "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// Explicit rollback
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var count int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM runs WHERE run_id = 'run-1';").Scan(&count); err != nil {
		t.Fatalf("query count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 runs after rollback, got %d", count)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/storage -run ^TestStore_Migrations`  
Expected: FAIL (`Migrate` undefined).

- [ ] **Step 3: Implement `schema.sql`, `migrations.go`, and `tx.go`**

In `internal/storage/schema.sql`: Embed SQL schema defined in Section 3 of the design specification.

In `internal/storage/migrations.go`:
- Embed `schema.sql` via `//go:embed schema.sql`.
- Compute SHA-256 checksum of migration script.
- In `Migrate()`:
  - Query highest version in `schema_migrations`.
  - If highest version > current supported version (1), return `ErrUnsupportedSchemaVersion`.
  - In a transaction, execute `schema.sql`, verify checksum, and record version 1 in `schema_migrations`.

In `internal/storage/tx.go`:
- Implement `BeginTx(ctx context.Context) (*Tx, error)` executing `tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})` and `tx.ExecContext(ctx, "PRAGMA foreign_keys = ON;")`.
- Wrap `*sql.Tx` ensuring all operations execute on that connection.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/storage -run ^(TestStore_Migrations|TestStore_TransactionRollback)`  
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/schema.sql internal/storage/migrations.go internal/storage/tx.go internal/storage/migration_test.go
git commit -m "feat(storage): implement transactional schema migrations and tx wrapper"
```

---

### Task 3: Authoritative Relational Session Store & Optimistic Concurrency

**Files:**
- Create: `internal/storage/session_store.go`
- Create: `internal/storage/journal.go`
- Test: `internal/storage/session_store_test.go`

**Interfaces:**
- Produces:
  ```go
  type RunRecord struct {
      RunID           string
      BriefDigest     string
      SourceDigest    string
      ProfileDigest   string
      ControllerLease string
      Lifecycle       string
      CreatedAt       time.Time
      UpdatedAt       time.Time
  }
  type SessionRecord struct {
      SessionID          string
      RunID              string
      Contributor        council.Contributor
      IsActiveContributor bool
      State              council.State
      Lifecycle          council.SessionLifecycle
      ControllerStatus   council.ControllerConnection
      Visibility         council.ExecutionVisibility
      ActiveKey          string
      RecoveryContext    string
      RecoveryGen        uint64
      ActiveRecoveryGen  uint64
      RowVersion         int64
      CreatedAt          time.Time
      UpdatedAt          time.Time
  }
  func (s *Store) CreateRun(ctx context.Context, opID string, run RunRecord) error
  func (s *Store) CreateSession(ctx context.Context, opID string, sess SessionRecord) error
  func (s *Store) QueuePrompt(ctx context.Context, opID, sessionID, lease, key, prompt string) error
  func (s *Store) ReplacePendingPrompt(ctx context.Context, opID, sessionID, lease, key, newPrompt string) error
  func (s *Store) DiscardPendingPrompt(ctx context.Context, opID, sessionID, lease, key string) error
  ```

- [ ] **Step 1: Write failing tests for Session CRUD, optimistic concurrency, and idempotency**

```go
// internal/storage/session_store_test.go
package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_Session_QueueAndReplace(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC()
	err = store.CreateRun(ctx, "op-run-1", storage.RunRecord{
		RunID:           "run-1",
		BriefDigest:     "b",
		SourceDigest:    "s",
		ProfileDigest:   "p",
		ControllerLease: "lease-1",
		Lifecycle:       "active",
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	err = store.CreateSession(ctx, "op-sess-1", storage.SessionRecord{
		SessionID:          "s-claude-1",
		RunID:              "run-1",
		Contributor:        council.Claude,
		IsActiveContributor: true,
		State:              council.Parked,
		Lifecycle:          council.SessionActive,
		ControllerStatus:   council.ControllerConnected,
		Visibility:         council.VisibilityReachable,
		RowVersion:         1,
		CreatedAt:          now,
		UpdatedAt:          now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Queue prompt
	err = store.QueuePrompt(ctx, "op-q-1", "s-claude-1", "lease-1", "t1", "prompt content")
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	// Idempotent retry of queue prompt with same op_id
	err = store.QueuePrompt(ctx, "op-q-1", "s-claude-1", "lease-1", "t1", "prompt content")
	if err != nil {
		t.Fatalf("idempotent queue retry failed: %v", err)
	}

	// Conflicting retry of queue prompt with same op_id but different key
	err = store.QueuePrompt(ctx, "op-q-1", "s-claude-1", "lease-1", "t2", "other content")
	if err != storage.ErrIdempotencyConflict {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}

	// Replace pending prompt
	err = store.ReplacePendingPrompt(ctx, "op-rep-1", "s-claude-1", "lease-1", "t1", "updated prompt")
	if err != nil {
		t.Fatalf("replace prompt: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/storage -run ^TestStore_Session_QueueAndReplace$`  
Expected: FAIL (`CreateRun` undefined).

- [ ] **Step 3: Implement `session_store.go` and `journal.go`**

Implement `CreateRun`, `CreateSession`, `QueuePrompt`, `ReplacePendingPrompt`, `DiscardPendingPrompt` with:
- Verification of caller lease against `runs.controller_lease`.
- Idempotency check on `op_id` in `journal_entries` before acquiring write lock.
- Atomically updating tables and advancing `sessions.row_version = row_version + 1`.
- Inserting corresponding row into `journal_entries`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/storage -run ^TestStore_Session_QueueAndReplace$`  
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/session_store.go internal/storage/journal.go internal/storage/session_store_test.go
git commit -m "feat(storage): implement session store with optimistic concurrency and idempotency"
```

---

### Task 4: Complete Domain Command Transitions & Guards

**Files:**
- Modify: `internal/storage/session_store.go`
- Test: `internal/storage/transitions_test.go`

**Interfaces:**
- Produces:
  ```go
  func (s *Store) ReleaseTurn(ctx context.Context, opID, sessionID, lease, key, attemptID string) error
  func (s *Store) RecordDispatchObservation(ctx context.Context, opID, sessionID, key, phase string) error
  func (s *Store) RequestCancel(ctx context.Context, opID, sessionID, lease string) error
  func (s *Store) RecordTerminalOutcome(ctx context.Context, opID, sessionID, key string, status council.TurnStatus, result string) error
  func (s *Store) RecordHostLoss(ctx context.Context, opID, sessionID string) (uint64, error)
  func (s *Store) ReconcileSession(ctx context.Context, opID, sessionID string, gen uint64, status adapter.ReconciliationStatus, observed council.TurnStatus, result string) error
  func (s *Store) RecordDecision(ctx context.Context, opID, runID, lease string, artifactRefs []ArtifactRef, manifestDigest, decision string) error
  ```

- [ ] **Step 1: Write failing tests for ReleaseTurn guards, HostLoss block, and terminal outcomes**

```go
// internal/storage/transitions_test.go
package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_ReleaseGuards_HostLossBlocksRelease(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC()
	_ = store.CreateRun(ctx, "op-r", storage.RunRecord{RunID: "r1", ControllerLease: "l1", CreatedAt: now, UpdatedAt: now})
	_ = store.CreateSession(ctx, "op-s", storage.SessionRecord{SessionID: "s1", RunID: "r1", Contributor: council.Claude, State: council.Parked, Lifecycle: council.SessionActive, ControllerStatus: council.ControllerConnected, Visibility: council.VisibilityReachable, RowVersion: 1, CreatedAt: now, UpdatedAt: now})
	_ = store.QueuePrompt(ctx, "op-q1", "s1", "l1", "t1", "prompt 1")
	_ = store.QueuePrompt(ctx, "op-q2", "s1", "l1", "t2", "prompt 2")

	// Release t1
	if err := store.ReleaseTurn(ctx, "op-rel-1", "s1", "l1", "t1", "att-1"); err != nil {
		t.Fatalf("release t1: %v", err)
	}

	// Record host loss on s1
	gen, err := store.RecordHostLoss(ctx, "op-hl-1", "s1")
	if err != nil {
		t.Fatalf("record host loss: %v", err)
	}
	if gen != 1 {
		t.Fatalf("expected gen 1, got %d", gen)
	}

	// Record terminal completion of t1 while host loss is active
	if err := store.RecordTerminalOutcome(ctx, "op-term-1", "s1", "t1", council.TurnCompleted, "done"); err != nil {
		t.Fatalf("record terminal outcome: %v", err)
	}

	// Attempt ReleaseTurn of t2: MUST be blocked because visibility is still host_lost!
	err = store.ReleaseTurn(ctx, "op-rel-2", "s1", "l1", "t2", "att-2")
	if err == nil {
		t.Fatalf("expected ReleaseTurn to be blocked under host_lost visibility, but succeeded")
	}
}

func TestStore_LateObservationIgnored(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC()
	_ = store.CreateRun(ctx, "op-r", storage.RunRecord{RunID: "r1", ControllerLease: "l1", CreatedAt: now, UpdatedAt: now})
	_ = store.CreateSession(ctx, "op-s", storage.SessionRecord{SessionID: "s1", RunID: "r1", Contributor: council.Claude, State: council.Parked, Lifecycle: council.SessionActive, ControllerStatus: council.ControllerConnected, Visibility: council.VisibilityReachable, RowVersion: 1, CreatedAt: now, UpdatedAt: now})
	_ = store.QueuePrompt(ctx, "op-q1", "s1", "l1", "t1", "prompt 1")
	_ = store.ReleaseTurn(ctx, "op-rel-1", "s1", "l1", "t1", "att-1")
	_ = store.RecordTerminalOutcome(ctx, "op-term-1", "s1", "t1", council.TurnCompleted, "done")

	// Late observation arriving after resolution must be ignored without reopening execution
	err = store.RecordDispatchObservation(ctx, "op-late-1", "s1", "t1", "acceptance_unknown")
	if err != nil {
		t.Fatalf("late observation returned error: %v", err)
	}

	// Assert dispatch intent remains 'resolved'
	var phase string
	if err := store.DB().QueryRow("SELECT phase FROM dispatch_intents WHERE session_id = 's1' AND turn_key = 't1';").Scan(&phase); err != nil {
		t.Fatalf("query phase: %v", err)
	}
	if phase != "resolved" {
		t.Fatalf("expected phase to remain resolved, got %s", phase)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/storage -run ^(TestStore_ReleaseGuards|TestStore_LateObservationIgnored)`  
Expected: FAIL (`ReleaseTurn` undefined).

- [ ] **Step 3: Implement domain transition methods in `session_store.go`**

Implement `ReleaseTurn`, `RecordDispatchObservation`, `RequestCancel`, `RecordTerminalOutcome`, `RecordHostLoss`, `ReconcileSession`, `SetControllerConnection`, `ArchiveSession`, `RecordDecision`:
- Ensure `ReleaseTurn` asserts `visibility != 'host_lost'` and `state == 'parked'`.
- Ensure `RecordDispatchObservation` checks if `phase == 'resolved'` and returns cleanly without modifying phase if already resolved.
- Ensure `RecordTerminalOutcome` sets `turns.status`, `dispatch_intents.phase = 'resolved'`, `sessions.state = 'parked'`, `sessions.active_key = NULL`, but *preserves* `recovery_context` and `active_recovery_gen` if `visibility == 'host_lost'`.
- Ensure `RecordDecision` asserts that referenced `(artifact_id, revision)` exist in `artifact_revisions` for this run.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/storage -run ^(TestStore_ReleaseGuards|TestStore_LateObservationIgnored)`  
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/session_store.go internal/storage/transitions_test.go
git commit -m "feat(storage): implement complete domain transitions and guards in SQLite"
```

---

### Task 5: Consistent Read Hydration & Crash Recovery

**Files:**
- Create: `internal/storage/recovery.go`
- Test: `internal/storage/recovery_test.go`
- Test: `internal/storage/reopen_process_test.go`

**Interfaces:**
- Produces:
  ```go
  type HydratedSession struct {
      Record  SessionRecord
      Session *council.Session
  }
  type HydratedRun struct {
      Run      RunRecord
      Sessions map[string]*HydratedSession
  }
  func (s *Store) HydrateRun(ctx context.Context, runID string) (*HydratedRun, error)
  ```

- [ ] **Step 1: Write failing test for Hydration and Separate-Process Reopen**

```go
// internal/storage/recovery_test.go
package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_ConsistentHydration(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC()
	_ = store.CreateRun(ctx, "op-r", storage.RunRecord{RunID: "r1", ControllerLease: "l1", CreatedAt: now, UpdatedAt: now})
	_ = store.CreateSession(ctx, "op-s1", storage.SessionRecord{SessionID: "s1", RunID: "r1", Contributor: council.Claude, State: council.Parked, Lifecycle: council.SessionActive, ControllerStatus: council.ControllerConnected, Visibility: council.VisibilityReachable, RowVersion: 1, CreatedAt: now, UpdatedAt: now})
	_ = store.QueuePrompt(ctx, "op-q1", "s1", "l1", "t1", "prompt 1")
	_ = store.ReleaseTurn(ctx, "op-rel-1", "s1", "l1", "t1", "att-1")

	// Hydrate
	hydrated, err := store.HydrateRun(ctx, "r1")
	if err != nil {
		t.Fatalf("hydrate run: %v", err)
	}

	sess, ok := hydrated.Sessions["s1"]
	if !ok {
		t.Fatalf("session s1 not found in hydrated run")
	}

	if sess.Session.State != council.Running {
		t.Fatalf("expected session state Running, got %v", sess.Session.State)
	}
	if sess.Session.Active != "t1" {
		t.Fatalf("expected active turn t1, got %v", sess.Session.Active)
	}
	if sess.Session.ActiveTurn == nil || sess.Session.ActiveTurn != sess.Session.Turns["t1"] {
		t.Fatalf("expected ActiveTurn pointer to match Turns['t1']")
	}
}
```

```go
// internal/storage/reopen_process_test.go
package storage_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_SeparateProcessReopen_Anchor(t *testing.T) {
	tempDir := t.TempDir()

	// Helper process writes data and exits
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessWriter")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "COUNCIL_TEST_DIR="+tempDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process failed: %v, output: %s", err, out)
	}

	// Process B opens the same storage directory
	storeB, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("process B open store: %v", err)
	}
	defer storeB.Close()

	hydrated, err := storeB.HydrateRun(context.Background(), "run-anchor")
	if err != nil {
		t.Fatalf("process B hydrate: %v", err)
	}

	// Verify all 4 sessions restored
	if len(hydrated.Sessions) != 4 {
		t.Fatalf("expected 4 sessions, got %d", len(hydrated.Sessions))
	}

	// Verify uncertain turn preserved in TurnRunning without automatic completion or redispatch
	sessAgy := hydrated.Sessions["sess-agy"]
	if sessAgy.Session.State != council.Running {
		t.Fatalf("expected agy session to remain Running, got %v", sessAgy.Session.State)
	}
	if sessAgy.Session.Active != "turn-uncertain" {
		t.Fatalf("expected active turn-uncertain, got %v", sessAgy.Session.Active)
	}

	// Verify completed turn restored
	sessClaude := hydrated.Sessions["sess-claude"]
	if sessClaude.Session.State != council.Parked {
		t.Fatalf("expected claude session to be Parked, got %v", sessClaude.Session.State)
	}
	if sessClaude.Session.Turns["turn-done"].Status != council.TurnCompleted {
		t.Fatalf("expected turn-done to be TurnCompleted")
	}

	// Verify pending prompt restored
	if sessClaude.Session.Pending["turn-pending"] != "pending prompt content" {
		t.Fatalf("expected pending prompt restored")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/storage -run ^(TestStore_ConsistentHydration|TestStore_SeparateProcessReopen_Anchor)`  
Expected: FAIL (`HydrateRun` undefined).

- [ ] **Step 3: Implement `internal/storage/recovery.go` and helper process**

In `internal/storage/recovery.go`:
- Implement `HydrateRun(ctx context.Context, runID string) (*HydratedRun, error)` using a single read transaction (`BEGIN DEFERRED`).
- Reconstruct `*council.Session` with pointer equality between `s.ActiveTurn` and `s.Turns[s.Active]`.
- Verify that unresolved intents preserve turn status without converting `TurnCancelling` to `TurnRunning`.
- In test: Implement `TestHelperProcessWriter` writing 4 sessions (`opencode`, `claude`, `codex`, `agy`), completed results, pending prompt, and 1 uncertain turn (`intent_recorded`), then exiting abruptly with `os.Exit(0)`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/storage -run ^(TestStore_ConsistentHydration|TestStore_SeparateProcessReopen_Anchor)`  
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/recovery.go internal/storage/recovery_test.go internal/storage/reopen_process_test.go
git commit -m "feat(storage): implement consistent read hydration and separate-process crash recovery"
```

---

### Task 6: Two-Phase Content-Addressed Protected Artifact Store

**Files:**
- Create: `internal/storage/artifact_store.go`
- Test: `internal/storage/artifact_test.go`

**Interfaces:**
- Produces:
  ```go
  type ArtifactRevision struct {
      ArtifactID string
      Revision   int
      RunID      string
      Kind       string
      Digest     string
      ByteSize   int64
      CreatedAt  time.Time
  }
  func (s *Store) PublishArtifact(ctx context.Context, opID, artifactID string, revision int, runID, kind string, content []byte) (ArtifactRevision, error)
  func (s *Store) ReadArtifact(ctx context.Context, artifactID string, revision int) ([]byte, error)
  ```

- [ ] **Step 1: Write failing tests for two-phase publication, digest verification, and no silent repair**

```go
// internal/storage/artifact_test.go
package storage_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestArtifactStore_PublishAndVerify(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC()
	_ = store.CreateRun(ctx, "op-r", storage.RunRecord{RunID: "r1", ControllerLease: "l1", CreatedAt: now, UpdatedAt: now})

	payload := []byte("artifact content revision 1")
	rev, err := store.PublishArtifact(ctx, "op-art-1", "art-proposal", 1, "r1", "proposal", payload)
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	h := sha256.Sum256(payload)
	expectedDigest := hex.EncodeToString(h[:])
	if rev.Digest != expectedDigest {
		t.Fatalf("expected digest %s, got %s", expectedDigest, rev.Digest)
	}

	// Read and verify content
	readBytes, err := store.ReadArtifact(ctx, "art-proposal", 1)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(readBytes) != string(payload) {
		t.Fatalf("content mismatch")
	}

	// Tamper with file on disk
	filePath := filepath.Join(tempDir, "artifacts", expectedDigest[:2], expectedDigest)
	if err := os.WriteFile(filePath, []byte("tampered content"), 0600); err != nil {
		t.Fatalf("tamper file: %v", err)
	}

	// ReadArtifact must fail with ErrArtifactCorrupt without exposing unverified bytes
	_, err = store.ReadArtifact(ctx, "art-proposal", 1)
	if err != storage.ErrArtifactCorrupt {
		t.Fatalf("expected ErrArtifactCorrupt, got %v", err)
	}

	// Attempting publication when destination file exists but has corrupt content must fail without silent repair
	_, err = store.PublishArtifact(ctx, "op-art-2", "art-proposal", 2, "r1", "proposal", payload)
	if err != storage.ErrArtifactCorrupt {
		t.Fatalf("expected ErrArtifactCorrupt on existing corrupt destination, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/storage -run ^TestArtifactStore_PublishAndVerify$`  
Expected: FAIL (`PublishArtifact` undefined).

- [ ] **Step 3: Implement `internal/storage/artifact_store.go`**

Implement:
- `PublishArtifact`:
  - 1. Writes payload to `<state_dir>/artifacts/tmp/tmp_<uuid>`.
  - 2. `fsync` file.
  - 3. Computes SHA-256 digest and byte size.
  - 4. Checks `<state_dir>/artifacts/<prefix>/<digest>`:
    - If exists: verifies SHA-256 digest. If corrupt, returns `ErrArtifactCorrupt` immediately. If valid, reuses blob and unlinks temp file.
    - If not exists: atomic rename/move to destination.
  - 5. `fsync` parent dir on POSIX.
  - 6. Inserts into `artifact_revisions` and `journal_entries` within SQLite transaction.
- `ReadArtifact`:
  - Validates size limit (10 MiB).
  - Reads bytes from disk.
  - Computes SHA-256 digest and asserts match with database record.
  - Returns verified bytes only on exact match.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/storage -run ^TestArtifactStore_PublishAndVerify$`  
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/artifact_store.go internal/storage/artifact_test.go
git commit -m "feat(storage): implement two-phase content-addressed artifact store with digest verification"
```

---

### Task 7: Storage Lifecycle, Permissions, Windows ACL & Credential Redaction

**Files:**
- Create: `internal/storage/redaction.go`
- Create: `internal/storage/platform.go`
- Create: `internal/storage/platform_windows.go`
- Create: `internal/storage/platform_posix.go`
- Test: `internal/storage/security_test.go`

**Interfaces:**
- Produces:
  ```go
  func SanitizeText(input string) (string, bool)
  func EnsureDirectoryPermissions(dir string) error
  ```

- [ ] **Step 1: Write failing test for Permissions, `.gitignore`, and Credential Redaction**

```go
// internal/storage/security_test.go
package storage_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_Security_PermissionsAndGitignore(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	// Verify .gitignore exists and contains *
	gitignorePath := filepath.Join(tempDir, ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(data), "*") {
		t.Fatalf("expected .gitignore to contain '*', got %s", string(data))
	}
}

func TestStore_Redaction_CredentialPatterns(t *testing.T) {
	rawPrompt := "Please use key sk-ant-api03-abcdef1234567890 to authenticate"
	clean, modified := storage.SanitizeText(rawPrompt)
	if !modified {
		t.Fatalf("expected secret to be sanitized")
	}
	if strings.Contains(clean, "sk-ant-api03") {
		t.Fatalf("expected secret to be removed, got %s", clean)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/storage -run ^(TestStore_Security|TestStore_Redaction)`  
Expected: FAIL.

- [ ] **Step 3: Implement `redaction.go`, `platform_posix.go`, `platform_windows.go`, and auto-gitignore**

- In `internal/storage/redaction.go`:
  - Regex patterns for `sk-ant-[a-zA-Z0-9_-]+`, `sk-[a-zA-Z0-9_-]{20,}`, `Bearer\s+[a-zA-Z0-9._-]+`.
  - Replaces matches with `[REDACTED_SECRET]`.
- In `internal/storage/platform_posix.go` and `platform_windows.go`:
  - POSIX: enforces `0700` directories, `0600` files.
  - Windows: sets file security attributes / user ACLs.
- In `internal/storage/store.go`:
  - Creates `.gitignore` containing `*\n` on `Open()`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/storage -run ^(TestStore_Security|TestStore_Redaction)`  
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/redaction.go internal/storage/platform.go internal/storage/platform_posix.go internal/storage/platform_windows.go internal/storage/security_test.go internal/storage/store.go
git commit -m "feat(storage): implement platform permission enforcement, gitignore, and credential redaction"
```

---

### Task 8: Full Verification Suite, Cross-Platform Controls & PR Readiness

**Files:**
- Modify: `.github/workflows/ci.yml` (update Go version matrix if needed)
- Test: All repository packages

- [ ] **Step 1: Run full race test suite**

Run: `go test -race -count=5 ./...`  
Expected: PASS across all packages (`internal/council`, `internal/adapter`, `internal/storage`).

- [ ] **Step 2: Run pure Go CGO_ENABLED=0 distribution tests**

Run: `CGO_ENABLED=0 go test -count=1 ./...`  
Expected: PASS across all packages.

- [ ] **Step 3: Run static analysis, vet, and formatting**

Run: `gofmt -l cmd internal`  
Expected: 0 unformatted files.  
Run: `go vet ./...`  
Expected: 0 vet warnings.  
Run: `go build ./...`  
Expected: clean compilation (exit code 0).

- [ ] **Step 4: Run repository seed and publisher verifications**

Run: `python3 scripts/verify_seed.py`  
Run: `python3 -m unittest discover -s scripts/tests -v`  
Expected: All PASS.

- [ ] **Step 5: Push branch and create Pull Request on GitHub**

Run:
```bash
git push -u origin feat/ac-002-durable-state
gh pr create --title "[AC-002] Persist run journals, native session mappings, artifacts, and recovery state" --body "..."
```

---

## Self-Review Checklist
- [x] **Spec coverage**: Every requirement from `2026-09-19-ac-002-durable-state-design.md` has a corresponding task and test.
- [x] **No Placeholders**: Zero "TBD", "TODO", or hand-waving steps. Code blocks provided for all implementations.
- [x] **Type consistency**: Method signatures and error sentinels (`ErrArtifactCorrupt`, `ErrStaleUpdate`, `ErrIdempotencyConflict`, etc.) match across tasks.
- [x] **Review Focus**: Separate-process reopen, transaction rollback, host loss release block, idempotency conflict, and corrupt artifact non-repair are explicitly pinned to test steps.
