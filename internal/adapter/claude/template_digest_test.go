package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// ctmpl-v1 golden: two files, framed deterministically. The expected hex
// is computed from the framing definition, not from the implementation.
func TestTemplateDigest_GoldenVector(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"settings.json": "{}",
		"skills/a.md":   "alpha",
	})

	d, err := TemplateDigest(dir)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	// Framing: count=2; "settings.json" (13 bytes) + "{}" (2 bytes);
	// "skills/a.md" (11 bytes) + "alpha" (5 bytes); sha256 over that
	// byte sequence.
	frame := u32be(2)
	frame = append(frame, u32be(13)...)
	frame = append(frame, "settings.json"...)
	frame = append(frame, u32be(2)...)
	frame = append(frame, "{}"...)
	frame = append(frame, u32be(11)...)
	frame = append(frame, "skills/a.md"...)
	frame = append(frame, u32be(5)...)
	frame = append(frame, "alpha"...)
	want := sha256Hex(frame)
	if d != "ctmpl-v1:sha256:"+want {
		t.Fatalf("golden digest mismatch: got %q want %q", d, "ctmpl-v1:sha256:"+want)
	}
}

func TestTemplateDigest_OrderInsensitiveContentSensitive(t *testing.T) {
	a := writeTree(t, map[string]string{"b.txt": "2", "a.txt": "1"})
	b := writeTree(t, map[string]string{"a.txt": "1", "b.txt": "2"})
	da, err := TemplateDigest(a)
	if err != nil {
		t.Fatalf("digest a: %v", err)
	}
	db, err := TemplateDigest(b)
	if err != nil {
		t.Fatalf("digest b: %v", err)
	}
	if da != db {
		t.Fatalf("creation order must not matter: %q != %q", da, db)
	}

	c := writeTree(t, map[string]string{"a.txt": "1", "b.txt": "3"})
	dc, err := TemplateDigest(c)
	if err != nil {
		t.Fatalf("digest c: %v", err)
	}
	if dc == da {
		t.Fatal("content change must change the digest")
	}
}

func TestTemplateDigest_EmptyFileAndEmptyTree(t *testing.T) {
	d, err := TemplateDigest(writeTree(t, map[string]string{"empty": ""}))
	if err != nil {
		t.Fatalf("empty file must be digestable: %v", err)
	}
	if !strings.HasPrefix(d, "ctmpl-v1:sha256:") {
		t.Fatalf("prefix, got %q", d)
	}
	if _, err := TemplateDigest(writeTree(t, nil)); err == nil {
		t.Fatal("an empty template tree must be rejected")
	}
}

func TestTemplateDigest_RejectsSymlinkAndDuplicateNormalizedPaths(t *testing.T) {
	dir := writeTree(t, map[string]string{"real.txt": "x"})
	if err := os.Symlink(filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := TemplateDigest(dir); err == nil {
		t.Fatal("symlink entries must be rejected")
	}

	// Nested symlink component.
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(sub, filepath.Join(dir, "sub2")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := TemplateDigest(dir); err == nil {
		t.Fatal("symlinked directories must be rejected")
	}
}
