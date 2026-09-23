package codextest

// Build guard (AC-009 spec §3.3, mirroring the AC-006 adaptertest guard
// in internal/adapter/council_boundary_test.go): production source files
// anywhere in the repository must not import this test-only package.
// codextest is a test utility — there is no production fallback through
// it. Test files are excluded (they are not production surface).

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
