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

func TestDiscovery_PreExistingRegularFileAtSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "council.sock")
	if err := os.WriteFile(sockPath, []byte("regular file content"), 0644); err != nil {
		t.Fatalf("create regular file: %v", err)
	}

	meta := DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      "test-inst",
		PID:             os.Getpid(),
		Transport:       "unix",
		Endpoint:        sockPath,
		StateDir:        dir,
	}

	err := PublishDiscovery(dir, meta, "secret-token")
	if err == nil {
		t.Fatal("expected PublishDiscovery to fail when council.sock is a regular file")
	}

	// Verify regular file was not deleted
	if !fileExists(sockPath) {
		t.Fatal("regular file at council.sock must not be deleted")
	}
}

func TestDiscovery_PreExistingSymlinkAtTokenTmp(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "external-target.txt")
	if err := os.WriteFile(targetFile, []byte("original content"), 0644); err != nil {
		t.Fatalf("create target file: %v", err)
	}

	symlinkPath := filepath.Join(dir, "auth.token.tmp")
	if err := os.Symlink(targetFile, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	meta := DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      "test-inst",
		PID:             os.Getpid(),
		Transport:       "unix",
		Endpoint:        filepath.Join(dir, "council.sock"),
		StateDir:        dir,
	}

	_ = PublishDiscovery(dir, meta, "secret-token")

	// Target file must not have been overwritten
	content, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("read target file: %v", err)
	}
	if string(content) != "original content" {
		t.Fatalf("external file was overwritten through symlink: %s", string(content))
	}
}

func TestDiscovery_PreExistingBroadModeTokenTmp(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "auth.token.tmp")
	if err := os.WriteFile(tmpPath, []byte("old content"), 0644); err != nil {
		t.Fatalf("create broad mode tmp: %v", err)
	}

	meta := DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      "test-inst",
		PID:             os.Getpid(),
		Transport:       "unix",
		Endpoint:        filepath.Join(dir, "council.sock"),
		StateDir:        dir,
	}

	if err := PublishDiscovery(dir, meta, "secret-token"); err != nil {
		t.Fatalf("publish discovery: %v", err)
	}

	tokenPath := filepath.Join(dir, "auth.token")
	fi, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat auth.token: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected 0600 mode for auth.token, got %o", perm)
	}
}

func TestLock_RejectsSymlinkLockFile(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "target.lock")
	if err := os.WriteFile(targetFile, []byte("content"), 0600); err != nil {
		t.Fatalf("create target lock: %v", err)
	}

	symlinkPath := filepath.Join(dir, "service.lock")
	if err := os.Symlink(targetFile, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	lock, err := AcquireServiceLock(dir)
	if err == nil {
		_ = lock.Release()
		t.Fatal("expected AcquireServiceLock to reject symlink service.lock")
	}
}
