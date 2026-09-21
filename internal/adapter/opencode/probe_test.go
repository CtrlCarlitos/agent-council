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
)

// stubProbeTemplate is a test operator-owned template: it allocates its
// own scratch directories and produces the exact approved serve shape.
type stubProbeTemplate struct {
	base string
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
	scratch, err := os.MkdirTemp(t.base, "probe-scratch-")
	if err != nil {
		return execpolicy.LaunchRequest{}, err
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

	adp := NewOpenCodeAdapterWithLaunch(execpolicy.New(), &stubProbeTemplate{base: dir}, nil, nil)
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

	// The scratch directory is inside the template's base.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read template base: %v", err)
	}
	var sawScratch bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "probe-scratch-") {
			sawScratch = true
			scratch := filepath.Join(dir, e.Name())
			if fails := readStubFile(t, scratch, ".stub-authfail"); len(fails) != 0 {
				t.Fatalf("probe calls must authenticate, auth failures: %v", fails)
			}
			for _, hit := range readStubFile(t, scratch, ".stub-hits") {
				if hit != "GET /api/health" && hit != "GET /api/model" {
					t.Fatalf("probe may only call health and model endpoints, saw: %q", hit)
				}
			}
		}
	}
	if !sawScratch {
		t.Fatal("probe scratch directory must be allocated by the template")
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
