package storage

// AC-010 durable cprot-v2 protection-attestation records and the
// creation-uncertainty EPISODE journal for the Agy adapter — copied
// structurally from the AC-009 Codex adapter (codex_attestation.go):
// the attestation row is created only by an operator-authorized journal
// operation — idempotent by op_id with receipt replay, the attestation
// identity (cprot-v2 digest) unique — and an uncertain SESSION creation
// is recorded durably as a numbered episode so a service restart can
// never silently re-create a native identity for it (automatic
// recreation blocked until a controller-authorized resolution of that
// exact episode).
//
// The attestation table exists here for readiness probing only: unlike
// Codex, Agy turn attempts have no redispatch branch, so nothing in
// this adapter gates a second launch on a frozen attestation id.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AgyProtectionAttestationRecord is the durable form of a cprot-v2
// protection attestation for the Agy adapter. ProbeResults carries the
// canonical framed record list produced by the cprot-v2 encoder — never
// free-form JSON. The run provenance lives on the journal entry, not
// the row (the agy_protection_attestations table has no run column).
type AgyProtectionAttestationRecord struct {
	AttestationID  string // cprot-v2:sha256:<hex> over the canonical frame
	RunID          string // the run the probe was performed for (journal provenance)
	AgyVersion     string
	Platform       string // canonical os + "/" + family identity
	ManifestDigest string // sha256:<hex> — the ComputeToolkitManifestDigest value
	ProfileDigest  string
	ProbeResults   string // canonical cprot-v2 framed record list
	ProbedAt       string // RFC3339 UTC
	Actor          string // operator identity (journal-linked)
}

// RecordAgyProtectionAttestation persists the attestation row and its
// journal entry in one transaction. Idempotent by op_id: replaying the
// same operation returns the committed receipt; the same op_id with
// different evidence is an idempotency conflict; the same evidence
// under a new op_id is rejected (the row already exists).
func (s *Store) RecordAgyProtectionAttestation(ctx context.Context, opID string, rec AgyProtectionAttestationRecord) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return OperationReceipt{}, errors.New("attestation operation requires an op_id")
	}
	if strings.TrimSpace(rec.RunID) == "" {
		return OperationReceipt{}, errors.New("attestation operation requires the run provenance")
	}
	// The durable table CHECK enforces exactly this shape; validate
	// before the transaction so a malformed id fails with a readable
	// error instead of a constraint violation.
	if len(rec.AttestationID) != 80 || !strings.HasPrefix(rec.AttestationID, "cprot-v2:sha256:") {
		return OperationReceipt{}, fmt.Errorf("attestation id %q is not a cprot-v2 digest", rec.AttestationID)
	}
	for name, value := range map[string]string{
		"agy_version":     rec.AgyVersion,
		"platform":        rec.Platform,
		"manifest_digest": rec.ManifestDigest,
		"profile_digest":  rec.ProfileDigest,
		"probe_results":   rec.ProbeResults,
		"probed_at":       rec.ProbedAt,
		"actor":           rec.Actor,
	} {
		if strings.TrimSpace(value) == "" {
			return OperationReceipt{}, fmt.Errorf("attestation %s is required", name)
		}
	}

	fp := computeFingerprint("record_agy_attestation", rec.AttestationID, rec.RunID, rec.AgyVersion,
		rec.Platform, rec.ManifestDigest, rec.ProfileDigest, rec.ProbeResults, rec.ProbedAt, rec.Actor)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, rec.Actor, "record_agy_attestation", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	now := time.Now().UTC()
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "record_agy_attestation",
		CommittedVersion: 1,
		CreatedAt:        now,
		Payload:          rec.AttestationID,
	}
	if err := recordJournalEntry(tx.Tx(), opID, "record_agy_attestation", fp, rec.RunID, "", "",
		"agy_attestation", receipt, rec.Actor); err != nil {
		return OperationReceipt{}, err
	}

	// Duplicate evidence identity: the same attestation under a new
	// op_id is rejected rather than silently duplicated.
	var existingID string
	if err := tx.Tx().QueryRowContext(ctx,
		`SELECT attestation_id FROM agy_protection_attestations WHERE attestation_id = ?`,
		rec.AttestationID).Scan(&existingID); err == nil {
		return OperationReceipt{}, fmt.Errorf("attestation %s is already recorded", rec.AttestationID)
	}

	res, err := tx.Tx().ExecContext(ctx, `
INSERT INTO agy_protection_attestations
	(attestation_id, agy_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.AttestationID, rec.AgyVersion, rec.Platform, rec.ManifestDigest,
		rec.ProfileDigest, rec.ProbeResults, rec.ProbedAt, rec.Actor)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert agy protection attestation: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return OperationReceipt{}, fmt.Errorf("attestation %s is already recorded", rec.AttestationID)
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// FindAgyProtectionAttestation returns the attestation id whose durable
// cprot-v2 row matches the frozen launch tuple (agy version, platform,
// manifest digest, profile digest) EXACTLY — all four binding columns
// — or "" when none matches. A row that disagrees on ANY tuple member
// (manifest digest included) never satisfies the lookup.
func (s *Store) FindAgyProtectionAttestation(ctx context.Context, agyVersion, platform, manifestDigest, profileDigest string) (string, error) {
	var id string
	err := s.DB().QueryRowContext(ctx, `
SELECT attestation_id FROM agy_protection_attestations
WHERE agy_version = ? AND platform = ? AND manifest_digest = ? AND profile_digest = ?
ORDER BY probed_at DESC LIMIT 1`,
		agyVersion, platform, manifestDigest, profileDigest).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query agy protection attestation: %w", err)
	}
	return id, nil
}

// AgyProtectionProbeResults returns the durable cprot-v2 record frame
// for an attestation id, or nil when no row carries that id. A missing
// row is nil, not an error — absence of evidence fails closed at the
// caller.
func (s *Store) AgyProtectionProbeResults(ctx context.Context, attestationID string) ([]byte, error) {
	if strings.TrimSpace(attestationID) == "" {
		return nil, nil
	}
	var raw []byte
	err := s.DB().QueryRowContext(ctx,
		`SELECT probe_results FROM agy_protection_attestations WHERE attestation_id = ?`,
		attestationID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query agy protection probe results: %w", err)
	}
	return raw, nil
}

// GetAgyProtectionAttestation returns the durable attestation row (its
// binding tuple and cprot-v2 frame) by id, or nil when no row carries
// that id. The Agy adapter re-compares the stored tuple against the
// frozen policy in force before it validates coverage (AC-010 §3.2): a
// row found by id is never trusted on the lookup's say-so alone. RunID
// is not a column (journal provenance only) and stays empty.
func (s *Store) GetAgyProtectionAttestation(ctx context.Context, attestationID string) (*AgyProtectionAttestationRecord, error) {
	if strings.TrimSpace(attestationID) == "" {
		return nil, nil
	}
	var rec AgyProtectionAttestationRecord
	err := s.DB().QueryRowContext(ctx, `
SELECT attestation_id, agy_version, platform, manifest_digest, profile_digest,
       probe_results, probed_at, actor
FROM agy_protection_attestations WHERE attestation_id = ?`, attestationID).Scan(
		&rec.AttestationID, &rec.AgyVersion, &rec.Platform, &rec.ManifestDigest, &rec.ProfileDigest,
		&rec.ProbeResults, &rec.ProbedAt, &rec.Actor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query agy protection attestation: %w", err)
	}
	return &rec, nil
}

// ── Durable creation uncertainty — episodes ─────────────────────────────

// AgyCreationUncertainty is the durable record request for a session
// creation whose native identity MAY have been created: automatic
// recreation is blocked across service restarts until an explicit,
// controller-authorized resolution of that exact episode.
type AgyCreationUncertainty struct {
	RunID      string
	SessionID  string
	Reason     string
	RecordedBy string // the identity attributed in the journal entry (the adapter)
	CauseOpID  string // the creation operation that ended uncertain (provenance)
	// OrphanNativeID is the native conversation id the creation child
	// reported before the creation was abandoned (e.g. profile/tool drift
	// after a valid init): durable diagnostic evidence of the possibly
	// orphaned conversation, never bound. Empty when none was observed.
	OrphanNativeID string
}

// AgyCreationUncertaintyEpisode is one durable uncertainty episode of a
// logical session. Episodes are numbered monotonically per session; at
// most one is open (unresolved) at a time — the schema enforces it.
type AgyCreationUncertaintyEpisode struct {
	SessionID            string
	Episode              int64
	RunID                string
	Reason               string
	RecordedBy           string
	RecordOpID           string
	CauseOpID            string
	OrphanNativeID       *string
	RecordedAt           time.Time
	Disposition          *string
	ResolutionReason     *string
	ResolutionGeneration *uint64
	ResolutionOpID       *string
	ResolvedAt           *time.Time
}

// Dispositions a controller may record for an open episode. Both permit
// a fresh creation afterwards; they differ in what the controller
// asserts about the possibly-orphaned native identity.
const (
	// AgyUncertaintyAbandonOrphan: the native identity, if it exists, is
	// abandoned (operator purge is a separate action).
	AgyUncertaintyAbandonOrphan = "abandon_orphan"
	// AgyUncertaintyVerifiedAbsent: the controller verified out of band
	// that no native identity was created.
	AgyUncertaintyVerifiedAbsent = "verified_absent"
)

const (
	agyUncertainCmd  = "record_agy_creation_uncertain"
	agyUncertainKind = "agy_creation_uncertain"
	agyResolveCmd    = "resolve_agy_creation_uncertain"
	agyResolveKind   = "agy_creation_uncertainty_resolved"
)

// agyUncertaintyOpID derives the deterministic journal op id of one
// episode: the episode number, not the caller's op id, is the identity,
// so N concurrent callers sharing one uncertain creation record ONE
// episode, and a replay after a crash re-records the same fact.
func agyUncertaintyOpID(sessionID string, episode int64) string {
	return fmt.Sprintf("op-agy-creation-uncertain-%s-e%d", sessionID, episode)
}

// RecordAgyCreationUncertain durably opens an uncertainty episode for
// the session, or returns the already-open episode's receipt: while an
// episode is open, creation is blocked, so a second uncertain outcome
// cannot be a distinct event. After a resolution, a NEW uncertain
// outcome opens the next episode (monotonic), never replays the old
// one. Returns the episode number with the committed receipt.
func (s *Store) RecordAgyCreationUncertain(ctx context.Context, rec AgyCreationUncertainty) (int64, OperationReceipt, error) {
	if strings.TrimSpace(rec.SessionID) == "" || strings.TrimSpace(rec.RunID) == "" {
		return 0, OperationReceipt{}, errors.New("creation-uncertainty record requires the session and run provenance")
	}
	if strings.TrimSpace(rec.Reason) == "" {
		return 0, OperationReceipt{}, errors.New("creation-uncertainty record requires the reason")
	}
	if strings.TrimSpace(rec.RecordedBy) == "" || strings.TrimSpace(rec.CauseOpID) == "" {
		return 0, OperationReceipt{}, errors.New("creation-uncertainty record requires the recording identity and the causing operation id")
	}

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return 0, OperationReceipt{}, err
	}
	defer tx.Rollback()

	// Open episode: the fact is already durable — replay its receipt.
	var openEpisode int64
	var openOpID string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT episode, record_op_id FROM agy_creation_uncertainties
WHERE session_id = ? AND disposition IS NULL`, rec.SessionID).Scan(&openEpisode, &openOpID)
	if err == nil {
		receipt, err := journalReceipt(ctx, tx.Tx(), openOpID)
		if err != nil {
			return 0, OperationReceipt{}, err
		}
		return openEpisode, receipt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, OperationReceipt{}, fmt.Errorf("query open creation uncertainty: %w", err)
	}

	var maxEpisode sql.NullInt64
	if err := tx.Tx().QueryRowContext(ctx,
		`SELECT MAX(episode) FROM agy_creation_uncertainties WHERE session_id = ?`, rec.SessionID).Scan(&maxEpisode); err != nil {
		return 0, OperationReceipt{}, fmt.Errorf("query creation uncertainty episodes: %w", err)
	}
	episode := int64(1)
	if maxEpisode.Valid {
		episode = maxEpisode.Int64 + 1
	}
	opID := agyUncertaintyOpID(rec.SessionID, episode)
	fp := computeFingerprint(agyUncertainCmd, rec.SessionID, rec.RunID, fmt.Sprintf("%d", episode), rec.Reason)
	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, rec.RecordedBy, agyUncertainCmd, fp); err != nil {
		return 0, OperationReceipt{}, err
	} else if receipt != nil {
		// A journal entry without its episode row cannot exist (same
		// transaction); a stale match here is a corrupt store.
		return 0, OperationReceipt{}, fmt.Errorf("creation-uncertainty journal entry %s exists without its episode row", opID)
	}

	now := time.Now().UTC()
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      agyUncertainCmd,
		SessionID:        rec.SessionID,
		CommittedVersion: episode,
		CreatedAt:        now,
		Payload:          rec.Reason,
	}
	if err := recordJournalEntry(tx.Tx(), opID, agyUncertainCmd, fp, rec.RunID, rec.SessionID, "",
		agyUncertainKind, receipt, rec.RecordedBy); err != nil {
		return 0, OperationReceipt{}, err
	}
	if _, err := tx.Tx().ExecContext(ctx, `
INSERT INTO agy_creation_uncertainties
	(session_id, episode, run_id, reason, recorded_by, record_op_id, cause_op_id, orphan_native_id, recorded_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.SessionID, episode, rec.RunID, rec.Reason, rec.RecordedBy, opID, rec.CauseOpID,
		nullableString(rec.OrphanNativeID), now.Format(time.RFC3339Nano)); err != nil {
		return 0, OperationReceipt{}, fmt.Errorf("insert creation uncertainty episode: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, OperationReceipt{}, err
	}
	return episode, receipt, nil
}

// ResolveAgyCreationUncertainty durably resolves ONE exact open episode
// under CURRENT controller authority: the presented lease must be the
// run's active adopted controller credential at the expected
// generation, re-validated inside the write transaction (a superseded
// or revoked lease, or a stale generation, is refused). The disposition
// and reason are journaled; the operation is idempotent by op_id with
// receipt replay. An unknown or already-resolved episode is refused —
// automatic retries never reach this seam.
func (s *Store) ResolveAgyCreationUncertainty(ctx context.Context, opID, callerLease string, expectedGeneration uint64, sessionID string, episode int64, disposition, reason string) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" || strings.TrimSpace(callerLease) == "" {
		return OperationReceipt{}, errors.New("creation-uncertainty resolution requires the operation id and the controller lease")
	}
	if strings.TrimSpace(sessionID) == "" || episode < 1 {
		return OperationReceipt{}, errors.New("creation-uncertainty resolution requires the session id and the episode")
	}
	switch disposition {
	case AgyUncertaintyAbandonOrphan, AgyUncertaintyVerifiedAbsent:
	default:
		return OperationReceipt{}, fmt.Errorf("creation-uncertainty resolution disposition %q is not one of %s|%s",
			disposition, AgyUncertaintyAbandonOrphan, AgyUncertaintyVerifiedAbsent)
	}
	if strings.TrimSpace(reason) == "" {
		return OperationReceipt{}, errors.New("creation-uncertainty resolution requires the reason")
	}

	fp := computeFingerprint(agyResolveCmd, sessionID, fmt.Sprintf("%d", episode), disposition, reason)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	var runID string
	if err := tx.Tx().QueryRowContext(ctx,
		`SELECT run_id FROM sessions WHERE session_id = ?`, sessionID).Scan(&runID); err != nil {
		return OperationReceipt{}, fmt.Errorf("query session for uncertainty resolution: %w", err)
	}
	gen, err := classifyCredential(ctx, tx.Tx(), runID, callerLease, true)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("controller authority: %w", err)
	}
	if gen != expectedGeneration {
		return OperationReceipt{}, fmt.Errorf("controller authority: expected generation %d, active generation is %d", expectedGeneration, gen)
	}
	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, agyResolveCmd, fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var existing sql.NullString
	err = tx.Tx().QueryRowContext(ctx, `
SELECT disposition FROM agy_creation_uncertainties
WHERE session_id = ? AND episode = ?`, sessionID, episode).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return OperationReceipt{}, fmt.Errorf("session %s has no creation-uncertainty episode %d", sessionID, episode)
	}
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query creation uncertainty episode: %w", err)
	}
	if existing.Valid {
		return OperationReceipt{}, fmt.Errorf("session %s creation-uncertainty episode %d is already resolved (%s)",
			sessionID, episode, existing.String)
	}

	now := time.Now().UTC()
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      agyResolveCmd,
		SessionID:        sessionID,
		CommittedVersion: episode,
		CreatedAt:        now,
		Payload:          disposition,
	}
	if err := recordJournalEntry(tx.Tx(), opID, agyResolveCmd, fp, runID, sessionID, "",
		agyResolveKind, receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}
	res, err := tx.Tx().ExecContext(ctx, `
UPDATE agy_creation_uncertainties
SET disposition = ?, resolution_reason = ?, resolution_generation = ?, resolution_op_id = ?, resolved_at = ?
WHERE session_id = ? AND episode = ? AND disposition IS NULL`,
		disposition, reason, gen, opID, now.Format(time.RFC3339Nano), sessionID, episode)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("resolve creation uncertainty episode: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return OperationReceipt{}, fmt.Errorf("resolve creation uncertainty episode: expected one open row, updated %d (%v)", n, err)
	}
	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// HasAgyCreationUncertainty reports whether the logical session has an
// OPEN creation-uncertainty episode.
func (s *Store) HasAgyCreationUncertainty(ctx context.Context, sessionID string) (bool, error) {
	ep, err := s.OpenAgyCreationUncertainty(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return ep != nil, nil
}

// OpenAgyCreationUncertainty returns the session's open episode, or nil
// when none is open — the controller resolves exactly this one.
func (s *Store) OpenAgyCreationUncertainty(ctx context.Context, sessionID string) (*AgyCreationUncertaintyEpisode, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT session_id, episode, run_id, reason, recorded_by, record_op_id, cause_op_id, orphan_native_id, recorded_at,
       disposition, resolution_reason, resolution_generation, resolution_op_id, resolved_at
FROM agy_creation_uncertainties WHERE session_id = ? AND disposition IS NULL`, sessionID)
	return scanAgyUncertaintyEpisode(row)
}

// AgyCreationUncertaintyEpisodes returns every episode of a session in
// episode order (evidence: the resolved history is never deleted).
func (s *Store) AgyCreationUncertaintyEpisodes(ctx context.Context, sessionID string) ([]AgyCreationUncertaintyEpisode, error) {
	rows, err := s.DB().QueryContext(ctx, `
SELECT session_id, episode, run_id, reason, recorded_by, record_op_id, cause_op_id, orphan_native_id, recorded_at,
       disposition, resolution_reason, resolution_generation, resolution_op_id, resolved_at
FROM agy_creation_uncertainties WHERE session_id = ? ORDER BY episode`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query creation uncertainty episodes: %w", err)
	}
	defer rows.Close()
	var out []AgyCreationUncertaintyEpisode
	for rows.Next() {
		ep, err := scanAgyUncertaintyEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ep)
	}
	return out, rows.Err()
}

func scanAgyUncertaintyEpisode(row rowScanner) (*AgyCreationUncertaintyEpisode, error) {
	var ep AgyCreationUncertaintyEpisode
	var recordedAt string
	var disposition, resolutionReason, resolutionOpID, resolvedAt, orphan sql.NullString
	var resolutionGen sql.NullInt64
	if err := row.Scan(&ep.SessionID, &ep.Episode, &ep.RunID, &ep.Reason, &ep.RecordedBy, &ep.RecordOpID,
		&ep.CauseOpID, &orphan, &recordedAt, &disposition, &resolutionReason, &resolutionGen, &resolutionOpID, &resolvedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan creation uncertainty episode: %w", err)
	}
	ep.RecordedAt, _ = time.Parse(time.RFC3339Nano, recordedAt)
	ep.Disposition = nullStr(disposition)
	ep.OrphanNativeID = nullStr(orphan)
	ep.ResolutionReason = nullStr(resolutionReason)
	ep.ResolutionOpID = nullStr(resolutionOpID)
	if resolutionGen.Valid {
		g := uint64(resolutionGen.Int64)
		ep.ResolutionGeneration = &g
	}
	if resolvedAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, resolvedAt.String); err == nil {
			ep.ResolvedAt = &t
		}
	}
	return &ep, nil
}

// nullableString maps an empty string to SQL NULL.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
