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
		_ = txB.Rollback()
		_ = txA.Rollback()
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

func TestStore_WriteTx_RollbackAndReuse(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Transaction 1: insert dummy data into schema_migrations and rollback
	tx1, err := store.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	_, err = tx1.Tx().ExecContext(ctx, "INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (50, 'rollback_test', 'dummy', '2026-09-19T00:00:00Z');")
	if err != nil {
		_ = tx1.Rollback()
		t.Fatalf("exec insert: %v", err)
	}
	if err := tx1.Rollback(); err != nil {
		t.Fatalf("rollback tx1: %v", err)
	}

	// Verify row was not committed
	var count int
	if err := store.DB().QueryRow("SELECT count(*) FROM schema_migrations WHERE version = 50;").Scan(&count); err != nil {
		t.Fatalf("query count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows after rollback, got %d", count)
	}

	// Transaction 2: connection can be reused immediately
	tx2, err := store.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("begin tx2 after rollback: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("rollback tx2: %v", err)
	}
}
