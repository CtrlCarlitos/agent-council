//go:build unix

package main

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// adoptForTest converts a fixture's caller-supplied run lease into an
// explicit adopted controller grant (AC-004 Task 3 fixture conversion).
func adoptForTest(t *testing.T, store *storage.Store, runID, lease string) {
	t.Helper()
	if _, err := store.AdoptController(context.Background(), "op-adopt-fixture-"+runID, runID, "claude", "fixture-controller", lease, nil, lease); err != nil {
		t.Fatalf("adopt controller for fixture (run %s): %v", runID, err)
	}
}
