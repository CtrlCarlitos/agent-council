package adapter_test

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// testBoundaryDriver delivers events from an adapter to a council.Session,
// strictly enforcing council turn invariants.
type testBoundaryDriver struct {
	sess      *council.Session
	sessionID adapter.SessionID
}

func newBoundaryDriver(sess *council.Session, sessionID adapter.SessionID) *testBoundaryDriver {
	return &testBoundaryDriver{
		sess:      sess,
		sessionID: sessionID,
	}
}

func (d *testBoundaryDriver) HandleEvent(ev adapter.Event) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	if d.sessionID != "" && ev.Ref.SessionID != d.sessionID {
		return errors.New("mismatched session id")
	}
	if ev.Ref.TurnKey != d.sess.Active {
		return errors.New("mismatched turn key")
	}

	switch ev.Type {
	case adapter.EventProgress:
		if d.sess.State != council.Running || d.sess.Active != ev.Ref.TurnKey {
			return errors.New("progress event received for inactive council session")
		}
		return nil

	case adapter.EventToolDenied:
		// Tool denial records diagnostic, but crucially does NOT retire active turn or clear slot
		if ev.Status != council.TurnRunning {
			return errors.New("tool denial event carries non-running status")
		}
		if d.sess.State != council.Running || d.sess.Active != ev.Ref.TurnKey {
			return errors.New("tool denial received for inactive session")
		}
		return nil

	case adapter.EventTerminal:
		switch ev.Status {
		case council.TurnCompleted:
			return d.sess.CompleteWithResult(ev.Ref.TurnKey, ev.Payload)
		case council.TurnCancelled:
			if d.sess.ActiveTurn != nil && d.sess.ActiveTurn.Status == council.TurnCancelling {
				return d.sess.ConfirmCancel(ev.Ref.TurnKey)
			}
			return d.sess.Interrupt(ev.Ref.TurnKey)
		case council.TurnFailed:
			return d.sess.Fail(ev.Ref.TurnKey, ev.Payload)
		default:
			return fmt.Errorf("unexpected terminal status: %s", ev.Status)
		}
	}
	return nil
}

func TestCouncilBoundary_UnacknowledgedDispatchPreservesReservation(t *testing.T) {
	ctx := context.Background()
	sess, err := council.NewSession(council.Claude, "lease-1")
	if err != nil {
		t.Fatal(err)
	}

	_ = sess.Queue("lease-1", "t1", "prompt 1")
	_ = sess.Queue("lease-1", "t2", "prompt 2")

	// Release t1 from Council: returns prompt content, while key is "t1"
	prompt1, err := sess.Release("lease-1", "t1")
	if err != nil {
		t.Fatalf("release t1 failed: %v", err)
	}
	if prompt1 != "prompt 1" {
		t.Fatalf("expected prompt 'prompt 1', got %q", prompt1)
	}

	// Dispatch t1 to fake with held acknowledgement
	holdAck := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		HoldDispatchAck: holdAck,
		DispatchStatus:  adapter.DispatchUnknown,
	})

	turnRef1 := adapter.TurnRef{SessionID: adapter.SessionID(fmt.Sprintf("sess-%s", sess.ID)), TurnKey: "t1"}
	_, err = fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   turnRef1.SessionID,
		Contributor: sess.ID,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	doneDispatch := make(chan adapter.DispatchOutcome)
	go func() {
		outcome, _ := fake.Dispatch(ctx, turnRef1, prompt1)
		doneDispatch <- outcome
	}()

	close(holdAck)
	outcome := <-doneDispatch
	if outcome.Status != adapter.DispatchUnknown {
		t.Fatalf("expected DispatchUnknown, got %s", outcome.Status)
	}

	// Turn reservation must be preserved: attempting to release t2 must fail!
	if _, err := sess.Release("lease-1", "t2"); err == nil {
		t.Fatal("release of t2 permitted while t1 execution reservation remains active")
	}

	if sess.Active != "t1" || sess.State != council.Running {
		t.Fatalf("session state corrupted after DispatchUnknown: active=%s state=%s", sess.Active, sess.State)
	}

	// Authoritative reconciliation resolves the unknown turn
	recOutcome, err := fake.Reconcile(ctx, adapter.RecoveryRef{
		TurnRef:    turnRef1,
		Generation: 1,
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// Deliver reconciled outcome to Council
	if err := sess.CompleteWithResult(recOutcome.Ref.TurnKey, recOutcome.Result); err != nil {
		t.Fatalf("reconciling turn in Council failed: %v", err)
	}

	if sess.State != council.Parked || sess.Active != "" {
		t.Fatalf("session not parked after reconciliation: state=%s active=%s", sess.State, sess.Active)
	}

	// Now t2 can be successfully released
	prompt2, err := sess.Release("lease-1", "t2")
	if err != nil {
		t.Fatalf("release of t2 failed after t1 reconciliation: %v", err)
	}
	if prompt2 != "prompt 2" || sess.Active != "t2" {
		t.Fatalf("t2 state unexpected: prompt=%q active=%s", prompt2, sess.Active)
	}
}

func TestCouncilBoundary_ToolDenialDoesNotRetireSlot(t *testing.T) {
	ctx := context.Background()
	sess, err := council.NewSession(council.Agy, "lease-1")
	if err != nil {
		t.Fatal(err)
	}

	_ = sess.Queue("lease-1", "t1", "prompt 1")
	_ = sess.Queue("lease-1", "t2", "prompt 2")
	prompt1, err := sess.Release("lease-1", "t1")
	if err != nil {
		t.Fatal(err)
	}

	stall := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		AutoDenyTools: map[string]bool{"git-commit": true},
		StallStream:   stall,
	})

	turnRef1 := adapter.TurnRef{SessionID: adapter.SessionID(fmt.Sprintf("sess-%s", sess.ID)), TurnKey: "t1"}
	_, err = fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   turnRef1.SessionID,
		Contributor: sess.ID,
		Config:      adapter.SessionConfig{Model: "agy-1", Tooling: []string{"git-commit"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = fake.Dispatch(ctx, turnRef1, prompt1)
	if err != nil {
		t.Fatal(err)
	}

	stream, err := fake.Observe(ctx, turnRef1)
	if err != nil {
		t.Fatal(err)
	}

	driver := newBoundaryDriver(sess, turnRef1.SessionID)

	// Read stream events until tool denial arrives
	var sawDenial bool
	for ev := range stream.Events() {
		if err := driver.HandleEvent(ev); err != nil {
			t.Fatalf("driver error on event %s: %v", ev.Type, err)
		}
		if ev.Type == adapter.EventToolDenied {
			sawDenial = true
			break
		}
	}
	if !sawDenial {
		t.Fatal("expected EventToolDenied before stream ended")
	}

	// Tool denial does NOT retire the slot
	if sess.Active != "t1" || sess.State != council.Running {
		t.Fatalf("tool denial prematurely cleared active turn: active=%s state=%s", sess.Active, sess.State)
	}

	// Releasing t2 must remain blocked
	if _, err := sess.Release("lease-1", "t2"); err == nil {
		t.Fatal("release of t2 permitted while t1 is unretired after tool denial")
	}

	// Release stall so turn finishes with terminal event
	close(stall)

	for ev := range stream.Events() {
		if err := driver.HandleEvent(ev); err != nil {
			t.Fatalf("driver error on event %s: %v", ev.Type, err)
		}
	}

	if sess.State != council.Parked {
		t.Fatalf("expected parked after terminal completion, got %s", sess.State)
	}

	// Now t2 can be released
	if _, err := sess.Release("lease-1", "t2"); err != nil {
		t.Fatalf("release of t2 failed after terminal outcome: %v", err)
	}
}

func TestCouncilBoundary_StaleRecoveryGenerationRejectedAcrossOutages(t *testing.T) {
	sess, err := council.NewSession(council.Codex, "lease-1")
	if err != nil {
		t.Fatal(err)
	}

	_ = sess.Queue("lease-1", "t1", "p1")
	_, _ = sess.Release("lease-1", "t1")

	// Outage 1
	gen1, err := sess.RecordHostLoss()
	if err != nil {
		t.Fatal(err)
	}

	// Outage 1 recovers
	if err := sess.ReconcileHost("t1", gen1, council.TurnRunning, ""); err != nil {
		t.Fatal(err)
	}

	// Outage 2
	gen2, err := sess.RecordHostLoss()
	if err != nil {
		t.Fatal(err)
	}
	if gen2 <= gen1 {
		t.Fatalf("expected gen2 > gen1, got %d <= %d", gen2, gen1)
	}

	// Stale recovery response with gen1 from fake
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StaleRecoveryRef: &adapter.RecoveryRef{
			TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID(fmt.Sprintf("sess-%s", sess.ID)), TurnKey: "t1"},
			Generation: gen1,
		},
	})

	outcome, err := fake.Reconcile(context.Background(), adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID(fmt.Sprintf("sess-%s", sess.ID)), TurnKey: "t1"},
		Generation: gen2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Delivery of stale outcome to Council must be rejected
	err = sess.ReconcileHost(outcome.Ref.TurnKey, outcome.Ref.Generation, outcome.Observed, outcome.Result)
	if err == nil {
		t.Fatal("stale generation 1 reconciliation was accepted during outage 2")
	}

	if sess.Visibility != council.VisibilityHostLost {
		t.Fatalf("stale reconciliation cleared host uncertainty: visibility=%s", sess.Visibility)
	}
}

func TestCouncilBoundary_RosterSessionIsolation(t *testing.T) {
	ctx := context.Background()
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	roster := council.Roster()

	type rosterWorker struct {
		contrib council.Contributor
		sess    *council.Session
		turnRef adapter.TurnRef
		prompt  string
	}

	workers := make([]rosterWorker, len(roster))
	for i, contrib := range roster {
		lease := fmt.Sprintf("lease-%s", contrib)
		sess, err := council.NewSession(contrib, lease)
		if err != nil {
			t.Fatalf("NewSession failed for %s: %v", contrib, err)
		}
		turnKey := fmt.Sprintf("turn-%s", contrib)
		prompt := fmt.Sprintf("prompt for %s", contrib)
		if err := sess.Queue(lease, turnKey, prompt); err != nil {
			t.Fatalf("Queue failed for %s: %v", contrib, err)
		}
		relPrompt, err := sess.Release(lease, turnKey)
		if err != nil {
			t.Fatalf("Release failed for %s: %v", contrib, err)
		}

		turnRef := adapter.TurnRef{
			SessionID: adapter.SessionID(fmt.Sprintf("sess-%s", contrib)),
			TurnKey:   turnKey,
		}
		_, err = fake.CreateSession(ctx, adapter.CreateSessionRequest{
			SessionID:   turnRef.SessionID,
			Contributor: contrib,
			Config: adapter.SessionConfig{
				WorkspaceRoot: "/workspace/" + string(contrib),
				Model:         "model-" + string(contrib),
			},
		})
		if err != nil {
			t.Fatalf("CreateSession failed for %s: %v", contrib, err)
		}

		workers[i] = rosterWorker{
			contrib: contrib,
			sess:    sess,
			turnRef: turnRef,
			prompt:  relPrompt,
		}
	}

	// Execute concurrent turns and interleave events across all roster contributors
	var wg sync.WaitGroup
	errs := make(chan error, len(workers))

	for _, w := range workers {
		wg.Add(1)
		go func(worker rosterWorker) {
			defer wg.Done()
			driver := newBoundaryDriver(worker.sess, worker.turnRef.SessionID)

			outcome, err := fake.Dispatch(ctx, worker.turnRef, worker.prompt)
			if err != nil || outcome.Status != adapter.DispatchAccepted {
				errs <- fmt.Errorf("dispatch failed for %s: err=%v outcome=%+v", worker.contrib, err, outcome)
				return
			}

			stream, err := fake.Observe(ctx, worker.turnRef)
			if err != nil {
				errs <- fmt.Errorf("observe failed for %s: %v", worker.contrib, err)
				return
			}

			for ev := range stream.Events() {
				if err := driver.HandleEvent(ev); err != nil {
					errs <- fmt.Errorf("driver failed for %s on event %s: %v", worker.contrib, ev.Type, err)
					return
				}
			}

			res, err := fake.Collect(ctx, worker.turnRef)
			if err != nil || res.Status != council.TurnCompleted {
				errs <- fmt.Errorf("collect failed for %s: err=%v res=%+v", worker.contrib, err, res)
				return
			}
		}(w)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	// Verify all roster sessions transitioned to Parked with no crosstalk
	for _, w := range workers {
		if w.sess.State != council.Parked || w.sess.Active != "" {
			t.Fatalf("session for %s did not park cleanly: state=%s active=%s", w.contrib, w.sess.State, w.sess.Active)
		}
	}
}

func TestCouncilBoundary_ProductionRegistryRejectsFake(t *testing.T) {
	// 1. Production registry rejects fake resolution and fails closed on unknown contributors
	reg := adapter.NewRegistry()

	if _, err := reg.ResolveByName("fake"); !errors.Is(err, adapter.ErrFakeAdapterProhibited) {
		t.Fatalf("expected ErrFakeAdapterProhibited, got %v", err)
	}
	if _, err := reg.ResolveByName("fake-adapter"); !errors.Is(err, adapter.ErrFakeAdapterProhibited) {
		t.Fatalf("expected ErrFakeAdapterProhibited, got %v", err)
	}
	if _, err := reg.ResolveByName("unknown"); !errors.Is(err, adapter.ErrUnknownAdapter) {
		t.Fatalf("expected ErrUnknownAdapter, got %v", err)
	}
	if err := reg.Register(council.Claude, nil); err == nil {
		t.Fatal("expected error registering nil adapter")
	}

	// 2. Production build dependency graph check:
	// Verify that no production source file in internal/ or cmd/ imports internal/adapter/adaptertest
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == "vendor" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip adaptertest package itself
		if strings.Contains(path, filepath.Join("internal", "adapter", "adaptertest")) {
			return nil
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(importPath, "internal/adapter/adaptertest") {
				t.Fatalf("production file %s illegally imports adaptertest: %s", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed checking production package imports: %v", err)
	}
}
