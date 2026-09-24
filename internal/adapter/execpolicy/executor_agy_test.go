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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
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

// TestIsAgyLaunch_CommandBasenameRecognition covers Minor 1: recognition
// by filepath.Base(req.Command) being "agy"/"agy.exe", independent of
// argv shape or order.
func TestIsAgyLaunch_CommandBasenameRecognition(t *testing.T) {
	cases := []struct {
		name    string
		command string
		args    []string
		want    bool
	}{
		{"bare_agy_install", "agy", []string{"install"}, true},
		{"bare_agy_install_absolute_path", "/usr/local/bin/agy", []string{"install"}, true},
		{"bare_agy_exe_install_windows", "agy.exe", []string{"install"}, true},
		{"reordered_frozen_argv_on_agy_binary", "agy", []string{"--output-format", "stream-json", "--print=", "--input-format", "stream-json"}, true},
		{"unrelated_binary_bare_word_not_agy_shaped", "echo", []string{"install"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := execpolicy.LaunchRequest{Command: tc.command, Args: tc.args}
			if got := execpolicy.IsAgyLaunch(req); got != tc.want {
				t.Errorf("IsAgyLaunch(command=%q args=%v) = %v, want %v", tc.command, tc.args, got, tc.want)
			}
		})
	}
}

// TestPolicyExecutor_AgyLaunch_BareInstallRefusedUnsealedOnLinux (Minor
// 1): "agy install" — a bare invocation that does not match the frozen
// stream-json argv shape at all — is still agy-shaped by command
// basename, and an unsealed launch of it is refused on Linux. ("install"
// also happens to be independently forbidden, so either typed reason is
// accepted as evidence of refusal here; what this test pins down is
// that Start() never reaches a running process.)
func TestPolicyExecutor_AgyLaunch_BareInstallRefusedUnsealedOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-specific: production sealing is unconditional there")
	}
	ctx := context.Background()
	executor := execpolicy.New()
	paths := setupWorkspacePaths(t, "run-agy-bare-install", "session-agy-bare-install")

	req := execpolicy.LaunchRequest{
		RunID:     "run-agy-bare-install",
		SessionID: "session-agy-bare-install",
		Command:   "agy",
		Args:      []string{"install"},
		Paths:     paths,
		Profile:   agyProfile(),
	}
	if !execpolicy.IsAgyLaunch(req) {
		t.Fatalf("expected bare %q install to be recognized as agy-shaped", req.Command)
	}
	_, err := executor.Start(ctx, req)
	if !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) {
		t.Fatalf("expected ErrInvalidLaunchRequest, got %v", err)
	}
	if !errors.Is(err, execpolicy.ErrAgyLaunchForbiddenArg) && !errors.Is(err, execpolicy.ErrAgyLaunchNotSealed) {
		t.Fatalf("expected ErrAgyLaunchForbiddenArg or ErrAgyLaunchNotSealed, got %v", err)
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

// sanitizeForIdentifier maps any character workspace.ValidateIdentifier
// (^[a-zA-Z0-9_-]+$) would reject to "_", so a raw forbidden-arg string
// like "--add-dir=/x" can be embedded in a test session id.
func sanitizeForIdentifier(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func TestPolicyExecutor_AgyLaunch_ForbiddenArgRefused(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	for _, forbidden := range []string{
		"--dangerously-skip-permissions", "-c", "--continue", "-i",
		"--prompt-interactive", "--remote-control", "--add-dir",
		"--project", "--new-project", "install", "update", "mic-serve",
		// --flag=value forms (Important 7): the forbidden-arg check must
		// match on the part before "=", not only the exact token.
		"--add-dir=/x", "--project=p", "--dangerously-skip-permissions=true",
	} {
		t.Run(forbidden, func(t *testing.T) {
			sanitized := sanitizeForIdentifier(forbidden)
			paths := setupWorkspacePaths(t, "run-agy-forbidden", "session-agy-forbidden-"+sanitized)
			req := execpolicy.LaunchRequest{
				RunID:         "run-agy-forbidden",
				SessionID:     "session-agy-forbidden-" + sanitized,
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

// TestPolicyExecutor_AgyLaunch_FixtureMarkerLinuxBehavior proves the
// controller ruling on Important 3: NewSealedImage is always available
// on Linux, so an agy-shaped launch without a SealedImage is refused
// UNCONDITIONALLY there — the FixtureLaunch marker never relaxes the
// sealed-image requirement on Linux, marker or not. Off Linux, where
// NewSealedImage is unsupported, the marker is the agytest harness's
// only way to launch its fixture binary, and must succeed. IsAgyLaunch
// itself does not care what the command actually is, so a trivial real
// binary stands in for the fixture here.
func TestPolicyExecutor_AgyLaunch_FixtureMarkerLinuxBehavior(t *testing.T) {
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

	if runtime.GOOS == "linux" {
		if !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) || !errors.Is(err, execpolicy.ErrAgyLaunchNotSealed) {
			t.Fatalf("expected Linux to refuse the fixture-marker bypass unconditionally (ErrAgyLaunchNotSealed), got proc=%v err=%v", proc, err)
		}
		return
	}

	if err != nil {
		t.Fatalf("expected fixture-marker launch to succeed off Linux, got: %v", err)
	}
	if code, err := proc.Wait(); err != nil || code != 0 {
		t.Fatalf("expected clean exit, got code=%d err=%v", code, err)
	}
}

// ── FixtureLaunch forgery guard (AC-010, controller ruling on
// Important 4) ─────────────────────────────────────────────────────────
//
// The original guard only matched composite-literal keys and plain
// assignments named FixtureLaunch, scoped to "outside agytest". That
// misses several forge patterns: a bare selector read/take-address
// (&req.FixtureLaunch), an aliased import of execpolicy, and — the
// sharpest gap — a POSITIONAL (unkeyed) execpolicy.LaunchRequest
// composite literal, which can set the FixtureLaunch bool field by
// position without ever naming it. The tightened guard below is
// therefore two independent checks, scoped per-file:
//   - checkFixtureLaunchIdentifierMisuse: ANY reference to the
//     identifier FixtureLaunch (selector, address-of a selector, or a
//     composite-literal key) — required to be absent everywhere except
//     internal/adapter/execpolicy (where the field is declared) and
//     internal/adapter/agy/agytest (the one package authorized to set
//     it, always keyed).
//   - checkPositionalLaunchRequestLiteral: any positional
//     execpolicy.LaunchRequest literal, resolved by import PATH (alias-
//     safe, not by assuming the identifier "execpolicy") — required to
//     be absent everywhere except internal/adapter/execpolicy itself.

const execpolicyImportPath = "github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"

// shouldSkipWalkDir reports whether a directory encountered during a
// repo-wide AST walk must be skipped entirely (Minor 8): VCS/vendor/
// scratch directories, and any nested checkout — a directory other than
// the walk root that owns its own go.mod — so the guard never descends
// into a worktree or an unrelated vendored/nested module.
func shouldSkipWalkDir(root, dirPath string, info os.FileInfo) bool {
	switch info.Name() {
	case ".git", "vendor", ".superpowers", ".worktrees", "node_modules":
		return true
	}
	if dirPath == root {
		return false
	}
	if _, err := os.Stat(filepath.Join(dirPath, "go.mod")); err == nil {
		return true
	}
	return false
}

// importLocalNames returns the set of local identifiers f binds to
// importPath: the alias when one is given (excluding "_"/"." which
// cannot be used as a selector base or are out of scope here),
// otherwise the conventional last path segment. Resolving by import
// path — not by assuming the identifier "execpolicy" — is what makes
// the guard alias-safe.
func importLocalNames(f *ast.File, importPath string) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != importPath {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name != "_" && imp.Name.Name != "." {
				names[imp.Name.Name] = true
			}
			continue
		}
		names[path.Base(importPath)] = true
	}
	return names
}

func isLaunchRequestType(t ast.Expr, execpolicyNames map[string]bool) bool {
	switch x := t.(type) {
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && execpolicyNames[id.Name] && x.Sel.Name == "LaunchRequest"
	case *ast.StarExpr:
		return isLaunchRequestType(x.X, execpolicyNames)
	}
	return false
}

func hasPositionalElement(node *ast.CompositeLit) bool {
	if len(node.Elts) == 0 {
		return false
	}
	for _, elt := range node.Elts {
		if _, ok := elt.(*ast.KeyValueExpr); !ok {
			return true
		}
	}
	return false
}

// checkFixtureLaunchIdentifierMisuse flags any reference to the
// identifier FixtureLaunch: a selector expression (req.FixtureLaunch —
// this also catches &req.FixtureLaunch, since ast.Inspect visits the
// inner SelectorExpr regardless of the enclosing UnaryExpr) or a
// composite-literal key.
func checkFixtureLaunchIdentifierMisuse(fset *token.FileSet, f *ast.File) []string {
	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if node.Sel.Name == "FixtureLaunch" {
				violations = append(violations, fmt.Sprintf("%s: illegal reference to FixtureLaunch", fset.Position(node.Pos())))
			}
		case *ast.KeyValueExpr:
			if id, ok := node.Key.(*ast.Ident); ok && id.Name == "FixtureLaunch" {
				violations = append(violations, fmt.Sprintf("%s: illegal FixtureLaunch key in composite literal", fset.Position(node.Pos())))
			}
		}
		return true
	})
	return violations
}

// checkPositionalLaunchRequestLiteral flags any positional (unkeyed)
// composite literal of execpolicy.LaunchRequest, resolved by import
// path: a positional literal can set the FixtureLaunch bool field
// without ever naming the identifier, forging the marker past a guard
// that only looks for the name "FixtureLaunch".
func checkPositionalLaunchRequestLiteral(fset *token.FileSet, f *ast.File) []string {
	execpolicyNames := importLocalNames(f, execpolicyImportPath)
	if len(execpolicyNames) == 0 {
		return nil
	}
	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if isLaunchRequestType(lit.Type, execpolicyNames) && hasPositionalElement(lit) {
			violations = append(violations, fmt.Sprintf("%s: illegal positional (unkeyed) execpolicy.LaunchRequest composite literal", fset.Position(lit.Pos())))
		}
		return true
	})
	return violations
}

// fixtureLaunchGuardViolations applies the exact per-file scoping the
// repo-wide guard test uses to a single parsed file, keyed by its
// slash-form path: internal/adapter/execpolicy is fully exempt (both
// checks — the field is declared there); internal/adapter/agy/agytest
// is exempt from the identifier check only (it legitimately writes
// FixtureLaunch: true as a keyed field, but a positional literal there
// would still be suspicious and stays flagged). Shared by the real repo
// walk and the unit fixtures below so both exercise identical logic.
func fixtureLaunchGuardViolations(fset *token.FileSet, f *ast.File, slashPath string) []string {
	if strings.Contains(slashPath, "internal/adapter/execpolicy") {
		return nil
	}
	violations := checkPositionalLaunchRequestLiteral(fset, f)
	if !strings.Contains(slashPath, "internal/adapter/agy/agytest") {
		violations = append(violations, checkFixtureLaunchIdentifierMisuse(fset, f)...)
	}
	return violations
}

// TestFixtureLaunch_NeverForgedOutsideExecpolicyAndAgytest is the build
// guard (AC-010, mirroring the codex/codextest import/reference guards,
// tightened per controller ruling on Important 4): no production .go
// file outside internal/adapter/execpolicy or
// internal/adapter/agy/agytest may reference the FixtureLaunch
// identifier, and no production .go file outside
// internal/adapter/execpolicy may construct a positional (unkeyed)
// execpolicy.LaunchRequest literal.
func TestFixtureLaunch_NeverForgedOutsideExecpolicyAndAgytest(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	walkErr := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if shouldSkipWalkDir(root, p, info) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, p, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, v := range fixtureLaunchGuardViolations(fset, f, filepath.ToSlash(p)) {
			t.Errorf("production file %s: %s", p, v)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("failed checking FixtureLaunch forgery patterns: %v", walkErr)
	}
}

// TestFixtureLaunchGuard_ForgePatterns unit-tests the checker functions
// directly against synthetic Go source (parsed in isolation, independent
// of what happens to be in the repo right now): one case per forge
// pattern named in the controller ruling, plus a positive case proving
// the legitimate keyed agytest usage is not flagged.
func TestFixtureLaunchGuard_ForgePatterns(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		path       string
		wantCaught bool
	}{
		{
			name: "selector_assignment",
			src: `package p
import "github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
func f(req *execpolicy.LaunchRequest) { req.FixtureLaunch = true }
`,
			path:       "internal/adapter/somewhere/thing.go",
			wantCaught: true,
		},
		{
			name: "address_of_selector",
			src: `package p
import "github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
func f(req execpolicy.LaunchRequest) *bool { return &req.FixtureLaunch }
`,
			path:       "internal/adapter/somewhere/thing.go",
			wantCaught: true,
		},
		{
			name: "keyed_composite_literal",
			src: `package p
import "github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
func f() execpolicy.LaunchRequest { return execpolicy.LaunchRequest{FixtureLaunch: true} }
`,
			path:       "internal/adapter/somewhere/thing.go",
			wantCaught: true,
		},
		{
			name: "positional_literal",
			src: `package p
import "github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
func f() execpolicy.LaunchRequest { return execpolicy.LaunchRequest{"cmd", nil, true} }
`,
			path:       "internal/adapter/somewhere/thing.go",
			wantCaught: true,
		},
		{
			name: "positional_literal_aliased_import",
			src: `package p
import ep "github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
func f() ep.LaunchRequest { return ep.LaunchRequest{"cmd", nil, true} }
`,
			path:       "internal/adapter/somewhere/thing.go",
			wantCaught: true,
		},
		{
			name: "positional_literal_inside_agytest_still_caught",
			src: `package agytest
import "github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
func f() execpolicy.LaunchRequest { return execpolicy.LaunchRequest{"cmd", nil, true} }
`,
			path:       "internal/adapter/agy/agytest/fixture.go",
			wantCaught: true,
		},
		{
			name: "positive_keyed_agytest_usage",
			src: `package agytest
import "github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
func f() execpolicy.LaunchRequest {
	return execpolicy.LaunchRequest{RunID: "run", SessionID: "session", FixtureLaunch: true}
}
`,
			path:       "internal/adapter/agy/agytest/fixture.go",
			wantCaught: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, tc.path, tc.src, 0)
			if err != nil {
				t.Fatalf("parse fixture source: %v", err)
			}
			violations := fixtureLaunchGuardViolations(fset, f, filepath.ToSlash(tc.path))
			caught := len(violations) > 0
			if caught != tc.wantCaught {
				t.Fatalf("caught=%v (violations=%v), want caught=%v", caught, violations, tc.wantCaught)
			}
		})
	}
}

// TestPolicyExecutor_HomeDir_RefusedOffAgyShape: LaunchRequest.HomeDir
// is an agy-only extension. A non-agy launch carrying it is refused
// before any process starts, and so is an unsealed agy launch.
func TestPolicyExecutor_HomeDir_RefusedOffAgyShape(t *testing.T) {
	root := t.TempDir()
	config := t.TempDir()
	paths := workspace.WorkspacePaths{Root: root, Config: config, Worktree: root, Mode: "isolated_branch"}
	nonAgy := execpolicy.LaunchRequest{
		SessionID: "sess-home-nonagy", Command: "echo", Args: []string{"hello"},
		Paths: paths, Profile: agyProfile(), HomeDir: t.TempDir(),
	}
	nonAgy.Profile.Tooling = []string{"echo"}
	if _, err := execpolicy.New().Start(context.Background(), nonAgy); !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) {
		t.Fatalf("non-agy launch with HomeDir: want ErrInvalidLaunchRequest, got %v", err)
	}

	unsealed := execpolicy.LaunchRequest{
		SessionID: "sess-home-unsealed", Command: "agy", Args: frozenAgyArgs(),
		Paths: paths, Profile: agyProfile(), HomeDir: t.TempDir(),
	}
	if _, err := execpolicy.New().Start(context.Background(), unsealed); !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) {
		t.Fatalf("unsealed agy launch with HomeDir: want ErrInvalidLaunchRequest, got %v", err)
	}
}
