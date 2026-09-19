package storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestArtifactStore_ReadArtifact_VerifyBeforeExposure(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	content := []byte("legitimate artifact content")

	meta, err := store.PublishArtifact(ctx, "op-pub-1", "lease-1", storage.ArtifactMetadata{
		ID: "art-1", RunID: "run-1", SessionID: "sess-1", TurnKey: "turn-1", Name: "report.md",
	}, content)
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	// Reading valid artifact returns content
	readBytes, err := store.ReadArtifact(meta.Digest)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !bytes.Equal(readBytes, content) {
		t.Fatalf("content mismatch")
	}

	// Corrupt file on disk: tamper with bytes
	blobPath := filepath.Join(tempDir, "artifacts", meta.Digest[:2], meta.Digest)
	if err := os.WriteFile(blobPath, []byte("tampered data"), 0600); err != nil {
		t.Fatalf("tamper file: %v", err)
	}

	// ReadArtifact must return ErrArtifactCorrupt and ZERO bytes exposed
	tamperedBytes, err := store.ReadArtifact(meta.Digest)
	if err != storage.ErrArtifactCorrupt {
		t.Fatalf("expected ErrArtifactCorrupt on tampered file, got %v", err)
	}
	if len(tamperedBytes) != 0 {
		t.Fatalf("expected zero bytes exposed on corruption, got %d bytes", len(tamperedBytes))
	}
}

func TestArtifactStore_PublishArtifact_NoSilentRepair(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	content := []byte("clean artifact data")
	digest := fmt.Sprintf("%x", sha256.Sum256(content))

	// Pre-seed corrupt destination file
	destDir := filepath.Join(tempDir, "artifacts", digest[:2])
	_ = os.MkdirAll(destDir, 0700)
	destPath := filepath.Join(destDir, digest)
	corruptBytes := []byte("pre-existing corrupt bytes")
	_ = os.WriteFile(destPath, corruptBytes, 0600)

	// PublishArtifact must detect corrupt destination and return ErrArtifactCorrupt immediately WITHOUT repairing or overwriting
	_, err = store.PublishArtifact(ctx, "op-pub-corrupt", "lease-1", storage.ArtifactMetadata{
		ID: "art-corrupt", RunID: "run-1", SessionID: "sess-1", TurnKey: "turn-1", Name: "diff.patch",
	}, content)
	if err != storage.ErrArtifactCorrupt {
		t.Fatalf("expected ErrArtifactCorrupt when destination exists and is corrupt, got %v", err)
	}

	// Verify destination was NOT overwritten
	actualBytes, _ := os.ReadFile(destPath)
	if !bytes.Equal(actualBytes, corruptBytes) {
		t.Fatalf("corrupt destination was modified or repaired; must remain untouched")
	}

	// Verify no artifact revision was recorded in database
	var count int
	_ = store.DB().QueryRow("SELECT count(*) FROM artifact_revisions WHERE artifact_id = 'art-corrupt';").Scan(&count)
	if count != 0 {
		t.Fatalf("expected 0 artifact revisions recorded on failed publish, got %d", count)
	}
}

func TestArtifactStore_PublishArtifact_ConcurrentIdentical(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	content := []byte("concurrent artifact content")
	var wg sync.WaitGroup
	errCh := make(chan error, 5)

	for i := 1; i <= 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := store.PublishArtifact(ctx, fmt.Sprintf("op-pub-conc-%d", idx), "lease-1", storage.ArtifactMetadata{
				ID: fmt.Sprintf("art-%d", idx), RunID: "run-1", SessionID: "sess-1", TurnKey: "turn-1", Name: fmt.Sprintf("artifact-%d.txt", idx),
			}, content)
			if err != nil {
				errCh <- err
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent publish failed: %v", err)
	}
}

func TestArtifactStore_PublishArtifact_PathTraversalRejected(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	// Name with path traversal
	_, err = store.PublishArtifact(ctx, "op-pub-bad", "lease-1", storage.ArtifactMetadata{
		ID: "art-bad", RunID: "run-1", SessionID: "sess-1", TurnKey: "turn-1", Name: "../../../etc/passwd",
	}, []byte("evil"))
	if err != storage.ErrInvalidPath {
		t.Fatalf("expected ErrInvalidPath on path traversal, got %v", err)
	}
}
