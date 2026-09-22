package storage

// Task 5 crash-boundary regressions for the Claude adapter durable
// state (spec §3.11): every crash gap between durable transitions is
// verified across a store reopen — the classification must survive the
// restart.

import (
	"context"
	"strings"
	"testing"
)

func openClaudeStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(StoreOptions{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func reopenStore(t *testing.T, old *Store) *Store {
	t.Helper()
	old.Close()
	store, err := Open(StoreOptions{StateDir: old.StateDir()})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func stringsContains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func claudeAttemptFixture(attemptID, sessionID, turnKey, nativeID string) ClaudeTurnAttempt {
	return ClaudeTurnAttempt{
		AttemptID: attemptID, SessionID: sessionID, TurnKey: turnKey,
		NativeID: nativeID, PromptDigest: "sha256:pd",
		BaselineIdentity: "0000:00", BaselineSize: 0, BaselineEntries: 0,
		TranscriptProtection: "advisory",
	}
}

// Crash before launch reservation: durable state proves no process was
// authorized — this is pre-start evidence, so the attempt remains safely
// dispatchable (not uncertain-blocked).
func TestClaudeState_CrashBeforeLaunchReservation(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-a", "sess-a", "t-a", "nat-a")); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Reopen.
	reopened := reopenStore(t, store)

	a, err := reopened.GetClaudeTurnAttempt(ctx, "att-a")
	if err != nil || a == nil {
		t.Fatalf("attempt must survive reopen, got %+v err=%v", a, err)
	}
	if a.LaunchCount != 0 {
		t.Fatalf("launch_count must be 0 (process never started), got %d", a.LaunchCount)
	}
	// Pre-start evidence: a fresh reservation is safely authorized.
	seq, err := reopened.ReserveClaudeLaunch(ctx, "att-a", "fixture")
	if err != nil || seq != 1 {
		t.Fatalf("pre-start attempt must remain safely dispatchable, got seq=%d err=%v", seq, err)
	}
}

// Crash boundary (b): launch reserved durably but process never started
// (started_at NULL, state reserved). On reopen the launch_count is 1
// and no redispatch is authorized.
func TestClaudeState_CrashAfterReservationBeforeStart(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-b", "sess-b", "t-b", "nat-b")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveClaudeLaunch(ctx, "att-b", "fixture")
	if err != nil || seq != 1 {
		t.Fatalf("reserve: seq=%d err=%v", seq, err)
	}

	// Reopen: the reserved row must survive with state=reserved.
	reopened := reopenStore(t, store)

	a, err := reopened.GetClaudeTurnAttempt(ctx, "att-b")
	if err != nil || a == nil {
		t.Fatalf("attempt must survive reopen: %v", err)
	}
	if a.LaunchCount != 1 {
		t.Fatalf("launch_count must be 1, got %d", a.LaunchCount)
	}
	// Redispatch precondition: count=1, consumed=0, prior dead = 0 →
	// the redispatch cannot authorize (the process is possibly running).
	_, err = reopened.ReserveClaudeLaunch(ctx, "att-b", "fixture")
	if err == nil {
		t.Fatal("redispatch must not authorize without verified absence and a dead prior launch")
	}
}

// Crash boundary (c): stdin transmitted but acceptance never recorded.
// accepted stays NULL; observed_status stays uncertain.
func TestClaudeState_CrashAfterStdinBeforeAcceptance(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-c", "sess-c", "t-c", "nat-c")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.ReserveClaudeLaunch(ctx, "att-c", "fixture"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-c", 1, "started", nil); err != nil {
		t.Fatalf("mark started: %v", err)
	}

	// Reopen: accepted must be NULL (unknown), not false.
	reopened := reopenStore(t, store)
	a, err := reopened.GetClaudeTurnAttempt(ctx, "att-c")
	if err != nil || a == nil {
		t.Fatalf("attempt must survive: %v", err)
	}
	if a.Accepted != nil {
		t.Fatalf("accepted must be NULL (unknown), got %v", *a.Accepted)
	}
	if a.ObservedStatus != "uncertain" {
		t.Fatalf("status must be uncertain, got %q", a.ObservedStatus)
	}
}

// Retained start_failed rows neither collide on reservation_seq nor
// consume the two-started-launch cap.
func TestClaudeState_StartFailedRetainedNoCollision(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-sf", "sess-sf", "t-sf", "nat-sf")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq1, err := store.ReserveClaudeLaunch(ctx, "att-sf", "fixture")
	if err != nil || seq1 != 1 {
		t.Fatalf("reserve 1: %d %v", seq1, err)
	}
	exit := 1
	if err := store.RecordClaudeLaunchState(ctx, "att-sf", seq1, "start_failed", &exit); err != nil {
		t.Fatalf("mark start_failed: %v", err)
	}

	// A new reservation takes seq 2 without colliding.
	seq2, err := store.ReserveClaudeLaunch(ctx, "att-sf", "fixture")
	if err != nil || seq2 != 2 {
		t.Fatalf("reserve 2 after start_failed: %d %v", seq2, err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-sf", seq2, "started", nil); err != nil {
		t.Fatalf("mark started: %v", err)
	}

	// The started/dead count is 1 (the failed row doesn't count): the
	// verified-absence redispatch is still available.
	consumed, err := store.ReserveClaudeLaunch(ctx, "att-sf", "fixture")
	_ = consumed
	if err == nil {
		t.Fatal("redispatch requires explicit verified absence; without it, must fail")
	}
}

// Consumed-before-second-Start: the absence redispatch authorization is
// consumed (flag set) and the second launch reserved; a third launch is
// rejected by the count constraint.
func TestClaudeState_RedispatchConsumedCannotAuthorizeThird(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-rd", "sess-rd", "t-rd", "nat-rd")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Launch 1: started then dead (verified absence).
	if _, err := store.ReserveClaudeLaunch(ctx, "att-rd", "fixture"); err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-rd", 1, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	exit := 0
	if err := store.RecordClaudeLaunchState(ctx, "att-rd", 1, "dead", &exit); err != nil {
		t.Fatalf("dead: %v", err)
	}

	// Consume the redispatch authorization.
	seq2, err := store.ReserveClaudeLaunch(ctx, "att-rd", "fixture")
	if err != nil || seq2 != 2 {
		t.Fatalf("redispatch reservation: %d %v", seq2, err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-rd", seq2, "started", nil); err != nil {
		t.Fatalf("mark started 2: %v", err)
	}

	// A third launch must be rejected by the count constraint.
	_, err = store.ReserveClaudeLaunch(ctx, "att-rd", "fixture")
	if err == nil {
		t.Fatal("third launch must be rejected")
	}
	if !stringsContains(err.Error(), "precondition failed") {
		t.Fatalf("expected precondition error, got %v", err)
	}
}

// Crash boundary (b): Start succeeds but started_at never persists —
// the reserved row with started_at NULL means possibly-running; the
// attempt must remain uncertain and no redispatch is authorized.
func TestClaudeState_CrashAfterStartBeforeStartedAt(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-d", "sess-d", "t-d", "nat-d")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.ReserveClaudeLaunch(ctx, "att-d", "fixture"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Simulate crash AFTER executor.Start but BEFORE started_at was
	// recorded: the reservation exists (count=1) but started_at is NULL.

	// Reopen and verify: the attempt is possibly-running.
	reopened := reopenStore(t, store)
	a, err := reopened.GetClaudeTurnAttempt(ctx, "att-d")
	if err != nil || a == nil {
		t.Fatalf("attempt must survive: %v", err)
	}
	if a.LaunchCount != 1 {
		t.Fatalf("launch_count must be 1, got %d", a.LaunchCount)
	}
	if a.ObservedStatus != "uncertain" {
		t.Fatalf("possibly-running attempt must be uncertain, got %q", a.ObservedStatus)
	}

	// No redispatch is authorized: the process is possibly running.
	_, err = reopened.ReserveClaudeLaunch(ctx, "att-d", "fixture")
	if err == nil {
		t.Fatal("redispatch must not authorize while the process is possibly running")
	}
}

// Crash boundary (d): after verified absence is recorded but before the
// redispatch consumption transaction, a crash leaves the consumed flag
// unset. On reopen the decision re-derives: exactly one new reservation.
func TestClaudeState_RedispatchDecisionBeforeConsumption(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-e", "sess-e", "t-e", "nat-e")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Launch 1: started then dead (verified absence).
	if _, err := store.ReserveClaudeLaunch(ctx, "att-e", "fixture"); err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	exit := 0
	if err := store.RecordClaudeLaunchState(ctx, "att-e", 1, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-e", 1, "dead", &exit); err != nil {
		t.Fatalf("dead: %v", err)
	}

	// Reopen: the redispatch decision re-derives exactly one reservation.
	reopened := reopenStore(t, store)
	seq2, err := reopened.ReserveClaudeLaunch(ctx, "att-e", "fixture")
	if err != nil || seq2 != 2 {
		t.Fatalf("redispatch decision must re-derive after crash, got seq=%d err=%v", seq2, err)
	}

	// The consumed authorization cannot produce a third launch.
	_, err = reopened.ReserveClaudeLaunch(ctx, "att-e", "fixture")
	if err == nil {
		t.Fatal("third launch must be rejected")
	}
}

// Terminal persistence is exactly-once: the second call cannot
// overwrite the first.
func TestClaudeState_TerminalPersistenceExactlyOnce(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-once", "sess-once", "t-once", "nat-once")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.SetClaudeAttemptTerminal(ctx, "att-once", "first result", `{"cost":1}`); err != nil {
		t.Fatalf("first terminal: %v", err)
	}
	// A second (different) result must be rejected by the exactly-once guard.
	if err := store.SetClaudeAttemptTerminal(ctx, "att-once", "second result", `{"cost":2}`); err == nil {
		t.Fatal("second terminal must fail with the exactly-once guard")
	}
	a, err := store.GetClaudeTurnAttempt(ctx, "att-once")
	if err != nil || a == nil {
		t.Fatalf("get: %v", err)
	}
	if a.ResultPayload == nil || *a.ResultPayload != "first result" {
		t.Fatalf("first result must be preserved, got %v", a.ResultPayload)
	}
}

// Unresolved uncertain attempts without a disposition are reported.
func TestClaudeState_UnresolvedAttemptsReported(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-un", "sess-un", "t-un", "nat-un")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.ReserveClaudeLaunch(ctx, "att-un", "fixture"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-un", 1, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}

	unresolved, err := store.HasClaudeUnresolvedAttempts(ctx, "nat-un")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !unresolved {
		t.Fatal("started-without-result attempt must be unresolved")
	}
}

// The schema-v4 migration must be asserted exactly: at least one test
// pins the version to 4 to prevent v4 migration omission from passing.
func TestClaudeState_SchemaVersionIsExactly4(t *testing.T) {
	store := openClaudeStore(t)
	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if ver != 4 {
		t.Fatalf("expected schema version exactly 4, got %d", ver)
	}
	// The AC-008 tables must exist.
	for _, table := range []string{
		"claude_session_bindings", "claude_turn_attempts",
		"claude_attempt_launches", "claude_protection_attestations",
	} {
		var name string
		err := store.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %s must exist: %v", table, err)
		}
	}
}
