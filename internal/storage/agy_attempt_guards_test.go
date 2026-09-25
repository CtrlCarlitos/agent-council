package storage

import (
	"context"
	"testing"
)

// A queue_prompt op journaled before v7 (fingerprint without a
// required-tools part) replays: a stated-empty tools set leaves the
// fingerprint unchanged. A stated set still distinguishes the op.
func TestQueuePrompt_ReplaysPreV7JournalEntry(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	ver, err := store.GetSessionVersion(ctx, agyUncSession)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	prompt := PendingPrompt{SessionID: agyUncSession, TurnKey: "t-prev7", Prompt: "legacy prompt"}
	first, err := store.QueuePrompt(ctx, "op-q-prev7", agyUncLease, agyUncSession, ver, prompt)
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	// Rewrite the journal entry into its pre-v7 shape: the fingerprint
	// a pre-v7 binary recorded had no required-tools part.
	preV7 := computeFingerprint("queue_prompt", agyUncSession, "t-prev7", SanitizeText("legacy prompt"))
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE journal_entries SET command_fingerprint = ? WHERE op_id = ?`, preV7, "op-q-prev7"); err != nil {
		t.Fatalf("rewrite journal entry: %v", err)
	}
	replay, err := store.QueuePrompt(ctx, "op-q-prev7", agyUncLease, agyUncSession, ver, prompt)
	if err != nil {
		t.Fatalf("a pre-v7 queue_prompt op must replay, got %v", err)
	}
	if replay.OpID != first.OpID || replay.CommittedVersion != first.CommittedVersion {
		t.Fatalf("replay returns the committed receipt, got %+v want %+v", replay, first)
	}
	withTools := prompt
	withTools.RequiredTools = []string{"view_file"}
	if _, err := store.QueuePrompt(ctx, "op-q-prev7", agyUncLease, agyUncSession, ver, withTools); err == nil {
		t.Fatal("the same op id with a stated tools set is a different command (idempotency conflict)")
	}
}

// SetAgyAttemptObservedStatus allows only uncertain -> missing on a
// non-terminal attempt (idempotent re-record); it never rewrites a
// terminal attempt and never sets any other status.
func TestAgyState_ObservedStatusGuard(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()
	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-os", "sess-os", "t-os")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for _, bad := range []string{"completed", "failed", "cancelled", "uncertain", "weird"} {
		if err := store.SetAgyAttemptObservedStatus(ctx, "att-os", bad); err == nil {
			t.Fatalf("status %q must be refused", bad)
		}
	}
	if err := store.SetAgyAttemptObservedStatus(ctx, "att-os", "missing"); err != nil {
		t.Fatalf("uncertain -> missing: %v", err)
	}
	if err := store.SetAgyAttemptObservedStatus(ctx, "att-os", "missing"); err != nil {
		t.Fatalf("re-recording missing is idempotent: %v", err)
	}
	if err := store.SetAgyAttemptTerminal(ctx, "att-os", "completed", "r", "{}", AgyVerification{}); err == nil {
		t.Fatal("a missing attempt never gains a terminal result")
	}

	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-os-term", "sess-os-term", "t-os-term")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.SetAgyAttemptTerminal(ctx, "att-os-term", "completed", "r", "{}", AgyVerification{}); err != nil {
		t.Fatalf("terminal: %v", err)
	}
	if err := store.SetAgyAttemptObservedStatus(ctx, "att-os-term", "missing"); err == nil {
		t.Fatal("a terminal attempt's observed status is never rewritten")
	}
	a, err := store.GetAgyTurnAttempt(ctx, "att-os-term")
	if err != nil || a == nil || a.ObservedStatus != "completed" || !a.Terminal {
		t.Fatalf("the terminal attempt is unchanged, got %+v err=%v", a, err)
	}
	if err := store.SetAgyAttemptObservedStatus(ctx, "att-absent", "missing"); err == nil {
		t.Fatal("an unknown attempt is an error")
	}
}

// A dispatch intent's required_tools_json that does not decode fails
// closed: the details carry the decode error and no (empty) set.
func TestTurnDetails_MalformedRequiredToolsFailsClosed(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	ver, err := store.GetSessionVersion(ctx, agyUncSession)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-bad", agyUncLease, agyUncSession, ver, PendingPrompt{
		SessionID: agyUncSession, TurnKey: "t-bad", Prompt: "p", RequiredTools: []string{"view_file"},
	}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if ver, err = store.GetSessionVersion(ctx, agyUncSession); err != nil {
		t.Fatalf("version: %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-bad", agyUncLease, agyUncSession, ver, "t-bad"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE dispatch_intents SET required_tools_json = '["view_file"' WHERE session_id = ? AND turn_key = ?`,
		agyUncSession, "t-bad"); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	details, err := store.GetTurnDetails(ctx, agyUncSession, "t-bad")
	if err != nil || details == nil || details.DispatchIntent == nil {
		t.Fatalf("details: %+v err=%v", details, err)
	}
	if details.DispatchIntent.RequiredToolsErr == nil || details.DispatchIntent.RequiredTools != nil {
		t.Fatalf("a malformed required set must fail closed, got tools=%v err=%v",
			details.DispatchIntent.RequiredTools, details.DispatchIntent.RequiredToolsErr)
	}
}
