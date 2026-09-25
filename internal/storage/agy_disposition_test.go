package storage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func seedAgyDisposition(t *testing.T) (*Store, ExecutionRef, int64) {
	t.Helper()
	s := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	v, _ := s.GetSessionVersion(ctx, agyUncSession)
	q, err := s.QueuePrompt(ctx, "q-disposition", agyUncLease, agyUncSession, v, PendingPrompt{SessionID: agyUncSession, TurnKey: "lost", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ReleaseTurn(ctx, "rel-disposition", agyUncLease, agyUncSession, q.CommittedVersion, "lost")
	if err != nil {
		t.Fatal(err)
	}
	ref := ExecutionRef{SessionID: agyUncSession, TurnKey: "lost", AttemptID: r.Receipt.AttemptID}
	if err := s.InsertAgyTurnAttempt(ctx, agyAttemptFixture(ref.AttemptID, ref.SessionID, ref.TurnKey)); err != nil {
		t.Fatal(err)
	}
	return s, ref, r.Receipt.CommittedVersion
}

func TestAgyDisposition_AtomicReplayAndAuthority(t *testing.T) {
	s, ref, version := seedAgyDisposition(t)
	ctx := context.Background()
	resolve := func(op, lease string, generation uint64, v int64, target ExecutionRef, reason string) (OperationReceipt, error) {
		return s.ResolveAgyTurnUncertainty(ctx, op, lease, generation, v, target, "abandoned", reason)
	}
	for _, tc := range []struct {
		name, lease string
		generation  uint64
		version     int64
		ref         ExecutionRef
		want        error
	}{
		{"foreign lease", "worker", 1, version, ref, ErrUnauthorizedOperation},
		{"generation", agyUncLease, 2, version, ref, ErrGenerationMismatch},
		{"version", agyUncLease, 1, version + 1, ref, ErrStaleUpdate},
		{"attempt", agyUncLease, 1, version, ExecutionRef{SessionID: ref.SessionID, TurnKey: ref.TurnKey, AttemptID: "foreign"}, ErrWrongExecutionAttempt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolve("bad-"+tc.name, tc.lease, tc.generation, tc.version, tc.ref, "reason"); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
	// Fail the last write: no disposition, turn completion, intent change,
	// version bump or journal entry may escape a rolled-back transaction.
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_disposition BEFORE INSERT ON journal_entries WHEN NEW.command_type = 'resolve_agy_turn_uncertain' BEGIN SELECT RAISE(ABORT, 'injected disposition failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve("dispose", agyUncLease, 1, version, ref, "confirmed process gone; abandon"); err == nil {
		t.Fatal("expected injected failure")
	}
	a, _ := s.GetAgyTurnAttempt(ctx, ref.AttemptID)
	d, _ := s.GetTurnDetails(ctx, ref.SessionID, ref.TurnKey)
	v, _ := s.GetSessionVersion(ctx, ref.SessionID)
	if a.UncertaintyDisposition != nil || d.Status != council.TurnRunning || d.DispatchIntent.Phase == "resolved" || v != version || journalCount(t, s, "resolve_agy_turn_uncertain") != 0 {
		t.Fatal("failed transaction changed state")
	}
	if _, err := s.DB().Exec(`DROP TRIGGER fail_disposition`); err != nil {
		t.Fatal(err)
	}
	r, err := resolve("dispose", agyUncLease, 1, version, ref, "confirmed process gone; abandon")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Payload, "generation") || !strings.Contains(r.Payload, ref.AttemptID) {
		t.Fatalf("missing provenance: %+v", r)
	}
	s = reopenStore(t, s)
	replay, err := resolve("dispose", agyUncLease, 1, version, ref, "confirmed process gone; abandon")
	if err != nil || replay != r {
		t.Fatalf("replay after reopen: %+v %v", replay, err)
	}
	if _, err := resolve("dispose", agyUncLease, 1, version, ref, "different"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay: %v", err)
	}
	if _, err := resolve("dispose-again", agyUncLease, 1, r.CommittedVersion, ref, "reason"); err == nil {
		t.Fatal("second disposition accepted")
	}
	a, _ = s.GetAgyTurnAttempt(ctx, ref.AttemptID)
	d, _ = s.GetTurnDetails(ctx, ref.SessionID, ref.TurnKey)
	if a.Terminal || a.ObservedStatus != "uncertain" || a.UncertaintyDisposition == nil || d.Status != council.TurnInterrupted || d.DispatchIntent.Phase != "resolved" {
		t.Fatalf("incorrect disposition: %+v %+v", a, d)
	}
	if _, err := s.HandoffController(ctx, "handoff-disposition", agyUncRunID, agyUncLease, 1, "codex", "new-controller", "new-lease"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve("dispose", agyUncLease, 1, version, ref, "confirmed process gone; abandon"); !errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("superseded replay: %v", err)
	}
}

func TestAgyDisposition_ConcurrentReplayAndDisconnectedWriteFence(t *testing.T) {
	s, ref, version := seedAgyDisposition(t)
	ctx := context.Background()
	record, err := s.GetControllerRecord(ctx, agyUncRunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DisconnectRunController(ctx, "disconnect-disposition", agyUncRunID, agyUncLease, record.AttachmentID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAgyTurnUncertainty(ctx, "resolve-disposition", agyUncLease, 1, version, ref, "abandoned", "retired"); !errors.Is(err, ErrControllerDisconnected) {
		t.Fatalf("storage connection fence: %v", err)
	}
	if _, err := s.ConnectRunController(ctx, "reconnect-disposition", agyUncRunID, agyUncLease, 1, "inst-1"); err != nil {
		t.Fatal(err)
	}
	version, err = s.GetSessionVersion(ctx, ref.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	receipts := make([]OperationReceipt, 4)
	errs := make([]error, 4)
	for i := range receipts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			receipts[i], errs[i] = s.ResolveAgyTurnUncertainty(ctx, "resolve-disposition", agyUncLease, 1, version, ref, "abandoned", "retired")
		}(i)
	}
	wg.Wait()
	for i := range receipts {
		if errs[i] != nil || receipts[i] != receipts[0] {
			t.Fatalf("concurrent receipt %d: %+v %v", i, receipts[i], errs[i])
		}
	}
	if n := journalCount(t, s, "resolve_agy_turn_uncertain"); n != 1 {
		t.Fatalf("duplicate journal entries: %d", n)
	}
}
