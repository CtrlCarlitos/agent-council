package service

import (
	"context"
	"errors"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// A controller resolution of the in-flight marker a live birth of THIS
// process owns is refused typed; the live birth then binds normally and
// closes its marker bound.
func TestServiceAgySession_ResolveLiveCreationMarkerRefusedTyped(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	w.stage(t, `{"wait_for_file": "gate"}`, `{"conversation_id": "`+agyWireNativeID+`"}`)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _, err := w.srv.CreateAgySession(ctx, "op-create-live", agyWireLease, agyWireSession)
		done <- err
	}()
	w.waitCreationStarted(t, 1)
	marker := w.assertOneEpisodeOpenFromOp(t, store, "op-create-live")

	_, err := w.srv.ResolveAgySessionCreationUncertainty(ctx, AgyCreationUncertaintyResolution{
		OpID: "op-resolve-live", ControllerLease: agyWireLease, ExpectedGeneration: 1,
		SessionID: agyWireSession, Episode: marker.Episode, Disposition: storage.AgyUncertaintyVerifiedAbsent,
		Reason: "controller thinks nothing was created",
	})
	var inProgress *ErrCreationInProgress
	if !errors.As(err, &inProgress) || inProgress.Episode != marker.Episode || inProgress.SessionID != agyWireSession {
		t.Fatalf("resolving a live marker must be refused typed ErrCreationInProgress, got %T: %v", err, err)
	}
	w.openGate(t)
	if err := <-done; err != nil {
		t.Fatalf("the live birth binds normally after the refused resolution: %v", err)
	}
	b, err := store.GetAgySessionBinding(ctx, agyWireSession)
	if err != nil || b == nil || b.NativeID != agyWireNativeID {
		t.Fatalf("the session is bound to the created conversation, got %+v err=%v", b, err)
	}
	eps, err := store.AgyCreationUncertaintyEpisodes(ctx, agyWireSession)
	if err != nil || len(eps) != 1 || eps[0].Disposition == nil || *eps[0].Disposition != storage.AgyUncertaintyBound {
		t.Fatalf("the one marker is closed bound, got %+v err=%v", eps, err)
	}
}

// The fallback of recordAgyBindingOrphan: the marker is resolved out
// from under a running creation — TEST-ONLY, by calling the storage
// resolution directly, bypassing the service's in-flight refusal. The
// failed bind must not lose the created id: exactly one NEW open episode
// carries it, and the next birth is blocked before any child.
func TestServiceAgySession_OrphanFallbackOpensNewEpisodeWhenMarkerGone(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	w.stage(t, `{"wait_for_file": "gate"}`, `{"conversation_id": "`+agyWireNativeID+`"}`)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _, err := w.srv.CreateAgySession(ctx, "op-create-lost-marker", agyWireLease, agyWireSession)
		done <- err
	}()
	w.waitCreationStarted(t, 1)
	marker := w.assertOneEpisodeOpenFromOp(t, store, "op-create-lost-marker")
	if _, err := store.ResolveAgyCreationUncertainty(ctx, "op-resolve-under", agyWireLease, 1,
		agyWireSession, marker.Episode, storage.AgyUncertaintyVerifiedAbsent, "test-only: resolved via storage directly"); err != nil {
		t.Fatalf("storage resolution (test seam): %v", err)
	}
	w.openGate(t)
	if err := <-done; err == nil {
		t.Fatal("the bind must fail: its marker episode is no longer open")
	}
	if b, err := store.GetAgySessionBinding(ctx, agyWireSession); err != nil || b != nil {
		t.Fatalf("the session stays unbound, got %+v err=%v", b, err)
	}
	eps, err := store.AgyCreationUncertaintyEpisodes(ctx, agyWireSession)
	if err != nil {
		t.Fatalf("episodes: %v", err)
	}
	var open []storage.AgyCreationUncertaintyEpisode
	for _, ep := range eps {
		if ep.Disposition == nil {
			open = append(open, ep)
		}
	}
	if len(eps) != 2 || len(open) != 1 || open[0].Episode == marker.Episode ||
		open[0].OrphanNativeID == nil || *open[0].OrphanNativeID != agyWireNativeID ||
		open[0].RunID != agyWireRunID || open[0].CauseOpID != "op-create-lost-marker" {
		t.Fatalf("exactly one NEW open episode carries the created id, got %+v", eps)
	}
	before := w.creationLaunches()
	_, _, err = w.srv.CreateAgySession(ctx, "op-create-next", agyWireLease, agyWireSession)
	var unc *adapter.ErrSessionCreationUncertain
	if !errors.As(err, &unc) {
		t.Fatalf("the next birth is blocked by the new episode, got %T: %v", err, err)
	}
	if n := w.creationLaunches(); n != before {
		t.Fatalf("the blocked birth starts no child, launches %d -> %d", before, n)
	}
}
