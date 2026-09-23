package storage

// Crash-boundary regressions for the Codex adapter durable state
// (spec §3.11): every crash gap between durable transitions is
// verified across a store reopen — the classification must survive the
// restart.

import (
	"context"
	"strings"
	"testing"
)

func openCodexStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(StoreOptions{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func codexAttemptFixture(attemptID, sessionID, turnKey string) CodexTurnAttempt {
	return CodexTurnAttempt{
		AttemptID: attemptID, SessionID: sessionID, TurnKey: turnKey,
		PromptDigest:     "pdig-v1:sha256:fixed-test-digest",
		BaselineIdentity: "0000:00", BaselineSize: 0, BaselineEntries: 0,
		RolloutProtection: "advisory",
	}
}

func codexProtectedAttempt(attemptID, sessionID, turnKey string) CodexTurnAttempt {
	attempt := codexAttemptFixture(attemptID, sessionID, turnKey)
	attempt.RolloutProtection = "protected"
	attempt.AttestationID = codexAttestationIDPtr()
	return attempt
}

func codexAttestationID() string {
	return "cprot-v2:sha256:" + strings.Repeat("ab", 32)
}

func codexAttestationIDPtr() *string {
	id := codexAttestationID()
	return &id
}

func insertCodexAttestation(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.DB().ExecContext(context.Background(), `
INSERT INTO codex_protection_attestations
	(attestation_id, codex_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, '0.154.0', 'test', 'sha256:md', 'cprof-v3:sha256:pd', '[]', '2026-09-23T00:00:00Z', 'op')`,
		codexAttestationID()); err != nil {
		t.Fatalf("insert attestation: %v", err)
	}
}

func insertCodexBinding(t *testing.T, store *Store, sessionID, nativeID string) {
	t.Helper()
	if err := store.InsertCodexSessionBinding(context.Background(), CodexSessionBinding{
		SessionID: sessionID, NativeID: nativeID,
		Model: "gpt-5-codex", Workspace: "/tmp/ws",
		ProfileDigest: "cprof-v3:sha256:pd",
	}); err != nil {
		t.Fatalf("insert binding: %v", err)
	}
}

// Crash before launch reservation: durable state proves no process was
// authorized — this is pre-start evidence, so the attempt remains safely
// dispatchable (not uncertain-blocked).
func TestCodexState_CrashBeforeLaunchReservation(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-a", "sess-a", "t-a")); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Reopen.
	reopened := reopenStore(t, store)

	a, err := reopened.GetCodexTurnAttempt(ctx, "att-a")
	if err != nil || a == nil {
		t.Fatalf("attempt must survive reopen, got %+v err=%v", a, err)
	}
	if a.LaunchCount != 0 {
		t.Fatalf("launch_count must be 0 (process never started), got %d", a.LaunchCount)
	}
	// Pre-start evidence: a fresh reservation is safely authorized.
	seq, err := reopened.ReserveCodexLaunch(ctx, "att-a", "fixture", 0)
	if err != nil || seq != 1 {
		t.Fatalf("pre-start attempt must remain safely dispatchable, got seq=%d err=%v", seq, err)
	}
}

// Crash boundary (b): launch reserved durably but process never started
// (started_at NULL, state reserved). On reopen the launch_count is 1
// and no redispatch is authorized.
func TestCodexState_CrashAfterReservationBeforeStart(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-b", "sess-b", "t-b")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveCodexLaunch(ctx, "att-b", "fixture", 0)
	if err != nil || seq != 1 {
		t.Fatalf("reserve: seq=%d err=%v", seq, err)
	}

	// Reopen: the reserved row must survive with state=reserved.
	reopened := reopenStore(t, store)

	a, err := reopened.GetCodexTurnAttempt(ctx, "att-b")
	if err != nil || a == nil {
		t.Fatalf("attempt must survive reopen: %v", err)
	}
	if a.LaunchCount != 1 {
		t.Fatalf("launch_count must be 1, got %d", a.LaunchCount)
	}
	// Redispatch precondition: count=1, consumed=0, prior dead = 0 →
	// the redispatch cannot authorize (the process is possibly running).
	_, err = reopened.ReserveCodexLaunch(ctx, "att-b", "fixture", 0)
	if err == nil {
		t.Fatal("redispatch must not authorize without verified absence and a dead prior launch")
	}
}

// Crash boundary (c): Start succeeded but started_at never persisted —
// the reserved row with started_at NULL means possibly-running; the
// attempt must remain uncertain and no redispatch is authorized.
func TestCodexState_CrashAfterStartBeforeStartedAt(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-c", "sess-c", "t-c")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.ReserveCodexLaunch(ctx, "att-c", "fixture", 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Simulate crash AFTER executor.Start but BEFORE started_at was
	// recorded: the reservation exists (count=1) but started_at is NULL.

	// Reopen and verify: the attempt is possibly-running.
	reopened := reopenStore(t, store)
	a, err := reopened.GetCodexTurnAttempt(ctx, "att-c")
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
	_, err = reopened.ReserveCodexLaunch(ctx, "att-c", "fixture", 0)
	if err == nil {
		t.Fatal("redispatch must not authorize while the process is possibly running")
	}
}

// Crash boundary (d): stdin transmitted but acceptance never recorded.
// accepted stays NULL; observed_status stays uncertain.
func TestCodexState_CrashAfterStdinBeforeAcceptance(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-d", "sess-d", "t-d")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.ReserveCodexLaunch(ctx, "att-d", "fixture", 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-d", 1, "started", nil); err != nil {
		t.Fatalf("mark started: %v", err)
	}
	// Simulate stdin transmission, then crash before acceptance.
	if err := store.RecordCodexStdinTransmitted(ctx, "att-d", 1); err != nil {
		t.Fatalf("record stdin: %v", err)
	}

	// Reopen: accepted must be NULL (unknown), not false.
	reopened := reopenStore(t, store)
	a, err := reopened.GetCodexTurnAttempt(ctx, "att-d")
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

// Crash boundary (e): after verified absence is recorded but before the
// redispatch consumption transaction, a crash leaves the consumed flag
// unset. On reopen the decision re-derives: exactly one new reservation.
func TestCodexState_RedispatchDecisionBeforeConsumption(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	// Protected mode with a valid frozen cprot-v2 attestation.
	if err := store.InsertCodexTurnAttempt(ctx, codexProtectedAttempt("att-e", "sess-e", "t-e")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	insertCodexAttestation(t, store)
	// Launch 1: started then dead.
	if _, err := store.ReserveCodexLaunch(ctx, "att-e", "fixture", 0); err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-e", 1, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	exit := 0
	if err := store.RecordCodexLaunchState(ctx, "att-e", 1, "dead", &exit); err != nil {
		t.Fatalf("dead: %v", err)
	}
	// Separately record the positive-absence decision.
	if err := store.RecordCodexAbsenceVerified(ctx, "att-e"); err != nil {
		t.Fatalf("absence verified: %v", err)
	}

	// Reopen: the redispatch decision re-derives exactly one reservation.
	reopened := reopenStore(t, store)
	seq2, err := reopened.ReserveCodexLaunch(ctx, "att-e", "fixture", 1)
	if err != nil || seq2 != 2 {
		t.Fatalf("redispatch decision must re-derive after crash, got seq=%d err=%v", seq2, err)
	}

	// The consumed authorization cannot produce a third launch.
	_, err = reopened.ReserveCodexLaunch(ctx, "att-e", "fixture", 1)
	if err == nil {
		t.Fatal("third launch must be rejected")
	}
}

// Retained start_failed rows neither collide on reservation_seq nor
// consume the two-started-launch cap.
func TestCodexState_StartFailedRetainedNoCollision(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-sf", "sess-sf", "t-sf")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq1, err := store.ReserveCodexLaunch(ctx, "att-sf", "fixture", 0)
	if err != nil || seq1 != 1 {
		t.Fatalf("reserve 1: %d %v", seq1, err)
	}
	exit := 1
	if err := store.RecordCodexLaunchState(ctx, "att-sf", seq1, "start_failed", &exit); err != nil {
		t.Fatalf("mark start_failed: %v", err)
	}

	// A new reservation takes seq 2 without colliding.
	seq2, err := store.ReserveCodexLaunch(ctx, "att-sf", "fixture", 0)
	if err != nil || seq2 != 2 {
		t.Fatalf("reserve 2 after start_failed: %d %v", seq2, err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-sf", seq2, "started", nil); err != nil {
		t.Fatalf("mark started: %v", err)
	}

	// The failed row is retained as evidence with its own seq.
	rows, err := store.DB().QueryContext(ctx, `
SELECT reservation_seq, state FROM codex_attempt_launches
WHERE attempt_id = 'att-sf' ORDER BY reservation_seq`)
	if err != nil {
		t.Fatalf("query launch rows: %v", err)
	}
	defer rows.Close()
	var seqs []int64
	var states []string
	for rows.Next() {
		var seq int64
		var state string
		if err := rows.Scan(&seq, &state); err != nil {
			t.Fatalf("scan launch row: %v", err)
		}
		seqs = append(seqs, seq)
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("retained rows must keep their seqs, got %v", seqs)
	}
	if len(states) != 2 || states[0] != "start_failed" || states[1] != "started" {
		t.Fatalf("expected [start_failed started] retained in order, got %v", states)
	}

	// The started/dead count is 1 (the failed row doesn't count): the
	// verified-absence redispatch is still available.
	consumed, err := store.ReserveCodexLaunch(ctx, "att-sf", "fixture", 0)
	_ = consumed
	if err == nil {
		t.Fatal("redispatch requires explicit verified absence; without it, must fail")
	}
}

// Consumed-before-second-Start: the absence redispatch authorization is
// consumed (flag set) and the second launch reserved; a third launch is
// rejected — the cap never exceeds two started launches.
func TestCodexState_RedispatchConsumedCannotAuthorizeThird(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	// Protected mode: the redispatch requires a valid frozen attestation
	// and a separately recorded positive-absence decision.
	if err := store.InsertCodexTurnAttempt(ctx, codexProtectedAttempt("att-rd", "sess-rd", "t-rd")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	insertCodexAttestation(t, store)
	// Launch 1: started then dead (process death alone is insufficient).
	if _, err := store.ReserveCodexLaunch(ctx, "att-rd", "fixture", 0); err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-rd", 1, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	exit := 0
	if err := store.RecordCodexLaunchState(ctx, "att-rd", 1, "dead", &exit); err != nil {
		t.Fatalf("dead: %v", err)
	}
	// Separately record the positive-absence decision.
	if err := store.RecordCodexAbsenceVerified(ctx, "att-rd"); err != nil {
		t.Fatalf("absence verified: %v", err)
	}

	// Consume the redispatch authorization.
	seq2, err := store.ReserveCodexLaunch(ctx, "att-rd", "fixture", 1)
	if err != nil || seq2 != 2 {
		t.Fatalf("redispatch reservation: %d %v", seq2, err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-rd", seq2, "started", nil); err != nil {
		t.Fatalf("mark started 2: %v", err)
	}

	// A third launch must be rejected by the count constraint.
	_, err = store.ReserveCodexLaunch(ctx, "att-rd", "fixture", 1)
	if err == nil {
		t.Fatal("third launch must be rejected")
	}
	if !stringsContains(err.Error(), "precondition failed") {
		t.Fatalf("expected precondition error, got %v", err)
	}
}

// Terminal persistence is exactly-once: the second call cannot
// overwrite the first.
func TestCodexState_TerminalPersistenceExactlyOnce(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-once", "sess-once", "t-once")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.SetCodexAttemptTerminal(ctx, "att-once", "completed", "first result", `{"cost":1}`); err != nil {
		t.Fatalf("first terminal: %v", err)
	}
	// A second (different) result must be rejected by the exactly-once guard.
	if err := store.SetCodexAttemptTerminal(ctx, "att-once", "completed", "second result", `{"cost":2}`); err == nil {
		t.Fatal("second terminal must fail with the exactly-once guard")
	}
	a, err := store.GetCodexTurnAttempt(ctx, "att-once")
	if err != nil || a == nil {
		t.Fatalf("get: %v", err)
	}
	if a.ResultPayload == nil || *a.ResultPayload != "first result" {
		t.Fatalf("first result must be preserved, got %v", a.ResultPayload)
	}
}

// A verified error result (turn failed) is terminal with
// observed_status='failed', not 'completed'. Invalid statuses are
// rejected.
func TestCodexState_TerminalFailedStatusMapping(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-fail", "sess-fail", "t-fail")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.SetCodexAttemptTerminal(ctx, "att-fail", "error_max_turns", "turn failed", `{"error":"turn failed"}`); err == nil {
		t.Fatal("error_max_turns is not a valid observed status; must be rejected")
	}
	if err := store.SetCodexAttemptTerminal(ctx, "att-fail", "failed", "turn failed", `{"error":"turn failed"}`); err != nil {
		t.Fatalf("failed terminal: %v", err)
	}
	a, err := store.GetCodexTurnAttempt(ctx, "att-fail")
	if err != nil || a == nil {
		t.Fatalf("get: %v", err)
	}
	if !a.Terminal || a.ObservedStatus != "failed" {
		t.Fatalf("expected terminal failed, got terminal=%v status=%q", a.Terminal, a.ObservedStatus)
	}
}

// A definitive start failure classifies the attempt as missing —
// positive pre-start evidence — which never blocks the native session.
func TestCodexState_StartFailedClassifiesMissing(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	insertCodexBinding(t, store, "sess-miss", "01934f7a-1b2c-7def-9abc-def012345678")
	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-miss", "sess-miss", "t-miss")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveCodexLaunch(ctx, "att-miss", "fixture", 0)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-miss", seq, "start_failed", nil); err != nil {
		t.Fatalf("start_failed: %v", err)
	}
	a, err := store.GetCodexTurnAttempt(ctx, "att-miss")
	if err != nil || a == nil {
		t.Fatalf("get: %v", err)
	}
	if a.ObservedStatus != "missing" || a.Terminal {
		t.Fatalf("start failure must classify missing, got status=%q terminal=%v", a.ObservedStatus, a.Terminal)
	}
	blocked, err := store.HasCodexUnresolvedAttempts(ctx, "01934f7a-1b2c-7def-9abc-def012345678")
	if err != nil {
		t.Fatalf("unresolved query: %v", err)
	}
	if blocked {
		t.Fatal("a missing attempt must not count as unresolved")
	}
}

// Unresolved uncertain attempts without a disposition are reported.
func TestCodexState_UnresolvedAttemptsReported(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	insertCodexBinding(t, store, "sess-un", "01934f7a-1b2c-7def-9abc-def012345679")
	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-un", "sess-un", "t-un")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.ReserveCodexLaunch(ctx, "att-un", "fixture", 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-un", 1, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}

	unresolved, err := store.HasCodexUnresolvedAttempts(ctx, "01934f7a-1b2c-7def-9abc-def012345679")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !unresolved {
		t.Fatal("started-without-result attempt must be unresolved")
	}
}

// Advisory-mode attempts cannot redispatch after verified absence: the
// protection check fails closed.
func TestCodexState_AdvisoryModeBlocksRedispatch(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	// Advisory attempt (the default): started then dead.
	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-adv", "sess-adv", "t-adv")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.ReserveCodexLaunch(ctx, "att-adv", "fixture", 0); err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	exit := 0
	if err := store.RecordCodexLaunchState(ctx, "att-adv", 1, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-adv", 1, "dead", &exit); err != nil {
		t.Fatalf("dead: %v", err)
	}

	_, err := store.ReserveCodexLaunch(ctx, "att-adv", "fixture", 0)
	if err == nil || !strings.Contains(err.Error(), "protected rollout evidence") {
		t.Fatalf("advisory-mode redispatch must fail closed, got %v", err)
	}
}

// The stdin transmission boundary is durably recorded on the launch row.
func TestCodexState_StdinTransmissionRecorded(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-stdin", "sess-stdin", "t-stdin")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveCodexLaunch(ctx, "att-stdin", "fixture", 0)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-stdin", seq, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := store.RecordCodexStdinTransmitted(ctx, "att-stdin", seq); err != nil {
		t.Fatalf("record stdin: %v", err)
	}

	// Reopen and verify the launch row carries the stdin boundary.
	reopened := reopenStore(t, store)
	var stdinAt *string
	err = reopened.DB().QueryRow(
		`SELECT first_stdin_byte_at FROM codex_attempt_launches WHERE attempt_id = ? AND reservation_seq = ?`,
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
func TestCodexState_AbsenceVerifiedFailsWithoutAttestation(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	attempt := codexProtectedAttempt("att-noatt", "sess-noatt", "t-noatt")
	missing := "cprot-v2:sha256:" + strings.Repeat("ff", 32)
	attempt.AttestationID = &missing
	if err := store.InsertCodexTurnAttempt(ctx, attempt); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The attestation row does not exist: the verification must fail.
	if err := store.RecordCodexAbsenceVerified(ctx, "att-noatt"); err == nil {
		t.Fatal("absence verification without a valid attestation row must fail")
	}
}

// A duplicate stdin-transmission call does not overwrite the original
// ambiguity boundary timestamp.
func TestCodexState_StdinTransmissionFirstWriteWins(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-stdin-wins", "sess-stdin-wins", "t-stdin-wins")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveCodexLaunch(ctx, "att-stdin-wins", "fixture", 0)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-stdin-wins", seq, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}

	// First call: records the boundary.
	if err := store.RecordCodexStdinTransmitted(ctx, "att-stdin-wins", seq); err != nil {
		t.Fatalf("first stdin: %v", err)
	}

	var first string
	err = store.DB().QueryRow(
		`SELECT first_stdin_byte_at FROM codex_attempt_launches
		 WHERE attempt_id = ? AND reservation_seq = ?`, "att-stdin-wins", seq,
	).Scan(&first)
	if err != nil {
		t.Fatalf("read boundary: %v", err)
	}

	// A duplicate call must not change the timestamp (the WHERE clause
	// requires first_stdin_byte_at IS NULL, so it is a no-op).
	if err := store.RecordCodexStdinTransmitted(ctx, "att-stdin-wins", seq); err == nil {
		// Idempotent no-op is also acceptable, but the timestamp must be
		// unchanged either way.
	}
	var second string
	err = store.DB().QueryRow(
		`SELECT first_stdin_byte_at FROM codex_attempt_launches
		 WHERE attempt_id = ? AND reservation_seq = ?`, "att-stdin-wins", seq,
	).Scan(&second)
	if err != nil {
		t.Fatalf("read boundary 2: %v", err)
	}
	if first != second {
		t.Fatalf("duplicate stdin call must not change the boundary, got %q then %q", first, second)
	}
}

// Absence verification is derived protected evidence: it cannot be set
// on advisory-mode attempts.
func TestCodexState_AbsenceVerifiedAdvisoryModeFails(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-adv-abs", "sess-adv-abs", "t-adv-abs")); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := store.RecordCodexAbsenceVerified(ctx, "att-adv-abs"); err == nil {
		t.Fatal("advisory-mode attempts cannot have absence verified")
	}
}

// Session bindings round-trip: insert unmaterialized, duplicate native
// identity rejected, non-UUIDv7 native ids rejected by the schema
// CHECK, and materialization records the rollout path exactly once.
func TestCodexState_SessionBindingLifecycle(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	native := "01934f7a-1b2c-7def-9abc-def01234567a"
	insertCodexBinding(t, store, "sess-bind", native)

	b, err := store.GetCodexSessionBinding(ctx, "sess-bind")
	if err != nil || b == nil {
		t.Fatalf("get binding: %v", err)
	}
	if b.NativeID != native || b.Materialized {
		t.Fatalf("unexpected binding: %+v", b)
	}
	if b.RolloutPath != nil || b.FirstPromptDigest != nil {
		t.Fatalf("rollout_path and first_prompt_digest must start NULL, got %v %v", b.RolloutPath, b.FirstPromptDigest)
	}

	// A missing binding is nil, not an error.
	missing, err := store.GetCodexSessionBinding(ctx, "sess-missing")
	if err != nil || missing != nil {
		t.Fatalf("missing binding must be nil, got %+v err=%v", missing, err)
	}

	// Duplicate native identity is rejected (UNIQUE).
	err = store.InsertCodexSessionBinding(ctx, CodexSessionBinding{
		SessionID: "sess-other", NativeID: native,
		Model: "gpt-5-codex", Workspace: "/tmp/ws",
		ProfileDigest: "cprof-v3:sha256:pd",
	})
	if err == nil {
		t.Fatal("duplicate native id must be rejected")
	}

	// Non-UUIDv7 native ids are rejected by the schema CHECK.
	err = store.InsertCodexSessionBinding(ctx, CodexSessionBinding{
		SessionID: "sess-bad", NativeID: "01934f7a-1b2c-4def-9abc-def01234567a",
		Model: "gpt-5-codex", Workspace: "/tmp/ws",
		ProfileDigest: "cprof-v3:sha256:pd",
	})
	if err == nil {
		t.Fatal("non-UUIDv7 native id must be rejected by the schema CHECK")
	}

	// Materialization records the rollout path; a second call is a no-op
	// (only transitions false → true).
	rollout := "/home/op/.codex/sessions/01934f7a-1b2c-7def-9abc-def01234567a.jsonl"
	if err := store.MarkCodexSessionMaterialized(ctx, "sess-bind", rollout); err != nil {
		t.Fatalf("mark materialized: %v", err)
	}
	if err := store.MarkCodexSessionMaterialized(ctx, "sess-bind", "/other/path"); err != nil {
		t.Fatalf("second mark: %v", err)
	}
	b2, err := store.GetCodexSessionBinding(ctx, "sess-bind")
	if err != nil || b2 == nil {
		t.Fatalf("get binding 2: %v", err)
	}
	if !b2.Materialized {
		t.Fatal("binding must be materialized")
	}
	if b2.RolloutPath == nil || *b2.RolloutPath != rollout {
		t.Fatalf("rollout path must be recorded at materialization, got %v", b2.RolloutPath)
	}
}

// The latest attempt for a turn is returned in insertion order.
func TestCodexState_LatestAttemptPerTurn(t *testing.T) {
	store := openCodexStore(t)
	ctx := context.Background()

	if err := store.InsertCodexTurnAttempt(ctx, codexAttemptFixture("att-l1", "sess-l", "t-l")); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	latest, err := store.GetLatestCodexTurnAttempt(ctx, "sess-l", "t-l")
	if err != nil || latest == nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.AttemptID != "att-l1" {
		t.Fatalf("expected att-l1, got %q", latest.AttemptID)
	}

	// A missing turn returns nil, not an error.
	none, err := store.GetLatestCodexTurnAttempt(ctx, "sess-l", "t-none")
	if err != nil || none != nil {
		t.Fatalf("missing turn must be nil, got %+v err=%v", none, err)
	}
}
