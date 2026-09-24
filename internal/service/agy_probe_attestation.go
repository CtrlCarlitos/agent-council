package service

// AC-010 service surface for the Agy adapter, mirroring the AC-009 codex
// file one-to-one (codex_probe_attestation.go):
//
//   - RecordAgyProbeAttestation: the operator-authorized cprot-v2
//     journal operation (spec §3.2). The binding tuple (cli version,
//     platform identity, toolkit-manifest digest, profile digest) is
//     DERIVED from the run's stored frozen profile — never from the
//     request — and the record set must cover that profile exactly
//     (ExpectedCoverage + ValidateCoverage, then the canonical frame is
//     re-checked with ValidateFrame); anything less is refused before
//     any write.
//   - CreateAgySession: the production birth path (§3.3): model and
//     workspace DERIVED (frozen profile; the AC-005 allocation root —
//     Agy has no sandbox writable roots), authority pre-flighted and
//     re-validated inside BindAgySession's transaction. An uncertain
//     creation opens a durable episode (the adapter only tombstones in
//     memory); a binding refused after the native conversation was
//     created (e.g. the lease was superseded in flight) records that
//     conversation as the orphan of an episode, so no future birth can
//     silently create a second one.
//   - ResolveAgySessionCreationUncertainty: controller-authorized
//     resolution of ONE exact open episode, journaled; only then is the
//     adapter's in-process tombstone cleared.
//   - agyRequiredToolsSource: the adapter's RequiredToolsFor seam, read
//     from the journaled dispatch intent (the queue-time set, §3.5).

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/agy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// AgyProbeAttestationRequest carries one operator-authorized Agy
// attestation recording. The binding tuple and the expected coverage
// are derived from the run's stored profile, not taken from here.
type AgyProbeAttestationRequest struct {
	OpID string
	// OperatorToken is the operator credential (the service auth token),
	// compared in constant time before any write.
	OperatorToken string
	// Actor is the operator identity attributed on the record and its
	// journal entry; it must match the attestation's recorded actor.
	Actor string
	// RunID is the run whose stored frozen cprof-v4 profile the probe
	// suite covered: the SINGLE source of the tuple and of the coverage.
	RunID string
	// AttestationID, when supplied, must equal the recomputed cprot-v2
	// digest of Attestation (transport integrity); the recorded id is
	// always the recomputed one.
	AttestationID string
	Attestation   codex.ProtectionAttestation
}

// RecordAgyProbeAttestation validates the attestation against the run's
// frozen profile and persists the durable cprot-v2 row with its journal
// entry (idempotent by op_id with receipt replay). Fail closed before
// any write on: a missing op_id, operator credential, actor, or run; an
// actor mismatch; no agy wiring; a run profile that is not a launchable
// agy profile or does not re-derive its digest; any tuple member of the
// attestation disagreeing with the profile-derived tuple; incomplete or
// unexpected coverage (a frame missing a mapped tool included); or an
// AttestationID that is not the recomputed digest.
func (s *Server) RecordAgyProbeAttestation(ctx context.Context, req AgyProbeAttestationRequest) (storage.OperationReceipt, error) {
	if s.store == nil {
		return storage.OperationReceipt{}, errors.New("storage store is required")
	}
	if strings.TrimSpace(req.OpID) == "" {
		return storage.OperationReceipt{}, errors.New("attestation operation requires an op_id")
	}
	if req.OperatorToken == "" {
		return storage.OperationReceipt{}, errors.New("attestation operation requires the operator credential")
	}
	if subtle.ConstantTimeCompare([]byte(req.OperatorToken), []byte(s.cfg.AuthToken)) != 1 {
		return storage.OperationReceipt{}, errors.New("operator credential is invalid; refusing to record the attestation")
	}
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
	if strings.TrimSpace(s.cfg.AgyBinaryPath) == "" || strings.TrimSpace(s.cfg.AgyEvidenceRoot) == "" {
		return storage.OperationReceipt{}, errors.New(
			"no agy adapter is wired; cannot validate or record an agy attestation")
	}

	frozen, err := s.agyFrozenRun(ctx, req.RunID)
	if err != nil {
		return storage.OperationReceipt{}, err
	}
	cov, err := agy.ExpectedCoverage(frozen.policy)
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("run %s expected coverage: %w", req.RunID, err)
	}
	// Tuple binding: the digests inside the attestation (and therefore
	// inside its cprot-v2 id) must be the run-derived ones.
	if req.Attestation.ManifestDigest != frozen.manifestDigest {
		return storage.OperationReceipt{}, fmt.Errorf(
			"attestation manifest digest %q is not run %s's frozen toolkit-manifest digest %q",
			req.Attestation.ManifestDigest, req.RunID, frozen.manifestDigest)
	}
	if req.Attestation.ProfileDigest != frozen.profileDigest {
		return storage.OperationReceipt{}, fmt.Errorf(
			"attestation profile digest %q is not run %s's frozen profile digest %q",
			req.Attestation.ProfileDigest, req.RunID, frozen.profileDigest)
	}
	if err := agy.ValidateCoverage(req.Attestation, cov); err != nil {
		return storage.OperationReceipt{}, fmt.Errorf(
			"attestation does not cover run %s's frozen profile; refusing to record evidence that could never unlock it: %w",
			req.RunID, err)
	}

	records, err := req.Attestation.EncodeProbeRecords()
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("canonical probe-record encoding: %w", err)
	}
	// The durable frame is what the eligibility lookup re-validates:
	// check the encoded bytes exactly as they will be stored.
	if err := agy.ValidateFrame(records, cov); err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("canonical probe-record frame does not cover run %s: %w", req.RunID, err)
	}
	digest, err := req.Attestation.Digest()
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("canonical attestation digest: %w", err)
	}
	if req.AttestationID != "" && req.AttestationID != digest {
		return storage.OperationReceipt{}, fmt.Errorf(
			"supplied attestation id %q is not the recomputed cprot-v2 digest %q", req.AttestationID, digest)
	}

	return s.store.RecordAgyProtectionAttestation(ctx, req.OpID, storage.AgyProtectionAttestationRecord{
		AttestationID:  digest,
		RunID:          req.RunID,
		AgyVersion:     frozen.policy.CLIVersion,
		Platform:       req.Attestation.PlatformIdentity(),
		ManifestDigest: frozen.manifestDigest,
		ProfileDigest:  frozen.profileDigest,
		ProbeResults:   string(records),
		ProbedAt:       req.Attestation.ProbedAt,
		Actor:          req.Attestation.Actor,
	})
}

// agyFrozenRunRecord is a run's stored frozen profile validated as a
// launchable agy profile, with its re-derived digests.
type agyFrozenRunRecord struct {
	rec            storage.RunProfileRecord
	policy         agy.AgyLaunchPolicy
	profileDigest  string
	manifestDigest string
}

// agyFrozenRun resolves the run's stored frozen profile, validates it as
// a launchable agy profile against the wired evidence root, and re-
// derives its profile digest (checked against the stored one) and its
// toolkit-manifest digest.
func (s *Server) agyFrozenRun(ctx context.Context, runID string) (agyFrozenRunRecord, error) {
	rec, err := s.store.GetRunProfile(ctx, runID)
	if err != nil {
		return agyFrozenRunRecord{}, fmt.Errorf("run profile lookup for %s: %w", runID, err)
	}
	if strings.TrimSpace(rec.Profile.AlgoVersion) == "" {
		return agyFrozenRunRecord{}, fmt.Errorf("run %s has no parseable frozen profile", runID)
	}
	policy, err := agy.ValidateAgyHarness(rec.Profile, s.cfg.AgyEvidenceRoot)
	if err != nil {
		return agyFrozenRunRecord{}, fmt.Errorf("run %s's frozen profile is not a launchable agy profile: %w", runID, err)
	}
	digest, _, err := storage.ComputeProfileDigest(rec.Profile)
	if err != nil {
		return agyFrozenRunRecord{}, fmt.Errorf("run %s frozen profile digest: %w", runID, err)
	}
	if digest != rec.ProfileDigest {
		return agyFrozenRunRecord{}, fmt.Errorf(
			"run %s frozen profile digest %q does not re-derive from its stored profile (%q); refusing to bind evidence to a corrupt record",
			runID, rec.ProfileDigest, digest)
	}
	manifest, err := storage.ComputeToolkitManifestDigest(rec.Profile.ToolkitManifest.ToolkitManifest)
	if err != nil {
		return agyFrozenRunRecord{}, fmt.Errorf("run %s toolkit manifest digest: %w", runID, err)
	}
	return agyFrozenRunRecord{rec: rec, policy: policy, profileDigest: digest, manifestDigest: manifest}, nil
}

// agyCreationRecorder is the identity attributed on the durable
// creation-uncertainty episodes the service opens.
const agyCreationRecorder = "agy-service"

// CreateAgySession is the PRODUCTION agy session-birth path (§3.3): the
// service trusts nothing from the caller beyond the session identity and
// the controller credential. The model comes from the run's stored
// frozen profile (which must be the profile this service's agy adapter
// froze to), the workspace root is the AC-005 allocation for (run,
// session), and the persisted binding carries the run's re-derived
// profile digest. Controller authority is pre-flighted, the durable
// creation-uncertainty block is checked before any child, the wired
// adapter creates the native conversation provider-free, and the
// binding is persisted by BindAgySession (authority re-validated inside
// its transaction, journaled, receipt-replayed).
//
// Unlike the codex path, a binding refused AFTER the native conversation
// was created is recorded as the orphan of a durable uncertainty
// episode: an agy conversation is a file the adapter cannot re-offer
// across processes, so the current controller must dispose of it
// explicitly before a fresh one is created.
func (s *Server) CreateAgySession(ctx context.Context, opID, controllerLease, sessionID string) (adapter.SessionBinding, storage.OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" || strings.TrimSpace(controllerLease) == "" || strings.TrimSpace(sessionID) == "" {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, errors.New(
			"session creation requires the operation id, the controller lease, and the session id")
	}
	if s.adapter == nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, errors.New(
			"no contributor adapter is wired; cannot create an Agy session")
	}
	if strings.TrimSpace(s.cfg.AgyBinaryPath) == "" || strings.TrimSpace(s.cfg.AgyEvidenceRoot) == "" {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, errors.New(
			"no agy adapter is wired; cannot create an Agy session")
	}
	if s.workspaceManager == nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, errors.New(
			"no workspace manager is wired (WorkspaceBaseDir); cannot allocate an Agy session workspace")
	}
	meta, err := s.store.GetSessionMetadata(ctx, sessionID)
	if err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("session lookup: %w", err)
	}
	if meta.Contributor != string(council.Agy) {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf(
			"session %s contributor is %q, not agy; cannot create an Agy session", sessionID, meta.Contributor)
	}
	runID := meta.RunID
	// Pre-flight authority: the cheap early refusal before any child;
	// BindAgySession re-validates inside its transaction.
	if err := s.store.CheckControllerAuthority(ctx, runID, controllerLease); err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("controller authority: %w", err)
	}

	frozen, err := s.agyFrozenRun(ctx, runID)
	if err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, err
	}
	wired, _, err := storage.ComputeProfileDigest(s.cfg.AgyProfile)
	if err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("wired agy profile digest: %w", err)
	}
	if frozen.profileDigest != wired {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf(
			"run %s's frozen profile digest %q is not the profile this service's agy adapter froze to (%q); refusing to create a session the adapter could never dispatch",
			runID, frozen.profileDigest, wired)
	}

	// Durable block: an OPEN episode survives restarts; no child starts.
	if open, err := s.store.OpenAgyCreationUncertainty(ctx, sessionID); err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("creation-uncertainty lookup: %w", err)
	} else if open != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, &adapter.ErrSessionCreationUncertain{
			SessionID:   adapter.SessionID(sessionID),
			Contributor: council.Agy,
			Err: fmt.Errorf("a durable creation uncertainty is recorded for this session (episode %d); automatic recreation is blocked until it is explicitly resolved",
				open.Episode),
		}
	}

	workspaceRoot, err := s.agySessionWorkspace(runID, sessionID, frozen.rec)
	if err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, err
	}

	req := adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID(sessionID),
		Contributor: council.Agy,
		Config: adapter.SessionConfig{
			WorkspaceRoot: workspaceRoot,
			Model:         frozen.policy.Model,
		},
	}
	binding, err := s.adapter.CreateSession(ctx, req)
	if err != nil {
		// The native conversation MAY exist: open the durable episode
		// (idempotent while open; the creation-drift path already opened
		// it with the orphan id, which this replays).
		var unc *adapter.ErrSessionCreationUncertain
		if errors.As(err, &unc) {
			if _, _, jerr := s.store.RecordAgyCreationUncertain(ctx, storage.AgyCreationUncertainty{
				RunID:          runID,
				SessionID:      sessionID,
				Reason:         unc.Error(),
				RecordedBy:     agyCreationRecorder,
				CauseOpID:      opID,
				OrphanNativeID: unc.PartialNativeID,
			}); jerr != nil {
				return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf(
					"record durable creation uncertainty: %w (original: %w)", jerr, err)
			}
		}
		return adapter.SessionBinding{}, storage.OperationReceipt{}, fmt.Errorf("native session creation: %w", err)
	}

	receipt, err := s.store.BindAgySession(ctx, opID, controllerLease, storage.AgySessionBinding{
		SessionID:     sessionID,
		NativeID:      binding.NativeSessionID,
		Model:         frozen.policy.Model,
		Workspace:     workspaceRoot,
		ProfileDigest: frozen.profileDigest,
		CreatedAt:     time.Now().UTC(),
	})
	if err != nil {
		return adapter.SessionBinding{}, storage.OperationReceipt{}, s.recordAgyBindingOrphan(ctx, runID, sessionID, opID, binding.NativeSessionID, err)
	}
	return binding, receipt, nil
}

// recordAgyBindingOrphan handles a refused binding after the native
// conversation was created: unless the session is already bound to that
// very conversation (a replay under another op id), the conversation is
// recorded as the orphan of a durable uncertainty episode. The returned
// error always wraps the binding refusal.
func (s *Server) recordAgyBindingOrphan(ctx context.Context, runID, sessionID, opID, nativeID string, bindErr error) error {
	if existing, lerr := s.store.GetAgySessionBinding(ctx, sessionID); lerr == nil && existing != nil && existing.NativeID == nativeID {
		return fmt.Errorf("persist binding: %w", bindErr)
	}
	episode, _, jerr := s.store.RecordAgyCreationUncertain(ctx, storage.AgyCreationUncertainty{
		RunID:          runID,
		SessionID:      sessionID,
		Reason:         "native conversation created but its binding was refused: " + bindErr.Error(),
		RecordedBy:     agyCreationRecorder,
		CauseOpID:      opID,
		OrphanNativeID: nativeID,
	})
	if jerr != nil {
		return fmt.Errorf("persist binding: %w; the created conversation %s could not be recorded as an orphan: %v", bindErr, nativeID, jerr)
	}
	return fmt.Errorf("persist binding: %w; the created conversation %s is recorded as the orphan of creation-uncertainty episode %d",
		bindErr, nativeID, episode)
}

// agySessionWorkspace returns the AC-005 workspace root for (run,
// session), allocating it on first use (serialized, as for codex: the
// manager publishes a placeholder during allocation).
func (s *Server) agySessionWorkspace(runID, sessionID string, rec storage.RunProfileRecord) (string, error) {
	s.agyBirthMu.Lock()
	defer s.agyBirthMu.Unlock()
	paths, ok := s.workspaceManager.GetPaths(runID, sessionID)
	if !ok {
		var err error
		paths, err = s.workspaceManager.AllocateWorkspace(runID, sessionID, rec.Profile.WorkspaceMode, rec.SourceRepoIdentity, rec.SourceCommit)
		if err != nil {
			return "", fmt.Errorf("allocate workspace: %w", err)
		}
	}
	if strings.TrimSpace(paths.Root) == "" {
		return "", fmt.Errorf("workspace allocation for %s/%s has no root", runID, sessionID)
	}
	return paths.Root, nil
}

// AgyCreationUncertaintyResolution is one controller-authorized
// resolution of ONE exact open creation-uncertainty episode.
type AgyCreationUncertaintyResolution struct {
	OpID string
	// ControllerLease is re-validated inside the storage transaction.
	ControllerLease string
	// ExpectedGeneration must be the active controller generation.
	ExpectedGeneration uint64
	SessionID          string
	// Episode is the exact open episode (storage.OpenAgyCreationUncertainty).
	Episode int64
	// Disposition is storage.AgyUncertaintyAbandonOrphan or
	// storage.AgyUncertaintyVerifiedAbsent.
	Disposition string
	Reason      string
}

// ResolveAgySessionCreationUncertainty records the controller-authorized
// resolution journal entry FIRST (durable truth, authority re-validated
// in the transaction) and only then clears the wired agy adapter's
// in-process tombstone.
func (s *Server) ResolveAgySessionCreationUncertainty(ctx context.Context, req AgyCreationUncertaintyResolution) (storage.OperationReceipt, error) {
	if s.store == nil {
		return storage.OperationReceipt{}, errors.New("storage store is required")
	}
	if strings.TrimSpace(req.OpID) == "" || strings.TrimSpace(req.SessionID) == "" {
		return storage.OperationReceipt{}, errors.New("creation-uncertainty resolution requires the operation id and the session id")
	}
	if strings.TrimSpace(req.ControllerLease) == "" {
		return storage.OperationReceipt{}, errors.New("creation-uncertainty resolution requires the controller lease")
	}
	receipt, err := s.store.ResolveAgyCreationUncertainty(ctx, req.OpID, req.ControllerLease, req.ExpectedGeneration,
		req.SessionID, req.Episode, req.Disposition, req.Reason)
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("resolve creation uncertainty: %w", err)
	}
	if agyAdapter, ok := s.adapter.(*agy.AgyAdapter); ok {
		agyAdapter.ResolveCreationUncertainty(adapter.SessionID(req.SessionID))
	}
	return receipt, nil
}

// agyRequiredToolsSource is the service's agy.RequiredToolsSource: the
// required_tools set validated at queue time and copied onto the durable
// dispatch intent at release (§3.5). An empty set reports absent, so the
// adapter applies the frozen default_required_tools.
type agyRequiredToolsSource struct {
	store *storage.Store
}

var _ agy.RequiredToolsSource = (*agyRequiredToolsSource)(nil)

// RequiredToolsFor never substitutes the defaults for an unreadable set:
// a read error, or a turn without a durable dispatch intent, is returned
// as an error (the adapter refuses the dispatch before any reservation);
// only a present intent with an empty set reports ok=false.
func (s *agyRequiredToolsSource) RequiredToolsFor(ctx context.Context, ref adapter.TurnRef) ([]string, bool, error) {
	details, err := s.store.GetTurnDetails(ctx, string(ref.SessionID), ref.TurnKey)
	if err != nil {
		return nil, false, fmt.Errorf("turn %s/%s details: %w", ref.SessionID, ref.TurnKey, err)
	}
	if details == nil || details.DispatchIntent == nil {
		return nil, false, fmt.Errorf("turn %s/%s has no durable dispatch intent", ref.SessionID, ref.TurnKey)
	}
	if len(details.DispatchIntent.RequiredTools) == 0 {
		return nil, false, nil
	}
	return append([]string(nil), details.DispatchIntent.RequiredTools...), true, nil
}
