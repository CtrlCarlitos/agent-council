package execpolicy

import (
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
)

func serveLaunch(args []string) LaunchRequest {
	return LaunchRequest{
		RunID:     "run-shape",
		SessionID: "sess-shape",
		Command:   "opencode",
		Args:      args,
		Paths:     workspace.WorkspacePaths{Root: "/tmp/ws", Config: "/tmp/cfg"},
	}
}

// Only the exact approved invocation `opencode serve --hostname 127.0.0.1
// --port 0` may carry GeneratedServerEnv. Every deviation is rejected:
// alternate hostnames, nonzero ports, missing flags, permission bypasses,
// and any extra argument.
func TestOpenCodeServeShape_AcceptsExactApprovedInvocation(t *testing.T) {
	req := serveLaunch([]string{"serve", "--hostname", "127.0.0.1", "--port", "0"})
	if !IsOpenCodeServeLaunch(req) {
		t.Fatal("the exact approved invocation must be accepted")
	}
	// Windows-style absolute path to the same binary base is the same shape.
	req.Command = `C:\tools\opencode.exe`
	if IsOpenCodeServeLaunch(req) {
		t.Fatal("basename must be computed for the platform separator: opencode.exe is not opencode")
	}
	req.Command = "/usr/local/bin/opencode"
	if !IsOpenCodeServeLaunch(req) {
		t.Fatal("absolute path with base opencode must be accepted")
	}
}

func TestOpenCodeServeShape_Rejections(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"external hostname", []string{"serve", "--hostname", "0.0.0.0", "--port", "0"}},
		{"public hostname", []string{"serve", "--hostname", "example.com", "--port", "0"}},
		{"nonzero port", []string{"serve", "--hostname", "127.0.0.1", "--port", "4096"}},
		{"missing hostname flag", []string{"serve", "--port", "0"}},
		{"missing port flag", []string{"serve", "--hostname", "127.0.0.1"}},
		{"no flags at all", []string{"serve"}},
		{"permission bypass auto", []string{"serve", "--hostname", "127.0.0.1", "--port", "0", "--auto"}},
		{"extra unsupported flag", []string{"serve", "--hostname", "127.0.0.1", "--port", "0", "--verbose"}},
		{"reordered flags", []string{"serve", "--port", "0", "--hostname", "127.0.0.1"}},
		{"extra positional", []string{"serve", "--hostname", "127.0.0.1", "--port", "0", "extra"}},
		{"non-serve subcommand", []string{"run", "--hostname", "127.0.0.1", "--port", "0"}},
		{"empty args", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if IsOpenCodeServeLaunch(serveLaunch(tc.args)) {
				t.Fatalf("deviation %q must be rejected", tc.name)
			}
		})
	}
}

// The GeneratedServerEnv shape validation must reject non-serve launches
// with the dedicated error.
func TestGeneratedEnvShape_RejectsNonServe(t *testing.T) {
	req := serveLaunch([]string{"--version"})
	req.GeneratedServerEnv = &GeneratedServerEnv{Username: "u", Password: "p"}
	err := validateGeneratedServerEnvShape(req)
	if err == nil {
		t.Fatal("non-serve launch with GeneratedServerEnv must be rejected")
	}
	if !strings.Contains(err.Error(), ErrServerEnvShape.Error()) {
		t.Fatalf("expected ErrServerEnvShape, got %v", err)
	}
}
