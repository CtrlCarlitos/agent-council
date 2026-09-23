package storage

// AC-008 durable protection-attestation records (spec §3.6): the row is
// created only by an operator-authorized journal operation. The
// operation is idempotent by op_id with receipt replay, and the
// attestation identity (cprot-v1 digest) is unique — a re-recording of
// the same evidence under a new op_id is rejected, never silently
// duplicated.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ClaudeProtectionAttestationRecord is the durable form of a cprot-v1
// protection attestation. ProbeResults carries the canonical framed
// record list produced by the cprot-v1 encoder — never free-form JSON.
type ClaudeProtectionAttestationRecord struct {
	AttestationID  string // cprot-v1:sha256:<hex> over the canonical frame
	RunID          string // the run the probe was performed for (journal provenance)
	ClaudeVersion  string
	Platform       string
	ManifestDigest string
	TemplateDigest string
	ProbeResults   string // canonical cprot-v1 framed record list
	ProbedAt       string // RFC3339 UTC
	Actor          string // operator identity (journal-linked)
}

// FindClaudeProtectionAttestation returns the attestation id matching
// the four binding fields in force (probed CLI version, platform,
// manifest digest, template digest), or "" when none matches. This is
// the lookup Dispatch uses to freeze protection on new attempts.
func (s *Store) FindClaudeProtectionAttestation(ctx context.Context, claudeVersion, platform, manifestDigest, templateDigest string) (string, error) {
	var id string
	err := s.DB().QueryRowContext(ctx, `
SELECT attestation_id FROM claude_protection_attestations
WHERE claude_version = ? AND platform = ? AND manifest_digest = ? AND template_digest = ?
ORDER BY probed_at DESC LIMIT 1`,
		claudeVersion, platform, manifestDigest, templateDigest).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query claude protection attestation: %w", err)
	}
	return id, nil
}

// RecordClaudeProtectionAttestation persists the attestation row and
// its journal entry in one transaction. Idempotent by op_id: replaying
// the same operation returns the committed receipt; the same op_id with
// different evidence is an idempotency conflict; the same evidence
// under a new op_id is rejected (the row already exists).
func (s *Store) RecordClaudeProtectionAttestation(ctx context.Context, opID string, rec ClaudeProtectionAttestationRecord) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return OperationReceipt{}, errors.New("attestation operation requires an op_id")
	}
	if strings.TrimSpace(rec.RunID) == "" {
		return OperationReceipt{}, errors.New("attestation operation requires the run provenance")
	}
	if !strings.HasPrefix(rec.AttestationID, "cprot-v1:sha256:") {
		return OperationReceipt{}, fmt.Errorf("attestation id %q is not a cprot-v1 digest", rec.AttestationID)
	}
	for name, value := range map[string]string{
		"claude_version":  rec.ClaudeVersion,
		"platform":        rec.Platform,
		"manifest_digest": rec.ManifestDigest,
		"template_digest": rec.TemplateDigest,
		"probe_results":   rec.ProbeResults,
		"probed_at":       rec.ProbedAt,
		"actor":           rec.Actor,
	} {
		if strings.TrimSpace(value) == "" {
			return OperationReceipt{}, fmt.Errorf("attestation %s is required", name)
		}
	}

	fp := computeFingerprint("record_claude_attestation", rec.AttestationID, rec.RunID, rec.ClaudeVersion,
		rec.Platform, rec.ManifestDigest, rec.TemplateDigest, rec.ProbeResults, rec.ProbedAt, rec.Actor)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, rec.Actor, "record_claude_attestation", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	now := time.Now().UTC()
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "record_claude_attestation",
		CommittedVersion: 1,
		CreatedAt:        now,
		Payload:          rec.AttestationID,
	}
	if err := recordJournalEntry(tx.Tx(), opID, "record_claude_attestation", fp, rec.RunID, "", "",
		"claude_attestation", receipt, rec.Actor); err != nil {
		return OperationReceipt{}, err
	}

	// Duplicate evidence identity: the same attestation under a new
	// op_id is rejected rather than silently duplicated.
	var existingID string
	if err := tx.Tx().QueryRowContext(ctx,
		`SELECT attestation_id FROM claude_protection_attestations WHERE attestation_id = ?`,
		rec.AttestationID).Scan(&existingID); err == nil {
		return OperationReceipt{}, fmt.Errorf("attestation %s is already recorded", rec.AttestationID)
	}

	res, err := tx.Tx().ExecContext(ctx, `
INSERT INTO claude_protection_attestations
	(attestation_id, claude_version, platform, manifest_digest, template_digest,
	 probe_results, probed_at, actor)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.AttestationID, rec.ClaudeVersion, rec.Platform, rec.ManifestDigest,
		rec.TemplateDigest, rec.ProbeResults, rec.ProbedAt, rec.Actor)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert claude protection attestation: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return OperationReceipt{}, fmt.Errorf("attestation %s is already recorded", rec.AttestationID)
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
