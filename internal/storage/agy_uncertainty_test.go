package storage

// AC-010 durable creation-uncertainty EPISODES and the production
// binding transition for the Agy adapter — copied shapes from
// codex_uncertainty_test.go (AC-009): the record is idempotent per open
// episode (N callers sharing one uncertain creation open ONE episode),
// resolution requires CURRENT controller authority at the expected
// generation and targets one exact episode, a resolved session that
// ends uncertain again opens the NEXT episode, and BindAgySession
// re-validates the lease inside its transaction (a superseded
// credential cannot publish a binding), journals the op, and replays
// its receipt.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	agyUncRunID   = "run-agy-unc"
	agyUncSession = "sess-agy-unc"
	agyUncLease   = "lease-agy-unc-gen1"
)

// newAgyUncertaintyFixture seeds a run with an ADOPTED, connected
// controller (generation 1) and one agy session.
func newAgyUncertaintyFixture(t *testing.T) *Store {
	t.Helper()
	store := openAgyStore(t)
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run", agyUncRunID, "brief-sha", "src-sha", "prof-sha", "bootstrap-"+agyUncLease); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess", "bootstrap-"+agyUncLease, SessionRecord{
		ID: agyUncSession, RunID: agyUncRunID, Contributor: "agy", Role: "worker",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.AdoptController(ctx, "op-adopt", agyUncRunID, "agy", "ctrl-ref", "bootstrap-"+agyUncLease, nil, agyUncLease); err != nil {
		t.Fatalf("adopt controller: %v", err)
	}
	if _, err := store.ConnectRunController(ctx, "op-conn", agyUncRunID, agyUncLease, 1, "inst-1"); err != nil {
		t.Fatalf("connect controller: %v", err)
	}
	return store
}

func TestAgyUncertainty_OneOpenEpisodeSharedByConcurrentCallers(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	rec := AgyCreationUncertainty{RunID: agyUncRunID, SessionID: agyUncSession, Reason: "lost start response",
		RecordedBy: "agy-adapter", CauseOpID: "op-create-a"}

	ep, receipt, err := store.RecordAgyCreationUncertain(ctx, rec)
	if err != nil || ep != 1 {
		t.Fatalf("first record must open episode 1, got %d err=%v", ep, err)
	}
	if receipt.OpID != "op-agy-creation-uncertain-"+agyUncSession+"-e1" || receipt.CommittedVersion != 1 {
		t.Fatalf("receipt must carry the episode identity, got %+v", receipt)
	}
	// A second caller sharing the same uncertain creation — even with a
	// different causing op and reason text — records nothing new: the
	// open episode's receipt is replayed.
	rec2 := rec
	rec2.CauseOpID = "op-create-b"
	rec2.Reason = "lost start response (caller b)"
	ep2, receipt2, err := store.RecordAgyCreationUncertain(ctx, rec2)
	if err != nil || ep2 != 1 || receipt2.OpID != receipt.OpID {
		t.Fatalf("an open episode must be replayed, got %d %+v err=%v", ep2, receipt2, err)
	}
	if n := journalCount(t, store, agyUncertainCmd); n != 1 {
		t.Fatalf("exactly one uncertainty journal entry, got %d", n)
	}
	open, err := store.OpenAgyCreationUncertainty(ctx, agyUncSession)
	if err != nil || open == nil || open.Episode != 1 || open.CauseOpID != "op-create-a" || open.Reason != rec.Reason {
		t.Fatalf("open episode must be the first record, got %+v err=%v", open, err)
	}
	if blocked, err := store.HasAgyCreationUncertainty(ctx, agyUncSession); err != nil || !blocked {
		t.Fatalf("session must be blocked, got %v err=%v", blocked, err)
	}

	// Validation: every provenance field is required.
	for name, bad := range map[string]AgyCreationUncertainty{
		"no session": {RunID: agyUncRunID, Reason: "r", RecordedBy: "a", CauseOpID: "c"},
		"no reason":  {RunID: agyUncRunID, SessionID: agyUncSession, RecordedBy: "a", CauseOpID: "c"},
		"no cause":   {RunID: agyUncRunID, SessionID: agyUncSession, Reason: "r", RecordedBy: "a"},
	} {
		if _, _, err := store.RecordAgyCreationUncertain(ctx, bad); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestAgyUncertainty_ResolutionRequiresCurrentAuthorityAndExactEpisode(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	rec := AgyCreationUncertainty{RunID: agyUncRunID, SessionID: agyUncSession, Reason: "lost",
		RecordedBy: "agy-adapter", CauseOpID: "op-create-1"}
	if _, _, err := store.RecordAgyCreationUncertain(ctx, rec); err != nil {
		t.Fatalf("record: %v", err)
	}
	resolve := func(opID, lease string, gen uint64, episode int64, disposition, reason string) (OperationReceipt, error) {
		return store.ResolveAgyCreationUncertainty(ctx, opID, lease, gen, agyUncSession, episode, disposition, reason)
	}

	if _, err := resolve("op-r-empty", "", 1, 1, AgyUncertaintyAbandonOrphan, "x"); err == nil {
		t.Fatal("an empty lease must be refused")
	}
	if _, err := resolve("op-r-wrong", "not-a-lease", 1, 1, AgyUncertaintyAbandonOrphan, "x"); err == nil ||
		!errors.Is(err, ErrUnauthorizedOperation) {
		t.Fatalf("a foreign lease must be refused with ErrUnauthorizedOperation, got %v", err)
	}
	if _, err := resolve("op-r-stale", agyUncLease, 2, 1, AgyUncertaintyAbandonOrphan, "x"); err == nil ||
		!strings.Contains(err.Error(), "expected generation 2, active generation is 1") {
		t.Fatalf("a stale generation must be refused, got %v", err)
	}
	if _, err := resolve("op-r-disp", agyUncLease, 1, 1, "whatever", "x"); err == nil ||
		!strings.Contains(err.Error(), "disposition") {
		t.Fatalf("an unknown disposition must be refused, got %v", err)
	}
	if _, err := resolve("op-r-noreason", agyUncLease, 1, 1, AgyUncertaintyAbandonOrphan, " "); err == nil {
		t.Fatal("an empty reason must be refused")
	}
	if _, err := resolve("op-r-ep2", agyUncLease, 1, 2, AgyUncertaintyAbandonOrphan, "x"); err == nil ||
		!strings.Contains(err.Error(), "no creation-uncertainty episode 2") {
		t.Fatalf("a resolution must name an existing episode, got %v", err)
	}
	if n := journalCount(t, store, agyResolveCmd); n != 0 {
		t.Fatalf("refused resolutions must journal nothing, got %d", n)
	}
	if blocked, _ := store.HasAgyCreationUncertainty(ctx, agyUncSession); !blocked {
		t.Fatal("refused resolutions must leave the block")
	}

	// A handoff supersedes generation 1: the old lease can no longer
	// resolve; the new lease at generation 2 can.
	grant, err := store.HandoffController(ctx, "op-handoff", agyUncRunID, agyUncLease, 1, "agy", "ctrl-ref-2", "lease-agy-unc-gen2")
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if _, err := resolve("op-r-superseded", agyUncLease, 1, 1, AgyUncertaintyAbandonOrphan, "x"); err == nil ||
		!errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("a superseded lease must be refused with ErrLeaseSuperseded, got %v", err)
	}
	receipt, err := resolve("op-r-ok", grant.LeaseSecret, grant.Generation, 1, AgyUncertaintyVerifiedAbsent, "diagnostic sweep: absent")
	if err != nil {
		t.Fatalf("current controller must resolve: %v", err)
	}
	if receipt.CommandType != agyResolveCmd || receipt.Payload != AgyUncertaintyVerifiedAbsent || receipt.CommittedVersion != 1 {
		t.Fatalf("unexpected resolution receipt %+v", receipt)
	}
	if blocked, err := store.HasAgyCreationUncertainty(ctx, agyUncSession); err != nil || blocked {
		t.Fatalf("the block must clear, blocked=%v err=%v", blocked, err)
	}
	episodes, err := store.AgyCreationUncertaintyEpisodes(ctx, agyUncSession)
	if err != nil || len(episodes) != 1 {
		t.Fatalf("one episode expected, got %d err=%v", len(episodes), err)
	}
	e := episodes[0]
	if e.Disposition == nil || *e.Disposition != AgyUncertaintyVerifiedAbsent || e.ResolutionReason == nil ||
		e.ResolutionGeneration == nil || *e.ResolutionGeneration != grant.Generation ||
		e.ResolutionOpID == nil || *e.ResolutionOpID != "op-r-ok" || e.ResolvedAt == nil {
		t.Fatalf("resolution provenance must be durable, got %+v", e)
	}

	// Idempotent replay of the same resolution returns the receipt; the
	// same op with different parameters conflicts; a new op against the
	// resolved episode is refused.
	replay, err := resolve("op-r-ok", grant.LeaseSecret, grant.Generation, 1, AgyUncertaintyVerifiedAbsent, "diagnostic sweep: absent")
	if err != nil || replay.OpID != receipt.OpID || replay.Payload != receipt.Payload {
		t.Fatalf("replay must return the committed receipt, got %+v err=%v", replay, err)
	}
	if _, err := resolve("op-r-ok", grant.LeaseSecret, grant.Generation, 1, AgyUncertaintyAbandonOrphan, "different"); err == nil ||
		!errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("op reuse with different parameters must conflict, got %v", err)
	}
	if _, err := resolve("op-r-twice", grant.LeaseSecret, grant.Generation, 1, AgyUncertaintyAbandonOrphan, "x"); err == nil ||
		!strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("a resolved episode must not be resolved again, got %v", err)
	}
	if n := journalCount(t, store, agyResolveCmd); n != 1 {
		t.Fatalf("exactly one resolution journal entry, got %d", n)
	}
}

func TestAgyUncertainty_SecondEpisodeAfterResolution(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	rec := AgyCreationUncertainty{RunID: agyUncRunID, SessionID: agyUncSession, Reason: "lost once",
		RecordedBy: "agy-adapter", CauseOpID: "op-create-1"}
	if ep, _, err := store.RecordAgyCreationUncertain(ctx, rec); err != nil || ep != 1 {
		t.Fatalf("episode 1: %d %v", ep, err)
	}
	if _, err := store.ResolveAgyCreationUncertainty(ctx, "op-r-1", agyUncLease, 1, agyUncSession, 1,
		AgyUncertaintyAbandonOrphan, "abandon"); err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	// The session ends uncertain AGAIN: a distinct episode, whether the
	// reason repeats verbatim or differs — never a conflict, never a
	// replay of the resolved episode, and the block returns.
	rec2 := rec
	rec2.CauseOpID = "op-create-2"
	ep, receipt, err := store.RecordAgyCreationUncertain(ctx, rec2)
	if err != nil || ep != 2 || receipt.OpID != "op-agy-creation-uncertain-"+agyUncSession+"-e2" {
		t.Fatalf("second outcome must open episode 2, got %d %+v err=%v", ep, receipt, err)
	}
	if blocked, err := store.HasAgyCreationUncertainty(ctx, agyUncSession); err != nil || !blocked {
		t.Fatalf("episode 2 must block, got %v err=%v", blocked, err)
	}
	rec3 := rec
	rec3.CauseOpID = "op-create-3"
	rec3.Reason = "lost differently"
	if ep, _, err := store.RecordAgyCreationUncertain(ctx, rec3); err != nil || ep != 2 {
		t.Fatalf("while episode 2 is open, another outcome shares it, got %d err=%v", ep, err)
	}
	// Resolving episode 1 again does nothing to episode 2.
	if _, err := store.ResolveAgyCreationUncertainty(ctx, "op-r-1-again", agyUncLease, 1, agyUncSession, 1,
		AgyUncertaintyAbandonOrphan, "abandon"); err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("episode 1 stays resolved, got %v", err)
	}
	if blocked, _ := store.HasAgyCreationUncertainty(ctx, agyUncSession); !blocked {
		t.Fatal("episode 2 must still block")
	}
	if _, err := store.ResolveAgyCreationUncertainty(ctx, "op-r-2", agyUncLease, 1, agyUncSession, 2,
		AgyUncertaintyVerifiedAbsent, "verified"); err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	episodes, err := store.AgyCreationUncertaintyEpisodes(ctx, agyUncSession)
	if err != nil || len(episodes) != 2 || episodes[0].Episode != 1 || episodes[1].Episode != 2 {
		t.Fatalf("two ordered episodes expected, got %+v err=%v", episodes, err)
	}
	if *episodes[0].Disposition != AgyUncertaintyAbandonOrphan || *episodes[1].Disposition != AgyUncertaintyVerifiedAbsent {
		t.Fatalf("each episode carries its own disposition, got %+v", episodes)
	}
	if n := journalCount(t, store, agyUncertainCmd); n != 2 {
		t.Fatalf("two uncertainty journal entries, got %d", n)
	}
}

func TestAgyBind_AuthorityInsideTransactionAndIdempotency(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	binding := AgySessionBinding{
		SessionID: agyUncSession, NativeID: "0195f7a1-2b3c-4def-9abc-def012345678",
		Model: "agy-1", Workspace: "/tmp/ws", ProfileDigest: "aprof-v1:sha256:pd",
	}

	if _, err := store.BindAgySession(ctx, "op-bind-wrong", "not-a-lease", binding); err == nil ||
		!errors.Is(err, ErrUnauthorizedOperation) {
		t.Fatalf("a foreign lease must not bind, got %v", err)
	}
	noDigest := binding
	noDigest.ProfileDigest = ""
	if _, err := store.BindAgySession(ctx, "op-bind-nodigest", agyUncLease, noDigest); err == nil {
		t.Fatal("a binding without the frozen profile digest must be refused")
	}
	if b, _ := store.GetAgySessionBinding(ctx, agyUncSession); b != nil {
		t.Fatal("refused binds must persist nothing")
	}

	// A handoff supersedes generation 1 BEFORE the old controller's
	// bind reaches the transaction: the old lease cannot publish the
	// binding — the transaction re-validates, the pre-flight is not the
	// authority.
	grant, err := store.HandoffController(ctx, "op-handoff", agyUncRunID, agyUncLease, 1, "agy", "ctrl-ref-2", "lease-agy-unc-gen2")
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if _, err := store.BindAgySession(ctx, "op-bind-old", agyUncLease, binding); err == nil ||
		!errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("a superseded lease must not bind, got %v", err)
	}
	if b, _ := store.GetAgySessionBinding(ctx, agyUncSession); b != nil {
		t.Fatal("the superseded controller's bind must persist nothing")
	}

	receipt, err := store.BindAgySession(ctx, "op-bind-new", grant.LeaseSecret, binding)
	if err != nil {
		t.Fatalf("current controller must bind: %v", err)
	}
	if receipt.CommandType != "bind_agy_session" || receipt.Payload != binding.NativeID || receipt.SessionID != agyUncSession {
		t.Fatalf("unexpected bind receipt %+v", receipt)
	}
	stored, err := store.GetAgySessionBinding(ctx, agyUncSession)
	if err != nil || stored == nil || stored.NativeID != binding.NativeID || stored.Materialized || stored.CreatedAt.IsZero() {
		t.Fatalf("binding must be persisted unmaterialized, got %+v err=%v", stored, err)
	}
	if n := journalCount(t, store, "bind_agy_session"); n != 1 {
		t.Fatalf("exactly one bind journal entry, got %d", n)
	}
	// Idempotent replay; conflicting reuse; second binding refused.
	replay, err := store.BindAgySession(ctx, "op-bind-new", grant.LeaseSecret, binding)
	if err != nil || replay.Payload != receipt.Payload || replay.OpID != receipt.OpID {
		t.Fatalf("replay must return the committed receipt, got %+v err=%v", replay, err)
	}
	other := binding
	other.NativeID = "0195f7a1-2b3c-4def-9abc-def012345679"
	if _, err := store.BindAgySession(ctx, "op-bind-new", grant.LeaseSecret, other); err == nil || !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("op reuse with a different binding must conflict, got %v", err)
	}
	if _, err := store.BindAgySession(ctx, "op-bind-second", grant.LeaseSecret, other); err == nil ||
		!strings.Contains(err.Error(), "already bound") {
		t.Fatalf("one native identity per session, got %v", err)
	}
}
