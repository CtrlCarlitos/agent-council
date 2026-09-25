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

	attemptID := s.receipt.AttemptID
	if attemptID == "" {
		attemptID = "1"
	}

	execRef := storage.ExecutionRef{
		SessionID: s.sessionID,
		TurnKey:   s.turnKey,
		AttemptID: attemptID,
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
		if err := s.recordTerminalOutcomeWithRetry(ctx, council.TurnFailed, fmt.Sprintf("failed to read turn prompt: %v", err)); err != nil {
			if s.coordinator != nil {
				s.coordinator.AddRecoveryBlocker()
			}
		}
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
			if err := s.recordTerminalOutcomeWithRetry(ctx, council.TurnFailed, reason); err != nil {
				if s.coordinator != nil {
					s.coordinator.AddRecoveryBlocker()
				}
			}
			return
		}
		// DispatchUnknown or transport error: record uncertainty observation durably without fabricated completion
		obsOpID := fmt.Sprintf("op-obs-%s-%s-%s", s.sessionID, s.turnKey, attemptID)
		_, _ = s.store.RecordObservedDispatchAcknowledgement(ctx, obsOpID, execRef, "acceptance_unknown")
		if s.coordinator != nil {
			s.coordinator.AddRecoveryBlocker()
		}
		return
	}

	// Dispatch accepted: record observation under the execution reference —
	// the acknowledgement never depends on the issuing controller's lease.
	obsOpID := fmt.Sprintf("op-obs-%s-%s-%s", s.sessionID, s.turnKey, attemptID)
	_, _ = s.store.RecordObservedDispatchAcknowledgement(ctx, obsOpID, execRef, "receipt_acknowledged")

	// Observe stream until completion. The worker context is the termination
	// scope only; a cancellation request does not cancel it, so observation,
	// collection, and persistence stay alive while the native execution may
	// still be running (cancel requested, unsupported, rejected, or unknown).
	stream, err := s.adapter.Observe(ctx, ref)
	if err == nil {
		for ev := range stream.Events() {
			if ev.Type == adapter.EventTerminal {
				// Authoritative terminal event is emitted only after durable database commit
				continue
			}
			if s.coordinator != nil {
				evBytes, _ := json.Marshal(ev)
				s.coordinator.BroadcastEvent(ref, SSEEvent{
					Event: "progress",
					Data:  string(evBytes),
				})
			}
		}
	}

	// If the service is terminating (grace expiry or forced exit), do not
	// fabricate terminal failure; preserve the unconfirmed outcome as
	// unresolved. This is the only ctx-cancelled exit path.
	if ctx.Err() != nil {
		return
	}

	// Collect the authoritative outcome. Collect may report a pending result
	// while the native execution is still finishing (for example after a
	// cancellation request that the adapter could not confirm), so poll until
	// a terminal result is verified or the service terminates.
	turnResult, err := s.collectUntilTerminal(ctx, ref)
	if err != nil {
		if ctx.Err() != nil || s.turnAlreadyCommittedTerminal() {
			return
		}
		// Transport or collection error must NOT record TurnFailed; preserve uncertainty
		if s.coordinator != nil {
			s.coordinator.AddRecoveryBlocker()
		}
		return
	}

	// Validate execution result ref matches requested turn ref
	if turnResult.Ref.SessionID != ref.SessionID || turnResult.Ref.TurnKey != ref.TurnKey {
		// Foreign execution result: quarantine and do not commit to this turn
		if s.coordinator != nil {
			s.coordinator.AddRecoveryBlocker()
		}
		return
	}

	err = s.recordTerminalOutcomeWithRetry(ctx, turnResult.Status, turnResult.Output)
	if err != nil {
		// A command-driven path (confirmed cancellation, reconciliation) may
		// have committed the terminal outcome concurrently. Durable terminal
		// state means the outcome was recorded authoritatively: not a blocker.
		if s.turnAlreadyCommittedTerminal() {
			return
		}
		if s.coordinator != nil {
			s.coordinator.AddRecoveryBlocker()
		}
		return
	}

	if s.coordinator != nil {
		termBytes, _ := json.Marshal(map[string]any{
			"session_id": s.sessionID,
			"turn_key":   s.turnKey,
			"status":     turnResult.Status,
			"result":     turnResult.Output,
		})
		s.coordinator.BroadcastEvent(ref, SSEEvent{
			Event: "terminal",
			Data:  string(termBytes),
		})
	}
}

// recordTerminalOutcomeWithRetry persists the terminal outcome under the
// execution's original identity (no controller-lease comparison — the
// issuing controller's authority was consumed at release time), resilient
// to concurrent session row_version advances.
func (s *ExecutionSupervisor) recordTerminalOutcomeWithRetry(ctx context.Context, status council.TurnStatus, rawResult string) error {
	if s.coordinator != nil {
		doneCommit := s.coordinator.TrackCommit()
		defer doneCommit()
	}

	attemptID := s.receipt.AttemptID
	if attemptID == "" {
		attemptID = "1"
	}
	execRef := storage.ExecutionRef{
		SessionID: s.sessionID,
		TurnKey:   s.turnKey,
		AttemptID: attemptID,
	}
	opID := fmt.Sprintf("op-term-%s-%s-%s", s.sessionID, s.turnKey, attemptID)

	dbCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for retries := 0; retries < 10; retries++ {
		_, err := s.store.RecordObservedExecutionOutcome(dbCtx, opID, execRef, status, rawResult)
		// The observed-outcome operation reads authoritative state itself;
		// retries cover transient write contention only.
		if err != nil && !errors.Is(err, storage.ErrWrongExecutionAttempt) {
			return err
		}
		if err == nil {
			return nil
		}
	}
	return errors.New("exhausted retries recording observed execution outcome")
}

// collectUntilTerminal polls the adapter until it verifies a terminal result
// or the termination scope ctx is cancelled. A pending result (execution
// still finishing) is not an error.
func (s *ExecutionSupervisor) collectUntilTerminal(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	for {
		if ctx.Err() != nil {
			return adapter.TurnResult{}, ctx.Err()
		}
		// A controller can retire an uncertain attempt without fabricating
		// a native result. Stop observing after that durable transition.
		if s.turnAlreadyCommittedTerminal() {
			return adapter.TurnResult{}, errors.New("Council turn already resolved")
		}
		res, err := s.adapter.Collect(ctx, ref)
		if err != nil {
			return adapter.TurnResult{}, err
		}
		if isTerminalTurnStatus(res.Status) && res.ResultStatus != adapter.ResultPending {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return adapter.TurnResult{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// turnAlreadyCommittedTerminal reports whether the turn already has a
// durable terminal outcome recorded by another authoritative path.
func (s *ExecutionSupervisor) turnAlreadyCommittedTerminal() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	details, err := s.store.GetTurnDetails(ctx, s.sessionID, s.turnKey)
	if err != nil || details == nil {
		return false
	}
	return isTerminalTurnStatus(details.Status)
}

func isTerminalTurnStatus(status council.TurnStatus) bool {
	switch status {
	case council.TurnCompleted, council.TurnFailed, council.TurnCancelled, council.TurnInterrupted:
		return true
	default:
		return false
	}
}
