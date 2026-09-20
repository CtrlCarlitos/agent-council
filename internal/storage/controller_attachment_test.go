package storage

import (
	"context"
	"testing"
)

// Regression evidence for AC-004 Task 5: attachment episodes are
// identity-tracked per generation, delayed events from superseded episodes
// cannot affect the current connection, and handoff/revoke invalidate the
// exact old attachment with transactional session projections.

func TestAC004_AttachmentEpisodesAndDelayedDisconnect(t *testing.T) {
	store, runID, _ := adoptedFixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	rec1, err := store.ConnectRunController(ctx, "op-conn-A1", runID, "lease-A", 1)
	if err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if rec1.AttachmentID == "" {
		t.Fatal("connect must establish an attachment identity")
	}

	// Replay of the same connect operation recovers the same episode.
	rec1Replay, err := store.ConnectRunController(ctx, "op-conn-A1", runID, "lease-A", 1)
	if err != nil || rec1Replay.AttachmentID != rec1.AttachmentID {
		t.Fatalf("replay must recover the existing episode: %+v err=%v", rec1Replay, err)
	}

	// Same controller, same generation: reconnecting creates episode B.
	rec2, err := store.ConnectRunController(ctx, "op-conn-B", runID, "lease-A", 1)
	if err != nil {
		t.Fatalf("second connect: %v", err)
	}
	if rec2.AttachmentID == rec1.AttachmentID {
		t.Fatal("reconnect must create a distinct attachment episode")
	}

	// A delayed disconnect from episode A leaves B connected.
	if _, err := store.DisconnectRunController(ctx, "op-disc-A", runID, "lease-A", rec1.AttachmentID, 1); err != nil {
		t.Fatalf("delayed disconnect: %v", err)
	}
	rec, err := store.GetControllerRecord(ctx, runID)
	if err != nil || !rec.Connected || rec.AttachmentID != rec2.AttachmentID {
		t.Fatalf("B must remain connected after A's delayed disconnect: %+v err=%v", rec, err)
	}

	// B's own disconnect ends the episode.
	if _, err := store.DisconnectRunController(ctx, "op-disc-B", runID, "lease-A", rec2.AttachmentID, 1); err != nil {
		t.Fatalf("B disconnect: %v", err)
	}
	rec, err = store.GetControllerRecord(ctx, runID)
	if err != nil || rec.Connected || rec.AttachmentID != "" {
		t.Fatalf("B's disconnect must end the episode: %+v err=%v", rec, err)
	}
}

func TestAC004_HandoffInvalidatesAttachmentAndProjects(t *testing.T) {
	store, runID, sessID := adoptedFixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	if _, err := store.ConnectRunController(ctx, "op-conn-h", runID, "lease-A", 1); err != nil {
		t.Fatalf("connect: %v", err)
	}

	if _, err := store.HandoffController(ctx, "op-handoff-inv", runID, "lease-A", 1, "codex", "ref-B", "lease-B"); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	rec, err := store.GetControllerRecord(ctx, runID)
	if err != nil || rec.Connected || rec.AttachmentID != "" {
		t.Fatalf("handoff must invalidate the old attachment: %+v err=%v", rec, err)
	}
	// Session projections were updated transactionally.
	var status string
	if err := store.readDB.QueryRow(`SELECT controller_status FROM sessions WHERE session_id = ?;`, sessID).Scan(&status); err != nil || status != "disconnected" {
		t.Fatalf("session projection must be disconnected after handoff: %q err=%v", status, err)
	}

	// The new controller connects explicitly (never inherits flags) and
	// reconnecting never creates a second controller.
	recB, err := store.ConnectRunController(ctx, "op-conn-B2", runID, "lease-B", 2)
	if err != nil {
		t.Fatalf("new controller connect: %v", err)
	}
	if recB.AttachmentID == "" {
		t.Fatal("new controller connect must establish an attachment")
	}
	var active int
	if err := store.readDB.QueryRow(`SELECT count(*) FROM controller_leases WHERE run_id = ? AND status = 'active';`, runID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("reconnect must not create a second controller: %d err=%v", active, err)
	}
}

func TestAC004_RevokeInvalidatesAttachment(t *testing.T) {
	store, runID, _ := adoptedFixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	if _, err := store.ConnectRunController(ctx, "op-conn-r", runID, "lease-A", 1); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := store.RevokeController(ctx, "op-revoke-inv", runID, "lease-A", nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rec, err := store.GetControllerRecord(ctx, runID)
	if err != nil || rec.Connected {
		t.Fatalf("revocation must end the attachment: %+v err=%v", rec, err)
	}
}

func TestAC004_ConnectRequiresCurrentGrantNotPriorConnection(t *testing.T) {
	store, runID, _ := adoptedFixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// Connecting while disconnected (no attachment) works — the connect
	// itself establishes the episode.
	if _, err := store.DisconnectRunController(ctx, "op-disc-x", runID, "lease-A", "nonexistent-attachment", 1); err != nil {
		t.Fatalf("stale-attachment disconnect on unattached grant: %v", err)
	}
	if _, err := store.ConnectRunController(ctx, "op-conn-fresh", runID, "lease-A", 1); err != nil {
		t.Fatalf("connect must not require a prior attachment: %v", err)
	}
}
