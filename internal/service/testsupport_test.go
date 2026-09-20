//go:build unix

package service

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// adoptForTest converts a fixture's caller-supplied run lease into an
// explicit adopted controller grant (AC-004 Task 3 fixture conversion),
// recorded through the production storage contract.
func adoptForTest(t *testing.T, store *storage.Store, runID, lease string) {
	t.Helper()
	if _, err := store.AdoptController(context.Background(), "op-adopt-fixture-"+runID, runID, "claude", "fixture-controller", lease, nil, lease); err != nil {
		t.Fatalf("adopt controller for fixture (run %s): %v", runID, err)
	}
}
