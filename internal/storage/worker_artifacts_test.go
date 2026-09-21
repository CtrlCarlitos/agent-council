package storage_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Helper to seed a fixture run, controller, and active execution ref.
func setupWorkerArtifactFixture(t *testing.T, lease string) (*storage.Store, string, storage.ExecutionRef, storage.ExecutionRef) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	runID := "run-worker-art"
	if _, err := store.CreateRun(ctx, "op-run-1", runID, "brief-sha", "src-sha", "prof-sha", "bootstrap-"+lease); err != nil {
		t.Fatalf("create run: %v", err)
	}
	// Author session
	_, err = store.CreateSession(ctx, "op-sess-author", "bootstrap-"+lease, storage.SessionRecord{
		ID: "sess-author", RunID: runID, Contributor: "claude", Role: "worker",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create author session: %v", err)
	}
	// Sibling session
	_, err = store.CreateSession(ctx, "op-sess-sibling", "bootstrap-"+lease, storage.SessionRecord{
		ID: "sess-sibling", RunID: runID, Contributor: "codex", Role: "worker",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create sibling session: %v", err)
	}

	if _, err := store.AdoptController(ctx, "op-adopt", runID, "claude", "ctrl-ref", "bootstrap-"+lease, nil, lease); err != nil {
		t.Fatalf("adopt controller: %v", err)
	}
	if _, err := store.ConnectRunController(ctx, "op-conn", runID, lease, 1, "inst-1"); err != nil {
		t.Fatalf("connect controller: %v", err)
	}

	// Release a turn for author session
	ver, err := store.GetSessionVersion(ctx, "sess-author")
	if err != nil {
		t.Fatalf("get author session version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-auth", lease, "sess-author", ver, storage.PendingPrompt{
		SessionID: "sess-author", TurnKey: "turn-auth-1", Prompt: "work on task",
	}); err != nil {
		t.Fatalf("queue author prompt: %v", err)
	}
	ver, _ = store.GetSessionVersion(ctx, "sess-author")
	relAuth, err := store.ReleaseTurn(ctx, "op-rel-auth", lease, "sess-author", ver, "turn-auth-1")
	if err != nil {
		t.Fatalf("release author turn: %v", err)
	}
	authorRef := storage.ExecutionRef{
		SessionID: "sess-author",
		TurnKey:   "turn-auth-1",
		AttemptID: relAuth.Receipt.AttemptID,
	}

	// Release a turn for sibling session
	sibVer, err := store.GetSessionVersion(ctx, "sess-sibling")
	if err != nil {
		t.Fatalf("get sibling session version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-sib", lease, "sess-sibling", sibVer, storage.PendingPrompt{
		SessionID: "sess-sibling", TurnKey: "turn-sib-1", Prompt: "work on sibling task",
	}); err != nil {
		t.Fatalf("queue sibling prompt: %v", err)
	}
	sibVer, _ = store.GetSessionVersion(ctx, "sess-sibling")
	relSib, err := store.ReleaseTurn(ctx, "op-rel-sib", lease, "sess-sibling", sibVer, "turn-sib-1")
	if err != nil {
		t.Fatalf("release sibling turn: %v", err)
	}
	siblingRef := storage.ExecutionRef{
		SessionID: "sess-sibling",
		TurnKey:   "turn-sib-1",
		AttemptID: relSib.Receipt.AttemptID,
	}

	return store, runID, authorRef, siblingRef
}

func TestWorkerArtifacts_RecordObservedArtifact_ActiveTurn(t *testing.T) {
	store, _, authorRef, _ := setupWorkerArtifactFixture(t, "lease-art-1")
	defer store.Close()
	ctx := context.Background()

	content := []byte("# Proposal\nProposed changes here.\n")
	meta, err := store.RecordObservedArtifact(ctx, authorRef, "proposal.md", content)
	if err != nil {
		t.Fatalf("record observed artifact during active turn: %v", err)
	}

	if meta.ID != "proposal.md" {
		t.Errorf("expected ID 'proposal.md', got %q", meta.ID)
	}
	if meta.Name != "proposal.md" {
		t.Errorf("expected Name 'proposal.md', got %q", meta.Name)
	}
	if meta.SessionID != authorRef.SessionID {
		t.Errorf("expected SessionID %q, got %q", authorRef.SessionID, meta.SessionID)
	}
	if meta.TurnKey != authorRef.TurnKey {
		t.Errorf("expected TurnKey %q, got %q", authorRef.TurnKey, meta.TurnKey)
	}
	if meta.Revision != 1 {
		t.Errorf("expected Revision 1, got %d", meta.Revision)
	}
	if meta.ByteCount != int64(len(content)) {
		t.Errorf("expected ByteCount %d, got %d", len(content), meta.ByteCount)
	}
	if meta.Digest == "" {
		t.Errorf("expected non-empty digest")
	}

	// CAS read must succeed and match content
	blobBytes, err := store.ReadArtifact(meta.Digest)
	if err != nil {
		t.Fatalf("read artifact CAS: %v", err)
	}
	if !bytes.Equal(blobBytes, content) {
		t.Fatalf("CAS blob content mismatch: expected %q, got %q", string(content), string(blobBytes))
	}

	// Database record must verify unreleased draft status
	var released int
	var propSetDigest *string
	var sessID, attemptID string
	err = store.DB().QueryRowContext(ctx, `
SELECT released, proposal_set_digest, session_id, attempt_id
FROM artifact_revisions
WHERE artifact_id = ? AND revision = ?;`, meta.ID, meta.Revision).Scan(&released, &propSetDigest, &sessID, &attemptID)
	if err != nil {
		t.Fatalf("query artifact_revisions: %v", err)
	}
	if released != 0 {
		t.Errorf("expected released = 0, got %d", released)
	}
	if propSetDigest != nil {
		t.Errorf("expected proposal_set_digest = NULL, got %v", *propSetDigest)
	}
	if sessID != authorRef.SessionID {
		t.Errorf("expected session_id %q, got %q", authorRef.SessionID, sessID)
	}
	if attemptID != authorRef.AttemptID {
		t.Errorf("expected attempt_id %q, got %q", authorRef.AttemptID, attemptID)
	}
}

func TestWorkerArtifacts_RecordObservedArtifact_TerminalTurn(t *testing.T) {
	store, _, authorRef, _ := setupWorkerArtifactFixture(t, "lease-art-2")
	defer store.Close()
	ctx := context.Background()

	// Move turn to terminal completed state
	_, err := store.RecordObservedExecutionOutcome(ctx, "op-term-1", authorRef, council.TurnCompleted, "execution finished")
	if err != nil {
		t.Fatalf("record observed terminal outcome: %v", err)
	}

	// Ingestion immediately after terminal turn completion must succeed
	content := []byte("terminal artifact content")
	meta, err := store.RecordObservedArtifact(ctx, authorRef, "output.json", content)
	if err != nil {
		t.Fatalf("record observed artifact on terminal turn: %v", err)
	}
	if meta.ID != "output.json" || meta.Revision != 1 {
		t.Errorf("unexpected metadata: %+v", meta)
	}

	// Reading via ReadAuthorArtifact should succeed
	readBytes, readMeta, err := store.ReadAuthorArtifact(ctx, authorRef, "output.json")
	if err != nil {
		t.Fatalf("read author artifact: %v", err)
	}
	if !bytes.Equal(readBytes, content) {
		t.Fatalf("content mismatch")
	}
	if readMeta.Digest != meta.Digest {
		t.Errorf("digest mismatch: %s vs %s", readMeta.Digest, meta.Digest)
	}
}

func TestWorkerArtifacts_RecordObservedArtifact_RejectsMismatchedAttempt(t *testing.T) {
	store, _, authorRef, _ := setupWorkerArtifactFixture(t, "lease-art-3")
	defer store.Close()
	ctx := context.Background()

	wrongRef := authorRef
	wrongRef.AttemptID = "att_forged_mismatched"

	_, err := store.RecordObservedArtifact(ctx, wrongRef, "draft.md", []byte("some draft"))
	if err == nil {
		t.Fatalf("expected error for mismatched AttemptID, got nil")
	}
	if !errors.Is(err, storage.ErrInvalidAttempt) {
		t.Fatalf("expected ErrInvalidAttempt, got %v", err)
	}

	// Verify nothing was recorded
	var count int
	_ = store.DB().QueryRowContext(ctx, "SELECT count(*) FROM artifact_revisions WHERE artifact_id = 'draft.md';").Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 artifact revisions, got %d", count)
	}
}

func TestWorkerArtifacts_RecordObservedArtifact_IdempotencyAndConflict(t *testing.T) {
	store, _, authorRef, _ := setupWorkerArtifactFixture(t, "lease-art-4")
	defer store.Close()
	ctx := context.Background()

	content := []byte("identical original content")
	meta1, err := store.RecordObservedArtifact(ctx, authorRef, "doc.txt", content)
	if err != nil {
		t.Fatalf("first ingestion failed: %v", err)
	}

	// Duplicate exact same content must return existing metadata idempotently
	meta2, err := store.RecordObservedArtifact(ctx, authorRef, "doc.txt", content)
	if err != nil {
		t.Fatalf("idempotent re-ingestion failed: %v", err)
	}
	if meta1.Digest != meta2.Digest || meta1.Revision != meta2.Revision || meta1.ID != meta2.ID {
		t.Fatalf("idempotent metadata mismatch: %+v vs %+v", meta1, meta2)
	}

	// Conflicting content under same (ref, name) must return ErrConflictingArtifact
	conflictingContent := []byte("different conflicting content")
	_, err = store.RecordObservedArtifact(ctx, authorRef, "doc.txt", conflictingContent)
	if err == nil {
		t.Fatalf("expected ErrConflictingArtifact for conflicting content, got nil")
	}
	if !errors.Is(err, storage.ErrConflictingArtifact) {
		t.Fatalf("expected ErrConflictingArtifact, got %v", err)
	}
}

func TestWorkerArtifacts_ReadAuthorArtifact(t *testing.T) {
	store, _, authorRef, _ := setupWorkerArtifactFixture(t, "lease-art-5")
	defer store.Close()
	ctx := context.Background()

	content := []byte("worker private scratchpad")
	meta, err := store.RecordObservedArtifact(ctx, authorRef, "scratchpad.txt", content)
	if err != nil {
		t.Fatalf("record observed artifact: %v", err)
	}

	// Author session can read its own draft
	readData, readMeta, err := store.ReadAuthorArtifact(ctx, authorRef, "scratchpad.txt")
	if err != nil {
		t.Fatalf("read author artifact: %v", err)
	}
	if !bytes.Equal(readData, content) {
		t.Fatalf("content mismatch: got %q, expected %q", string(readData), string(content))
	}
	if readMeta.Digest != meta.Digest || readMeta.Revision != meta.Revision {
		t.Fatalf("metadata mismatch")
	}

	// Nonexistent artifact returns ErrArtifactNotFound
	_, _, err = store.ReadAuthorArtifact(ctx, authorRef, "nonexistent.txt")
	if !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound, got %v", err)
	}
}

func TestWorkerArtifacts_SiblingSession_IsolationBeforeRelease(t *testing.T) {
	store, runID, authorRef, siblingRef := setupWorkerArtifactFixture(t, "lease-art-6")
	defer store.Close()
	ctx := context.Background()

	content := []byte("confidential unreleased proposal")
	meta, err := store.RecordObservedArtifact(ctx, authorRef, "secret-proposal.md", content)
	if err != nil {
		t.Fatalf("record observed artifact: %v", err)
	}

	// Sibling session attempting to read author draft via ReadAuthorArtifact receives ErrArtifactNotFound
	_, _, err = store.ReadAuthorArtifact(ctx, siblingRef, "secret-proposal.md")
	if !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected sibling ReadAuthorArtifact to fail with ErrArtifactNotFound, got %v", err)
	}

	// Sibling session attempting to read unreleased artifact via ReadReleasedArtifact receives ErrArtifactNotFound
	_, _, err = store.ReadReleasedArtifact(ctx, siblingRef.SessionID, "propset-unreleased", "secret-proposal.md")
	if !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected sibling ReadReleasedArtifact before release to fail with ErrArtifactNotFound, got %v", err)
	}

	// Now simulate release into proposal_sets and proposal_set_members
	propsetDigest := "propset-v1:sha256:abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = store.DB().ExecContext(ctx, `
INSERT INTO proposal_sets (proposal_set_digest, run_id, released_by_op_id, issuing_controller_generation, sealed_at)
VALUES (?, ?, 'op-rel-test', 1, ?);`, propsetDigest, runID, now)
	if err != nil {
		t.Fatalf("insert proposal_sets: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
INSERT INTO proposal_set_members (proposal_set_digest, artifact_id, revision, digest, author_session_id, author_contributor)
VALUES (?, ?, ?, ?, ?, 'claude');`, propsetDigest, meta.ID, meta.Revision, meta.Digest, authorRef.SessionID)
	if err != nil {
		t.Fatalf("insert proposal_set_members: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
UPDATE artifact_revisions
SET released = 1, proposal_set_digest = ?
WHERE artifact_id = ? AND revision = ?;`, propsetDigest, meta.ID, meta.Revision)
	if err != nil {
		t.Fatalf("update artifact_revisions to released: %v", err)
	}

	// Now sibling reviewer CAN read released artifact
	relData, relMeta, err := store.ReadReleasedArtifact(ctx, siblingRef.SessionID, propsetDigest, "secret-proposal.md")
	if err != nil {
		t.Fatalf("read released artifact after release failed: %v", err)
	}
	if !bytes.Equal(relData, content) {
		t.Fatalf("released content mismatch")
	}
	if relMeta.Digest != meta.Digest {
		t.Fatalf("released metadata digest mismatch")
	}

	// Reading with wrong proposalSetDigest fails with ErrArtifactNotFound
	_, _, err = store.ReadReleasedArtifact(ctx, siblingRef.SessionID, "propset-v1:wrong", "secret-proposal.md")
	if !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound on wrong propset digest, got %v", err)
	}
}
