//go:build unix

package service

import (
	"context"
	"net/http"
	"testing"
)

// Gate 1 ordering regressions (head c3bee62): attachment revisions order
// episodes WITHIN a controller generation; they must not prevent the next
// controller generation from attaching.

// Direct coordinator cases: a replacement generation's first attachment
// must replace an older generation's record regardless of revision numbers.
func TestGate1ReviewC3B_GenerationOrdersBeforeRevision(t *testing.T) {
	c := NewCoordinator()

	// Generation 1, revision 1, episode A published.
	c.MarkControllerAttached("run-x", 1, "episode-A", "inst", 1)
	if !c.ControllerAttachedEpisode("run-x", 1, "episode-A") {
		t.Fatal("setup: generation 1 episode A must be published")
	}

	// Generation 2's FIRST attachment (revision restarts at 1 per generation
	// row) replaces the older generation.
	c.MarkControllerAttached("run-x", 2, "episode-B", "inst", 1)
	if !c.ControllerAttachedEpisode("run-x", 2, "episode-B") {
		t.Fatal("a replacement generation's first attachment must replace the older generation")
	}
	if c.ControllerAttachedEpisode("run-x", 1, "episode-A") {
		t.Fatal("the superseded generation's episode must not remain authoritative")
	}

	// Within the same generation, a newer revision wins and an older loses.
	c.MarkControllerAttached("run-x", 2, "episode-B2", "inst", 2)
	if !c.ControllerAttachedEpisode("run-x", 2, "episode-B2") {
		t.Fatal("same-generation newer revision must win")
	}
	c.MarkControllerAttached("run-x", 2, "episode-B", "inst", 1)
	if c.ControllerAttachedEpisode("run-x", 2, "episode-B") {
		t.Fatal("same-generation older revision must not replace the newer episode")
	}

	// An older generation cannot overwrite a successor even with a larger
	// revision number.
	c.MarkControllerAttached("run-x", 1, "episode-A9", "inst", 9)
	if c.ControllerAttachedEpisode("run-x", 1, "episode-A9") {
		t.Fatal("an older generation must not overwrite a successor despite a larger revision")
	}
	if !c.ControllerAttachedEpisode("run-x", 2, "episode-B2") {
		t.Fatal("the successor's episode must survive the older-generation publication")
	}
}

// Real-store/HTTP case: after a handoff, the replacement controller's first
// connect must publish and authorize decisions.
func TestGate1ReviewC3B_HandoffReplacementAttaches(t *testing.T) {
	dir := testStateDir(t)
	ctx := context.Background()
	srv, store, lock := gate1ServerFixture(t, dir, "inst-hb", "tok-hb")
	defer func() { _ = srv.Close(); _ = store.Close(); _ = lock.Release() }()
	if _, err := store.CreateRun(ctx, "op-run-hb", "run-hb", "b", "s", "p", "boot-A"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adoptForTest(t, store, "run-hb", "boot-A")

	// Generation 1 attaches (and accumulates revisions through a reconnect).
	gate1HTTPConnect(t, srv, "tok-hb", "run-hb", "boot-A", 1, "op-conn-a1")
	gate1HTTPConnect(t, srv, "tok-hb", "run-hb", "boot-A", 1, "op-conn-a2")

	// Handoff to generation 2.
	if _, err := store.HandoffController(ctx, "op-handoff-hb", "run-hb", "boot-A", 1, "codex", "ref-B", "lease-B"); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	// The replacement's first connect must publish generation 2 and
	// authorize its decisions.
	gate1HTTPConnect(t, srv, "tok-hb", "run-hb", "lease-B", 2, "op-conn-b1")
	rec, err := store.GetControllerRecord(ctx, "run-hb")
	if err != nil || rec.Generation != 2 || !rec.Connected {
		t.Fatalf("durable state must be generation 2 connected: %+v err=%v", rec, err)
	}
	if code := gate1Decision(t, srv, "tok-hb", "run-hb", "lease-B"); code == http.StatusConflict {
		t.Fatal("the replacement controller's first attachment must authorize decisions")
	}
}
