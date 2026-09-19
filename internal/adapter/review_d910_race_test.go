package adapter_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// TestReviewD910_ConcurrentFakeSessionsPromptRace verifies that concurrent calls
// to FakeAdapter.Dispatch across distinct sessions do not race with background
// runWorker inspecting the prompt content.
func TestReviewD910_ConcurrentFakeSessionsPromptRace(t *testing.T) {
	ctx := context.Background()
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})

	const sessionCount = 50
	refs := make([]adapter.TurnRef, sessionCount)

	// Pre-create distinct sessions
	for i := 0; i < sessionCount; i++ {
		sID := adapter.SessionID(fmt.Sprintf("s-race-%d", i))
		req := adapter.CreateSessionRequest{
			SessionID:   sID,
			Contributor: council.Claude,
			Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		if _, err := fake.CreateSession(ctx, req); err != nil {
			t.Fatalf("CreateSession failed for %s: %v", sID, err)
		}
		refs[i] = adapter.TurnRef{SessionID: sID, TurnKey: "t1"}
	}

	var wg sync.WaitGroup
	wg.Add(sessionCount)

	for i := 0; i < sessionCount; i++ {
		idx := i
		ref := refs[idx]
		go func() {
			defer wg.Done()
			var prompt string
			if idx%2 == 0 {
				prompt = "test tool denial prompt"
			} else {
				prompt = fmt.Sprintf("regular prompt %d", idx)
			}
			outcome, err := fake.Dispatch(ctx, ref, prompt)
			if err != nil || outcome.Status != adapter.DispatchAccepted {
				t.Errorf("dispatch failed for %v: err=%v outcome=%+v", ref, err, outcome)
			}
		}()
	}

	wg.Wait()
}
