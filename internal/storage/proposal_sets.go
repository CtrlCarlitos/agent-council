package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ProposalMemberRef identifies an artifact revision to include in a sealed proposal set.
type ProposalMemberRef struct {
	ArtifactID string `json:"artifact_id"`
	Revision   int64  `json:"revision"`
	Digest     string `json:"digest"`
}

// ProposalSetReceipt is the committed receipt returned upon successfully sealing a proposal set.
type ProposalSetReceipt struct {
	ProposalSetDigest string    `json:"proposal_set_digest"`
	RunID             string    `json:"run_id"`
	SealedAt          time.Time `json:"sealed_at"`
}

type canonicalMemberRecord struct {
	ArtifactID        string `json:"artifact_id"`
	AuthorContributor string `json:"author_contributor"`
	AuthorSessionID   string `json:"author_session_id"`
	Digest            string `json:"digest"`
	Revision          int64  `json:"revision"`
}

type releaseArtifactsJournalPayload struct {
	CallerLease string             `json:"caller_lease"`
	Receipt     ProposalSetReceipt `json:"receipt"`
}

// ReleaseArtifacts atomically seals a canonical proposal set from verified worker
// artifact revisions under active controller authority.
func (s *Store) ReleaseArtifacts(ctx context.Context, opID string, callerLease string, runID string, members []ProposalMemberRef) (ProposalSetReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return ProposalSetReceipt{}, errors.New("empty operation id")
	}
	if strings.TrimSpace(runID) == "" {
		return ProposalSetReceipt{}, errors.New("empty run id")
	}
	if len(members) == 0 {
		return ProposalSetReceipt{}, errors.New("empty proposal members")
	}

	// Validate members for duplicate or malformed references
	type memberKey struct {
		id  string
		rev int64
	}
	seen := make(map[memberKey]bool, len(members))
	for _, m := range members {
		if strings.TrimSpace(m.ArtifactID) == "" {
			return ProposalSetReceipt{}, errors.New("empty artifact id in member reference")
		}
		if m.Revision <= 0 {
			return ProposalSetReceipt{}, errors.New("invalid revision in member reference")
		}
		if strings.TrimSpace(m.Digest) == "" {
			return ProposalSetReceipt{}, errors.New("empty digest in member reference")
		}
		key := memberKey{id: m.ArtifactID, rev: m.Revision}
		if seen[key] {
			return ProposalSetReceipt{}, errors.New("duplicate proposal member")
		}
		seen[key] = true
	}

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return ProposalSetReceipt{}, err
	}
	defer tx.Rollback()

	// Current controller authority precedes idempotent replay (AC-004 §4.3).
	activeGen, err := classifyRunController(ctx, tx.Tx(), runID, callerLease)
	if err != nil {
		return ProposalSetReceipt{}, err
	}

	// Validate each referenced artifact revision against persisted records
	records := make([]canonicalMemberRecord, 0, len(members))
	for _, m := range members {
		var actualDigest, authorSessionID, authorContributor string
		err := tx.Tx().QueryRowContext(ctx, `
SELECT ar.digest, coalesce(ar.session_id, ''), coalesce(s.contributor, '')
FROM artifact_revisions ar
LEFT JOIN sessions s ON s.session_id = ar.session_id
WHERE ar.artifact_id = ? AND ar.revision = ? AND ar.run_id = ?;`, m.ArtifactID, m.Revision, runID).
			Scan(&actualDigest, &authorSessionID, &authorContributor)
		if err == sql.ErrNoRows {
			return ProposalSetReceipt{}, fmt.Errorf("%w: artifact %s revision %d not found for run %s", ErrArtifactNotFound, m.ArtifactID, m.Revision, runID)
		}
		if err != nil {
			return ProposalSetReceipt{}, fmt.Errorf("query artifact revision %s revision %d: %w", m.ArtifactID, m.Revision, err)
		}

		if actualDigest != m.Digest {
			return ProposalSetReceipt{}, fmt.Errorf("digest mismatch for artifact %s revision %d: expected %s, got %s", m.ArtifactID, m.Revision, m.Digest, actualDigest)
		}
		if authorContributor == "" {
			return ProposalSetReceipt{}, fmt.Errorf("missing author contributor for artifact %s revision %d", m.ArtifactID, m.Revision)
		}

		records = append(records, canonicalMemberRecord{
			ArtifactID:        m.ArtifactID,
			AuthorContributor: authorContributor,
			AuthorSessionID:   authorSessionID,
			Digest:            actualDigest,
			Revision:          m.Revision,
		})
	}

	// Sort canonical member records by (author_contributor, artifact_id, revision)
	sort.Slice(records, func(i, j int) bool {
		if records[i].AuthorContributor != records[j].AuthorContributor {
			return records[i].AuthorContributor < records[j].AuthorContributor
		}
		if records[i].ArtifactID != records[j].ArtifactID {
			return records[i].ArtifactID < records[j].ArtifactID
		}
		return records[i].Revision < records[j].Revision
	})

	// Compute canonical proposal set digest: "propset-v1:sha256:" + hex(sha256(canonical_members_json))
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(records); err != nil {
		return ProposalSetReceipt{}, fmt.Errorf("encode canonical members: %w", err)
	}
	canonicalJSON := bytes.TrimRight(buf.Bytes(), "\n")
	sum := sha256.Sum256(canonicalJSON)
	proposalSetDigest := fmt.Sprintf("propset-v1:sha256:%x", sum)

	fp := computeFingerprint("release_artifacts", runID, proposalSetDigest)

	// Idempotency replay check
	var storedCmdType, storedFingerprint, payloadJSON string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT command_type, command_fingerprint, payload_json
FROM journal_entries
WHERE op_id = ?;`, opID).Scan(&storedCmdType, &storedFingerprint, &payloadJSON)

	if err == nil {
		if storedCmdType != "release_artifacts" || storedFingerprint != fp {
			return ProposalSetReceipt{}, ErrIdempotencyConflict
		}
		var ajp releaseArtifactsJournalPayload
		if err := json.Unmarshal([]byte(payloadJSON), &ajp); err != nil || ajp.Receipt.ProposalSetDigest == "" {
			return ProposalSetReceipt{}, fmt.Errorf("release artifacts operation %s has an unreadable committed receipt", opID)
		}
		return ajp.Receipt, nil
	} else if err != sql.ErrNoRows {
		return ProposalSetReceipt{}, fmt.Errorf("query release journal op: %w", err)
	}

	nowTime := time.Now().UTC()
	nowStr := nowTime.Format(time.RFC3339Nano)

	// Insert proposal_sets
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO proposal_sets (
	proposal_set_digest, run_id, released_by_op_id, issuing_controller_generation, sealed_at
) VALUES (?, ?, ?, ?, ?);`, proposalSetDigest, runID, opID, int64(activeGen), nowStr)
	if err != nil {
		return ProposalSetReceipt{}, fmt.Errorf("insert proposal_sets: %w", err)
	}

	// Insert proposal_set_members
	for _, rec := range records {
		_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO proposal_set_members (
	proposal_set_digest, artifact_id, revision, digest, author_session_id, author_contributor
) VALUES (?, ?, ?, ?, ?, ?);`, proposalSetDigest, rec.ArtifactID, rec.Revision, rec.Digest, rec.AuthorSessionID, rec.AuthorContributor)
		if err != nil {
			return ProposalSetReceipt{}, fmt.Errorf("insert proposal_set_members: %w", err)
		}
	}

	// Update artifact_revisions: SET released = 1, proposal_set_digest = ?
	for _, rec := range records {
		_, err = tx.Tx().ExecContext(ctx, `
UPDATE artifact_revisions
SET released = 1, proposal_set_digest = ?
WHERE artifact_id = ? AND revision = ? AND run_id = ?;`, proposalSetDigest, rec.ArtifactID, rec.Revision, runID)
		if err != nil {
			return ProposalSetReceipt{}, fmt.Errorf("update artifact_revisions released: %w", err)
		}
	}

	receipt := ProposalSetReceipt{
		ProposalSetDigest: proposalSetDigest,
		RunID:             runID,
		SealedAt:          nowTime,
	}

	ajp := releaseArtifactsJournalPayload{
		CallerLease: callerLease,
		Receipt:     receipt,
	}
	ajpBytes, err := json.Marshal(ajp)
	if err != nil {
		return ProposalSetReceipt{}, fmt.Errorf("marshal release artifacts journal payload: %w", err)
	}

	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO journal_entries (
	op_id, command_type, command_fingerprint, run_id, session_id, turn_key, event_kind, payload_version, payload_json, created_at
) VALUES (?, 'release_artifacts', ?, ?, NULL, NULL, 'artifacts_released', 1, ?, ?);`,
		opID, fp, runID, string(ajpBytes), nowStr)
	if err != nil {
		return ProposalSetReceipt{}, fmt.Errorf("insert journal entry artifacts_released: %w", err)
	}

	if s.testHookBeforeCommit != nil {
		s.testHookBeforeCommit("uncommitted_proposal_set")
	}

	if err := tx.Commit(); err != nil {
		return ProposalSetReceipt{}, err
	}

	return receipt, nil
}
