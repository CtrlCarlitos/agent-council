package service

// AC-009 cprot-v2 protection-attestation journal operation and the
// production codex session-birth path (spec §3.3/§3.4/§3.7, AC-008
// pattern):
//
//   - The durable attestation row is created ONLY through the operator-
//     authorized operation. The operator actor is bound into the request,
//     the credential is the configured service auth token compared in
//     constant time, and the binding tuple is DERIVED from the run's
//     stored frozen profile — never from caller-supplied digests — so the
//     row can only ever carry the tuple that belongs to the referenced
//     run. The record set must COVER that profile (codex coverage.go):
//     every enabled tool path, every mutation operation on every
//     mutation-capable path, every pinned approval method with a native
//     refusal enum; anything less, or anything extra, is refused before
//     any write.
//   - CreateCodexSession is the production birth path: controller
//     authority is pre-flighted, the wired adapter creates the native
//     thread, and the binding is persisted by BindCodexSession — which
//     RE-VALIDATES the lease inside its write transaction, journals the
//     op_id, and replays the committed receipt. A creation that ended
//     UNCERTAIN opens a durable uncertainty EPISODE so a service restart
//     can never silently re-create a native thread (§3.4).
//   - ResolveCodexSessionCreationUncertainty resolves ONE exact episode
//     under current controller authority (lease + expected generation
//     re-validated inside the storage transaction) with a disposition
//     and reason; only then is the in-process adapter tombstone cleared.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// CodexProbeAttestationRequest carries one operator-authorized
// attestation recording: the idempotency op_id, the acting operator
// identity, the run whose frozen profile the probe suite covered, and
// the typed cprot-v2 attestation. The binding tuple (codex version,
// platform, manifest digest, profile digest) and the expected coverage
// set are derived from the run's stored profile, not taken from here.
type CodexProbeAttestationRequest struct {
	OpID string
	// OperatorToken is the operator credential (the service auth
	// token). Authority is validated against the configured service
	// token before any write; a claimed actor alone authorizes nothing.
	OperatorToken string
	// Actor is the operator identity attributed on the record and its
	// journal entry. It must match the attestation's recorded actor.
	Actor string
	// RunID is the run the probe suite was performed for: its stored
	// frozen cprof-v3 profile is the SINGLE source of the tuple the row
	// is bound to and of the coverage the records must satisfy.
	RunID       string
	Attestation codex.ProtectionAttestation
}

// RecordCodexProbeAttestation validates the attestation against the
// run's frozen profile, canonically encodes its probe records, and
// persists the durable cprot-v2 row with its journal entry. Fail closed:
// a missing op_id, a missing or mismatched operator actor, a run whose
// stored profile is not a launchable codex profile, any tuple member
// disagreeing with the profile-derived tuple, incomplete or unexpected
// coverage, or any attestation invariant violation rejects the
// operation before any write.
func (s *Server) RecordCodexProbeAttestation(ctx context.Context, req CodexProbeAttestationRequest) (storage.OperationReceipt, error) {
	if s.store == nil {
		return storage.OperationReceipt{}, errors.New("storage store is required")
	}
	if strings.TrimSpace(req.OpID) == "" {
		return storage.OperationReceipt{}, errors.New("attestation operation requires an op_id")
	}
	// Operator authority (AC-004): the caller must present the
	// operator credential — the configured service auth token —
	// compared in constant time. A claimed actor alone authorizes
	// nothing.
	if req.OperatorToken == "" {
		return storage.OperationReceipt{}, errors.New("attestation operation requires the operator credential")
	}
	if subtle.ConstantTimeCompare([]byte(req.OperatorToken), []byte(s.cfg.AuthToken)) != 1 {
		return storage.OperationReceipt{}, errors.New("operator credential is invalid; refusing to record the attestation")
	}
	// The acting operator identity is mandatory and must match the
	// attestation's recorded actor (attribution, journal-linked).
	if strings.TrimSpace(req.Actor) == "" {
		return storage.OperationReceipt{}, errors.New("attestation operation requires the operator actor")
	}
	if strings.TrimSpace(req.RunID) == "" {
		return storage.OperationReceipt{}, errors.New("attestation operation requires the run provenance")
	}
	if strings.TrimSpace(req.Attestation.Actor) != strings.TrimSpace(req.Actor) {
		return storage.OperationReceipt{}, errors.New(
			"attestation actor does not match the requesting operator; refusing to record")
	}
	// The coverage universe and the event-universe re-hash need the
	// codex wiring's evidence root: without a wired codex adapter no
	// codex attestation can be validated, so none is recorded.
	if strings.TrimSpace(s.cfg.CodexBinaryPath) == "" || strings.TrimSpace(s.cfg.CodexEvidenceRoot) == "" {
		return storage.OperationReceipt{}, errors.New(
			"no codex adapter is wired; cannot validate or record a codex attestation")
	}

	// Run-scoped provenance: the tuple the row binds to is derived from
	// the run's STORED frozen profile. A caller cannot journal
	// provenance for run A while recording digests that belong to some
	// other profile.
	cov, err := s.codexCoverageForRun(ctx, req.RunID)
	if err != nil {
		return storage.OperationReceipt{}, err
	}
	if err := req.Attestation.ValidateCoverage(cov); err != nil {
		return storage.OperationReceipt{}, fmt.Errorf(
			"attestation does not cover run %s's frozen profile; refusing to record evidence that could never unlock it: %w",
			req.RunID, err)
	}

	records, err := req.Attestation.EncodeProbeRecords()
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("canonical probe-record encoding: %w", err)
	}
	digest, err := req.Attestation.Digest()
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("canonical attestation digest: %w", err)
	}

	return s.store.RecordCodexProtectionAttestation(ctx, req.OpID, storage.CodexProtectionAttestationRecord{
		AttestationID:  digest,
		RunID:          req.RunID,
		CodexVersion:   cov.CodexVersion,
		Platform:       req.Attestation.PlatformIdentity(),
		ManifestDigest: cov.ManifestDigest,
		ProfileDigest:  cov.ProfileDigest,
		ProbeResults:   string(records),
		ProbedAt:       req.Attestation.ProbedAt,
		Actor:          req.Attestation.Actor,
	})
}

// codexCoverageForRun resolves the run's stored frozen profile,
// validates it as a launchable codex profile (the same derivation the
// production constructor freezes: ValidateCodexHarness, including the
// event-universe re-hash against the wired evidence root), re-derives
// the profile digest and checks it against the stored one, and returns
// the expected cprot-v2 coverage set.
func (s *Server) codexCoverageForRun(ctx context.Context, runID string) (codex.ProtectionCoverage, error) {
	rec, err := s.store.GetRunProfile(ctx, runID)
	if err != nil {
		return codex.ProtectionCoverage{}, fmt.Errorf("run profile lookup for %s: %w", runID, err)
	}
	if strings.TrimSpace(rec.Profile.AlgoVersion) == "" {
		return codex.ProtectionCoverage{}, fmt.Errorf("run %s has no parseable frozen profile", runID)
	}
	policy, err := codex.ValidateCodexHarness(rec.Profile, s.cfg.CodexEvidenceRoot)
	if err != nil {
		return codex.ProtectionCoverage{}, fmt.Errorf("run %s's frozen profile is not a launchable codex profile: %w", runID, err)
	}
	profileDigest, _, err := storage.ComputeProfileDigest(rec.Profile)
	if err != nil {
		return codex.ProtectionCoverage{}, fmt.Errorf("run %s frozen profile digest: %w", runID, err)
	}
	if profileDigest != rec.ProfileDigest {
		return codex.ProtectionCoverage{}, fmt.Errorf(
			"run %s frozen profile digest %q does not re-derive from its stored profile (%q); refusing to bind evidence to a corrupt record",
			runID, rec.ProfileDigest, profileDigest)
	}
	return codex.CoverageFor(policy, profileDigest), nil
}

// CreateCodexSession is the PRODUCTION codex session-birth path (§3.3/
// §3.4): the service owns the whole flow — controller authority is
// pre-flighted, the durable §3.4 creation-uncertainty block is checked
// BEFORE any child can start (a restart cannot silently re-create a
// native thread for an uncertain session), the wired adapter generates
// the native identity, and the returned binding is persisted under the
// run controller's credential in ONE durable transition
// (BindCodexSession re-validates authority inside its transaction,
// journals the op_id, and returns the committed receipt). When the
// adapter reports the creation UNCERTAIN (the native thread may exist),
// a durable uncertainty episode is opened in the same breath — the
// in-adapter tombstone covers this process only; the episode covers
// every future one.
//
// If the binding is refused after the native thread was created (for
// example the lease was handed off during creation), the thread is NOT
// orphaned by the service: the adapter's creation reservation still
// holds the binding, so the current controller's retry receives the
// same native identity and can persist it under its own authority.
func (s *Server) CreateCodexSession(ctx context.Context, opID, controllerLease string, req adapter.CreateSessionRequest) (adapter.SessionBinding, storage.OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" || strings.TrimSpace(controllerLease) == "" {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, errors.New(
			"session creation requires the operation id and the controller lease")
	}
	if s.adapter == nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, errors.New(
			"no contributor adapter is wired; cannot create a Codex session")
	}
	if strings.TrimSpace(s.cfg.CodexBinaryPath) == "" {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, errors.New(
			"no codex adapter is wired; cannot create a Codex session")
	}
	runID, err := s.store.GetSessionRunID(ctx, string(req.SessionID))
	if err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("session run lookup: %w", err)
	}
	// Pre-flight authority: reject before any state is read for the
	// caller or any native identity is minted. BindCodexSession
	// re-validates inside its transaction; this check is the cheap
	// early refusal, not the authority.
	if err := s.store.CheckControllerAuthority(ctx, runID, controllerLease); err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("controller authority: %w", err)
	}

	// Durable §3.4 tombstone: a session with an OPEN uncertainty
	// episode is blocked across restarts until that episode is
	// explicitly resolved. The in-adapter tombstone cannot cover a
	// fresh process; this durable record can. Checked BEFORE the
	// adapter runs so no child can start.
	if open, err := s.store.OpenCodexCreationUncertainty(ctx, string(req.SessionID)); err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("creation-uncertainty lookup: %w", err)
	} else if open != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, &adapter.ErrSessionCreationUncertain{
			SessionID:   req.SessionID,
			Contributor: req.Contributor,
			Err: fmt.Errorf("a durable creation uncertainty is recorded for this session (episode %d); automatic recreation is blocked until it is explicitly resolved",
				open.Episode),
		}
	}

	binding, err := s.adapter.CreateSession(ctx, req)
	if err != nil {
		// §3.4: the native thread MAY exist. Open the durable episode
		// (idempotent while open: N callers sharing one uncertain
		// creation record ONE episode) so no future service instance
		// can auto-recreate the thread; then surface the typed failure.
		var unc *adapter.ErrSessionCreationUncertain
		if errors.As(err, &unc) {
			if _, _, jerr := s.store.RecordCodexCreationUncertain(ctx, storage.CodexCreationUncertainty{
				RunID:      runID,
				SessionID:  string(req.SessionID),
				Reason:     unc.Err.Error(),
				RecordedBy: "codex-adapter",
				CauseOpID:  opID,
			}); jerr != nil {
				return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf(
					"record durable creation uncertainty: %w (original: %w)", jerr, err)
			}
		}
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("native session creation: %w", err)
	}

	profileDigest, _, err := storage.ComputeProfileDigest(s.cfg.CodexProfile)
	if err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("frozen profile digest: %w", err)
	}
	receipt, err := s.store.BindCodexSession(ctx, opID, controllerLease, storage.CodexSessionBinding{
		SessionID:     string(req.SessionID),
		NativeID:      binding.NativeSessionID,
		Model:         req.Config.Model,
		Workspace:     req.Config.WorkspaceRoot,
		ProfileDigest: profileDigest,
		CreatedAt:     time.Now().UTC(),
	})
	if err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("persist binding: %w", err)
	}
	return binding, receipt, nil
}

// CodexCreationUncertaintyResolution is one controller-authorized
// resolution of ONE exact open creation-uncertainty episode.
type CodexCreationUncertaintyResolution struct {
	OpID string
	// ControllerLease is the current controller credential; it is
	// re-validated inside the storage transaction.
	ControllerLease string
	// ExpectedGeneration is the controller generation the caller
	// believes is current; a stale generation is refused.
	ExpectedGeneration uint64
	SessionID          string
	// Episode is the exact open episode being resolved (from
	// storage.OpenCodexCreationUncertainty); a resolution never targets
	// "whatever is open".
	Episode int64
	// Disposition is storage.CodexUncertaintyAbandonOrphan or
	// storage.CodexUncertaintyVerifiedAbsent.
	Disposition string
	Reason      string
}

// ResolveCodexSessionCreationUncertainty is the explicit, controller-
// authorized resolution seam for a session's durable creation
// uncertainty (§3.4): it records the resolution journal entry FIRST
// (durable truth, authority re-validated in the transaction) and only
// then clears the wired adapter's in-process tombstone. Automatic
// retries never reach this seam; a caller without current controller
// authority cannot clear the block.
func (s *Server) ResolveCodexSessionCreationUncertainty(ctx context.Context, req CodexCreationUncertaintyResolution) (storage.OperationReceipt, error) {
	if s.store == nil {
		return storage.OperationReceipt{}, errors.New("storage store is required")
	}
	if strings.TrimSpace(req.OpID) == "" || strings.TrimSpace(req.SessionID) == "" {
		return storage.OperationReceipt{}, errors.New("creation-uncertainty resolution requires the operation id and the session id")
	}
	if strings.TrimSpace(req.ControllerLease) == "" {
		return storage.OperationReceipt{}, errors.New("creation-uncertainty resolution requires the controller lease")
	}
	receipt, err := s.store.ResolveCodexCreationUncertainty(ctx, req.OpID, req.ControllerLease, req.ExpectedGeneration,
		req.SessionID, req.Episode, req.Disposition, req.Reason)
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("resolve creation uncertainty: %w", err)
	}
	if codexAdapter, ok := s.adapter.(*codex.CodexAdapter); ok {
		codexAdapter.ResolveCreationUncertainty(adapter.SessionID(req.SessionID))
	}
	return receipt, nil
}
