package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ExecutionSupervisor supervises an individual turn's dispatch, observation,
// outcome collection, and terminal persistence under service ownership.
type ExecutionSupervisor struct {
	store                  *storage.Store
	adapter                adapter.Adapter
	coordinator            *Coordinator
	runID                  string
	sessionID              string
	turnKey                string
	callerLease            string
	initialExpectedVersion int64
	receipt                storage.ReleaseReceipt
	done                   func()
}

// NewExecutionSupervisor constructs a supervisor instance for an admitted turn execution.
func NewExecutionSupervisor(
	store *storage.Store,
	adp adapter.Adapter,
	coordinator *Coordinator,
	runID, sessionID, turnKey, callerLease string,
	initialExpectedVersion int64,
	receipt storage.ReleaseReceipt,
	done func(),
) *ExecutionSupervisor {
	return &ExecutionSupervisor{
		store:                  store,
		adapter:                adp,
		coordinator:            coordinator,
		runID:                  runID,
		sessionID:              sessionID,
		turnKey:                turnKey,
		callerLease:            callerLease,
		initialExpectedVersion: initialExpectedVersion,
		receipt:                receipt,
		done:                   done,
	}
}

// Run executes the supervisor pipeline to completion under the provided worker context.
// It decrements coordinator worker accounting only after the terminal outcome is committed.
func (s *ExecutionSupervisor) Run(ctx context.Context) {
	defer s.done()

	if ctx.Err() != nil {
		return
	}

	ref := adapter.TurnRef{
		SessionID: adapter.SessionID(s.sessionID),
		TurnKey:   s.turnKey,
	}

	prompt, err := s.store.GetTurnPrompt(ctx, s.sessionID, s.turnKey)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		// If prompt cannot be retrieved from store, record failure outcome
		_ = s.recordTerminalOutcomeWithRetry(ctx, council.TurnFailed, fmt.Sprintf("failed to read turn prompt: %v", err))
		return
	}

	outcome, err := s.adapter.Dispatch(ctx, ref, prompt)
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		if ctx.Err() != nil {
			return
		}
		if outcome.Status == adapter.DispatchRejected {
			reason := outcome.Reason
			if reason == "" && err != nil {
				reason = err.Error()
			}
			_ = s.recordTerminalOutcomeWithRetry(ctx, council.TurnFailed, reason)
			return
		}
		// DispatchUnknown or context error preserves reservation without fabricated completion
		return
	}

	// Dispatch accepted: record observation
	obsOpID := fmt.Sprintf("op-obs-acc-%s", s.turnKey)
	_, _ = s.store.RecordDispatchObservation(ctx, obsOpID, s.callerLease, s.sessionID, s.turnKey, "receipt_acknowledged")

	// Observe stream until completion
	stream, err := s.adapter.Observe(ctx, ref)
	if err == nil {
		for ev := range stream.Events() {
			if s.coordinator != nil {
				evBytes, _ := json.Marshal(ev)
				s.coordinator.BroadcastEvent(s.turnKey, SSEEvent{
					Event: "progress",
					Data:  string(evBytes),
				})
			}
		}
	}

	// If worker context was cancelled (grace expiry or forced exit),
	// do not fabricate terminal failure; preserve unconfirmed outcome as unresolved.
	if ctx.Err() != nil {
		return
	}

	// Collect authoritative outcome
	turnResult, err := s.adapter.Collect(ctx, ref)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		_ = s.recordTerminalOutcomeWithRetry(ctx, council.TurnFailed, fmt.Sprintf("collect failed: %v", err))
		return
	}

	err = s.recordTerminalOutcomeWithRetry(ctx, turnResult.Status, turnResult.Output)
	if err == nil && s.coordinator != nil {
		termBytes, _ := json.Marshal(map[string]any{
			"session_id": s.sessionID,
			"turn_key":   s.turnKey,
			"status":     turnResult.Status,
			"result":     turnResult.Output,
		})
		s.coordinator.BroadcastEvent(s.turnKey, SSEEvent{
			Event: "terminal",
			Data:  string(termBytes),
		})
	}
}

// recordTerminalOutcomeWithRetry persists the terminal outcome, resilient to concurrent
// session row_version advances.
func (s *ExecutionSupervisor) recordTerminalOutcomeWithRetry(ctx context.Context, status council.TurnStatus, rawResult string) error {
	if s.coordinator != nil {
		doneCommit := s.coordinator.TrackCommit()
		defer doneCommit()
	}

	expectedVer := s.receipt.CommittedVersion
	if expectedVer <= 0 {
		expectedVer = s.initialExpectedVersion
	}
	opID := fmt.Sprintf("op-term-%s", s.turnKey)

	dbCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for retries := 0; retries < 10; retries++ {
		latestVer, err := s.store.GetSessionVersion(dbCtx, s.sessionID)
		if err == nil && latestVer > 0 {
			expectedVer = latestVer
		}
		_, err = s.store.RecordTerminalOutcome(dbCtx, opID, s.callerLease, s.sessionID, expectedVer, s.turnKey, status, rawResult)
		if errors.Is(err, storage.ErrStaleUpdate) {
			continue
		}
		return err
	}
	return errors.New("exhausted version retries recording terminal outcome")
}
