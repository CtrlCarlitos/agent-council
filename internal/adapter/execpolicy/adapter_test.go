package execpolicy_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

type mockProfileStore struct {
	profiles map[string]storage.RunProfileRecord
}

func (m *mockProfileStore) GetRunProfile(ctx context.Context, runID string) (storage.RunProfileRecord, error) {
	if p, ok := m.profiles[runID]; ok {
		return p, nil
	}
	return storage.RunProfileRecord{}, storage.ErrRunNotFound
}

func (m *mockProfileStore) GetSessionRunID(ctx context.Context, sessionID string) (string, error) {
	return "default", nil
}

func TestManagedWorkerAdapter_Lifecycle(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	stateDir := t.TempDir()

	wm, err := workspace.NewWorkspaceManager(tmp, stateDir)
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}

	executor := execpolicy.New()
	store := &mockProfileStore{
		profiles: map[string]storage.RunProfileRecord{
			"default": {
				RunID:         "default",
				WorkspaceMode: "none",
				Profile: storage.CanonicalProfile{
					AlgoVersion:         "cprof-v1",
					WorkspaceMode:       "none",
					IsolationStrictness: "permissive_dev",
					NetworkMode:         "unrestricted",
					Tooling:             []string{"echo"},
				},
			},
		},
	}

	adp := execpolicy.NewWorkerAdapter(wm, executor, store)

	// 1. Probe
	report, err := adp.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if report.Capabilities.StreamingObservation != adapter.CapabilitySupported {
		t.Fatalf("expected StreamingObservation supported")
	}

	// 2. CreateSession
	req := adapter.CreateSessionRequest{
		SessionID:   "session-1",
		Contributor: council.Claude,
		Config: adapter.SessionConfig{
			Tooling: []string{"echo"},
		},
	}
	binding, err := adp.CreateSession(ctx, req)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if binding.SessionID != "session-1" {
		t.Fatalf("expected session-1, got: %s", binding.SessionID)
	}

	// Verify workspace allocated
	paths, ok := wm.GetPaths("default", "session-1")
	if !ok {
		t.Fatalf("expected workspace paths allocated for default/session-1")
	}
	if paths.Mode != "none" {
		t.Fatalf("expected mode none, got: %s", paths.Mode)
	}

	// 3. Dispatch
	ref := adapter.TurnRef{
		SessionID: "session-1",
		TurnKey:   "turn-1",
	}
	outcome, err := adp.Dispatch(ctx, ref, "test prompt")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("expected DispatchAccepted, got: %s", outcome.Status)
	}

	// 4. Collect
	collectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := adp.Collect(collectCtx, ref)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Ref != ref {
		t.Fatalf("expected TurnRef %v, got %v", ref, res.Ref)
	}

	// Cleanup
	_ = wm.CloseWorkspace("default", "session-1")
}
