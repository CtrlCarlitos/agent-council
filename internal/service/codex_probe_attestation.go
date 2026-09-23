package service

// AC-009 cprot-v2 protection-attestation journal operation and the
// production codex session-birth path (spec §3.3/§3.7, AC-008 pattern):
// the durable attestation row is created ONLY through the operator-
// authorized operation — the operator actor is bound into the request,
// the credential is the configured service auth token compared in
// constant time, and the manifest digest is re-derived from the SINGLE
// canonical source (storage.ComputeToolkitManifestDigest) so the durable
// manifest_digest column can never disagree with the frozen launch
// tuple. CreateCodexSession is the production birth path: controller
// authority is pre-flighted, the wired adapter creates the native
// thread, and the binding is persisted under the run controller's
// credential; a creation that ended UNCERTAIN is recorded durably so a
// service restart can never silently re-create a native thread (§3.4).

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
// identity, the run provenance, the frozen toolkit manifest in force,
// and the typed cprot-v2 attestation.
type CodexProbeAttestationRequest struct {
	OpID string
	// OperatorToken is the operator credential (the service auth
	// token). Authority is validated against the configured service
	// token before any write; a claimed actor alone authorizes nothing.
	OperatorToken string
	// Actor is the operator identity attributed on the record and its
	// journal entry. It must match the attestation's recorded actor.
	Actor string
	RunID string // the run the probe was performed for (journal provenance)
	// ToolkitManifest is the frozen toolkit manifest in force for the
	// probe. The canonical manifest digest is re-derived here — the
	// SINGLE source (storage.ComputeToolkitManifestDigest) shared with
	// the launch-time freeze — and the attestation's recorded manifest
	// digest must match it exactly, or the evidence would never satisfy
	// the protection tuple match.
	ToolkitManifest storage.ToolkitManifest
	Attestation     codex.ProtectionAttestation
}

// RecordCodexProbeAttestation validates the attestation, re-derives the
// canonical manifest digest, canonically encodes its probe records, and
// persists the durable cprot-v2 row with its journal entry. Fail closed:
// a missing op_id, a missing or mismatched operator actor, a manifest
// digest disagreement, or any attestation invariant violation (including
// a denied=false probe record) rejects the operation before any write.
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

	// Manifest identity: the durable manifest_digest column value comes
	// ONLY from storage.ComputeToolkitManifestDigest — the same
	// derivation the launch policy froze at validation (spec §3.7 tuple
	// match). Evidence claiming a different manifest digest would never
	// satisfy the lookup, so it is refused here instead of being
	// persisted as unusable rows.
	manifestDigest, err := storage.ComputeToolkitManifestDigest(req.ToolkitManifest)
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("canonical toolkit manifest digest: %w", err)
	}
	if req.Attestation.ManifestDigest != manifestDigest {
		return storage.OperationReceipt{}, fmt.Errorf(
			"attestation manifest digest %q does not match the canonical toolkit manifest digest %q; refusing to record evidence that could never satisfy the launch tuple",
			req.Attestation.ManifestDigest, manifestDigest)
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
		CodexVersion:   req.Attestation.CodexVersion,
		Platform:       req.Attestation.PlatformIdentity(),
		ManifestDigest: manifestDigest,
		ProfileDigest:  req.Attestation.ProfileDigest,
		ProbeResults:   string(records),
		ProbedAt:       req.Attestation.ProbedAt,
		Actor:          req.Attestation.Actor,
	})
}

// codexCreationUncertaintyOpID derives the deterministic journal op id
// for a session's creation-uncertainty record: a replay after a crash
// re-records the same fact idempotently instead of duplicating it.
func codexCreationUncertaintyOpID(sessionID string) string {
	return "op-codex-creation-uncertain-" + sessionID
}

// CreateCodexSession is the PRODUCTION codex session-birth path (§3.3/
// §3.4): the service owns the whole flow — controller authority is
// pre-flighted, the durable §3.4 creation-uncertainty block is checked
// BEFORE any child can start (a restart cannot silently re-create a
// native thread for an uncertain session), the wired adapter generates
// the native identity, and the returned binding is persisted under the
// run controller's credential. When the adapter reports the creation
// UNCERTAIN (the native thread may exist), the uncertainty is recorded
// durably in the same breath — the in-adapter tombstone covers this
// process only; this journal record covers every future one.
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
	// caller or any native identity is minted.
	if err := s.store.CheckControllerAuthority(ctx, runID, controllerLease); err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("controller authority: %w", err)
	}

	// Durable §3.4 tombstone: a session whose creation ended uncertain
	// is blocked across restarts until an explicit resolution. The
	// in-adapter tombstone cannot cover a fresh process; this durable
	// record can. Checked BEFORE the adapter runs so no child can start.
	if blocked, err := s.store.HasCodexCreationUncertainty(ctx, string(req.SessionID)); err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("creation-uncertainty lookup: %w", err)
	} else if blocked {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, &adapter.ErrSessionCreationUncertain{
			SessionID:   req.SessionID,
			Contributor: req.Contributor,
			Err:         errors.New("a durable creation uncertainty is recorded for this session; automatic recreation is blocked until it is explicitly resolved"),
		}
	}

	binding, err := s.adapter.CreateSession(ctx, req)
	if err != nil {
		// §3.4: the native thread MAY exist. Record the uncertainty
		// durably (idempotent, deterministic op id) so no future service
		// instance can auto-recreate the thread; then surface the typed
		// failure.
		var unc *adapter.ErrSessionCreationUncertain
		if errors.As(err, &unc) {
			reason := unc.Err.Error()
			if _, jerr := s.store.RecordCodexCreationUncertain(ctx, codexCreationUncertaintyOpID(string(req.SessionID)),
				storage.CodexCreationUncertainty{
					RunID:      runID,
					SessionID:  string(req.SessionID),
					Reason:     reason,
					RecordedBy: "codex-adapter",
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
	if err := s.store.InsertCodexSessionBinding(ctx, storage.CodexSessionBinding{
		SessionID:     string(req.SessionID),
		NativeID:      binding.NativeSessionID,
		Model:         req.Config.Model,
		Workspace:     req.Config.WorkspaceRoot,
		ProfileDigest: profileDigest,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("persist binding: %w", err)
	}
	return binding, storage.OperationReceipt{}, nil
}

// ResolveCodexSessionCreationUncertainty is the explicit, controller-
// visible resolution seam for a session's durable creation uncertainty
// (§3.4): it records the resolution journal entry FIRST (durable truth)
// and then clears the wired adapter's in-process tombstone. Automatic
// retries never reach this seam.
func (s *Server) ResolveCodexSessionCreationUncertainty(ctx context.Context, opID, sessionID, resolvedBy string) (storage.OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" || strings.TrimSpace(sessionID) == "" {
		return storage.OperationReceipt{}, errors.New("creation-uncertainty resolution requires the operation id and the session id")
	}
	receipt, err := s.store.ResolveCodexCreationUncertainty(ctx, opID, sessionID, resolvedBy)
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("resolve creation uncertainty: %w", err)
	}
	if codexAdapter, ok := s.adapter.(*codex.CodexAdapter); ok {
		codexAdapter.ResolveCreationUncertainty(adapter.SessionID(sessionID))
	}
	return receipt, nil
}
