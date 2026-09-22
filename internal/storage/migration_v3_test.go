package storage

import (
	"database/sql"
	"testing"
	"time"
)

func applyFrozenV2(t *testing.T, dir string) *sql.DB {
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

	return db
}

func seedV2Data(t *testing.T, db *sql.DB) {
	t.Helper()
	now := "2026-09-20T12:00:00Z"
	stmts := []string{
		`INSERT INTO runs (run_id, brief_digest, source_digest, profile_digest, controller_lease, lifecycle, created_at, updated_at, controller_adopted)
		 VALUES ('run-v2-legacy', 'bd-1', 'sd-1', 'pd-legacy-123', 'lease-v2', 'active', '` + now + `', '` + now + `', 0);`,
		`INSERT INTO artifact_revisions (artifact_id, revision, run_id, kind, digest, byte_size, created_at)
		 VALUES ('art-1', 1, 'run-v2-legacy', 'proposal', 'sha-art-1', 100, '` + now + `');`,
		`INSERT INTO artifact_revisions (artifact_id, revision, run_id, kind, digest, byte_size, created_at)
		 VALUES ('art-2', 1, 'run-v2-legacy', 'finding', 'sha-art-2', 200, '` + now + `');`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed v2 data failed (%q): %v", stmt, err)
		}
	}
}

func TestAC005_MigrationV3_UpgradesVerifiedV2Database(t *testing.T) {
	dir := t.TempDir()
	rawDB := applyFrozenV2(t, dir)
	seedV2Data(t, rawDB)
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw v2 db: %v", err)
	}

	// Open with Store to trigger migration to v3
	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer store.Close()

	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if ver < 3 {
		t.Fatalf("expected schema version, got %d", ver)
	}

	// Check schema_migrations row for v3
	var v3Name, v3Checksum string
	err = store.readDB.QueryRow("SELECT name, checksum FROM schema_migrations WHERE version = 3;").Scan(&v3Name, &v3Checksum)
	if err != nil {
		t.Fatalf("read v3 migration record: %v", err)
	}
	if v3Name != "run_profiles_proposal_sets" || v3Checksum == "" {
		t.Fatalf("unexpected v3 record: name=%q checksum=%q", v3Name, v3Checksum)
	}

	// Verify legacy run backfilled in run_profiles
	var algoVer, wsMode, isoStrict, netMode, canonJSON, srcRepo, srcCommit, srcTree, briefArtDigest, createdAt string
	var profDigest string
	err = store.readDB.QueryRow(`
		SELECT profile_digest, algorithm_version, workspace_mode, isolation_strictness, network_mode,
		       canonical_profile_json, source_repo_identity, source_commit, source_tree, brief_artifact_digest, created_at
		FROM run_profiles
		WHERE run_id = 'run-v2-legacy';
	`).Scan(&profDigest, &algoVer, &wsMode, &isoStrict, &netMode, &canonJSON, &srcRepo, &srcCommit, &srcTree, &briefArtDigest, &createdAt)
	if err != nil {
		t.Fatalf("read run_profiles row for legacy run: %v", err)
	}

	if profDigest != "pd-legacy-123" {
		t.Fatalf("expected profile_digest pd-legacy-123, got %q", profDigest)
	}
	if algoVer != "legacy-unverified" {
		t.Fatalf("expected algorithm_version legacy-unverified, got %q", algoVer)
	}
	if wsMode != "none" {
		t.Fatalf("expected workspace_mode none, got %q", wsMode)
	}
	if isoStrict != "permissive_dev" {
		t.Fatalf("expected isolation_strictness permissive_dev, got %q", isoStrict)
	}
	if netMode != "unrestricted" {
		t.Fatalf("expected network_mode unrestricted, got %q", netMode)
	}
	if canonJSON != "" {
		t.Fatalf("expected empty canonical_profile_json, got %q", canonJSON)
	}
	if srcRepo != "legacy" {
		t.Fatalf("expected source_repo_identity legacy, got %q", srcRepo)
	}
	if srcCommit != "" || srcTree != "" || briefArtDigest != "" {
		t.Fatalf("expected empty commit, tree, and brief digests, got commit=%q tree=%q brief=%q", srcCommit, srcTree, briefArtDigest)
	}
	if createdAt != "2026-09-20T12:00:00Z" {
		t.Fatalf("expected created_at 2026-09-20T12:00:00Z, got %q", createdAt)
	}

	// Verify artifact_revisions backfill
	rows, err := store.readDB.Query("SELECT artifact_id, session_id, attempt_id, released, proposal_set_digest FROM artifact_revisions ORDER BY artifact_id;")
	if err != nil {
		t.Fatalf("query artifact_revisions: %v", err)
	}
	defer rows.Close()

	artCount := 0
	for rows.Next() {
		artCount++
		var artID string
		var sessID, attemptID, propSetDigest sql.NullString
		var released int
		if err := rows.Scan(&artID, &sessID, &attemptID, &released, &propSetDigest); err != nil {
			t.Fatalf("scan artifact_revisions: %v", err)
		}
		if sessID.Valid {
			t.Fatalf("expected NULL session_id for legacy artifact, got %q", sessID.String)
		}
		if attemptID.Valid {
			t.Fatalf("expected NULL attempt_id for legacy artifact, got %q", attemptID.String)
		}
		if released != 1 {
			t.Fatalf("expected released = 1 for legacy artifact, got %d", released)
		}
		if propSetDigest.Valid {
			t.Fatalf("expected NULL proposal_set_digest for legacy artifact, got %q", propSetDigest.String)
		}
	}
	if artCount != 2 {
		t.Fatalf("expected 2 artifact revisions, got %d", artCount)
	}

	// Verify proposal_sets and proposal_set_members are empty
	var propSetCount, memberCount int
	if err := store.readDB.QueryRow("SELECT count(*) FROM proposal_sets;").Scan(&propSetCount); err != nil {
		t.Fatalf("count proposal_sets: %v", err)
	}
	if propSetCount != 0 {
		t.Fatalf("expected 0 proposal_sets, got %d", propSetCount)
	}
	if err := store.readDB.QueryRow("SELECT count(*) FROM proposal_set_members;").Scan(&memberCount); err != nil {
		t.Fatalf("count proposal_set_members: %v", err)
	}
	if memberCount != 0 {
		t.Fatalf("expected 0 proposal_set_members, got %d", memberCount)
	}
}

func TestAC005_MigrationV3_Idempotence(t *testing.T) {
	dir := t.TempDir()
	rawDB := applyFrozenV2(t, dir)
	seedV2Data(t, rawDB)
	rawDB.Close()

	store1, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store1: %v", err)
	}
	ver1, err := store1.CurrentSchemaVersion()
	if err != nil || ver1 < 3 {
		t.Fatalf("store1 version: %d, err: %v", ver1, err)
	}
	// Calling Migrate again is clean no-op
	if err := store1.Migrate(); err != nil {
		t.Fatalf("repeated migrate failed: %v", err)
	}
	store1.Close()

	// Reopen
	store2, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("reopen store2: %v", err)
	}
	defer store2.Close()
	ver2, err := store2.CurrentSchemaVersion()
	if err != nil || ver2 < 3 {
		t.Fatalf("store2 version: %d, err: %v", ver2, err)
	}
}

func TestAC005_MigrationV3_RollbackOnFailure(t *testing.T) {
	dir := t.TempDir()
	rawDB := applyFrozenV2(t, dir)
	seedV2Data(t, rawDB)
	rawDB.Close()

	// Injected failure before committing migration v3
	_, err := Open(StoreOptions{
		StateDir: dir,
		TestHookBeforeCommit: func(boundary string) {
			if boundary == "pre_commit_migration_v3" {
				panic("injected migration v3 failure")
			}
		},
	})
	if err == nil {
		t.Fatal("expected error on injected migration v3 failure")
	}

	// Verify the database is still at v2 and unpolluted
	verifyDB, err := sql.Open("sqlite", buildDSN(dir+"/state.db", "immediate"))
	if err != nil {
		t.Fatalf("open verify db: %v", err)
	}
	defer verifyDB.Close()

	var maxVer int
	if err := verifyDB.QueryRow("SELECT max(version) FROM schema_migrations;").Scan(&maxVer); err != nil {
		t.Fatalf("read max version: %v", err)
	}
	if maxVer != 2 {
		t.Fatalf("expected db to remain at version 2 after rollback, got %d", maxVer)
	}

	var tableCount int
	if err := verifyDB.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='run_profiles';").Scan(&tableCount); err != nil {
		t.Fatalf("check run_profiles: %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("expected run_profiles table to not exist after rollback, got %d", tableCount)
	}

	// Clean retry should succeed
	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("reopen after rollback failed: %v", err)
	}
	defer store.Close()

	ver, err := store.CurrentSchemaVersion()
	if err != nil || ver < 3 {
		t.Fatalf("expected upgrade to version 3 on retry, got %d, err: %v", ver, err)
	}
}
