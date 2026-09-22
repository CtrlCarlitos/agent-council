//go:build unix

package opencode

// Probe lifecycle evidence (POSIX): the probe runs a real stub serve child
// through the PolicyExecutor and must authenticate both health and model
// calls with the generated probe credentials, call only the health/model
// endpoints, never register the probe child as a contributor server, and
// drain both of the child's output pipes.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// stubProbeTemplate is a test operator-owned template: it allocates its
// own scratch directories and produces the exact approved serve shape.
// keepScratch preserves the scratch directory after the run so tests can
// inspect child-side evidence files; production cleanup is asserted by
// TestProbe_ProductionTemplateEndToEndCleansScratch.
type stubProbeTemplate struct {
	base        string
	keepScratch bool
}

func (t *stubProbeTemplate) VersionLaunch(_ context.Context) (execpolicy.LaunchRequest, error) {
	return execpolicy.LaunchRequest{
		RunID:     "run-probe",
		SessionID: "sess-probe-version",
		Command:   "opencode",
		Args:      []string{"--version"},
		Paths:     workspace.WorkspacePaths{Root: t.base, Config: t.base},
		Profile:   lifecycleProfile(),
	}, nil
}

func (t *stubProbeTemplate) ProbeServeLaunch(_ context.Context) (execpolicy.LaunchRequest, error) {
	var scratch string
	if t.keepScratch {
		// Preserve child-side evidence: Root is a symlink to a target
		// outside Probe's RemoveAll reach (RemoveAll unlinks the symlink,
		// never the target).
		target, err := os.MkdirTemp(t.base, "kept-target-")
		if err != nil {
			return execpolicy.LaunchRequest{}, err
		}
		link := filepath.Join(t.base, "probe-scratch-linked")
		if err := os.Symlink(target, link); err != nil {
			return execpolicy.LaunchRequest{}, err
		}
		_ = os.WriteFile(filepath.Join(t.base, "kept-scratch"), []byte(target), 0o600)
		scratch = link
	} else {
		dir, err := os.MkdirTemp(t.base, "probe-scratch-")
		if err != nil {
			return execpolicy.LaunchRequest{}, err
		}
		scratch = dir
	}
	return execpolicy.LaunchRequest{
		RunID:     "run-probe",
		SessionID: "sess-probe-serve",
		Command:   "opencode",
		Args:      []string{"serve", "--hostname", "127.0.0.1", "--port", "0"},
		Paths:     workspace.WorkspacePaths{Root: scratch, Config: scratch},
		Profile:   lifecycleProfile(),
	}, nil
}

// The probe succeeds end-to-end against a real child: version captured,
// model inventory populated, credentials enforced by the child, only
// health/model endpoints hit, both pipes drained (the stub writes 200KiB
// of stderr before listening), and the probe child never registered as a
// contributor server.
func TestProbe_SuccessWithRealChild(t *testing.T) {
	dir := t.TempDir()
	binDir := compileStubOpencode(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	adp := NewOpenCodeAdapterWithLaunch(execpolicy.New(), &stubProbeTemplate{base: dir, keepScratch: true}, nil, nil)
	report, err := adp.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	if !report.HarnessVersion.Available || !strings.Contains(report.HarnessVersion.Value, "1.18.31-stub") {
		t.Fatalf("probe must report the stub version, got %+v", report.HarnessVersion)
	}
	if !report.ModelInventory.Available {
		t.Fatal("probe must report the model inventory")
	}
	found := false
	for _, m := range report.ModelInventory.Value {
		if m == "stub/model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("model inventory must include stub/model, got %v", report.ModelInventory.Value)
	}

	// The scratch directory is preserved by the fixture so child-side
	// evidence survives Probe's cleanup.
	kept, err := os.ReadFile(filepath.Join(dir, "kept-scratch"))
	if err != nil {
		t.Fatalf("fixture scratch path: %v", err)
	}
	scratch := string(kept)
	if fails := readStubFile(t, scratch, ".stub-authfail"); len(fails) != 0 {
		t.Fatalf("probe calls must authenticate, auth failures: %v", fails)
	}
	for _, hit := range readStubFile(t, scratch, ".stub-hits") {
		if hit != "GET /api/health" && hit != "GET /api/model" {
			t.Fatalf("probe may only call health and model endpoints, saw: %q", hit)
		}
	}
	_ = scratch

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read template base: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "kept-scratch" || strings.HasPrefix(e.Name(), "kept-target-") {
			continue
		}
		t.Fatalf("fixture probe run must leave only preserved evidence, found %q", e.Name())
	}

	// The probe child must never be registered as a contributor server.
	adp.mu.Lock()
	registered := len(adp.servers.children)
	adp.mu.Unlock()
	if registered != 0 {
		t.Fatalf("probe child must never be registered, children=%d", registered)
	}
}

func TestProbe_FailsWhenVersionFails(t *testing.T) {
	dir := t.TempDir()
	binDir := compileStubOpencode(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	adp := NewOpenCodeAdapterWithLaunch(execpolicy.New(), &stubProbeTemplate{base: dir}, nil, nil)
	// Replace the template with one whose version launch cannot resolve.
	adp.probeTemplate = &brokenVersionTemplate{stubProbeTemplate{base: dir}}
	if _, err := adp.Probe(context.Background()); err == nil {
		t.Fatal("probe must fail when the version check fails")
	}
}

type brokenVersionTemplate struct{ stubProbeTemplate }

func (t brokenVersionTemplate) VersionLaunch(_ context.Context) (execpolicy.LaunchRequest, error) {
	return execpolicy.LaunchRequest{
		RunID:     "run-probe",
		SessionID: "sess-probe-version",
		Command:   "opencode-not-on-path",
		Args:      []string{"--version"},
		Paths:     workspace.WorkspacePaths{Root: t.base, Config: t.base},
		Profile:   lifecycleProfile(),
	}, nil
}

// A probe launch that deviates from the exact approved serve shape must be
// rejected before credentials are generated or a process started.
func TestProbe_RejectsNonServeShape(t *testing.T) {
	dir := t.TempDir()
	binDir := compileStubOpencode(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	tpl := &shapeViolationTemplate{stubProbeTemplate{base: dir}}
	adp := NewOpenCodeAdapterWithLaunch(execpolicy.New(), tpl, nil, nil)
	_, err := adp.Probe(context.Background())
	if err == nil {
		t.Fatal("probe must reject a non-exact serve launch shape")
	}
	if !strings.Contains(err.Error(), "opencode serve") {
		t.Fatalf("expected exact serve shape error, got %v", err)
	}
}

type shapeViolationTemplate struct{ stubProbeTemplate }

func (t shapeViolationTemplate) ProbeServeLaunch(_ context.Context) (execpolicy.LaunchRequest, error) {
	scratch, err := os.MkdirTemp(t.base, "probe-scratch-")
	if err != nil {
		return execpolicy.LaunchRequest{}, err
	}
	return execpolicy.LaunchRequest{
		RunID:     "run-probe",
		SessionID: "sess-probe-serve",
		Command:   "opencode",
		Args:      []string{"serve", "--hostname", "0.0.0.0", "--port", "0"},
		Paths:     workspace.WorkspacePaths{Root: scratch, Config: scratch},
		Profile:   lifecycleProfile(),
	}, nil
}

// The exported production template constructor must run a real probe
// end-to-end with an operator-approved profile and must remove the
// template-created scratch directory when Probe finishes.
func TestProbe_ProductionTemplateEndToEndCleansScratch(t *testing.T) {
	dir := t.TempDir()
	binDir := compileStubOpencode(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	scratchRoot := filepath.Join(dir, "operator-scratch")
	if err := os.MkdirAll(scratchRoot, 0o700); err != nil {
		t.Fatalf("mkdir scratch root: %v", err)
	}
	profile := lifecycleProfile()
	profile.Harnesses = map[string]storage.HarnessProfileSpec{
		"opencode": {Model: "stub/model", NativeAuthMode: "managed_by_council"},
	}
	tpl := NewOperatorProbeLaunchTemplate("opencode", scratchRoot, profile)
	adp := NewOpenCodeAdapterWithLaunch(execpolicy.New(), tpl, nil, nil)

	report, err := adp.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe through production template: %v", err)
	}
	if !report.HarnessVersion.Available || !strings.Contains(report.HarnessVersion.Value, "1.18.31-stub") {
		t.Fatalf("probe must capture the stub version, got %+v", report.HarnessVersion)
	}

	entries, err := os.ReadDir(scratchRoot)
	if err != nil {
		t.Fatalf("read scratch root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("template-created scratch must be removed after Probe, remaining: %v", names)
	}
}

// A probe whose serve child cannot start must still remove the
// template-created scratch directory: cleanup covers every path past
// directory allocation, not only successful termination.
func TestProbe_StartFailureStillCleansScratch(t *testing.T) {
	dir := t.TempDir()
	binDir := compileStubOpencode(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	scratchRoot := filepath.Join(dir, "operator-scratch")
	if err := os.MkdirAll(scratchRoot, 0o700); err != nil {
		t.Fatalf("mkdir scratch root: %v", err)
	}
	tpl := &brokenServeBinaryTemplate{stubProbeTemplate{base: scratchRoot}}
	adp := NewOpenCodeAdapterWithLaunch(execpolicy.New(), tpl, nil, nil)

	if _, err := adp.Probe(context.Background()); err == nil {
		t.Fatal("probe must fail when the serve child cannot start")
	}
	entries, err := os.ReadDir(scratchRoot)
	if err != nil {
		t.Fatalf("read scratch root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("failed probe start must not leak the scratch directory, remaining: %v", names)
	}
}

type brokenServeBinaryTemplate struct{ stubProbeTemplate }

func (t brokenServeBinaryTemplate) ProbeServeLaunch(_ context.Context) (execpolicy.LaunchRequest, error) {
	scratch, err := os.MkdirTemp(t.base, "probe-scratch-")
	if err != nil {
		return execpolicy.LaunchRequest{}, err
	}
	return execpolicy.LaunchRequest{
		RunID:     "run-probe",
		SessionID: "sess-probe-serve",
		Command:   "opencode-not-on-path",
		Args:      []string{"serve", "--hostname", "127.0.0.1", "--port", "0"},
		Paths:     workspace.WorkspacePaths{Root: scratch, Config: scratch},
		Profile:   lifecycleProfile(),
	}, nil
}
