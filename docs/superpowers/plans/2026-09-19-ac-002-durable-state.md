# AC-002: Durable Run Journals, Native Session Mappings, Artifacts, and Recovery State Implementation Plan

> **Execution Mode:** Single implementation owner using inline TDD (native execution mode, no subagent dispatch). Tasks include two internal verification gates: Gate 1 after Tasks 1–2 (foundation), and Gate 2 after Tasks 3–5 (durable commands and recovery).

**Goal:** Implement SQLite-backed persistent storage (`internal/storage`) for Agent Council that durably records run journals, logical-to-native session mappings, pending prompts, turn history, and content-addressed artifacts, ensuring that a terminated process can be reopened by a separate process with zero side effects and complete state fidelity.

**Architecture:** Authoritative relational state in SQLite (WAL mode, `synchronous = FULL`, `_txlock=immediate` on writes, `_txlock=deferred` on read hydration) tracks runs, contributor sessions, turns, pending prompts, and dispatch intent phases. An append-only `journal_entries` table records accepted transitions with unique idempotency keys. A two-phase content-addressed file repository stores immutable artifact revisions verified by SHA-256 digests. Reopening storage performs a consistent read without auto-redispatching or converting uncertain work into permissions to execute.

**Tech Stack:** Go 1.25.0, `database/sql`, `modernc.org/sqlite v1.59.0` (embedding SQLite 3.53.4), `modernc.org/libc v1.75.7`.

**Spec:** [docs/superpowers/specs/2026-09-19-ac-002-durable-state-design.md](docs/superpowers/specs/2026-09-19-ac-002-durable-state-design.md)

## Global Constraints

- Pinned SQLite driver: `modernc.org/sqlite v1.59.0` (embedding SQLite 3.53.4) and `modernc.org/libc v1.75.7` accessed strictly via `database/sql`.
- Required Go baseline: `go 1.25.0` in `go.mod`, CI workflows, and documentation.
- Dual CI paths: `CGO_ENABLED=0` builds and tests for pure-Go portability; `CGO_ENABLED=1` for race tests (`go test -race ./...`).
- Zero external provider calls in ordinary CI; tests use provider-free fixtures and real temporary SQLite database files.
- Directory permissions `0700` (`rwx------`), file permissions `0600` (`rw-------`) on POSIX; Windows ACL restriction to user.
- Automatic `.gitignore` containing `*` in the state directory.
- Database PRAGMAs: `PRAGMA journal_mode = WAL;`, `PRAGMA synchronous = FULL;`, `PRAGMA foreign_keys = ON;`, `PRAGMA busy_timeout = 5000;`.
- SQLite connection discipline: Write operations execute with `_txlock=immediate` and `SetMaxOpenConns(1)`; all transactional statements run on connection-bound `*sql.Tx`. Read hydration executes in snapshot mode (`_txlock=deferred`).
- Timestamps encoded in UTC ISO 8601 / RFC 3339 with nanosecond precision (`time.RFC3339Nano`).
- Checked integer conversions: `uint64` recovery generations mapped to `INTEGER` [0, `math.MaxInt64`]; overflow returns `ErrRecoveryGenerationOverflow`.
- Safe URI escaping: SQLite file paths constructed using `url.PathEscape` to handle special characters (`?`, `#`, spaces) and Windows path separators.
- Pre-write protection: Sensitive credentials matching known token patterns are redacted to `[REDACTED]` before SQLite persistence or artifact storage; path traversal in artifact names is rejected with `ErrInvalidPath`.

## Requirement-to-Test Mapping

| Requirement | Specification Ref | Implementation Task | Observable Test Assertion |
|---|---|---|---|
| Pinned SQLite 3.53.4 engine | Sec 1.1, 7.3 | Task 1 | `TestStore_OpenCloseAndPRAGMAs` asserts `sqlite_version() == "3.53.4"` |
| Durability PRAGMAs (WAL, FULL, FK, timeout) | Sec 7.3 | Task 1 | `TestStore_OpenCloseAndPRAGMAs` & `TestStore_ConnectionPRAGMAs_ReplacementConnection` assert `wal`, `synchronous=2`, `foreign_keys=1`, `busy_timeout=5000` on new and replacement connections |
| Safe URI path escaping | Sec 7.3 | Task 1 | `TestStore_SafeURIEscaping` verifies database paths containing spaces, `#`, `?`, and Windows separators open and operate reliably |
| Write-lock exclusivity (`_txlock=immediate`) | Sec 4.1, 7.3 | Task 2 | `TestStore_WriteLockExclusivity` verifies Store A holds immediate lock without writes; Store B write transaction is blocked/rejected |
| Schema migration lifecycle & validation | Sec 3.1, 4.3 | Task 2 | `TestStore_Migrations_Lifecycle` tests new DB, no-op migration, checksum mismatch (`ErrMigrationChecksumMismatch`), newer schema rejection (`ErrUnsupportedSchemaVersion`), and concurrent open |
| Optimistic concurrency (`row_version`) | Sec 4.1 | Task 3 | `TestStore_OptimisticConcurrency_StaleUpdateRejected` tests two stores with same snapshot version; one succeeds, other rejected with `ErrStaleUpdate` and unmodified state |
| Authoritative inside-tx idempotency | Sec 4.2 | Task 3 | `TestStore_Idempotency_InsideTx` asserts duplicate identical request returns committed receipt with 1 journal entry; conflicting reuse returns `ErrIdempotencyConflict`; retrying completed turn returns receipt without re-dispatch |
| Caller authority validation | Sec 4.2 | Task 3 | `TestStore_Authority_LeaseValidation` asserts unauthorized caller lease is rejected with `ErrUnauthorizedOperation` |
| Host loss blocks release | Sec 4.3 | Task 4 | `TestStore_ReleaseGuards_HostLossBlocksRelease` asserts session in `VisibilityHostLost` rejects `ReleaseTurn` with `ErrHostLost` and retains visibility |
| Distinct native bindings per session | Sec 2.1, 3.2 | Task 4 | `TestStore_NativeBindings_MultipleSessionsSameContributor` asserts two sessions for same contributor maintain independent bindings and workspace configurations |
| Authoritative reconciliation | Sec 4.3 | Task 4 | `TestStore_ReconcileSession_GenerationAndTurnValidation` asserts mismatched turn key or stale recovery generation returns error |
| Late observation ignored | Sec 4.3 | Task 4 | `TestStore_RecordDispatchObservation_LateArrivalIgnored` asserts observation arriving after intent resolved is a no-op |
| Complete read hydration | Sec 5.1 | Task 5 | `TestStore_HydrateState_ExactStateReconstruction` asserts separate reopen reconstructs runs, sessions, bindings, prompts, turns, exact intent phases, leases, generations |
| Multi-boundary crash recovery | Sec 5.2 | Task 5 | `TestStore_CrashRecovery_MultiBoundaryMatrix` tests 4 subprocess crash boundaries (pre-commit, post-intent, unacknowledged acceptance, uncommitted artifact) |
| Safe artifact read & exposure | Sec 6.2 | Task 6 | `TestArtifactStore_ReadArtifact_VerifyBeforeExposure` bounds read, asserts zero bytes exposed on digest or size corruption |
| No silent repair on publish | Sec 6.1 | Task 6 | `TestArtifactStore_PublishArtifact_NoSilentRepair` asserts corrupt destination returns `ErrArtifactCorrupt` without modifying destination or committing revision |
| Concurrent publication convergence | Sec 6.1 | Task 6 | `TestArtifactStore_PublishArtifact_ConcurrentIdentical` asserts concurrent writes of same artifact converge safely |
| Storage permissions & sidecars | Sec 7.1, 7.2 | Task 7 | `TestStore_PermissionsAndGitignore` asserts directory `0700`, files `0600`, `.gitignore` contains `*`, sidecars protected |
| Pre-write credential redaction | Sec 7.4 | Task 7 | `TestStore_CredentialRedaction_RealStorageBoundary` exercises real storage writes and verifies tokens never reach database, WAL, or artifact files |
| Pure-Go & race CI verification | Sec 8.1 | Task 8 | CI matrix runs `CGO_ENABLED=0 go test ./...` and `CGO_ENABLED=1 go test -race ./...` |

---

### Task 1: Go 1.25 Baseline, Dependency Pinning & Store Connection Lifecycle

**Files:**
- Modify: `go.mod`
- Modify: `.github/workflows/ci.yml`
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
  func (s *Store) ReadDB() *sql.DB
  func (s *Store) StateDir() string
  ```

- [ ] **Step 1: Write the failing tests for Store Open, Engine Version, PRAGMAs on initial and replacement connections, and safe URI escaping**

```go
// internal/storage/store_test.go
package storage_test

import (
	"os"
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

	// Assert exact pinned SQLite engine version 3.53.4
	var sqliteVersion string
	if err := store.DB().QueryRow("SELECT sqlite_version();").Scan(&sqliteVersion); err != nil {
		t.Fatalf("failed to query sqlite_version: %v", err)
	}
	if sqliteVersion != "3.53.4" {
		t.Fatalf("expected SQLite engine version 3.53.4, got %s", sqliteVersion)
	}

	// Verify PRAGMAs on initial connection
	verifyPRAGMAs(t, store.DB())
}

func TestStore_ConnectionPRAGMAs_ReplacementConnection(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Force connection closure and replacement
	store.DB().SetMaxIdleConns(0)
	if err := store.DB().Ping(); err != nil {
		t.Fatalf("failed to ping db after pool reset: %v", err)
	}

	// Verify PRAGMAs survive on replacement connection
	verifyPRAGMAs(t, store.DB())
}

func TestStore_SafeURIEscaping(t *testing.T) {
	// Test paths containing spaces, hash, and question mark
	tempDir := t.TempDir()
	trickyPath := filepath.Join(tempDir, "special dir #1 ? test")
	if err := os.MkdirAll(trickyPath, 0700); err != nil {
		t.Fatalf("failed to create tricky path: %v", err)
	}

	store, err := storage.Open(storage.StoreOptions{StateDir: trickyPath})
	if err != nil {
		t.Fatalf("failed to open store with special characters in path: %v", err)
	}
	defer store.Close()

	verifyPRAGMAs(t, store.DB())
}

func verifyPRAGMAs(t *testing.T, db storage.QueryRower) {
	t.Helper()
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("expected journal_mode=wal, got %s", journalMode)
	}

	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys;").Scan(&foreignKeys); err != nil {
		t.Fatalf("failed to query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("expected foreign_keys=1, got %d", foreignKeys)
	}

	var synchronous int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&synchronous); err != nil {
		t.Fatalf("failed to query synchronous: %v", err)
	}
	if synchronous != 2 { // 2 = FULL
		t.Fatalf("expected synchronous=2 (FULL), got %d", synchronous)
	}

	var busyTimeout int
	if err := db.QueryRow("PRAGMA busy_timeout;").Scan(&busyTimeout); err != nil {
		t.Fatalf("failed to query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("expected busy_timeout=5000, got %d", busyTimeout)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/storage -run '^TestStore_OpenCloseAndPRAGMAs$'`  
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
Update `.github/workflows/ci.yml`:
Set `go-version: '1.25.0'`.
Run: `go mod tidy`

- [ ] **Step 4: Implement `internal/storage/types.go` and `internal/storage/store.go`**

In `internal/storage/types.go`:
```go
package storage

import (
	"database/sql"
	"errors"
	"time"
)

var (
	ErrStaleUpdate                = errors.New("stale update: row_version mismatch")
	ErrIdempotencyConflict        = errors.New("idempotency conflict: operation id exists with different parameters")
	ErrUnauthorizedOperation      = errors.New("unauthorized operation: caller lease mismatch")
	ErrInconsistentStorage        = errors.New("inconsistent storage: database records violate domain invariants")
	ErrArtifactNotFound           = errors.New("artifact not found")
	ErrArtifactCorrupt            = errors.New("artifact corrupt: sha256 digest mismatch")
	ErrMigrationChecksumMismatch  = errors.New("migration checksum mismatch: schema file has been modified")
	ErrUnsupportedSchemaVersion   = errors.New("unsupported schema version: database version is newer than binary supports")
	ErrRecoveryGenerationOverflow = errors.New("recovery generation overflow: exceeds maximum int64 range")
	ErrHostLost                   = errors.New("host lost: session visibility is host lost")
	ErrInvalidPath                = errors.New("invalid path: path traversal detected")
)

type QueryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

type StoreOptions struct {
	StateDir string
}

type OperationReceipt struct {
	OpID             string    `json:"op_id"`
	CommandType      string    `json:"command_type"`
	SessionID        string    `json:"session_id"`
	TurnKey          string    `json:"turn_key,omitempty"`
	CommittedVersion int64     `json:"committed_version"`
	CreatedAt        time.Time `json:"created_at"`
	Payload          string    `json:"payload,omitempty"`
}

type ReleaseReceipt struct {
	OperationReceipt
	SanitizedPrompt string `json:"sanitized_prompt"`
	TurnKey         string `json:"turn_key"`
	AttemptID       string `json:"attempt_id"`
}
```

In `internal/storage/store.go`:
```go
package storage

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	writeDB  *sql.DB
	readDB   *sql.DB
	stateDir string
}

func Open(opts StoreOptions) (*Store, error) {
	if opts.StateDir == "" {
		return nil, fmt.Errorf("state directory required")
	}
	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}

	// Install .gitignore with *
	gitignorePath := filepath.Join(opts.StateDir, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		_ = os.WriteFile(gitignorePath, []byte("*\n"), 0600)
	}

	dbPath := filepath.Clean(filepath.Join(opts.StateDir, "state.db"))
	escapedPath := url.PathEscape(filepath.ToSlash(dbPath))

	// Write DSN configures _txlock=immediate and required PRAGMAs
	writeDSN := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)", escapedPath)
	writeDB, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("open sqlite write db: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)

	// Read DSN configures _txlock=deferred
	readDSN := fmt.Sprintf("file:%s?_txlock=deferred&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)", escapedPath)
	readDB, err := sql.Open("sqlite", readDSN)
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("open sqlite read db: %w", err)
	}

	s := &Store{
		writeDB:  writeDB,
		readDB:   readDB,
		stateDir: opts.StateDir,
	}

	// Verify PRAGMAs on write connection
	if err := s.verifyConnectionPRAGMAs(writeDB); err != nil {
		s.Close()
		return nil, fmt.Errorf("verify write db pragmas: %w", err)
	}

	// Run migration lifecycle
	if err := s.Migrate(); err != nil {
		s.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	return s, nil
}

func (s *Store) verifyConnectionPRAGMAs(db *sql.DB) error {
	var jm string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&jm); err != nil || jm != "wal" {
		return fmt.Errorf("failed to verify WAL mode (got %q, err %v)", jm, err)
	}
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys;").Scan(&fk); err != nil || fk != 1 {
		return fmt.Errorf("failed to verify foreign_keys (got %d, err %v)", fk, err)
	}
	var sync int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&sync); err != nil || sync != 2 {
		return fmt.Errorf("failed to verify synchronous FULL (got %d, err %v)", sync, err)
	}
	return nil
}

func (s *Store) Close() error {
	var err1, err2 error
	if s.writeDB != nil {
		err1 = s.writeDB.Close()
	}
	if s.readDB != nil {
		err2 = s.readDB.Close()
	}
	if err1 != nil {
		return err1
	}
	return err2
}

func (s *Store) DB() *sql.DB {
	return s.writeDB
}

func (s *Store) ReadDB() *sql.DB {
	return s.readDB
}

func (s *Store) StateDir() string {
	return s.stateDir
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -v ./internal/storage -run '^TestStore_'`  
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum .github/workflows/ci.yml internal/storage/types.go internal/storage/store.go internal/storage/store_test.go
git commit -m "feat(storage): configure Go 1.25 baseline, SQLite 3.53.4, safe URI escaping, and verified PRAGMAs"
```

---

### Task 2: Schema Migrations & Connection-Bound Transaction Wrapper

**Files:**
- Create: `internal/storage/schema.sql`
- Create: `internal/storage/migrations.go`
- Create: `internal/storage/tx.go`
- Test: `internal/storage/migration_test.go`
- Test: `internal/storage/tx_test.go`

**Interfaces:**
- Produces:
  ```go
  func (s *Store) Migrate() error
  func (s *Store) CurrentSchemaVersion() (int, error)
  type WriteTx struct { ... }
  func (s *Store) BeginWrite(ctx context.Context) (*WriteTx, error)
  func (tx *WriteTx) Commit() error
  func (tx *WriteTx) Rollback() error
  func (tx *WriteTx) Tx() *sql.Tx
  ```

- [ ] **Step 1: Write the failing tests for schema migrations lifecycle and two-store write-lock exclusivity**

```go
// internal/storage/migration_test.go
package storage_test

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_Migrations_Lifecycle(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Brand-new DB: Open applied schema v1
	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if ver != 1 {
		t.Fatalf("expected schema version 1, got %d", ver)
	}

	// Repeated Migrate call is a clean no-op
	if err := store.Migrate(); err != nil {
		t.Fatalf("repeated migrate failed: %v", err)
	}

	// Corrupt checksum in schema_migrations: next open must reject with ErrMigrationChecksumMismatch
	_, err = store.DB().Exec("UPDATE schema_migrations SET checksum = 'corrupted_hash' WHERE version = 1;")
	if err != nil {
		t.Fatalf("corrupt checksum: %v", err)
	}
	store.Close()

	_, err = storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err == nil {
		t.Fatalf("expected ErrMigrationChecksumMismatch on corrupt checksum, got nil")
	}

	// Newer unsupported version: insert version 99; next open must reject with ErrUnsupportedSchemaVersion
	store2, err := storage.Open(storage.StoreOptions{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	defer store2.Close()
	_, err = store2.DB().Exec("INSERT INTO schema_migrations (version, applied_at, checksum) VALUES (99, '2026-09-19T00:00:00Z', 'dummy');")
	if err != nil {
		t.Fatalf("insert v99: %v", err)
	}
	store2Dir := store2.StateDir()
	store2.Close()

	_, err = storage.Open(storage.StoreOptions{StateDir: store2Dir})
	if err == nil {
		t.Fatalf("expected ErrUnsupportedSchemaVersion on newer version, got nil")
	}
}
```

```go
// internal/storage/tx_test.go
package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_WriteLockExclusivity(t *testing.T) {
	tempDir := t.TempDir()
	storeA, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open storeA: %v", err)
	}
	defer storeA.Close()

	storeB, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open storeB: %v", err)
	}
	defer storeB.Close()

	ctx := context.Background()

	// Store A begins write transaction without issuing any write statements
	txA, err := storeA.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("storeA BeginWrite: %v", err)
	}

	// Store B attempts to begin write transaction with short timeout; must be blocked and fail with busy/timeout
	ctxB, cancelB := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelB()

	txB, err := storeB.BeginWrite(ctxB)
	if err == nil {
		txB.Rollback()
		txA.Rollback()
		t.Fatalf("expected storeB BeginWrite to be blocked/rejected while storeA holds write lock, but it succeeded")
	}

	// Store A rolls back
	if err := txA.Rollback(); err != nil {
		t.Fatalf("storeA Rollback: %v", err)
	}

	// Now Store B can acquire write transaction
	txB2, err := storeB.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("storeB BeginWrite after storeA release: %v", err)
	}
	if err := txB2.Rollback(); err != nil {
		t.Fatalf("storeB Rollback: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v ./internal/storage -run '^(TestStore_Migrations_Lifecycle|TestStore_WriteLockExclusivity)$'`  
Expected: FAIL.

- [ ] **Step 3: Define schema in `internal/storage/schema.sql`**

Include:
- `runs` table with `controller_lease`, `active_coordinator`
- `sessions` table with `is_active_contributor` partial unique index, `row_version`, `visibility`, `recovery_generation`
- `pending_prompts` table with `UNIQUE(session_id)`
- `turns` table
- `dispatch_intents` table
- `native_bindings` table
- `journal_entries` table with `UNIQUE(op_id)`
- `artifacts` table
- `schema_migrations` table

- [ ] **Step 4: Implement `internal/storage/migrations.go` and `internal/storage/tx.go`**

In `internal/storage/migrations.go`:
Embed `schema.sql` via `//go:embed schema.sql`. Compute SHA-256 checksum of the schema text.
Implement `Migrate()`:
1. Ensure `schema_migrations` table exists.
2. Query max `version`. If max version > 1, return `ErrUnsupportedSchemaVersion`.
3. If version == 1, compare stored checksum with embedded schema checksum. If mismatch, return `ErrMigrationChecksumMismatch`. If match, return `nil` (no-op).
4. If version == 0 (fresh DB), begin write transaction, execute `schema.sql`, record version 1 with checksum and timestamp, and commit.

In `internal/storage/tx.go`:
```go
package storage

import (
	"context"
	"database/sql"
	"fmt"
)

type WriteTx struct {
	tx *sql.Tx
}

func (s *Store) BeginWrite(ctx context.Context) (*WriteTx, error) {
	// Write connection was opened with _txlock=immediate in DSN
	tx, err := s.writeDB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin immediate write tx: %w", err)
	}
	return &WriteTx{tx: tx}, nil
}

func (tx *WriteTx) Commit() error {
	return tx.tx.Commit()
}

func (tx *WriteTx) Rollback() error {
	return tx.tx.Rollback()
}

func (tx *WriteTx) Tx() *sql.Tx {
	return tx.tx
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -v ./internal/storage -run '^(TestStore_Migrations_Lifecycle|TestStore_WriteLockExclusivity)$'`  
Expected: PASS.

- [ ] **Step 6: Internal Verification Gate 1**

Run: `go test -race -v ./internal/storage/...`  
Expected: All tests pass.

- [ ] **Step 7: Commit**

```bash
git add internal/storage/schema.sql internal/storage/migrations.go internal/storage/tx.go internal/storage/migration_test.go internal/storage/tx_test.go
git commit -m "feat(storage): implement schema migrations lifecycle and immediate write transactions"
```

---

### Task 3: Authoritative Relational Session Store, Optimistic Concurrency & Inside-Transaction Idempotency

**Files:**
- Create: `internal/storage/session_store.go`
- Test: `internal/storage/session_store_test.go`
- Test: `internal/storage/idempotency_test.go`

**Interfaces:**
- Produces:
  ```go
  type SessionRecord struct {
      ID                 string
      RunID              string
      Contributor        string
      Role               string
      IsActiveContributor bool
      State              string
      Visibility         string
      RecoveryGeneration uint64
      ActiveTurnKey      *string
  }
  type PendingPrompt struct {
      SessionID string
      Prompt    string
      CreatedAt time.Time
  }
  func (s *Store) CreateRun(ctx context.Context, opID string, runID string, lease string) (OperationReceipt, error)
  func (s *Store) CreateSession(ctx context.Context, opID string, callerLease string, session SessionRecord) (OperationReceipt, error)
  func (s *Store) QueuePrompt(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, prompt PendingPrompt) (OperationReceipt, error)
  func (s *Store) ReplacePendingPrompt(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, prompt PendingPrompt) (OperationReceipt, error)
  func (s *Store) DiscardPendingPrompt(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64) (OperationReceipt, error)
  ```

- [ ] **Step 1: Write the failing tests for optimistic concurrency, lease authority, and authoritative inside-transaction idempotency**

```go
// internal/storage/session_store_test.go
package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_OptimisticConcurrency_StaleUpdateRejected(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-1")
	sessReceipt, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	initialVersion := sessReceipt.CommittedVersion

	// Store A queues a prompt with expectedVersion = initialVersion -> succeeds, bumps to initialVersion + 1
	r1, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", initialVersion, storage.PendingPrompt{
		SessionID: "sess-1", Prompt: "first prompt", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue prompt 1: %v", err)
	}
	if r1.CommittedVersion != initialVersion+1 {
		t.Fatalf("expected version %d, got %d", initialVersion+1, r1.CommittedVersion)
	}

	// Store B attempts to replace prompt with STALE expectedVersion = initialVersion -> rejected with ErrStaleUpdate
	_, err = store.ReplacePendingPrompt(ctx, "op-rep-stale", "lease-1", "sess-1", initialVersion, storage.PendingPrompt{
		SessionID: "sess-1", Prompt: "stale prompt", CreatedAt: time.Now(),
	})
	if err != storage.ErrStaleUpdate {
		t.Fatalf("expected ErrStaleUpdate on stale replacement, got %v", err)
	}
}

func TestStore_Authority_LeaseValidation(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-valid")

	// Caller with invalid lease must be rejected with ErrUnauthorizedOperation
	_, err = store.CreateSession(ctx, "op-sess-1", "lease-wrong", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != storage.ErrUnauthorizedOperation {
		t.Fatalf("expected ErrUnauthorizedOperation on wrong lease, got %v", err)
	}
}
```

```go
// internal/storage/idempotency_test.go
package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_Idempotency_InsideTx(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	// Initial QueuePrompt
	r1, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", Prompt: "Review this diff", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("first queue prompt: %v", err)
	}

	// Simultaneous / duplicate QueuePrompt with identical op_id and parameters -> returns original receipt
	r2, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", Prompt: "Review this diff", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("duplicate queue prompt: %v", err)
	}
	if r1.OpID != r2.OpID || r1.CommittedVersion != r2.CommittedVersion {
		t.Fatalf("expected identical receipt on retry: r1=%+v, r2=%+v", r1, r2)
	}

	// Conflicting reuse of same op_id with different prompt text -> ErrIdempotencyConflict
	_, err = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", Prompt: "DIFFERENT PROMPT TEXT", CreatedAt: time.Now(),
	})
	if err != storage.ErrIdempotencyConflict {
		t.Fatalf("expected ErrIdempotencyConflict on mismatched parameters, got %v", err)
	}

	// Unauthorized caller retrying existing op_id -> ErrUnauthorizedOperation
	_, err = store.QueuePrompt(ctx, "op-q-1", "lease-attacker", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", Prompt: "Review this diff", CreatedAt: time.Now(),
	})
	if err != storage.ErrUnauthorizedOperation {
		t.Fatalf("expected ErrUnauthorizedOperation on unauthorized receipt lookup, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v ./internal/storage -run '^(TestStore_OptimisticConcurrency_|TestStore_Authority_|TestStore_Idempotency_)'`  
Expected: FAIL.

- [ ] **Step 3: Implement authoritative inside-transaction idempotency and relational mutation methods in `internal/storage/session_store.go`**

Implement:
1. `checkOrRecordIdempotency(tx *sql.Tx, opID string, callerLease string, cmdType string, sessionID string, fingerprint string) (*OperationReceipt, error)`
   - Queries `journal_entries` by `op_id`.
   - If found:
     - Verify `callerLease` matches stored receipt authority. If mismatch, return `ErrUnauthorizedOperation`.
     - Verify `command_type == cmdType` and `session_id == sessionID` and `command_fingerprint == fingerprint`. If mismatch, return `ErrIdempotencyConflict`.
     - Decode and return stored `OperationReceipt`.
   - If not found: return `nil, nil`.
2. Wire pre-write prompt sanitization:
   - Apply token redaction (`sanitizeText(prompt.Prompt)`) before saving prompt into `pending_prompts` and constructing receipt.
3. Implement `CreateRun`, `CreateSession`, `QueuePrompt`, `ReplacePendingPrompt`, `DiscardPendingPrompt` following the inside-tx sequence.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/storage -run '^(TestStore_OptimisticConcurrency_|TestStore_Authority_|TestStore_Idempotency_)'`  
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/session_store.go internal/storage/session_store_test.go internal/storage/idempotency_test.go
git commit -m "feat(storage): implement relational session store with authoritative inside-transaction idempotency"
```

---

### Task 4: Complete Domain Command Transitions & Guards

**Files:**
- Create: `internal/storage/transitions.go`
- Test: `internal/storage/transitions_test.go`

**Interfaces:**
- Produces:
  ```go
  type NativeBinding struct {
      LogicalSessionID string `json:"logical_session_id"`
      NativeSessionID  string `json:"native_session_id"`
      WorkspaceMode    string `json:"workspace_mode"`
      ToolingConfig    string `json:"tooling_config"`
  }
  func (s *Store) SetNativeBinding(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, binding NativeBinding) (OperationReceipt, error)
  func (s *Store) ReleaseTurn(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, turnKey string) (ReleaseReceipt, error)
  func (s *Store) RecordDispatchObservation(ctx context.Context, opID string, callerLease string, sessionID string, turnKey string, phase string) (OperationReceipt, error)
  func (s *Store) ReconcileSession(ctx context.Context, opID string, callerLease string, ref adapter.RecoveryRef, outcome adapter.ReconciliationOutcome) (OperationReceipt, error)
  ```

- [ ] **Step 1: Write the failing tests for domain command transitions, guards, host loss protection, and native bindings**

```go
// internal/storage/transitions_test.go
package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_ReleaseGuards_HostLossBlocksRelease(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "host_lost",
	})
	_, _ = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", Prompt: "Review diff", CreatedAt: time.Now(),
	})

	// ReleaseTurn on session in VisibilityHostLost must be rejected with ErrHostLost
	_, err = store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", 2, "turn-1")
	if err != storage.ErrHostLost {
		t.Fatalf("expected ErrHostLost when releasing turn on host_lost session, got %v", err)
	}
}

func TestStore_NativeBindings_MultipleSessionsSameContributor(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-1")

	// Session 1: active contributor for claude
	_, err = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session 1: %v", err)
	}

	// Bind Session 1 to native workspace 1
	_, err = store.SetNativeBinding(ctx, "op-bind-1", "lease-1", "sess-1", 1, storage.NativeBinding{
		LogicalSessionID: "sess-1", NativeSessionID: "native-agent-101", WorkspaceMode: "branch", ToolingConfig: "tools-full",
	})
	if err != nil {
		t.Fatalf("bind session 1: %v", err)
	}

	// Session 2: historical contributor for claude in same run (IsActiveContributor = false)
	_, err = store.CreateSession(ctx, "op-sess-2", "lease-1", storage.SessionRecord{
		ID: "sess-2", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: false, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session 2: %v", err)
	}

	// Bind Session 2 to native workspace 2
	_, err = store.SetNativeBinding(ctx, "op-bind-2", "lease-1", "sess-2", 1, storage.NativeBinding{
		LogicalSessionID: "sess-2", NativeSessionID: "native-agent-102", WorkspaceMode: "share", ToolingConfig: "tools-read",
	})
	if err != nil {
		t.Fatalf("bind session 2: %v", err)
	}
}

func TestStore_ReconcileSession_GenerationAndTurnValidation(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable", RecoveryGeneration: 1,
	})
	_, _ = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", Prompt: "Review diff", CreatedAt: time.Now(),
	})
	relReceipt, err := store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", 2, "turn-1")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}
	if relReceipt.SanitizedPrompt != "Review diff" {
		t.Fatalf("expected sanitized prompt in release receipt, got %s", relReceipt.SanitizedPrompt)
	}

	// Reconcile with mismatched turn key must be rejected
	_, err = store.ReconcileSession(ctx, "op-rec-mismatch", "lease-1", adapter.RecoveryRef{
		SessionID: "sess-1", TurnKey: "turn-WRONG", Generation: 1,
	}, adapter.ReconciliationOutcome{Status: adapter.TurnCompleted})
	if err == nil {
		t.Fatalf("expected error reconciling mismatched turn key, got nil")
	}

	// Reconcile with stale generation must be rejected
	_, err = store.ReconcileSession(ctx, "op-rec-stale", "lease-1", adapter.RecoveryRef{
		SessionID: "sess-1", TurnKey: "turn-1", Generation: 0,
	}, adapter.ReconciliationOutcome{Status: adapter.TurnCompleted})
	if err == nil {
		t.Fatalf("expected error reconciling stale recovery generation, got nil")
	}
}

func TestStore_RecordDispatchObservation_LateArrivalIgnored(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable", RecoveryGeneration: 1,
	})
	_, _ = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", Prompt: "Review diff", CreatedAt: time.Now(),
	})
	_, _ = store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", 2, "turn-1")

	// Reconcile turn as terminal -> intent resolved
	_, err = store.ReconcileSession(ctx, "op-rec-1", "lease-1", adapter.RecoveryRef{
		SessionID: "sess-1", TurnKey: "turn-1", Generation: 1,
	}, adapter.ReconciliationOutcome{Status: adapter.TurnCompleted})
	if err != nil {
		t.Fatalf("reconcile turn: %v", err)
	}

	// Late dispatch observation arriving after resolution is ignored as a clean no-op
	r, err := store.RecordDispatchObservation(ctx, "op-obs-late", "lease-1", "sess-1", "turn-1", "receipt_acknowledged")
	if err != nil {
		t.Fatalf("expected late observation to be ignored without error, got %v", err)
	}
	if r.Payload != "late_observation_ignored" {
		t.Fatalf("expected payload late_observation_ignored, got %s", r.Payload)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v ./internal/storage -run '^(TestStore_ReleaseGuards_|TestStore_NativeBindings_|TestStore_ReconcileSession_|TestStore_RecordDispatchObservation_)'`  
Expected: FAIL.

- [ ] **Step 3: Implement domain transitions in `internal/storage/transitions.go`**

Implement:
- `ReleaseTurn`: validates caller lease, checks `Visibility != VisibilityHostLost`, retrieves and deletes from `pending_prompts`, inserts into `turns` (`status = 'running'`), inserts into `dispatch_intents` (`phase = 'intent_recorded'`), updates session `state = 'running'`, `active_key = turnKey`, bumps `row_version`. Returns `ReleaseReceipt` containing the exact `SanitizedPrompt` so callers dispatch the sanitized text.
- `SetNativeBinding`: records native session ID, workspace mode, tooling config in `native_bindings`.
- `ReconcileSession`: validates `ref.TurnKey == session.ActiveTurnKey` and generation matches. If outcome is terminal, updates turn status, updates intent to `resolved`, clears active turn key on session, bumps `row_version`.
- `RecordDispatchObservation`: updates `dispatch_intents.phase`. If intent already `resolved`, returns cleanly with `"late_observation_ignored"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/storage -run '^(TestStore_ReleaseGuards_|TestStore_NativeBindings_|TestStore_ReconcileSession_|TestStore_RecordDispatchObservation_)'`  
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/transitions.go internal/storage/transitions_test.go
git commit -m "feat(storage): implement complete domain command transitions, guards, and native bindings"
```

---

### Task 5: Consistent Read Hydration & Crash Recovery

**Files:**
- Create: `internal/storage/recovery.go`
- Test: `internal/storage/recovery_test.go`
- Test: `internal/storage/crash_recovery_test.go`

**Interfaces:**
- Produces:
  ```go
  type HydratedSession struct {
      SessionRecord
      PendingPrompt *PendingPrompt
      ActiveTurn    *TurnRecord
      ActiveIntent  *DispatchIntentRecord
      NativeBinding *NativeBinding
      RowVersion    int64
  }
  type HydratedState struct {
      Runs     map[string]RunRecord
      Sessions map[string]HydratedSession
      Journals []OperationReceipt
  }
  func (s *Store) HydrateState(ctx context.Context) (*HydratedState, error)
  ```

- [ ] **Step 1: Write the failing tests for complete read hydration and multi-boundary crash recovery**

```go
// internal/storage/recovery_test.go
package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_HydrateState_ExactStateReconstruction(t *testing.T) {
	tempDir := t.TempDir()
	storeA, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open storeA: %v", err)
	}

	ctx := context.Background()
	_, _ = storeA.CreateRun(ctx, "op-run-1", "run-1", "lease-1")

	// 4 sessions
	for i := 1; i <= 4; i++ {
		sessID := fmt.Sprintf("sess-%d", i)
		_, err := storeA.CreateSession(ctx, fmt.Sprintf("op-sess-%d", i), "lease-1", storage.SessionRecord{
			ID: sessID, RunID: "run-1", Contributor: fmt.Sprintf("agent-%d", i), Role: "worker", IsActiveContributor: true, State: "parked", Visibility: "reachable",
		})
		if err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
	}

	// Session 1: has completed turn
	_, _ = storeA.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{SessionID: "sess-1", Prompt: "P1", CreatedAt: time.Now()})
	_, _ = storeA.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", 2, "turn-1")
	_, _ = storeA.ReconcileSession(ctx, "op-rec-1", "lease-1", adapter.RecoveryRef{SessionID: "sess-1", TurnKey: "turn-1", Generation: 0}, adapter.ReconciliationOutcome{Status: adapter.TurnCompleted})

	// Session 2: has pending prompt
	_, _ = storeA.QueuePrompt(ctx, "op-q-2", "lease-1", "sess-2", 1, storage.PendingPrompt{SessionID: "sess-2", Prompt: "Pending P2", CreatedAt: time.Now()})

	// Session 3: has uncertain turn (intent_recorded)
	_, _ = storeA.QueuePrompt(ctx, "op-q-3", "lease-1", "sess-3", 1, storage.PendingPrompt{SessionID: "sess-3", Prompt: "Uncertain P3", CreatedAt: time.Now()})
	_, _ = storeA.ReleaseTurn(ctx, "op-rel-3", "lease-1", "sess-3", 2, "turn-3")

	// Session 4: idle parked
	// Close Store A abruptly
	storeA.Close()

	// Store B opens same directory and hydrations state
	storeB, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open storeB: %v", err)
	}
	defer storeB.Close()

	hydrated, err := storeB.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate state: %v", err)
	}

	if len(hydrated.Sessions) != 4 {
		t.Fatalf("expected 4 sessions, got %d", len(hydrated.Sessions))
	}
	if hydrated.Sessions["sess-2"].PendingPrompt == nil || hydrated.Sessions["sess-2"].PendingPrompt.Prompt != "Pending P2" {
		t.Fatalf("session 2 missing pending prompt")
	}
	if hydrated.Sessions["sess-3"].ActiveIntent == nil || hydrated.Sessions["sess-3"].ActiveIntent.Phase != "intent_recorded" {
		t.Fatalf("session 3 missing intent_recorded phase")
	}
	if hydrated.Sessions["sess-1"].ActiveTurn != nil {
		t.Fatalf("session 1 active turn must be nil after completed reconciliation")
	}
}
```

```go
// internal/storage/crash_recovery_test.go
package storage_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_CrashRecovery_MultiBoundaryMatrix(t *testing.T) {
	if os.Getenv("GO_TEST_SUBPROCESS") == "1" {
		runCrashSubprocess()
		return
	}

	boundaries := []string{
		"pre_commit_release",
		"post_intent_unacknowledged",
		"accepted_unrecorded_ack",
		"uncommitted_artifact_metadata",
	}

	for _, boundary := range boundaries {
		t.Run(boundary, func(t *testing.T) {
			tempDir := t.TempDir()

			// Initialize valid run and session records
			initStore, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
			if err != nil {
				t.Fatalf("init store: %v", err)
			}
			ctx := context.Background()
			_, _ = initStore.CreateRun(ctx, "op-run-init", "run-1", "lease-1")
			_, _ = initStore.CreateSession(ctx, "op-sess-init", "lease-1", storage.SessionRecord{
				ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
			})
			_, _ = initStore.QueuePrompt(ctx, "op-q-init", "lease-1", "sess-1", 1, storage.PendingPrompt{
				SessionID: "sess-1", Prompt: "Initial Prompt", CreatedAt: time.Now(),
			})
			initStore.Close()

			// Run helper subprocess targeted to crash at boundary
			subCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			cmd := exec.CommandContext(subCtx, os.Args[0], "-test.run=^TestStore_CrashRecovery_MultiBoundaryMatrix$")
			cmd.Env = append(os.Environ(),
				"GO_TEST_SUBPROCESS=1",
				"SUBPROCESS_CRASH_BOUNDARY="+boundary,
				"SUBPROCESS_STATE_DIR="+tempDir,
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			_ = cmd.Run() // Expect crash/exit

			// Parent reopens store and asserts durable state invariant
			store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
			if err != nil {
				t.Fatalf("parent reopen: %v", err)
			}
			defer store.Close()

			hydrated, err := store.HydrateState(context.Background())
			if err != nil {
				t.Fatalf("hydrate after crash %s: %v", boundary, err)
			}

			sess := hydrated.Sessions["sess-1"]
			switch boundary {
			case "pre_commit_release":
				// Zero partial release: session still parked, prompt still queued, no turn or intent
				if sess.State != "parked" || sess.PendingPrompt == nil || sess.ActiveTurn != nil {
					t.Fatalf("pre-commit crash left partial release: state=%s, prompt=%v, turn=%v", sess.State, sess.PendingPrompt, sess.ActiveTurn)
				}
			case "post_intent_unacknowledged":
				// Turn reserved and uncertain: phase == intent_recorded
				if sess.State != "running" || sess.ActiveIntent == nil || sess.ActiveIntent.Phase != "intent_recorded" {
					t.Fatalf("post-intent crash did not preserve uncertain reservation: %+v", sess)
				}
			case "accepted_unrecorded_ack":
				// Uncertain reservation preserved; ReleaseTurn on same session remains blocked
				_, err := store.ReleaseTurn(context.Background(), "op-illegal-rel", "lease-1", "sess-1", sess.RowVersion, "turn-2")
				if err == nil {
					t.Fatalf("expected ReleaseTurn to remain blocked on uncertain turn, but it succeeded")
				}
			case "uncommitted_artifact_metadata":
				// Zero logical artifacts hydrated in database
				var count int
				_ = store.DB().QueryRow("SELECT count(*) FROM artifacts;").Scan(&count)
				if count != 0 {
					t.Fatalf("expected 0 logical artifacts committed, got %d", count)
				}
			}
		})
	}
}

func runCrashSubprocess() {
	boundary := os.Getenv("SUBPROCESS_CRASH_BOUNDARY")
	stateDir := os.Getenv("SUBPROCESS_STATE_DIR")
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		os.Exit(1)
	}

	ctx := context.Background()
	switch boundary {
	case "pre_commit_release":
		// Begin write tx, modify tables, crash before commit
		tx, _ := store.BeginWrite(ctx)
		_, _ = tx.Tx().Exec("UPDATE sessions SET state = 'running' WHERE id = 'sess-1';")
		// Abrupt exit before tx.Commit()
		os.Exit(1)
	case "post_intent_unacknowledged":
		_, _ = store.ReleaseTurn(ctx, "op-crash-rel", "lease-1", "sess-1", 2, "turn-crash-1")
		os.Exit(1)
	case "accepted_unrecorded_ack":
		_, _ = store.ReleaseTurn(ctx, "op-crash-rel2", "lease-1", "sess-1", 2, "turn-crash-2")
		// External harness accepted, but before RecordDispatchObservation committed, crash
		os.Exit(1)
	case "uncommitted_artifact_metadata":
		// Install blob into artifacts/ dir, but crash before database transaction
		blobDir := filepath.Join(stateDir, "artifacts", "ab")
		_ = os.MkdirAll(blobDir, 0700)
		_ = os.WriteFile(filepath.Join(blobDir, "abcdef123456"), []byte("data"), 0600)
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v ./internal/storage -run '^(TestStore_HydrateState_|TestStore_CrashRecovery_)'`  
Expected: FAIL.

- [ ] **Step 3: Implement `internal/storage/recovery.go`**

Implement `HydrateState(ctx context.Context)`:
1. Begins read snapshot on `readDB` (`_txlock=deferred`).
2. Loads all runs into `map[string]RunRecord`.
3. Loads all sessions, their native bindings, pending prompts, active turns, and dispatch intents.
4. Loads journal entries into slice.
5. Assembles `HydratedState` without any side effects or status mutations.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/storage -run '^(TestStore_HydrateState_|TestStore_CrashRecovery_)'`  
Expected: PASS.

- [ ] **Step 5: Internal Verification Gate 2**

Run: `go test -race -v ./internal/storage/...`  
Expected: All tests in Tasks 1–5 pass under race detector.

- [ ] **Step 6: Commit**

```bash
git add internal/storage/recovery.go internal/storage/recovery_test.go internal/storage/crash_recovery_test.go
git commit -m "feat(storage): implement consistent read hydration and multi-boundary crash recovery"
```

---

### Task 6: Two-Phase Content-Addressed Protected Artifact Store

**Files:**
- Create: `internal/storage/artifact_store.go`
- Test: `internal/storage/artifact_store_test.go`

**Interfaces:**
- Produces:
  ```go
  type ArtifactMetadata struct {
      ID          string    `json:"id"`
      RunID       string    `json:"run_id"`
      SessionID   string    `json:"session_id"`
      TurnKey     string    `json:"turn_key"`
      Name        string    `json:"name"`
      Digest      string    `json:"digest"`
      ByteCount   int64     `json:"byte_count"`
      Revision    int64     `json:"revision"`
      CreatedAt   time.Time `json:"created_at"`
  }
  func (s *Store) PublishArtifact(ctx context.Context, opID string, callerLease string, meta ArtifactMetadata, content []byte) (ArtifactMetadata, error)
  func (s *Store) ReadArtifact(digest string) ([]byte, error)
  ```

- [ ] **Step 1: Write the failing tests for safe artifact read (verify before exposure), no silent repair, and concurrent publication convergence**

```go
// internal/storage/artifact_store_test.go
package storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestArtifactStore_ReadArtifact_VerifyBeforeExposure(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	content := []byte("legitimate artifact content")
	digest := fmt.Sprintf("%x", sha256.Sum256(content))

	meta, err := store.PublishArtifact(ctx, "op-pub-1", "lease-1", storage.ArtifactMetadata{
		RunID: "run-1", SessionID: "sess-1", TurnKey: "turn-1", Name: "report.md",
	}, content)
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	// Reading valid artifact returns content
	readBytes, err := store.ReadArtifact(meta.Digest)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !bytes.Equal(readBytes, content) {
		t.Fatalf("content mismatch")
	}

	// Corrupt file on disk: tamper with bytes
	blobPath := filepath.Join(tempDir, "artifacts", meta.Digest[:2], meta.Digest)
	if err := os.WriteFile(blobPath, []byte("tampered data"), 0600); err != nil {
		t.Fatalf("tamper file: %v", err)
	}

	// ReadArtifact must return ErrArtifactCorrupt and ZERO bytes exposed
	tamperedBytes, err := store.ReadArtifact(meta.Digest)
	if err != storage.ErrArtifactCorrupt {
		t.Fatalf("expected ErrArtifactCorrupt on tampered file, got %v", err)
	}
	if len(tamperedBytes) != 0 {
		t.Fatalf("expected zero bytes exposed on corruption, got %d bytes", len(tamperedBytes))
	}
}

func TestArtifactStore_PublishArtifact_NoSilentRepair(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	content := []byte("clean artifact data")
	digest := fmt.Sprintf("%x", sha256.Sum256(content))

	// Pre-seed corrupt destination file
	destDir := filepath.Join(tempDir, "artifacts", digest[:2])
	_ = os.MkdirAll(destDir, 0700)
	destPath := filepath.Join(destDir, digest)
	corruptBytes := []byte("pre-existing corrupt bytes")
	_ = os.WriteFile(destPath, corruptBytes, 0600)

	// PublishArtifact must detect corrupt destination and return ErrArtifactCorrupt immediately WITHOUT repairing or overwriting
	_, err = store.PublishArtifact(ctx, "op-pub-corrupt", "lease-1", storage.ArtifactMetadata{
		RunID: "run-1", SessionID: "sess-1", TurnKey: "turn-1", Name: "diff.patch",
	}, content)
	if err != storage.ErrArtifactCorrupt {
		t.Fatalf("expected ErrArtifactCorrupt when destination exists and is corrupt, got %v", err)
	}

	// Verify destination was NOT overwritten
	actualBytes, _ := os.ReadFile(destPath)
	if !bytes.Equal(actualBytes, corruptBytes) {
		t.Fatalf("corrupt destination was modified or repaired; must remain untouched")
	}
}

func TestArtifactStore_PublishArtifact_ConcurrentIdentical(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	content := []byte("concurrent artifact content")
	var wg sync.WaitGroup
	errCh := make(chan error, 5)

	for i := 1; i <= 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := store.PublishArtifact(ctx, fmt.Sprintf("op-pub-conc-%d", idx), "lease-1", storage.ArtifactMetadata{
				RunID: "run-1", SessionID: "sess-1", TurnKey: "turn-1", Name: fmt.Sprintf("artifact-%d.txt", idx),
			}, content)
			if err != nil {
				errCh <- err
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent publish failed: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v ./internal/storage -run '^TestArtifactStore_'`  
Expected: FAIL.

- [ ] **Step 3: Implement `internal/storage/artifact_store.go`**

Implement:
1. `PublishArtifact`:
   - Validates `meta.Name`: reject path separators (`/`, `\`, `..`) with `ErrInvalidPath`.
   - Computes SHA-256 digest and byte count.
   - Checks destination `artifacts/<digest[:2]>/<digest>`.
   - If destination exists:
     - Read and verify existing file bytes and digest using `io.LimitReader`.
     - If hash mismatch: return `ArtifactMetadata{}, ErrArtifactCorrupt`! Do NOT overwrite or repair.
     - If match: reuse existing file.
   - If destination does not exist:
     - Write to staging temp file in `artifacts/tmp/`.
     - Atomic rename or link into destination. If destination created concurrently, verify the winner before reuse.
   - Inside write transaction:
     - Check/record idempotency.
     - Insert into `artifacts` table.
     - Append journal entry and commit.
2. `ReadArtifact(digest string) ([]byte, error)`:
   - Validates digest format (`len(digest) == 64`, all hex lowercase).
   - Bounds read with `io.LimitReader` (max 100MB).
   - Computes SHA-256 digest while reading into buffer.
   - If byte count or digest does not match: returns `nil, ErrArtifactCorrupt` (zero bytes exposed).
   - Returns verified bytes.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/storage -run '^TestArtifactStore_'`  
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/artifact_store.go internal/storage/artifact_store_test.go
git commit -m "feat(storage): implement two-phase content-addressed verified artifact store"
```

---

### Task 7: Storage Lifecycle, Permissions, Windows ACL & Credential Redaction

**Files:**
- Create: `internal/storage/platform.go`
- Create: `internal/storage/redaction.go`
- Test: `internal/storage/platform_test.go`
- Test: `internal/storage/redaction_test.go`

**Interfaces:**
- Produces:
  ```go
  func SanitizeText(input string) string
  func EnsureDirectoryPermissions(dir string) error
  func EnsureFilePermissions(file string) error
  ```

- [ ] **Step 1: Write the failing tests for storage permissions, Windows ACL, and pre-write credential redaction through real storage commands**

```go
// internal/storage/platform_test.go
package storage_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_PermissionsAndGitignore(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Verify .gitignore exists and contains *
	gi, err := os.ReadFile(filepath.Join(tempDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if string(gi) != "*\n" {
		t.Fatalf("expected .gitignore to contain '*\\n', got %q", string(gi))
	}

	if runtime.GOOS != "windows" {
		// Verify POSIX directory mode 0700
		info, err := os.Stat(tempDir)
		if err != nil {
			t.Fatalf("stat tempDir: %v", err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("expected directory perm 0700, got %o", info.Mode().Perm())
		}

		// Verify POSIX db file mode 0600
		dbInfo, err := os.Stat(filepath.Join(tempDir, "state.db"))
		if err != nil {
			t.Fatalf("stat state.db: %v", err)
		}
		if dbInfo.Mode().Perm() != 0600 {
			t.Fatalf("expected db file perm 0600, got %o", dbInfo.Mode().Perm())
		}
	}
}
```

```go
// internal/storage/redaction_test.go
package storage_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_CredentialRedaction_RealStorageBoundary(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	secretKey := "sk-12345678901234567890123456789012"
	githubToken := "ghp_123456789012345678901234567890123456"
	slackToken := "xoxb-1234567890-abcdefg"

	rawPrompt := "Use secret " + secretKey + " and token " + githubToken + " and " + slackToken

	// QueuePrompt with raw secrets
	r, err := store.QueuePrompt(ctx, "op-q-sec", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", Prompt: rawPrompt, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	// Receipt payload must have redacted tokens
	if strings.Contains(r.Payload, secretKey) || strings.Contains(r.Payload, githubToken) {
		t.Fatalf("receipt payload contains raw secret tokens")
	}

	// ReleaseTurn receipt must also have redacted tokens
	relReceipt, err := store.ReleaseTurn(ctx, "op-rel-sec", "lease-1", "sess-1", 2, "turn-sec")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}
	if strings.Contains(relReceipt.SanitizedPrompt, secretKey) {
		t.Fatalf("release receipt contains raw secret tokens")
	}

	// Close store to flush WAL
	store.Close()

	// Read state.db and state.db-wal directly from disk; assert raw secrets never appear anywhere on disk
	for _, fname := range []string{"state.db", "state.db-wal"} {
		fpath := filepath.Join(tempDir, fname)
		data, err := os.ReadFile(fpath)
		if err == nil {
			if strings.Contains(string(data), secretKey) {
				t.Fatalf("file %s on disk contains leaked secret token!", fname)
			}
			if strings.Contains(string(data), githubToken) {
				t.Fatalf("file %s on disk contains leaked github token!", fname)
			}
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v ./internal/storage -run '^(TestStore_PermissionsAndGitignore|TestStore_CredentialRedaction_)'`  
Expected: FAIL.

- [ ] **Step 3: Implement `internal/storage/platform.go` and `internal/storage/redaction.go`**

In `internal/storage/redaction.go`:
Implement regex matching for GitHub tokens (`ghp_[A-Za-z0-9_]{36}`), OpenAI/Anthropic API keys (`sk-[A-Za-z0-9_]{32,}`), Slack tokens (`xox[baprs]-[A-Za-z0-9_]+`), replacing them with `[REDACTED]`.
Connect this to `QueuePrompt`, `ReplacePendingPrompt`, `ReleaseTurn`, `PublishArtifact`.

In `internal/storage/platform.go`:
Implement POSIX `0700`/`0600` permission setting and Windows `icacls` restriction to user SID when on Windows.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/storage -run '^(TestStore_PermissionsAndGitignore|TestStore_CredentialRedaction_)'`  
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/platform.go internal/storage/redaction.go internal/storage/platform_test.go internal/storage/redaction_test.go
git commit -m "feat(storage): enforce platform permissions, sidecar protection, and pre-write credential redaction"
```

---

### Task 8: Full Verification Suite, Cross-Platform Controls & PR Readiness

**Files:**
- Modify: `docs/superpowers/plans/2026-09-19-ac-002-durable-state.md` (check all tasks)
- Test: All packages across repo

**Steps:**
- [ ] **Step 1: Run complete pure-Go suite with CGO_ENABLED=0**

```bash
CGO_ENABLED=0 go test -v ./...
```
Expected: All tests pass across all packages (`internal/adapter/...`, `internal/storage/...`, etc.).

- [ ] **Step 2: Run race detector suite with CGO_ENABLED=1**

```bash
CGO_ENABLED=1 go test -race -count=3 ./...
```
Expected: All tests pass with zero data races.

- [ ] **Step 3: Run linter and formatting checks**

```bash
go vet ./...
test -z "$(gofmt -s -l .)"
```
Expected: Zero vet issues, zero unformatted files.

- [ ] **Step 4: Verify Python test suite and repository integrity**

```bash
pytest
```
Expected: All Python tests pass.

- [ ] **Step 5: Commit any adjustments, push branch to origin, and open PR for Issue #2**

```bash
git push origin feat/ac-002-durable-state
gh pr create --title "feat(storage): AC-002 durable state, native bindings, verified artifacts, and crash recovery" ...
```
