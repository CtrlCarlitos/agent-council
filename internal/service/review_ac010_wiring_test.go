//go:build unix

package service

// AC-010 Task 7 wiring evidence: fail-closed Agy service construction
// (every configuration piece required, exactly one native adapter per
// server, the §3.12 platform refusal before any child), the derived
// production session birth (model/workspace/digest from the run's
// frozen profile, pre-flighted and in-transaction controller authority,
// a superseded lease during creation records the created conversation
// as an orphan episode), the operator-authorized run-bound cprot-v2
// attestation operation, controller-authorized resolution of creation-
// uncertainty episodes, and queue-time required_tools validation.
//
// Fixture only: the compiled agytest `agy` executable, temp homes, temp
// evidence roots. The real agy binary and ~/.gemini are never touched.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/agy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/agy/agytest"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const (
	agyWireRunID    = "run-ac010-wire"
	agyWireSession  = "sess-ac010-wire"
	agyWireLease    = "lease-ac010-wire"
	agyWireModel    = "gpt-oss-120b-medium"
	agyWireNativeID = "3f2c1a9e-8b7d-4c6e-9f01-23456789abcd"
	agyWireOtherID  = "7a6b5c4d-3e2f-4a1b-8c9d-0e1f2a3b4c5d"
	agyWireHooks    = `{"guardrail": {"command": "guardrail.sh", "enabled": true, "event": "PreToolUse"}}`
)

// agyWireTools is the frozen expected_tools of the service profile: one
// sibling_read_path tool, one own_mutation_path tool, one control tool
// (all classified in the committed 1.2.9 coverage evidence).
var agyWireTools = []string{"ask_permission", "view_file", "write_to_file"}

var (
	agyWireFxOnce sync.Once
	agyWireFx     *agytest.Fixture
	agyWireFxErr  error
)

func agyWireFixture(t *testing.T) *agytest.Fixture {
	t.Helper()
	agyWireFxOnce.Do(func() { agyWireFx, agyWireFxErr = agytest.NewFixture() })
	if agyWireFxErr != nil {
		t.Fatalf("agytest fixture: %v", agyWireFxErr)
	}
	return agyWireFx
}

// agyWireExecutor is the real PolicyExecutor; off Linux (no sealed
// image) the fixture launch is authorized through the explicit
// test-only marker, exactly as the agy package's own harness does.
type agyWireExecutor struct {
	inner execpolicy.PolicyExecutor
}

func (e *agyWireExecutor) Start(ctx context.Context, req execpolicy.LaunchRequest) (execpolicy.ManagedProcess, error) {
	if req.SealedImage == nil && runtime.GOOS != "linux" {
		req.FixtureLaunch = true
	}
	return e.inner.Start(ctx, req)
}

type agyWireEnv struct {
	stateDir, wsBase, scratch, home, evidenceRoot string
	fx                                            *agytest.Fixture
	profile                                       storage.CanonicalProfile
	policy                                        agy.AgyLaunchPolicy
	digest                                        string
	cfg                                           ServerConfig
}

func agyWriteUnder(t *testing.T, root, rel string, raw []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// newAgyWireEnv builds a complete, valid Agy service configuration over
// disjoint temp directories: the committed plugins/coverage evidence
// bytes staged under a temp evidence root, a synthetic init capture for
// agyWireTools, the hooks capture and expected skill staged in a temp
// .gemini home, the compiled fixture as the pinned binary.
func newAgyWireEnv(t *testing.T, mutate func(*storage.CanonicalProfile)) *agyWireEnv {
	t.Helper()
	fx := agyWireFixture(t)
	dir := testStateDir(t)
	e := &agyWireEnv{
		stateDir:     filepath.Join(dir, "state"),
		wsBase:       filepath.Join(dir, "ws"),
		scratch:      filepath.Join(dir, "agy-scratch"),
		home:         filepath.Join(dir, "home", ".gemini"),
		evidenceRoot: filepath.Join(dir, "evidence"),
		fx:           fx,
	}
	for _, p := range []string{e.stateDir, e.wsBase, e.scratch, e.home, e.evidenceRoot} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	const (
		pluginsRel  = "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json"
		coverageRel = "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json"
		initRel     = "docs/superpowers/evidence/ac010-agy-init-1.2.9.json"
	)
	repo := filepath.Join("..", "..")
	pluginsRaw, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(pluginsRel)))
	if err != nil {
		t.Fatalf("committed plugins evidence: %v", err)
	}
	coverageRaw, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(coverageRel)))
	if err != nil {
		t.Fatalf("committed coverage evidence: %v", err)
	}
	initRaw, err := json.Marshal(map[string]any{
		"conversation_id": "<conversation-id>", "event": "init",
		"init": map[string]any{"cwd": "<workspace>", "permission_mode": "request-review", "tools": agyWireTools},
	})
	if err != nil {
		t.Fatalf("marshal init capture: %v", err)
	}
	agyWriteUnder(t, e.evidenceRoot, pluginsRel, pluginsRaw)
	agyWriteUnder(t, e.evidenceRoot, coverageRel, coverageRaw)
	agyWriteUnder(t, e.evidenceRoot, initRel, initRaw)

	agyWriteUnder(t, e.home, "config/hooks.json", []byte(agyWireHooks))
	hooksDigest, err := agy.CanonicalHooksConfigDigest([]byte(agyWireHooks))
	if err != nil {
		t.Fatalf("hooks digest: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(e.home, "antigravity-cli", "skills", "research"), 0o700); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}

	spec := &storage.AgyHarnessSpec{
		CLIVersion:                  "1.2.9",
		BinaryPath:                  fx.BinaryPath,
		BinaryDigest:                fx.Digest,
		ExpectedHome:                e.home,
		Platform:                    storage.AgyPlatformSpec{OS: runtime.GOOS, Family: "unix"},
		PermissionMode:              "request-review",
		ExecutionMode:               "default",
		Sandbox:                     true,
		PrintTimeoutBackstopSeconds: 1800,
		ExpectedTools:               append([]string(nil), agyWireTools...),
		ExpectedMCPServers:          []string{},
		ExpectedMCPTools:            []string{},
		ExpectedPluginTools:         []string{},
		DefaultRequiredTools:        []string{},
		HooksEvidence:               storage.AgyHooksEvidenceSpec{Verified: []string{"guardrail hook present"}},
		ExpectedSkills:              []string{"research"},
		HooksConfigDigest:           hooksDigest,
		RequiredHooks:               []string{"/guardrail"},
		InitEvidencePath:            initRel,
		InitEvidenceDigest:          fmt.Sprintf("sha256:%x", sha256.Sum256(initRaw)),
		ToolCoveragePath:            coverageRel,
		ToolCoverageDigest:          fmt.Sprintf("sha256:%x", sha256.Sum256(coverageRaw)),
		PluginsEvidencePath:         pluginsRel,
		PluginsEvidenceDigest:       fmt.Sprintf("sha256:%x", sha256.Sum256(pluginsRaw)),
	}
	e.profile = storage.CanonicalProfile{
		AlgoVersion:         "cprof-v4",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"agy", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"agy": {Model: agyWireModel, NativeAuthMode: "inherited_gemini_home", Agy: spec},
		},
		ToolkitManifest: &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
			ProbedCLIVersion:       "2.1.278",
			UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
			UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
			ApprovedTools:          []string{"Read"},
			ExpectedHooks:          []string{"SessionStart:startup"},
			TurnsBound:             8,
		}},
	}
	if mutate != nil {
		mutate(&e.profile)
	}
	e.policy, err = agy.ValidateAgyHarness(e.profile, e.evidenceRoot)
	if err != nil {
		t.Fatalf("validate agy harness: %v", err)
	}
	e.digest, _, err = storage.ComputeProfileDigest(e.profile)
	if err != nil {
		t.Fatalf("profile digest: %v", err)
	}
	e.cfg = ServerConfig{
		StateDir:         e.stateDir,
		InstanceID:       "inst-ac010-wire",
		AuthToken:        "tok-ac010-wire",
		WorkspaceBaseDir: e.wsBase,
		AgyBinaryPath:    fx.BinaryPath,
		AgyProfile:       e.profile,
		AgyEvidenceRoot:  e.evidenceRoot,
		AgyHomeDir:       filepath.Dir(e.home),
		AgyScratchRoot:   e.scratch,
	}
	return e
}

func (e *agyWireEnv) openStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(storage.StoreOptions{StateDir: e.stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seed creates the run (frozen on profile), the agy session, and the
// adopted + connected controller.
func (e *agyWireEnv) seed(t *testing.T, store *storage.Store, profile storage.CanonicalProfile) {
	t.Helper()
	e.seedAdopted(t, store, profile)
	if _, err := store.ConnectRunController(context.Background(), "op-conn-ac010", agyWireRunID, agyWireLease, 1, e.cfg.InstanceID); err != nil {
		t.Fatalf("connect: %v", err)
	}
}

// seedAdopted creates the run, the agy and claude sessions, and the
// adopted (not yet connected) controller.
func (e *agyWireEnv) seedAdopted(t *testing.T, store *storage.Store, profile storage.CanonicalProfile) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-ac010", ControllerLease: agyWireLease, RunID: agyWireRunID,
		Brief: "agy service wiring", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      profile,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, s := range []struct{ id, contributor string }{{agyWireSession, "agy"}, {"sess-ac010-claude", "claude"}} {
		if _, err := store.CreateSession(ctx, "op-sess-"+s.id, agyWireLease, storage.SessionRecord{
			ID: s.id, RunID: agyWireRunID, Contributor: s.contributor, Role: "reviewer",
			IsActiveContributor: true, State: "parked", Visibility: "reachable",
		}); err != nil {
			t.Fatalf("create session %s: %v", s.id, err)
		}
	}
	if _, err := store.AdoptController(ctx, "op-adopt-ac010", agyWireRunID, "agy", "controller-ref-ac010", agyWireLease, nil, agyWireLease); err != nil {
		t.Fatalf("adopt: %v", err)
	}
}

// agyWireServer is a configured service over a FIXTURE-scoped agy
// adapter that shares the service's workspace manager (the AC-005
// allocation the birth path derives is the one the launch source uses).
type agyWireServer struct {
	srv  *Server
	adp  *agy.AgyAdapter
	wm   *workspace.WorkspaceManager
	root string // the AC-005 allocation root of agyWireSession
}

func (e *agyWireEnv) fixtureServer(t *testing.T, store *storage.Store) *agyWireServer {
	t.Helper()
	wm, err := workspace.NewWorkspaceManager(e.stateDir, e.wsBase)
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}
	exec := &agyWireExecutor{inner: execpolicy.New()}
	src := agy.NewAgyTurnLaunchSource(store, wm, e.policy, e.fx.SealedImage)
	identity := agyWireIdentity{}
	adp, err := agy.NewFixtureScopedAdapter(store, exec, src, wm, e.policy, e.digest, e.fx.SealedImage,
		identity, &agyRequiredToolsSource{store: store}, agy.FixtureMode{})
	if err != nil {
		t.Fatalf("fixture-scoped adapter: %v", err)
	}
	srv, err := newServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, adp, wm, exec)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	// Pre-allocate exactly what the birth path derives (same run
	// provenance) so the scenario can be staged in the child's cwd.
	paths, ok := wm.GetPaths(agyWireRunID, agyWireSession)
	if !ok {
		paths, err = wm.AllocateWorkspace(agyWireRunID, agyWireSession, e.profile.WorkspaceMode,
			"example/repo", "0123456789012345678901234567890123456789")
		if err != nil {
			t.Fatalf("allocate workspace: %v", err)
		}
	}
	return &agyWireServer{srv: srv, adp: adp, wm: wm, root: paths.Root}
}

type agyWireIdentity struct{}

func (agyWireIdentity) AttemptFor(_ context.Context, ref adapter.TurnRef) (string, bool) {
	return "att-" + ref.TurnKey, true
}

// stage writes the fixture scenario into the allocation (the child's
// cwd): the frozen tool inventory first, then lines.
func (w *agyWireServer) stage(t *testing.T, lines ...string) {
	t.Helper()
	tools, _ := json.Marshal(map[string]any{"tools": agyWireTools})
	all := append([]string{string(tools)}, lines...)
	if err := os.WriteFile(filepath.Join(w.root, ".agy-fixture-scenario.jsonl"), []byte(strings.Join(all, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("stage scenario: %v", err)
	}
}

// creationLaunches counts stream-json (creation/turn) child launches in
// the allocation; gate children log elsewhere.
func (w *agyWireServer) creationLaunches() int {
	raw, err := os.ReadFile(filepath.Join(w.root, ".agy-fixture-args"))
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

// agyCoveringAttestation covers agyWireTools exactly: one sibling_read
// for view_file (read class), the five self_mutation operations for
// write_to_file (bash_absolute), and the single denied_actions approval.
func (e *agyWireEnv) coveringAttestation(t *testing.T, actor string) codex.ProtectionAttestation {
	t.Helper()
	manifest, err := storage.ComputeToolkitManifestDigest(e.profile.ToolkitManifest.ToolkitManifest)
	if err != nil {
		t.Fatalf("manifest digest: %v", err)
	}
	rec := func(class codex.RecordClass, tc codex.ToolClass, name string, op codex.Operation) codex.ProbeRecord {
		return codex.ProbeRecord{Class: class, ToolClass: tc, ToolName: name, Operation: op, Denied: true,
			EnforcingCapability: codex.CapSandboxRestrictedFS, DenialText: "denied: " + name}
	}
	att := codex.ProtectionAttestation{
		CodexVersion: e.policy.CLIVersion, PlatformOS: e.policy.PlatformOS, PlatformFamily: e.policy.PlatformFamily,
		ManifestDigest: manifest, ProfileDigest: e.digest,
		ProbedAt: "2026-09-24T00:00:00Z", Actor: actor,
	}
	att.ProbeRecords = append(att.ProbeRecords, rec(codex.RecordSiblingRead, codex.ToolRead, "view_file", codex.OpRead))
	for _, op := range []codex.Operation{codex.OpWrite, codex.OpAppend, codex.OpTruncate, codex.OpRename, codex.OpDelete} {
		att.ProbeRecords = append(att.ProbeRecords, rec(codex.RecordSelfMutation, codex.ToolBashAbsolute, "write_to_file", op))
	}
	att.ApprovalDenies = append(att.ApprovalDenies, codex.ApprovalDenyRecord{MethodName: "denied_actions", RefusalKind: codex.RefusalNativeEnum})
	return att
}

func (e *agyWireEnv) recordAttestationDirect(t *testing.T, store *storage.Store) string {
	t.Helper()
	att := e.coveringAttestation(t, "operator-test")
	id, err := att.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	frame, err := att.EncodeProbeRecords()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := store.RecordAgyProtectionAttestation(context.Background(), "op-att-direct", storage.AgyProtectionAttestationRecord{
		AttestationID: id, RunID: agyWireRunID, AgyVersion: e.policy.CLIVersion, Platform: att.PlatformIdentity(),
		ManifestDigest: att.ManifestDigest, ProfileDigest: e.digest, ProbeResults: string(frame),
		ProbedAt: att.ProbedAt, Actor: att.Actor,
	}); err != nil {
		t.Fatalf("record attestation: %v", err)
	}
	return id
}

func constructionProbeRan(scratch string) bool {
	_, err := os.Stat(filepath.Join(scratch, "agy-toolkit-probe", ".agy-fixture-gate-args"))
	return err == nil
}

// ── Configuration: fail closed ──────────────────────────────────────────

func TestServiceWiring_AgyConfigurationFailClosed(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	e.recordAttestationDirect(t, store)
	otherHome := t.TempDir()

	cases := map[string]struct {
		mutate func(*ServerConfig)
		want   string
	}{
		"profile":            {func(c *ServerConfig) { c.AgyProfile = storage.CanonicalProfile{} }, "AgyProfile"},
		"evidence root":      {func(c *ServerConfig) { c.AgyEvidenceRoot = "" }, "AgyEvidenceRoot"},
		"evidence missing":   {func(c *ServerConfig) { c.AgyEvidenceRoot = filepath.Join(otherHome, "absent") }, "evidence root"},
		"home dir":           {func(c *ServerConfig) { c.AgyHomeDir = "" }, "AgyHomeDir"},
		"home dir relative":  {func(c *ServerConfig) { c.AgyHomeDir = "home" }, "AgyHomeDir"},
		"scratch root":       {func(c *ServerConfig) { c.AgyScratchRoot = "" }, "AgyScratchRoot"},
		"scratch in state":   {func(c *ServerConfig) { c.AgyScratchRoot = filepath.Join(c.StateDir, "agy") }, "StateDir"},
		"binary not pinned":  {func(c *ServerConfig) { c.AgyBinaryPath = filepath.Join(otherHome, "agy") }, "binary_path"},
		"with claude":        {func(c *ServerConfig) { c.ClaudeBinaryPath = "/usr/bin/claude" }, "only one persistent contributor adapter"},
		"with codex":         {func(c *ServerConfig) { c.CodexBinaryPath = "/usr/bin/codex" }, "only one persistent contributor adapter"},
		"with opencode":      {func(c *ServerConfig) { c.OpenCodeBinaryPath = "/usr/bin/opencode" }, "only one persistent contributor adapter"},
		"home not the owner": {func(c *ServerConfig) { c.AgyHomeDir = otherHome }, "expected_home"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := e.cfg
			tc.mutate(&cfg)
			_, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), cfg, nil)
			if err == nil {
				t.Fatalf("construction must fail closed")
			}
			if name == "home not the owner" && runtime.GOOS == "linux" {
				var mismatch *agy.ErrHomeDirMismatch
				if !errors.As(err, &mismatch) {
					t.Fatalf("want ErrHomeDirMismatch, got %T: %v", err, err)
				}
			} else if runtime.GOOS == "linux" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
			if constructionProbeRan(e.scratch) {
				t.Fatal("no child may start for a refused configuration")
			}
		})
	}
}

// §3.12: a frozen platform other than linux/unix, or a non-Linux host,
// is refused typed at construction, before any child.
func TestServiceWiring_AgyConstructionRefusesNonLinuxPlatform(t *testing.T) {
	for _, pl := range []storage.AgyPlatformSpec{{OS: "darwin", Family: "unix"}, {OS: "windows", Family: "windows"}, {OS: "linux", Family: "bsd"}} {
		t.Run(pl.OS+"/"+pl.Family, func(t *testing.T) {
			e := newAgyWireEnv(t, func(p *storage.CanonicalProfile) { p.Harnesses["agy"].Agy.Platform = pl })
			store := e.openStore(t)
			_, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, nil)
			var unsupported *agy.ErrUnsupportedProfile
			if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "linux") {
				t.Fatalf("want the typed platform refusal, got %T: %v", err, err)
			}
			if constructionProbeRan(e.scratch) {
				t.Fatal("no child may start for a refused platform")
			}
		})
	}
	if runtime.GOOS != "linux" {
		e := newAgyWireEnv(t, func(p *storage.CanonicalProfile) {
			p.Harnesses["agy"].Agy.Platform = storage.AgyPlatformSpec{OS: "linux", Family: "unix"}
		})
		_, err := NewServerWithAdapter(e.openStore(t), mustLock(t, e.stateDir), e.cfg, nil)
		var unsupported *agy.ErrUnsupportedProfile
		if !errors.As(err, &unsupported) {
			t.Fatalf("a non-Linux host is refused typed, got %T: %v", err, err)
		}
	}
}

// Production construction end to end (Linux): without a covering row
// the eligibility gate refuses before any child; with one the adapter is
// wired, the construction `plugin list` ran once in its scratch scope.
func TestServiceWiring_AgyProductionConstruction(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production agy construction is Linux-only (spec §3.12)")
	}
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)

	t.Run("ineligible", func(t *testing.T) {
		_, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, nil)
		var ne *agy.ErrNotEligible
		if !errors.As(err, &ne) {
			t.Fatalf("without an attestation construction is ineligible, got %T: %v", err, err)
		}
		if constructionProbeRan(e.scratch) {
			t.Fatal("eligibility fails closed before any child")
		}
	})

	e.recordAttestationDirect(t, store)
	t.Run("eligible", func(t *testing.T) {
		srv, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, nil)
		if err != nil {
			t.Fatalf("construction: %v", err)
		}
		if _, ok := srv.adapter.(*agy.AgyAdapter); !ok {
			t.Fatalf("the production agy adapter must be wired, got %T", srv.adapter)
		}
		if !constructionProbeRan(e.scratch) {
			t.Fatal("the construction-scoped plugin list must run")
		}
	})
}

// ── Attestation: operator authority, run-bound tuple, coverage ──────────

func TestServiceAttestation_AgyAuthorityIdempotencyCoverage(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	ctx := context.Background()
	valid := func() AgyProbeAttestationRequest {
		return AgyProbeAttestationRequest{OpID: "op-agy-att", OperatorToken: e.cfg.AuthToken, Actor: "operator-test",
			RunID: agyWireRunID, Attestation: e.coveringAttestation(t, "operator-test")}
	}
	rows := func() int {
		var n int
		if err := store.DB().QueryRow(`SELECT count(*) FROM agy_protection_attestations`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	refusals := map[string]func(*AgyProbeAttestationRequest){
		"no op id":            func(r *AgyProbeAttestationRequest) { r.OpID = "" },
		"no credential":       func(r *AgyProbeAttestationRequest) { r.OperatorToken = "" },
		"wrong credential":    func(r *AgyProbeAttestationRequest) { r.OperatorToken = "not-the-token" },
		"no actor":            func(r *AgyProbeAttestationRequest) { r.Actor = "" },
		"actor mismatch":      func(r *AgyProbeAttestationRequest) { r.Actor = "someone-else" },
		"no run":              func(r *AgyProbeAttestationRequest) { r.RunID = "" },
		"unknown run":         func(r *AgyProbeAttestationRequest) { r.RunID = "run-absent" },
		"missing mapped tool": func(r *AgyProbeAttestationRequest) { r.Attestation.ProbeRecords = r.Attestation.ProbeRecords[1:] },
		"extra record": func(r *AgyProbeAttestationRequest) {
			r.Attestation.ProbeRecords = append(r.Attestation.ProbeRecords, codex.ProbeRecord{Class: codex.RecordSiblingRead,
				ToolClass: codex.ToolGrep, ToolName: "grep_search", Operation: codex.OpRead, Denied: true,
				EnforcingCapability: codex.CapSandboxRestrictedFS, DenialText: "denied"})
		},
		"version mismatch":  func(r *AgyProbeAttestationRequest) { r.Attestation.CodexVersion = "1.2.8" },
		"platform mismatch": func(r *AgyProbeAttestationRequest) { r.Attestation.PlatformOS = "plan9" },
		"manifest mismatch": func(r *AgyProbeAttestationRequest) {
			r.Attestation.ManifestDigest = "sha256:" + strings.Repeat("0", 64)
		},
		"profile mismatch": func(r *AgyProbeAttestationRequest) {
			r.Attestation.ProfileDigest = "cprof-v4:sha256:" + strings.Repeat("0", 64)
		},
		"id mismatch": func(r *AgyProbeAttestationRequest) {
			r.AttestationID = "cprot-v2:sha256:" + strings.Repeat("0", 64)
		},
	}
	for name, mutate := range refusals {
		t.Run(name, func(t *testing.T) {
			req := valid()
			mutate(&req)
			if _, err := w.srv.RecordAgyProbeAttestation(ctx, req); err == nil {
				t.Fatalf("%s must be refused", name)
			}
			if n := rows(); n != 0 {
				t.Fatalf("a refused attestation writes nothing, rows=%d", n)
			}
		})
	}

	req := valid()
	wantID, err := req.Attestation.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	req.AttestationID = wantID
	receipt, err := w.srv.RecordAgyProbeAttestation(ctx, req)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if receipt.CommandType != "record_agy_attestation" || receipt.Payload != wantID {
		t.Fatalf("receipt must carry the recomputed attestation id, got %+v", receipt)
	}
	// The row is bound to the tuple DERIVED from the run's frozen profile.
	manifest, _ := storage.ComputeToolkitManifestDigest(e.profile.ToolkitManifest.ToolkitManifest)
	found, err := store.FindAgyProtectionAttestation(ctx, e.policy.CLIVersion, req.Attestation.PlatformIdentity(), manifest, e.digest)
	if err != nil || found != wantID {
		t.Fatalf("the row must be found by the frozen tuple, got %q err=%v", found, err)
	}
	var journalRun string
	if err := store.DB().QueryRow(`SELECT run_id FROM journal_entries WHERE op_id = ?`, "op-agy-att").Scan(&journalRun); err != nil || journalRun != agyWireRunID {
		t.Fatalf("the journal entry must carry the run provenance, got %q err=%v", journalRun, err)
	}
	replay, err := w.srv.RecordAgyProbeAttestation(ctx, req)
	if err != nil || replay.OpID != receipt.OpID || replay.Payload != receipt.Payload {
		t.Fatalf("replay by op id returns the committed receipt, got %+v err=%v", replay, err)
	}
	req.OpID = "op-agy-att-dup"
	if _, err := w.srv.RecordAgyProbeAttestation(ctx, req); err == nil {
		t.Fatal("the same evidence under a new op id must be refused")
	}
	if n := rows(); n != 1 {
		t.Fatalf("exactly one row, got %d", n)
	}

	// Without an agy wiring nothing can be validated or recorded.
	unwired := &Server{store: store, cfg: ServerConfig{AuthToken: e.cfg.AuthToken}}
	if _, err := unwired.RecordAgyProbeAttestation(ctx, valid()); err == nil || !strings.Contains(err.Error(), "no agy adapter is wired") {
		t.Fatalf("an unwired service refuses, got %v", err)
	}
}

// ── Derived birth ───────────────────────────────────────────────────────

func TestServiceAgySession_BirthDerivesModelWorkspaceAndDigest(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	ctx := context.Background()
	w.stage(t, `{"conversation_id": "`+agyWireNativeID+`"}`)

	// A foreign credential is refused before any child.
	if _, _, err := w.srv.CreateAgySession(ctx, "op-create-foreign", "lease-foreign", agyWireSession); err == nil ||
		!strings.Contains(err.Error(), "controller authority") {
		t.Fatalf("a foreign lease must be refused, got %v", err)
	}
	// A non-agy session is refused.
	if _, _, err := w.srv.CreateAgySession(ctx, "op-create-claude", agyWireLease, "sess-ac010-claude"); err == nil ||
		!strings.Contains(err.Error(), "not agy") {
		t.Fatalf("a non-agy session must be refused, got %v", err)
	}
	if n := w.creationLaunches(); n != 0 {
		t.Fatalf("no creation child for a refused birth, launches=%d", n)
	}

	binding, receipt, err := w.srv.CreateAgySession(ctx, "op-create-agy", agyWireLease, agyWireSession)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if binding.NativeSessionID != agyWireNativeID || receipt.CommandType != "bind_agy_session" || receipt.Payload != agyWireNativeID {
		t.Fatalf("binding/receipt mismatch: %+v %+v", binding, receipt)
	}
	stored, err := store.GetAgySessionBinding(ctx, agyWireSession)
	if err != nil || stored == nil {
		t.Fatalf("binding must be persisted: %+v err=%v", stored, err)
	}
	if stored.Model != agyWireModel || stored.Workspace != w.root || stored.ProfileDigest != e.digest || stored.Materialized {
		t.Fatalf("binding must carry the derived model/workspace/digest, unmaterialized; got %+v (root %s)", stored, w.root)
	}
	if binding.Config.Model != agyWireModel || binding.Config.WorkspaceRoot != w.root {
		t.Fatalf("the native conversation must be created with the derived config, got %+v", binding.Config)
	}
	replay, replayReceipt, err := w.srv.CreateAgySession(ctx, "op-create-agy", agyWireLease, agyWireSession)
	if err != nil || replayReceipt.OpID != receipt.OpID || replay.NativeSessionID != agyWireNativeID {
		t.Fatalf("replay returns the committed receipt, got %+v %+v err=%v", replay, replayReceipt, err)
	}
	if n := w.creationLaunches(); n != 1 {
		t.Fatalf("exactly one creation child, launches=%d", n)
	}
}

// A run frozen on a profile other than the wired one is refused at
// birth before any child: the adapter could never dispatch it.
func TestServiceAgySession_BirthRefusesRunProfileNotWiredProfile(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	runProfile := e.profile
	runProfile.Harnesses = map[string]storage.HarnessProfileSpec{}
	for k, v := range e.profile.Harnesses {
		runProfile.Harnesses[k] = v
	}
	spec := runProfile.Harnesses["agy"]
	spec.Model = "gemini-3.1-pro-low"
	runProfile.Harnesses["agy"] = spec
	e.seed(t, store, runProfile)
	w := e.fixtureServer(t, store)
	w.stage(t, `{"conversation_id": "`+agyWireNativeID+`"}`)
	_, _, err := w.srv.CreateAgySession(context.Background(), "op-create-mismatch", agyWireLease, agyWireSession)
	if err == nil || !strings.Contains(err.Error(), "is not the profile this service's agy adapter froze to") {
		t.Fatalf("a run frozen on another profile must be refused at birth, got %v", err)
	}
	if n := w.creationLaunches(); n != 0 {
		t.Fatalf("no child for a refused birth, launches=%d", n)
	}
	if b, _ := store.GetAgySessionBinding(context.Background(), agyWireSession); b != nil {
		t.Fatal("no binding for a refused birth")
	}
}

// A lease superseded while the native creation is in flight cannot
// publish the binding: the created conversation is recorded durably as
// the orphan of a creation-uncertainty episode, which blocks recreation
// until the NEW controller resolves it.
func TestServiceAgySession_SupersededLeaseDuringCreationRecordsOrphan(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	ctx := context.Background()
	w.stage(t, `{"conversation_id": "`+agyWireNativeID+`"}`, `{"slow_init_ms": 1500}`)

	type outcome struct {
		binding adapter.SessionBinding
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		b, _, err := w.srv.CreateAgySession(ctx, "op-create-old", agyWireLease, agyWireSession)
		done <- outcome{b, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for w.creationLaunches() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the creation child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	grant, err := store.HandoffController(ctx, "op-handoff-ac010", agyWireRunID, agyWireLease, 1, "agy", "ctrl-ref-new", "lease-ac010-gen2")
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	old := <-done
	if old.err == nil || !errors.Is(old.err, storage.ErrLeaseSuperseded) || !strings.Contains(old.err.Error(), "persist binding") {
		t.Fatalf("the superseded controller's bind must be refused by the in-transaction authority check, got %v", old.err)
	}
	if b, _ := store.GetAgySessionBinding(ctx, agyWireSession); b != nil {
		t.Fatalf("nothing may be bound for the superseded controller, got %+v", b)
	}
	ep, err := store.OpenAgyCreationUncertainty(ctx, agyWireSession)
	if err != nil || ep == nil || ep.OrphanNativeID == nil || *ep.OrphanNativeID != agyWireNativeID {
		t.Fatalf("the created conversation must be recorded as an orphan episode, got %+v err=%v", ep, err)
	}

	// The new controller is blocked (durably) until it resolves.
	w.stage(t, `{"conversation_id": "`+agyWireOtherID+`"}`)
	_, _, err = w.srv.CreateAgySession(ctx, "op-create-new", grant.LeaseSecret, agyWireSession)
	var unc *adapter.ErrSessionCreationUncertain
	if !errors.As(err, &unc) {
		t.Fatalf("an open episode blocks recreation, got %T: %v", err, err)
	}
	if n := w.creationLaunches(); n != 1 {
		t.Fatalf("no second creation child while the episode is open, launches=%d", n)
	}
	if _, err := w.srv.ResolveAgySessionCreationUncertainty(ctx, AgyCreationUncertaintyResolution{
		OpID: "op-resolve-ac010", ControllerLease: grant.LeaseSecret, ExpectedGeneration: grant.Generation,
		SessionID: agyWireSession, Episode: ep.Episode, Disposition: storage.AgyUncertaintyAbandonOrphan,
		Reason: "orphan of the superseded controller's creation",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	binding, _, err := w.srv.CreateAgySession(ctx, "op-create-new-2", grant.LeaseSecret, agyWireSession)
	if err != nil || binding.NativeSessionID != agyWireOtherID {
		t.Fatalf("after resolution the current controller creates a fresh conversation, got %+v err=%v", binding, err)
	}
}

// ── Uncertain creation: durable episode, restart block, resolution ─────

func TestServiceAgySession_UncertainCreationEpisodeAndResolution(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seed(t, store, e.profile)
	w := e.fixtureServer(t, store)
	ctx := context.Background()
	// A non-UUIDv4 init id: the creation ends UNCERTAIN in the adapter
	// (in-memory tombstone only); the SERVICE opens the durable episode.
	w.stage(t, `{"conversation_id": "not-a-uuid"}`)
	_, _, err := w.srv.CreateAgySession(ctx, "op-create-unc", agyWireLease, agyWireSession)
	var unc *adapter.ErrSessionCreationUncertain
	if !errors.As(err, &unc) {
		t.Fatalf("want the typed uncertain creation, got %T: %v", err, err)
	}
	ep, err := store.OpenAgyCreationUncertainty(ctx, agyWireSession)
	if err != nil || ep == nil {
		t.Fatalf("the service must record the durable episode, got %+v err=%v", ep, err)
	}
	if ep.RunID != agyWireRunID || ep.CauseOpID != "op-create-unc" || ep.OrphanNativeID == nil || *ep.OrphanNativeID != "not-a-uuid" {
		t.Fatalf("episode provenance (run, cause op, observed id) must be recorded, got %+v", ep)
	}

	// A restarted service (fresh adapter, no tombstone) is blocked by
	// the durable episode before any child.
	w.stage(t, `{"conversation_id": "`+agyWireNativeID+`"}`)
	before := w.creationLaunches()
	w2 := e.fixtureServerSharing(t, store, w)
	if _, _, err := w2.srv.CreateAgySession(ctx, "op-create-after-restart", agyWireLease, agyWireSession); !errors.As(err, &unc) ||
		!strings.Contains(err.Error(), fmt.Sprintf("episode %d", ep.Episode)) {
		t.Fatalf("the durable episode blocks a restarted service, got %v", err)
	}
	if n := w.creationLaunches(); n != before {
		t.Fatalf("no child while the episode is open, launches %d -> %d", before, n)
	}

	base := AgyCreationUncertaintyResolution{
		OpID: "op-resolve-unc", ControllerLease: agyWireLease, ExpectedGeneration: 1,
		SessionID: agyWireSession, Episode: ep.Episode, Disposition: storage.AgyUncertaintyVerifiedAbsent,
		Reason: "operator verified no conversation file exists",
	}
	for name, mutate := range map[string]func(*AgyCreationUncertaintyResolution){
		"no op id":        func(r *AgyCreationUncertaintyResolution) { r.OpID = "" },
		"no lease":        func(r *AgyCreationUncertaintyResolution) { r.ControllerLease = "" },
		"foreign lease":   func(r *AgyCreationUncertaintyResolution) { r.ControllerLease = "lease-foreign" },
		"stale gen":       func(r *AgyCreationUncertaintyResolution) { r.ExpectedGeneration = 7 },
		"unknown episode": func(r *AgyCreationUncertaintyResolution) { r.Episode = ep.Episode + 1 },
		"bad disposition": func(r *AgyCreationUncertaintyResolution) { r.Disposition = "forget_it" },
		"no reason":       func(r *AgyCreationUncertaintyResolution) { r.Reason = "" },
	} {
		req := base
		req.OpID = base.OpID + "-" + strings.ReplaceAll(name, " ", "-")
		mutate(&req)
		if _, err := w2.srv.ResolveAgySessionCreationUncertainty(ctx, req); err == nil {
			t.Fatalf("%s: resolution must be refused", name)
		}
	}
	if open, _ := store.HasAgyCreationUncertainty(ctx, agyWireSession); !open {
		t.Fatal("refused resolutions leave the episode open")
	}

	// The durable resolution also clears the in-memory tombstone of the
	// adapter that saw the uncertain creation.
	receipt, err := w.srv.ResolveAgySessionCreationUncertainty(ctx, base)
	if err != nil || receipt.Payload != storage.AgyUncertaintyVerifiedAbsent {
		t.Fatalf("resolve: %+v err=%v", receipt, err)
	}
	if w.adp.ResolveCreationUncertainty(agyWireSession) {
		t.Fatal("the service resolution must already have cleared the adapter tombstone")
	}
	if replay, err := w.srv.ResolveAgySessionCreationUncertainty(ctx, base); err != nil || replay.OpID != receipt.OpID {
		t.Fatalf("resolution replay by op id, got %+v err=%v", replay, err)
	}
	episodes, err := store.AgyCreationUncertaintyEpisodes(ctx, agyWireSession)
	if err != nil || len(episodes) != 1 || episodes[0].Disposition == nil {
		t.Fatalf("the resolved episode is kept as evidence, got %+v err=%v", episodes, err)
	}
	binding, _, err := w.srv.CreateAgySession(ctx, "op-create-after-resolve", agyWireLease, agyWireSession)
	if err != nil || binding.NativeSessionID != agyWireNativeID {
		t.Fatalf("after resolution creation proceeds, got %+v err=%v", binding, err)
	}
}

// fixtureServerSharing builds a second service instance ("restart")
// with a fresh adapter over the same store and allocation.
func (e *agyWireEnv) fixtureServerSharing(t *testing.T, store *storage.Store, prev *agyWireServer) *agyWireServer {
	t.Helper()
	exec := &agyWireExecutor{inner: execpolicy.New()}
	src := agy.NewAgyTurnLaunchSource(store, prev.wm, e.policy, e.fx.SealedImage)
	adp, err := agy.NewFixtureScopedAdapter(store, exec, src, prev.wm, e.policy, e.digest, e.fx.SealedImage,
		agyWireIdentity{}, &agyRequiredToolsSource{store: store}, agy.FixtureMode{})
	if err != nil {
		t.Fatalf("fixture-scoped adapter: %v", err)
	}
	// Same state dir: the first instance's lock is released first.
	prev.srv.lock.Release()
	srv, err := newServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, adp, prev.wm, exec)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	return &agyWireServer{srv: srv, adp: adp, wm: prev.wm, root: prev.root}
}

// ── Queue-time required_tools ───────────────────────────────────────────

func TestServiceQueue_AgyRequiredToolsValidatedAtQueueTime(t *testing.T) {
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	cfg := e.cfg
	cfg.AgyBinaryPath = "" // the queue path needs no native adapter
	srv, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), cfg, nil)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	e.seedAdopted(t, store, e.profile)
	bridge := &acceptanceBridge{t: t, client: newTestClient(srv.SocketPath()), token: cfg.AuthToken}
	ctx := context.Background()
	if code, resp := bridge.do("POST", "/v1/runs/"+agyWireRunID+"/controller/connect", fmt.Sprintf(
		`{"op_id": "op-conn-ac010-http", "controller_lease": %q, "expected_generation": 1, "instance_id": %q}`,
		agyWireLease, srv.InstanceID())); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("connect: %d %v", code, resp)
	}

	queue := func(session, turnKey string, tools any) (int, map[string]any) {
		t.Helper()
		ver, err := store.GetSessionVersion(ctx, session)
		if err != nil {
			t.Fatalf("version: %v", err)
		}
		body := map[string]any{"instance_id": srv.InstanceID(), "op_id": "op-q-" + turnKey, "controller_lease": agyWireLease,
			"expected_version": ver, "turn_key": turnKey, "prompt": "review " + turnKey}
		if tools != nil {
			body["required_tools"] = tools
		}
		raw, _ := json.Marshal(body)
		return bridge.do("POST", "/v1/runs/"+agyWireRunID+"/sessions/"+session+"/prompts/queue", string(raw))
	}
	storedTools := func(session, turnKey string) string {
		t.Helper()
		var js string
		if err := store.DB().QueryRow(`SELECT required_tools_json FROM pending_prompts WHERE session_id = ? AND turn_key = ?`,
			session, turnKey).Scan(&js); err != nil {
			t.Fatalf("pending row %s/%s: %v", session, turnKey, err)
		}
		return js
	}
	errCode := func(resp map[string]any) (string, string) {
		env, _ := resp["error"].(map[string]any)
		code, _ := env["code"].(string)
		msg, _ := env["message"].(string)
		return code, msg
	}

	if code, resp := queue(agyWireSession, "t-ok", []string{"write_to_file", "view_file"}); code != http.StatusOK {
		t.Fatalf("a subset of expected_tools is accepted, got %d %v", code, resp)
	}
	if got := storedTools(agyWireSession, "t-ok"); got != `["write_to_file","view_file"]` {
		t.Fatalf("the set is journaled with the prompt in order, got %s", got)
	}
	code, resp := queue(agyWireSession, "t-unknown", []string{"view_file", "run_command", "bogus"})
	if c, msg := errCode(resp); code != http.StatusBadRequest || c != "invalid_required_tools" ||
		!strings.Contains(msg, "run_command") || !strings.Contains(msg, "bogus") || strings.Contains(msg, "view_file,") {
		t.Fatalf("unknown names are refused 400 and listed, got %d %v", code, resp)
	}
	code, resp = queue(agyWireSession, "t-dup", []string{"view_file", "view_file"})
	if c, _ := errCode(resp); code != http.StatusBadRequest || c != "invalid_required_tools" {
		t.Fatalf("duplicates are refused 400, got %d %v", code, resp)
	}
	code, resp = queue(agyWireSession, "t-empty-name", []string{""})
	if c, _ := errCode(resp); code != http.StatusBadRequest || c != "invalid_required_tools" {
		t.Fatalf("an empty name is refused 400, got %d %v", code, resp)
	}
	var pending int
	_ = store.DB().QueryRow(`SELECT count(*) FROM pending_prompts WHERE turn_key IN ('t-unknown','t-dup','t-empty-name')`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("a refused queue writes nothing, rows=%d", pending)
	}
	if code, resp := queue(agyWireSession, "t-absent", nil); code != http.StatusOK {
		t.Fatalf("absent required_tools is accepted (defaults apply at dispatch), got %d %v", code, resp)
	}
	if got := storedTools(agyWireSession, "t-absent"); got != "[]" {
		t.Fatalf("absent is journaled empty, got %s", got)
	}
	code, resp = queue("sess-ac010-claude", "t-claude", []string{"view_file"})
	if c, _ := errCode(resp); code != http.StatusBadRequest || c != "invalid_required_tools" {
		t.Fatalf("required_tools on a non-agy session is refused 400, got %d %v", code, resp)
	}
	if code, resp := queue("sess-ac010-claude", "t-claude-plain", nil); code != http.StatusOK {
		t.Fatalf("a non-agy queue without required_tools is unchanged, got %d %v", code, resp)
	}

	// The adapter's RequiredToolsFor seam reads the journaled set from
	// the dispatch intent; an absent set reports ok=false.
	src := &agyRequiredToolsSource{store: store}
	for _, rel := range []struct{ session, key string }{{agyWireSession, "t-ok"}, {"sess-ac010-claude", "t-claude-plain"}} {
		ver, _ := store.GetSessionVersion(ctx, rel.session)
		if _, err := store.ReleaseTurn(ctx, "op-rel-"+rel.key, agyWireLease, rel.session, ver, rel.key); err != nil {
			t.Fatalf("release %s: %v", rel.key, err)
		}
	}
	tools, ok := src.RequiredToolsFor(ctx, adapter.TurnRef{SessionID: agyWireSession, TurnKey: "t-ok"})
	if !ok || len(tools) != 2 || tools[0] != "write_to_file" || tools[1] != "view_file" {
		t.Fatalf("the seam returns the journaled set, got %v ok=%v", tools, ok)
	}
	if tools, ok := src.RequiredToolsFor(ctx, adapter.TurnRef{SessionID: "sess-ac010-claude", TurnKey: "t-claude-plain"}); ok {
		t.Fatalf("an absent set reports ok=false (frozen defaults), got %v", tools)
	}
	if _, ok := src.RequiredToolsFor(ctx, adapter.TurnRef{SessionID: agyWireSession, TurnKey: "t-never"}); ok {
		t.Fatal("no dispatch intent reports ok=false")
	}
}
