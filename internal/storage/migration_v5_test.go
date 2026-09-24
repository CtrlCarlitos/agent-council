package storage

// The schema-v5 migration must be asserted exactly: at least one test
// pins the version to 6 to prevent v5/v6 migration omission from passing,
// with the AC-009 tables present and all prior tables intact.

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestAC009_MigrationV5_SchemaVersionIsExactly6(t *testing.T) {
	store := openCodexStore(t)
	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if ver != 6 {
		t.Fatalf("expected schema version exactly 6, got %d", ver)
	}
	// The AC-009 tables must exist.
	for _, table := range []string{
		"codex_session_bindings", "codex_turn_attempts",
		"codex_attempt_launches", "codex_protection_attestations",
	} {
		var name string
		err := store.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %s must exist: %v", table, err)
		}
	}
	// Prior tables must remain intact.
	for _, table := range []string{
		"runs", "sessions", "schema_migrations",
		"claude_session_bindings", "claude_turn_attempts",
		"claude_attempt_launches", "claude_protection_attestations",
	} {
		var name string
		err := store.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("prior table %s must remain intact: %v", table, err)
		}
	}
}

func applyFrozenV4(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", buildDSN(dir+"/state.db", "immediate"))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}

	// Apply v1
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("apply frozen v1 schema: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at)
		VALUES (1, 'initial_schema', ?, ?);`, schemaChecksum(), now); err != nil {
		t.Fatalf("record v1 migration row: %v", err)
	}

	// Apply v2
	if _, err := db.Exec(schemaV2DDL); err != nil {
		t.Fatalf("apply frozen v2 schema: %v", err)
	}
	if _, err := db.Exec(backfillControllerLeaseProvenance, now, now); err != nil {
		t.Fatalf("apply v2 backfill: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at)
		VALUES (2, 'controller_leases_provenance', ?, ?);`, schemaV2Checksum(), now); err != nil {
		t.Fatalf("record v2 migration row: %v", err)
	}

	// Apply v3
	if _, err := db.Exec(schemaV3DDL); err != nil {
		t.Fatalf("apply frozen v3 schema: %v", err)
	}
	if _, err := db.Exec(backfillRunProfilesAndArtifacts); err != nil {
		t.Fatalf("apply v3 backfill: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at)
		VALUES (3, 'run_profiles_proposal_sets', ?, ?);`, schemaV3Checksum(), now); err != nil {
		t.Fatalf("record v3 migration row: %v", err)
	}

	// Apply v4
	if _, err := db.Exec(claudeStateV4DDL); err != nil {
		t.Fatalf("apply frozen v4 schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at)
		VALUES (4, 'claude_adapter_state', ?, ?);`, "claude-state-v4", now); err != nil {
		t.Fatalf("record v4 migration row: %v", err)
	}

	return db
}

func TestAC009_MigrationV5_UpgradesV4Database(t *testing.T) {
	dir := t.TempDir()
	rawDB := applyFrozenV4(t, dir)

	// Seed prior-version data that must survive the upgrade.
	now := "2026-09-23T12:00:00Z"
	if _, err := rawDB.Exec(`
INSERT INTO claude_session_bindings
	(session_id, native_id, materialized, model, workspace, config_root,
	 template_digest, first_prompt_digest, created_at)
VALUES ('claude-sess', '11111111-2222-4333-8444-555555555555', 0, 'm', 'w', 'cr', 'td', 'fpd', '` + now + `');`); err != nil {
		t.Fatalf("seed claude binding: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw v4 db: %v", err)
	}

	// Open with Store to trigger migration to v5.
	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer store.Close()

	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if ver != 6 {
		t.Fatalf("expected schema version exactly 6, got %d", ver)
	}

	// Check the schema_migrations row for v5.
	var v5Name, v5Checksum string
	err = store.readDB.QueryRow("SELECT name, checksum FROM schema_migrations WHERE version = 5;").Scan(&v5Name, &v5Checksum)
	if err != nil {
		t.Fatalf("read v5 migration record: %v", err)
	}
	if v5Name != "codex_adapter_state" || v5Checksum != "codex-state-v5" {
		t.Fatalf("unexpected v5 record: name=%q checksum=%q", v5Name, v5Checksum)
	}

	// The v4 row must not be duplicated or corrupted.
	var v4Count int
	if err := store.readDB.QueryRow("SELECT count(*) FROM schema_migrations WHERE version = 4;").Scan(&v4Count); err != nil {
		t.Fatalf("count v4 rows: %v", err)
	}
	if v4Count != 1 {
		t.Fatalf("expected exactly one v4 migration row, got %d", v4Count)
	}

	// Prior data is intact.
	var bindingCount int
	if err := store.readDB.QueryRow("SELECT count(*) FROM claude_session_bindings WHERE session_id = 'claude-sess';").Scan(&bindingCount); err != nil {
		t.Fatalf("count claude bindings: %v", err)
	}
	if bindingCount != 1 {
		t.Fatalf("expected the v4 binding to survive the upgrade, got %d", bindingCount)
	}

	// The new codex tables are usable.
	if err := store.InsertCodexSessionBinding(context.Background(), CodexSessionBinding{
		SessionID: "codex-sess", NativeID: "01934f7a-1b2c-7def-9abc-def01234567a",
		Model: "gpt-5-codex", Workspace: "/tmp/ws",
		ProfileDigest: "cprof-v3:sha256:pd",
	}); err != nil {
		t.Fatalf("insert codex binding on upgraded store: %v", err)
	}
}
