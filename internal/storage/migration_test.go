package storage_test

import (
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

	// Brand-new DB: Open applied schema v1 then v2 sequentially
	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if ver != 2 {
		t.Fatalf("expected schema version 2, got %d", ver)
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
	if err != storage.ErrMigrationChecksumMismatch {
		t.Fatalf("expected ErrMigrationChecksumMismatch on corrupt checksum, got %v", err)
	}

	// Newer unsupported version: insert version 99; next open must reject with ErrUnsupportedSchemaVersion
	store2, err := storage.Open(storage.StoreOptions{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	defer store2.Close()
	_, err = store2.DB().Exec("INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (99, 'future_migration', 'dummy', '2026-09-19T00:00:00Z');")
	if err != nil {
		t.Fatalf("insert v99: %v", err)
	}
	store2Dir := store2.StateDir()
	store2.Close()

	_, err = storage.Open(storage.StoreOptions{StateDir: store2Dir})
	if err != storage.ErrUnsupportedSchemaVersion {
		t.Fatalf("expected ErrUnsupportedSchemaVersion on newer version, got %v", err)
	}
}

func TestStore_Migrations_ConcurrentOpen(t *testing.T) {
	tempDir := t.TempDir()
	// Open two stores pointing to the same uninitialized directory
	s1, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open s1: %v", err)
	}
	defer s1.Close()

	s2, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open s2: %v", err)
	}
	defer s2.Close()

	ver1, err := s1.CurrentSchemaVersion()
	if err != nil || ver1 != 2 {
		t.Fatalf("s1 ver: %d, err: %v", ver1, err)
	}
	ver2, err := s2.CurrentSchemaVersion()
	if err != nil || ver2 != 2 {
		t.Fatalf("s2 ver: %d, err: %v", ver2, err)
	}
}
