package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Regression evidence for AC-004 Task 2: grant transitions with request
// contracts, generated-once secrets, and credential recovery. The cases
// implement the plan's required-results table verbatim.

func newGrantTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-g", "run-g", "brief", "source", "profile", "legacy-lease-g"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return store, "run-g"
}

func TestAC004_AdoptFromLegacyProvenance(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	grant, err := store.AdoptController(ctx, "op-adopt-1", runID, "opencode", "conversation-ref-1", "legacy-lease-g", nil, "candidate-secret-1")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if grant.Generation != 1 || grant.LeaseSecret != "candidate-secret-1" {
		t.Fatalf("unexpected grant: %+v", grant)
	}

	rec, err := store.GetControllerRecord(ctx, runID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if !rec.Adopted || rec.Generation != 1 || rec.Harness != "opencode" || rec.ControllerRef != "conversation-ref-1" || rec.Status != "active" {
		t.Fatalf("unexpected record: %+v", rec)
	}

	// The legacy provenance row is superseded and the run's authority is the
	// adopted generation.
	var runLease string
	if err := store.readDB.QueryRow(`SELECT controller_lease FROM runs WHERE run_id = ?;`, runID).Scan(&runLease); err != nil {
		t.Fatalf("read run lease: %v", err)
	}
	if runLease != "candidate-secret-1" {
		t.Fatalf("runs.controller_lease must hold the adopted secret, got %q", runLease)
	}
	var legacyStatus string
	if err := store.readDB.QueryRow(`SELECT status FROM controller_leases WHERE run_id = ? AND generation = 0;`, runID).Scan(&legacyStatus); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if legacyStatus != "superseded" {
		t.Fatalf("legacy row must be superseded after adoption, got %q", legacyStatus)
	}
}

func TestAC004_AdoptViaOperatorRecovery(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	grant, err := store.AdoptController(ctx, "op-adopt-rec", runID, "agy", "conversation-ref-agy", "", &OperatorRecovery{Reason: "legacy lease lost", ExpectedGeneration: 0}, "cand-rec-1")
	if err != nil {
		t.Fatalf("adopt via recovery: %v", err)
	}
	if grant.Generation != 1 {
		t.Fatalf("expected generation 1, got %d", grant.Generation)
	}
}

func TestAC004_AdoptRefusesWhileActiveAndContinuesGenerationsAfterRevoke(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.AdoptController(ctx, "op-a1", runID, "claude", "ref-a", "legacy-lease-g", nil, "s1"); err != nil {
		t.Fatalf("first adopt: %v", err)
	}

	// Refusal while an adopted active controller exists.
	if _, err := store.AdoptController(ctx, "op-a2", runID, "codex", "ref-b", "s1", nil, "s2"); !errors.Is(err, ErrAdoptionExists) {
		t.Fatalf("expected ErrAdoptionExists, got %v", err)
	}

	// Revoke, then re-adopt: generation must continue, never reset.
	if _, err := store.RevokeController(ctx, "op-r1", runID, "s1", nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	grant, err := store.AdoptController(ctx, "op-a3", runID, "codex", "ref-b", "", &OperatorRecovery{Reason: "rotation", ExpectedGeneration: 1}, "s3")
	if err != nil {
		t.Fatalf("re-adopt after revoke: %v", err)
	}
	if grant.Generation != 2 {
		t.Fatalf("generation must continue after revocation, got %d", grant.Generation)
	}
}

func TestAC004_RevokeClearsAuthorityAndEmptyNeverPasses(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.AdoptController(ctx, "op-a1", runID, "claude", "ref-a", "legacy-lease-g", nil, "s1"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if _, err := store.RevokeController(ctx, "op-r1", runID, "s1", nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	var runLease string
	if err := store.readDB.QueryRow(`SELECT controller_lease FROM runs WHERE run_id = ?;`, runID).Scan(&runLease); err != nil {
		t.Fatalf("read run lease: %v", err)
	}
	if runLease != "" {
		t.Fatalf("revocation must clear runs.controller_lease, got %q", runLease)
	}

	// A revoked run authorizes nothing, including empty presentations.
	if _, err := store.HandoffController(ctx, "op-h1", runID, "", 1, "codex", "ref-b", "s2"); err == nil {
		t.Fatal("empty credential must never pass")
	}
	if _, err := store.RevokeController(ctx, "op-r2", runID, "", nil); err == nil {
		t.Fatal("empty credential must never authorize self-revocation")
	}
}

func TestAC004_HandoffSupersedesAndClassifiesStaleAuthorityFirst(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.AdoptController(ctx, "op-a1", runID, "claude", "ref-a", "legacy-lease-g", nil, "s1"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	grant, err := store.HandoffController(ctx, "op-h1", runID, "s1", 1, "codex", "ref-b", "s2")
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if grant.Generation != 2 || grant.LeaseSecret != "s2" {
		t.Fatalf("unexpected handoff grant: %+v", grant)
	}

	// The superseded controller cannot hand off again: stale-authority
	// classification takes precedence over any generation expectation.
	if _, err := store.HandoffController(ctx, "op-h2", runID, "s1", 1, "agy", "ref-c", "s3"); !errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("expected ErrLeaseSuperseded for superseded handoff caller, got %v", err)
	}

	// An otherwise-authorized operation with a stale expected generation
	// receives the generation mismatch error.
	if _, err := store.HandoffController(ctx, "op-h3", runID, "s2", 1, "agy", "ref-c", "s4"); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("expected ErrGenerationMismatch for stale expected generation, got %v", err)
	}
}

func TestAC004_ConcurrentHandoffsSingleWinner(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.AdoptController(ctx, "op-a1", runID, "claude", "ref-a", "legacy-lease-g", nil, "s1"); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	type result struct {
		grant ControllerGrantReceipt
		err   error
	}
	res := make(chan result, 2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			grant, err := store.HandoffController(ctx, "op-ch"+strings.Repeat("x", i+1), runID, "s1", 1, "codex", "ref-race", "s-race-"+strings.Repeat(string(rune('a'+i)), 4))
			res <- result{grant, err}
		}()
	}
	r1, r2 := <-res, <-res

	wins, superseded, other := 0, 0, 0
	for _, r := range []result{r1, r2} {
		switch {
		case r.err == nil:
			wins++
			if r.grant.Generation != 2 {
				t.Fatalf("winner must install generation 2, got %d", r.grant.Generation)
			}
		case errors.Is(r.err, ErrLeaseSuperseded):
			superseded++
		default:
			other++
			t.Logf("losing handoff error: %v", r.err)
		}
	}
	if wins != 1 || superseded+other != 1 {
		t.Fatalf("expected exactly one winning handoff, got wins=%d superseded=%d other=%d", wins, superseded, other)
	}

	// Exactly one active generation remains, and the superseded controller
	// cannot replace the winner with its stale credential.
	var active int
	if err := store.readDB.QueryRow(`SELECT count(*) FROM controller_leases WHERE run_id = ? AND status = 'active';`, runID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("expected exactly one active grant, got %d err=%v", active, err)
	}
}

func TestAC004_ReplayReturnsOriginalIssuanceNeverTheCandidate(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.AdoptController(ctx, "op-adopt-x", runID, "claude", "ref-a", "legacy-lease-g", nil, "committed-secret"); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	// Duplicate request with a different random candidate returns the
	// original committed issuance; the unused candidate is neither installed
	// nor disclosed.
	replay, err := store.AdoptController(ctx, "op-adopt-x", runID, "claude", "ref-a", "", &OperatorRecovery{Reason: "lost response", ExpectedGeneration: 1}, "fresh-unused-candidate")
	if err != nil {
		t.Fatalf("replay adopt: %v", err)
	}
	if replay.LeaseSecret != "committed-secret" {
		t.Fatalf("replay must return the original issuance, got %q", replay.LeaseSecret)
	}
	if replay.Generation != 1 {
		t.Fatalf("replay must not advance the generation, got %d", replay.Generation)
	}
	var count int
	if err := store.readDB.QueryRow(`SELECT count(*) FROM controller_leases WHERE run_id = ?;`, runID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("replay must not create rows (legacy+adopted only), got %d err=%v", count, err)
	}

	// Command-content mismatch on the same operation ID is a conflict.
	if _, err := store.AdoptController(ctx, "op-adopt-x", runID, "codex", "ref-a", "", &OperatorRecovery{Reason: "lost response", ExpectedGeneration: 1}, "c"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict for changed command, got %v", err)
	}
}

func TestAC004_RecoverIssuedCredential(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.AdoptController(ctx, "op-adopt-y", runID, "claude", "ref-a", "legacy-lease-g", nil, "secret-y"); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	// Operator-authorized recovery of the still-active grant returns the
	// same secret; recovery requires no old lease.
	recovered, err := store.RecoverControllerCredential(ctx, "op-recover-1", runID, "op-adopt-y", 1)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered.LeaseSecret != "secret-y" || recovered.Generation != 1 {
		t.Fatalf("recovery must return the same active grant, got %+v", recovered)
	}

	// After supersession there is no resurrection or disclosure.
	if _, err := store.HandoffController(ctx, "op-handoff-y", runID, "secret-y", 1, "codex", "ref-b", "secret-z"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if _, err := store.RecoverControllerCredential(ctx, "op-recover-2", runID, "op-adopt-y", 1); !errors.Is(err, ErrRecoveryUnavailable) {
		t.Fatalf("expected ErrRecoveryUnavailable after supersession, got %v", err)
	}

	// Stale expected generation on an otherwise valid recovery target is a
	// generation mismatch; the replacement stays untouched.
	if _, err := store.RecoverControllerCredential(ctx, "op-recover-3", runID, "op-handoff-y", 1); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("expected ErrGenerationMismatch for stale recovery generation, got %v", err)
	}
	recovered2, err := store.RecoverControllerCredential(ctx, "op-recover-4", runID, "op-handoff-y", 2)
	if err != nil || recovered2.LeaseSecret != "secret-z" {
		t.Fatalf("current generation recovery failed: %+v err=%v", recovered2, err)
	}
}

func TestAC004_GrantRecordsExposeNoSecrets(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.AdoptController(ctx, "op-adopt-s", runID, "agy", "ref-agy", "legacy-lease-g", nil, "super-secret-value"); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	rec, err := store.GetControllerRecord(ctx, runID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if strings.Contains(rec.ControllerRef, "super-secret-value") || strings.Contains(rec.Status, "super-secret-value") {
		t.Fatal("controller record must not disclose the lease secret")
	}

	// Ordinary operation receipts and hydrated journal evidence never embed
	// the secret.
	hydrated, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	for _, j := range hydrated.Journals {
		if strings.Contains(j.Payload, "super-secret-value") {
			t.Fatalf("journal payload disclosed the lease secret: %s", j.Payload)
		}
	}
}

func TestAC004_AllFourHarnessesAccepted(t *testing.T) {
	for i, harness := range []string{"opencode", "claude", "codex", "agy"} {
		t.Run(harness, func(t *testing.T) {
			store, runID := newGrantTestStore(t)
			defer store.Close()
			grant, err := store.AdoptController(context.Background(), "op-h4-"+string(rune('a'+i)), runID, harness, "ref-"+harness, "legacy-lease-g", nil, "sec")
			if err != nil {
				t.Fatalf("adopt with harness %s: %v", harness, err)
			}
			rec, err := store.GetControllerRecord(context.Background(), runID)
			if err != nil || rec.Harness != harness {
				t.Fatalf("harness not recorded: %+v err=%v", rec, err)
			}
			_ = grant
		})
	}
}

func TestAC004_OneActiveGrantEnforcedAtSQLLevel(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.AdoptController(ctx, "op-a1", runID, "claude", "ref-a", "legacy-lease-g", nil, "s1"); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	// Direct insertion of a second active row is rejected by the mandatory
	// partial unique index, not merely by transition logic.
	_, err := store.writeDB.Exec(`INSERT INTO controller_leases
		(run_id, generation, harness, controller_ref, lease, status, granted_by_op_id, attached_at, updated_at)
		VALUES (?, 9, 'codex', 'direct', 'direct-secret', 'active', 'op-direct', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z');`, runID)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("expected UNIQUE constraint failure for a second active grant, got %v", err)
	}
}

func TestAC004_InvalidGrantInputsLeaveNoTrace(t *testing.T) {
	store, runID := newGrantTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Invalid harness must fail without creating rows or journal entries.
	if _, err := store.AdoptController(ctx, "op-bad-harness", runID, "unknown-harness", "ref", "legacy-lease-g", nil, "s"); err == nil {
		t.Fatal("invalid harness must be rejected")
	}
	var rows int
	if err := store.readDB.QueryRow(`SELECT count(*) FROM controller_leases WHERE run_id = ?;`, runID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("invalid adoption must leave only the legacy row, got %d err=%v", rows, err)
	}
	if _, found, _ := store.FindOperationReceipt(ctx, "op-bad-harness", ""); found {
		t.Fatal("failed adoption must not journal")
	}

	// Wrong bootstrap credential is unauthorized.
	if _, err := store.AdoptController(ctx, "op-wrong-boot", runID, "claude", "ref", "not-the-legacy-lease", nil, "s"); !errors.Is(err, ErrUnauthorizedOperation) {
		t.Fatalf("expected ErrUnauthorizedOperation for wrong bootstrap credential, got %v", err)
	}

	// Nonexistent run fails cleanly.
	if _, err := store.AdoptController(ctx, "op-no-run", "run-missing", "claude", "ref", "", &OperatorRecovery{Reason: "x", ExpectedGeneration: 0}, "s"); err == nil {
		t.Fatal("adoption of a nonexistent run must fail")
	}

}
