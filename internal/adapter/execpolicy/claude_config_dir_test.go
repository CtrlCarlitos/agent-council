package execpolicy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
)

func claudeLaunch(configDir string) LaunchRequest {
	return LaunchRequest{
		RunID: "run-cl", SessionID: "sess-cl",
		Command:         "claude",
		Args:            claudeArgs(),
		Paths:           workspace.WorkspacePaths{Root: "/tmp/ws", Config: "/tmp/cfg"},
		ClaudeConfigDir: configDir,
	}
}

// claudeArgs returns a FRESH slice each call: mutation-sensitive tests
// must not share a backing array.
func claudeArgs() []string {
	return append([]string{}, []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--session-id", "e8e4074f-8a52-4c28-b1f7-9a2e5dbf4a11",
		"--model", "haiku", "--max-turns", "8",
	}...)
}

// containedConfigRoot builds a REAL config root inside a temporary base
// and returns both paths.
func containedConfigRoot(t *testing.T) (base, dir string) {
	t.Helper()
	base = t.TempDir()
	dir = filepath.Join(base, "run-cl", "sess-cl", "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir config root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return base, dir
}

// The typed extension is accepted only on the exact Claude launch shape
// with the config dir contained in the trusted base.
func TestClaudeConfigDir_AcceptedOnExactClaudeShape(t *testing.T) {
	base, dir := containedConfigRoot(t)
	req := claudeLaunch(dir)
	req.ClaudeConfigBaseDir = base
	if err := validateClaudeConfigDir(req); err != nil {
		t.Fatalf("exact claude shape must accept a contained config dir: %v", err)
	}
	if !IsClaudeLaunch(req) {
		t.Fatal("sanity: exact shape must be recognized")
	}
}

func TestClaudeConfigDir_RejectedOnOtherLaunches(t *testing.T) {
	base, dir := containedConfigRoot(t)
	cases := []struct {
		name string
		mut  func(*LaunchRequest)
	}{
		{"opencode serve launch", func(r *LaunchRequest) {
			r.Command = "opencode"
			r.Args = []string{"serve", "--hostname", "127.0.0.1", "--port", "0"}
		}},
		{"bare claude invocation", func(r *LaunchRequest) {
			r.Args = []string{"--version"}
		}},
		{"forbidden flag injected", func(r *LaunchRequest) {
			r.Args = append(append([]string{}, r.Args...), "--bare")
		}},
		{"reordered args", func(r *LaunchRequest) {
			r.Args = []string{"--model", "haiku", "-p", "--output-format", "stream-json",
				"--verbose", "--session-id", "e8e4074f-8a52-4c28-b1f7-9a2e5dbf4a11",
				"--max-turns", "8"}
		}},
		{"extra argument", func(r *LaunchRequest) {
			r.Args = append(append([]string{}, r.Args...), "extra")
		}},
		{"invalid session uuid", func(r *LaunchRequest) {
			r.Args[5] = "not-a-uuid"
		}},
		{"non-positive max turns", func(r *LaunchRequest) {
			r.Args[9] = "0"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := claudeLaunch(dir)
			req.ClaudeConfigBaseDir = base
			tc.mut(&req)
			err := validateClaudeConfigDir(req)
			if err == nil {
				t.Fatal("config dir must be rejected on a non-exact launch")
			}
			if !strings.Contains(err.Error(), ErrClaudeConfigDirShape.Error()) {
				t.Fatalf("expected shape error, got %v", err)
			}
		})
	}
}

func TestClaudeConfigDir_UnsetIsNoOp(t *testing.T) {
	// Unset extension: nothing to validate, any launch shape is fine.
	req := LaunchRequest{Command: "opencode", Args: []string{"serve"}}
	if err := validateClaudeConfigDir(req); err != nil {
		t.Fatalf("unset extension must be a no-op, got %v", err)
	}
	whitespace := claudeLaunch("   ")
	if err := validateClaudeConfigDir(whitespace); err != nil {
		t.Fatalf("whitespace-only extension must be treated as unset, got %v", err)
	}
	// Set on a claude launch: accepted when the dir is contained in a
	// real base.
	b, d := containedConfigRoot(t)
	withBase := claudeLaunch(d)
	withBase.ClaudeConfigBaseDir = b
	if err := validateClaudeConfigDir(withBase); err != nil {
		t.Fatalf("set extension on an exact claude launch must validate: %v", err)
	}
}

// IsClaudeLaunch recognizes only the exact contract: forbidden flags,
// unknown arguments, or missing pairs are rejected.
func TestIsClaudeLaunch_ExactContract(t *testing.T) {
	if !IsClaudeLaunch(claudeLaunch("/x")) {
		t.Fatal("exact contract must be recognized")
	}
	resume := claudeLaunch("/x")
	resume.Args[4] = "--resume"
	if !IsClaudeLaunch(resume) {
		t.Fatal("the --resume variant must be recognized")
	}
	withLists := claudeLaunch("/x")
	withLists.Args = append(withLists.Args,
		"--allowedTools", "Read", "Glob",
		"--disallowedTools", "WebSearch")
	if !IsClaudeLaunch(withLists) {
		t.Fatal("the tool-list variant must be recognized")
	}
	if IsClaudeLaunch(LaunchRequest{Command: "opencode", Args: claudeArgs()}) {
		t.Fatal("wrong command must not be recognized")
	}
	if _, err := uuidFromArgs(claudeArgs()); err != nil {
		t.Fatalf("uuid must parse: %v", err)
	}
	if _, err := uuidFromArgs([]string{"-p", "--output-format", "stream-json", "--verbose",
		"--session-id", "nope"}); err == nil {
		t.Fatal("malformed uuid must fail the shape")
	}
}

// ErrClaudeConfigDirShape must be the typed rejection.
func TestClaudeConfigDir_TypedError(t *testing.T) {
	base, dir := containedConfigRoot(t)
	req := claudeLaunch(dir)
	req.ClaudeConfigBaseDir = base
	req.Command = "opencode"
	req.Args = []string{"serve", "--hostname", "127.0.0.1", "--port", "0"}
	err := validateClaudeConfigDir(req)
	if !errors.Is(err, ErrClaudeConfigDirShape) {
		t.Fatalf("expected ErrClaudeConfigDirShape, got %v", err)
	}
}

func TestClaudeConfigDir_ContainmentRules(t *testing.T) {
	base, dir := containedConfigRoot(t)
	launchWith := func(configDir, configBase string) LaunchRequest {
		req := claudeLaunch(configDir)
		req.ClaudeConfigBaseDir = configBase
		return req
	}

	t.Run("missing base", func(t *testing.T) {
		req := launchWith(dir, "")
		err := validateClaudeConfigDir(req)
		if err == nil || !strings.Contains(err.Error(), "ClaudeConfigBaseDir is required") {
			t.Fatalf("missing base must be rejected, got %v", err)
		}
	})
	t.Run("config dir outside base", func(t *testing.T) {
		outside := t.TempDir()
		insideOutside := filepath.Join(outside, "config")
		os.MkdirAll(insideOutside, 0o700)
		err := validateClaudeConfigDir(launchWith(insideOutside, base))
		if err == nil || !strings.Contains(err.Error(), "outside the claude config base") {
			t.Fatalf("config dir outside the base must be rejected, got %v", err)
		}
	})
	t.Run("config dir is symlink", func(t *testing.T) {
		outside := t.TempDir()
		target := filepath.Join(outside, "real-config")
		os.MkdirAll(target, 0o700)
		link := filepath.Join(base, "link-config")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		err := validateClaudeConfigDir(launchWith(link, base))
		if err == nil {
			t.Fatal("symlinked config dir must be rejected")
		}
	})
	t.Run("config dir does not exist", func(t *testing.T) {
		err := validateClaudeConfigDir(launchWith(filepath.Join(base, "run-x", "sess-x", "config"), base))
		if err == nil {
			t.Fatal("non-existent config dir must be rejected")
		}
	})
}
