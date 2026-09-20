package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	turnKey := strings.TrimSpace(r.PathValue("turn_key"))

	if runID == "" || sessionID == "" || turnKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id, session_id, and turn_key are required", "")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "streaming unsupported", "")
		return
	}

	// 1. Atomic Snapshot & Subscription Registration
	// Register subscriber first so no events emitted after snapshot are missed
	subCh, unsub := s.coordinator.RegisterSubscriber(turnKey)
	defer unsub()

	// Query authoritative current state from store
	details, err := s.store.GetTurnDetails(r.Context(), sessionID, turnKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query_failed", err.Error(), "")
		return
	}
	if details == nil {
		writeError(w, http.StatusNotFound, "turn_not_found", "turn not found", "")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Emit initial state snapshot event
	snapBytes, _ := json.Marshal(map[string]any{
		"session_id": sessionID,
		"turn_key":   turnKey,
		"status":     details.Status,
		"result":     details.Result,
	})
	fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", string(snapBytes))
	flusher.Flush()

	// If already terminal, emit terminal event and cleanly close
	if details.Status == council.TurnCompleted || details.Status == council.TurnFailed || details.Status == council.TurnCancelled {
		termBytes, _ := json.Marshal(map[string]any{
			"session_id": sessionID,
			"turn_key":   turnKey,
			"status":     details.Status,
			"result":     details.Result,
		})
		fmt.Fprintf(w, "event: terminal\ndata: %s\n\n", string(termBytes))
		flusher.Flush()
		return
	}

	// Event streaming loop with bounded write handling
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.coordinator.Context().Done():
			return
		case ev, ok := <-subCh:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, ev.Data)
			flusher.Flush()
			if ev.Event == "terminal" {
				return
			}
		}
	}
}
