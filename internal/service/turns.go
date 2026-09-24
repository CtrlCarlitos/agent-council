package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func (s *Server) handleGetTurn(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	turnKey := strings.TrimSpace(r.PathValue("turn_key"))

	if runID == "" || sessionID == "" || turnKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id, session_id, and turn_key are required", "")
		return
	}

	if err := s.store.ValidateSessionRun(r.Context(), sessionID, runID); err != nil {
		if errors.Is(err, storage.ErrSessionNotFound) || errors.Is(err, storage.ErrRunSessionMismatch) {
			writeError(w, http.StatusNotFound, "session_not_found", err.Error(), "")
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), "")
		return
	}

	details, err := s.store.GetTurnDetails(r.Context(), sessionID, turnKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query_failed", err.Error(), "")
		return
	}
	if details == nil {
		writeError(w, http.StatusNotFound, "turn_not_found", "turn not found", "")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(details)
}
