//go:build unix

package service

// AC-010 spec §14.18 evidence: the attestation bootstrap state. A server
// configured with AgyBinaryPath but without a covering cprot-v2 row
// starts WITHOUT the agy adapter in the explicit awaiting_attestation
// state (AgyStatus, GET /v1/status); birth, release, reconcile and
// queue-time validation refuse with the typed ineligibility error, other
// agy operations report the harness as unavailable, no child starts; the first row is
// recordable on that server (RecordAgyProbeAttestation needs no wired
// adapter); a restart over the same store constructs the production
// adapter. Every other construction error still fails the server.
//
// Fixture only: the compiled agytest `agy` executable, temp homes, temp
// evidence roots. The real agy binary and ~/.gemini are never touched.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/agy"
)

// agyServeJSON runs one authenticated in-process request against the
// server's handler.
func agyServeJSON(t *testing.T, srv *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+srv.cfg.AuthToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// agyNoChildAnywhere reports whether any fixture child left a trace
// under the env's scratch root or workspace base (construction probe,
// creation or turn launch).
func agyNoChildAnywhere(t *testing.T, e *agyWireEnv) {
	t.Helper()
	for _, root := range []string{e.scratch, e.wsBase} {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), ".agy-fixture-") && !strings.HasSuffix(d.Name(), "scenario.jsonl") {
				t.Fatalf("no agy child may start while awaiting attestation, found %s", path)
			}
			return nil
		})
	}
}

func TestServiceBootstrap_AgyAwaitingAttestationRecordRestart(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production agy construction is Linux-only (spec §3.12)")
	}
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seedAdopted(t, store, e.profile)
	ctx := context.Background()

	// (a) No covering row: the server starts in awaiting_attestation.
	srv, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, nil)
	if err != nil {
		t.Fatalf("a missing attestation must not fail the server: %v", err)
	}
	st := srv.AgyStatus()
	if st.State != AgyAwaitingAttestation || !strings.Contains(st.Reason, "attestation") || !strings.Contains(st.Reason, "restart") ||
		!strings.Contains(st.Reason, "/agy/attestations") {
		t.Fatalf("want the awaiting_attestation state naming the restart, got %+v", st)
	}
	if srv.adapter != nil {
		t.Fatalf("no adapter is wired while awaiting attestation, got %T", srv.adapter)
	}
	code, resp := agyServeJSON(t, srv, "GET", "/v1/status", "")
	agyBlock, _ := resp["agy"].(map[string]any)
	if code != http.StatusOK || agyBlock["state"] != AgyAwaitingAttestation || agyBlock["reason"] != st.Reason {
		t.Fatalf("GET /v1/status must surface the agy block, got %d %v", code, resp)
	}
	if constructionProbeRan(e.scratch) {
		t.Fatal("eligibility fails closed before the construction child")
	}

	// The controller attaches to THIS instance (new decisions need it).
	if code, resp := agyServeJSON(t, srv, "POST", "/v1/runs/"+agyWireRunID+"/controller/connect", fmt.Sprintf(
		`{"op_id": "op-conn-boot", "controller_lease": %q, "expected_generation": 1, "instance_id": %q}`,
		agyWireLease, e.cfg.InstanceID)); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("connect: %d %v", code, resp)
	}

	// (e) Every agy operation refuses typed; no child runs.
	_, _, err = srv.CreateAgySession(ctx, "op-create-awaiting", agyWireLease, agyWireSession)
	var ne *agy.ErrNotEligible
	var missing *agy.ErrProductionEligibilityMissing
	if !errors.As(err, &ne) || !errors.As(err, &missing) {
		t.Fatalf("CreateAgySession while awaiting must refuse typed, got %T: %v", err, err)
	}
	if eps, err := store.AgyCreationUncertaintyEpisodes(ctx, agyWireSession); err != nil || len(eps) != 0 {
		t.Fatalf("a refused birth opens no creation marker, got %+v err=%v", eps, err)
	}
	queueBody := func(turnKey string, tools []string) string {
		ver, err := store.GetSessionVersion(ctx, agyWireSession)
		if err != nil {
			t.Fatalf("version: %v", err)
		}
		body := map[string]any{"instance_id": e.cfg.InstanceID, "op_id": "op-q-" + turnKey, "controller_lease": agyWireLease,
			"expected_version": ver, "turn_key": turnKey, "prompt": "review " + turnKey}
		if tools != nil {
			body["required_tools"] = tools
		}
		raw, _ := json.Marshal(body)
		return string(raw)
	}
	queuePath := "/v1/runs/" + agyWireRunID + "/sessions/" + agyWireSession + "/prompts/queue"
	errCode := func(resp map[string]any) string {
		env, _ := resp["error"].(map[string]any)
		c, _ := env["code"].(string)
		return c
	}
	if code, resp := agyServeJSON(t, srv, "POST", queuePath, queueBody("t-tools", []string{"view_file"})); code != http.StatusServiceUnavailable || errCode(resp) != "agy_not_eligible" {
		t.Fatalf("queue-time required_tools validation refuses typed while awaiting, got %d %v", code, resp)
	}
	// A plain prompt parks (harmless, releasable after the restart); its
	// release is refused typed before anything is committed.
	if code, resp := agyServeJSON(t, srv, "POST", queuePath, queueBody("t-plain", nil)); code != http.StatusOK {
		t.Fatalf("queue a plain prompt: %d %v", code, resp)
	}
	ver, err := store.GetSessionVersion(ctx, agyWireSession)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	relBody := fmt.Sprintf(`{"instance_id": %q, "op_id": "op-rel-awaiting", "controller_lease": %q, "expected_version": %d}`,
		e.cfg.InstanceID, agyWireLease, ver)
	if code, resp := agyServeJSON(t, srv, "POST", "/v1/runs/"+agyWireRunID+"/sessions/"+agyWireSession+"/turns/t-plain/release", relBody); code != http.StatusServiceUnavailable || errCode(resp) != "agy_not_eligible" {
		t.Fatalf("release of an agy turn refuses typed while awaiting, got %d %v", code, resp)
	}
	if details, err := store.GetTurnDetails(ctx, agyWireSession, "t-plain"); err == nil && details != nil && details.DispatchIntent != nil {
		t.Fatalf("a refused release commits no dispatch intent, got %+v", details.DispatchIntent)
	}
	agyNoChildAnywhere(t, e)

	// (b) The first row is recordable on the awaiting server.
	att := e.coveringAttestation(t, "operator-boot")
	id, err := att.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	body, err := json.Marshal(map[string]any{"op_id": "op-att-boot", "actor": "operator-boot", "attestation_id": id, "attestation": att})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/runs/" + agyWireRunID + "/agy/attestations"
	code, recorded := agyServeJSON(t, srv, "POST", path, string(body))
	receipt, _ := recorded["receipt"].(map[string]any)
	if code != http.StatusOK || receipt["payload"] != id {
		t.Fatalf("recording over HTTP on the awaiting server: %d %v", code, recorded)
	}
	if code, replay := agyServeJSON(t, srv, "POST", path, string(body)); code != http.StatusOK || fmt.Sprint(replay) != fmt.Sprint(recorded) {
		t.Fatalf("attestation replay: %d %v, original %v", code, replay, recorded)
	}
	// No hot reload: this instance stays awaiting.
	if srv.AgyStatus().State != AgyAwaitingAttestation {
		t.Fatalf("recording does not hot-reload the adapter, got %+v", srv.AgyStatus())
	}
	if _, _, err := srv.CreateAgySession(ctx, "op-create-awaiting-2", agyWireLease, agyWireSession); !errors.As(err, &ne) {
		t.Fatalf("still awaiting until restart, got %v", err)
	}
	agyNoChildAnywhere(t, e)

	// (c) A restart over the same store constructs the production adapter.
	srv.lock.Release()
	restarted, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, nil)
	if err != nil {
		t.Fatalf("restart after recording: %v", err)
	}
	if _, ok := restarted.adapter.(*agy.AgyAdapter); !ok {
		t.Fatalf("the production agy adapter must be wired after the restart, got %T", restarted.adapter)
	}
	if st := restarted.AgyStatus(); st.State != AgyWired {
		t.Fatalf("want wired after the restart, got %+v", st)
	}
	if !constructionProbeRan(e.scratch) {
		t.Fatal("the restart's construction-scoped plugin list must run")
	}
	code, resp = agyServeJSON(t, restarted, "GET", "/v1/status", "")
	if agyBlock, _ := resp["agy"].(map[string]any); code != http.StatusOK || agyBlock["state"] != AgyWired {
		t.Fatalf("GET /v1/status reports wired after the restart, got %d %v", code, resp)
	}
}

// (d) Only the missing-attestation refusal is a bootstrap state: any
// other construction error still fails the server, before any child.
func TestServiceBootstrap_AgyOtherConstructionErrorsStillFail(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)

	cfg := e.cfg
	cfg.AgyHomeDir = t.TempDir() // not the parent of expected_home
	srv, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), cfg, nil)
	if err == nil {
		t.Fatalf("a wrong AgyHomeDir must fail the server, got status %+v", srv.AgyStatus())
	}
	if isAgyAwaitingAttestation(err) {
		t.Fatalf("a home mismatch is not the awaiting state: %v", err)
	}
	if constructionProbeRan(e.scratch) {
		t.Fatal("no child for a refused configuration")
	}

	// A non-attestation ineligibility is not the awaiting state either.
	if isAgyAwaitingAttestation(&agy.ErrNotEligible{Reason: "the held sealed image digest differs"}) {
		t.Fatal("only an ineligibility caused by the missing attestation is the awaiting state")
	}
}

// A server with no agy configuration reports not_configured and omits
// the status block; recording still needs only the evidence root.
func TestServiceBootstrap_AgyNotConfiguredAndRecordNeedsNoAdapter(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	cfg := e.cfg
	cfg.AgyBinaryPath = ""
	srv, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), cfg, nil)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if st := srv.AgyStatus(); st.State != AgyNotConfigured {
		t.Fatalf("want not_configured, got %+v", st)
	}
	if code, resp := agyServeJSON(t, srv, "GET", "/v1/status", ""); code != http.StatusOK || resp["agy"] != nil {
		t.Fatalf("no agy block without agy configuration, got %d %v", code, resp)
	}
	att := e.coveringAttestation(t, "operator-boot")
	if _, err := srv.RecordAgyProbeAttestation(context.Background(), AgyProbeAttestationRequest{
		OpID: "op-att-noadapter", OperatorToken: cfg.AuthToken, Actor: "operator-boot",
		RunID: agyWireRunID, Attestation: att,
	}); err != nil {
		t.Fatalf("recording depends on the store, credential and evidence root only: %v", err)
	}
}

// The wiring status follows the adapter actually wired: AgyBinaryPath
// with an injected non-agy adapter is not_wired, never "wired".
func TestServiceBootstrap_AgyConfiguredWithNonAgyAdapterIsNotWired(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	srv, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, fake)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if st := srv.AgyStatus(); st.State != AgyNotWired || !strings.Contains(st.Reason, "not the agy adapter") {
		t.Fatalf("a non-agy adapter with AgyBinaryPath is not_wired, got %+v", st)
	}
}
