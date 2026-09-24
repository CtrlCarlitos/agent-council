package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

type CancelRequest struct {
	InstanceID      string `json:"instance_id"`
	OpID            string `json:"op_id"`
	ControllerLease string `json:"controller_lease"`
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
}

type CancelResponse struct {
	InstanceID         string                   `json:"instance_id"`
	OpID               string                   `json:"op_id"`
	Receipt            storage.OperationReceipt `json:"receipt"`
	CancellationStatus string                   `json:"cancellation_status"`
}

type ControllerConnectRequest struct {
	InstanceID      string `json:"instance_id"`
	OpID            string `json:"op_id"`
	ControllerLease string `json:"controller_lease"`
	ExpectedVersion int64  `json:"expected_version"`
}

type ControllerConnectResponse struct {
	InstanceID string                   `json:"instance_id"`
	OpID       string                   `json:"op_id"`
	Receipt    storage.OperationReceipt `json:"receipt"`
}

type ReconcileRequest struct {
	InstanceID      string `json:"instance_id"`
	OpID            string `json:"op_id"`
	ControllerLease string `json:"controller_lease"`
	ExpectedVersion int64  `json:"expected_version"`
}

type ReconcileResponse struct {
	InstanceID string                   `json:"instance_id"`
	OpID       string                   `json:"op_id"`
	Receipt    storage.OperationReceipt `json:"receipt"`
}

type QueuePromptRequest struct {
	InstanceID      string `json:"instance_id"`
	OpID            string `json:"op_id"`
	ControllerLease string `json:"controller_lease"`
	ExpectedVersion int64  `json:"expected_version"`
	TurnKey         string `json:"turn_key"`
	Prompt          string `json:"prompt"`
	// RequiredTools is the optional AC-010 required-tool set for an agy
	// session's turn: validated here against the run's frozen
	// expected_tools, journaled with the prompt, immutable thereafter.
	// Absent/empty means the frozen default_required_tools apply.
	RequiredTools []string `json:"required_tools,omitempty"`
}

type QueuePromptResponse struct {
	InstanceID string                   `json:"instance_id"`
	OpID       string                   `json:"op_id"`
	Receipt    storage.OperationReceipt `json:"receipt"`
}

type ReplacePromptRequest struct {
	InstanceID      string `json:"instance_id"`
	OpID            string `json:"op_id"`
	ControllerLease string `json:"controller_lease"`
	ExpectedVersion int64  `json:"expected_version"`
	Prompt          string `json:"prompt"`
}

type ReplacePromptResponse struct {
	InstanceID string                   `json:"instance_id"`
	OpID       string                   `json:"op_id"`
	Receipt    storage.OperationReceipt `json:"receipt"`
}

type DiscardPromptRequest struct {
	InstanceID      string `json:"instance_id"`
	OpID            string `json:"op_id"`
	ControllerLease string `json:"controller_lease"`
	ExpectedVersion int64  `json:"expected_version"`
}

type DiscardPromptResponse struct {
	InstanceID string                   `json:"instance_id"`
	OpID       string                   `json:"op_id"`
	Receipt    storage.OperationReceipt `json:"receipt"`
}

type RecordDecisionRequest struct {
	InstanceID      string `json:"instance_id"`
	OpID            string `json:"op_id"`
	ControllerLease string `json:"controller_lease"`
	ArtifactID      string `json:"artifact_id"`
	Revision        int64  `json:"revision"`
	DecisionPayload string `json:"decision_payload"`
}

type RecordDecisionResponse struct {
	InstanceID string                   `json:"instance_id"`
	OpID       string                   `json:"op_id"`
	Receipt    storage.OperationReceipt `json:"receipt"`
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	turnKey := strings.TrimSpace(r.PathValue("turn_key"))

	if runID == "" || sessionID == "" || turnKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id, session_id, and turn_key are required", "")
		return
	}

	var req CancelRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}

	if err := s.store.ValidateSessionRun(r.Context(), sessionID, runID); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) || errors.Is(err, storage.ErrRunSessionMismatch) {
			writeError(w, http.StatusNotFound, "session_not_found", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), req.OpID)
		return
	}

	if req.OpID == "" || req.ControllerLease == "" || req.ExpectedVersion <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "op_id, controller_lease, and expected_version (>0) are required", req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	// New decisions require the current controller attached to this instance.
	if !s.requireConnectedController(w, r, runID, sessionID, req.ControllerLease, req.OpID) {
		return
	}

	stageOpID := fmt.Sprintf("%s:req", req.OpID)
	receipt, err := s.store.RequestCancel(r.Context(), stageOpID, req.ControllerLease, sessionID, req.ExpectedVersion, turnKey)
	if err != nil {
		if errors.Is(err, storage.ErrStaleUpdate) {
			writeError(w, http.StatusConflict, "stale_version", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		if writeControllerAuthError(w, err, req.OpID) {
			return
		}
		writeError(w, http.StatusBadRequest, "cancel_failed", err.Error(), req.OpID)
		return
	}

	turnRef := adapter.TurnRef{
		SessionID: adapter.SessionID(sessionID),
		TurnKey:   turnKey,
	}

	// Capture the accepted execution's identity before contacting the
	// adapter: the confirmed-commit evidence write validates exactly this
	// reference rather than a later lookup.
	execRef := s.executionRefForTurn(r.Context(), sessionID, turnKey)

	// Request cancellation through the adapter under an independent timeout.
	// The supervisor's observation, collection, and persistence
	// responsibilities stay alive independently of this request: a request,
	// unsupported, rejected, or unknown outcome is not termination, and the
	// native execution may still be running.
	cancellationStatus := "requested"
	if s.adapter != nil {
		cancelCtx, cancelTimeout := context.WithTimeout(context.Background(), 5*time.Second)
		outcome, err := s.adapter.Cancel(cancelCtx, turnRef)
		cancelTimeout()
		if err != nil {
			cancellationStatus = "unavailable"
		} else {
			switch outcome.Disposition {
			case adapter.CancelConfirmed:
				if outcome.Ref == turnRef {
					termOpID := fmt.Sprintf("%s:term", req.OpID)
					reason := outcome.Reason
					if reason == "" {
						reason = "cancelled by operator"
					}
					bgCtx, bgCancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer bgCancel()

					// The evidence write uses the identity captured before
					// the adapter call (AC-004 §6) — it must not depend on
					// the requesting controller's lease remaining current.
					persisted := false
					if execRef != nil {
						_, termErr := s.store.RecordObservedExecutionOutcome(bgCtx, termOpID, *execRef, council.TurnCancelled, reason)
						persisted = termErr == nil
					}
					// Resolve against durable state: the cancellation is
					// confirmed only when the turn is durably cancelled.
					if details, derr := s.store.GetTurnDetails(bgCtx, sessionID, turnKey); derr == nil && details != nil && details.Status == council.TurnCancelled {
						if !persisted {
							persisted = true
						}
						reason = details.Result
					}
					if persisted {
						cancellationStatus = "confirmed"
						// Notify existing SSE subscribers after the
						// authoritative command-driven terminal commit; the
						// supervisor's stream cannot be relied upon for this
						// notification.
						termBytes, _ := json.Marshal(map[string]any{
							"session_id": sessionID,
							"turn_key":   turnKey,
							"status":     council.TurnCancelled,
							"result":     reason,
						})
						s.coordinator.BroadcastEvent(turnRef, SSEEvent{
							Event: "terminal",
							Data:  string(termBytes),
						})
					} else {
						cancellationStatus = "unknown"
					}
				} else {
					cancellationStatus = "unknown"
				}
			case adapter.CancelAlreadyTerminal:
				cancellationStatus = "already_terminal"
			case adapter.CancelRejected:
				cancellationStatus = "rejected"
			case adapter.CancelUnsupported:
				cancellationStatus = "unsupported"
			case adapter.CancelRequested:
				cancellationStatus = "requested"
			case adapter.CancelUnknown:
				cancellationStatus = "unknown"
			default:
				cancellationStatus = string(outcome.Disposition)
			}
		}
	}

	// The response preserves the original committed request acceptance
	// receipt; the evolving cancellation status is reported separately and a
	// confirmed terminal commit is queryable on the turn resource.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(CancelResponse{
		InstanceID:         s.cfg.InstanceID,
		OpID:               req.OpID,
		Receipt:            receipt,
		CancellationStatus: cancellationStatus,
	})
}

func (s *Server) handleControllerConnect(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	sessionID := strings.TrimSpace(r.PathValue("session_id"))

	if runID == "" || sessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id and session_id are required", "")
		return
	}

	var req ControllerConnectRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}

	if err := s.store.ValidateSessionRun(r.Context(), sessionID, runID); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) || errors.Is(err, storage.ErrRunSessionMismatch) {
			writeError(w, http.StatusNotFound, "session_not_found", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), req.OpID)
		return
	}

	if req.OpID == "" || req.ControllerLease == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "op_id and controller_lease are required", req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	// Compatibility delegation (AC-004 §7): the session route establishes
	// the run-scoped attachment episode; session controller_status columns
	// are projections.
	rec, err := s.store.GetControllerRecord(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), req.OpID)
		return
	}
	if !rec.Adopted {
		writeError(w, http.StatusConflict, "adoption_required", "run has no adopted controller", req.OpID)
		return
	}
	connectReceipt, err := s.store.ConnectRunController(r.Context(), req.OpID, runID, req.ControllerLease, rec.Generation, s.cfg.InstanceID)
	if err != nil {
		s.writeGrantError(w, err, req.OpID)
		return
	}
	s.coordinator.MarkControllerAttached(runID, rec.Generation, connectReceipt.AttachmentID, s.cfg.InstanceID, connectReceipt.AttachmentRev)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ControllerConnectResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		Receipt:    connectReceipt.OperationReceipt,
	})
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	turnKey := strings.TrimSpace(r.PathValue("turn_key"))

	if runID == "" || sessionID == "" || turnKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id, session_id, and turn_key are required", "")
		return
	}

	var req ReconcileRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}

	if err := s.store.ValidateSessionRun(r.Context(), sessionID, runID); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) || errors.Is(err, storage.ErrRunSessionMismatch) {
			writeError(w, http.StatusNotFound, "session_not_found", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), req.OpID)
		return
	}

	if req.OpID == "" || req.ControllerLease == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "op_id and controller_lease are required", req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	// New decisions require the current controller attached to this instance.
	if !s.requireConnectedController(w, r, runID, sessionID, req.ControllerLease, req.OpID) {
		return
	}

	stageReconcileID := fmt.Sprintf("%s:reconcile", req.OpID)
	stageHostLossID := fmt.Sprintf("%s:host_loss", req.OpID)

	// Step 1: Current controller authority precedes receipt replay
	// (AC-004): a superseded controller cannot recover a committed
	// reconcile response.
	if err := s.store.ValidateControllerLease(r.Context(), sessionID, req.ControllerLease); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "session_not_found", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrLeaseSuperseded) {
			writeError(w, http.StatusForbidden, "lease_superseded", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrAdoptionRequired) {
			writeError(w, http.StatusConflict, "adoption_required", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), req.OpID)
		return
	}

	// Step 2: Check completed stage next.
	committedReceipt, found, err := s.store.FindOperationReceipt(r.Context(), stageReconcileID, req.ControllerLease)
	if err != nil {
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, "idempotency_conflict", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusBadRequest, "check_reconcile_failed", err.Error(), req.OpID)
		return
	}
	if found {
		if committedReceipt.SessionID != sessionID || (committedReceipt.TurnKey != "" && committedReceipt.TurnKey != turnKey) || committedReceipt.CommandType != "reconcile_session" {
			writeError(w, http.StatusConflict, "idempotency_conflict", "existing receipt targets different command, session, or turn", req.OpID)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ReconcileResponse{
			InstanceID: s.cfg.InstanceID,
			OpID:       req.OpID,
			Receipt:    *committedReceipt,
		})
		return
	}

	// Step 3: Load current recovery state for turn-scope authorization.
	recState, err := s.store.GetSessionRecoveryState(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get_session_failed", err.Error(), req.OpID)
		return
	}
	// Authorize against the active execution OR the exact retained
	// post-terminal recovery reference. A turn whose terminal outcome was
	// recorded while host visibility was lost has an empty active key; its
	// recovery_context and active_recovery_gen retain the unresolved episode
	// that must be reconciled.
	activeMatch := recState.ActiveKey == turnKey
	retainedMatch := recState.ActiveKey == "" &&
		recState.Visibility == "host_lost" &&
		recState.RecoveryContext == turnKey &&
		recState.ActiveRecoveryGen > 0
	if !activeMatch && !retainedMatch {
		writeError(w, http.StatusBadRequest, "invalid_turn", fmt.Sprintf("session active turn is %q, not %q", recState.ActiveKey, turnKey), req.OpID)
		return
	}

	// Capture the original execution reference after turn-scope
	// authorization and before the probe: the atomic commit validates
	// exactly this identity.
	execRef := s.executionRefForTurn(r.Context(), sessionID, turnKey)
	if execRef == nil {
		writeError(w, http.StatusConflict, "no_accepted_execution", "no accepted execution reference for this turn", req.OpID)
		return
	}

	var generation uint64
	if recState.Visibility == "host_lost" && recState.ActiveRecoveryGen > 0 {
		generation = recState.ActiveRecoveryGen
	} else {
		expectedVer := req.ExpectedVersion
		if expectedVer <= 0 {
			expectedVer = recState.RowVersion
		}
		hlReceipt, err := s.store.RecordHostLoss(r.Context(), stageHostLossID, req.ControllerLease, sessionID, expectedVer)
		if err != nil {
			if errors.Is(err, storage.ErrStaleUpdate) {
				writeError(w, http.StatusConflict, "stale_version", err.Error(), req.OpID)
				return
			}
			if errors.Is(err, storage.ErrUnauthorizedOperation) {
				writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
				return
			}
			writeError(w, http.StatusBadRequest, "record_host_loss_failed", err.Error(), req.OpID)
			return
		}
		if _, err := fmt.Sscanf(hlReceipt.Payload, "%d", &generation); err != nil || generation == 0 {
			writeError(w, http.StatusInternalServerError, "invalid_recovery_gen", "failed to determine active recovery generation from host loss receipt", req.OpID)
			return
		}
	}

	// Step 4: Load the saved native-session binding and attach to the
	// original execution. Recovery must never substitute a fresh session.
	if s.adapter == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "harness adapter unavailable", req.OpID)
		return
	}
	binding, contributor, found, err := s.store.GetSessionNativeBinding(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get_binding_failed", err.Error(), req.OpID)
		return
	}
	if !found {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "no saved native session binding for recovery", req.OpID)
		return
	}
	sessionBinding, err := sessionBindingFromStorage(binding, contributor)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", err.Error(), req.OpID)
		return
	}
	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = s.adapter.ResumeSession(resumeCtx, sessionBinding)
	resumeCancel()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", fmt.Sprintf("cannot attach to saved native session: %v", err), req.OpID)
		return
	}

	recoveryRef := adapter.RecoveryRef{
		TurnRef: adapter.TurnRef{
			SessionID: adapter.SessionID(sessionID),
			TurnKey:   turnKey,
		},
		Generation: generation,
	}

	probeCtx, probeTimeout := context.WithTimeout(context.Background(), 10*time.Second)
	outcome, err := s.adapter.Reconcile(probeCtx, recoveryRef)
	probeTimeout()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "adapter_reconcile_failed", err.Error(), req.OpID)
		return
	}

	// Step 5: Persist reconciled outcome under bounded service context
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer persistCancel()
	recReceipt, err := s.store.ReconcileSession(persistCtx, stageReconcileID, *execRef, recoveryRef, outcome)
	if err != nil {
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, "idempotency_conflict", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusBadRequest, "persist_reconcile_failed", err.Error(), req.OpID)
		return
	}

	// Step 6: Notify terminal subscribers after any authoritative
	// command-driven terminal commit, not only supervisor-driven completion.
	if details, derr := s.store.GetTurnDetails(r.Context(), sessionID, turnKey); derr == nil && details != nil &&
		(details.Status == council.TurnCompleted || details.Status == council.TurnFailed || details.Status == council.TurnCancelled || details.Status == council.TurnInterrupted) {
		termBytes, _ := json.Marshal(map[string]any{
			"session_id": sessionID,
			"turn_key":   turnKey,
			"status":     details.Status,
			"result":     details.Result,
		})
		s.coordinator.BroadcastEvent(adapter.TurnRef{
			SessionID: adapter.SessionID(sessionID),
			TurnKey:   turnKey,
		}, SSEEvent{
			Event: "terminal",
			Data:  string(termBytes),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ReconcileResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		Receipt:    recReceipt,
	})
}

// ErrUnsupportedBindingConfig reports a persisted native-binding
// configuration that cannot be reconstructed faithfully for resume.
// Failing visibly is required: silently dropping configuration would
// restore a different session than the one that was saved.
var ErrUnsupportedBindingConfig = errors.New("unsupported native-binding configuration for resume")

// sessionBindingFromStorage reconstructs the adapter session binding from
// the persisted native binding and session contributor, preserving the
// saved workspace root, model, tooling, and native identity.
//
// The persisted tooling configuration may be empty, a JSON array of tool
// names, or a JSON object whose recognized session-config fields are
// workspace_root, model, and exactly one of tools/tooling (an array of tool
// names; both aliases present is ambiguous and rejected). Profile
// references (a bare identifier or a string/map tooling value) and other
// shapes cannot be expanded without a profile registry and are rejected
// with ErrUnsupportedBindingConfig.
//
// Resume reconstructs the supported adapter binding fields only. It does
// not, by itself, establish enforcement of stored read_only,
// env_allowlist, or other policy metadata: those keys are outside
// adapter.SessionConfig and require an explicit enforcement owner when
// native integrations use them.
// executionRefForTurn resolves the persisted execution reference (session,
// turn, attempt) for a nonterminal turn, or nil when none is recorded.
func (s *Server) executionRefForTurn(ctx context.Context, sessionID, turnKey string) *storage.ExecutionRef {
	details, err := s.store.GetTurnDetails(ctx, sessionID, turnKey)
	if err != nil || details == nil || details.DispatchIntent == nil {
		return nil
	}
	if details.DispatchIntent.AttemptID == "" {
		return nil
	}
	return &storage.ExecutionRef{
		SessionID: sessionID,
		TurnKey:   turnKey,
		AttemptID: details.DispatchIntent.AttemptID,
	}
}

func sessionBindingFromStorage(nb storage.NativeBinding, contributor string) (adapter.SessionBinding, error) {
	binding := adapter.SessionBinding{
		SessionID:       adapter.SessionID(nb.LogicalSessionID),
		Contributor:     council.Contributor(contributor),
		NativeSessionID: nb.NativeSessionID,
		Config: adapter.SessionConfig{
			Model: nb.Model,
		},
	}

	trimmed := strings.TrimSpace(nb.ToolingConfig)
	if trimmed == "" || trimmed == "{}" {
		return binding, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
		if raw, ok := obj["workspace_root"]; ok {
			var root string
			if err := json.Unmarshal(raw, &root); err != nil {
				return adapter.SessionBinding{}, fmt.Errorf("%w: workspace_root must be a string", ErrUnsupportedBindingConfig)
			}
			binding.Config.WorkspaceRoot = root
		}
		if raw, ok := obj["model"]; ok {
			var model string
			if err := json.Unmarshal(raw, &model); err != nil {
				return adapter.SessionBinding{}, fmt.Errorf("%w: model must be a string", ErrUnsupportedBindingConfig)
			}
			binding.Config.Model = model
		}
		// Exactly one tooling representation per object: accepting both
		// would let an unsupported profile reference or a conflicting
		// list disappear behind the other alias.
		toolsRaw, hasTools := obj["tools"]
		if _, hasTooling := obj["tooling"]; hasTooling {
			if hasTools {
				return adapter.SessionBinding{}, fmt.Errorf("%w: specify exactly one of tools or tooling", ErrUnsupportedBindingConfig)
			}
			toolsRaw = obj["tooling"]
			hasTools = true
		}
		if hasTools {
			tools, err := decodeToolList(toolsRaw)
			if err != nil {
				return adapter.SessionBinding{}, fmt.Errorf("%w: %v", ErrUnsupportedBindingConfig, err)
			}
			binding.Config.Tooling = tools
		}
		return binding, nil
	}

	// Legacy/alternate representation: a JSON array of tool names.
	tools, err := decodeToolList([]byte(trimmed))
	if err != nil {
		return adapter.SessionBinding{}, fmt.Errorf("%w: %v", ErrUnsupportedBindingConfig, err)
	}
	binding.Config.Tooling = tools
	return binding, nil
}

// decodeToolList decodes a JSON value into a tool-name list, rejecting
// profile strings and maps that cannot be faithfully expanded.
func decodeToolList(raw json.RawMessage) ([]string, error) {
	var tools []string
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, errors.New("tooling must be an array of tool names for resume")
	}
	return tools, nil
}

func (s *Server) handleQueuePrompt(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	sessionID := strings.TrimSpace(r.PathValue("session_id"))

	if runID == "" || sessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id and session_id are required", "")
		return
	}

	var req QueuePromptRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}

	if err := s.store.ValidateSessionRun(r.Context(), sessionID, runID); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) || errors.Is(err, storage.ErrRunSessionMismatch) {
			writeError(w, http.StatusNotFound, "session_not_found", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	// New decisions require the current controller attached to this instance.
	if !s.requireConnectedController(w, r, runID, sessionID, req.ControllerLease, req.OpID) {
		return
	}

	if err := s.validateQueuedRequiredTools(r.Context(), sessionID, req.RequiredTools); err != nil {
		status, code := queuedRequiredToolsErrorStatus(err)
		writeError(w, status, code, err.Error(), req.OpID)
		return
	}

	receipt, err := s.store.QueuePrompt(r.Context(), req.OpID, req.ControllerLease, sessionID, req.ExpectedVersion, storage.PendingPrompt{
		SessionID:     sessionID,
		TurnKey:       req.TurnKey,
		Prompt:        req.Prompt,
		RequiredTools: req.RequiredTools,
	})
	if err != nil {
		if errors.Is(err, storage.ErrStaleUpdate) {
			writeError(w, http.StatusConflict, "stale_version", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		if writeControllerAuthError(w, err, req.OpID) {
			return
		}
		writeError(w, http.StatusBadRequest, "queue_failed", err.Error(), req.OpID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(QueuePromptResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		Receipt:    receipt,
	})
}

// queuedRequiredToolsErrorStatus maps a validateQueuedRequiredTools
// failure to its HTTP status: a caller error is 400
// invalid_required_tools; a session (or its run) that vanished between
// the path check and the validation is the same 404 session_not_found
// the queue path returns for an unknown session; anything else is a
// storage error.
func queuedRequiredToolsErrorStatus(err error) (int, string) {
	var invalid *invalidRequiredToolsError
	switch {
	case errors.As(err, &invalid):
		return http.StatusBadRequest, "invalid_required_tools"
	case errors.Is(err, storage.ErrSessionNotFound), errors.Is(err, storage.ErrRunSessionMismatch), errors.Is(err, storage.ErrRunNotFound):
		return http.StatusNotFound, "session_not_found"
	default:
		return http.StatusInternalServerError, "storage_error"
	}
}

// invalidRequiredToolsError is a queue-time required_tools refusal (a
// caller error, HTTP 400 invalid_required_tools).
type invalidRequiredToolsError struct{ msg string }

func (e *invalidRequiredToolsError) Error() string { return e.msg }

// validateQueuedRequiredTools enforces the AC-010 §3.5 queue-time rule:
// required_tools is accepted only for an agy session; every name must be
// in the run's frozen harnesses.agy expected_tools (native tool names),
// and names must be non-empty and unique (the adapter refuses a
// duplicated set at dispatch, so it is refused here, before it can be
// journaled). Absent/empty needs no check.
func (s *Server) validateQueuedRequiredTools(ctx context.Context, sessionID string, tools []string) error {
	if len(tools) == 0 {
		return nil
	}
	meta, err := s.store.GetSessionMetadata(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session lookup: %w", err)
	}
	if meta.Contributor != string(council.Agy) {
		return &invalidRequiredToolsError{msg: fmt.Sprintf(
			"required_tools is only supported for agy sessions; session %s contributor is %q", sessionID, meta.Contributor)}
	}
	rec, err := s.store.GetRunProfile(ctx, meta.RunID)
	if err != nil {
		return fmt.Errorf("run profile lookup: %w", err)
	}
	spec, ok := rec.Profile.Harnesses["agy"]
	if !ok || spec.Agy == nil {
		return &invalidRequiredToolsError{msg: fmt.Sprintf(
			"run %s has no frozen agy harness block; required_tools cannot be validated", meta.RunID)}
	}
	expected := make(map[string]struct{}, len(spec.Agy.ExpectedTools))
	for _, t := range spec.Agy.ExpectedTools {
		expected[t] = struct{}{}
	}
	seen := make(map[string]struct{}, len(tools))
	var unknown, dups []string
	for _, t := range tools {
		if _, dup := seen[t]; dup {
			dups = append(dups, t)
			continue
		}
		seen[t] = struct{}{}
		if _, ok := expected[t]; !ok || strings.TrimSpace(t) == "" {
			unknown = append(unknown, fmt.Sprintf("%q", t))
		}
	}
	switch {
	case len(unknown) > 0:
		return &invalidRequiredToolsError{msg: fmt.Sprintf(
			"required_tools names not in the run's frozen expected_tools: %s", strings.Join(unknown, ", "))}
	case len(dups) > 0:
		return &invalidRequiredToolsError{msg: fmt.Sprintf(
			"required_tools lists duplicate names: %s", strings.Join(dups, ", "))}
	}
	return nil
}

func (s *Server) handleReplacePrompt(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	turnKey := strings.TrimSpace(r.PathValue("turn_key"))

	if runID == "" || sessionID == "" || turnKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id, session_id, and turn_key are required", "")
		return
	}

	var req ReplacePromptRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}

	if err := s.store.ValidateSessionRun(r.Context(), sessionID, runID); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) || errors.Is(err, storage.ErrRunSessionMismatch) {
			writeError(w, http.StatusNotFound, "session_not_found", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	// New decisions require the current controller attached to this instance.
	if !s.requireConnectedController(w, r, runID, sessionID, req.ControllerLease, req.OpID) {
		return
	}

	receipt, err := s.store.ReplacePendingPrompt(r.Context(), req.OpID, req.ControllerLease, sessionID, req.ExpectedVersion, storage.PendingPrompt{
		SessionID: sessionID,
		TurnKey:   turnKey,
		Prompt:    req.Prompt,
	})
	if err != nil {
		if errors.Is(err, storage.ErrStaleUpdate) {
			writeError(w, http.StatusConflict, "stale_version", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		if writeControllerAuthError(w, err, req.OpID) {
			return
		}
		writeError(w, http.StatusBadRequest, "replace_failed", err.Error(), req.OpID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ReplacePromptResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		Receipt:    receipt,
	})
}

func (s *Server) handleDiscardPrompt(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	turnKey := strings.TrimSpace(r.PathValue("turn_key"))

	if runID == "" || sessionID == "" || turnKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id, session_id, and turn_key are required", "")
		return
	}

	var req DiscardPromptRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}

	if err := s.store.ValidateSessionRun(r.Context(), sessionID, runID); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) || errors.Is(err, storage.ErrRunSessionMismatch) {
			writeError(w, http.StatusNotFound, "session_not_found", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	// New decisions require the current controller attached to this instance.
	if !s.requireConnectedController(w, r, runID, sessionID, req.ControllerLease, req.OpID) {
		return
	}

	receipt, err := s.store.DiscardPendingPrompt(r.Context(), req.OpID, req.ControllerLease, sessionID, req.ExpectedVersion, turnKey)
	if err != nil {
		if errors.Is(err, storage.ErrStaleUpdate) {
			writeError(w, http.StatusConflict, "stale_version", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		if writeControllerAuthError(w, err, req.OpID) {
			return
		}
		writeError(w, http.StatusBadRequest, "discard_failed", err.Error(), req.OpID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(DiscardPromptResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		Receipt:    receipt,
	})
}

func (s *Server) handleRecordDecision(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id is required", "")
		return
	}

	var req RecordDecisionRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}

	if req.OpID == "" || req.ControllerLease == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "op_id and controller_lease are required", req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	// New decisions require the current controller attached to this instance.
	if !s.requireConnectedController(w, r, runID, "", req.ControllerLease, req.OpID) {
		return
	}

	receipt, err := s.store.RecordDecision(r.Context(), req.OpID, req.ControllerLease, runID, req.ArtifactID, req.Revision, req.DecisionPayload)
	if err != nil {
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrArtifactNotFound) {
			writeError(w, http.StatusNotFound, "artifact_not_found", err.Error(), req.OpID)
			return
		}
		if writeControllerAuthError(w, err, req.OpID) {
			return
		}
		writeError(w, http.StatusBadRequest, "decision_failed", err.Error(), req.OpID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(RecordDecisionResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		Receipt:    receipt,
	})
}
