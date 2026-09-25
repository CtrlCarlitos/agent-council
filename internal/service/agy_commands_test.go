//go:build unix

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestAgyAttestationHTTP_AuthorityValidationAndReplay(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production Agy bootstrap is Linux-only")
	}
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seedAdopted(t, store, e.profile)
	srv, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	att := e.coveringAttestation(t, "operator-http")
	id, err := att.Digest()
	if err != nil {
		t.Fatal(err)
	}
	valid := AgyAttestationRequest{OpID: "att-http", Actor: att.Actor, AttestationID: id, Attestation: att}
	path := "/v1/runs/" + agyWireRunID + "/agy/attestations"
	post := func(token string, body any) (int, map[string]any) {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	for _, token := range []string{"", "foreign-operator", agyWireLease} {
		if code, out := post(token, valid); code != http.StatusUnauthorized {
			t.Fatalf("nonoperator token accepted: %d %v", code, out)
		}
	}
	for _, change := range []func(*AgyAttestationRequest){
		func(r *AgyAttestationRequest) { r.Actor = "worker" },
		func(r *AgyAttestationRequest) { r.AttestationID = "forged" },
		func(r *AgyAttestationRequest) { r.Attestation.ProbeRecords = nil },
		func(r *AgyAttestationRequest) { r.Attestation.ProfileDigest = "foreign-profile" },
	} {
		bad := valid
		change(&bad)
		if code, out := post(e.cfg.AuthToken, bad); code != http.StatusBadRequest {
			t.Fatalf("invalid attestation accepted: %d %v", code, out)
		}
	}
	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM agy_protection_attestations`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejections wrote attestation: %d %v", count, err)
	}
	code, original := post(e.cfg.AuthToken, valid)
	if code != http.StatusOK {
		t.Fatalf("valid: %d %v", code, original)
	}
	if code, replay := post(e.cfg.AuthToken, valid); code != http.StatusOK || fmt.Sprint(replay) != fmt.Sprint(original) {
		t.Fatalf("replay: %d %v", code, replay)
	}
	// Authority is checked even on receipt replay.
	if code, out := post(agyWireLease, valid); code != http.StatusUnauthorized {
		t.Fatalf("controller replay: %d %v", code, out)
	}
	conflict := valid
	conflict.Attestation.ProbedAt = "2026-09-24T12:00:00Z"
	conflict.AttestationID = ""
	if code, out := post(e.cfg.AuthToken, conflict); code != http.StatusConflict {
		t.Fatalf("changed evidence replay: %d %v", code, out)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE command_type='record_agy_attestation'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("journal count: %d %v", count, err)
	}
	agyNoChildAnywhere(t, e)
}

func TestAgyDispositionHTTP_RejectsLiveStaleAndDisconnected(t *testing.T) {
	acc := newAgyAcceptance(t, nil, nil)
	acc.create()
	acc.w.stage(t, agyKnown(agyWireNativeID), `{"wait_for_file":"gate"}`, agyUserInputDone, `{"exit_without_result":true}`)
	acc.queueRelease("live-disposition", "wait", nil)
	acc.w.waitCreationStarted(t, 2)
	ctx := context.Background()
	details, err := acc.store.GetTurnDetails(ctx, agyWireSession, "live-disposition")
	if err != nil {
		t.Fatal(err)
	}
	version, _ := acc.store.GetSessionVersion(ctx, agyWireSession)
	req := AgyTurnDispositionRequest{OpID: "disposition-negative", ControllerLease: agyWireLease, ExpectedGeneration: 1, ExpectedVersion: version, AttemptID: details.DispatchIntent.AttemptID, Disposition: "abandoned", Reason: "retired"}
	path := "/v1/runs/" + agyWireRunID + "/sessions/" + agyWireSession + "/turns/live-disposition/agy-disposition"
	post := func(r AgyTurnDispositionRequest) (int, map[string]any) {
		raw, _ := json.Marshal(r)
		return acc.bridge.do("POST", path, string(raw))
	}
	if code, out := post(req); code != http.StatusConflict {
		t.Fatalf("live attempt: %d %v", code, out)
	}
	// Retire the fixture, then exercise authority and optimistic fences.
	acc.w.openGate(t)
	acc.waitAttemptDead("live-disposition")
	version, _ = acc.store.GetSessionVersion(ctx, agyWireSession)
	req.ExpectedVersion = version
	for _, change := range []func(*AgyTurnDispositionRequest){
		func(r *AgyTurnDispositionRequest) { r.ControllerLease = "worker" },
		func(r *AgyTurnDispositionRequest) { r.ExpectedGeneration = 2 },
		func(r *AgyTurnDispositionRequest) { r.ExpectedVersion = version + 100 },
		func(r *AgyTurnDispositionRequest) { r.AttemptID = "foreign-attempt" },
		func(r *AgyTurnDispositionRequest) { r.Reason = "" },
		func(r *AgyTurnDispositionRequest) { r.Disposition = "completed" },
	} {
		bad := req
		change(&bad)
		if code, out := post(bad); code < 400 {
			t.Fatalf("invalid disposition accepted: %d %v", code, out)
		}
	}
	controller, err := acc.store.GetControllerRecord(ctx, agyWireRunID)
	if err != nil {
		t.Fatal(err)
	}
	disconnect := fmt.Sprintf(`{"op_id":"disconnect-disposition","controller_lease":%q,"expected_generation":1,"attachment_id":%q}`, agyWireLease, controller.AttachmentID)
	if code, out := acc.bridge.do("POST", "/v1/runs/"+agyWireRunID+"/controller/disconnect", disconnect); code != http.StatusOK {
		t.Fatalf("disconnect: %d %v", code, out)
	}
	if code, out := post(req); code != http.StatusConflict {
		t.Fatalf("disconnected disposition: %d %v", code, out)
	}
	a, _ := acc.store.GetAgyTurnAttempt(ctx, req.AttemptID)
	if a.UncertaintyDisposition != nil {
		t.Fatal("rejected disposition mutated attempt")
	}
	var count int
	if err := acc.store.DB().QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE command_type='resolve_agy_turn_uncertain'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected journal: %d %v", count, err)
	}
}
