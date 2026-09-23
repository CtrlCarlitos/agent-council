//go:build unix

package codextest

// Fixture-scope behavioral evidence over the compiled fixture executable
// (POSIX): production eligibility is SKIPPED by the explicit marker
// while every protocol validation stays in force — the exact contrast
// with the production path (internal/adapter/codex/
// production_guard_test.go), where a nil attestation fails closed before
// any child starts.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

const fixtureSessionID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6f"

// The fixture adapter runs the full §3.4 creation and §3.5 dispatch with
// NO attestation anywhere: eligibility is skipped (create succeeds, the
// launch gate at every dispatch does not refuse), the child really runs,
// and the turn reaches a verified terminal.
func TestCodexTest_FixtureAdapterSkipsProductionEligibility(t *testing.T) {
	f, err := NewFixtureAdapter(FixtureOptions{})
	if err != nil {
		t.Fatalf("new fixture adapter: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	ctx := context.Background()
	if err := f.SeedSession(ctx, fixtureSessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	binding, err := f.Adapter.CreateSession(ctx, f.NewCreateRequest(fixtureSessionID))
	if err != nil {
		t.Fatalf("CreateSession with no attestation must succeed in the fixture scope, got %T: %v", err, err)
	}
	if binding.NativeSessionID != fixtureThreadID {
		t.Fatalf("fixture thread binding, got %q", binding.NativeSessionID)
	}
	if err := f.PersistBinding(ctx, binding); err != nil {
		t.Fatalf("persist binding: %v", err)
	}

	ref := adapter.TurnRef{SessionID: fixtureSessionID, TurnKey: "t1"}
	out, err := f.Adapter.Dispatch(ctx, ref, "review the diff and summarize")
	if err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch with no attestation must be accepted in the fixture scope, got %+v err=%v", out, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := f.Adapter.Collect(ctx, ref)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if res.Status == council.TurnCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn never reached a verified terminal: %+v", res)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The child really ran: the request log proves the child was
	// launched, handshook, and served the creation — the gate was
	// skipped by the explicit marker, not by anything failing silently.
	raw, err := os.ReadFile(filepath.Join(f.ScratchDir, ".codex-fixture-requests"))
	if err != nil {
		t.Fatalf("request log: %v", err)
	}
	if !strings.Contains(string(raw), "initialize") || !strings.Contains(string(raw), "thread/start") {
		t.Fatalf("child must have served the creation flow, got %q", string(raw))
	}
}

// Eligibility skip must NOT skip validation: a poisoned thread/start id
// is still classified UNCERTAIN through the fixture adapter.
func TestCodexTest_FixtureAdapterRetainsProtocolValidation(t *testing.T) {
	f, err := NewFixtureAdapter(FixtureOptions{Scenario: PoisonThreadStartScenario()})
	if err != nil {
		t.Fatalf("new fixture adapter: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	ctx := context.Background()
	if err := f.SeedSession(ctx, fixtureSessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	_, err = f.Adapter.CreateSession(ctx, f.NewCreateRequest(fixtureSessionID))
	var uncertain *adapter.ErrSessionCreationUncertain
	if !errors.As(err, &uncertain) || uncertain.PartialNativeID != "definitely-not-a-real-id" {
		t.Fatalf("poisoned id must classify UNCERTAIN through the fixture adapter, got %T: %v", err, err)
	}
}

// Drift detection is retained: a thread/resume effective config that
// disagrees with the frozen profile rejects the dispatch BEFORE the
// prompt is transmitted (pre-acceptance, typed ErrProfileDrift).
func TestCodexTest_FixtureAdapterRetainsDriftDetection(t *testing.T) {
	f, err := NewFixtureAdapter(FixtureOptions{})
	if err != nil {
		t.Fatalf("new fixture adapter: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	// No child has started yet: restage with a drifted resume shape.
	lines := append([]string{authOKLine()}, threadStartRules(fixtureThreadID, f.WorkspaceRoot, f.Model)...)
	lines = append(lines, resumeRule(fixtureThreadID, f.WorkspaceRoot, f.Model, func(cfg map[string]any) {
		cfg["model"] = "drifted-model"
	}))
	if err := f.StageScenario(lines...); err != nil {
		t.Fatalf("stage drift scenario: %v", err)
	}

	ctx := context.Background()
	if err := f.SeedSession(ctx, fixtureSessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	binding, err := f.Adapter.CreateSession(ctx, f.NewCreateRequest(fixtureSessionID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.PersistBinding(ctx, binding); err != nil {
		t.Fatalf("persist binding: %v", err)
	}

	out, err := f.Adapter.Dispatch(ctx, adapter.TurnRef{SessionID: fixtureSessionID, TurnKey: "t-drift"}, "prompt")
	if out.Status != adapter.DispatchRejected {
		t.Fatalf("drifted resume must reject the dispatch, got %+v err=%v", out, err)
	}
	var drift *codex.ErrProfileDrift
	if !errors.As(err, &drift) || drift.Field != "model" {
		t.Fatalf("drift must be typed ErrProfileDrift on model, got %T: %v", err, err)
	}
}

// The emit_many directives are HONORED, not silently dropped: one
// emit_many_on_request on turn/start carries BOTH the turn/started and
// turn/completed notifications before the response. The native turn id
// binding proves line 1 arrived; the verified terminal proves line 2
// arrived. The child drops unknown directives without error, so if this
// scenario ever degrades to a no-op the turn never reaches a terminal
// and this test fails on the collect deadline.
func TestCodexTest_FixtureAdapterHonorsEmitManyDirective(t *testing.T) {
	f, err := NewFixtureAdapter(FixtureOptions{})
	if err != nil {
		t.Fatalf("new fixture adapter: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	// No child has started yet: restage with the multi-line directive.
	lines := append([]string{authOKLine()}, threadStartRules(fixtureThreadID, f.WorkspaceRoot, f.Model)...)
	lines = append(lines, resumeRule(fixtureThreadID, f.WorkspaceRoot, f.Model, nil))
	lines = append(lines,
		`{"emit_many_on_request": {"method":"turn/start","lines":[`+
			`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"`+fixtureThreadID+`","turnId":"`+fixtureTurnID+`"}},`+
			`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"`+fixtureThreadID+`","turn":{"id":"`+fixtureTurnID+`","items":[],"status":"completed","durationMs":100}}}`+
			`]}}`,
		`{"respond": {"method":"turn/start","result":{"id":"`+fixtureTurnID+`","threadId":"`+fixtureThreadID+`","status":{"type":"inProgress"}}}}`)
	if err := f.StageScenario(lines...); err != nil {
		t.Fatalf("stage emit_many scenario: %v", err)
	}

	ctx := context.Background()
	if err := f.SeedSession(ctx, fixtureSessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	binding, err := f.Adapter.CreateSession(ctx, f.NewCreateRequest(fixtureSessionID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.PersistBinding(ctx, binding); err != nil {
		t.Fatalf("persist binding: %v", err)
	}

	ref := adapter.TurnRef{SessionID: fixtureSessionID, TurnKey: "t-many"}
	out, err := f.Adapter.Dispatch(ctx, ref, "prompt")
	if err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%+v err=%v", out, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		att, err := f.Store.GetLatestCodexTurnAttempt(ctx, fixtureSessionID, ref.TurnKey)
		if err != nil {
			t.Fatalf("attempt lookup: %v", err)
		}
		if att != nil && att.Terminal && att.NativeTurnID != nil && *att.NativeTurnID == fixtureTurnID {
			// Both emitted lines took effect: the turn/started notification
			// bound the native turn id, and the turn/completed notification
			// classified the terminal.
			if att.ObservedStatus != "completed" {
				t.Fatalf("terminal classification: %q", att.ObservedStatus)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("emit_many lines never took effect: attempt=%+v", att)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
