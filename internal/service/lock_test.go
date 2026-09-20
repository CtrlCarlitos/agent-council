package service

import (
	"os"
	"path/filepath"
	"testing"
)

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestServiceLock_Exclusivity(t *testing.T) {
	dir := t.TempDir()
	l1, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("first lock acquisition failed: %v", err)
	}
	defer l1.Release()

	// Second acquisition must fail with ErrServiceAlreadyRunning
	_, err = AcquireServiceLock(dir)
	if err != ErrServiceAlreadyRunning {
		t.Fatalf("expected ErrServiceAlreadyRunning, got: %v", err)
	}

	// Release first lock, then second must succeed
	if err := l1.Release(); err != nil {
		t.Fatalf("first release failed: %v", err)
	}

	l2, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("second lock acquisition failed after release: %v", err)
	}
	defer l2.Release()

	// Ensure lock file is never deleted
	lockPath := filepath.Join(dir, "service.lock")
	if !fileExists(lockPath) {
		t.Fatal("service.lock must remain on disk after release")
	}
}
