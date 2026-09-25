package agy

// Production guard for the agy test-only construction mode (AC-010,
// mirroring internal/adapter/codex/production_guard_test.go): the
// production option-decoding seam fails closed with a typed error when
// handed the fixture option, independent of any other state.
// NewAgyAdapter routes through applyConstructionOptions, so this guard
// protects the real production constructor; the tests exercise the seam
// directly (package-internal: applyConstructionOptions is unexported on
// purpose).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestProductionGuard_ApplyConstructionOptionsRejectsFixtureOption(t *testing.T) {
	settings, err := applyConstructionOptions(FixtureOption(FixtureMode{}))
	var prohibited *ErrFixtureModeProhibited
	if !errors.As(err, &prohibited) {
		t.Fatalf("expected *ErrFixtureModeProhibited, got %T: %v", err, err)
	}
	if settings.fixtureScope {
		t.Fatalf("rejected settings must not carry fixtureScope=true, got %+v", settings)
	}
}

func TestProductionGuard_ApplyConstructionOptionsWithoutOptionsSucceeds(t *testing.T) {
	settings, err := applyConstructionOptions()
	if err != nil {
		t.Fatalf("no options must succeed, got: %v", err)
	}
	if settings.fixtureScope {
		t.Fatalf("no options must not set fixtureScope, got %+v", settings)
	}
}

func TestProductionGuard_ApplyConstructionOptionsIgnoresNilOption(t *testing.T) {
	settings, err := applyConstructionOptions(nil)
	if err != nil {
		t.Fatalf("nil option must not error, got: %v", err)
	}
	if settings.fixtureScope {
		t.Fatalf("nil option must not set fixtureScope, got %+v", settings)
	}
}

// ── NewProductionAgyAdapter guards (AC-010 Task 6) ──────────────────────

// refusingExecutor records any Start: the production guards below must
// fail closed BEFORE any child exists.
type refusingExecutor struct{ starts atomic.Int32 }

func (e *refusingExecutor) Start(context.Context, execpolicy.LaunchRequest) (execpolicy.ManagedProcess, error) {
	e.starts.Add(1)
	return nil, errors.New("production guard: no child may start")
}

type prodGuardFixture struct {
	store         *storage.Store
	wm            *workspace.WorkspaceManager
	profile       storage.CanonicalProfile
	policy        AgyLaunchPolicy
	profileDigest string
	evidenceRoot  string
	homeDir       string
	scratch       string
}

// newProdGuardFixture stages an accepted cprof-v4 profile whose pinned
// "binary" is an inert temp file (never executed: every guard refuses
// before a launch), a store holding the run, and a temp operator home.
func newProdGuardFixture(t *testing.T) *prodGuardFixture {
	t.Helper()
	profile, evidenceRoot := acceptedProfileAndRoot(t)
	scratch := t.TempDir()
	bin := filepath.Join(scratch, "bin", "agy")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	binRaw := []byte("inert agy stand-in; never executed\n")
	if err := os.WriteFile(bin, binRaw, 0o700); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(scratch, "home")
	a := profile.Harnesses["agy"].Agy
	a.BinaryPath = filepath.ToSlash(bin)
	a.BinaryDigest = digestOf(binRaw)
	a.ExpectedHome = filepath.ToSlash(filepath.Join(homeDir, ".gemini"))
	policy, err := ValidateAgyHarness(profile, evidenceRoot)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	digest, _, err := storage.ComputeProfileDigest(profile)
	if err != nil {
		t.Fatalf("profile digest: %v", err)
	}
	store, err := storage.Open(storage.StoreOptions{StateDir: filepath.Join(scratch, "state")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.CreateRunWithProfile(context.Background(), storage.CreateRunWithProfileRequest{
		OpID: "op-run-agy-guard", ControllerLease: "lease-guard", RunID: "run-agy-guard",
		Brief: "agy production guard", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      profile,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	wm, err := workspace.NewWorkspaceManager(filepath.Join(scratch, "wm-state"), filepath.Join(scratch, "ws"))
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}
	return &prodGuardFixture{store: store, wm: wm, profile: profile, policy: policy, profileDigest: digest,
		evidenceRoot: evidenceRoot, homeDir: homeDir, scratch: filepath.Join(scratch, "probe")}
}

// coveringAttestationFor builds the attestation covering cov exactly:
// one sibling_read per expected read path, the five self_mutation ops
// per expected mutation path, and the single denied_actions approval.
func coveringAttestationFor(cov ProtectionCoverage) codex.ProtectionAttestation {
	att := codex.ProtectionAttestation{
		CodexVersion: cov.AgyVersion, PlatformOS: cov.PlatformOS, PlatformFamily: cov.PlatformFamily,
		ProbedAt: "2026-09-24T00:00:00Z", Actor: "operator",
	}
	for _, p := range cov.siblingReads {
		att.ProbeRecords = append(att.ProbeRecords, probe(codex.RecordSiblingRead, p.class, p.name, codex.OpRead))
	}
	for _, p := range cov.mutations {
		for _, op := range allMutationOps {
			att.ProbeRecords = append(att.ProbeRecords, probe(codex.RecordSelfMutation, p.class, p.name, op))
		}
	}
	att.ApprovalDenies = append(att.ApprovalDenies,
		codex.ApprovalDenyRecord{MethodName: approvalDeniedActionsMethod, RefusalKind: codex.RefusalNativeEnum})
	return att
}

// recordAttestation journals a cprot-v2 row for the frozen tuple of
// (policy, profile, profileDigest); mutate may drop records first.
func recordAttestation(t *testing.T, store *storage.Store, runID string, policy AgyLaunchPolicy,
	profile storage.CanonicalProfile, profileDigest string, mutate func(*codex.ProtectionAttestation)) string {
	t.Helper()
	cov, err := ExpectedCoverage(policy)
	if err != nil {
		t.Fatalf("ExpectedCoverage: %v", err)
	}
	manifest, err := storage.ComputeToolkitManifestDigest(profile.ToolkitManifest.ToolkitManifest)
	if err != nil {
		t.Fatalf("manifest digest: %v", err)
	}
	att := coveringAttestationFor(cov)
	att.ManifestDigest = manifest
	att.ProfileDigest = profileDigest
	if mutate != nil {
		mutate(&att)
	}
	id, err := att.Digest()
	if err != nil {
		t.Fatalf("attestation digest: %v", err)
	}
	frame, err := att.EncodeProbeRecords()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := store.RecordAgyProtectionAttestation(context.Background(), "op-att-"+id[len(id)-8:], storage.AgyProtectionAttestationRecord{
		AttestationID: id, RunID: runID, AgyVersion: policy.CLIVersion, Platform: att.PlatformIdentity(),
		ManifestDigest: manifest, ProfileDigest: profileDigest, ProbeResults: string(frame),
		ProbedAt: att.ProbedAt, Actor: att.Actor,
	}); err != nil {
		t.Fatalf("record attestation: %v", err)
	}
	return id
}

func TestProductionGuard_NilAttestationFailsClosedPreChild(t *testing.T) {
	f := newProdGuardFixture(t)
	exec := &refusingExecutor{}
	a, err := NewProductionAgyAdapter(f.store, exec, f.wm, f.profile, f.evidenceRoot, f.homeDir, f.scratch)
	if err == nil || a != nil {
		t.Fatalf("no attestation row must fail production construction, got %v", err)
	}
	if runtime.GOOS == "linux" {
		var ne *ErrNotEligible
		var missing *ErrProductionEligibilityMissing
		if !errors.As(err, &ne) || !errors.As(err, &missing) {
			t.Fatalf("want ErrNotEligible wrapping ErrProductionEligibilityMissing, got %T: %v", err, err)
		}
	}
	if n := exec.starts.Load(); n != 0 {
		t.Fatalf("eligibility fails closed BEFORE any child (plugin list included), launches=%d", n)
	}
}

func TestProductionGuard_UncoveredRowStaysIneligible(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production construction is Linux-only (spec §3.12)")
	}
	f := newProdGuardFixture(t)
	recordAttestation(t, f.store, "run-agy-guard", f.policy, f.profile, f.profileDigest, func(att *codex.ProtectionAttestation) {
		att.ProbeRecords = att.ProbeRecords[1:] // one expected sibling_read missing
	})
	exec := &refusingExecutor{}
	_, err := NewProductionAgyAdapter(f.store, exec, f.wm, f.profile, f.evidenceRoot, f.homeDir, f.scratch)
	var ne *ErrNotEligible
	if !errors.As(err, &ne) {
		t.Fatalf("an uncovered attestation row stays ineligible, got %T: %v", err, err)
	}
	if n := exec.starts.Load(); n != 0 {
		t.Fatalf("no child starts for an ineligible tuple, launches=%d", n)
	}
}

func TestProductionGuard_HomeDirMustBeExpectedHomeParent(t *testing.T) {
	f := newProdGuardFixture(t)
	exec := &refusingExecutor{}
	_, err := NewProductionAgyAdapter(f.store, exec, f.wm, f.profile, f.evidenceRoot, filepath.Join(f.homeDir, "elsewhere"), f.scratch)
	var mismatch *ErrHomeDirMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("a homeDir that is not expected_home's parent fails closed typed, got %T: %v", err, err)
	}
	if n := exec.starts.Load(); n != 0 {
		t.Fatalf("launches=%d", n)
	}
}

func TestProductionGuard_RequiredDependencies(t *testing.T) {
	f := newProdGuardFixture(t)
	exec := &refusingExecutor{}
	for name, call := range map[string]func() (*AgyAdapter, error){
		"store": func() (*AgyAdapter, error) {
			return NewProductionAgyAdapter(nil, exec, f.wm, f.profile, f.evidenceRoot, f.homeDir, f.scratch)
		},
		"executor": func() (*AgyAdapter, error) {
			return NewProductionAgyAdapter(f.store, nil, f.wm, f.profile, f.evidenceRoot, f.homeDir, f.scratch)
		},
		"wm": func() (*AgyAdapter, error) {
			return NewProductionAgyAdapter(f.store, exec, nil, f.profile, f.evidenceRoot, f.homeDir, f.scratch)
		},
		"evidence": func() (*AgyAdapter, error) {
			return NewProductionAgyAdapter(f.store, exec, f.wm, f.profile, "", f.homeDir, f.scratch)
		},
		"scratch": func() (*AgyAdapter, error) {
			return NewProductionAgyAdapter(f.store, exec, f.wm, f.profile, f.evidenceRoot, f.homeDir, "")
		},
		"home": func() (*AgyAdapter, error) {
			return NewProductionAgyAdapter(f.store, exec, f.wm, f.profile, f.evidenceRoot, "", f.scratch)
		},
	} {
		if a, err := call(); err == nil || a != nil {
			t.Fatalf("missing %s must fail construction", name)
		}
	}
	if n := exec.starts.Load(); n != 0 {
		t.Fatalf("launches=%d", n)
	}
}
