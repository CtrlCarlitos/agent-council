//go:build unix

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Task 6 regressions: operator-administered grant endpoints, the restricted
// controller bridge (positive + escalation through the same interface), and
// the privilege/redaction boundaries.

type adminFixture struct {
	srv   *Server
	store *storage.Store
	lock  *ServiceLock
	dir   string
	token string
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	dir := testStateDir(t)
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	srv, err := NewServer(store, lock, ServerConfig{StateDir: dir, InstanceID: "inst-adm", AuthToken: "tok-adm"})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-adm", "run-adm", "b", "s", "p", "boot-adm"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return &adminFixture{srv: srv, store: store, lock: lock, dir: dir, token: "tok-adm"}
}

func (f *adminFixture) close(t *testing.T) {
	t.Helper()
	_ = f.srv.Close()
	_ = f.store.Close()
	_ = f.lock.Release()
}

func (f *adminFixture) post(t *testing.T, path, auth, body string) (int, string) {
	t.Helper()
	ctx := context.Background()
	c := newTestClient(f.srv.SocketPath())
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	_, _ = fmt.Fprint(&sb, readBody(t, resp))
	return resp.StatusCode, sb.String()
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var out strings.Builder
	dec := json.NewDecoder(resp.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err == nil {
		var indented bytes.Buffer
		if json.Indent(&indented, raw, "", " ") == nil {
			out.WriteString(indented.String())
		}
	}
	return out.String()
}

// Adoption over HTTP with the bootstrap credential issues the secret once;
// the redacted record exposes no secret.
func TestAC004_HTTPAdoptHandoffRecoverFlow(t *testing.T) {
	f := newAdminFixture(t)
	defer f.close(t)

	code, body := f.post(t, "/v1/runs/run-adm/controller/adopt", f.token,
		`{"op_id":"op-adopt-http","harness":"opencode","controller_ref":"conversation-A","bootstrap_lease":"boot-adm"}`)
	if code != http.StatusOK {
		t.Fatalf("adopt: %d %s", code, body)
	}
	var grant struct {
		Generation  uint64 `json:"generation"`
		LeaseSecret string `json:"lease_secret"`
	}
	if err := json.Unmarshal([]byte(body), &grant); err != nil {
		t.Fatalf("decode adopt: %v", err)
	}
	if grant.Generation != 1 || grant.LeaseSecret == "" || grant.LeaseSecret == "boot-adm" {
		t.Fatalf("adoption must issue a fresh secret for generation 1: %+v", grant)
	}

	// Handoff requires the current lease and expected generation; a stale
	// generation does not touch the controller.
	code, body = f.post(t, "/v1/runs/run-adm/controller/handoff", f.token,
		fmt.Sprintf(`{"op_id":"op-h-of","current_lease":%q,"expected_generation":7,"harness":"claude","controller_ref":"B"}`, grant.LeaseSecret))
	if code != http.StatusConflict || !strings.Contains(body, "generation_mismatch") {
		t.Fatalf("stale handoff generation must conflict: %d %s", code, body)
	}
	rec, _ := f.store.GetControllerRecord(context.Background(), "run-adm")
	if rec.Generation != 1 {
		t.Fatalf("controller untouched expected, got %+v", rec)
	}

	code, body = f.post(t, "/v1/runs/run-adm/controller/handoff", f.token,
		fmt.Sprintf(`{"op_id":"op-h-of","current_lease":%q,"expected_generation":1,"harness":"claude","controller_ref":"B"}`, grant.LeaseSecret))
	if code != http.StatusOK {
		t.Fatalf("handoff: %d %s", code, body)
	}
	var grantB struct {
		Generation  uint64 `json:"generation"`
		LeaseSecret string `json:"lease_secret"`
	}
	_ = json.Unmarshal([]byte(body), &grantB)
	if grantB.Generation != 2 || grantB.LeaseSecret == "" || grantB.LeaseSecret == grant.LeaseSecret {
		t.Fatalf("handoff must issue generation 2's secret: %+v", grantB)
	}

	// Credential recovery of the still-active generation 2 issuance.
	code, body = f.post(t, "/v1/runs/run-adm/controller/credential/recover", f.token,
		`{"op_id":"op-recover-http","source_op_id":"op-h-of","expected_generation":2}`)
	if code != http.StatusOK {
		t.Fatalf("recover: %d %s", code, body)
	}
	var recovered struct {
		LeaseSecret string `json:"lease_secret"`
	}
	_ = json.Unmarshal([]byte(body), &recovered)
	if recovered.LeaseSecret != grantB.LeaseSecret {
		t.Fatalf("recovery must return the same issuance, got %q", recovered.LeaseSecret)
	}

	// Recovery of the superseded generation 1 issuance is unavailable.
	code, body = f.post(t, "/v1/runs/run-adm/controller/credential/recover", f.token,
		`{"op_id":"op-recover-http-2","source_op_id":"op-adopt-http","expected_generation":1}`)
	if code != http.StatusConflict || !strings.Contains(body, "recovery_unavailable") {
		t.Fatalf("superseded recovery must be unavailable: %d %s", code, body)
	}

	// Operator recovery revocation requires reason + expected generation; a
	// stale generation leaves the controller untouched.
	code, body = f.post(t, "/v1/runs/run-adm/controller/revoke", f.token,
		`{"op_id":"op-rv-1","operator_recovery":true,"reason":"lost lease","expected_generation":1}`)
	if code != http.StatusConflict || !strings.Contains(body, "generation_mismatch") {
		t.Fatalf("stale recovery revoke must conflict: %d %s", code, body)
	}
	code, body = f.post(t, "/v1/runs/run-adm/controller/revoke", f.token,
		`{"op_id":"op-rv-2","operator_recovery":true,"reason":"lost lease","expected_generation":2}`)
	if code != http.StatusOK {
		t.Fatalf("recovery revoke: %d %s", code, body)
	}
	rec, _ = f.store.GetControllerRecord(context.Background(), "run-adm")
	if rec.Adopted || rec.Status != "revoked" {
		t.Fatalf("expected revoked unadopted run: %+v", rec)
	}
}

// Lease-as-bearer never authenticates transport; contributor clients (no
// credential) are rejected before any mutation; recovery flags are intent,
// not authority.
func TestAC004_AdminPrivilegeBoundary(t *testing.T) {
	f := newAdminFixture(t)
	defer f.close(t)

	adminPaths := []string{
		"/v1/runs/run-adm/controller/adopt",
		"/v1/runs/run-adm/controller/handoff",
		"/v1/runs/run-adm/controller/revoke",
		"/v1/runs/run-adm/controller/credential/recover",
	}
	body := `{"op_id":"op-x","harness":"claude","controller_ref":"x","operator_recovery":true,"reason":"r","expected_generation":0,"bootstrap_lease":"boot-adm","current_lease":"boot-adm","source_op_id":"s"}`
	for _, p := range adminPaths {
		// A controller lease presented as the transport bearer fails
		// authentication: it is not the operator capability.
		code, _ := f.post(t, p, "boot-adm", body)
		if code != http.StatusUnauthorized {
			t.Fatalf("lease-as-bearer on %s must fail transport authentication, got %d", p, code)
		}
		// No credential at all likewise.
		code, _ = f.post(t, p, "", body)
		if code != http.StatusUnauthorized {
			t.Fatalf("no-credential on %s must fail authentication, got %d", p, code)
		}
	}

	// Nothing was mutated by the denied attempts.
	rec, err := f.store.GetControllerRecord(context.Background(), "run-adm")
	if err != nil || rec.Adopted {
		t.Fatalf("denied attempts must not create authority: %+v err=%v", rec, err)
	}
}

// No lease secret appears in readiness, status, the controller record, or
// ordinary error envelopes; inspection does not reconnect a controller.
func TestAC004_AdminRedactionAndNoReconnectOnInspection(t *testing.T) {
	f := newAdminFixture(t)
	defer f.close(t)

	code, body := f.post(t, "/v1/runs/run-adm/controller/adopt", f.token,
		`{"op_id":"op-adopt-red","harness":"agy","controller_ref":"R","bootstrap_lease":"boot-adm"}`)
	if code != http.StatusOK {
		t.Fatalf("adopt: %d %s", code, body)
	}
	var grant struct {
		LeaseSecret string `json:"lease_secret"`
	}
	_ = json.Unmarshal([]byte(body), &grant)
	secret := grant.LeaseSecret

	c := newTestClient(f.srv.SocketPath())
	ctx := context.Background()

	get := func(path string) string {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost"+path, nil)
		req.Header.Set("Authorization", "Bearer "+f.token)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		return readBody(t, resp)
	}
	for _, path := range []string{"/v1/readiness", "/v1/status", "/v1/runs/run-adm/controller"} {
		if body := get(path); strings.Contains(body, secret) || strings.Contains(body, "boot-adm") {
			t.Fatalf("%s disclosed a lease secret: %s", path, body)
		}
	}

	// Inspection did not reconnect: no in-instance attachment exists.
	if f.srv.Coordinator().ControllerAttached("run-adm", 1) {
		t.Fatal("inspection must not mark a controller attached")
	}
}
