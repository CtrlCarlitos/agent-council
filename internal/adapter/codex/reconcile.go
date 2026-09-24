package codex

// Reconciliation evidence rules for the codex adapter (AC-009 spec
// §3.10). The verdict is about the EXACT turn — baseline + prompt digest
// + in-life turn id — never the bare thread:
//
//   - a verified in-life turn/completed|failed terminal commits the
//     durable outcome (ReachableTerminal);
//   - in protected mode, a rollout task_complete matching this attempt's
//     baseline and prompt digest (ordered §3.7 correlation incl. the
//     turn_context pin check) reconstructs the terminal
//     (ReachableTerminal, acceptance upgraded durably);
//   - protected-mode VERIFIED ABSENCE — baseline unchanged AND the child
//     known dead — records the positive never-accepted decision
//     (RecordCodexAbsenceVerified) and authorizes exactly ONE
//     same-attempt redispatch through the existing ReserveCodexLaunch
//     preconditions; the redispatch execution itself is the existing
//     dispatch path;
//   - only a recorded pre-acceptance rejection (observed_status
//     "missing") is DefinitivelyMissing;
//   - everything else — child unreachable, ambiguous idle, advisory-mode
//     rollout signals, resume liveness — stays Uncertain, permanently,
//     until the controller records a disposition. Probes are separate
//     attempts: Reconcile never dispatches.

import (
	"context"
	"encoding/json"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Reconcile classifies a recovered turn from durable evidence only
// (spec §3.10). RecoveryRef comes from the caller and is echoed back
// verbatim — never invented or substituted.
func (a *CodexAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	a.mu.Lock()
	_, live := a.turns[ref.TurnRef]
	a.mu.Unlock()
	if live {
		return adapter.ReconciliationOutcome{Ref: ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableActive,
			Observed:     council.TurnRunning,
		}, nil
	}

	attempt, err := a.store.GetLatestCodexTurnAttempt(ctx, string(ref.TurnRef.SessionID), ref.TurnRef.TurnKey)
	if err != nil || attempt == nil {
		return uncertainReconciliation(ref), nil
	}

	// §3.10 row 1: the verified in-life terminal is committed durable
	// state — the outcome stands regardless of the child.
	if attempt.Terminal {
		return terminalReconciliation(ref, attempt), nil
	}

	// §3.10 rows 2–3 require protected mode with the rollout at its
	// recorded path.
	if attempt.RolloutProtection == "protected" {
		if outcome, decided := a.reconcileProtected(ctx, ref, attempt); decided {
			return outcome, nil
		}
	}

	// Positive pre-acceptance evidence: the native side definitively
	// refused the turn before any prompt transmission (or the launch
	// never started) — DefinitivelyMissing, never uncertain-blocking.
	if attempt.ObservedStatus == "missing" {
		return adapter.ReconciliationOutcome{Ref: ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationDefinitivelyMissing,
			Observed:     council.TurnFailed,
		}, nil
	}

	// Anything else — reserved/started launches, advisory-mode rollout
	// signals, unreachable children — stays Uncertain, permanently, and
	// blocks the native session until the controller records a
	// disposition.
	return uncertainReconciliation(ref), nil
}

// reconcileProtected applies the two protected-mode §3.10 rows. decided
// reports whether a verdict was reached.
func (a *CodexAdapter) reconcileProtected(ctx context.Context, ref adapter.RecoveryRef, attempt *storage.CodexTurnAttempt) (adapter.ReconciliationOutcome, bool) {
	binding, err := a.store.GetCodexSessionBinding(ctx, string(ref.TurnRef.SessionID))
	if err != nil || binding == nil || !binding.Materialized || binding.RolloutPath == nil {
		return adapter.ReconciliationOutcome{}, false
	}

	// Verified absence first: baseline unchanged (file identity, byte
	// size, entry count) AND the child known dead proves the rollout
	// absorbed nothing from this attempt — nothing was accepted.
	if states, err := a.store.CodexAttemptLaunchStates(ctx, attempt.AttemptID); err == nil && len(states) > 0 && allLaunchesDead(states) {
		if identity, size, entries, berr := scanRolloutBaseline(*binding.RolloutPath, binding.NativeID); berr == nil &&
			identity == attempt.BaselineIdentity && size == attempt.BaselineSize && entries == attempt.BaselineEntries {
			if err := a.store.RecordCodexAbsenceVerified(ctx, attempt.AttemptID); err != nil {
				// Absence verification failed (non-protected attempt,
				// missing attestation row, or already verified): the
				// conservative verdict stands.
				return uncertainReconciliation(ref), true
			}
			// The one-same-attempt redispatch is authorized durably
			// (absence_verified + ReserveCodexLaunch preconditions);
			// execution goes through the existing dispatch path. The
			// attempt itself is positively never-accepted.
			return adapter.ReconciliationOutcome{Ref: ref,
				Reachability: council.VisibilityReachable,
				Status:       adapter.ReconciliationDefinitivelyMissing,
				Observed:     council.TurnFailed,
				Result:       "verified absence: rollout baseline unchanged, child known dead; one same-attempt redispatch authorized",
			}, true
		}
	}

	// Rollout reconstruction: the §3.7 ordered correlation over the tail
	// past the recorded baseline — turn_context pins, pdig match, then
	// the matching task_complete (error absent ⇒ completed, populated ⇒
	// failed).
	entries, err := readRolloutTail(*binding.RolloutPath, attempt.BaselineSize)
	if err != nil || len(entries) == 0 {
		return adapter.ReconciliationOutcome{}, false
	}
	pins, err := RolloutPinsFor(a.policy, binding.Model, binding.Workspace)
	if err != nil {
		return adapter.ReconciliationOutcome{}, false
	}
	ev, err := CorrelateRolloutTurn(entries, pins, binding.NativeID, ref.TurnRef.TurnKey, attempt.AttemptID, attempt.PromptDigest)
	if err != nil || ev.Drift != nil || !ev.TurnContextMatch || !ev.PromptDigestMatch || ev.TaskComplete == nil {
		// Advisory-grade or incomplete signals never upgrade evidence.
		return adapter.ReconciliationOutcome{}, false
	}

	observed := "completed"
	payload := map[string]any{"last_agent_message": ev.TaskComplete.LastAgentMessage}
	if ev.TaskComplete.Failed {
		observed = "failed"
		payload = map[string]any{"task_complete_error": ev.TaskComplete.ErrorMessage}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return adapter.ReconciliationOutcome{}, false
	}
	rawStr := string(raw)
	if err := a.store.SetCodexAttemptTerminal(ctx, attempt.AttemptID, observed, rawStr, ""); err != nil {
		return uncertainReconciliation(ref), true
	}
	if err := a.store.SetCodexAttemptAccepted(ctx, attempt.AttemptID); err != nil {
		// The terminal IS committed durably — that is the truth, even if
		// the acceptance upgrade could not be recorded. Re-read the
		// committed state and report the terminal; never contradict
		// durable state with an Uncertain verdict.
		committed, rerr := a.store.GetCodexTurnAttempt(ctx, attempt.AttemptID)
		if rerr != nil || committed == nil || !committed.Terminal {
			return uncertainReconciliation(ref), true
		}
		return terminalReconciliation(ref, committed), true
	}
	outcome := terminalReconciliation(ref, &storage.CodexTurnAttempt{ObservedStatus: observed, ResultPayload: &rawStr})
	return outcome, true
}

// allLaunchesDead reports whether every launch reservation reached the
// durably known-dead state — the child-death evidence verified absence
// requires.
func allLaunchesDead(states []string) bool {
	for _, s := range states {
		if s != "dead" {
			return false
		}
	}
	return true
}

// terminalReconciliation renders a committed terminal as
// ReachableTerminal with the mapped council status.
func terminalReconciliation(ref adapter.RecoveryRef, attempt *storage.CodexTurnAttempt) adapter.ReconciliationOutcome {
	observed := council.TurnCompleted
	switch attempt.ObservedStatus {
	case "failed":
		observed = council.TurnFailed
	case "interrupted":
		observed = council.TurnCancelled
	}
	return adapter.ReconciliationOutcome{Ref: ref,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableTerminal,
		Observed:     observed,
		Result:       ptrStr(attempt.ResultPayload),
	}
}

func uncertainReconciliation(ref adapter.RecoveryRef) adapter.ReconciliationOutcome {
	return adapter.ReconciliationOutcome{Ref: ref,
		Reachability: council.VisibilityHostLost,
		Status:       adapter.ReconciliationUncertain,
		Observed:     council.TurnRunning,
	}
}
