//go:build unix

package claude

// Task 5 adapter contract evidence (POSIX — real executor + fixture
// child): §3.3 creation reservation (shared identity, typed mismatch
// failures, no adapter persistence), §3.4 resume local inspection,
// §3.5 durable blocking + disposition unblocking, first-vs-resume
// identity selection, terminal mapping (completed AND failed),
// ambiguity slot semantics, cancel/observe semantics, and four-state
// reconcile over durable evidence.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const (
	primarySession = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	primaryModel   = "claude-haiku-4-5-20251001"
)

type adapterHarness struct {
	store      *storage.Store
	wm         *workspace.WorkspaceManager
	adapter    *ClaudeAdapter
	wsRoot     string
	configBase string
	template   string
	sessionID  adapter.SessionID
}

func newAdapterHarness(t *testing.T) *adapterHarness {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	configBase := filepath.Join(dir, "claude-config")
	for _, d := range []string{stateDir, wsBase, configBase} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	binDir := compileClaudeFixture(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	wm, err := workspace.NewWorkspaceManager(stateDir, wsBase)
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}

	// Evidence root and universe setup (before the profile so the digest is available).
	evidenceRoot := filepath.Join(dir, "evidence")
	os.MkdirAll(filepath.Join(evidenceRoot, "docs", "superpowers", "evidence"), 0o700)
	universeDoc := `{"claude_code_version":"2.1.278","tools":["Read","Glob","Grep","Bash","Write","WebSearch"]}`
	universePath := filepath.Join(evidenceRoot, "docs", "superpowers", "evidence", "ac008-native-tool-universe-2.1.278.json")
	os.WriteFile(universePath, []byte(universeDoc), 0o600)

	const lease = "lease-adapter"
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-adapter", ControllerLease: lease, RunID: "run-adapter",
		Brief: "adapter test", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      claudeProfile("sha256:"+sha256Sum(universeDoc), primaryModel),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-adapter", lease, storage.SessionRecord{
		ID: primarySession, RunID: "run-adapter", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	executor := execpolicy.New()
	launchSrc := NewClaudeTurnLaunchSource(store, wm, configBase, evidenceRoot)

	ws, err := wm.AllocateWorkspace("run-adapter", primarySession, "none", "example/repo",
		"0123456789012345678901234567890123456789")
	if err != nil {
		t.Fatalf("allocate workspace: %v", err)
	}

	// The frozen config template: CreateSession materializes the
	// per-session root from it.
	template := filepath.Join(dir, "template")
	os.MkdirAll(filepath.Join(template, "skills"), 0o700)
	os.WriteFile(filepath.Join(template, "settings.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(template, "skills", "s.md"), []byte("skill"), 0o600)

	adp := NewClaudeAdapter(store, wm, executor, launchSrc, testIdentitySource{}, configBase, template)

	return &adapterHarness{
		store: store, wm: wm, adapter: adp,
		wsRoot: ws.Root, configBase: configBase, template: template,
		sessionID: primarySession,
	}
}

func claudeProfile(universeDigest, model string) storage.CanonicalProfile {
	p := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v2",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"claude", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"claude": {Model: model, NativeAuthMode: "inherited_host_keychain"},
		},
	}
	p.ToolkitManifest = &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
		ProbedCLIVersion:       "2.1.278",
		UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
		UniverseEvidenceDigest: universeDigest,
		ApprovedTools:          []string{"Read", "Glob", "Grep"},
		DeniedComplement:       []string{"Bash", "Write", "WebSearch"},
		TurnsBound:             8,
	}}
	return p
}

// createSession drives the production CreateSession path and then
// performs the §3.3 persistence the service/storage layer owns.
func (h *adapterHarness) createSession(t *testing.T, id string) adapter.SessionBinding {
	t.Helper()
	return h.createSessionWithConfig(t, id, primaryModel, h.workspaceRoot(t, id))
}

func (h *adapterHarness) createSessionWithConfig(t *testing.T, id, model, workspaceRoot string) adapter.SessionBinding {
	t.Helper()
	binding, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID(id),
		Contributor: "claude",
		Config: adapter.SessionConfig{
			WorkspaceRoot: workspaceRoot,
			Model:         model,
		},
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	h.persistBinding(t, id, binding.NativeSessionID, model, workspaceRoot)
	return binding
}

// persistBinding is the §3.3 service/storage persistence seam: the
// adapter returned the binding, the storage layer records it.
func (h *adapterHarness) persistBinding(t *testing.T, id, nativeID, model, workspaceRoot string) {
	t.Helper()
	digest, err := TemplateDigest(h.template)
	if err != nil {
		t.Fatalf("template digest: %v", err)
	}
	runID, err := h.store.GetSessionRunID(context.Background(), id)
	if err != nil {
		t.Fatalf("run lookup for %s: %v", id, err)
	}
	if err := h.store.InsertClaudeSessionBinding(context.Background(), storage.ClaudeSessionBinding{
		SessionID:      id,
		NativeID:       nativeID,
		Model:          model,
		Workspace:      workspaceRoot,
		ConfigRoot:     ConfigRootPath(h.configBase, runID, id),
		TemplateDigest: digest,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("persist binding for %s: %v", id, err)
	}
}

// simulateDisposition records the controller's uncertainty disposition
// directly (Task 6 wraps this transition with journal authority); the
// adapter must observe the durable state change and unblock.
func (h *adapterHarness) simulateDisposition(t *testing.T, attemptID string) {
	t.Helper()
	_, err := h.store.DB().ExecContext(context.Background(), `
UPDATE claude_turn_attempts SET uncertainty_disposition = 'abandoned',
	disposition_actor = 'controller-test', disposition_generation = 1,
	disposition_op_id = 'op-disposition-test', disposition_at = ?
WHERE attempt_id = ?`, time.Now().UTC().Format(time.RFC3339), attemptID)
	if err != nil {
		t.Fatalf("simulate disposition: %v", err)
	}
}

func (h *adapterHarness) workspaceRoot(t *testing.T, sessionID string) string {
	t.Helper()
	return h.workspaceRootFor(t, "run-adapter", sessionID)
}

func (h *adapterHarness) workspaceRootFor(t *testing.T, runID, sessionID string) string {
	t.Helper()
	if paths, ok := h.wm.GetPaths(runID, sessionID); ok {
		return paths.Root
	}
	paths, err := h.wm.AllocateWorkspace(runID, sessionID, "none", "example/repo",
		"0123456789012345678901234567890123456789")
	if err != nil {
		t.Fatalf("allocate workspace for %s: %v", sessionID, err)
	}
	return paths.Root
}

// waitTerminal polls Collect until the turn leaves pending.
func waitTerminal(t *testing.T, h *adapterHarness, ref adapter.TurnRef) adapter.TurnResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		result, err := h.adapter.Collect(context.Background(), ref)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if result.ResultStatus != adapter.ResultPending {
			return result
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s did not reach a terminal result within 5s", ref.TurnKey)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// drainStream consumes every event until the stream closes.
func drainStream(t *testing.T, stream adapter.Stream) []adapter.Event {
	t.Helper()
	var events []adapter.Event
	for ev := range stream.Events() {
		events = append(events, ev)
	}
	return events
}

// fixtureStream builds a full fixture stream replay (hook + init with
// the exact runtime session id and cwd, then the custom lines).
func fixtureStream(wsRoot, nativeID, model string, extra ...string) string {
	lines := []string{
		`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup"}`,
		fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"cwd":%q,"claude_code_version":"2.1.278","model":%q,"permissionMode":"default","tools":["Read","Glob","Grep"],"skills":[],"plugins":[]}`,
			nativeID, wsRoot, model),
	}
	lines = append(lines, extra...)
	return strings.Join(lines, "\n") + "\n"
}

// writeKnob places a fixture knob file in the session's workspace (the
// child's CWD).
func writeKnob(t *testing.T, wsRoot, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wsRoot, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write knob %s: %v", name, err)
	}
}

// fixtureArgLines returns the fixture's argv log, one invocation per
// entry, fields joined by 0x1f.
func fixtureArgLines(t *testing.T, wsRoot string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(wsRoot, ".claude-fixture-args"))
	if err != nil {
		t.Fatalf("read fixture args log: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, strings.Split(line, "\x1f"))
	}
	return out
}

func argInvoked(invocation []string, flag string) bool {
	for _, a := range invocation {
		if a == flag {
			return true
		}
	}
	return false
}

func sha256Sum(data string) string {
	sum := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", sum)
}

type testIdentitySource struct{}

func (testIdentitySource) AttemptFor(_ context.Context, ref adapter.TurnRef) (string, bool) {
	return "att_" + ref.TurnKey, true
}

func unusedIdentityFunc(ctx context.Context, ref adapter.TurnRef) (string, bool) {
	return "att_" + ref.TurnKey, true
}

// ── §3.3 CreateSession reservation ──────────────────────────────────────

// CreateSession reserves one UUIDv4 native identity and materializes
// the config root but does NOT persist; duplicates share the reserved
// identity concurrently; mismatched callers fail closed with typed
// errors.
func TestClaudeAdapter_CreateSessionReservation(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	binding, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   h.sessionID,
		Contributor: "claude",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: primaryModel},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if binding.NativeSessionID == string(h.sessionID) {
		t.Fatal("native id must be freshly generated, not the logical session id")
	}
	if !isValidUUIDv4(binding.NativeSessionID) {
		t.Fatalf("native id must be UUIDv4, got %q", binding.NativeSessionID)
	}

	// §3.3: the adapter does not persist.
	if stored, _ := h.store.GetClaudeSessionBinding(ctx, string(h.sessionID)); stored != nil {
		t.Fatal("CreateSession must not persist; the service/storage layer owns persistence")
	}

	wantRoot := ConfigRootPath(h.configBase, "run-adapter", string(h.sessionID))
	if _, err := os.Stat(filepath.Join(wantRoot, "settings.json")); err != nil {
		t.Fatalf("materialized config root missing settings.json: %v", err)
	}

	// Duplicate request before persistence: shared reserved identity.
	again, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   h.sessionID,
		Contributor: "claude",
		Config: adapter.SessionConfig{
			WorkspaceRoot: h.wsRoot,
			Model:         primaryModel,
		},
	})
	if err != nil {
		t.Fatalf("duplicate create: %v", err)
	}
	if again.NativeSessionID != binding.NativeSessionID {
		t.Fatalf("duplicate create returned %q, want the reserved %q", again.NativeSessionID, binding.NativeSessionID)
	}

	// Concurrent duplicates: one native identity, shared result.
	const n = 4
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			b, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
				SessionID:   h.sessionID,
				Contributor: "claude",
				Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: primaryModel},
			})
			if err == nil {
				ids[slot] = b.NativeSessionID
			}
			errs[slot] = err
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent create %d: %v", i, errs[i])
		}
		if ids[i] != binding.NativeSessionID {
			t.Fatalf("concurrent create %d returned %q, want shared %q", i, ids[i], binding.NativeSessionID)
		}
	}

	// Mismatched callers fail closed with typed errors.
	mismatch := func(cfg adapter.SessionConfig, contributor council.Contributor, field string) {
		t.Helper()
		_, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
			SessionID: h.sessionID, Contributor: contributor, Config: cfg,
		})
		var typed *ErrSessionConfigMismatch
		if !asConfigMismatch(err, &typed) {
			t.Fatalf("mismatched %s must fail with ErrSessionConfigMismatch, got %v", field, err)
		}
		if typed.Field != field {
			t.Fatalf("mismatch reported field %q, want %q", typed.Field, field)
		}
	}
	mismatch(adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: "claude-opus-4-1-20250805"}, "claude", "model")
	mismatch(adapter.SessionConfig{WorkspaceRoot: h.wsRoot + "-other", Model: primaryModel}, "claude", "workspace")
	mismatch(adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: primaryModel}, "opencode", "contributor")

	// Non-claude session records are refused outright.
	if _, err := h.store.CreateSession(ctx, "op-sess-other", "lease-adapter", storage.SessionRecord{
		ID: "sess-other", RunID: "run-adapter", Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create other session: %v", err)
	}
	if _, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   "sess-other",
		Contributor: "opencode",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: primaryModel},
	}); err == nil {
		t.Fatal("non-claude contributor must be refused")
	}

	// A mutated frozen template fails the digest comparison on the
	// reserved session.
	writeKnob(t, h.template, "drift.md", "drift")
	if _, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID: h.sessionID, Contributor: "claude",
		Config: adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: primaryModel},
	}); !asConfigMismatch(err, nil) {
		t.Fatalf("mutated template must fail the digest comparison, got %v", err)
	}
	os.Remove(filepath.Join(h.template, "drift.md"))
}

func asConfigMismatch(err error, target **ErrSessionConfigMismatch) bool {
	for err != nil {
		if typed, ok := err.(*ErrSessionConfigMismatch); ok {
			if target != nil {
				*target = typed
			}
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// A failed creation shares its typed failure with every concurrent
// waiting caller; later callers may retry independently.
func TestClaudeAdapter_ConcurrentCreationFailureShared(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	failSession := "c3d4e5f6-a7b8-4c9d-8e0f-1a2b3c4d5e6f"
	failRun := "run-adapter-fail"
	universeDoc := `{"claude_code_version":"2.1.278","tools":["Read","Glob","Grep","Bash","Write","WebSearch"]}`
	if _, err := h.store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-adapter-fail", ControllerLease: "lease-adapter-fail", RunID: failRun,
		Brief: "adapter failure-sharing test", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      claudeProfile("sha256:"+sha256Sum(universeDoc), primaryModel),
	}); err != nil {
		t.Fatalf("create failure-run: %v", err)
	}
	if _, err := h.store.CreateSession(ctx, "op-sess-fail", "lease-adapter-fail", storage.SessionRecord{
		ID: failSession, RunID: failRun, Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create failing session record: %v", err)
	}
	// Instrument materialization through the adapter-owned seam to
	// prove ONE creation attempt is shared by every concurrent caller.
	var matCalls atomic.Int32
	origMaterialize := h.adapter.materialize
	h.adapter.materialize = func(templateDir, base, runID, sessionID string) (string, string, error) {
		matCalls.Add(1)
		time.Sleep(100 * time.Millisecond)
		return "", "", fmt.Errorf("injected materialization failure for %s", sessionID)
	}
	defer func() { h.adapter.materialize = origMaterialize }()

	const n = 3
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			_, errs[slot] = h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
				SessionID:   adapter.SessionID(failSession),
				Contributor: "claude",
				Config: adapter.SessionConfig{
					WorkspaceRoot: h.wsRoot,
					Model:         primaryModel,
				},
			})
		}(i)
	}
	wg.Wait()
	if got := matCalls.Load(); got != 1 {
		t.Fatalf("concurrent creation must run materialization exactly once, got %d attempts", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			t.Fatalf("concurrent caller %d must fail", i)
		}
		if errs[i] != errs[0] {
			t.Fatalf("concurrent caller %d must share the SAME typed failure instance, got %v vs %v", i, errs[i], errs[0])
		}
	}

	// A later caller (after the failed reservation was retired) retries
	// independently: a second materialization attempt runs.
	if _, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID(failSession),
		Contributor: "claude",
		Config:      adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: primaryModel},
	}); err == nil || !strings.Contains(err.Error(), "materialize config root") {
		t.Fatalf("independent retry must fail on its own while the root exists, got %v", err)
	}
	if got := matCalls.Load(); got != 2 {
		t.Fatalf("the independent retry must run its own materialization attempt, got %d total", got)
	}
}

// After the service persists the binding, a duplicate CreateSession
// must match the persisted frozen config and share its native id.
func TestClaudeAdapter_CreateSessionMatchesPersistedBinding(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	binding := h.createSession(t, string(h.sessionID))

	again, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID: h.sessionID, Contributor: "claude",
		Config: adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: primaryModel},
	})
	if err != nil {
		t.Fatalf("duplicate after persistence: %v", err)
	}
	if again.NativeSessionID != binding.NativeSessionID {
		t.Fatalf("persisted native id mismatch: %q vs %q", again.NativeSessionID, binding.NativeSessionID)
	}

	if _, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID: h.sessionID, Contributor: "claude",
		Config: adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: "claude-opus-4-1-20250805"},
	}); !asConfigMismatch(err, nil) {
		t.Fatalf("persisted model mismatch must fail closed, got %v", err)
	}
}

// ── §3.4 ResumeSession local inspection ─────────────────────────────────

func TestClaudeAdapter_ResumeSessionInspection(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	// Unpersisted session: resume fails closed.
	if err := h.adapter.ResumeSession(ctx, adapter.SessionBinding{
		SessionID:       h.sessionID,
		NativeSessionID: "00000000-0000-4000-8000-00000000000f",
	}); err == nil {
		t.Fatal("resume without a persisted binding must fail")
	}

	binding := h.createSession(t, string(h.sessionID))

	// Steps 1–2 on an unmaterialized binding: success, no transcript
	// required — never classified as lost.
	if err := h.adapter.ResumeSession(ctx, binding); err != nil {
		t.Fatalf("unmaterialized resume must succeed: %v", err)
	}

	// Native id mismatch: typed.
	if err := h.adapter.ResumeSession(ctx, adapter.SessionBinding{
		SessionID:       h.sessionID,
		NativeSessionID: "00000000-0000-4000-8000-00000000000f",
		Config:          binding.Config,
	}); !asConfigMismatch(err, nil) {
		t.Fatalf("native id mismatch must be typed, got %v", err)
	}

	// Model mismatch: typed.
	if err := h.adapter.ResumeSession(ctx, adapter.SessionBinding{
		SessionID:       h.sessionID,
		NativeSessionID: binding.NativeSessionID,
		Config:          adapter.SessionConfig{Model: "claude-opus-4-1-20250805", WorkspaceRoot: h.wsRoot},
	}); !asConfigMismatch(err, nil) {
		t.Fatalf("model mismatch must be typed, got %v", err)
	}

	// Frozen template drift: typed.
	writeKnob(t, h.template, "drift.md", "drift")
	if err := h.adapter.ResumeSession(ctx, binding); !asConfigMismatch(err, nil) {
		t.Fatalf("template drift must be typed, got %v", err)
	}
	os.Remove(filepath.Join(h.template, "drift.md"))

	// Missing config root: failure.
	stored, _ := h.store.GetClaudeSessionBinding(ctx, string(h.sessionID))
	rootBackup := filepath.Join(filepath.Dir(stored.ConfigRoot), "root-backup")
	if err := os.Rename(stored.ConfigRoot, rootBackup); err != nil {
		t.Fatalf("stash config root: %v", err)
	}
	if err := h.adapter.ResumeSession(ctx, binding); err == nil {
		t.Fatal("missing config root must fail")
	}
	if err := os.Rename(rootBackup, stored.ConfigRoot); err != nil {
		t.Fatalf("restore config root: %v", err)
	}

	// Materialized binding without a transcript: failure.
	if err := h.store.MarkClaudeSessionMaterialized(ctx, string(h.sessionID)); err != nil {
		t.Fatalf("mark materialized: %v", err)
	}
	if err := h.adapter.ResumeSession(ctx, binding); err == nil {
		t.Fatal("materialized binding without its transcript must fail")
	}

	// Materialized binding whose transcript carries the accepted user
	// entry correlating with the recorded attempt's prompt digest. An
	// UNCERTAIN attempt is never authoritative: even a perfectly
	// matching user entry must not validate resume.
	if err := h.store.InsertClaudeTurnAttempt(ctx, storage.ClaudeTurnAttempt{
		AttemptID: "att_uncertain", SessionID: string(h.sessionID), TurnKey: "t-uncertain",
		NativeID: stored.NativeID, PromptDigest: PromptDigest("uncertain prompt", "att_uncertain"),
		TranscriptProtection: "advisory",
	}); err != nil {
		t.Fatalf("insert uncertain attempt: %v", err)
	}
	if err := h.store.InsertClaudeTurnAttempt(ctx, storage.ClaudeTurnAttempt{
		AttemptID: "att_resume", SessionID: string(h.sessionID), TurnKey: "t-resume",
		NativeID: stored.NativeID, PromptDigest: PromptDigest("the resume prompt", "att_resume"),
		TranscriptProtection: "advisory",
	}); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
	if err := h.store.SetClaudeAttemptTerminal(ctx, "att_resume", "completed", "ok result", "{}"); err != nil {
		t.Fatalf("make attempt authoritative: %v", err)
	}
	path, err := TranscriptPath(stored.ConfigRoot, stored.Workspace, stored.NativeID)
	if err != nil {
		t.Fatalf("transcript path: %v", err)
	}
	if os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	writeTranscript := func(userText string) {
		t.Helper()
		transcript := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`+"\n", userText)
		if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
	}
	writeTranscript("uncertain prompt")
	if err := h.adapter.ResumeSession(ctx, binding); err == nil ||
		!strings.Contains(err.Error(), "matching any recorded prompt digest") {
		t.Fatalf("an uncertain attempt's entry must not validate resume, got %v", err)
	}

	writeTranscript("the resume prompt")
	if err := h.adapter.ResumeSession(ctx, binding); err != nil {
		t.Fatalf("materialized resume with correlating transcript must succeed: %v", err)
	}

	// Mutation of the MATERIALIZED root is detected: the copied tree
	// must still match the frozen template digest (the §3.6 projects/
	// runtime subtree is excluded from that comparison).
	writeKnob(t, stored.ConfigRoot, "injected.txt", "mutation")
	if err := h.adapter.ResumeSession(ctx, binding); !asConfigMismatch(err, nil) {
		t.Fatalf("mutated config root must fail the frozen digest, got %v", err)
	}
	os.Remove(filepath.Join(stored.ConfigRoot, "injected.txt"))
	if err := h.adapter.ResumeSession(ctx, binding); err != nil {
		t.Fatalf("restored config root must resume cleanly: %v", err)
	}

	// A user entry that matches no recorded prompt digest fails the
	// session-identity proof.
	writeTranscript("a different prompt entirely")
	if err := h.adapter.ResumeSession(ctx, binding); err == nil ||
		!strings.Contains(err.Error(), "matching any recorded prompt digest") {
		t.Fatalf("uncorrelatable transcript must fail, got %v", err)
	}
	writeTranscript("the resume prompt")

	// Corrupt transcript: integrity failure.
	if err := os.WriteFile(path, []byte("{\"type\":\"user\"\nNOT JSON\n"), 0o600); err != nil {
		t.Fatalf("write corrupt transcript: %v", err)
	}
	if err := h.adapter.ResumeSession(ctx, binding); err == nil {
		t.Fatal("corrupt transcript must fail integrity checks")
	}

	// Symlinked transcript: rejected.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove transcript: %v", err)
	}
	if err := os.WriteFile(path+"-real", []byte(fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`+"\n", "the resume prompt")), 0o600); err != nil {
		t.Fatalf("write real transcript: %v", err)
	}
	if err := os.Symlink(path+"-real", path); err != nil {
		t.Fatalf("symlink transcript: %v", err)
	}
	if err := h.adapter.ResumeSession(ctx, binding); err == nil {
		t.Fatal("symlinked transcript must be rejected")
	}
}

// A template that defines the reserved projects/ directory is
// rejected at creation: the runtime transcript subtree (§3.6) is owned
// by the runtime, and a template collision would make the frozen
// digest and the copied-root digest disagree forever.
func TestClaudeAdapter_TemplateMayNotReserveProjectsDir(t *testing.T) {
	h := newAdapterHarness(t)

	writeKnob(t, h.template, "keep.txt", "template marker")
	os.MkdirAll(filepath.Join(h.template, "projects"), 0o700)
	defer os.RemoveAll(filepath.Join(h.template, "projects"))

	_, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   h.sessionID,
		Contributor: "claude",
		Config: adapter.SessionConfig{
			WorkspaceRoot: h.wsRoot,
			Model:         primaryModel,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved for runtime transcripts") {
		t.Fatalf("template defining projects/ must be rejected, got %v", err)
	}
	if stored, _ := h.store.GetClaudeSessionBinding(context.Background(), string(h.sessionID)); stored != nil {
		t.Fatal("no binding may exist for a session whose template was rejected")
	}

	// After removing the reserved directory the same request succeeds.
	if err := os.RemoveAll(filepath.Join(h.template, "projects")); err != nil {
		t.Fatalf("remove reserved dir: %v", err)
	}
	binding, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   h.sessionID,
		Contributor: "claude",
		Config: adapter.SessionConfig{
			WorkspaceRoot: h.wsRoot,
			Model:         primaryModel,
		},
	})
	if err != nil {
		t.Fatalf("create after removing reserved dir: %v", err)
	}
	h.persistBinding(t, string(h.sessionID), binding.NativeSessionID, primaryModel, h.wsRoot)
	if err := h.adapter.ResumeSession(context.Background(), binding); err != nil {
		t.Fatalf("binding from a clean template must pass local inspection: %v", err)
	}
}

// ── §3.5 durable blocking + disposition unblocking ──────────────────────

// An unresolved attempt blocks every later turn on the native session
// from DURABLE state — including through an adapter "restart" (fresh
// in-memory state, same store) — and the controller's recorded
// disposition unblocks it.
func TestClaudeAdapter_RestartBlockingAndDispositionUnblock(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()
	h.createSession(t, string(h.sessionID))

	// Force an uncertain attempt: the child exits without reading
	// stdin, so a large prompt fails post-transmission.
	writeKnob(t, h.wsRoot, ".claude-fixture-stdin-exit", "")
	ref := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-ambig"}
	outcome, err := h.adapter.Dispatch(ctx, ref, strings.Repeat("x", 1<<20))
	if err != nil || outcome.Status != adapter.DispatchUnknown {
		t.Fatalf("ambiguous dispatch: %v %v", outcome.Status, err)
	}
	attempt, err := h.store.GetLatestClaudeTurnAttempt(ctx, string(h.sessionID), "t-ambig")
	if err != nil || attempt == nil || attempt.Terminal || attempt.ObservedStatus != "uncertain" {
		t.Fatalf("attempt must be durably uncertain: %+v %v", attempt, err)
	}
	if attempt.UncertaintyDisposition != nil {
		t.Fatal("uncertainty disposition must be unset before the controller acts")
	}

	// Same process: blocked from durable state.
	next := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-next-1"}
	outcome, _ = h.adapter.Dispatch(ctx, next, "prompt")
	if outcome.Status != adapter.DispatchRejected ||
		!strings.Contains(outcome.Reason, "unresolved attempt") {
		t.Fatalf("unresolved attempt must block, got %v (%s)", outcome.Status, outcome.Reason)
	}

	// Restart: a fresh adapter over the same store (empty in-memory
	// slot and turn maps) must still observe the durable block.
	restarted := NewClaudeAdapter(h.store, h.wm, h.adapter.executor, h.adapter.launchSource,
		testIdentitySource{}, h.configBase, h.template)
	outcome, _ = restarted.Dispatch(ctx, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-next-2"}, "prompt")
	if outcome.Status != adapter.DispatchRejected ||
		!strings.Contains(outcome.Reason, "unresolved attempt") {
		t.Fatalf("durable block must survive restart, got %v (%s)", outcome.Status, outcome.Reason)
	}

	// The controller records the disposition; the durable state change
	// unblocks the session — including for the restarted adapter.
	h.simulateDisposition(t, attempt.AttemptID)

	// Clear the fault knob so the unblocking turn can run the happy
	// stream.
	os.Remove(filepath.Join(h.wsRoot, ".claude-fixture-stdin-exit"))

	outcome, err = restarted.Dispatch(ctx, next, "prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("disposition must unblock the session, got %v (%s) err=%v", outcome.Status, outcome.Reason, err)
	}
	result := waitTerminal(t, h, next)
	if result.Status != council.TurnCompleted {
		t.Fatalf("post-disposition turn must complete, got %+v", result)
	}
}

// Protection freezing through the production dispatch path: attempts
// launch advisory when no matching attestation exists, and upgrade to
// protected with the attestation id frozen at baseline once an
// attestation matching (version, platform, manifest digest, template
// digest) is recorded.
func TestClaudeAdapter_FreezesMatchingAttestation(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()
	h.createSession(t, string(h.sessionID))

	// No attestation yet: the attempt is advisory with no frozen id.
	advRef := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-adv"}
	if _, err := h.adapter.Dispatch(ctx, advRef, "prompt"); err != nil {
		t.Fatalf("dispatch advisory: %v", err)
	}
	adv := waitTerminal(t, h, advRef)
	if adv.Status != council.TurnCompleted {
		t.Fatalf("advisory turn must complete, got %+v", adv)
	}
	advAttempt, err := h.store.GetLatestClaudeTurnAttempt(ctx, string(h.sessionID), "t-adv")
	if err != nil || advAttempt == nil {
		t.Fatalf("get advisory attempt: %v", err)
	}
	if advAttempt.TranscriptProtection != "advisory" || advAttempt.AttestationID != "" {
		t.Fatalf("without a matching attestation the attempt must be advisory, got %q id=%q",
			advAttempt.TranscriptProtection, advAttempt.AttestationID)
	}

	// Record an attestation matching the four binding fields in force.
	profileRec, err := h.store.GetRunProfile(ctx, "run-adapter")
	if err != nil {
		t.Fatalf("run profile: %v", err)
	}
	templateDigest, err := TemplateDigest(h.template)
	if err != nil {
		t.Fatalf("template digest: %v", err)
	}
	att := ProtectionAttestation{
		ClaudeVersion:  "2.1.278",
		Platform:       runtime.GOOS + "/" + runtime.GOARCH,
		ManifestDigest: profileRec.ProfileDigest,
		TemplateDigest: templateDigest,
		Records: []ProbeRecord{{
			ToolClass:           ProbeRead,
			ToolName:            "Read",
			Denied:              true,
			EnforcingCapability: CapCwdBoundary,
			DenialText:          "Claude requested permissions to read the sibling transcript",
		}},
		ProbedAt: "2026-09-22T00:00:00Z",
		Actor:    "operator-test",
	}
	digest, err := att.Digest()
	if err != nil {
		t.Fatalf("attestation digest: %v", err)
	}
	if _, err := h.store.RecordClaudeProtectionAttestation(ctx, "op-att-adapter", storage.ClaudeProtectionAttestationRecord{
		AttestationID:  digest,
		RunID:          "run-adapter",
		ClaudeVersion:  att.ClaudeVersion,
		Platform:       att.Platform,
		ManifestDigest: att.ManifestDigest,
		TemplateDigest: att.TemplateDigest,
		ProbeResults:   string(mustEncodeProbeRecords(t, att)),
		ProbedAt:       att.ProbedAt,
		Actor:          att.Actor,
	}); err != nil {
		t.Fatalf("record attestation: %v", err)
	}

	protRef := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-prot"}
	if _, err := h.adapter.Dispatch(ctx, protRef, "prompt"); err != nil {
		t.Fatalf("dispatch protected: %v", err)
	}
	waitTerminal(t, h, protRef)
	protAttempt, err := h.store.GetLatestClaudeTurnAttempt(ctx, string(h.sessionID), "t-prot")
	if err != nil || protAttempt == nil {
		t.Fatalf("get protected attempt: %v", err)
	}
	if protAttempt.TranscriptProtection != "protected" || protAttempt.AttestationID != digest {
		t.Fatalf("dispatch must freeze the matching attestation, got protection=%q id=%q",
			protAttempt.TranscriptProtection, protAttempt.AttestationID)
	}
}

func mustEncodeProbeRecords(t *testing.T, a ProtectionAttestation) []byte {
	t.Helper()
	encoded, err := a.EncodeProbeRecords()
	if err != nil {
		t.Fatalf("encode probe records: %v", err)
	}
	return encoded
}

// ── Dispatch identity selection ─────────────────────────────────────────

// The first turn on an unmaterialized binding launches with
// --session-id; the adapter marks the binding materialized after the
// verified first-turn terminal, and the next turn resumes with
// --resume.
func TestClaudeAdapter_FirstTurnThenResumeSelection(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	h.createSession(t, string(h.sessionID))

	if _, err := h.adapter.Dispatch(ctx, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-1"}, "prompt one"); err != nil {
		t.Fatalf("dispatch t-1: %v", err)
	}
	result := waitTerminal(t, h, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-1"})
	if result.Status != council.TurnCompleted || result.Output != "fixture response" {
		t.Fatalf("t-1 result: %+v", result)
	}

	invocations := fixtureArgLines(t, h.wsRoot)
	if len(invocations) != 1 {
		t.Fatalf("expected 1 fixture invocation, got %d", len(invocations))
	}
	if !argInvoked(invocations[0], "--session-id") {
		t.Fatalf("first turn must launch with --session-id, got %v", invocations[0])
	}

	binding, err := h.store.GetClaudeSessionBinding(ctx, string(h.sessionID))
	if err != nil || binding == nil {
		t.Fatalf("get binding: %v", err)
	}
	if !binding.Materialized {
		t.Fatal("binding must be materialized after the verified first-turn terminal")
	}

	// §3.4 step 3 end-to-end: the fixture wrote the accepted user
	// entry with the dispatched prompt, so the materialized binding
	// resumes against its own transcript.
	resumeReq := adapter.SessionBinding{
		SessionID:       h.sessionID,
		NativeSessionID: binding.NativeID,
		Config:          adapter.SessionConfig{Model: primaryModel, WorkspaceRoot: h.wsRoot},
	}
	if err := h.adapter.ResumeSession(ctx, resumeReq); err != nil {
		t.Fatalf("resume must correlate the transcript's accepted user entry: %v", err)
	}

	if _, err := h.adapter.Dispatch(ctx, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-2"}, "prompt two"); err != nil {
		t.Fatalf("dispatch t-2: %v", err)
	}
	result2 := waitTerminal(t, h, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-2"})
	if result2.Status != council.TurnCompleted {
		t.Fatalf("t-2 result: %+v", result2)
	}
	invocations = fixtureArgLines(t, h.wsRoot)
	if len(invocations) != 2 {
		t.Fatalf("expected 2 fixture invocations, got %d", len(invocations))
	}
	if !argInvoked(invocations[1], "--resume") {
		t.Fatalf("second turn must resume with --resume, got %v", invocations[1])
	}
}

// Materialization follows the transcript evidence boundary, not the
// result event: a verified terminal without an observed bound
// transcript leaves the binding unmaterialized.
func TestClaudeAdapter_MaterializationRequiresTranscriptObservation(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()
	binding := h.createSession(t, string(h.sessionID))

	// The child emits a verified success but writes no transcript.
	writeKnob(t, h.wsRoot, ".claude-fixture-no-transcript", "")
	writeKnob(t, h.wsRoot, ".claude-fixture", fixtureStream(h.wsRoot, binding.NativeSessionID, primaryModel,
		fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"session_id":%q,"result":"fixture response"}`, binding.NativeSessionID),
	))

	ref := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-no-transcript"}
	if _, err := h.adapter.Dispatch(ctx, ref, "prompt"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	result := waitTerminal(t, h, ref)
	if result.Status != council.TurnCompleted {
		t.Fatalf("verified result must complete the turn, got %+v", result)
	}

	after, err := h.store.GetClaudeSessionBinding(ctx, string(h.sessionID))
	if err != nil || after == nil {
		t.Fatalf("get binding: %v", err)
	}
	if after.Materialized {
		t.Fatal("binding must stay unmaterialized without an observed bound transcript")
	}
}

// A pre-materialized binding resumes on its first dispatch: the
// materialized flag, not dispatch order, selects the identity flag.
func TestClaudeAdapter_PreMaterializedBindingResumes(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	h.createSession(t, string(h.sessionID))
	if err := h.store.MarkClaudeSessionMaterialized(ctx, string(h.sessionID)); err != nil {
		t.Fatalf("mark materialized: %v", err)
	}

	if _, err := h.adapter.Dispatch(ctx, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-1"}, "prompt"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitTerminal(t, h, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-1"})

	invocations := fixtureArgLines(t, h.wsRoot)
	if len(invocations) != 1 || !argInvoked(invocations[0], "--resume") {
		t.Fatalf("pre-materialized binding must launch with --resume, got %v", invocations)
	}
}

// The stream validates the frozen launch model, not a hardcoded one: a
// second run pinned to a different Claude model completes.
func TestClaudeAdapter_CarriesFrozenModel(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	const otherModel = "claude-sonnet-4-5-20250929"
	const otherSession = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"
	const otherRun = "run-adapter-2"

	universeDoc := `{"claude_code_version":"2.1.278","tools":["Read","Glob","Grep","Bash","Write","WebSearch"]}`
	if _, err := h.store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-adapter-2", ControllerLease: "lease-adapter-2", RunID: otherRun,
		Brief: "adapter model test", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      claudeProfile("sha256:"+sha256Sum(universeDoc), otherModel),
	}); err != nil {
		t.Fatalf("create second run: %v", err)
	}
	if _, err := h.store.CreateSession(ctx, "op-sess-adapter-2", "lease-adapter-2", storage.SessionRecord{
		ID: otherSession, RunID: otherRun, Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create second session: %v", err)
	}

	h.createSessionWithConfig(t, otherSession, otherModel, h.workspaceRootFor(t, otherRun, otherSession))

	if _, err := h.adapter.Dispatch(ctx, adapter.TurnRef{SessionID: otherSession, TurnKey: "t-model"}, "prompt"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	result := waitTerminal(t, h, adapter.TurnRef{SessionID: otherSession, TurnKey: "t-model"})
	if result.Status != council.TurnCompleted || result.Output != "fixture response" {
		t.Fatalf("frozen model must be carried into stream validation, got %+v", result)
	}

	wsRoot := h.workspaceRootFor(t, otherRun, otherSession)
	invocations := fixtureArgLines(t, wsRoot)
	if len(invocations) != 1 || argValue(invocations[0], "--model") != otherModel {
		t.Fatalf("launch must pin the frozen model, got %v", invocations)
	}
}

// ── Ambiguous write: durable uncertainty ────────────────────────────────

// A stdin failure after the first transmitted byte is ambiguous: the
// outcome is unknown, the first-byte boundary is persisted, the attempt
// stays uncertain, and the session is blocked from durable state.
func TestClaudeAdapter_PostTransmissionFailureBlocksDurally(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()
	h.createSession(t, string(h.sessionID))

	writeKnob(t, h.wsRoot, ".claude-fixture-stdin-exit", "")

	outcome, err := h.adapter.Dispatch(ctx, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-big"}, strings.Repeat("x", 1<<20))
	if err != nil {
		t.Fatalf("ambiguous dispatch must not return a transport error: %v", err)
	}
	if outcome.Status != adapter.DispatchUnknown {
		t.Fatalf("post-transmission failure must be unknown, got %v (%s)", outcome.Status, outcome.Reason)
	}

	attempt, err := h.store.GetLatestClaudeTurnAttempt(ctx, string(h.sessionID), "t-big")
	if err != nil || attempt == nil {
		t.Fatalf("get attempt: %v", err)
	}
	if attempt.Terminal || attempt.ObservedStatus != "uncertain" {
		t.Fatalf("attempt must stay uncertain, got terminal=%v status=%q", attempt.Terminal, attempt.ObservedStatus)
	}

	// First-byte boundary persisted at the write boundary.
	var boundary interface{}
	if err := h.store.DB().QueryRow(
		`SELECT first_stdin_byte_at FROM claude_attempt_launches WHERE attempt_id = ?`, attempt.AttemptID,
	).Scan(&boundary); err != nil {
		t.Fatalf("query launch row: %v", err)
	}
	if boundary == nil {
		t.Fatal("first-byte transmission boundary must be persisted")
	}

	// The block derives from durable state: the next dispatch is
	// rejected with the unresolved-attempt reason (the in-memory slot
	// was released when active execution ended).
	second, _ := h.adapter.Dispatch(ctx, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-second"}, "prompt")
	if second.Status != adapter.DispatchRejected ||
		!strings.Contains(second.Reason, "unresolved attempt") {
		t.Fatalf("blocked session must reject with the durable reason, got %v (%s)", second.Status, second.Reason)
	}
}

// ── Terminal mapping ────────────────────────────────────────────────────

// A verified error result (error_max_turns) is a FAILED terminal: the
// durable attempt records observed_status='failed', Collect reports
// TurnFailed, and the stream's terminal event carries TurnFailed.
func TestClaudeAdapter_FailedTerminalMapping(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()
	binding := h.createSession(t, string(h.sessionID))

	writeKnob(t, h.wsRoot, ".claude-fixture", fixtureStream(h.wsRoot, binding.NativeSessionID, primaryModel,
		fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]},"session_id":%q}`, binding.NativeSessionID),
		fmt.Sprintf(`{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":%q,"result":"reached max turns"}`, binding.NativeSessionID),
	))
	// Keep the turn alive briefly: subscribing must deterministically
	// win the race against a fast child completing and removing the
	// live-turn entry.
	writeKnob(t, h.wsRoot, ".claude-fixture-delay", "200")

	ref := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-fail"}
	if _, err := h.adapter.Dispatch(ctx, ref, "prompt"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	stream, err := h.adapter.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	var terminalEvents []adapter.Event
	for ev := range stream.Events() {
		if ev.Type == adapter.EventTerminal {
			terminalEvents = append(terminalEvents, ev)
		}
	}
	if len(terminalEvents) != 1 || terminalEvents[0].Status != council.TurnFailed {
		t.Fatalf("expected exactly one TurnFailed terminal event, got %+v", terminalEvents)
	}

	result := waitTerminal(t, h, ref)
	if result.Status != council.TurnFailed || result.ResultStatus != adapter.ResultFailed {
		t.Fatalf("collect must report a failed turn, got %+v", result)
	}
	if result.Output != "reached max turns" {
		t.Fatalf("verified failure text must be retained, got %q", result.Output)
	}

	attempt, err := h.store.GetLatestClaudeTurnAttempt(ctx, string(h.sessionID), "t-fail")
	if err != nil || attempt == nil {
		t.Fatalf("get attempt: %v", err)
	}
	if !attempt.Terminal || attempt.ObservedStatus != "failed" {
		t.Fatalf("durable attempt must be terminal failed, got terminal=%v status=%q", attempt.Terminal, attempt.ObservedStatus)
	}

	// A verified failed terminal still proves the native process
	// accepted the prompt: the first turn materializes the binding and
	// its transcript satisfies resume through the FAILED attempt.
	after, err := h.store.GetClaudeSessionBinding(ctx, string(h.sessionID))
	if err != nil || after == nil {
		t.Fatalf("get binding: %v", err)
	}
	if !after.Materialized {
		t.Fatal("a verified failed first turn must still materialize the binding")
	}
	if err := h.adapter.ResumeSession(ctx, binding); err != nil {
		t.Fatalf("materialized failed first turn must resume via its transcript: %v", err)
	}

	rec, err := h.adapter.Reconcile(ctx, adapter.RecoveryRef{TurnRef: ref, Generation: 1})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Status != adapter.ReconciliationReachableTerminal || rec.Observed != council.TurnFailed {
		t.Fatalf("reconcile must report a failed terminal, got %+v", rec)
	}
}

// ── Observe / detach ────────────────────────────────────────────────────

// Observe rejects unknown refs, a detached observer does not affect the
// turn, and the map entry is removed after completion.
func TestClaudeAdapter_ObserveDetachAndCompletion(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()
	h.createSession(t, string(h.sessionID))

	ref := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-obs"}
	if _, err := h.adapter.Observe(ctx, ref); err == nil {
		t.Fatal("observe before dispatch must fail")
	}

	writeKnob(t, h.wsRoot, ".claude-fixture-delay", "200")
	if _, err := h.adapter.Dispatch(ctx, ref, "prompt"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	stream, err := h.adapter.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	// Detach after the first event: the turn must complete regardless.
	for ev := range stream.Events() {
		_ = ev
		break
	}
	result := waitTerminal(t, h, ref)
	if result.Status != council.TurnCompleted || result.Output != "fixture response" {
		t.Fatalf("detached turn must still complete, got %+v", result)
	}

	if _, err := h.adapter.Observe(ctx, ref); err == nil {
		t.Fatal("observe after completion must fail (map entry removed)")
	}
}

// ── Cancel ──────────────────────────────────────────────────────────────

// Cancelling an unknown or completed turn reports CancelUnknown; a
// mid-turn cancel terminates the process, leaves the attempt uncertain
// without a result, and the session blocks from durable state.
func TestClaudeAdapter_CancelSemantics(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()
	h.createSession(t, string(h.sessionID))

	unknown := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "never-dispatched"}
	if out, err := h.adapter.Cancel(ctx, unknown); err != nil || out.Disposition != adapter.CancelUnknown {
		t.Fatalf("cancel of unknown ref must be CancelUnknown, got %v %v", out.Disposition, err)
	}

	writeKnob(t, h.wsRoot, ".claude-fixture-delay", "30000")
	ref := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-cancel"}
	if _, err := h.adapter.Dispatch(ctx, ref, "prompt"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Subscribe BEFORE cancelling: a dead turn is no longer observable
	// (the live-turn entry is removed at process death), so observing
	// after Cancel would race the removal.
	stream, err := h.adapter.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	out, err := h.adapter.Cancel(ctx, ref)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if out.Disposition != adapter.CancelUnknown {
		t.Fatalf("terminate without a result is CancelUnknown, got %v", out.Disposition)
	}

	drainStream(t, stream)

	attempt, err := h.store.GetLatestClaudeTurnAttempt(ctx, string(h.sessionID), "t-cancel")
	if err != nil || attempt == nil {
		t.Fatalf("get attempt: %v", err)
	}
	if attempt.Terminal {
		t.Fatal("cancelled turn has no verified result and must not be terminal")
	}

	// The block derives from durable state.
	blocked, _ := h.adapter.Dispatch(ctx, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-next"}, "prompt")
	if blocked.Status != adapter.DispatchRejected ||
		!strings.Contains(blocked.Reason, "unresolved attempt") {
		t.Fatalf("blocked session must reject dispatch after cancel, got %v (%s)", blocked.Status, blocked.Reason)
	}
}

// ── Reconcile four-state ────────────────────────────────────────────────

// Reconcile over durable evidence: unknown refs are Uncertain (never
// DefinitivelyMissing by absence) and a live in-process run is
// reachable-active.
func TestClaudeAdapter_ReconcileActiveAndUnknownStates(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	// Uncertain: an unknown ref has no durable attempt — absence of
	// evidence is never positive proof.
	ghost := adapter.RecoveryRef{
		TurnRef: adapter.TurnRef{SessionID: h.sessionID, TurnKey: "ghost"}, Generation: 1,
	}
	rec, err := h.adapter.Reconcile(ctx, ghost)
	if err != nil {
		t.Fatalf("reconcile ghost: %v", err)
	}
	if rec.Status != adapter.ReconciliationUncertain || rec.Reachability != council.VisibilityHostLost ||
		rec.Observed != council.TurnRunning {
		t.Fatalf("unknown ref must reconcile uncertain, got %+v", rec)
	}

	// Reachable-active: a dispatched turn with a live in-process run.
	h.createSession(t, string(h.sessionID))
	writeKnob(t, h.wsRoot, ".claude-fixture-delay", "30000")
	activeRef := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-active"}
	if _, err := h.adapter.Dispatch(ctx, activeRef, "prompt"); err != nil {
		t.Fatalf("dispatch active: %v", err)
	}
	rec, err = h.adapter.Reconcile(ctx, adapter.RecoveryRef{TurnRef: activeRef, Generation: 1})
	if err != nil {
		t.Fatalf("reconcile active: %v", err)
	}
	if rec.Status != adapter.ReconciliationReachableActive || rec.Reachability != council.VisibilityReachable ||
		rec.Observed != council.TurnRunning {
		t.Fatalf("live run must reconcile reachable-active, got %+v", rec)
	}
	if _, err := h.adapter.Cancel(ctx, activeRef); err != nil {
		t.Fatalf("cancel active: %v", err)
	}
}

// The terminal and missing reconciliation legs run on their own
// session so the uncertain legs above do not block them.
func TestClaudeAdapter_ReconcileTerminalAndMissingStates(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	// Reachable-terminal.
	h.createSession(t, string(h.sessionID))
	termRef := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-term"}
	if _, err := h.adapter.Dispatch(ctx, termRef, "prompt"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitTerminal(t, h, termRef)
	rec, err := h.adapter.Reconcile(ctx, adapter.RecoveryRef{TurnRef: termRef, Generation: 1})
	if err != nil {
		t.Fatalf("reconcile terminal: %v", err)
	}
	if rec.Status != adapter.ReconciliationReachableTerminal || rec.Observed != council.TurnCompleted ||
		rec.Result != "fixture response" {
		t.Fatalf("verified result must reconcile reachable-terminal, got %+v", rec)
	}

	// DefinitivelyMissing: a definitive start failure retains
	// start_failed evidence — positive pre-start proof that no process
	// was ever created. A fresh turn key on the bound session isolates
	// the launch from the earlier terminal turn.
	emptyBin := t.TempDir()
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", emptyBin)
	t.Cleanup(func() { os.Setenv("PATH", origPath) })

	missingRef := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-missing"}
	outcome, _ := h.adapter.Dispatch(ctx, missingRef, "prompt")
	if outcome.Status != adapter.DispatchRejected {
		t.Fatalf("start failure must reject pre-acceptance, got %v (%s)", outcome.Status, outcome.Reason)
	}

	rec, err = h.adapter.Reconcile(ctx, adapter.RecoveryRef{TurnRef: missingRef, Generation: 1})
	if err != nil {
		t.Fatalf("reconcile missing: %v", err)
	}
	if rec.Status != adapter.ReconciliationDefinitivelyMissing || rec.Observed != council.TurnFailed ||
		rec.Reachability != council.VisibilityReachable {
		t.Fatalf("start_failed evidence must reconcile definitively-missing, got %+v", rec)
	}

	// A definitive start failure is a safe pre-start failure, not an
	// unresolved uncertainty: the attempt is observed_status='missing'
	// and does NOT block the session.
	missingAttempt, err := h.store.GetLatestClaudeTurnAttempt(ctx, string(h.sessionID), "t-missing")
	if err != nil || missingAttempt == nil {
		t.Fatalf("get missing attempt: %v", err)
	}
	if missingAttempt.ObservedStatus != "missing" {
		t.Fatalf("start failure must classify missing, got %q", missingAttempt.ObservedStatus)
	}
	if blocked, _ := h.store.HasClaudeUnresolvedAttempts(ctx, missingAttempt.NativeID); blocked {
		t.Fatal("a missing attempt must not block the native session")
	}

	os.Setenv("PATH", origPath)
	afterRef := adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-after-missing"}
	outcome, err = h.adapter.Dispatch(ctx, afterRef, "prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("session must not be blocked after a definitive start failure, got %v (%s) err=%v",
			outcome.Status, outcome.Reason, err)
	}
	waitTerminal(t, h, afterRef)
}
