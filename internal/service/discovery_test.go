package service

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscovery_SeparationAndPermissions(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	meta := DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      "inst-test-1",
		PID:             os.Getpid(),
		Transport:       "unix",
		Endpoint:        filepath.Join(dir, "council.sock"),
		StateDir:        dir,
	}
	token := "0123456789abcdef0123456789abcdef"

	if err := PublishDiscovery(dir, meta, token); err != nil {
		t.Fatalf("publish discovery failed: %v", err)
	}

	// Check auth.token exists with mode 0600
	tokenPath := filepath.Join(dir, "auth.token")
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat auth.token: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("auth.token permissions not 0600: %v", info.Mode().Perm())
	}

	// Check service.json exists with mode 0600 and does NOT contain token
	metaPath := filepath.Join(dir, "service.json")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read service.json: %v", err)
	}
	if len(metaBytes) == 0 || bytes.Contains(metaBytes, []byte(token)) {
		t.Fatal("service.json must not leak auth.token")
	}

	// Owner-bound cleanup removes service.json, auth.token, council.sock
	if err := lock.CleanupDiscovery(); err != nil {
		t.Fatalf("cleanup discovery failed: %v", err)
	}
	if fileExists(tokenPath) || fileExists(metaPath) {
		t.Fatal("expected discovery and token files to be unlinked")
	}
}

func TestDiscovery_FailedStartAndSuccessorSafety(t *testing.T) {
	dir := t.TempDir()
	winnerLock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("winner lock failed: %v", err)
	}
	defer winnerLock.Release()

	meta := DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      "winner-1",
		PID:             os.Getpid(),
		Transport:       "unix",
		Endpoint:        filepath.Join(dir, "council.sock"),
		StateDir:        dir,
	}
	token := "winner-token"
	if err := PublishDiscovery(dir, meta, token); err != nil {
		t.Fatalf("winner publish failed: %v", err)
	}

	// Loser attempts to acquire lock and fails
	loserLock, err := AcquireServiceLock(dir)
	if err != ErrServiceAlreadyRunning {
		t.Fatalf("expected ErrServiceAlreadyRunning, got %v", err)
	}
	if loserLock != nil {
		t.Fatal("loser lock must be nil")
	}

	// Unowned / unheld lock cannot remove winner's files
	var nilLock *ServiceLock
	if err := nilLock.CleanupDiscovery(); err == nil {
		t.Fatal("unowned cleanup must return error")
	}

	// Winner's discovery files remain intact
	metaPath := filepath.Join(dir, "service.json")
	if !fileExists(metaPath) {
		t.Fatal("winner's service.json must remain intact after loser failure")
	}
}
