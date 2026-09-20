package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// StopRequest defines the payload for POST /v1/service/stop.
type StopRequest struct {
	InstanceID string `json:"instance_id"`
	Drain      bool   `json:"drain"`
}

// StopResponse defines the response payload for POST /v1/service/stop.
type StopResponse struct {
	InstanceID string         `json:"instance_id"`
	Status     string         `json:"status"` // "stopping" or "draining"
	Counters   map[string]int `json:"counters,omitempty"`
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req StopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}

	if req.InstanceID != "" && req.InstanceID != s.cfg.InstanceID {
		writeError(w, http.StatusConflict, "instance_mismatch",
			fmt.Sprintf("instance_id %q does not match current instance %q", req.InstanceID, s.cfg.InstanceID), "")
		return
	}

	if !req.Drain {
		if !s.coordinator.IsShutdownEligible() {
			writeError(w, http.StatusConflict, "service_busy", "service has active executions or pending operations", "")
			return
		}

		s.coordinator.SetState(ServiceStateStopping)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(StopResponse{
			InstanceID: s.cfg.InstanceID,
			Status:     "stopping",
			Counters:   s.coordinator.ShutdownCounters(),
		})

		go func() {
			_ = s.Teardown(5 * time.Second)
		}()
		return
	}

	// Drain == true: close release admission gate and drain active executions
	s.coordinator.SetState(ServiceStateDraining)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(StopResponse{
		InstanceID: s.cfg.InstanceID,
		Status:     "draining",
		Counters:   s.coordinator.ShutdownCounters(),
	})

	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		timeout := time.After(30 * time.Second)
		for {
			select {
			case <-timeout:
				s.coordinator.CancelActiveWorkers()
				_ = s.Teardown(5 * time.Second)
				return
			case <-ticker.C:
				if s.coordinator.IsShutdownEligible() {
					_ = s.Teardown(5 * time.Second)
					return
				}
			}
		}
	}()
}

// Teardown performs an orderly shutdown sequence bounded by the provided timeout.
func (s *Server) Teardown(timeout time.Duration) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	s.mu.Unlock()

	// 1. Close command admission gate
	s.coordinator.SetState(ServiceStateStopping)

	// 2. Quiesce or terminate SSE streams
	s.coordinator.CloseAllSubscribers()

	// 3. HTTP Server Shutdown bounded by timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := s.httpServer.Shutdown(ctx)
	if s.listener != nil {
		_ = s.listener.Close()
	}

	// 4. Wait for worker tasks under bounded timeout
	workerDone := make(chan struct{})
	go func() {
		s.coordinator.WaitWorkers()
		close(workerDone)
	}()

	select {
	case <-workerDone:
	case <-ctx.Done():
		s.coordinator.Close()
	}

	// 5. Close storage store
	if s.store != nil {
		_ = s.store.Close()
	}

	// 6. Cleanup discovery files (unlink socket, token, metadata)
	_ = s.lock.CleanupDiscovery()

	// 7. Lock handle remains held until process exits (closed last)
	return err
}

// StartSignalHandler installs OS signal listeners for SIGINT and SIGTERM.
func (s *Server) StartSignalHandler() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case <-s.shutdown:
			signal.Stop(sigCh)
			return
		case <-sigCh:
			signal.Stop(sigCh)
			s.handleSignalGrace()
		}
	}()
}

func (s *Server) handleSignalGrace() {
	s.coordinator.SetState(ServiceStateDraining)

	graceTimer := time.NewTimer(15 * time.Second)
	defer graceTimer.Stop()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if s.coordinator.IsShutdownEligible() {
				_ = s.Teardown(5 * time.Second)
				return
			}
		case <-graceTimer.C:
			s.coordinator.CancelActiveWorkers()
			_ = s.Teardown(5 * time.Second)
			return
		case <-s.shutdown:
			return
		}
	}
}
