package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type RunRecord struct {
	RunID           string
	BriefDigest     string
	SourceDigest    string
	ProfileDigest   string
	ControllerLease string
	Lifecycle       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type TurnRecord struct {
	SessionID   string
	TurnKey     string
	Prompt      string
	Status      string
	Result      string
	AttemptID   string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

type DispatchIntentRecord struct {
	SessionID  string
	TurnKey    string
	AttemptID  string
	Phase      string
	RecordedAt time.Time
	UpdatedAt  time.Time
}

type HydratedSession struct {
	SessionRecord
	Lifecycle         string                          `json:"lifecycle"`
	ControllerStatus  string                          `json:"controller_status"`
	RecoveryContext   string                          `json:"recovery_context"`
	ActiveRecoveryGen uint64                          `json:"active_recovery_gen"`
	RowVersion        int64                           `json:"row_version"`
	PendingPrompts    map[string]PendingPrompt        `json:"pending_prompts"`
	Turns             map[string]TurnRecord           `json:"turns"`
	DispatchIntents   map[string]DispatchIntentRecord `json:"dispatch_intents"`
	NativeBinding     *NativeBinding                  `json:"native_binding,omitempty"`
	ActiveTurn        *TurnRecord                     `json:"active_turn,omitempty"`
	ActiveIntent      *DispatchIntentRecord           `json:"active_intent,omitempty"`
}

type HydratedState struct {
	Runs     map[string]RunRecord
	Sessions map[string]HydratedSession
	Journals []OperationReceipt
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, s)
	}
	return t
}

func (s *Store) HydrateState(ctx context.Context) (*HydratedState, error) {
	state := &HydratedState{
		Runs:     make(map[string]RunRecord),
		Sessions: make(map[string]HydratedSession),
		Journals: make([]OperationReceipt, 0),
	}

	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin read transaction for hydration: %w", err)
	}
	defer tx.Rollback()

	// 1. Load runs
	runRows, err := tx.QueryContext(ctx, `
SELECT run_id, brief_digest, source_digest, profile_digest, controller_lease, lifecycle, created_at, updated_at
FROM runs;`)
	if err != nil {
		return nil, fmt.Errorf("hydrate runs: %w", err)
	}
	defer runRows.Close()

	for runRows.Next() {
		var r RunRecord
		var cAt, uAt string
		if err := runRows.Scan(&r.RunID, &r.BriefDigest, &r.SourceDigest, &r.ProfileDigest, &r.ControllerLease, &r.Lifecycle, &cAt, &uAt); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		r.CreatedAt = parseTime(cAt)
		r.UpdatedAt = parseTime(uAt)
		state.Runs[r.RunID] = r
	}
	if err := runRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}

	// 2. Load sessions
	sessRows, err := tx.QueryContext(ctx, `
SELECT session_id, run_id, contributor, is_active_contributor, state, lifecycle, controller_status, visibility, active_key, coalesce(recovery_context, ''), recovery_gen, active_recovery_gen, row_version
FROM sessions;`)
	if err != nil {
		return nil, fmt.Errorf("hydrate sessions: %w", err)
	}
	defer sessRows.Close()

	for sessRows.Next() {
		var hs HydratedSession
		var isActive int
		var activeKey sql.NullString
		if err := sessRows.Scan(&hs.ID, &hs.RunID, &hs.Contributor, &isActive, &hs.State, &hs.Lifecycle, &hs.ControllerStatus, &hs.Visibility, &activeKey, &hs.RecoveryContext, &hs.RecoveryGeneration, &hs.ActiveRecoveryGen, &hs.RowVersion); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		hs.IsActiveContributor = (isActive == 1)
		if activeKey.Valid {
			k := activeKey.String
			hs.ActiveTurnKey = &k
		}
		hs.PendingPrompts = make(map[string]PendingPrompt)
		hs.Turns = make(map[string]TurnRecord)
		hs.DispatchIntents = make(map[string]DispatchIntentRecord)
		state.Sessions[hs.ID] = hs
	}
	if err := sessRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}

	// 3. Load native bindings
	bindRows, err := tx.QueryContext(ctx, `
SELECT session_id, native_session_id, harness, model, workspace_mode, config_json FROM native_bindings;`)
	if err != nil {
		return nil, fmt.Errorf("hydrate native bindings: %w", err)
	}
	defer bindRows.Close()

	for bindRows.Next() {
		var b NativeBinding
		if err := bindRows.Scan(&b.LogicalSessionID, &b.NativeSessionID, &b.Harness, &b.Model, &b.WorkspaceMode, &b.ToolingConfig); err != nil {
			return nil, fmt.Errorf("scan native binding: %w", err)
		}
		if hs, exists := state.Sessions[b.LogicalSessionID]; exists {
			hs.NativeBinding = &b
			state.Sessions[b.LogicalSessionID] = hs
		}
	}
	if err := bindRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate native bindings: %w", err)
	}

	// 4. Load pending prompts
	promptRows, err := tx.QueryContext(ctx, `
SELECT session_id, turn_key, prompt, queued_at FROM pending_prompts;`)
	if err != nil {
		return nil, fmt.Errorf("hydrate pending prompts: %w", err)
	}
	defer promptRows.Close()

	for promptRows.Next() {
		var p PendingPrompt
		var turnKey, qAt string
		if err := promptRows.Scan(&p.SessionID, &turnKey, &p.Prompt, &qAt); err != nil {
			return nil, fmt.Errorf("scan pending prompt: %w", err)
		}
		p.TurnKey = turnKey
		p.CreatedAt = parseTime(qAt)
		if hs, exists := state.Sessions[p.SessionID]; exists {
			hs.PendingPrompts[turnKey] = p
			state.Sessions[p.SessionID] = hs
		}
	}
	if err := promptRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending prompts: %w", err)
	}

	// 5. Load turns
	turnRows, err := tx.QueryContext(ctx, `
SELECT session_id, turn_key, prompt, status, result, attempt_id, created_at, completed_at FROM turns;`)
	if err != nil {
		return nil, fmt.Errorf("hydrate turns: %w", err)
	}
	defer turnRows.Close()

	for turnRows.Next() {
		var t TurnRecord
		var cAt string
		var compAt sql.NullString
		if err := turnRows.Scan(&t.SessionID, &t.TurnKey, &t.Prompt, &t.Status, &t.Result, &t.AttemptID, &cAt, &compAt); err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		t.CreatedAt = parseTime(cAt)
		if compAt.Valid {
			ct := parseTime(compAt.String)
			t.CompletedAt = &ct
		}
		if hs, exists := state.Sessions[t.SessionID]; exists {
			hs.Turns[t.TurnKey] = t
			state.Sessions[t.SessionID] = hs
		}
	}
	if err := turnRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate turns: %w", err)
	}

	// 6. Load dispatch intents
	intentRows, err := tx.QueryContext(ctx, `
SELECT session_id, turn_key, attempt_id, phase, recorded_at, updated_at FROM dispatch_intents;`)
	if err != nil {
		return nil, fmt.Errorf("hydrate dispatch intents: %w", err)
	}
	defer intentRows.Close()

	for intentRows.Next() {
		var di DispatchIntentRecord
		var rAt, uAt string
		if err := intentRows.Scan(&di.SessionID, &di.TurnKey, &di.AttemptID, &di.Phase, &rAt, &uAt); err != nil {
			return nil, fmt.Errorf("scan dispatch intent: %w", err)
		}
		di.RecordedAt = parseTime(rAt)
		di.UpdatedAt = parseTime(uAt)
		if hs, exists := state.Sessions[di.SessionID]; exists {
			hs.DispatchIntents[di.TurnKey] = di
			state.Sessions[di.SessionID] = hs
		}
	}
	if err := intentRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dispatch intents: %w", err)
	}

	// 7. Load journal entries
	journalRows, err := tx.QueryContext(ctx, `
SELECT op_id, command_type, payload_json FROM journal_entries ORDER BY seq ASC;`)
	if err != nil {
		return nil, fmt.Errorf("hydrate journals: %w", err)
	}
	defer journalRows.Close()

	for journalRows.Next() {
		var opID, cmdType, payloadJSON string
		if err := journalRows.Scan(&opID, &cmdType, &payloadJSON); err != nil {
			return nil, fmt.Errorf("scan journal: %w", err)
		}
		if cmdType == "release_turn" {
			var rjp releaseJournalPayload
			if err := json.Unmarshal([]byte(payloadJSON), &rjp); err != nil {
				return nil, fmt.Errorf("unmarshal release journal: %w", err)
			}
			state.Journals = append(state.Journals, rjp.Receipt.OperationReceipt)
		} else {
			var jp journalPayload
			if err := json.Unmarshal([]byte(payloadJSON), &jp); err != nil {
				return nil, fmt.Errorf("unmarshal journal: %w", err)
			}
			state.Journals = append(state.Journals, jp.Receipt)
		}
	}
	if err := journalRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate journals: %w", err)
	}

	// 8. Establish active turn/intent pointers and enforce relational invariants
	for id, hs := range state.Sessions {
		if hs.ActiveTurnKey != nil && *hs.ActiveTurnKey != "" {
			k := *hs.ActiveTurnKey
			turn, exists := hs.Turns[k]
			if !exists {
				return nil, fmt.Errorf("%w: session %s active turn %s not found in turns", ErrInconsistentStorage, id, k)
			}
			turnCopy := turn
			hs.ActiveTurn = &turnCopy
			if intent, hasIntent := hs.DispatchIntents[k]; hasIntent {
				intentCopy := intent
				hs.ActiveIntent = &intentCopy
			}
		} else {
			for _, t := range hs.Turns {
				if t.Status == "running" || t.Status == "cancelling" {
					return nil, fmt.Errorf("%w: session %s has inactive session but turn %s has active status %s", ErrInconsistentStorage, id, t.TurnKey, t.Status)
				}
			}
		}
		state.Sessions[id] = hs
	}

	return state, nil
}
