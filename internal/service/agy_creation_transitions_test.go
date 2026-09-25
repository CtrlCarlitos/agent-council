//go:build unix

package service

// AC-010 Task 7 fix round 1: the service-owned creation transitions.
// Every post-creation durable write survives a cancelled request
// context (client disconnect is not cancellation); a pre-launch
// creation marker closes the create→bind crash gap; the drift and
// clean-rejection outcomes leave exactly one correctly-disposed
// episode; the required-tools seam's error channel refuses a dispatch.
// Fixture only (the compiled agytest `agy`, temp dirs).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/agy"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Important 1: the request ctx is cancelled while the creation child is
// in flight. The created conversation must still be durably accounted
// for (a binding or an open episode carrying its id), and a restarted
// service must not start a second creation child.
func TestServiceAgySession_CancelledRequestStillRecordsTheCreatedConversation(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	w.stage(t, `{"conversation_id": "`+agyWireNativeID+`"}`, `{"wait_for_file": "gate"}`)

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		binding adapter.SessionBinding
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		b, _, err := w.srv.CreateAgySession(reqCtx, "op-create-cancel", agyWireLease, agyWireSession)
		done <- outcome{b, err}
	}()
	w.waitCreationStarted(t, 1)
	cancel() // the client disconnects AFTER the child started
	w.openGate(t)
	got := <-done

	ctx := context.Background()
	bound, err := store.GetAgySessionBinding(ctx, agyWireSession)
	if err != nil {
		t.Fatalf("binding lookup: %v", err)
	}
	open, err := store.OpenAgyCreationUncertainty(ctx, agyWireSession)
	if err != nil {
		t.Fatalf("episode lookup: %v", err)
	}
	switch {
	case bound != nil && bound.NativeID == agyWireNativeID:
		if open != nil {
			t.Fatalf("a bound session keeps no open episode, got %+v", open)
		}
		if got.err != nil || got.binding.NativeSessionID != agyWireNativeID {
			t.Fatalf("the bind completes despite the disconnect, got %+v err=%v", got.binding, got.err)
		}
	case open != nil && open.OrphanNativeID != nil && *open.OrphanNativeID == agyWireNativeID:
	default:
		t.Fatalf("the created conversation must be bound or carried by an open episode; binding=%+v episode=%+v err=%v",
			bound, open, got.err)
	}

	// A restarted service starts no second creation child: the replay
	// of the same op returns the committed receipt; another op is refused.
	w2 := e.fixtureServerSharing(t, store, w)
	if b, receipt, err := w2.srv.CreateAgySession(ctx, "op-create-cancel", agyWireLease, agyWireSession); err != nil ||
		b.NativeSessionID != agyWireNativeID || receipt.OpID != "op-create-cancel" {
		t.Fatalf("the restarted replay returns the committed receipt, got %+v %+v err=%v", b, receipt, err)
	}
	if _, _, err := w2.srv.CreateAgySession(ctx, "op-create-other", agyWireLease, agyWireSession); err == nil ||
		!strings.Contains(err.Error(), "already bound") {
		t.Fatalf("another op on a bound session is refused, got %v", err)
	}
	if n := w.creationLaunches(); n != 1 {
		t.Fatalf("no second creation child after the restart, launches=%d", n)
	}
	if eps, _ := store.AgyCreationUncertaintyEpisodes(ctx, agyWireSession); len(eps) != 1 {
		t.Fatalf("replays open no new marker, episodes=%+v", eps)
	}
}

// Important 2 (a): the process "crashes" after the native creation
// succeeded and before the bind. The pre-launch marker is durable: a
// restarted service is blocked and starts NO creation child until the
// controller resolves it; then creation proceeds.
func TestServiceAgySession_CrashBetweenCreateAndBindIsBlockedByTheMarker(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	ctx := context.Background()
	w.stage(t, `{"conversation_id": "`+agyWireNativeID+`"}`)
	var crashedAt string
	w.srv.agyAfterNativeCreate = func(nativeID string) bool { crashedAt = nativeID; return true }
	if _, _, err := w.srv.CreateAgySession(ctx, "op-create-crash", agyWireLease, agyWireSession); err == nil {
		t.Fatal("the crash seam must stop the birth")
	}
	if crashedAt != agyWireNativeID || w.creationLaunches() != 1 {
		t.Fatalf("the native creation succeeded before the crash, id=%q launches=%d", crashedAt, w.creationLaunches())
	}
	if b, _ := store.GetAgySessionBinding(ctx, agyWireSession); b != nil {
		t.Fatalf("nothing is bound across the crash, got %+v", b)
	}
	marker := w.assertOneEpisodeOpenFromOp(t, store, "op-create-crash")
	if marker.Reason != storage.AgyCreationInFlightReason || marker.OrphanNativeID != nil {
		t.Fatalf("the crash left the bare pre-launch marker, got %+v", marker)
	}

	// Restart: blocked by the durable marker before any child, for the
	// same op and for a new one.
	w.stage(t, `{"conversation_id": "`+agyWireOtherID+`"}`)
	w2 := e.fixtureServerSharing(t, store, w)
	var unc *adapter.ErrSessionCreationUncertain
	for _, op := range []string{"op-create-crash", "op-create-after-crash"} {
		if _, _, err := w2.srv.CreateAgySession(ctx, op, agyWireLease, agyWireSession); !errors.As(err, &unc) ||
			!strings.Contains(err.Error(), storage.AgyCreationInFlightReason) {
			t.Fatalf("%s: the in-flight marker blocks the restarted service, got %v", op, err)
		}
	}
	if n := w.creationLaunches(); n != 1 {
		t.Fatalf("no creation child while the marker is open, launches=%d", n)
	}
	if _, err := w2.srv.ResolveAgySessionCreationUncertainty(ctx, AgyCreationUncertaintyResolution{
		OpID: "op-resolve-crash", ControllerLease: agyWireLease, ExpectedGeneration: 1,
		SessionID: agyWireSession, Episode: marker.Episode, Disposition: storage.AgyUncertaintyAbandonOrphan,
		Reason: "conversation of the crashed birth is abandoned",
	}); err != nil {
		t.Fatalf("the controller resolves the in-flight marker: %v", err)
	}
	b, _, err := w2.srv.CreateAgySession(ctx, "op-create-after-resolve", agyWireLease, agyWireSession)
	if err != nil || b.NativeSessionID != agyWireOtherID || w.creationLaunches() != 2 {
		t.Fatalf("after resolution creation proceeds, got %+v err=%v launches=%d", b, err, w.creationLaunches())
	}
	eps, _ := store.AgyCreationUncertaintyEpisodes(ctx, agyWireSession)
	if len(eps) != 2 || *eps[0].Disposition != storage.AgyUncertaintyAbandonOrphan || *eps[1].Disposition != storage.AgyUncertaintyBound {
		t.Fatalf("history: the resolved crash marker, then the bound marker; got %+v", eps)
	}
}

// Minor: the valid-UUID creation-drift path (ErrCreationDrift) leaves
// ONE open episode — the service's marker — carrying the orphan id with
// the SERVICE's op provenance.
func TestServiceAgySession_CreationDriftAnnotatesTheServiceMarker(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	ctx := context.Background()
	w.stage(t, `{"conversation_id": "`+agyWireNativeID+`"}`, `{"permission_mode": "strict"}`)
	_, _, err := w.srv.CreateAgySession(ctx, "op-create-drift", agyWireLease, agyWireSession)
	var drift *agy.ErrCreationDrift
	if !errors.As(err, &drift) || drift.NativeID != agyWireNativeID {
		t.Fatalf("want ErrCreationDrift carrying the created id, got %T: %v", err, err)
	}
	ep := w.assertOneEpisodeOpenFromOp(t, store, "op-create-drift")
	if ep.OrphanNativeID == nil || *ep.OrphanNativeID != agyWireNativeID || drift.Episode != ep.Episode ||
		!strings.HasPrefix(ep.Reason, storage.AgyCreationInFlightReason+": creation drift") {
		t.Fatalf("the service marker carries the orphan and the drift outcome, got %+v (error episode %d)", ep, drift.Episode)
	}
	if eps, _ := store.AgyCreationUncertaintyEpisodes(ctx, agyWireSession); len(eps) != 1 {
		t.Fatalf("exactly one episode in the whole history, got %+v", eps)
	}
}

// rejectingAgy is the wired fixture adapter whose CreateSession rejects
// before any child (the adapter contract for a non-uncertain error).
type rejectingAgy struct {
	*agy.AgyAdapter
}

func (rejectingAgy) CreateSession(context.Context, adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	return adapter.SessionBinding{}, errors.New("creation launch: refused before any child")
}

// A creation the adapter rejected before any child existed closes the
// marker not_created: no native identity can exist, nothing blocks.
func TestServiceAgySession_CleanRejectionClosesTheMarker(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	w.srv.adapter = rejectingAgy{w.adp}
	ctx := context.Background()
	if _, _, err := w.srv.CreateAgySession(ctx, "op-create-rejected", agyWireLease, agyWireSession); err == nil ||
		!strings.Contains(err.Error(), "refused before any child") {
		t.Fatalf("want the adapter rejection, got %v", err)
	}
	if open, _ := store.OpenAgyCreationUncertainty(ctx, agyWireSession); open != nil {
		t.Fatalf("a clean rejection leaves no open episode, got %+v", open)
	}
	eps, _ := store.AgyCreationUncertaintyEpisodes(ctx, agyWireSession)
	if len(eps) != 1 || eps[0].Disposition == nil || *eps[0].Disposition != storage.AgyUncertaintyNotCreated {
		t.Fatalf("the marker is closed not_created, got %+v", eps)
	}
	w.srv.adapter = w.adp
	w.stage(t, `{"conversation_id": "`+agyWireNativeID+`"}`)
	if b, _, err := w.srv.CreateAgySession(ctx, "op-create-next", agyWireLease, agyWireSession); err != nil || b.NativeSessionID != agyWireNativeID {
		t.Fatalf("the next birth proceeds, got %+v err=%v", b, err)
	}
}

// Important 3 at the service seam: a turn without a durable dispatch
// intent is refused DispatchRejected (typed) before any reservation —
// no attempt row, no child — never dispatched against the defaults.
func TestServiceAgyDispatch_MissingIntentRefusedBeforeReservation(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	ctx := context.Background()
	w.stage(t, `{"conversation_id": "`+agyWireNativeID+`"}`)
	if _, _, err := w.srv.CreateAgySession(ctx, "op-create-dispatch", agyWireLease, agyWireSession); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := w.creationLaunches()
	out, err := w.adp.Dispatch(ctx, adapter.TurnRef{SessionID: agyWireSession, TurnKey: "t-no-intent"}, "review")
	var typed *agy.ErrRequiredToolsUnavailable
	if !errors.As(err, &typed) || out.Status != adapter.DispatchRejected {
		t.Fatalf("a missing intent is a typed pre-transmission rejection, got %+v err=%v", out, err)
	}
	if n := w.creationLaunches(); n != before {
		t.Fatalf("no child, launches %d -> %d", before, n)
	}
	if a, err := store.GetLatestAgyTurnAttempt(ctx, agyWireSession, "t-no-intent"); err != nil || a != nil {
		t.Fatalf("no attempt row, got %+v err=%v", a, err)
	}
}
