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
	// Simulate stdin transmission, then crash before acceptance.
	if err := store.RecordClaudeStdinTransmitted(ctx, "att-c", 1); err != nil {
		t.Fatalf("record stdin: %v", err)
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

	// Protected mode: the redispatch requires a valid frozen attestation
	// and a separately recorded positive-absence decision.
	attempt := claudeAttemptFixture("att-rd", "sess-rd", "t-rd", "nat-rd")
	attempt.TranscriptProtection = "protected"
	attempt.AttestationID = "cprot-v1:sha256:fixed-test-id"
	if err := store.InsertClaudeTurnAttempt(ctx, attempt); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO claude_protection_attestations
	(attestation_id, claude_version, platform, manifest_digest, template_digest,
	 probe_results, probed_at, actor)
VALUES ('cprot-v1:sha256:fixed-test-id', '2.1.278', 'test', 'md', 'td', '[]', '2026-09-22T00:00:00Z', 'op')`,
	); err != nil {
		t.Fatalf("insert attestation: %v", err)
	}
	// Launch 1: started then dead (process death alone is insufficient).
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
	// Separately record the positive-absence decision (controller action).
	if err := store.RecordClaudeAbsenceVerified(ctx, "att-rd"); err != nil {
		t.Fatalf("absence verified: %v", err)
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

	// Protected mode with a valid frozen attestation.
	attempt := claudeAttemptFixture("att-e", "sess-e", "t-e", "nat-e")
	attempt.TranscriptProtection = "protected"
	attempt.AttestationID = "cprot-v1:sha256:fixed-test-id"
	if err := store.InsertClaudeTurnAttempt(ctx, attempt); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO claude_protection_attestations
	(attestation_id, claude_version, platform, manifest_digest, template_digest,
	 probe_results, probed_at, actor)
VALUES ('cprot-v1:sha256:fixed-test-id', '2.1.278', 'test', 'md', 'td', '[]', '2026-09-22T00:00:00Z', 'op')`,
	); err != nil {
		t.Fatalf("insert attestation: %v", err)
	}
	// Launch 1: started then dead.
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
	// Separately record the positive-absence decision (controller action).
	if err := store.RecordClaudeAbsenceVerified(ctx, "att-e"); err != nil {
		t.Fatalf("absence verified: %v", err)
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
	if err := store.SetClaudeAttemptTerminal(ctx, "att-once", "completed", "first result", `{"cost":1}`); err != nil {
		t.Fatalf("first terminal: %v", err)
	}
	// A second (different) result must be rejected by the exactly-once guard.
	if err := store.SetClaudeAttemptTerminal(ctx, "att-once", "completed", "second result", `{"cost":2}`); err == nil {
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

// A verified error result (error_max_turns) is terminal with
// observed_status='failed', not 'completed'. Invalid statuses are
// rejected.
func TestClaudeState_TerminalFailedStatusMapping(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-fail", "sess-fail", "t-fail", "nat-fail")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.SetClaudeAttemptTerminal(ctx, "att-fail", "error_max_turns", "max turns reached", `{"subtype":"error_max_turns"}`); err == nil {
		t.Fatal("error_max_turns is not a valid observed status; must be rejected")
	}
	if err := store.SetClaudeAttemptTerminal(ctx, "att-fail", "failed", "max turns reached", `{"subtype":"error_max_turns"}`); err != nil {
		t.Fatalf("failed terminal: %v", err)
	}
	a, err := store.GetClaudeTurnAttempt(ctx, "att-fail")
	if err != nil || a == nil {
		t.Fatalf("get: %v", err)
	}
	if !a.Terminal || a.ObservedStatus != "failed" {
		t.Fatalf("expected terminal failed, got terminal=%v status=%q", a.Terminal, a.ObservedStatus)
	}
}

// Launch states are returned in reservation order for reconciliation
// evidence.
func TestClaudeState_LaunchStatesInReservationOrder(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-states", "sess-states", "t-states", "nat-states")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq1, err := store.ReserveClaudeLaunch(ctx, "att-states", "fixture")
	if err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-states", seq1, "start_failed", nil); err != nil {
		t.Fatalf("start_failed: %v", err)
	}
	seq2, err := store.ReserveClaudeLaunch(ctx, "att-states", "fixture")
	if err != nil {
		t.Fatalf("reserve 2 (after start_failed slot release): %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-states", seq2, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}

	states, err := store.ClaudeAttemptLaunchStates(ctx, "att-states")
	if err != nil {
		t.Fatalf("launch states: %v", err)
	}
	if len(states) != 2 || states[0] != "start_failed" || states[1] != "started" {
		t.Fatalf("expected [start_failed started] in reservation order, got %v", states)
	}
}

// A definitive start failure classifies the attempt as missing —
// positive pre-start evidence — which never blocks the native session.
func TestClaudeState_StartFailedClassifiesMissing(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-miss", "sess-miss", "t-miss", "nat-miss")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveClaudeLaunch(ctx, "att-miss", "fixture")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-miss", seq, "start_failed", nil); err != nil {
		t.Fatalf("start_failed: %v", err)
	}
	a, err := store.GetClaudeTurnAttempt(ctx, "att-miss")
	if err != nil || a == nil {
		t.Fatalf("get: %v", err)
	}
	if a.ObservedStatus != "missing" || a.Terminal {
		t.Fatalf("start failure must classify missing, got status=%q terminal=%v", a.ObservedStatus, a.Terminal)
	}
	blocked, err := store.HasClaudeUnresolvedAttempts(ctx, "nat-miss")
	if err != nil {
		t.Fatalf("unresolved query: %v", err)
	}
	if blocked {
		t.Fatal("a missing attempt must not count as unresolved")
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

// The schema version must be asserted exactly: at least one test pins
// the current version to 6 (AC-009 advanced the AC-008 pin) to prevent
// migration omission from passing.
func TestClaudeState_SchemaVersionIsExactly6(t *testing.T) {
	store := openClaudeStore(t)
	ver, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if ver != 6 {
		t.Fatalf("expected schema version exactly 6, got %d", ver)
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

// Advisory-mode attempts cannot redispatch after verified absence: the
// protection check fails closed.
func TestClaudeState_AdvisoryModeBlocksRedispatch(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	// Advisory attempt (the default): started then dead.
	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-adv", "sess-adv", "t-adv", "nat-adv")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.ReserveClaudeLaunch(ctx, "att-adv", "fixture"); err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	exit := 0
	if err := store.RecordClaudeLaunchState(ctx, "att-adv", 1, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-adv", 1, "dead", &exit); err != nil {
		t.Fatalf("dead: %v", err)
	}

	_, err := store.ReserveClaudeLaunch(ctx, "att-adv", "fixture")
	if err == nil || !strings.Contains(err.Error(), "protected transcript evidence") {
		t.Fatalf("advisory-mode redispatch must fail closed, got %v", err)
	}
}

// The stdin transmission boundary is durably recorded on the launch row.
func TestClaudeState_StdinTransmissionRecorded(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-stdin", "sess-stdin", "t-stdin", "nat-stdin")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveClaudeLaunch(ctx, "att-stdin", "fixture")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-stdin", seq, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := store.RecordClaudeStdinTransmitted(ctx, "att-stdin", seq); err != nil {
		t.Fatalf("record stdin: %v", err)
	}

	// Reopen and verify the launch row carries the stdin boundary.
	reopened := reopenStore(t, store)
	var stdinAt *string
	err = reopened.DB().QueryRow(
		`SELECT first_stdin_byte_at FROM claude_attempt_launches WHERE attempt_id = ? AND reservation_seq = ?`,
		"att-stdin", seq,
	).Scan(&stdinAt)
	if err != nil {
		t.Fatalf("read stdin boundary: %v", err)
	}
	if stdinAt == nil || *stdinAt == "" {
		t.Fatal("first_stdin_byte_at must be recorded after transmission")
	}
}

// Absence verification fails closed without a valid attestation row.
func TestClaudeState_AbsenceVerifiedFailsWithoutAttestation(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	attempt := claudeAttemptFixture("att-noatt", "sess-noatt", "t-noatt", "nat-noatt")
	attempt.TranscriptProtection = "protected"
	attempt.AttestationID = "cprot-v1:sha256:nonexistent"
	if err := store.InsertClaudeTurnAttempt(ctx, attempt); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The attestation row does not exist: the verification must fail.
	if err := store.RecordClaudeAbsenceVerified(ctx, "att-noatt"); err == nil {
		t.Fatal("absence verification without a valid attestation row must fail")
	}
}

// A duplicate stdin-transmission call does not overwrite the original
// ambiguity boundary timestamp.
func TestClaudeState_StdinTransmissionFirstWriteWins(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	if err := store.InsertClaudeTurnAttempt(ctx, claudeAttemptFixture("att-stdin-wins", "sess-stdin-wins", "t-stdin-wins", "nat-stdin-wins")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveClaudeLaunch(ctx, "att-stdin-wins", "fixture")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-stdin-wins", seq, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}

	// First call: records the boundary.
	if err := store.RecordClaudeStdinTransmitted(ctx, "att-stdin-wins", seq); err != nil {
		t.Fatalf("first stdin: %v", err)
	}

	var first string
	err = store.DB().QueryRow(
		`SELECT first_stdin_byte_at FROM claude_attempt_launches
		 WHERE attempt_id = ? AND reservation_seq = ?`, "att-stdin-wins", seq,
	).Scan(&first)
	if err != nil {
		t.Fatalf("read boundary: %v", err)
	}

	// A duplicate call must not change the timestamp (the WHERE clause
	// requires first_stdin_byte_at IS NULL, so it is a no-op).
	if err := store.RecordClaudeStdinTransmitted(ctx, "att-stdin-wins", seq); err == nil {
		// Idempotent no-op is also acceptable, but the timestamp must be
		// unchanged either way.
	}
	var second string
	err = store.DB().QueryRow(
		`SELECT first_stdin_byte_at FROM claude_attempt_launches
		 WHERE attempt_id = ? AND reservation_seq = ?`, "att-stdin-wins", seq,
	).Scan(&second)
	if err != nil {
		t.Fatalf("read boundary 2: %v", err)
	}
	if first != second {
		t.Fatalf("duplicate stdin call must not change the boundary, got %q then %q", first, second)
	}
}

// Absence verification is service-derived protected evidence, not a
// controller decision: it cannot be set on advisory-mode attempts.
func TestClaudeState_AbsenceVerifiedAdvisoryModeFails(t *testing.T) {
	store := openClaudeStore(t)
	ctx := context.Background()

	attempt := claudeAttemptFixture("att-adv-abs", "sess-adv-abs", "t-adv-abs", "nat-adv-abs")
	// TranscriptProtection is "advisory" (zero value in the fixture)
	if err := store.InsertClaudeTurnAttempt(ctx, attempt); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := store.RecordClaudeAbsenceVerified(ctx, "att-adv-abs"); err == nil {
		t.Fatal("advisory-mode attempts cannot have absence verified")
	}
}
