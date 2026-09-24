//go:build !windows

package execpolicy_test

// Tests for the agy-shaped launch recognition and production refusal
// (AC-010): IsAgyLaunch's shape recognition, the forbidden-argv refusal,
// the production refusal of an unsealed agy launch, the FixtureLaunch
// marker's narrow bypass, and a build guard proving no production
// package ever sets FixtureLaunch (mirroring the codex/codextest
// import/reference guards).

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// frozenAgyArgs is the shape IsAgyLaunch recognizes: the leading
// "--print=" flag together with both stream-json format flags (AC-010
// Global Constraints, verbatim). The full frozen argv belongs to Task
// 5's launch source; this is only the recognition shape.
func frozenAgyArgs(extra ...string) []string {
	base := []string{
		"--print=",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--disable-slash-commands",
		"--model", "gemini-3.8-flash-low",
		"--print-timeout", "120s",
		"--log-file", "/tmp/agy.log",
	}
	return append(base, extra...)
}

func TestIsAgyLaunch(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"frozen_shape", frozenAgyArgs(), true},
		{"frozen_shape_with_mode_sandbox_conversation", frozenAgyArgs("--mode", "plan", "--sandbox", "--conversation", "11111111-1111-4111-8111-111111111111"), true},
		{"nil_args", nil, false},
		{"missing_leading_print_equals", []string{"--input-format", "stream-json", "--output-format", "stream-json"}, false},
		{"missing_output_format", []string{"--print=", "--input-format", "stream-json"}, false},
		{"missing_input_format", []string{"--print=", "--output-format", "stream-json"}, false},
		{"non_stream_json_input_format", []string{"--print=", "--input-format", "text", "--output-format", "stream-json"}, false},
		{"unrelated_codex_shape", []string{"app-server"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := execpolicy.LaunchRequest{Args: tc.args}
			if got := execpolicy.IsAgyLaunch(req); got != tc.want {
				t.Errorf("IsAgyLaunch(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func agyProfile() storage.CanonicalProfile {
	return storage.CanonicalProfile{
		AlgoVersion:         "cprof-v4",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"agy"},
	}
}

func TestPolicyExecutor_AgyLaunch_ProductionRefusesUnsealed(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()
	paths := setupWorkspacePaths(t, "run-agy-unsealed", "session-agy-unsealed")

	req := execpolicy.LaunchRequest{
		RunID:     "run-agy-unsealed",
		SessionID: "session-agy-unsealed",
		Command:   "agy",
		Args:      frozenAgyArgs(),
		Paths:     paths,
		Profile:   agyProfile(),
	}
	_, err := executor.Start(ctx, req)
	if !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) {
		t.Fatalf("expected ErrInvalidLaunchRequest, got %v", err)
	}
	if !errors.Is(err, execpolicy.ErrAgyLaunchNotSealed) {
		t.Fatalf("expected ErrAgyLaunchNotSealed, got %v", err)
	}
}

func TestPolicyExecutor_AgyLaunch_ForbiddenArgRefused(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	for _, forbidden := range []string{
		"--dangerously-skip-permissions", "-c", "--continue", "-i",
		"--prompt-interactive", "--remote-control", "--add-dir",
		"--project", "--new-project", "install", "update", "mic-serve",
	} {
		t.Run(forbidden, func(t *testing.T) {
			paths := setupWorkspacePaths(t, "run-agy-forbidden", "session-agy-forbidden-"+strings.ReplaceAll(forbidden, "-", "_"))
			req := execpolicy.LaunchRequest{
				RunID:         "run-agy-forbidden",
				SessionID:     "session-agy-forbidden-" + strings.ReplaceAll(forbidden, "-", "_"),
				Command:       "agy",
				Args:          frozenAgyArgs(forbidden),
				Paths:         paths,
				Profile:       agyProfile(),
				FixtureLaunch: true, // even the fixture bypass must not buy a forbidden flag
			}
			_, err := executor.Start(ctx, req)
			if !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) {
				t.Fatalf("expected ErrInvalidLaunchRequest, got %v", err)
			}
			if !errors.Is(err, execpolicy.ErrAgyLaunchForbiddenArg) {
				t.Fatalf("expected ErrAgyLaunchForbiddenArg, got %v", err)
			}
		})
	}
}

// TestPolicyExecutor_AgyLaunch_FixtureMarkerAllowsUnsealed proves the
// narrow bypass: with the explicit FixtureLaunch marker set, an
// agy-shaped launch is allowed to proceed without a SealedImage (the
// agytest harness's only path off Linux, where NewSealedImage is
// unsupported). IsAgyLaunch itself does not care what the command
// actually is, so a trivial real binary stands in for the fixture here.
func TestPolicyExecutor_AgyLaunch_FixtureMarkerAllowsUnsealed(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()
	paths := setupWorkspacePaths(t, "run-agy-fixture", "session-agy-fixture")

	profile := agyProfile()
	profile.Tooling = []string{"echo"}
	req := execpolicy.LaunchRequest{
		RunID:         "run-agy-fixture",
		SessionID:     "session-agy-fixture",
		Command:       "echo",
		Args:          frozenAgyArgs(),
		Paths:         paths,
		Profile:       profile,
		FixtureLaunch: true,
	}
	proc, err := executor.Start(ctx, req)
	if err != nil {
		t.Fatalf("expected fixture-marker launch to succeed, got: %v", err)
	}
	if code, err := proc.Wait(); err != nil || code != 0 {
		t.Fatalf("expected clean exit, got code=%d err=%v", code, err)
	}
}

// TestFixtureLaunch_NeverSetOutsideAgytest is the build guard (AC-010,
// mirroring the codex/codextest import/reference guards): no production
// .go file may construct or assign LaunchRequest.FixtureLaunch. Only the
// internal/adapter/agy/agytest package (and any _test.go file, already
// excluded below) is allowed to.
func TestFixtureLaunch_NeverSetOutsideAgytest(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "internal/adapter/agy/agytest") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.KeyValueExpr:
				if id, ok := node.Key.(*ast.Ident); ok && id.Name == "FixtureLaunch" {
					t.Errorf("production file %s illegally constructs FixtureLaunch in a composite literal", path)
				}
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if ok && sel.Sel.Name == "FixtureLaunch" {
						t.Errorf("production file %s illegally assigns FixtureLaunch", path)
					}
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("failed checking FixtureLaunch references: %v", walkErr)
	}
}
