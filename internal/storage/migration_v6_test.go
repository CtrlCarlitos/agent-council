package storage

// The schema-v6 migration (AC-009 creation-uncertainty episodes) must be
// asserted exactly: a fresh store lands on 6 with the episode table and
// its open-episode partial unique index; a frozen v5 database upgrades
// to 6 with every prior row intact.

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestAC009_MigrationV6_FreshStoreHasEpisodeTable(t *testing.T) {
	store := openCodexStore(t)
	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if ver != 7 {
		t.Fatalf("expected schema version exactly 7, got %d", ver)
	}
	var name string
	if err := store.DB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='codex_creation_uncertainties'`).Scan(&name); err != nil {
		t.Fatalf("episode table must exist: %v", err)
	}
	if err := store.DB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='codex_creation_uncertainties_open'`).Scan(&name); err != nil {
		t.Fatalf("open-episode partial unique index must exist: %v", err)
	}
	// The schema itself refuses two open episodes for one session.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`
INSERT INTO codex_creation_uncertainties
	(session_id, episode, run_id, reason, recorded_by, record_op_id, cause_op_id, recorded_at)
VALUES ('s', 1, 'r', 'lost', 'adapter', 'op-1', 'op-c1', ?)`, now); err != nil {
		t.Fatalf("seed episode 1: %v", err)
	}
	if _, err := store.DB().Exec(`
INSERT INTO codex_creation_uncertainties
	(session_id, episode, run_id, reason, recorded_by, record_op_id, cause_op_id, recorded_at)
VALUES ('s', 2, 'r', 'lost again', 'adapter', 'op-2', 'op-c2', ?)`, now); err == nil {
		t.Fatal("a second OPEN episode for the same session must violate the partial unique index")
	}
}

func applyFrozenV5(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db := applyFrozenV4(t, dir)
	if _, err := db.Exec(codexStateV5DDL); err != nil {
		t.Fatalf("apply frozen v5 schema: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at)
		VALUES (5, 'codex_adapter_state', ?, ?);`, "codex-state-v5", now); err != nil {
		t.Fatalf("record v5 migration row: %v", err)
	}
	return db
}

func TestAC009_MigrationV6_UpgradesV5Database(t *testing.T) {
	dir := t.TempDir()
	rawDB := applyFrozenV5(t, dir)

	// Seed v5 data that must survive the upgrade.
	if _, err := rawDB.Exec(`
INSERT INTO codex_session_bindings
	(session_id, native_id, materialized, model, workspace, profile_digest, created_at)
VALUES ('codex-sess', '01934f7a-1b2c-7def-9abc-def01234567a', 0, 'm', 'w', 'cprof-v3:sha256:pd', '2026-09-23T12:00:00Z');`); err != nil {
		t.Fatalf("seed codex binding: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw v5 db: %v", err)
	}

	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer store.Close()

	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if ver != 7 {
		t.Fatalf("expected schema version exactly 7, got %d", ver)
	}
	var v6Name, v6Checksum string
	if err := store.readDB.QueryRow("SELECT name, checksum FROM schema_migrations WHERE version = 6;").Scan(&v6Name, &v6Checksum); err != nil {
		t.Fatalf("read v6 migration record: %v", err)
	}
	if v6Name != "codex_creation_uncertainty_episodes" || v6Checksum != "codex-uncertainty-v6" {
		t.Fatalf("unexpected v6 record: name=%q checksum=%q", v6Name, v6Checksum)
	}
	var v5Count int
	if err := store.readDB.QueryRow("SELECT count(*) FROM schema_migrations WHERE version = 5;").Scan(&v5Count); err != nil || v5Count != 1 {
		t.Fatalf("expected exactly one v5 migration row, got %d err=%v", v5Count, err)
	}
	var bindingCount int
	if err := store.readDB.QueryRow("SELECT count(*) FROM codex_session_bindings WHERE session_id = 'codex-sess';").Scan(&bindingCount); err != nil || bindingCount != 1 {
		t.Fatalf("expected the v5 binding to survive the upgrade, got %d err=%v", bindingCount, err)
	}
	if open, err := store.OpenCodexCreationUncertainty(context.Background(), "codex-sess"); err != nil || open != nil {
		t.Fatalf("the upgraded episode table must be queryable and empty, got %+v err=%v", open, err)
	}
}
