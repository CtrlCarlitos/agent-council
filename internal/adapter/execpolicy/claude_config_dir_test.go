package execpolicy

import (
	"errors"
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

// The typed extension is accepted only on the exact Claude launch shape.
func TestClaudeConfigDir_AcceptedOnExactClaudeShape(t *testing.T) {
	req := claudeLaunch("/svc/claude/run-cl/sess-cl/config")
	if err := validateClaudeConfigDir(req); err != nil {
		t.Fatalf("exact claude shape must accept the config dir: %v", err)
	}
	if !IsClaudeLaunch(req) {
		t.Fatal("sanity: exact shape must be recognized")
	}
}

func TestClaudeConfigDir_RejectedOnOtherLaunches(t *testing.T) {
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
			req := claudeLaunch("/svc/claude/config")
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
	// Set on a claude launch: accepted.
	if err := validateClaudeConfigDir(claudeLaunch("/svc/config")); err != nil {
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
	req := claudeLaunch("/svc/config")
	req.Command = "opencode"
	req.Args = []string{"serve", "--hostname", "127.0.0.1", "--port", "0"}
	err := validateClaudeConfigDir(req)
	if !errors.Is(err, ErrClaudeConfigDirShape) {
		t.Fatalf("expected ErrClaudeConfigDirShape, got %v", err)
	}
}
