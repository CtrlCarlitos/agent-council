//go:build unix

package claude

// Task 5 adapter contract evidence (POSIX — real executor + fixture
// child): session creation persistence, first-vs-resume identity
// selection, process lifecycle through PolicyExecutor, parser-to-tap
// event delivery, terminal persistence (completed AND failed), slot
// retention on ambiguity, cancel/observe semantics, and four-state
// reconcile over durable evidence.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	universeDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(universeDoc)))

	const lease = "lease-adapter"
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-adapter", ControllerLease: lease, RunID: "run-adapter",
		Brief: "adapter test", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile: func() storage.CanonicalProfile {
			p := storage.CanonicalProfile{
				AlgoVersion:         "cprof-v2",
				WorkspaceMode:       "none",
				IsolationStrictness: "permissive_dev",
				NetworkMode:         "unrestricted",
				Tooling:             []string{"claude", "git", "go"},
				Harnesses: map[string]storage.HarnessProfileSpec{
					"claude": {Model: primaryModel, NativeAuthMode: "inherited_host_keychain"},
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
		}(),
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

// createSession drives the production CreateSession path and registers
// a durable claude binding for the (claude-contributor) session record.
func (h *adapterHarness) createSession(t *testing.T, id string) adapter.SessionBinding {
	t.Helper()
	binding, err := h.adapter.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID(id),
		Contributor: "claude",
		Config: adapter.SessionConfig{
			WorkspaceRoot: h.workspaceRoot(t, id),
			Model:         primaryModel,
		},
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	return binding
}

func (h *adapterHarness) workspaceRoot(t *testing.T, sessionID string) string {
	t.Helper()
	if paths, ok := h.wm.GetPaths("run-adapter", sessionID); ok {
		return paths.Root
	}
	paths, err := h.wm.AllocateWorkspace("run-adapter", sessionID, "none", "example/repo",
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
func fixtureStream(wsRoot, nativeID string, extra ...string) string {
	lines := []string{
		`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup"}`,
		fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"cwd":%q,"claude_code_version":"2.1.278","model":%q,"permissionMode":"default","tools":["Read","Glob","Grep"],"skills":[],"plugins":[]}`,
			nativeID, wsRoot, primaryModel),
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

type testIdentitySource struct{}

func (testIdentitySource) AttemptFor(_ context.Context, ref adapter.TurnRef) (string, bool) {
	return "att_" + ref.TurnKey, true
}

func unusedIdentityFunc(ctx context.Context, ref adapter.TurnRef) (string, bool) {
	return "att_" + ref.TurnKey, true
}

// ── CreateSession ───────────────────────────────────────────────────────

// CreateSession generates a fresh UUIDv4 native identity, materializes
// the per-session config root, and persists an unmaterialized binding;
// duplicates are idempotent; non-claude contributors are refused.
func TestClaudeAdapter_CreateSessionPersistsBinding(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	binding := h.createSession(t, string(h.sessionID))
	if binding.NativeSessionID == string(h.sessionID) {
		t.Fatal("native id must be freshly generated, not the logical session id")
	}
	if !isValidUUIDv4(binding.NativeSessionID) {
		t.Fatalf("native id must be UUIDv4, got %q", binding.NativeSessionID)
	}

	stored, err := h.store.GetClaudeSessionBinding(ctx, string(h.sessionID))
	if err != nil || stored == nil {
		t.Fatalf("get binding: %v", err)
	}
	if stored.NativeID != binding.NativeSessionID {
		t.Fatalf("persisted native id %q != returned %q", stored.NativeID, binding.NativeSessionID)
	}
	if stored.Materialized {
		t.Fatal("fresh binding must be unmaterialized")
	}
	wantRoot := ConfigRootPath(h.configBase, "run-adapter", string(h.sessionID))
	if stored.ConfigRoot != wantRoot {
		t.Fatalf("config root %q != expected %q", stored.ConfigRoot, wantRoot)
	}
	if stored.TemplateDigest == "" {
		t.Fatal("template digest must be recorded")
	}
	if _, err := os.Stat(filepath.Join(stored.ConfigRoot, "settings.json")); err != nil {
		t.Fatalf("materialized config root missing settings.json: %v", err)
	}

	// Duplicate request: idempotent, same native id.
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
		t.Fatalf("duplicate create returned %q, want %q", again.NativeSessionID, binding.NativeSessionID)
	}

	// Non-claude contributor: refused before any native identity is minted.
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
	if b, _ := h.store.GetClaudeSessionBinding(ctx, "sess-other"); b != nil {
		t.Fatal("no binding may be persisted for a non-claude contributor")
	}
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

// ── Ambiguous write: slot blocking ──────────────────────────────────────

// A stdin failure after the first transmitted byte is ambiguous: the
// outcome is unknown, the first-byte boundary is persisted, the attempt
// stays uncertain, and the single-flight slot is NOT released — the
// native session is blocked until disposition.
func TestClaudeAdapter_PostTransmissionFailureBlocksSlot(t *testing.T) {
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

	// The slot is retained: a second dispatch on the native session
	// cannot acquire single-flight access.
	blockedCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	second, _ := h.adapter.Dispatch(blockedCtx, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-second"}, "prompt")
	if second.Status != adapter.DispatchRejected {
		t.Fatalf("blocked session must reject the second dispatch, got %v (%s)", second.Status, second.Reason)
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

	writeKnob(t, h.wsRoot, ".claude-fixture", fixtureStream(h.wsRoot, binding.NativeSessionID,
		fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]},"session_id":%q}`, binding.NativeSessionID),
		fmt.Sprintf(`{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":%q,"result":"reached max turns"}`, binding.NativeSessionID),
	))

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
// without a result, and retains the slot.
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

	out, err := h.adapter.Cancel(ctx, ref)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if out.Disposition != adapter.CancelUnknown {
		t.Fatalf("terminate without a result is CancelUnknown, got %v", out.Disposition)
	}

	stream, err := h.adapter.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	drainStream(t, stream)

	attempt, err := h.store.GetLatestClaudeTurnAttempt(ctx, string(h.sessionID), "t-cancel")
	if err != nil || attempt == nil {
		t.Fatalf("get attempt: %v", err)
	}
	if attempt.Terminal {
		t.Fatal("cancelled turn has no verified result and must not be terminal")
	}

	// The slot is retained: the session is blocked until disposition.
	blockedCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	blocked, _ := h.adapter.Dispatch(blockedCtx, adapter.TurnRef{SessionID: h.sessionID, TurnKey: "t-next"}, "prompt")
	if blocked.Status != adapter.DispatchRejected {
		t.Fatalf("blocked session must reject dispatch after cancel, got %v (%s)", blocked.Status, blocked.Reason)
	}
}

// ── Reconcile four-state ────────────────────────────────────────────────

// Reconcile over durable evidence: unknown refs are Uncertain (never
// DefinitivelyMissing by absence), a live in-process run is
// reachable-active, a verified terminal is committed, and retained
// start_failed rows are positive pre-start evidence for
// DefinitivelyMissing.
func TestClaudeAdapter_ReconcileFourStates(t *testing.T) {
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
}

func sha256Sum(data string) string {
	sum := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", sum)
}
