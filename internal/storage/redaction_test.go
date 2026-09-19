package storage_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_CredentialRedaction_RealStorageBoundary(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	secretKey := "sk-12345678901234567890123456789012"
	githubToken := "ghp_123456789012345678901234567890123456"
	slackToken := "xoxb-1234567890-abcdefg"

	rawPrompt := "Use secret " + secretKey + " and token " + githubToken + " and " + slackToken

	// 1. QueuePrompt with raw secrets
	r, err := store.QueuePrompt(ctx, "op-q-sec", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-sec", Prompt: rawPrompt, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	// Receipt payload must have redacted tokens
	if strings.Contains(r.Payload, secretKey) || strings.Contains(r.Payload, githubToken) || strings.Contains(r.Payload, slackToken) {
		t.Fatalf("receipt payload contains raw secret tokens: %s", r.Payload)
	}

	// 2. ReleaseTurn receipt must also have redacted tokens
	relReceipt, err := store.ReleaseTurn(ctx, "op-rel-sec", "lease-1", "sess-1", 2, "turn-sec")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}
	if strings.Contains(relReceipt.SanitizedPrompt, secretKey) || strings.Contains(relReceipt.SanitizedPrompt, githubToken) || strings.Contains(relReceipt.SanitizedPrompt, slackToken) {
		t.Fatalf("release receipt contains raw secret tokens: %s", relReceipt.SanitizedPrompt)
	}

	// 3. PublishArtifact with raw secrets in content
	rawArtifact := []byte("Config with " + secretKey + " and " + githubToken)
	artMeta, err := store.PublishArtifact(ctx, "op-pub-sec", "lease-1", storage.ArtifactMetadata{
		ID: "art-sec", RunID: "run-1", SessionID: "sess-1", TurnKey: "turn-sec", Name: "env.json",
	}, rawArtifact)
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	// Close store to flush WAL
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// 4. Read database files and artifact file from disk; assert raw secrets NEVER appear anywhere on disk
	filesToCheck := []string{
		filepath.Join(tempDir, "state.db"),
		filepath.Join(tempDir, "state.db-wal"),
		filepath.Join(tempDir, "artifacts", artMeta.Digest[:2], artMeta.Digest),
	}

	for _, fpath := range filesToCheck {
		data, err := os.ReadFile(fpath)
		if err == nil {
			if strings.Contains(string(data), secretKey) {
				t.Fatalf("file %s on disk contains leaked secret key!", fpath)
			}
			if strings.Contains(string(data), githubToken) {
				t.Fatalf("file %s on disk contains leaked github token!", fpath)
			}
			if strings.Contains(string(data), slackToken) {
				t.Fatalf("file %s on disk contains leaked slack token!", fpath)
			}
		}
	}
}
