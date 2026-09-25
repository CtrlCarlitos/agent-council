package agytest

// End-to-end exercise of NewFixture/LaunchRequest (Minor 6): these two
// exported entry points are what Task 5's fixture-scoped adapter
// construction and any operator-invoked manual evidence executable
// actually call, but until now nothing in this package's own test suite
// called them — every existing test here (guard_test.go) exercises the
// embedded fixtureSource constant directly, never NewFixture or
// LaunchRequest. This test builds the fixture via NewFixture, launches
// it through the real execpolicy.New().Start with the exact
// LaunchRequest this package hands external callers, reads the init
// event off real stdout, and — on Linux — asserts the launched
// process's ExecutableIdentity digest matches the fixture's own Digest,
// proving the sealed path this package builds is the one that actually
// ran (not just that Start() happened not to error).

import (
	"bufio"
	"context"
	"encoding/json"
	"runtime"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
)

func TestFixture_NewFixtureAndLaunchRequestEndToEnd(t *testing.T) {
	fx, err := NewFixture()
	if err != nil {
		t.Fatalf("NewFixture: %v", err)
	}
	t.Cleanup(func() { _ = fx.Close() })

	scratch := t.TempDir()
	paths := workspace.WorkspacePaths{Root: scratch, Config: scratch}
	args := []string{
		"--print=",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--disable-slash-commands",
		"--model", "m",
		"--print-timeout", "120s",
		"--log-file", "agy.log",
	}
	req := fx.LaunchRequest("session-agytest-e2e", "run-agytest-e2e", args, paths, DefaultProfile())

	proc, err := execpolicy.New().Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = proc.Terminate(context.Background()) })

	scanner := bufio.NewScanner(proc.Stdout())
	if !scanner.Scan() {
		t.Fatalf("expected at least one line of stdout (the init event), scanner err: %v", scanner.Err())
	}
	var probe struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &probe); err != nil {
		t.Fatalf("decode init line: %v (line: %q)", err, scanner.Text())
	}
	if probe.Event != "init" {
		t.Fatalf("expected the init event first, got %q (line: %q)", probe.Event, scanner.Text())
	}

	if runtime.GOOS == "linux" {
		got := proc.ExecutableIdentity()
		if got.Digest == "" {
			t.Fatalf("expected a non-empty ExecutableIdentity().Digest on Linux (sealed path)")
		}
		if got.Digest != fx.Digest {
			t.Fatalf("ExecutableIdentity().Digest = %q, want the fixture digest %q", got.Digest, fx.Digest)
		}
	}
}
