package storage

// AC-010 Task 5 fix round 1: the orphan conversation id is durable and
// once-only on the attempt; the attempt insert and its launch
// reservation are ONE transaction (a crash leaves nothing or both); a
// creation-uncertainty episode carries the orphan native id.

import (
	"context"
	"testing"
)

func TestAgyState_OrphanConversationDurableOnceOnly(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()
	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-o", "sess-o", "t-o")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	const orphan = "7a6b5c4d-3e2f-4a1b-8c9d-0e1f2a3b4c5d"
	if err := store.RecordAgyOrphanConversation(ctx, "att-o", orphan); err != nil {
		t.Fatalf("record orphan: %v", err)
	}
	if err := store.RecordAgyOrphanConversation(ctx, "att-o", orphan); err != nil {
		t.Fatalf("same-id replay must be idempotent: %v", err)
	}
	if err := store.RecordAgyOrphanConversation(ctx, "att-o", "11111111-2222-4333-8444-555555555555"); err == nil {
		t.Fatal("a different orphan id must be refused (once-only)")
	}
	if err := store.RecordAgyOrphanConversation(ctx, "att-missing", orphan); err == nil {
		t.Fatal("an unknown attempt must be refused")
	}

	reopened := reopenStore(t, store)
	a, err := reopened.GetAgyTurnAttempt(ctx, "att-o")
	if err != nil || a == nil || a.OrphanConversationID == nil || *a.OrphanConversationID != orphan {
		t.Fatalf("orphan id must survive reopen, got %+v err=%v", a, err)
	}
	latest, err := reopened.GetLatestAgyTurnAttempt(ctx, "sess-o", "t-o")
	if err != nil || latest == nil || latest.OrphanConversationID == nil || *latest.OrphanConversationID != orphan {
		t.Fatalf("latest attempt must surface the orphan id, got %+v err=%v", latest, err)
	}
}

func TestAgyState_InsertAndReserveIsAtomic(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	seq, err := store.InsertAgyTurnAttemptAndReserveLaunch(ctx, agyAttemptFixtureWithTools("att-r", "sess-r", "t-r", []string{"view_file"}), "fixture")
	if err != nil || seq != 1 {
		t.Fatalf("insert+reserve: seq=%d err=%v", seq, err)
	}
	reopened := reopenStore(t, store)
	a, err := reopened.GetAgyTurnAttempt(ctx, "att-r")
	if err != nil || a == nil || a.LaunchCount != 1 || len(a.RequiredTools) != 1 {
		t.Fatalf("attempt and reservation must both be durable, got %+v err=%v", a, err)
	}
	if states, _ := reopened.AgyAttemptLaunchStates(ctx, "att-r"); len(states) != 1 || states[0] != "reserved" {
		t.Fatalf("one reserved launch row expected, got %v", states)
	}

	// Simulated failure between the two writes: the reservation insert
	// aborts, so the attempt insert must roll back with it.
	if _, err := reopened.DB().Exec(`CREATE TRIGGER agy_fail_reserve BEFORE INSERT ON agy_attempt_launches
BEGIN SELECT RAISE(ABORT, 'simulated reservation failure'); END;`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}
	if _, err := reopened.InsertAgyTurnAttemptAndReserveLaunch(ctx, agyAttemptFixture("att-f", "sess-r", "t-f"), "fixture"); err == nil {
		t.Fatal("the simulated reservation failure must surface")
	}
	again := reopenStore(t, reopened)
	if a, err := again.GetAgyTurnAttempt(ctx, "att-f"); err != nil || a != nil {
		t.Fatalf("a failed reservation must leave no attempt behind, got %+v err=%v", a, err)
	}
	if states, _ := again.AgyAttemptLaunchStates(ctx, "att-f"); len(states) != 0 {
		t.Fatalf("no launch rows expected, got %v", states)
	}
	// The same attempt reserved twice through the combined path fails.
	if _, err := again.InsertAgyTurnAttemptAndReserveLaunch(ctx, agyAttemptFixture("att-r", "sess-r", "t-r"), "fixture"); err == nil {
		t.Fatal("a duplicate attempt must be refused")
	}
}

func TestAgyUncertainty_EpisodeCarriesOrphanNativeID(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	const orphan = "3f2c1a9e-8b7d-4c6e-9f01-23456789abcd"
	if _, _, err := store.RecordAgyCreationUncertain(ctx, AgyCreationUncertainty{RunID: agyUncRunID, SessionID: agyUncSession,
		Reason: "profile drift after init", RecordedBy: "agy-adapter", CauseOpID: "op-c", OrphanNativeID: orphan}); err != nil {
		t.Fatalf("record: %v", err)
	}
	ep, err := store.OpenAgyCreationUncertainty(ctx, agyUncSession)
	if err != nil || ep == nil || ep.OrphanNativeID == nil || *ep.OrphanNativeID != orphan {
		t.Fatalf("open episode must carry the orphan id, got %+v err=%v", ep, err)
	}
	eps, err := store.AgyCreationUncertaintyEpisodes(ctx, agyUncSession)
	if err != nil || len(eps) != 1 || eps[0].OrphanNativeID == nil {
		t.Fatalf("episode history must carry the orphan id, got %+v err=%v", eps, err)
	}
}
