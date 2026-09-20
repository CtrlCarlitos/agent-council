package storage

import (
	"context"
	"testing"
)

// adoptControllerForTest converts a fixture's caller-supplied run lease
// into an explicit adopted controller grant (AC-004 Task 3 fixture
// conversion), recorded through the production contract.
func adoptControllerForTest(t *testing.T, store *Store, runID, lease string) {
	t.Helper()
	if _, err := store.AdoptController(context.Background(), "op-adopt-fixture-"+runID, runID, "claude", "fixture-controller", lease, nil, lease); err != nil {
		t.Fatalf("adopt controller for fixture (run %s): %v", runID, err)
	}
	if _, err := store.ConnectRunController(context.Background(), "op-conn-fixture-"+runID, runID, lease, 1, "test-instance"); err != nil {
		t.Fatalf("connect fixture controller (run %s): %v", runID, err)
	}
}
