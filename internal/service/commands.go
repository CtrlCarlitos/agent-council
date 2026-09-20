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

	// Signal coordinator worker context cancellation
	s.coordinator.CancelWorker(turnRef)

	// Call adapter Cancel under independent timeout
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
						termReceipt, termErr := s.store.RecordTerminalOutcome(bgCtx, termOpID, req.ControllerLease, sessionID, ver, turnKey, council.TurnCancelled, reason)
						if termErr == nil {
							receipt = termReceipt
							persisted = true
							break
						}
						if !errors.Is(termErr, storage.ErrStaleUpdate) {
							break
						}
					}
					if persisted {
						cancellationStatus = "confirmed"
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
	if recState.ActiveKey != turnKey {
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

	// Step 3: Probe adapter Reconcile
	if s.adapter == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "harness adapter unavailable", req.OpID)
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

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ReconcileResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		Receipt:    recReceipt,
	})
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
