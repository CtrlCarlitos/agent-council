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
	PendingPrompt *PendingPrompt
	ActiveTurn    *TurnRecord
	ActiveIntent  *DispatchIntentRecord
	NativeBinding *NativeBinding
	RowVersion    int64
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

	// 1. Load runs
	runRows, err := s.readDB.QueryContext(ctx, `
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

	// 2. Load sessions
	sessRows, err := s.readDB.QueryContext(ctx, `
SELECT session_id, run_id, contributor, is_active_contributor, state, visibility, active_key, recovery_gen, row_version
FROM sessions;`)
	if err != nil {
		return nil, fmt.Errorf("hydrate sessions: %w", err)
	}
	defer sessRows.Close()

	for sessRows.Next() {
		var hs HydratedSession
		var isActive int
		var activeKey sql.NullString
		if err := sessRows.Scan(&hs.ID, &hs.RunID, &hs.Contributor, &isActive, &hs.State, &hs.Visibility, &activeKey, &hs.RecoveryGeneration, &hs.RowVersion); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		hs.IsActiveContributor = (isActive == 1)
		if activeKey.Valid {
			k := activeKey.String
			hs.ActiveTurnKey = &k
		}
		state.Sessions[hs.ID] = hs
	}

	// 3. Load native bindings
	bindRows, err := s.readDB.QueryContext(ctx, `
SELECT session_id, native_session_id, harness, model, config_json FROM native_bindings;`)
	if err == nil {
		defer bindRows.Close()
		for bindRows.Next() {
			var b NativeBinding
			if err := bindRows.Scan(&b.LogicalSessionID, &b.NativeSessionID, &b.Harness, &b.Model, &b.ToolingConfig); err == nil {
				if hs, exists := state.Sessions[b.LogicalSessionID]; exists {
					hs.NativeBinding = &b
					state.Sessions[b.LogicalSessionID] = hs
				}
			}
		}
	}

	// 4. Load pending prompts
	promptRows, err := s.readDB.QueryContext(ctx, `
SELECT session_id, turn_key, prompt, queued_at FROM pending_prompts;`)
	if err == nil {
		defer promptRows.Close()
		for promptRows.Next() {
			var p PendingPrompt
			var turnKey, qAt string
			if err := promptRows.Scan(&p.SessionID, &turnKey, &p.Prompt, &qAt); err == nil {
				p.CreatedAt = parseTime(qAt)
				if hs, exists := state.Sessions[p.SessionID]; exists {
					hs.PendingPrompt = &p
					state.Sessions[p.SessionID] = hs
				}
			}
		}
	}

	// 5. Load active turns
	turnRows, err := s.readDB.QueryContext(ctx, `
SELECT session_id, turn_key, prompt, status, result, attempt_id, created_at, completed_at FROM turns;`)
	if err == nil {
		defer turnRows.Close()
		for turnRows.Next() {
			var t TurnRecord
			var cAt string
			var compAt sql.NullString
			if err := turnRows.Scan(&t.SessionID, &t.TurnKey, &t.Prompt, &t.Status, &t.Result, &t.AttemptID, &cAt, &compAt); err == nil {
				t.CreatedAt = parseTime(cAt)
				if compAt.Valid {
					ct := parseTime(compAt.String)
					t.CompletedAt = &ct
				}
				if hs, exists := state.Sessions[t.SessionID]; exists && hs.ActiveTurnKey != nil && *hs.ActiveTurnKey == t.TurnKey {
					hs.ActiveTurn = &t
					state.Sessions[t.SessionID] = hs
				}
			}
		}
	}

	// 6. Load active dispatch intents
	intentRows, err := s.readDB.QueryContext(ctx, `
SELECT session_id, turn_key, attempt_id, phase, recorded_at, updated_at FROM dispatch_intents;`)
	if err == nil {
		defer intentRows.Close()
		for intentRows.Next() {
			var di DispatchIntentRecord
			var rAt, uAt string
			if err := intentRows.Scan(&di.SessionID, &di.TurnKey, &di.AttemptID, &di.Phase, &rAt, &uAt); err == nil {
				di.RecordedAt = parseTime(rAt)
				di.UpdatedAt = parseTime(uAt)
				if hs, exists := state.Sessions[di.SessionID]; exists && hs.ActiveTurnKey != nil && *hs.ActiveTurnKey == di.TurnKey {
					hs.ActiveIntent = &di
					state.Sessions[di.SessionID] = hs
				}
			}
		}
	}

	// 7. Load journal entries
	journalRows, err := s.readDB.QueryContext(ctx, `
SELECT op_id, command_type, payload_json FROM journal_entries ORDER BY seq ASC;`)
	if err == nil {
		defer journalRows.Close()
		for journalRows.Next() {
			var opID, cmdType, payloadJSON string
			if err := journalRows.Scan(&opID, &cmdType, &payloadJSON); err == nil {
				if cmdType == "release_turn" {
					var rjp releaseJournalPayload
					if err := json.Unmarshal([]byte(payloadJSON), &rjp); err == nil {
						state.Journals = append(state.Journals, rjp.Receipt.OperationReceipt)
					}
				} else {
					var jp journalPayload
					if err := json.Unmarshal([]byte(payloadJSON), &jp); err == nil {
						state.Journals = append(state.Journals, jp.Receipt)
					}
				}
			}
		}
	}

	return state, nil
}
