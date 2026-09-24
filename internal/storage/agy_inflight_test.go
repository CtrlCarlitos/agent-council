package storage

// AC-010 Task 7 fix round 1 (Important 2): the service-owned pre-launch
// creation marker. An in-flight episode is opened BEFORE the creation
// child, blocks birth exactly like any other open episode, is closed
// with disposition "bound" by BindAgySessionClosingEpisode in the bind's
// own transaction, and otherwise stays open (annotated with the observed
// orphan) until a controller resolves it.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const agyInFlightNative = "0195f7a1-2b3c-4def-9abc-def012345678"

func agyInFlightBinding() AgySessionBinding {
	return AgySessionBinding{SessionID: agyUncSession, NativeID: agyInFlightNative,
		Model: "agy-1", Workspace: "/tmp/ws", ProfileDigest: "aprof-v1:sha256:pd"}
}

func TestAgyInFlight_MarkerBlocksAndBindClosesItInOneTransaction(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()

	ep, receipt, err := store.BeginAgyCreationInFlight(ctx, agyUncRunID, agyUncSession, "op-create-1", "agy-service")
	if err != nil || ep != 1 || receipt.CommandType != agyUncertainCmd {
		t.Fatalf("begin marker: ep=%d receipt=%+v err=%v", ep, receipt, err)
	}
	open, err := store.OpenAgyCreationUncertainty(ctx, agyUncSession)
	if err != nil || open == nil || open.Episode != 1 || open.Reason != AgyCreationInFlightReason ||
		open.CauseOpID != "op-create-1" || open.RecordedBy != "agy-service" || open.OrphanNativeID != nil || !open.IsAgyCreationInFlight() {
		t.Fatalf("the in-flight marker must be the open episode, got %+v err=%v", open, err)
	}
	if has, _ := store.HasAgyCreationUncertainty(ctx, agyUncSession); !has {
		t.Fatal("an in-flight marker blocks birth like any open episode")
	}
	var typed *ErrAgyCreationUncertaintyOpen
	if _, _, err := store.BeginAgyCreationInFlight(ctx, agyUncRunID, agyUncSession, "op-create-2", "agy-service"); !errors.As(err, &typed) || typed.Episode != 1 {
		t.Fatalf("a second marker is refused typed, got %v", err)
	}
	if _, err := store.BindAgySession(ctx, "op-bind-plain", agyUncLease, agyInFlightBinding()); !errors.As(err, &typed) {
		t.Fatalf("a plain bind is refused while an episode is open, got %v", err)
	}
	if _, err := store.BindAgySessionClosingEpisode(ctx, "op-bind-wrong-ep", agyUncLease, agyInFlightBinding(), 2); err == nil {
		t.Fatal("closing a different episode than the open marker must be refused")
	}
	if b, _ := store.GetAgySessionBinding(ctx, agyUncSession); b != nil {
		t.Fatal("refused binds persist nothing")
	}

	receipt, err = store.BindAgySessionClosingEpisode(ctx, "op-create-1", agyUncLease, agyInFlightBinding(), 1)
	if err != nil || receipt.CommandType != "bind_agy_session" || receipt.Payload != agyInFlightNative {
		t.Fatalf("closing bind: %+v err=%v", receipt, err)
	}
	if open, _ := store.OpenAgyCreationUncertainty(ctx, agyUncSession); open != nil {
		t.Fatalf("the successful bind closes the marker, still open: %+v", open)
	}
	eps, err := store.AgyCreationUncertaintyEpisodes(ctx, agyUncSession)
	if err != nil || len(eps) != 1 || eps[0].Disposition == nil || *eps[0].Disposition != AgyUncertaintyBound ||
		eps[0].ResolutionOpID == nil || *eps[0].ResolutionOpID != "op-create-1" ||
		eps[0].ResolutionGeneration == nil || *eps[0].ResolutionGeneration != 1 || eps[0].ResolvedAt == nil {
		t.Fatalf("the history shows the marker closed bound by the bind op, got %+v err=%v", eps, err)
	}
	// Replay by op id through either entry point returns the receipt
	// and opens nothing.
	if r, err := store.BindAgySessionClosingEpisode(ctx, "op-create-1", agyUncLease, agyInFlightBinding(), 1); err != nil || r.OpID != receipt.OpID {
		t.Fatalf("closing-bind replay: %+v err=%v", r, err)
	}
	if r, err := store.BindAgySession(ctx, "op-create-1", agyUncLease, agyInFlightBinding()); err != nil || r.OpID != receipt.OpID {
		t.Fatalf("plain replay of the create op: %+v err=%v", r, err)
	}
	if _, _, err := store.BeginAgyCreationInFlight(ctx, agyUncRunID, agyUncSession, "op-create-3", "agy-service"); err == nil ||
		!strings.Contains(err.Error(), "already bound") {
		t.Fatalf("a bound session cannot open a marker, got %v", err)
	}
}

func TestAgyInFlight_RefusedBindLeavesMarkerOpenAndNothingBound(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	if _, _, err := store.BeginAgyCreationInFlight(ctx, agyUncRunID, agyUncSession, "op-create-1", "agy-service"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.HandoffController(ctx, "op-handoff", agyUncRunID, agyUncLease, 1, "agy", "ctrl-ref-2", "lease-agy-unc-gen2"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if _, err := store.BindAgySessionClosingEpisode(ctx, "op-create-1", agyUncLease, agyInFlightBinding(), 1); !errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("a superseded lease cannot bind, got %v", err)
	}
	if b, _ := store.GetAgySessionBinding(ctx, agyUncSession); b != nil {
		t.Fatal("nothing bound")
	}
	open, _ := store.OpenAgyCreationUncertainty(ctx, agyUncSession)
	if open == nil || open.Episode != 1 {
		t.Fatalf("the marker stays open after a refused bind, got %+v", open)
	}
	if n := journalCount(t, store, "bind_agy_session"); n != 0 {
		t.Fatalf("the refused bind journals nothing, got %d", n)
	}
}

func TestAgyInFlight_AnnotationKeepsOneOpenEpisode(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	ep, _, err := store.BeginAgyCreationInFlight(ctx, agyUncRunID, agyUncSession, "op-create-1", "agy-service")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// The adapter's own drift record lands on the service's marker.
	got, _, err := store.RecordAgyCreationUncertain(ctx, AgyCreationUncertainty{RunID: agyUncRunID, SessionID: agyUncSession,
		Reason: "creation drift after a valid init: model", RecordedBy: "agy-adapter", CauseOpID: "agy-create-x",
		OrphanNativeID: agyInFlightNative})
	if err != nil || got != ep {
		t.Fatalf("the adapter record must replay the open marker, got %d err=%v", got, err)
	}
	eps, _ := store.AgyCreationUncertaintyEpisodes(ctx, agyUncSession)
	if len(eps) != 1 || eps[0].OrphanNativeID == nil || *eps[0].OrphanNativeID != agyInFlightNative ||
		eps[0].Reason != AgyCreationInFlightReason+": creation drift after a valid init: model" ||
		eps[0].RecordedBy != "agy-service" || eps[0].CauseOpID != "op-create-1" || !eps[0].IsAgyCreationInFlight() {
		t.Fatalf("exactly one episode, annotated with the orphan and outcome, service provenance kept; got %+v", eps)
	}
	if err := store.SetAgyCreationUncertaintyOrphan(ctx, agyUncSession, ep, "0195f7a1-2b3c-4def-9abc-def012345679", "other"); err == nil {
		t.Fatal("a different orphan id must be refused")
	}
	if err := store.SetAgyCreationUncertaintyOrphan(ctx, agyUncSession, ep, agyInFlightNative, "restated"); err != nil {
		t.Fatalf("restating the same orphan is accepted: %v", err)
	}
	if err := store.CloseAgyCreationInFlight(ctx, agyUncSession, ep, AgyUncertaintyNotCreated, "no child"); err == nil {
		t.Fatal("an annotated marker needs a controller resolution, not an automatic close")
	}
	if _, err := store.ResolveAgyCreationUncertainty(ctx, "op-resolve", agyUncLease, 1, agyUncSession, ep,
		AgyUncertaintyAbandonOrphan, "operator abandons the drifted conversation"); err != nil {
		t.Fatalf("an in-flight episode is resolvable by the controller: %v", err)
	}
	if has, _ := store.HasAgyCreationUncertainty(ctx, agyUncSession); has {
		t.Fatal("resolved")
	}
}

func TestAgyInFlight_CleanRejectionClosesUnannotatedMarker(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	ep, _, err := store.BeginAgyCreationInFlight(ctx, agyUncRunID, agyUncSession, "op-create-1", "agy-service")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.CloseAgyCreationInFlight(ctx, agyUncSession, ep, AgyUncertaintyAbandonOrphan, "x"); err == nil {
		t.Fatal("only not_created|bound may close a marker automatically")
	}
	if err := store.CloseAgyCreationInFlight(ctx, agyUncSession, ep, AgyUncertaintyNotCreated, "creation rejected before any child"); err != nil {
		t.Fatalf("close: %v", err)
	}
	eps, _ := store.AgyCreationUncertaintyEpisodes(ctx, agyUncSession)
	if len(eps) != 1 || eps[0].Disposition == nil || *eps[0].Disposition != AgyUncertaintyNotCreated {
		t.Fatalf("closed not_created, got %+v", eps)
	}
	if ep2, _, err := store.BeginAgyCreationInFlight(ctx, agyUncRunID, agyUncSession, "op-create-2", "agy-service"); err != nil || ep2 != 2 {
		t.Fatalf("the next creation opens the next episode, got %d err=%v", ep2, err)
	}
}
