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
		writeError(w, http.StatusBadRequest, "cancel_failed", err.Error(), req.OpID)
		return
	}

	turnRef := adapter.TurnRef{
		SessionID: adapter.SessionID(sessionID),
		TurnKey:   turnKey,
	}

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

					persisted := false
					for retries := 0; retries < 5; retries++ {
						ver, verErr := s.store.GetSessionVersion(bgCtx, sessionID)
						if verErr != nil {
							ver = receipt.CommittedVersion
						}
						_, termErr := s.store.RecordTerminalOutcome(bgCtx, termOpID, req.ControllerLease, sessionID, ver, turnKey, council.TurnCancelled, reason)
						if termErr == nil {
							persisted = true
							break
						}
						if errors.Is(termErr, storage.ErrConflictingTerminalOutcome) {
							// Another authoritative committer (the
							// supervisor collecting the cancelled result)
							// recorded this outcome first.
							break
						}
						if !errors.Is(termErr, storage.ErrStaleUpdate) {
							break
						}
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

	receipt, err := s.store.SetControllerConnection(r.Context(), req.OpID, req.ControllerLease, sessionID, req.ExpectedVersion, council.ControllerConnected)
	if err != nil {
		if errors.Is(err, storage.ErrStaleUpdate) {
			writeError(w, http.StatusConflict, "stale_version", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusBadRequest, "connect_failed", err.Error(), req.OpID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ControllerConnectResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		Receipt:    receipt,
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

	stageReconcileID := fmt.Sprintf("%s:reconcile", req.OpID)
	stageHostLossID := fmt.Sprintf("%s:host_loss", req.OpID)

	// Step 1: Check completed stage first!
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

	// Step 2: Validate caller authority against runs.controller_lease and active turn before probes
	if err := s.store.ValidateControllerLease(r.Context(), sessionID, req.ControllerLease); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "session_not_found", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), req.OpID)
		return
	}

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

	// Step 3: Load the saved native-session binding and attach to the
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
	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = s.adapter.ResumeSession(resumeCtx, adapter.SessionBinding{
		SessionID:       adapter.SessionID(binding.LogicalSessionID),
		Contributor:     council.Contributor(contributor),
		NativeSessionID: binding.NativeSessionID,
		Config: adapter.SessionConfig{
			WorkspaceRoot: binding.WorkspaceMode,
			Model:         binding.Model,
			Tooling:       parseToolingConfig(binding.ToolingConfig),
		},
	})
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

	// Step 4: Persist reconciled outcome under bounded service context
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer persistCancel()
	recReceipt, err := s.store.ReconcileSession(persistCtx, stageReconcileID, req.ControllerLease, recoveryRef, outcome)
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

	// Step 5: Notify terminal subscribers after any authoritative
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

// parseToolingConfig decodes the stored tooling configuration into the
// adapter's tool list, tolerating an empty configuration.
func parseToolingConfig(cfgJSON string) []string {
	if strings.TrimSpace(cfgJSON) == "" {
		return nil
	}
	var tools []string
	if err := json.Unmarshal([]byte(cfgJSON), &tools); err != nil {
		return nil
	}
	return tools
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

	receipt, err := s.store.QueuePrompt(r.Context(), req.OpID, req.ControllerLease, sessionID, req.ExpectedVersion, storage.PendingPrompt{
		SessionID: sessionID,
		TurnKey:   req.TurnKey,
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
