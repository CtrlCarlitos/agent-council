package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type ArtifactMetadata struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id"`
	TurnKey   string    `json:"turn_key"`
	Name      string    `json:"name"`
	Digest    string    `json:"digest"`
	ByteCount int64     `json:"byte_count"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
}

type artifactJournalPayload struct {
	CallerLease   string           `json:"caller_lease"`
	Receipt       OperationReceipt `json:"receipt"`
	CommittedMeta ArtifactMetadata `json:"committed_meta"`
}

const maxArtifactSize = 100 * 1024 * 1024 // 100MB

var artifactMu sync.Mutex

func isValidDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (s *Store) PublishArtifact(ctx context.Context, opID string, callerLease string, meta ArtifactMetadata, content []byte) (ArtifactMetadata, error) {
	// 1. Path traversal guard on artifact name
	if strings.Contains(meta.Name, "/") || strings.Contains(meta.Name, "\\") || strings.Contains(meta.Name, "..") {
		return ArtifactMetadata{}, ErrInvalidPath
	}

	// 2. Enforce pre-write content size bound
	if int64(len(content)) > maxArtifactSize {
		return ArtifactMetadata{}, ErrArtifactOversized
	}

	// 3. Pre-authorization: classify caller authority before any file work
	// (AC-004; administrator class — the current credential on unadopted
	// runs remains the operator's maintenance handle).
	if _, err := classifyCredential(ctx, s.readDB, meta.RunID, callerLease, false); err != nil {
		return ArtifactMetadata{}, err
	}

	// 4. Pre-write sanitization of content
	sanitizedContent := []byte(SanitizeText(string(content)))

	// 5. Compute digest and byte count
	sum := sha256.Sum256(sanitizedContent)
	digest := hex.EncodeToString(sum[:])
	byteCount := int64(len(sanitizedContent))

	fp := computeFingerprint("publish_artifact", meta.ID, meta.RunID, meta.SessionID, meta.TurnKey, meta.Name, digest, fmt.Sprintf("%d", byteCount))

	// Check idempotency first before file creation
	var storedCmdType, storedFingerprint, payloadJSON string
	err := s.readDB.QueryRowContext(ctx, "SELECT command_type, command_fingerprint, payload_json FROM journal_entries WHERE op_id = ?;", opID).Scan(&storedCmdType, &storedFingerprint, &payloadJSON)
	if err == nil {
		var ajp artifactJournalPayload
		if err := json.Unmarshal([]byte(payloadJSON), &ajp); err == nil && ajp.CommittedMeta.ID != "" {
			// Authority was classified before this lookup; the recorded
			// caller_lease may belong to a superseded controller.
			if storedCmdType != "publish_artifact" || storedFingerprint != fp {
				return ArtifactMetadata{}, ErrIdempotencyConflict
			}
			return ajp.CommittedMeta, nil
		}
	}

	// 6. Filesystem staging and atomic no-clobber installation
	if err := s.writeCASBlob(sanitizedContent, digest); err != nil {
		return ArtifactMetadata{}, err
	}

	// 7. Record metadata in relational store inside write transaction
	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return ArtifactMetadata{}, err
	}
	defer tx.Rollback()

	// Recheck idempotency inside write lock
	var storedCmdType2, storedFingerprint2, payloadJSON2 string
	err = tx.Tx().QueryRowContext(ctx, "SELECT command_type, command_fingerprint, payload_json FROM journal_entries WHERE op_id = ?;", opID).Scan(&storedCmdType2, &storedFingerprint2, &payloadJSON2)
	if err == nil {
		var ajp artifactJournalPayload
		if err := json.Unmarshal([]byte(payloadJSON2), &ajp); err == nil && ajp.CommittedMeta.ID != "" {
			if ajp.CallerLease != callerLease {
				return ArtifactMetadata{}, ErrUnauthorizedOperation
			}
			if storedCmdType2 != "publish_artifact" || storedFingerprint2 != fp {
				return ArtifactMetadata{}, ErrIdempotencyConflict
			}
			return ajp.CommittedMeta, nil
		}
	}

	var maxRev int64
	_ = tx.Tx().QueryRowContext(ctx, "SELECT coalesce(max(revision), 0) FROM artifact_revisions WHERE artifact_id = ?;", meta.ID).Scan(&maxRev)
	newRev := maxRev + 1

	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339Nano)

	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO artifact_revisions (artifact_id, revision, run_id, kind, digest, byte_size, created_at)
VALUES (?, ?, ?, 'patch', ?, ?, ?);`, meta.ID, newRev, meta.RunID, digest, byteCount, now)
	if err != nil {
		return ArtifactMetadata{}, fmt.Errorf("insert artifact revision: %w", err)
	}

	committedMeta := meta
	committedMeta.Digest = digest
	committedMeta.ByteCount = byteCount
	committedMeta.Revision = newRev
	committedMeta.CreatedAt = nowTime

	opReceipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "publish_artifact",
		SessionID:        meta.SessionID,
		TurnKey:          meta.TurnKey,
		CommittedVersion: newRev,
		CreatedAt:        nowTime,
		Payload:          digest,
	}

	ajp := artifactJournalPayload{
		CallerLease:   callerLease,
		Receipt:       opReceipt,
		CommittedMeta: committedMeta,
	}
	ajpBytes, err := json.Marshal(ajp)
	if err != nil {
		return ArtifactMetadata{}, fmt.Errorf("marshal artifact journal payload: %w", err)
	}

	var sessIDVal any = meta.SessionID
	if meta.SessionID == "" {
		sessIDVal = nil
	}
	var turnKeyVal any = meta.TurnKey
	if meta.TurnKey == "" {
		turnKeyVal = nil
	}

	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO journal_entries (op_id, command_type, command_fingerprint, run_id, session_id, turn_key, event_kind, payload_version, payload_json, created_at)
VALUES (?, 'publish_artifact', ?, ?, ?, ?, 'artifact_published', 1, ?, ?);`, opID, fp, meta.RunID, sessIDVal, turnKeyVal, string(ajpBytes), now)
	if err != nil {
		return ArtifactMetadata{}, fmt.Errorf("record artifact journal entry: %w", err)
	}

	if s.testHookBeforeCommit != nil {
		s.testHookBeforeCommit("uncommitted_artifact_metadata")
	}

	if err := tx.Commit(); err != nil {
		return ArtifactMetadata{}, err
	}
	return committedMeta, nil
}

func (s *Store) ReadArtifact(digest string) ([]byte, error) {
	if !isValidDigest(digest) {
		return nil, ErrArtifactCorrupt
	}

	blobPath := filepath.Join(s.stateDir, "artifacts", digest[:2], digest)
	artifactsDir := filepath.Join(s.stateDir, "artifacts")
	rel, err := filepath.Rel(artifactsDir, blobPath)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return nil, fmt.Errorf("%w: artifact path outside artifacts directory", ErrArtifactCorrupt)
	}

	if err := ensureNoSymlink(blobPath); err != nil {
		return nil, fmt.Errorf("%w: artifact symlink forbidden: %w", ErrArtifactCorrupt, err)
	}

	f, err := os.Open(blobPath)
	if os.IsNotExist(err) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() > maxArtifactSize {
		return nil, ErrArtifactCorrupt
	}

	lr := io.LimitReader(f, maxArtifactSize+1)
	var buf bytes.Buffer
	h := sha256.New()
	w := io.MultiWriter(&buf, h)

	n, err := io.Copy(w, lr)
	if err != nil || n > maxArtifactSize || n != info.Size() {
		return nil, ErrArtifactCorrupt
	}

	actualDigest := hex.EncodeToString(h.Sum(nil))
	if actualDigest != digest {
		return nil, ErrArtifactCorrupt
	}

	return buf.Bytes(), nil
}

func (s *Store) ReadArtifactRevision(ctx context.Context, artifactID string, revision int64) (ArtifactMetadata, []byte, error) {
	if strings.TrimSpace(artifactID) == "" || revision <= 0 {
		return ArtifactMetadata{}, nil, errors.New("invalid artifact id or revision")
	}

	var meta ArtifactMetadata
	var createdAtStr string
	err := s.readDB.QueryRowContext(ctx, `
SELECT artifact_id, revision, run_id, kind, digest, byte_size, created_at
FROM artifact_revisions
WHERE artifact_id = ? AND revision = ?;`, artifactID, revision).Scan(&meta.ID, &meta.Revision, &meta.RunID, &meta.Name, &meta.Digest, &meta.ByteCount, &createdAtStr)
	if err == sql.ErrNoRows {
		return ArtifactMetadata{}, nil, ErrArtifactNotFound
	}
	if err != nil {
		return ArtifactMetadata{}, nil, fmt.Errorf("query artifact revision: %w", err)
	}
	meta.CreatedAt = parseTime(createdAtStr)

	data, err := s.ReadArtifact(meta.Digest)
	if err != nil {
		return ArtifactMetadata{}, nil, err
	}
	if int64(len(data)) != meta.ByteCount {
		return ArtifactMetadata{}, nil, ErrArtifactCorrupt
	}

	return meta, data, nil
}

// writeCASBlob atomically installs sanitizedContent under artifacts/<digest[:2]>/<digest>
// via temporary staging and os.Link, guaranteeing no-clobber semantics.
func (s *Store) writeCASBlob(sanitizedContent []byte, digest string) error {
	artifactMu.Lock()
	defer artifactMu.Unlock()

	artifactsDir := filepath.Join(s.stateDir, "artifacts")
	if err := ensureNoSymlink(artifactsDir); err != nil {
		return err
	}
	destDir := filepath.Join(artifactsDir, digest[:2])
	if err := ensureNoSymlink(destDir); err != nil {
		return err
	}
	destPath := filepath.Join(destDir, digest)
	if err := ensureNoSymlink(destPath); err != nil {
		return err
	}

	tmpDir := filepath.Join(artifactsDir, "tmp")
	if err := ensureNoSymlink(tmpDir); err != nil {
		return err
	}
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}

	// Staging file
	tmpFile, err := os.CreateTemp(tmpDir, "blob-*")
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	tmpName := tmpFile.Name()

	n, writeErr := tmpFile.Write(sanitizedContent)
	if writeErr == nil && n != len(sanitizedContent) {
		writeErr = io.ErrShortWrite
	}
	syncErr := tmpFile.Sync()
	closeErr := tmpFile.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		if writeErr != nil {
			return fmt.Errorf("write staging file: %w", writeErr)
		}
		if syncErr != nil {
			return fmt.Errorf("sync staging file: %w", syncErr)
		}
		return fmt.Errorf("close staging file: %w", closeErr)
	}

	if err := os.Chmod(tmpName, 0600); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod staging file: %w", err)
	}

	// Cross-process no-clobber install via os.Link. Staged content was fully written and synced.
	linkErr := os.Link(tmpName, destPath)
	if linkErr == nil {
		_ = os.Remove(tmpName)
	} else if os.IsExist(linkErr) {
		_ = os.Remove(tmpName)
		// Verify existing destination: size and content digest; if corrupt, fail without repair
		existingBytes, err := s.ReadArtifact(digest)
		if err != nil || !bytes.Equal(existingBytes, sanitizedContent) {
			return ErrArtifactCorrupt
		}
	} else {
		_ = os.Remove(tmpName)
		return fmt.Errorf("install artifact file: %w", linkErr)
	}

	// Sync parent directory before metadata commit on supported POSIX filesystems.
	if runtime.GOOS != "windows" {
		d, err := os.Open(destDir)
		if err != nil {
			return fmt.Errorf("open dest dir for sync: %w", err)
		}
		dSyncErr := d.Sync()
		dCloseErr := d.Close()
		if dSyncErr != nil || dCloseErr != nil {
			if dSyncErr != nil {
				return fmt.Errorf("sync dest dir: %w", dSyncErr)
			}
			return fmt.Errorf("close dest dir: %w", dCloseErr)
		}
	}

	return nil
}
