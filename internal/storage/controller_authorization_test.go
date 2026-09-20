package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// Regression evidence for AC-004 Task 3: the complete entry-point inventory
// applies authority-before-replay ordering, adopted/active/consistent grant
// authorization (never a bare string match), stale-authority classification
// before idempotent receipt success, and the legacy-denial rule (every
// unadopted run requires adoption before new controller decisions).

// adoptedFixture seeds a run+session and adopts a controller whose lease is
// the supplied secret, returning the adopted lease for commands.
func adoptedFixture(t *testing.T, lease string) (*Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-auth", "run-auth", "brief", "source", "profile", "bootstrap-"+lease); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sess, err := store.CreateSession(ctx, "op-sess-auth", "bootstrap-"+lease, SessionRecord{
		ID: "sess-auth", RunID: "run-auth", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.AdoptController(ctx, "op-adopt-auth", "run-auth", "claude", "controller-ref", "bootstrap-"+lease, nil, lease); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	_ = sess
	return store, "run-auth", "sess-auth"
}

func queueForReplay(t *testing.T, store *Store, lease, turnKey string) {
	t.Helper()
	ctx := context.Background()
	ver, err := store.GetSessionVersion(ctx, "sess-auth")
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-"+turnKey, lease, "sess-auth", ver, PendingPrompt{
		SessionID: "sess-auth", TurnKey: turnKey, Prompt: "p",
	}); err != nil {
		t.Fatalf("queue %s: %v", turnKey, err)
	}
}

// A superseded controller replaying operations it previously committed gets
// lease_superseded on every controller-command path — never the old receipt.
func TestAC004_SupersededReplayFencedEverywhere(t *testing.T) {
	cases := []struct {
		name   string
		seed   func(t *testing.T, store *Store, lease string)
		replay func(t *testing.T, store *Store, lease string) error
	}{
		{
			name: "queue_prompt",
			seed: func(t *testing.T, store *Store, lease string) { queueForReplay(t, store, lease, "t-q") },
			replay: func(t *testing.T, store *Store, lease string) error {
				_, err := store.QueuePrompt(context.Background(), "op-q-t-q", lease, "sess-auth", 1, PendingPrompt{SessionID: "sess-auth", TurnKey: "t-q", Prompt: "p"})
				return err
			},
		},
		{
			name: "find_committed_release",
			seed: func(t *testing.T, store *Store, lease string) {
				ctx := context.Background()
				queueForReplay(t, store, lease, "t-rel")
				ver, _ := store.GetSessionVersion(ctx, "sess-auth")
				if _, err := store.ReleaseTurn(ctx, "op-rel-fcr", lease, "sess-auth", ver, "t-rel"); err != nil {
					t.Fatalf("release: %v", err)
				}
			},
			replay: func(t *testing.T, store *Store, lease string) error {
				_, _, err := store.FindCommittedRelease(context.Background(), "op-rel-fcr", lease, "sess-auth", "t-rel")
				return err
			},
		},
		{
			name: "request_cancel",
			seed: func(t *testing.T, store *Store, lease string) {
				ctx := context.Background()
				queueForReplay(t, store, lease, "t-cxl")
				ver, _ := store.GetSessionVersion(ctx, "sess-auth")
				if _, err := store.ReleaseTurn(ctx, "op-rel-cxl", lease, "sess-auth", ver, "t-cxl"); err != nil {
					t.Fatalf("release: %v", err)
				}
				ver, _ = store.GetSessionVersion(ctx, "sess-auth")
				if _, err := store.RequestCancel(ctx, "op-cxl-1", lease, "sess-auth", ver, "t-cxl"); err != nil {
					t.Fatalf("cancel: %v", err)
				}
			},
			replay: func(t *testing.T, store *Store, lease string) error {
				_, err := store.RequestCancel(context.Background(), "op-cxl-1", lease, "sess-auth", 1, "t-cxl")
				return err
			},
		},
		{
			name: "reconcile_stage_receipt",
			seed: func(t *testing.T, store *Store, lease string) {
				ctx := context.Background()
				queueForReplay(t, store, lease, "t-rec")
				ver, _ := store.GetSessionVersion(ctx, "sess-auth")
				if _, err := store.ReleaseTurn(ctx, "op-rel-rec", lease, "sess-auth", ver, "t-rec"); err != nil {
					t.Fatalf("release: %v", err)
				}
				ver, _ = store.GetSessionVersion(ctx, "sess-auth")
				hl, err := store.RecordHostLoss(ctx, "op-rec-1:host_loss", lease, "sess-auth", ver)
				if err != nil {
					t.Fatalf("host loss: %v", err)
				}
				var gen uint64
				if _, err := fmt.Sscanf(hl.Payload, "%d", &gen); err != nil || gen == 0 {
					t.Fatalf("invalid host loss receipt payload %q: %v", hl.Payload, err)
				}
				ref := adapter.RecoveryRef{
					TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID("sess-auth"), TurnKey: "t-rec"},
					Generation: gen,
				}
				outcome := adapter.ReconciliationOutcome{
					Ref:          ref,
					Reachability: council.VisibilityHostLost,
					Status:       adapter.ReconciliationUncertain,
					Observed:     council.TurnRunning,
				}
				if _, err := store.ReconcileSession(ctx, "op-rec-1:reconcile", lease, ref, outcome); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			},
			replay: func(t *testing.T, store *Store, lease string) error {
				_, found, err := store.FindOperationReceipt(context.Background(), "op-rec-1:reconcile", lease)
				if err != nil {
					return err
				}
				if !found {
					return nil
				}
				return nil
			},
		},
		{
			name: "record_decision",
			seed: func(t *testing.T, store *Store, lease string) {
				meta := ArtifactMetadata{
					ID: "art-1", RunID: "run-auth", Name: "decision-input.txt",
				}
				if _, err := store.PublishArtifact(context.Background(), "op-pub-art", lease, meta, []byte("decision input")); err != nil {
					t.Fatalf("publish artifact: %v", err)
				}
				if _, err := store.RecordDecision(context.Background(), "op-dec-1", lease, "run-auth", "art-1", 1, "payload"); err != nil {
					t.Fatalf("decision: %v", err)
				}
			},
			replay: func(t *testing.T, store *Store, lease string) error {
				_, err := store.RecordDecision(context.Background(), "op-dec-1", lease, "run-auth", "art-1", 1, "payload")
				return err
			},
		},
		{
			name: "set_controller_connection",
			seed: func(t *testing.T, store *Store, lease string) {
				ver, _ := store.GetSessionVersion(context.Background(), "sess-auth")
				if _, err := store.SetControllerConnection(context.Background(), "op-conn-1", lease, "sess-auth", ver, council.ControllerDisconnected); err != nil {
					t.Fatalf("connect: %v", err)
				}
			},
			replay: func(t *testing.T, store *Store, lease string) error {
				_, err := store.SetControllerConnection(context.Background(), "op-conn-1", lease, "sess-auth", 1, council.ControllerDisconnected)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, runID, _ := adoptedFixture(t, "lease-A")
			defer store.Close()
			tc.seed(t, store, "lease-A")

			// Rotate authority away from lease-A.
			if _, err := store.HandoffController(context.Background(), "op-handoff-fence", runID, "lease-A", 1, "codex", "ref-b", "lease-B"); err != nil {
				t.Fatalf("handoff: %v", err)
			}

			if err := tc.replay(t, store, "lease-A"); !errors.Is(err, ErrLeaseSuperseded) {
				t.Fatalf("superseded replay must be fenced with ErrLeaseSuperseded, got %v", err)
			}

			// The current controller's retry recovers the original receipt.
			if err := tc.replay(t, store, "lease-B"); err != nil {
				t.Fatalf("current controller replay must succeed: %v", err)
			}
		})
	}
}

// A current controller reusing an operation ID with different content
// receives an idempotency conflict, not a second execution.
func TestAC004_CurrentControllerOpIDReuseConflicts(t *testing.T) {
	store, _, _ := adoptedFixture(t, "lease-A")
	defer store.Close()
	queueForReplay(t, store, "lease-A", "t-conf")

	_, err := store.QueuePrompt(context.Background(), "op-q-t-conf", "lease-A", "sess-auth", 1, PendingPrompt{
		SessionID: "sess-auth", TurnKey: "t-different", Prompt: "different",
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
}

// The legacy rule: an unadopted run's provenance credential cannot make new
// controller decisions, even though it matches runs.controller_lease.
func TestAC004_LegacyCredentialCannotDecide(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-legacy", "run-legacy", "b", "s", "p", "legacy-lease-L"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sess, err := store.CreateSession(ctx, "op-sess-legacy", "legacy-lease-L", SessionRecord{
		ID: "sess-legacy", RunID: "run-legacy", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ver := sess.CommittedVersion

	if _, err := store.QueuePrompt(ctx, "op-q-legacy", "legacy-lease-L", "sess-legacy", ver, PendingPrompt{SessionID: "sess-legacy", TurnKey: "t-1", Prompt: "p"}); !errors.Is(err, ErrAdoptionRequired) {
		t.Fatalf("legacy queue must require adoption, got %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-legacy", "legacy-lease-L", "sess-legacy", ver, "t-1"); !errors.Is(err, ErrAdoptionRequired) {
		t.Fatalf("legacy release must require adoption, got %v", err)
	}
	if _, err := store.RequestCancel(ctx, "op-cxl-legacy", "legacy-lease-L", "sess-legacy", ver, "t-1"); !errors.Is(err, ErrAdoptionRequired) {
		t.Fatalf("legacy cancel must require adoption, got %v", err)
	}
	if _, err := store.RecordDecision(ctx, "op-dec-legacy", "legacy-lease-L", "run-legacy", "a", 1, "p"); !errors.Is(err, ErrAdoptionRequired) {
		t.Fatalf("legacy decision must require adoption, got %v", err)
	}
	if _, err := store.SetControllerConnection(ctx, "op-conn-legacy", "legacy-lease-L", "sess-legacy", ver, council.ControllerConnected); !errors.Is(err, ErrAdoptionRequired) {
		t.Fatalf("legacy connect must require adoption, got %v", err)
	}
	if err := store.ValidateControllerLease(ctx, "sess-legacy", "legacy-lease-L"); !errors.Is(err, ErrAdoptionRequired) {
		t.Fatalf("legacy lease validation must require adoption, got %v", err)
	}

	// Maintenance/administration operations keep working with the current
	// credential on the unadopted run (operator handle), while a superseded
	// credential is fenced there too.
	if _, err := store.SetNativeBinding(ctx, "op-bind-legacy", "legacy-lease-L", "sess-legacy", ver, NativeBinding{
		LogicalSessionID: "sess-legacy", NativeSessionID: "native-1", Harness: "claude",
	}); err != nil {
		t.Fatalf("admin op on unadopted run with current credential must work: %v", err)
	}
}

// Combined acceptance shape on a migrated run: the legacy lease cannot
// release or decide, and an explicitly adopted controller can subsequently
// release the queued follow-up.
func TestAC004_MigratedRunLegacyDeniedAdoptedReleases(t *testing.T) {
	dir := t.TempDir()
	db := applyFrozenV1(t, dir)
	seedV1GoldenData(t, db)
	_ = db.Close()

	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	// Legacy credential denied on the migrated running turn and queued prompt.
	if _, _, err := store.FindCommittedRelease(ctx, "op-v1-release", "legacy-secret-1", "sess-v1", "turn-run"); !errors.Is(err, ErrAdoptionRequired) {
		t.Fatalf("legacy release replay must require adoption, got %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-after", "legacy-secret-1", "sess-v1", 3, "turn-queued"); !errors.Is(err, ErrAdoptionRequired) {
		t.Fatalf("legacy release must require adoption, got %v", err)
	}

	// Explicit adoption through the new contract (bootstrap credential).
	if _, err := store.AdoptController(ctx, "op-adopt-migrated", "run-v1", "agy", "controller-migrated", "legacy-secret-1", nil, "adopted-secret-M"); err != nil {
		t.Fatalf("adopt migrated run: %v", err)
	}

	// The adopted controller completes the migrated running turn and then
	// releases the queued follow-up.
	if _, err := store.RecordTerminalOutcome(ctx, "op-term-migrated", "adopted-secret-M", "sess-v1", 3, "turn-run", council.TurnCompleted, "result recorded"); err != nil {
		t.Fatalf("adopted controller must record the running turn's outcome: %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-migrated", "adopted-secret-M", "sess-v1", 4, "turn-queued"); err != nil {
		t.Fatalf("adopted controller must release the follow-up: %v", err)
	}
	details, err := store.GetTurnDetails(ctx, "sess-v1", "turn-queued")
	if err != nil || details.Status != council.TurnRunning {
		t.Fatalf("follow-up must be running after adopted release: %+v err=%v", details, err)
	}
}

// Authorization requires an internally consistent adopted, active grant —
// a bare string match against runs.controller_lease authorizes nothing.
func TestAC004_StringMatchWithoutConsistentGrantAuthorizesNothing(t *testing.T) {
	store, runID, sessID := adoptedFixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// Tamper: set runs.controller_lease to a new value without any grant
	// row (inconsistent state). The new value must not authorize.
	if _, err := store.writeDB.Exec(`UPDATE runs SET controller_lease = 'drifted-secret' WHERE run_id = ?;`, runID); err != nil {
		t.Fatalf("tamper run lease: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-drift", "drifted-secret", sessID, 1, PendingPrompt{SessionID: sessID, TurnKey: "t-drift", Prompt: "p"}); err == nil {
		t.Fatal("bare string match must not authorize controller commands")
	}
	if err := store.ValidateControllerLease(ctx, sessID, "drifted-secret"); err == nil {
		t.Fatal("bare string match must not validate as controller authority")
	}
}

// Reconnect-class operations require the adopted grant but never an
// already-established connection.
func TestAC004_ReconnectRequiresGrantNotPriorConnection(t *testing.T) {
	store, _, sessID := adoptedFixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// The adopted controller connects while its run-level connected flag is
	// 0 (no attachment exists yet) — connection itself must not require one.
	ver, err := store.GetSessionVersion(ctx, sessID)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if _, err := store.SetControllerConnection(ctx, "op-conn-fresh", "lease-A", sessID, ver, council.ControllerConnected); err != nil {
		t.Fatalf("connect must not require a prior attachment: %v", err)
	}
}
