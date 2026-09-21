package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RecordObservedArtifact records an artifact revision produced by an accepted
// worker turn execution. It validates that ref matches the persisted turn
// and dispatch attempt inside the write transaction, tolerates active or
// terminal turn lifecycles, guarantees idempotency on matching content,
// rejects conflicting content under the same (ref, name), and keeps the
// revision unreleased.
func (s *Store) RecordObservedArtifact(ctx context.Context, ref ExecutionRef, name string, content []byte) (ArtifactMetadata, error) {
	if err := ref.validate(); err != nil {
		return ArtifactMetadata{}, fmt.Errorf("%w: %w", ErrInvalidAttempt, err)
	}
	if strings.TrimSpace(name) == "" {
		return ArtifactMetadata{}, errors.New("empty artifact name")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return ArtifactMetadata{}, ErrInvalidPath
	}
	if int64(len(content)) > maxArtifactSize {
		return ArtifactMetadata{}, ErrArtifactOversized
	}

	sanitizedContent := []byte(SanitizeText(string(content)))
	sum := sha256.Sum256(sanitizedContent)
	digest := hex.EncodeToString(sum[:])
	byteCount := int64(len(sanitizedContent))

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return ArtifactMetadata{}, err
	}
	defer tx.Rollback()

	var runID, lifecycle string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT run_id, lifecycle
FROM sessions
WHERE session_id = ?;`, ref.SessionID).Scan(&runID, &lifecycle)
	if err == sql.ErrNoRows {
		return ArtifactMetadata{}, fmt.Errorf("%w: session %q not found", ErrInvalidAttempt, ref.SessionID)
	}
	if err != nil {
		return ArtifactMetadata{}, fmt.Errorf("query session: %w", err)
	}
	if lifecycle == "archived" {
		return ArtifactMetadata{}, ErrSessionArchived
	}

	var turnStatus, turnAttemptID string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT status, attempt_id
FROM turns
WHERE session_id = ? AND turn_key = ?;`, ref.SessionID, ref.TurnKey).Scan(&turnStatus, &turnAttemptID)
	if err == sql.ErrNoRows {
		return ArtifactMetadata{}, fmt.Errorf("%w: turn %s/%s not found", ErrInvalidAttempt, ref.SessionID, ref.TurnKey)
	}
	if err != nil {
		return ArtifactMetadata{}, fmt.Errorf("query turn: %w", err)
	}
	if turnAttemptID != ref.AttemptID {
		return ArtifactMetadata{}, fmt.Errorf("%w: turn attempt mismatch: presented %q, persisted %q", ErrInvalidAttempt, ref.AttemptID, turnAttemptID)
	}

	switch turnStatus {
	case "running", "completed", "failed", "cancelling", "cancelled", "interrupted":
		// Lifecycle tolerant: valid for active execution or terminal outcome.
	default:
		return ArtifactMetadata{}, fmt.Errorf("%w: turn status %q not eligible for artifact ingestion", ErrInvalidAttempt, turnStatus)
	}

	var dispatchAttemptID string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT attempt_id
FROM dispatch_intents
WHERE session_id = ? AND turn_key = ?;`, ref.SessionID, ref.TurnKey).Scan(&dispatchAttemptID)
	if err == sql.ErrNoRows {
		return ArtifactMetadata{}, fmt.Errorf("%w: dispatch intent %s/%s not found", ErrInvalidAttempt, ref.SessionID, ref.TurnKey)
	}
	if err != nil {
		return ArtifactMetadata{}, fmt.Errorf("query dispatch intent: %w", err)
	}
	if dispatchAttemptID != ref.AttemptID {
		return ArtifactMetadata{}, fmt.Errorf("%w: dispatch attempt mismatch: presented %q, persisted %q", ErrInvalidAttempt, ref.AttemptID, dispatchAttemptID)
	}

	// Check existing artifact revision for (session_id, attempt_id, artifact_id)
	var existingRev int64
	var existingDigest string
	var existingByteSize int64
	var existingCreatedAt string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT revision, digest, byte_size, created_at
FROM artifact_revisions
WHERE session_id = ? AND attempt_id = ? AND artifact_id = ?;`, ref.SessionID, ref.AttemptID, name).
		Scan(&existingRev, &existingDigest, &existingByteSize, &existingCreatedAt)

	if err == nil {
		// Existing record found.
		if existingDigest == digest {
			// Idempotent duplicate: return existing metadata.
			_ = tx.Rollback()
			return ArtifactMetadata{
				ID:        name,
				RunID:     runID,
				SessionID: ref.SessionID,
				TurnKey:   ref.TurnKey,
				Name:      name,
				Digest:    existingDigest,
				ByteCount: existingByteSize,
				Revision:  existingRev,
				CreatedAt: parseTime(existingCreatedAt),
			}, nil
		}
		// Conflicting content under same (ref, name).
		_ = tx.Rollback()
		return ArtifactMetadata{}, fmt.Errorf("%w: conflicting content under %s/%s for artifact %s", ErrConflictingArtifact, ref.SessionID, ref.TurnKey, name)
	} else if err != sql.ErrNoRows {
		return ArtifactMetadata{}, fmt.Errorf("query existing artifact: %w", err)
	}

	// Install CAS blob
	if err := s.writeCASBlob(sanitizedContent, digest); err != nil {
		return ArtifactMetadata{}, err
	}

	var maxRev int64
	_ = tx.Tx().QueryRowContext(ctx, "SELECT coalesce(max(revision), 0) FROM artifact_revisions WHERE artifact_id = ?;", name).Scan(&maxRev)
	newRev := maxRev + 1

	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339Nano)

	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO artifact_revisions (
	artifact_id, revision, run_id, kind, digest, byte_size, created_at, session_id, attempt_id, released, proposal_set_digest
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, NULL);`,
		name, newRev, runID, name, digest, byteCount, now, ref.SessionID, ref.AttemptID)
	if err != nil {
		return ArtifactMetadata{}, fmt.Errorf("insert worker artifact revision: %w", err)
	}

	if s.testHookBeforeCommit != nil {
		s.testHookBeforeCommit("uncommitted_worker_artifact")
	}

	if err := tx.Commit(); err != nil {
		return ArtifactMetadata{}, err
	}

	return ArtifactMetadata{
		ID:        name,
		RunID:     runID,
		SessionID: ref.SessionID,
		TurnKey:   ref.TurnKey,
		Name:      name,
		Digest:    digest,
		ByteCount: byteCount,
		Revision:  newRev,
		CreatedAt: nowTime,
	}, nil
}

// ReadAuthorArtifact allows an author session to read its own drafts or authored
// revisions by verifying artifact_revisions.session_id == ref.SessionID.
func (s *Store) ReadAuthorArtifact(ctx context.Context, ref ExecutionRef, artifactID string) ([]byte, ArtifactMetadata, error) {
	if err := ref.validate(); err != nil {
		return nil, ArtifactMetadata{}, err
	}
	if strings.TrimSpace(artifactID) == "" {
		return nil, ArtifactMetadata{}, errors.New("empty artifact id")
	}

	var meta ArtifactMetadata
	var createdAtStr, sessID, attemptID, turnKey string
	err := s.readDB.QueryRowContext(ctx, `
SELECT ar.artifact_id, ar.revision, ar.run_id, ar.kind, ar.digest, ar.byte_size, ar.created_at,
       coalesce(ar.session_id, ''), coalesce(ar.attempt_id, ''), coalesce(t.turn_key, '')
FROM artifact_revisions ar
LEFT JOIN turns t ON t.session_id = ar.session_id AND t.attempt_id = ar.attempt_id
WHERE ar.artifact_id = ? AND ar.session_id = ?
ORDER BY ar.revision DESC
LIMIT 1;`, artifactID, ref.SessionID).Scan(
		&meta.ID, &meta.Revision, &meta.RunID, &meta.Name, &meta.Digest, &meta.ByteCount,
		&createdAtStr, &sessID, &attemptID, &turnKey,
	)
	if err == sql.ErrNoRows {
		return nil, ArtifactMetadata{}, ErrArtifactNotFound
	}
	if err != nil {
		return nil, ArtifactMetadata{}, fmt.Errorf("query author artifact: %w", err)
	}

	meta.CreatedAt = parseTime(createdAtStr)
	meta.SessionID = sessID
	meta.TurnKey = turnKey

	data, err := s.ReadArtifact(meta.Digest)
	if err != nil {
		return nil, ArtifactMetadata{}, err
	}
	if int64(len(data)) != meta.ByteCount {
		return nil, ArtifactMetadata{}, ErrArtifactCorrupt
	}

	return data, meta, nil
}

// ReadReleasedArtifact allows peer reviewers to read released proposal set members,
// verifying released = 1 and membership in proposal_set_members for proposalSetDigest.
func (s *Store) ReadReleasedArtifact(ctx context.Context, reviewerSessionID string, proposalSetDigest string, artifactID string) ([]byte, ArtifactMetadata, error) {
	if strings.TrimSpace(reviewerSessionID) == "" {
		return nil, ArtifactMetadata{}, errors.New("empty reviewer session id")
	}
	if strings.TrimSpace(proposalSetDigest) == "" {
		return nil, ArtifactMetadata{}, errors.New("empty proposal set digest")
	}
	if strings.TrimSpace(artifactID) == "" {
		return nil, ArtifactMetadata{}, errors.New("empty artifact id")
	}

	var revRunID string
	err := s.readDB.QueryRowContext(ctx, `SELECT run_id FROM sessions WHERE session_id = ?;`, reviewerSessionID).Scan(&revRunID)
	if err == sql.ErrNoRows {
		return nil, ArtifactMetadata{}, ErrSessionNotFound
	}
	if err != nil {
		return nil, ArtifactMetadata{}, fmt.Errorf("query reviewer session: %w", err)
	}

	var meta ArtifactMetadata
	var createdAtStr, sessID, attemptID, turnKey string
	err = s.readDB.QueryRowContext(ctx, `
SELECT ar.artifact_id, ar.revision, ar.run_id, ar.kind, ar.digest, ar.byte_size, ar.created_at,
       coalesce(ar.session_id, ''), coalesce(ar.attempt_id, ''), coalesce(t.turn_key, '')
FROM artifact_revisions ar
JOIN proposal_set_members psm ON psm.artifact_id = ar.artifact_id AND psm.revision = ar.revision
JOIN proposal_sets ps ON ps.proposal_set_digest = psm.proposal_set_digest
LEFT JOIN turns t ON t.session_id = ar.session_id AND t.attempt_id = ar.attempt_id
WHERE psm.proposal_set_digest = ?
  AND ar.artifact_id = ?
  AND ar.released = 1
  AND ar.run_id = ?
  AND ps.run_id = ?
ORDER BY ar.revision DESC
LIMIT 1;`, proposalSetDigest, artifactID, revRunID, revRunID).Scan(
		&meta.ID, &meta.Revision, &meta.RunID, &meta.Name, &meta.Digest, &meta.ByteCount,
		&createdAtStr, &sessID, &attemptID, &turnKey,
	)
	if err == sql.ErrNoRows {
		return nil, ArtifactMetadata{}, ErrArtifactNotFound
	}
	if err != nil {
		return nil, ArtifactMetadata{}, fmt.Errorf("query released artifact: %w", err)
	}

	meta.CreatedAt = parseTime(createdAtStr)
	meta.SessionID = sessID
	meta.TurnKey = turnKey

	data, err := s.ReadArtifact(meta.Digest)
	if err != nil {
		return nil, ArtifactMetadata{}, err
	}
	if int64(len(data)) != meta.ByteCount {
		return nil, ArtifactMetadata{}, ErrArtifactCorrupt
	}

	return data, meta, nil
}
