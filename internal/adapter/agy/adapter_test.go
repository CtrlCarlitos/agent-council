//go:build unix

package agy

// AC-006 adapter contract evidence for the agy adapter (AC-010 spec
// §3.3/§3.4/§3.12, Task 5): provider-free creation through the sealed
// fixture child (init verified, stdin closed WITHOUT any message),
// idempotent/changed/concurrent creation, the uncertain-creation
// tombstone, local-only resume, the fixture-scoped Probe, construction
// guards, and the launch matrix.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestAgyAdapter_CreateSessionBindsInitConversationID(t *testing.T) {
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "` + testNativeID + `"}`)

	binding, err := h.adapter.CreateSession(context.Background(), h.createRequest())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if binding.NativeSessionID != testNativeID {
		t.Fatalf("binding must carry init.conversation_id, got %q", binding.NativeSessionID)
	}
	args := h.fixtureFile(".agy-fixture-args")
	if len(args) != 1 {
		t.Fatalf("creation launches exactly one child, got %v", args)
	}
	if strings.Contains(args[0], "--conversation") {
		t.Fatalf("creation must not pass --conversation: %q", args[0])
	}
	if input := h.fixtureFile(".agy-fixture-input"); len(input) != 0 {
		t.Fatalf("creation closes stdin WITHOUT any message, child read %v", input)
	}
	// The adapter does not persist (§3.3): the service binds.
	if b, _ := h.store.GetAgySessionBinding(context.Background(), testSessionID); b != nil {
		t.Fatalf("the adapter must not persist the binding, found %+v", b)
	}
}

func TestAgyAdapter_CreateSessionIdempotentFromPersistedBinding(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)

	binding, err := h.adapter.CreateSession(context.Background(), h.createRequest())
	if err != nil {
		t.Fatalf("idempotent CreateSession: %v", err)
	}
	if binding.NativeSessionID != testNativeID {
		t.Fatalf("persisted binding must be returned, got %q", binding.NativeSessionID)
	}
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("an already-bound session needs no child, launches=%d", n)
	}
}

func TestAgyAdapter_CreateSessionChangedConfigRejected(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)

	req := h.createRequest()
	req.Config.Model = "some-other-model"
	_, err := h.adapter.CreateSession(context.Background(), req)
	var mismatch *ErrSessionConfigMismatch
	if !errors.As(err, &mismatch) || mismatch.Field != "model" {
		t.Fatalf("changed model must fail closed with ErrSessionConfigMismatch(model), got %v", err)
	}

	// Unbound session with a workspace that is not the AC-005 allocation:
	// refused before any child starts.
	h2 := newAgyHarness(t)
	req2 := h2.createRequest()
	req2.Config.WorkspaceRoot = t.TempDir()
	_, err = h2.adapter.CreateSession(context.Background(), req2)
	if !errors.As(err, &mismatch) || mismatch.Field != "workspace" {
		t.Fatalf("a non-allocation workspace must fail closed, got %v", err)
	}
	if n := h2.exec.starts.Load(); n != 0 {
		t.Fatalf("a mismatched creation must not start a child, launches=%d", n)
	}
}

func TestAgyAdapter_CreateSessionConcurrentDuplicatesShareOneReservation(t *testing.T) {
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "`+testNativeID+`"}`, `{"slow_init_ms": 300}`)

	const n = 6
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, err := h.adapter.CreateSession(context.Background(), h.createRequest())
			ids[i], errs[i] = b.NativeSessionID, err
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || ids[i] != testNativeID {
			t.Fatalf("caller %d: id=%q err=%v", i, ids[i], errs[i])
		}
	}
	if got := h.exec.starts.Load(); got != 1 {
		t.Fatalf("concurrent duplicates share ONE creation reservation, launches=%d", got)
	}
}

func TestAgyAdapter_CreateSessionUncertainTombstone(t *testing.T) {
	h := newAgyHarness(t)
	withShortTimers(t, 200*time.Millisecond, cancelGrace)
	h.scenario(`{"conversation_id": "`+testNativeID+`"}`, `{"slow_init_ms": 3000}`)

	_, err := h.adapter.CreateSession(context.Background(), h.createRequest())
	if _, ok := isUncertainCreation(err); !ok {
		t.Fatalf("init not observed within the bound must be ErrSessionCreationUncertain, got %v", err)
	}
	// Tombstone: no automatic recreation, no new child.
	before := h.exec.starts.Load()
	_, err = h.adapter.CreateSession(context.Background(), h.createRequest())
	if _, ok := isUncertainCreation(err); !ok {
		t.Fatalf("tombstoned session must keep refusing, got %v", err)
	}
	if h.exec.starts.Load() != before {
		t.Fatal("a tombstoned creation must not start a child")
	}

	// Explicit resolution clears the in-process tombstone.
	if !h.adapter.ResolveCreationUncertainty(testSessionID) {
		t.Fatal("ResolveCreationUncertainty must report a cleared tombstone")
	}
	h.scenario(`{"conversation_id": "` + testNativeID + `"}`)
	if _, err := h.adapter.CreateSession(context.Background(), h.createRequest()); err != nil {
		t.Fatalf("resolved session must be creatable again: %v", err)
	}
}

func TestAgyAdapter_CreateSessionDurableUncertaintyBlocksAcrossRestart(t *testing.T) {
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "` + testNativeID + `"}`)
	if _, _, err := h.store.RecordAgyCreationUncertain(context.Background(), storage.AgyCreationUncertainty{
		RunID: testRunID, SessionID: testSessionID, Reason: "lost init",
		RecordedBy: "agy-adapter", CauseOpID: "op-create-crashed",
	}); err != nil {
		t.Fatalf("record episode: %v", err)
	}
	h.reopen()
	_, err := h.adapter.CreateSession(context.Background(), h.createRequest())
	if _, ok := isUncertainCreation(err); !ok {
		t.Fatalf("an open durable episode must block creation after restart, got %v", err)
	}
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("no child may start while the episode is open, launches=%d", n)
	}
}

// Resolve-then-recreate: a durable episode blocks creation across a
// restart; once a controller resolves THAT episode durably, the same
// (restarted) adapter creates the session with exactly one child.
func TestAgyAdapter_ResolveThenRecreate(t *testing.T) {
	h := newAgyHarness(t)
	ctx := context.Background()
	h.scenario(`{"conversation_id": "` + testNativeID + `"}`)
	ep, _, err := h.store.RecordAgyCreationUncertain(ctx, storage.AgyCreationUncertainty{
		RunID: testRunID, SessionID: testSessionID, Reason: "lost init",
		RecordedBy: "agy-adapter", CauseOpID: "op-create-crashed",
	})
	if err != nil {
		t.Fatalf("record episode: %v", err)
	}
	h.reopen()
	if _, err := h.adapter.CreateSession(ctx, h.createRequest()); err == nil {
		t.Fatal("the open episode blocks creation")
	}
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("no child while the episode is open, launches=%d", n)
	}
	// Clearing the in-process tombstone alone never unblocks: the
	// durable episode is still open.
	h.adapter.ResolveCreationUncertainty(testSessionID)
	if _, err := h.adapter.CreateSession(ctx, h.createRequest()); err == nil || h.exec.starts.Load() != 0 {
		t.Fatalf("an in-memory clear without the durable resolution still blocks, err=%v launches=%d", err, h.exec.starts.Load())
	}
	if _, err := h.store.AdoptController(ctx, "op-adopt-agy", testRunID, "agy", "controller-ref-agy", testLease, nil, testLease); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if _, err := h.store.ResolveAgyCreationUncertainty(ctx, "op-resolve-agy", testLease, 1, string(testSessionID), ep,
		storage.AgyUncertaintyVerifiedAbsent, "operator verified no conversation file exists"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The service clears the tombstone only after the durable resolution
	// (Server.ResolveAgySessionCreationUncertainty); mirror that order.
	h.adapter.ResolveCreationUncertainty(testSessionID)
	binding, err := h.adapter.CreateSession(ctx, h.createRequest())
	if err != nil || binding.NativeSessionID != testNativeID {
		t.Fatalf("after the durable resolution creation proceeds, got %+v err=%v", binding, err)
	}
	if n := h.exec.starts.Load(); n != 1 {
		t.Fatalf("exactly one creation child after resolution, launches=%d", n)
	}
}

func TestAgyAdapter_CreateSessionNonUUIDConversationUncertain(t *testing.T) {
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "not-a-uuid"}`)
	_, err := h.adapter.CreateSession(context.Background(), h.createRequest())
	unc, ok := isUncertainCreation(err)
	if !ok {
		t.Fatalf("a non-UUIDv4 creation id must be ErrSessionCreationUncertain, got %v", err)
	}
	if unc.PartialNativeID != "not-a-uuid" {
		t.Fatalf("the orphan id must be carried as a diagnostic, got %q", unc.PartialNativeID)
	}
	var drift *ErrConversationDrift
	if !errors.As(err, &drift) {
		t.Fatalf("the uncertainty must wrap ErrConversationDrift, got %v", err)
	}
}

func TestAgyAdapter_CreateSessionProfileDriftRejected(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		check func(t *testing.T, err error)
	}{
		{"permission_mode", `{"permission_mode": "strict"}`, func(t *testing.T, err error) {
			var d *ErrProfileDrift
			if !errors.As(err, &d) || d.Field != "permission_mode" {
				t.Fatalf("want ErrProfileDrift(permission_mode), got %v", err)
			}
		}},
		{"tools_extra", `{"tools": ["view_file", "invoke_subagent"]}`, func(t *testing.T, err error) {
			var d *ErrToolInventoryDrift
			if !errors.As(err, &d) {
				t.Fatalf("want ErrToolInventoryDrift, got %v", err)
			}
		}},
		{"tools_empty", `{"tools": []}`, func(t *testing.T, err error) {
			var d *ErrToolInventoryDrift
			if !errors.As(err, &d) {
				t.Fatalf("an empty init.tools is drift, never success: got %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgyHarness(t)
			h.scenario(`{"conversation_id": "`+testNativeID+`"}`, tc.line)
			_, err := h.adapter.CreateSession(context.Background(), h.createRequest())
			tc.check(t, err)
			if _, unc := isUncertainCreation(err); unc {
				t.Fatalf("config drift is a definitive rejection, not uncertainty: %v", err)
			}
		})
	}
}

func TestAgyAdapter_ResumeSessionLocalChecks(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	binding := adapter.SessionBinding{
		SessionID: testSessionID, Contributor: "agy", NativeSessionID: testNativeID,
		Config: adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	}
	if err := h.adapter.ResumeSession(context.Background(), binding); err != nil {
		t.Fatalf("unmaterialized binding resumes locally: %v", err)
	}
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("resume is local-only, launches=%d", n)
	}

	wrong := binding
	wrong.NativeSessionID = otherNativeID
	var mismatch *ErrSessionConfigMismatch
	if err := h.adapter.ResumeSession(context.Background(), wrong); !errors.As(err, &mismatch) {
		t.Fatalf("native id mismatch must fail closed, got %v", err)
	}

	// Materialized: the conversation file must exist at the recorded
	// path with mode 0600 and the recorded identity.
	convDir := filepath.Join(h.home, "antigravity-cli", "conversations")
	if err := os.MkdirAll(convDir, 0o700); err != nil {
		t.Fatal(err)
	}
	convPath := filepath.Join(convDir, testNativeID+".db")
	if err := os.WriteFile(convPath, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := conversationFileIdentity(convPath)
	if err != nil {
		t.Fatalf("file identity: %v", err)
	}
	if err := h.store.MarkAgySessionMaterialized(context.Background(), testSessionID, convPath, identity); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.ResumeSession(context.Background(), binding); err != nil {
		t.Fatalf("materialized binding with a 0600 file resumes: %v", err)
	}
	if err := os.Chmod(convPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.ResumeSession(context.Background(), binding); err == nil {
		t.Fatal("a conversation file that is not 0600 must fail closed")
	}
	_ = os.Chmod(convPath, 0o600)
	// Replaced file (new inode: the replacement is created while the
	// original still exists, so its inode cannot be reused) ⇒ drift.
	if err := os.WriteFile(convPath+".tmp", []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(convPath+".tmp", convPath); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.ResumeSession(context.Background(), binding); err == nil {
		t.Fatal("a replaced conversation file (identity drift) must fail closed")
	}
	_ = os.Remove(convPath)
	if err := h.adapter.ResumeSession(context.Background(), binding); err == nil {
		t.Fatal("a missing conversation file must fail closed")
	}
}

func TestAgyAdapter_ProbeReportsFrozenIdentityWithoutLaunch(t *testing.T) {
	h := newAgyHarness(t)
	report, err := h.adapter.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !report.HarnessVersion.Available || report.HarnessVersion.Value != "1.2.9" {
		t.Fatalf("probe must report the frozen CLI version, got %+v", report.HarnessVersion)
	}
	if !report.ModelInventory.Available || len(report.ModelInventory.Value) != 1 || report.ModelInventory.Value[0] != h.model {
		t.Fatalf("probe must report the frozen model, got %+v", report.ModelInventory)
	}
	if report.Capabilities.MidTurnCancellation != adapter.CapabilitySupported ||
		report.Capabilities.StreamingObservation != adapter.CapabilitySupported {
		t.Fatalf("capabilities: %+v", report.Capabilities)
	}
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("Probe must not launch anything (auth gate is Task 6), launches=%d", n)
	}
}

func TestAgyAdapter_ProductionConstructorRejectsFixtureOption(t *testing.T) {
	h := newAgyHarness(t)
	att := func() (string, bool) { return "cprot-v2:sha256:" + strings.Repeat("ab", 32), true }
	_, err := NewAgyAdapter(h.store, h.exec, h.source, h.wm, h.policy, h.profileDigest, h.fx.SealedImage,
		fnIdentity{fn: defaultAttempt}, h.required, att, FixtureOption(FixtureMode{}))
	var prohibited *ErrFixtureModeProhibited
	if !errors.As(err, &prohibited) {
		t.Fatalf("the production constructor must refuse the fixture option, got %v", err)
	}
	_, err = NewAgyAdapter(h.store, h.exec, h.source, h.wm, h.policy, h.profileDigest, h.fx.SealedImage,
		fnIdentity{fn: defaultAttempt}, h.required, nil)
	if err == nil {
		t.Fatal("the production constructor requires the attestation lookup")
	}
}

func TestAgyAdapter_ProductionEligibilityMissingPreChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production construction is Linux-only (spec §3.12)")
	}
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "` + testNativeID + `"}`)
	att := func() (string, bool) { return "", false }
	prod, err := NewAgyAdapter(h.store, h.exec, h.source, h.wm, h.policy, h.profileDigest, h.fx.SealedImage,
		fnIdentity{fn: defaultAttempt}, h.required, att)
	if err != nil {
		t.Fatalf("NewAgyAdapter: %v", err)
	}
	_, err = prod.CreateSession(context.Background(), h.createRequest())
	var missing *ErrProductionEligibilityMissing
	if !errors.As(err, &missing) {
		t.Fatalf("no attestation must be ErrProductionEligibilityMissing, got %v", err)
	}
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("eligibility is checked BEFORE any child starts, launches=%d", n)
	}
}

func TestAgyAdapter_LaunchMatrix(t *testing.T) {
	t.Run("strict_unrestricted_refused", func(t *testing.T) {
		h := newAgyHarnessProfile(t, func(p *storage.CanonicalProfile) {
			p.IsolationStrictness = "strict"
			p.NetworkMode = "unrestricted"
		})
		h.scenario(`{"conversation_id": "` + testNativeID + `"}`)
		_, err := h.adapter.CreateSession(context.Background(), h.createRequest())
		var refused *ErrLaunchMatrixRefused
		if !errors.As(err, &refused) {
			t.Fatalf("strict+unrestricted must be refused fail-closed, got %v", err)
		}
		if n := h.exec.starts.Load(); n != 0 {
			t.Fatalf("a refused matrix row starts no process, launches=%d", n)
		}
	})
	t.Run("strict_allowlist_refused", func(t *testing.T) {
		h := newAgyHarnessProfile(t, func(p *storage.CanonicalProfile) {
			p.IsolationStrictness = "strict"
			p.NetworkMode = "allowlist"
			p.NetworkAllowlist = []string{"example.com"}
		})
		_, err := h.adapter.CreateSession(context.Background(), h.createRequest())
		var refused *ErrLaunchMatrixRefused
		if !errors.As(err, &refused) {
			t.Fatalf("strict+allowlist must be refused fail-closed, got %v", err)
		}
	})
	t.Run("permissive_none_fixture_allowed", func(t *testing.T) {
		h := newAgyHarnessProfile(t, func(p *storage.CanonicalProfile) {
			p.NetworkMode = "none"
		})
		h.scenario(`{"conversation_id": "` + testNativeID + `"}`)
		if _, err := h.adapter.CreateSession(context.Background(), h.createRequest()); err != nil {
			t.Fatalf("permissive_dev+none is allowed under FixtureMode: %v", err)
		}
	})
	t.Run("permissive_none_production_refused", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("production construction is Linux-only (spec §3.12)")
		}
		h := newAgyHarnessProfile(t, func(p *storage.CanonicalProfile) {
			p.NetworkMode = "none"
		})
		att := func() (string, bool) { return "cprot-v2:sha256:" + strings.Repeat("ab", 32), true }
		prod, err := NewAgyAdapter(h.store, h.exec, h.source, h.wm, h.policy, h.profileDigest, h.fx.SealedImage,
			fnIdentity{fn: defaultAttempt}, h.required, att)
		if err != nil {
			t.Fatalf("NewAgyAdapter: %v", err)
		}
		_, err = prod.CreateSession(context.Background(), h.createRequest())
		var refused *ErrLaunchMatrixRefused
		if !errors.As(err, &refused) {
			t.Fatalf("network none is fixture/diagnostic only, never production: got %v", err)
		}
		if n := h.exec.starts.Load(); n != 0 {
			t.Fatalf("refused row starts no process, launches=%d", n)
		}
	})
}
