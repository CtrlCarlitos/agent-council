package storage

// The schema-v7 migration (AC-010 Agy adapter durable state) must be
// asserted exactly: a fresh store lands on 7 with all five agy_* tables
// present and every prior table intact; a frozen v6 database upgrades
// to 7 with every prior codex row intact and the new tables usable.

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func openAgyStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(StoreOptions{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestAC010_MigrationV7_SchemaVersionIsExactly7(t *testing.T) {
	store := openAgyStore(t)
	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if ver != 7 {
		t.Fatalf("expected schema version exactly 7, got %d", ver)
	}
	// All five AC-010 agy tables must exist.
	for _, table := range []string{
		"agy_session_bindings", "agy_turn_attempts", "agy_attempt_launches",
		"agy_protection_attestations", "agy_creation_uncertainties",
	} {
		var name string
		err := store.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %s must exist: %v", table, err)
		}
	}
	// Prior tables (AC-004/AC-008/AC-009) must remain intact.
	for _, table := range []string{
		"runs", "sessions", "schema_migrations", "pending_prompts", "dispatch_intents",
		"claude_session_bindings", "claude_turn_attempts",
		"claude_attempt_launches", "claude_protection_attestations",
		"codex_session_bindings", "codex_turn_attempts",
		"codex_attempt_launches", "codex_protection_attestations",
		"codex_creation_uncertainties",
	} {
		var name string
		err := store.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("prior table %s must remain intact: %v", table, err)
		}
	}
	// The required_tools_json column must exist on pending_prompts and
	// dispatch_intents with the '[]' default.
	var defaultVal string
	if err := store.DB().QueryRow(`SELECT dflt_value FROM pragma_table_info('pending_prompts') WHERE name = 'required_tools_json'`).Scan(&defaultVal); err != nil {
		t.Fatalf("pending_prompts.required_tools_json must exist: %v", err)
	}
	if defaultVal != "'[]'" {
		t.Fatalf("pending_prompts.required_tools_json default must be '[]', got %q", defaultVal)
	}
	if err := store.DB().QueryRow(`SELECT dflt_value FROM pragma_table_info('dispatch_intents') WHERE name = 'required_tools_json'`).Scan(&defaultVal); err != nil {
		t.Fatalf("dispatch_intents.required_tools_json must exist: %v", err)
	}
	if defaultVal != "'[]'" {
		t.Fatalf("dispatch_intents.required_tools_json default must be '[]', got %q", defaultVal)
	}
}

// applyFrozenV6 extends applyFrozenV5 (migration_v6_test.go) with the
// frozen v6 DDL and its schema_migrations row, producing a raw v6
// database to upgrade from.
func applyFrozenV6(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db := applyFrozenV5(t, dir)
	if _, err := db.Exec(codexUncertaintyV6DDL); err != nil {
		t.Fatalf("apply frozen v6 schema: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at)
		VALUES (6, 'codex_creation_uncertainty_episodes', ?, ?);`, "codex-uncertainty-v6", now); err != nil {
		t.Fatalf("record v6 migration row: %v", err)
	}
	return db
}

func TestAC010_MigrationV7_UpgradesV6Database(t *testing.T) {
	dir := t.TempDir()
	rawDB := applyFrozenV6(t, dir)

	// Seed v6 (codex) data that must survive the upgrade.
	if _, err := rawDB.Exec(`
INSERT INTO codex_session_bindings
	(session_id, native_id, materialized, model, workspace, profile_digest, created_at)
VALUES ('codex-sess', '01934f7a-1b2c-7def-9abc-def01234567a', 0, 'm', 'w', 'cprof-v3:sha256:pd', '2026-09-23T12:00:00Z');`); err != nil {
		t.Fatalf("seed codex binding: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := rawDB.Exec(`
INSERT INTO codex_creation_uncertainties
	(session_id, episode, run_id, reason, recorded_by, record_op_id, cause_op_id, recorded_at)
VALUES ('codex-sess', 1, 'run-x', 'lost', 'adapter', 'op-1', 'op-c1', ?)`, now); err != nil {
		t.Fatalf("seed codex uncertainty episode: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw v6 db: %v", err)
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
	var v7Name, v7Checksum string
	if err := store.readDB.QueryRow("SELECT name, checksum FROM schema_migrations WHERE version = 7;").Scan(&v7Name, &v7Checksum); err != nil {
		t.Fatalf("read v7 migration record: %v", err)
	}
	if v7Name != "agy_adapter_state" || v7Checksum != "agy-state-v7" {
		t.Fatalf("unexpected v7 record: name=%q checksum=%q", v7Name, v7Checksum)
	}
	var v6Count int
	if err := store.readDB.QueryRow("SELECT count(*) FROM schema_migrations WHERE version = 6;").Scan(&v6Count); err != nil || v6Count != 1 {
		t.Fatalf("expected exactly one v6 migration row, got %d err=%v", v6Count, err)
	}

	// Prior codex data is intact.
	var bindingCount int
	if err := store.readDB.QueryRow("SELECT count(*) FROM codex_session_bindings WHERE session_id = 'codex-sess';").Scan(&bindingCount); err != nil || bindingCount != 1 {
		t.Fatalf("expected the v5 codex binding to survive the upgrade, got %d err=%v", bindingCount, err)
	}
	open, err := store.OpenCodexCreationUncertainty(context.Background(), "codex-sess")
	if err != nil || open == nil || open.Episode != 1 {
		t.Fatalf("expected the v6 codex uncertainty episode to survive the upgrade, got %+v err=%v", open, err)
	}

	// The new agy tables are usable.
	if err := store.InsertAgySessionBinding(context.Background(), AgySessionBinding{
		SessionID: "agy-sess", NativeID: "0195f7a1-2b3c-4def-9abc-def012345678",
		Model: "agy-1", Workspace: "/tmp/ws",
		ProfileDigest: "aprof-v1:sha256:pd",
	}); err != nil {
		t.Fatalf("insert agy binding on upgraded store: %v", err)
	}
	var reqTools string
	if err := store.DB().QueryRow(`SELECT dflt_value FROM pragma_table_info('pending_prompts') WHERE name = 'required_tools_json'`).Scan(&reqTools); err != nil {
		t.Fatalf("pending_prompts.required_tools_json must exist after upgrade: %v", err)
	}
	if reqTools != "'[]'" {
		t.Fatalf("pending_prompts.required_tools_json default must be '[]' after upgrade, got %q", reqTools)
	}
}
