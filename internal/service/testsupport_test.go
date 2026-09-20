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

// connectControllerForTest establishes the run-scoped attachment episode
// and the in-instance gate record for an adopted fixture controller
// (AC-004 Task 5 fixture conversion).
func connectControllerForTest(t *testing.T, srv *Server, store *storage.Store, runID, lease string) {
	t.Helper()
	rec, err := store.GetControllerRecord(context.Background(), runID)
	if err != nil || !rec.Adopted {
		t.Fatalf("fixture controller not adopted (run %s): %+v err=%v", runID, rec, err)
	}
	receipt, err := store.ConnectRunController(context.Background(), "op-conn-fixture-"+runID, runID, lease, rec.Generation)
	if err != nil {
		t.Fatalf("fixture connect (run %s): %v", runID, err)
	}
	srv.Coordinator().MarkControllerAttached(runID, rec.Generation, receipt.AttachmentID, srv.InstanceID())
}
