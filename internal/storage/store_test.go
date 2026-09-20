package storage_test

import (
	"os"
	"path/filepath"
	"runtime"
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
	tempDir := t.TempDir()
	specialName := "special dir #1 ? test"
	if runtime.GOOS == "windows" {
		specialName = "special dir #1 % test"
	}
	trickyPath := filepath.Join(tempDir, specialName)
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
