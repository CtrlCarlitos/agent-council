package adapter_test

import (
	"context"
	"sync"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/conformance"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

type fakeFixture struct {
	fake *adaptertest.FakeAdapter
}

func (f *fakeFixture) Adapter() adapter.Adapter { return f.fake }
func (f *fakeFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	st := f.fake.TurnState(ref)
	return st.Received, st.Accepted, st.Started
}
func (f *fakeFixture) Cleanup() error { return nil }

func TestConformance_ConformingFakePasses(t *testing.T) {
	conformance.Run(t, func(t *testing.T) conformance.Fixture {
		return &fakeFixture{fake: adaptertest.NewFake(adaptertest.ScriptedFaults{})}
	})
}

// collidingSessionAdapter intentionally forces identical native session IDs for all logical sessions.
type collidingSessionAdapter struct {
	*adaptertest.FakeAdapter
}

func (c *collidingSessionAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	return adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: "shared-colliding-native-id",
		Config:          req.Config,
	}, nil
}

type collidingFixture struct {
	ad adapter.Adapter
}

func (c *collidingFixture) Adapter() adapter.Adapter { return c.ad }
func (c *collidingFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	return false, false, false
}
func (c *collidingFixture) Cleanup() error { return nil }

type eofStream struct {
	adapter.Stream
	onClose func()
}

func (s *eofStream) Close() error {
	err := s.Stream.Close()
	s.onClose()
	return err
}

// eofCompletingAdapter fabricates turn completion whenever observe is closed.
type eofCompletingAdapter struct {
	*adaptertest.FakeAdapter
	mu        sync.Mutex
	completed bool
}

func (e *eofCompletingAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	stream, _ := adapter.NewBufferedStream(ref, 10)
	return &eofStream{
		Stream: stream,
		onClose: func() {
			e.mu.Lock()
			e.completed = true
			e.mu.Unlock()
		},
	}, nil
}

func (e *eofCompletingAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.completed {
		return adapter.TurnResult{
			Ref:          ref,
			Status:       council.TurnCompleted,
			ResultStatus: adapter.ResultAvailable,
		}, nil
	}
	return adapter.TurnResult{
		Ref:          ref,
		Status:       council.TurnRunning,
		ResultStatus: adapter.ResultPending,
	}, nil
}

type eofFixture struct {
	ad adapter.Adapter
}

func (e *eofFixture) Adapter() adapter.Adapter { return e.ad }
func (e *eofFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	return true, true, true
}
func (e *eofFixture) Cleanup() error { return nil }

func TestConformance_SensitivityChecks(t *testing.T) {
	t.Run("DetectsRewrittenRecoveryGeneration", func(t *testing.T) {
		staleRef := &adapter.RecoveryRef{
			TurnRef:    adapter.TurnRef{SessionID: "s1", TurnKey: "t1"},
			Generation: 1, // Will substitute gen 1 when gen 2 was asked
		}
		brokenFake := adaptertest.NewFake(adaptertest.ScriptedFaults{
			StaleRecoveryRef: staleRef,
		})

		f := &fakeFixture{fake: brokenFake}
		violations := conformance.Check(context.Background(), f, conformance.ScenarioRecoveryIntegrity)

		if len(violations) == 0 {
			t.Fatal("expected ViolationGenerationSubstituted, got 0 violations")
		}
		if violations[0].Code != conformance.ViolationGenerationSubstituted {
			t.Fatalf("expected ViolationGenerationSubstituted, got %s", violations[0].Code)
		}
	})

	t.Run("DetectsSessionIDCollision", func(t *testing.T) {
		brokenAdapter := &collidingSessionAdapter{FakeAdapter: adaptertest.NewFake(adaptertest.ScriptedFaults{})}
		f := &collidingFixture{ad: brokenAdapter}

		violations := conformance.Check(context.Background(), f, conformance.ScenarioSessionIsolation)
		if len(violations) == 0 {
			t.Fatal("expected ViolationSessionIDCollision, got 0 violations")
		}
		if violations[0].Code != conformance.ViolationSessionIDCollision {
			t.Fatalf("expected ViolationSessionIDCollision, got %s", violations[0].Code)
		}
	})

	t.Run("DetectsEOFAsCompletion", func(t *testing.T) {
		brokenAdapter := &eofCompletingAdapter{FakeAdapter: adaptertest.NewFake(adaptertest.ScriptedFaults{})}
		f := &eofFixture{ad: brokenAdapter}

		violations := conformance.Check(context.Background(), f, conformance.ScenarioObservationClose)
		if len(violations) == 0 {
			t.Fatal("expected ViolationEOFAsCompletion, got 0 violations")
		}
		if violations[0].Code != conformance.ViolationEOFAsCompletion {
			t.Fatalf("expected ViolationEOFAsCompletion, got %s", violations[0].Code)
		}
	})
}
