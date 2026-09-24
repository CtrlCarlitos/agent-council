package storage

// AC-009 §3.4 durable creation-uncertainty EPISODES and the production
// binding transition: the record is idempotent per open episode (N
// callers sharing one uncertain creation open ONE episode), resolution
// requires CURRENT controller authority at the expected generation and
// targets one exact episode, a resolved session that ends uncertain
// again opens the NEXT episode, and BindCodexSession re-validates the
// lease inside its transaction (a superseded credential cannot publish
// a binding), journals the op, and replays its receipt.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	uncRunID   = "run-unc"
	uncSession = "sess-unc"
	uncLease   = "lease-unc-gen1"
)

// newUncertaintyFixture seeds a run with an ADOPTED, connected
// controller (generation 1) and one codex session.
func newUncertaintyFixture(t *testing.T) *Store {
	t.Helper()
	store := openCodexStore(t)
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run", uncRunID, "brief-sha", "src-sha", "prof-sha", "bootstrap-"+uncLease); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess", "bootstrap-"+uncLease, SessionRecord{
		ID: uncSession, RunID: uncRunID, Contributor: "codex", Role: "worker",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.AdoptController(ctx, "op-adopt", uncRunID, "codex", "ctrl-ref", "bootstrap-"+uncLease, nil, uncLease); err != nil {
		t.Fatalf("adopt controller: %v", err)
	}
	if _, err := store.ConnectRunController(ctx, "op-conn", uncRunID, uncLease, 1, "inst-1"); err != nil {
		t.Fatalf("connect controller: %v", err)
	}
	return store
}

func journalCount(t *testing.T, store *Store, cmd string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT count(*) FROM journal_entries WHERE command_type = ?`, cmd).Scan(&n); err != nil {
		t.Fatalf("count journal: %v", err)
	}
	return n
}

func TestCodexUncertainty_OneOpenEpisodeSharedByConcurrentCallers(t *testing.T) {
	store := newUncertaintyFixture(t)
	ctx := context.Background()
	rec := CodexCreationUncertainty{RunID: uncRunID, SessionID: uncSession, Reason: "lost thread/start response",
		RecordedBy: "codex-adapter", CauseOpID: "op-create-a"}

	ep, receipt, err := store.RecordCodexCreationUncertain(ctx, rec)
	if err != nil || ep != 1 {
		t.Fatalf("first record must open episode 1, got %d err=%v", ep, err)
	}
	if receipt.OpID != "op-codex-creation-uncertain-"+uncSession+"-e1" || receipt.CommittedVersion != 1 {
		t.Fatalf("receipt must carry the episode identity, got %+v", receipt)
	}
	// A second caller sharing the same uncertain creation — even with a
	// different causing op and reason text — records nothing new: the
	// open episode's receipt is replayed.
	rec2 := rec
	rec2.CauseOpID = "op-create-b"
	rec2.Reason = "lost thread/start response (caller b)"
	ep2, receipt2, err := store.RecordCodexCreationUncertain(ctx, rec2)
	if err != nil || ep2 != 1 || receipt2.OpID != receipt.OpID {
		t.Fatalf("an open episode must be replayed, got %d %+v err=%v", ep2, receipt2, err)
	}
	if n := journalCount(t, store, codexUncertainCmd); n != 1 {
		t.Fatalf("exactly one uncertainty journal entry, got %d", n)
	}
	open, err := store.OpenCodexCreationUncertainty(ctx, uncSession)
	if err != nil || open == nil || open.Episode != 1 || open.CauseOpID != "op-create-a" || open.Reason != rec.Reason {
		t.Fatalf("open episode must be the first record, got %+v err=%v", open, err)
	}
	if blocked, err := store.HasCodexCreationUncertainty(ctx, uncSession); err != nil || !blocked {
		t.Fatalf("session must be blocked, got %v err=%v", blocked, err)
	}

	// Validation: every provenance field is required.
	for name, bad := range map[string]CodexCreationUncertainty{
		"no session": {RunID: uncRunID, Reason: "r", RecordedBy: "a", CauseOpID: "c"},
		"no reason":  {RunID: uncRunID, SessionID: uncSession, RecordedBy: "a", CauseOpID: "c"},
		"no cause":   {RunID: uncRunID, SessionID: uncSession, Reason: "r", RecordedBy: "a"},
	} {
		if _, _, err := store.RecordCodexCreationUncertain(ctx, bad); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestCodexUncertainty_ResolutionRequiresCurrentAuthorityAndExactEpisode(t *testing.T) {
	store := newUncertaintyFixture(t)
	ctx := context.Background()
	rec := CodexCreationUncertainty{RunID: uncRunID, SessionID: uncSession, Reason: "lost",
		RecordedBy: "codex-adapter", CauseOpID: "op-create-1"}
	if _, _, err := store.RecordCodexCreationUncertain(ctx, rec); err != nil {
		t.Fatalf("record: %v", err)
	}
	resolve := func(opID, lease string, gen uint64, episode int64, disposition, reason string) (OperationReceipt, error) {
		return store.ResolveCodexCreationUncertainty(ctx, opID, lease, gen, uncSession, episode, disposition, reason)
	}

	if _, err := resolve("op-r-empty", "", 1, 1, CodexUncertaintyAbandonOrphan, "x"); err == nil {
		t.Fatal("an empty lease must be refused")
	}
	if _, err := resolve("op-r-wrong", "not-a-lease", 1, 1, CodexUncertaintyAbandonOrphan, "x"); err == nil ||
		!errors.Is(err, ErrUnauthorizedOperation) {
		t.Fatalf("a foreign lease must be refused with ErrUnauthorizedOperation, got %v", err)
	}
	if _, err := resolve("op-r-stale", uncLease, 2, 1, CodexUncertaintyAbandonOrphan, "x"); err == nil ||
		!strings.Contains(err.Error(), "expected generation 2, active generation is 1") {
		t.Fatalf("a stale generation must be refused, got %v", err)
	}
	if _, err := resolve("op-r-disp", uncLease, 1, 1, "whatever", "x"); err == nil ||
		!strings.Contains(err.Error(), "disposition") {
		t.Fatalf("an unknown disposition must be refused, got %v", err)
	}
	if _, err := resolve("op-r-noreason", uncLease, 1, 1, CodexUncertaintyAbandonOrphan, " "); err == nil {
		t.Fatal("an empty reason must be refused")
	}
	if _, err := resolve("op-r-ep2", uncLease, 1, 2, CodexUncertaintyAbandonOrphan, "x"); err == nil ||
		!strings.Contains(err.Error(), "no creation-uncertainty episode 2") {
		t.Fatalf("a resolution must name an existing episode, got %v", err)
	}
	if n := journalCount(t, store, codexResolveCmd); n != 0 {
		t.Fatalf("refused resolutions must journal nothing, got %d", n)
	}
	if blocked, _ := store.HasCodexCreationUncertainty(ctx, uncSession); !blocked {
		t.Fatal("refused resolutions must leave the block")
	}

	// A handoff supersedes generation 1: the old lease can no longer
	// resolve; the new lease at generation 2 can.
	grant, err := store.HandoffController(ctx, "op-handoff", uncRunID, uncLease, 1, "codex", "ctrl-ref-2", "lease-unc-gen2")
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if _, err := resolve("op-r-superseded", uncLease, 1, 1, CodexUncertaintyAbandonOrphan, "x"); err == nil ||
		!errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("a superseded lease must be refused with ErrLeaseSuperseded, got %v", err)
	}
	receipt, err := resolve("op-r-ok", grant.LeaseSecret, grant.Generation, 1, CodexUncertaintyVerifiedAbsent, "thread/list sweep: absent")
	if err != nil {
		t.Fatalf("current controller must resolve: %v", err)
	}
	if receipt.CommandType != codexResolveCmd || receipt.Payload != CodexUncertaintyVerifiedAbsent || receipt.CommittedVersion != 1 {
		t.Fatalf("unexpected resolution receipt %+v", receipt)
	}
	if blocked, err := store.HasCodexCreationUncertainty(ctx, uncSession); err != nil || blocked {
		t.Fatalf("the block must clear, blocked=%v err=%v", blocked, err)
	}
	episodes, err := store.CodexCreationUncertaintyEpisodes(ctx, uncSession)
	if err != nil || len(episodes) != 1 {
		t.Fatalf("one episode expected, got %d err=%v", len(episodes), err)
	}
	e := episodes[0]
	if e.Disposition == nil || *e.Disposition != CodexUncertaintyVerifiedAbsent || e.ResolutionReason == nil ||
		e.ResolutionGeneration == nil || *e.ResolutionGeneration != grant.Generation ||
		e.ResolutionOpID == nil || *e.ResolutionOpID != "op-r-ok" || e.ResolvedAt == nil {
		t.Fatalf("resolution provenance must be durable, got %+v", e)
	}

	// Idempotent replay of the same resolution returns the receipt; the
	// same op with different parameters conflicts; a new op against the
	// resolved episode is refused.
	replay, err := resolve("op-r-ok", grant.LeaseSecret, grant.Generation, 1, CodexUncertaintyVerifiedAbsent, "thread/list sweep: absent")
	if err != nil || replay.OpID != receipt.OpID || replay.Payload != receipt.Payload {
		t.Fatalf("replay must return the committed receipt, got %+v err=%v", replay, err)
	}
	if _, err := resolve("op-r-ok", grant.LeaseSecret, grant.Generation, 1, CodexUncertaintyAbandonOrphan, "different"); err == nil ||
		!errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("op reuse with different parameters must conflict, got %v", err)
	}
	if _, err := resolve("op-r-twice", grant.LeaseSecret, grant.Generation, 1, CodexUncertaintyAbandonOrphan, "x"); err == nil ||
		!strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("a resolved episode must not be resolved again, got %v", err)
	}
	if n := journalCount(t, store, codexResolveCmd); n != 1 {
		t.Fatalf("exactly one resolution journal entry, got %d", n)
	}
}

func TestCodexUncertainty_SecondEpisodeAfterResolution(t *testing.T) {
	store := newUncertaintyFixture(t)
	ctx := context.Background()
	rec := CodexCreationUncertainty{RunID: uncRunID, SessionID: uncSession, Reason: "lost once",
		RecordedBy: "codex-adapter", CauseOpID: "op-create-1"}
	if ep, _, err := store.RecordCodexCreationUncertain(ctx, rec); err != nil || ep != 1 {
		t.Fatalf("episode 1: %d %v", ep, err)
	}
	if _, err := store.ResolveCodexCreationUncertainty(ctx, "op-r-1", uncLease, 1, uncSession, 1,
		CodexUncertaintyAbandonOrphan, "abandon"); err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	// The session ends uncertain AGAIN: a distinct episode, whether the
	// reason repeats verbatim or differs — never a conflict, never a
	// replay of the resolved episode, and the block returns.
	rec2 := rec
	rec2.CauseOpID = "op-create-2"
	ep, receipt, err := store.RecordCodexCreationUncertain(ctx, rec2)
	if err != nil || ep != 2 || receipt.OpID != "op-codex-creation-uncertain-"+uncSession+"-e2" {
		t.Fatalf("second outcome must open episode 2, got %d %+v err=%v", ep, receipt, err)
	}
	if blocked, err := store.HasCodexCreationUncertainty(ctx, uncSession); err != nil || !blocked {
		t.Fatalf("episode 2 must block, got %v err=%v", blocked, err)
	}
	rec3 := rec
	rec3.CauseOpID = "op-create-3"
	rec3.Reason = "lost differently"
	if ep, _, err := store.RecordCodexCreationUncertain(ctx, rec3); err != nil || ep != 2 {
		t.Fatalf("while episode 2 is open, another outcome shares it, got %d err=%v", ep, err)
	}
	// Resolving episode 1 again does nothing to episode 2.
	if _, err := store.ResolveCodexCreationUncertainty(ctx, "op-r-1-again", uncLease, 1, uncSession, 1,
		CodexUncertaintyAbandonOrphan, "abandon"); err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("episode 1 stays resolved, got %v", err)
	}
	if blocked, _ := store.HasCodexCreationUncertainty(ctx, uncSession); !blocked {
		t.Fatal("episode 2 must still block")
	}
	if _, err := store.ResolveCodexCreationUncertainty(ctx, "op-r-2", uncLease, 1, uncSession, 2,
		CodexUncertaintyVerifiedAbsent, "verified"); err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	episodes, err := store.CodexCreationUncertaintyEpisodes(ctx, uncSession)
	if err != nil || len(episodes) != 2 || episodes[0].Episode != 1 || episodes[1].Episode != 2 {
		t.Fatalf("two ordered episodes expected, got %+v err=%v", episodes, err)
	}
	if *episodes[0].Disposition != CodexUncertaintyAbandonOrphan || *episodes[1].Disposition != CodexUncertaintyVerifiedAbsent {
		t.Fatalf("each episode carries its own disposition, got %+v", episodes)
	}
	if n := journalCount(t, store, codexUncertainCmd); n != 2 {
		t.Fatalf("two uncertainty journal entries, got %d", n)
	}
}

func TestCodexBind_AuthorityInsideTransactionAndIdempotency(t *testing.T) {
	store := newUncertaintyFixture(t)
	ctx := context.Background()
	binding := CodexSessionBinding{
		SessionID: uncSession, NativeID: "01934f7a-1b2c-7def-9abc-def012345678",
		Model: "gpt-5-codex", Workspace: "/tmp/ws", ProfileDigest: "cprof-v3:sha256:pd",
	}

	if _, err := store.BindCodexSession(ctx, "op-bind-wrong", "not-a-lease", binding); err == nil ||
		!errors.Is(err, ErrUnauthorizedOperation) {
		t.Fatalf("a foreign lease must not bind, got %v", err)
	}
	noDigest := binding
	noDigest.ProfileDigest = ""
	if _, err := store.BindCodexSession(ctx, "op-bind-nodigest", uncLease, noDigest); err == nil {
		t.Fatal("a binding without the frozen profile digest must be refused")
	}
	if b, _ := store.GetCodexSessionBinding(ctx, uncSession); b != nil {
		t.Fatal("refused binds must persist nothing")
	}

	// A handoff supersedes generation 1 BEFORE the old controller's
	// bind reaches the transaction: the old lease cannot publish the
	// binding — the transaction re-validates, the pre-flight is not the
	// authority.
	grant, err := store.HandoffController(ctx, "op-handoff", uncRunID, uncLease, 1, "codex", "ctrl-ref-2", "lease-unc-gen2")
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if _, err := store.BindCodexSession(ctx, "op-bind-old", uncLease, binding); err == nil ||
		!errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("a superseded lease must not bind, got %v", err)
	}
	if b, _ := store.GetCodexSessionBinding(ctx, uncSession); b != nil {
		t.Fatal("the superseded controller's bind must persist nothing")
	}

	receipt, err := store.BindCodexSession(ctx, "op-bind-new", grant.LeaseSecret, binding)
	if err != nil {
		t.Fatalf("current controller must bind: %v", err)
	}
	if receipt.CommandType != "bind_codex_session" || receipt.Payload != binding.NativeID || receipt.SessionID != uncSession {
		t.Fatalf("unexpected bind receipt %+v", receipt)
	}
	stored, err := store.GetCodexSessionBinding(ctx, uncSession)
	if err != nil || stored == nil || stored.NativeID != binding.NativeID || stored.Materialized || stored.CreatedAt.IsZero() {
		t.Fatalf("binding must be persisted unmaterialized, got %+v err=%v", stored, err)
	}
	if n := journalCount(t, store, "bind_codex_session"); n != 1 {
		t.Fatalf("exactly one bind journal entry, got %d", n)
	}
	// Idempotent replay; conflicting reuse; second binding refused.
	replay, err := store.BindCodexSession(ctx, "op-bind-new", grant.LeaseSecret, binding)
	if err != nil || replay.Payload != receipt.Payload || replay.OpID != receipt.OpID {
		t.Fatalf("replay must return the committed receipt, got %+v err=%v", replay, err)
	}
	other := binding
	other.NativeID = "01934f7a-1b2c-7def-9abc-def012345679"
	if _, err := store.BindCodexSession(ctx, "op-bind-new", grant.LeaseSecret, other); err == nil || !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("op reuse with a different binding must conflict, got %v", err)
	}
	if _, err := store.BindCodexSession(ctx, "op-bind-second", grant.LeaseSecret, other); err == nil ||
		!strings.Contains(err.Error(), "already bound") {
		t.Fatalf("one native identity per session, got %v", err)
	}
}
