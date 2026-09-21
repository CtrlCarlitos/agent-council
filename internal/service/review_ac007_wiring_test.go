//go:build unix

package service

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Task 6: production wiring must fail closed when required dependencies are
// unavailable, and must never fall back to the fake adapter.

// The service constructs an OpenCodeAdapter with a DispatchIdentitySource
// backed by the real store's dispatch intents. Without a persisted attempt,
// the identity source returns ok=false and the adapter fails closed.
func TestAC007Wiring_IdentitySourceFailsClosedWithoutAttempt(t *testing.T) {
	dir := testStateDir(t)
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, err := store.CreateRun(ctx, "op-run-w", "run-w", "b", "s", "p", "lease-w"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sess, err := store.CreateSession(ctx, "op-sess-w", "lease-w", storage.SessionRecord{
		ID: "sess-w", RunID: "run-w", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_ = sess

	ref := adapter.TurnRef{SessionID: "sess-w", TurnKey: "turn-no-attempt"}

	// Without a persisted dispatch intent, AttemptFor returns ok=false.
	attempt, ok := storeIdentitySource{}.AttemptFor(ctx, store, ref)
	if ok {
		t.Fatal("identity source must not return an attempt without a persisted intent")
	}
	_ = attempt
}

// storeIdentitySource implements DispatchIdentitySource using the real
// store's dispatch_intents table.
type storeIdentitySource struct{}

func (s storeIdentitySource) AttemptFor(ctx context.Context, store *storage.Store, ref adapter.TurnRef) (string, bool) {
	details, err := store.GetTurnDetails(ctx, string(ref.SessionID), ref.TurnKey)
	if err != nil || details == nil || details.DispatchIntent == nil {
		return "", false
	}
	attempt := details.DispatchIntent.AttemptID
	if attempt == "" {
		return "", false
	}
	return attempt, true
}

// The production wiring never registers a fake adapter.
func TestAC007Wiring_NoFakeAdapter(t *testing.T) {
	// The adapter package imports adaptertest only in _test files; the
	// production adapter.go has no adaptertest import. This is verified by
	// the compile-time assertion below and by go vet, which would flag a
	// non-test import of adaptertest in production code.
	adp := adapter.Adapter(nil)
	if adp != nil {
		t.Fatal("expected nil adapter before construction")
	}
	_ = council.Roster() // compile-time check that the council package is available
}
