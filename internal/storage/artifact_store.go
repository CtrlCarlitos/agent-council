package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

	// 2. Compute digest and byte count
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	byteCount := int64(len(content))

	// 3. Filesystem staging and atomic no-clobber installation
	artifactMu.Lock()
	defer artifactMu.Unlock()

	artifactsDir := filepath.Join(s.stateDir, "artifacts")
	destDir := filepath.Join(artifactsDir, digest[:2])
	destPath := filepath.Join(destDir, digest)

	if info, err := os.Stat(destPath); err == nil {
		// Existing destination: must verify digest and size; if corrupt, fail immediately without repairing
		if info.Size() != byteCount {
			return ArtifactMetadata{}, ErrArtifactCorrupt
		}
		existingBytes, err := s.ReadArtifact(digest)
		if err != nil || !bytes.Equal(existingBytes, content) {
			return ArtifactMetadata{}, ErrArtifactCorrupt
		}
	} else if os.IsNotExist(err) {
		tmpDir := filepath.Join(artifactsDir, "tmp")
		if err := os.MkdirAll(tmpDir, 0700); err != nil {
			return ArtifactMetadata{}, fmt.Errorf("create tmp dir: %w", err)
		}
		if err := os.MkdirAll(destDir, 0700); err != nil {
			return ArtifactMetadata{}, fmt.Errorf("create dest dir: %w", err)
		}

		tmpFile, err := os.CreateTemp(tmpDir, "blob-*")
		if err != nil {
			return ArtifactMetadata{}, fmt.Errorf("create staging file: %w", err)
		}
		tmpName := tmpFile.Name()

		if _, err := tmpFile.Write(content); err != nil {
			tmpFile.Close()
			os.Remove(tmpName)
			return ArtifactMetadata{}, fmt.Errorf("write staging file: %w", err)
		}
		if err := tmpFile.Sync(); err != nil {
			tmpFile.Close()
			os.Remove(tmpName)
			return ArtifactMetadata{}, fmt.Errorf("sync staging file: %w", err)
		}
		tmpFile.Close()

		if err := os.Chmod(tmpName, 0600); err != nil {
			os.Remove(tmpName)
			return ArtifactMetadata{}, fmt.Errorf("chmod staging file: %w", err)
		}

		if err := os.Rename(tmpName, destPath); err != nil {
			os.Remove(tmpName)
			return ArtifactMetadata{}, fmt.Errorf("install artifact file: %w", err)
		}
	} else {
		return ArtifactMetadata{}, fmt.Errorf("stat artifact destination: %w", err)
	}

	// 4. Record metadata in relational store inside write transaction
	fp := computeFingerprint("publish_artifact", meta.ID, meta.RunID, meta.SessionID, meta.TurnKey, meta.Name, digest, fmt.Sprintf("%d", byteCount))

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return ArtifactMetadata{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "publish_artifact", fp); err != nil {
		return ArtifactMetadata{}, err
	} else if receipt != nil {
		return meta, nil
	}

	var runLease string
	err = tx.Tx().QueryRowContext(ctx, "SELECT controller_lease FROM runs WHERE run_id = ?;", meta.RunID).Scan(&runLease)
	if err != nil || runLease != callerLease {
		return ArtifactMetadata{}, ErrUnauthorizedOperation
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

	if err := recordJournalEntry(tx.Tx(), opID, "publish_artifact", fp, meta.RunID, meta.SessionID, meta.TurnKey, "artifact_published", opReceipt, callerLease); err != nil {
		return ArtifactMetadata{}, err
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
