package codextest

// Build guard (AC-009 spec §3.3, mirroring the AC-006 adaptertest guard
// in internal/adapter/council_boundary_test.go): production source files
// anywhere in the repository must not import this test-only package.
// codextest is a test utility — there is no production fallback through
// it. Test files are excluded (they are not production surface).

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCodexTest_ImportGuard_ProductionPackagesDoNotImportCodexTest(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
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
		// Skip the codextest package itself.
		if strings.Contains(filepath.ToSlash(path), "internal/adapter/codex/codextest") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(importPath, "internal/adapter/codex/codextest") {
				t.Errorf("production file %s illegally imports codextest: %s", path, importPath)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("failed checking production package imports: %v", walkErr)
	}
}

// The test-only construction surface is the fixture-scope seam: outside
// the codex package itself (where the constructors and the production
// guard live), NO production source file may reference
// NewFixtureScopedAdapter, FixtureOption, or FixtureMode. An import of
// codextest is already refused above; this guard closes the remaining
// path — a production package reaching the test-only scope through the
// codex package's constructors directly (Task 8 boundary extension).
func TestCodexTest_ImportGuard_NoFixtureConstructorReferencesOutsideCodexPackage(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	forbidden := map[string]bool{
		"NewFixtureScopedAdapter": true,
		"FixtureOption":           true,
		"FixtureMode":             true,
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
		// The codex package itself owns the constructors and the typed
		// production rejection; codextest carries the fixture harness.
		if strings.Contains(filepath.ToSlash(path), "internal/adapter/codex") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if forbidden[sel.Sel.Name] {
				t.Errorf("production file %s illegally references the test-only fixture constructor surface: %s.%s",
					path, identName(sel.X), sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("failed checking fixture constructor references: %v", walkErr)
	}
}

func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}

// evalStringConst evaluates a package-level const declared as a
// (possibly concatenated) Go string expression.
func evalStringConst(t *testing.T, f *ast.File, name string) string {
	t.Helper()
	var val string
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok {
			for i, spec := range vs.Names {
				if spec.Name == name && i < len(vs.Values) {
					s, err := evalStringExpr(vs.Values[i])
					if err != nil {
						t.Fatalf("evaluate %s: %v", name, err)
					}
					val = s
					found = true
				}
			}
		}
		return true
	})
	if !found {
		t.Fatalf("const %s not found", name)
	}
	return val
}

func evalStringExpr(e ast.Expr) (string, error) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", strconv.ErrSyntax
		}
		return strconv.Unquote(x.Value)
	case *ast.ParenExpr:
		return evalStringExpr(x.X)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", strconv.ErrSyntax
		}
		a, err := evalStringExpr(x.X)
		if err != nil {
			return "", err
		}
		b, err := evalStringExpr(x.Y)
		if err != nil {
			return "", err
		}
		return a + b, nil
	}
	return "", strconv.ErrSyntax
}

// Byte-equality drift guard: the harness copy of the fixture executable
// source must stay byte-identical to the codex package's authoritative
// copy. The child silently drops unknown directives (json.Unmarshal plus
// a switch with no default), so a one-sided edit would turn a staged
// emit_many/append scenario into a silent no-op while its suite still
// passes — this test makes one-sided drift FAIL instead.
func TestCodexTest_FixtureSourceByteIdenticalToFakeChild(t *testing.T) {
	fakePath := filepath.Join("..", "codexfake_test.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, fakePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", fakePath, err)
	}
	fake := evalStringConst(t, f, "codexFixtureSource")
	if fake == fixtureSource {
		return
	}
	sha := func(b string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(b))) }
	t.Fatalf(`fixture executable source drifted between copies:
  codex/codexfake_test.go codexFixtureSource: %d bytes %s
  codextest/fixture.go fixtureSource:         %d bytes %s
Sync the fixture executable source in BOTH files (or extend both together);
a one-sided change silently no-ops staged scenarios on the codextest side.`,
		len(fake), sha(fake), len(fixtureSource), sha(fixtureSource))
}
