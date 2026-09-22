package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemplate(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dir
}

// Materialization copies the template tree and produces the template's
// digest.
func TestMaterializeConfigRoot_CopiesTreeWithDigest(t *testing.T) {
	tmpl := writeTemplate(t, map[string]string{
		"settings.json": "{}",
		"skills/a.md":   "alpha",
	})
	base := t.TempDir()

	root, digest, err := MaterializeConfigRoot(tmpl, base, "run-1", "sess-1")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	wantRoot := filepath.Join(base, "run-1", "sess-1", "config")
	if root != wantRoot {
		t.Fatalf("root must be <base>/<run>/<session>/config, got %q", root)
	}
	b, err := os.ReadFile(filepath.Join(root, "settings.json"))
	if err != nil || string(b) != "{}" {
		t.Fatalf("settings.json must be copied, got %q err=%v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(root, "skills", "a.md"))
	if err != nil || string(b) != "alpha" {
		t.Fatalf("nested files must be copied, got %q err=%v", b, err)
	}
	wantDigest, err := TemplateDigest(tmpl)
	if err != nil {
		t.Fatalf("template digest: %v", err)
	}
	if digest != wantDigest {
		t.Fatalf("materialized digest must match the template, got %q want %q", digest, wantDigest)
	}
}

// Existing roots are never silently overwritten.
func TestMaterializeConfigRoot_FailsOnExistingRoot(t *testing.T) {
	tmpl := writeTemplate(t, map[string]string{"settings.json": "{}"})
	base := t.TempDir()

	if _, _, err := MaterializeConfigRoot(tmpl, base, "run-1", "sess-1"); err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	if _, _, err := MaterializeConfigRoot(tmpl, base, "run-1", "sess-1"); err == nil {
		t.Fatal("re-materializing an existing root must fail closed")
	}
}

// Invalid templates fail and leave nothing behind.
func TestMaterializeConfigRoot_FailureCleansUp(t *testing.T) {
	tmpl := writeTemplate(t, map[string]string{"real.txt": "x"})
	if err := os.Symlink(filepath.Join(tmpl, "real.txt"), filepath.Join(tmpl, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	base := t.TempDir()

	if _, _, err := MaterializeConfigRoot(tmpl, base, "run-1", "sess-1"); err == nil {
		t.Fatal("symlinked template must be rejected")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("failed materialization must leave no directories, found: %v", names)
	}
}

// Oversized files are rejected (bounds).
func TestMaterializeConfigRoot_RejectsOversizedFile(t *testing.T) {
	tmpl := writeTemplate(t, map[string]string{
		"big.bin": strings.Repeat("x", maxTemplateFileBytes+1),
	})
	base := t.TempDir()
	if _, _, err := MaterializeConfigRoot(tmpl, base, "run-1", "sess-1"); err == nil {
		t.Fatal("oversized template file must be rejected")
	}
}

// Secure permissions on POSIX (Windows reports synthetic modes).
func TestMaterializeConfigRoot_SecurePermissions(t *testing.T) {
	if runtimeGOOSWindows {
		t.Skip("permission semantics are POSIX-only")
	}
	tmpl := writeTemplate(t, map[string]string{"settings.json": "{}"})
	base := t.TempDir()

	root, _, err := MaterializeConfigRoot(tmpl, base, "run-1", "sess-1")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if perm := permOf(t, root); perm != 0o700 {
		t.Fatalf("config root must be 0700, got %v", perm)
	}
	if perm := permOf(t, filepath.Join(root, "settings.json")); perm != 0o600 {
		t.Fatalf("copied files must be 0600, got %v", perm)
	}
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
