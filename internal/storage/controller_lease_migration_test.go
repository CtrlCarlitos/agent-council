package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression evidence for AC-004 Task 1: migration v2 must genuinely upgrade
// a verified v1 database — frozen v1 inputs, sequential application, legacy
// generation-0 provenance (never an alternate controller), and complete
// preservation of pending prompts, turns, attempt identities, native
// bindings, and journal evidence.

// applyFrozenV1 creates a verified v1 database: the released v1 schema
// applied directly plus the version-1 migration row exactly as v1 code
// recorded it. Data is seeded through raw SQL matching the v1 schema so the
// fixture never depends on upgraded code paths.
func applyFrozenV1(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", buildDSN(dir+"/state.db", "immediate"))
	if err != nil {
		t.Fatalf("open raw v1 db: %v", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("apply frozen v1 schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at)
		VALUES (1, 'initial_schema', ?, ?);`, schemaChecksum(), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("record v1 migration row: %v", err)
	}
	return db
}

// seedV1GoldenData writes a deterministic v1 dataset with raw SQL:
// queued prompt (never released), running turn with attempt identity and
// acknowledged intent, cancelling turn, native binding, and journal evidence.
func seedV1GoldenData(t *testing.T, db *sql.DB) {
	t.Helper()
	now := "2026-09-20T00:00:00Z"
	stmts := []string{
		`INSERT INTO runs (run_id, brief_digest, source_digest, profile_digest, controller_lease, lifecycle, created_at, updated_at)
		 VALUES ('run-v1', 'brief', 'source', 'profile', 'legacy-secret-1', 'active', '` + now + `', '` + now + `');`,
		`INSERT INTO sessions (session_id, run_id, contributor, is_active_contributor, state, lifecycle, controller_status, visibility, recovery_gen, active_recovery_gen, row_version, created_at, updated_at)
		 VALUES ('sess-v1', 'run-v1', 'claude', 1, 'running', 'active', 'connected', 'reachable', 0, 0, 3, '` + now + `', '` + now + `');`,
		`UPDATE sessions SET active_key = 'turn-run' WHERE session_id = 'sess-v1';`,
		`INSERT INTO pending_prompts (session_id, turn_key, prompt, queued_at)
		 VALUES ('sess-v1', 'turn-queued', 'follow-up work', '` + now + `');`,
		`INSERT INTO turns (session_id, turn_key, prompt, status, result, attempt_id, created_at)
		 VALUES ('sess-v1', 'turn-run', 'accepted work', 'running', '', 'attempt-original-1', '` + now + `');`,
		`INSERT INTO dispatch_intents (session_id, turn_key, attempt_id, phase, recorded_at, updated_at)
		 VALUES ('sess-v1', 'turn-run', 'attempt-original-1', 'receipt_acknowledged', '` + now + `', '` + now + `');`,
		`INSERT INTO native_bindings (session_id, native_session_id, harness, model, workspace_mode, config_json, created_at, updated_at)
		 VALUES ('sess-v1', 'native-sess-v1', 'claude', '', '', '', '` + now + `', '` + now + `');`,
		`INSERT INTO journal_entries (op_id, command_type, command_fingerprint, run_id, session_id, turn_key, event_kind, payload_json, created_at)
		 VALUES ('op-v1-release', 'release_turn', 'fp1', 'run-v1', 'sess-v1', 'turn-run', 'turn_released',
		 '{"caller_lease":"legacy-secret-1","receipt":{"op_id":"op-v1-release","command_type":"release_turn","session_id":"sess-v1","turn_key":"turn-run","committed_version":3,"created_at":"2026-09-20T00:00:00Z","payload":"attempt-original-1"}}', '` + now + `');`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed v1 golden data (%q): %v", firstLine(stmt), err)
		}
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// The upgraded store must migrate a verified v1 database: version 2, one
// generation-0 provenance row per existing run, controller_adopted = 0,
// accepted intents stamped with issuing generation 0 and their original
// attempt identities intact, and complete record preservation.
func TestAC004_Migration_UpgradesVerifiedV1Database(t *testing.T) {
	dir := t.TempDir()
	db := applyFrozenV1(t, dir)
	seedV1GoldenData(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("close v1 fixture: %v", err)
	}

	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer store.Close()

	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if ver != 2 {
		t.Fatalf("expected schema version 2, got %d", ver)
	}

	// Generation-0 provenance row: legacy credential preserved as
	// provenance, not an adopted grant.
	var harness sql.NullString
	var controllerRef, leaseVal, status, grantedBy string
	var connected int
	err = store.readDB.QueryRow(`SELECT harness, controller_ref, lease, status, granted_by_op_id, connected
		FROM controller_leases WHERE run_id = 'run-v1' AND generation = 0;`).
		Scan(&harness, &controllerRef, &leaseVal, &status, &grantedBy, &connected)
	if err != nil {
		t.Fatalf("provenance row missing: %v", err)
	}
	if harness.Valid || controllerRef != "legacy-v1" || leaseVal != "legacy-secret-1" || status != "legacy" || grantedBy != "legacy-v1" || connected != 0 {
		t.Fatalf("unexpected provenance row: harness=%v ref=%q lease=%q status=%q granted_by=%q connected=%d",
			harness, controllerRef, leaseVal, status, grantedBy, connected)
	}

	var adopted int
	if err := store.readDB.QueryRow(`SELECT controller_adopted FROM runs WHERE run_id = 'run-v1';`).Scan(&adopted); err != nil {
		t.Fatalf("read controller_adopted: %v", err)
	}
	if adopted != 0 {
		t.Fatalf("migrated run must remain unadopted, got controller_adopted=%d", adopted)
	}

	// Accepted intent keeps its original attempt identity and is stamped
	// with issuing generation 0.
	var attemptID string
	var issuingGen int
	if err := store.readDB.QueryRow(`SELECT attempt_id, issuing_controller_generation FROM dispatch_intents
		WHERE session_id = 'sess-v1' AND turn_key = 'turn-run';`).Scan(&attemptID, &issuingGen); err != nil {
		t.Fatalf("read dispatch intent: %v", err)
	}
	if attemptID != "attempt-original-1" {
		t.Fatalf("original attempt identity modified: %q", attemptID)
	}
	if issuingGen != 0 {
		t.Fatalf("expected issuing_controller_generation 0 for migrated accepted attempt, got %d", issuingGen)
	}

	// Preservation: pending prompts, turns, bindings, journal evidence.
	hydrated, err := store.HydrateState(context.Background())
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	sess, ok := hydrated.Sessions["sess-v1"]
	if !ok {
		t.Fatal("session lost in migration")
	}
	if _, queued := sess.PendingPrompts["turn-queued"]; !queued {
		t.Fatalf("queued follow-up prompt lost: %+v", sess.PendingPrompts)
	}
	turnRun, ok := sess.Turns["turn-run"]
	if !ok || turnRun.Status != "running" {
		t.Fatalf("running turn lost or altered: %+v", sess.Turns)
	}
	if sess.NativeBinding == nil || sess.NativeBinding.NativeSessionID != "native-sess-v1" {
		t.Fatalf("native binding lost: %+v", sess.NativeBinding)
	}
	journalReceipt, found, err := store.FindOperationReceipt(context.Background(), "op-v1-release", "legacy-secret-1")
	if err != nil || !found || journalReceipt == nil {
		t.Fatalf("journal evidence lost: found=%v err=%v", found, err)
	}

	// No automatic execution: the queued follow-up was never released.
	if _, hasTurn := sess.Turns["turn-queued"]; hasTurn {
		t.Fatal("queued prompt must not be released by migration")
	}
}

// A fresh database applies v1 then v2 sequentially and carries no
// provenance rows for its (empty) runs.
func TestAC004_Migration_FreshDatabaseSequential(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	defer store.Close()

	ver, err := store.CurrentSchemaVersion()
	if err != nil || ver != 2 {
		t.Fatalf("expected fresh database at version 2, got %d err=%v", ver, err)
	}
	var count int
	if err := store.readDB.QueryRow(`SELECT count(*) FROM controller_leases;`).Scan(&count); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if count != 0 {
		t.Fatalf("fresh database must have no provenance rows, got %d", count)
	}
	var rows int
	if err := store.readDB.QueryRow(`SELECT count(*) FROM schema_migrations;`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("expected both migration rows recorded, got %d err=%v", rows, err)
	}
}

// Repeat-open is idempotent: no duplicate migrations or provenance rows.
func TestAC004_Migration_RepeatOpenIdempotent(t *testing.T) {
	dir := t.TempDir()
	db := applyFrozenV1(t, dir)
	seedV1GoldenData(t, db)
	_ = db.Close()

	for i := 0; i < 3; i++ {
		store, err := Open(StoreOptions{StateDir: dir})
		if err != nil {
			t.Fatalf("repeat open %d: %v", i, err)
		}
		_ = store.Close()
	}

	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("final open: %v", err)
	}
	defer store.Close()
	var provCount, migrationCount int
	_ = store.readDB.QueryRow(`SELECT count(*) FROM controller_leases WHERE run_id = 'run-v1';`).Scan(&provCount)
	_ = store.readDB.QueryRow(`SELECT count(*) FROM schema_migrations;`).Scan(&migrationCount)
	if provCount != 1 || migrationCount != 2 {
		t.Fatalf("repeat-open duplicated state: provenance=%d migrations=%d", provCount, migrationCount)
	}
}

// Concurrent initialization converges on a single applied migration set.
func TestAC004_Migration_ConcurrentInitialization(t *testing.T) {
	dir := t.TempDir()
	db := applyFrozenV1(t, dir)
	seedV1GoldenData(t, db)
	_ = db.Close()

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := Open(StoreOptions{StateDir: dir})
			if err != nil {
				errs[i] = err
				return
			}
			_ = s.Close()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent open %d: %v", i, err)
		}
	}

	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("verify open: %v", err)
	}
	defer store.Close()
	var migrationCount int
	if err := store.readDB.QueryRow(`SELECT count(*) FROM schema_migrations;`).Scan(&migrationCount); err != nil || migrationCount != 2 {
		t.Fatalf("expected exactly 2 migration rows after concurrency, got %d err=%v", migrationCount, err)
	}
}

// A checksum mismatch on the recorded v1 migration rejects the upgrade
// without applying v2 or partially modifying the database.
func TestAC004_Migration_ChecksumMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	db := applyFrozenV1(t, dir)
	if _, err := db.Exec(`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1;`); err != nil {
		t.Fatalf("tamper checksum: %v", err)
	}
	_ = db.Close()

	_, err := Open(StoreOptions{StateDir: dir})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum mismatch error, got %v", err)
	}

	// The database must remain at v1 with no v2 objects applied.
	verify, err := sql.Open("sqlite", buildDSN(dir+"/state.db", "immediate"))
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	defer verify.Close()
	var v2Tables int
	_ = verify.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='controller_leases';`).Scan(&v2Tables)
	if v2Tables != 0 {
		t.Fatal("v2 objects must not exist after checksum mismatch rejection")
	}
}

// Databases from a newer schema version are refused.
func TestAC004_Migration_UnsupportedNewerVersionRejected(t *testing.T) {
	dir := t.TempDir()
	db := applyFrozenV1(t, dir)
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at)
		VALUES (99, 'future', 'x', '2026-09-20T00:00:00Z');`); err != nil {
		t.Fatalf("insert future version: %v", err)
	}
	_ = db.Close()

	_, err := Open(StoreOptions{StateDir: dir})
	if err == nil || !strings.Contains(fmt.Sprint(err), "unsupported") {
		t.Fatalf("expected unsupported schema version error, got %v", err)
	}
}
