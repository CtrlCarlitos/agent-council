package agytest

// Build guard (AC-010, mirroring internal/adapter/codex/codextest/
// guard_test.go exactly): production source files anywhere in the
// repository must not import this test-only package, must not reference
// its fixture-construction surface, and the two copies of the fixture
// executable's source (this package's fixtureSource and
// internal/adapter/agy/agyfake_test.go's agyFixtureSource) must stay
// byte-identical. Test files are excluded from the import/reference
// scans (they are not production surface).

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

// agyImportPath is the agy package's import path, used to resolve a
// selector's package by import path rather than assuming the literal
// identifier "agy" (Minor 7): an aliased import (import a "…/agy") must
// still be recognized.
const agyImportPath = "github.com/CtrlCarlitos/agent-council/internal/adapter/agy"

// shouldSkipWalkDir reports whether a directory encountered during a
// repo-wide AST walk must be skipped entirely (Minor 8): VCS/vendor/
// scratch directories, and any nested checkout — a directory other than
// the walk root that owns its own go.mod — so these guards never
// descend into a worktree or an unrelated vendored/nested module.
func shouldSkipWalkDir(root, path string, info os.FileInfo) bool {
	switch info.Name() {
	case ".git", "vendor", ".superpowers", ".worktrees", "node_modules":
		return true
	}
	if path == root {
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
		return true
	}
	return false
}

// inPackageDir reports whether the file at path lives directly in the
// repo-relative package directory pkgDir (exact directory match: a
// sibling such as internal/adapter/agyx is NOT exempted, and neither is
// a nested package below pkgDir).
func inPackageDir(root, path, pkgDir string) bool {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	return err == nil && filepath.ToSlash(rel) == pkgDir
}

// agyDotImported reports whether f dot-imports the agy package (its
// exported names are then bare identifiers in f).
func agyDotImported(f *ast.File) bool {
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) == agyImportPath && imp.Name != nil && imp.Name.Name == "." {
			return true
		}
	}
	return false
}

// agyImportLocalNames returns the set of local identifiers f binds to
// the agy package's import path: the alias when one is given, otherwise
// the conventional last path segment "agy".
func agyImportLocalNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != agyImportPath {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name != "_" && imp.Name.Name != "." {
				names[imp.Name.Name] = true
			}
			continue
		}
		names["agy"] = true
	}
	return names
}

func TestAgyTest_ImportGuard_ProductionPackagesDoNotImportAgyTest(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if shouldSkipWalkDir(root, path, info) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip the agytest package itself (exact directory).
		if inPackageDir(root, path, "internal/adapter/agy/agytest") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(importPath, "internal/adapter/agy/agytest") {
				t.Errorf("production file %s illegally imports agytest: %s", path, importPath)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("failed checking production package imports: %v", walkErr)
	}
}

// The test-only construction surface is the fixture-scope seam: outside
// the agy package itself (where FixtureOption/FixtureMode and the
// eventual fixture-scoped constructor live), no production source file
// may reference it directly. An import of agytest is already refused
// above; this guard closes the remaining path -- a production package
// reaching the test-only scope through the agy package's own exported
// names (mirrors codextest's identical guard, forward-declared for
// Task 5's fixture-scoped constructor name too).
func TestAgyTest_ImportGuard_NoFixtureConstructorReferencesOutsideAgyPackage(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	forbidden := map[string]bool{
		"FixtureOption": true,
		"FixtureMode":   true,
		// Task 5's fixture-scoped constructor (production eligibility
		// skipped): never reachable from production wiring.
		"NewFixtureScopedAdapter": true,
	}
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if shouldSkipWalkDir(root, path, info) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The agy package itself owns the constructors and the typed
		// production rejection; agytest carries the fixture harness.
		if inPackageDir(root, path, "internal/adapter/agy") || inPackageDir(root, path, "internal/adapter/agy/agytest") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		// Resolve the agy package's local identifier(s) by import PATH
		// (Minor 7), not by assuming the literal identifier "agy": an
		// aliased import (import a "…/adapter/agy") must still be
		// recognized, and a same-named but unrelated package (e.g.
		// codex's own FixtureMode) must not false-positive.
		agyNames := agyImportLocalNames(f)
		dot := agyDotImported(f)
		if len(agyNames) == 0 && !dot {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			// A dot import makes the forbidden names bare identifiers.
			if id, ok := n.(*ast.Ident); ok && dot && forbidden[id.Name] {
				t.Errorf("production file %s illegally references the test-only fixture construction surface through a dot import: %s",
					path, id.Name)
				return true
			}
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if !agyNames[identName(sel.X)] {
				return true
			}
			if forbidden[sel.Sel.Name] {
				t.Errorf("production file %s illegally references the test-only fixture construction surface: %s.%s",
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
// source must stay byte-identical to the agy package's authoritative
// copy. The child silently drops unrecognized directives (json.Unmarshal
// plus a switch with no default), so a one-sided edit would turn a
// staged scenario into a silent no-op while this side's suite still
// passes.
func TestAgyTest_FixtureSourceByteIdenticalToFakeChild(t *testing.T) {
	fakePath := filepath.Join("..", "agyfake_test.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, fakePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", fakePath, err)
	}
	fake := evalStringConst(t, f, "agyFixtureSource")
	if fake == fixtureSource {
		return
	}
	sha := func(b string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(b))) }
	t.Fatalf(`fixture executable source drifted between copies:
  agy/agyfake_test.go agyFixtureSource: %d bytes %s
  agytest/fixture.go fixtureSource:     %d bytes %s
Sync the fixture executable source in BOTH files (or extend both together);
a one-sided change silently no-ops staged scenarios on the agytest side.`,
		len(fake), sha(fake), len(fixtureSource), sha(fixtureSource))
}

// The exemptions match exact package directories (a sibling like
// internal/adapter/agyx is not exempt) and a dot import is recognized.
func TestAgyTest_GuardHelpers_ExactDirsAndDotImport(t *testing.T) {
	root := filepath.FromSlash("/repo")
	for path, want := range map[string]bool{
		"/repo/internal/adapter/agy/adapter.go":       true,
		"/repo/internal/adapter/agyx/adapter.go":      false,
		"/repo/internal/adapter/agy/sub/x.go":         false,
		"/repo/other/internal/adapter/agy/adapter.go": false,
	} {
		if got := inPackageDir(root, filepath.FromSlash(path), "internal/adapter/agy"); got != want {
			t.Errorf("inPackageDir(%s) = %v, want %v", path, got, want)
		}
	}
	src := "package p\nimport . \"" + agyImportPath + "\"\nvar _ = FixtureOption\n"
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !agyDotImported(f) || len(agyImportLocalNames(f)) != 0 {
		t.Fatal("a dot import of agy must be recognized (and binds no selector name)")
	}
}
