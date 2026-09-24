package evidence

// The shared evidence-root contract (AC-008/AC-009/AC-010 rules,
// applied verbatim): symlink-safe containment, digest-bound re-hash,
// token-level strict JSON decode (unknown fields, duplicate keys,
// trailing content), and canonical re-encoding.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeEvidence(t *testing.T, root, rel string, content []byte) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, content, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(content))
}

func TestReadFile_ContainmentDigestAndSymlinkMatrix(t *testing.T) {
	root := t.TempDir()
	content := []byte(`{"a":1}`)
	digest := writeEvidence(t, root, "sub/dir/evidence.json", content)

	t.Run("happy path", func(t *testing.T) {
		got, err := ReadFile(root, "sub/dir/evidence.json", digest, "test", "test_path")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != string(content) {
			t.Fatalf("content mismatch: got %s want %s", got, content)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		if _, err := ReadFile(root, "", digest, "test", "test_path"); err == nil {
			t.Fatal("empty path must be rejected")
		}
	})

	t.Run("absolute path escapes", func(t *testing.T) {
		if _, err := ReadFile(root, "/etc/passwd", digest, "test", "test_path"); err == nil {
			t.Fatal("absolute path must be rejected")
		}
	})

	t.Run("dot-dot escapes", func(t *testing.T) {
		if _, err := ReadFile(root, "../outside.json", digest, "test", "test_path"); err == nil {
			t.Fatal("../ must be rejected")
		}
		outside := filepath.Join(filepath.Dir(root), "outside.json")
		if err := os.WriteFile(outside, content, 0o600); err != nil {
			t.Fatalf("write outside: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(outside) })
		if _, err := ReadFile(root, "sub/../../outside.json", digest, "test", "test_path"); err == nil {
			t.Fatal("a cleaned path that escapes the root must be rejected")
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		wrong := "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"[:64]
		if _, err := ReadFile(root, "sub/dir/evidence.json", wrong, "test", "test_path"); err == nil {
			t.Fatal("digest mismatch must be rejected")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := ReadFile(root, "does/not/exist.json", digest, "test", "test_path"); err == nil {
			t.Fatal("missing file must be rejected")
		}
	})

	t.Run("symlinked directory component rejected", func(t *testing.T) {
		linkRoot := t.TempDir()
		realDir := filepath.Join(linkRoot, "real")
		if err := os.MkdirAll(realDir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		linkPath := filepath.Join(linkRoot, "linked")
		if err := os.Symlink(realDir, linkPath); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		d := writeEvidence(t, linkRoot, "real/evidence.json", content)
		if _, err := ReadFile(linkRoot, "linked/evidence.json", d, "test", "test_path"); err == nil {
			t.Fatal("a symlinked directory component must be rejected")
		}
	})

	t.Run("symlinked file rejected", func(t *testing.T) {
		linkRoot := t.TempDir()
		d := writeEvidence(t, linkRoot, "real.json", content)
		linkPath := filepath.Join(linkRoot, "linked.json")
		if err := os.Symlink(filepath.Join(linkRoot, "real.json"), linkPath); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		if _, err := ReadFile(linkRoot, "linked.json", d, "test", "test_path"); err == nil {
			t.Fatal("a symlinked file must be rejected")
		}
	})

	t.Run("symlink escaping the root rejected", func(t *testing.T) {
		outerRoot := t.TempDir()
		trustedRoot := filepath.Join(outerRoot, "trusted")
		if err := os.MkdirAll(trustedRoot, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		outsideFile := filepath.Join(outerRoot, "outside.json")
		if err := os.WriteFile(outsideFile, content, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		linkPath := filepath.Join(trustedRoot, "escape.json")
		if err := os.Symlink(outsideFile, linkPath); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		d := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
		if _, err := ReadFile(trustedRoot, "escape.json", d, "test", "test_path"); err == nil {
			t.Fatal("a symlink resolving outside the evidence root must be rejected")
		}
	})
}

type strictTarget struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func TestDecodeStrictObject_Matrix(t *testing.T) {
	t.Run("accepts a well-formed single value", func(t *testing.T) {
		var out strictTarget
		if err := DecodeStrictObject([]byte(`{"name":"a","n":1}`), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Name != "a" || out.N != 1 {
			t.Fatalf("decode result: %+v", out)
		}
	})

	t.Run("rejects unknown fields", func(t *testing.T) {
		var out strictTarget
		if err := DecodeStrictObject([]byte(`{"name":"a","n":1,"bogus":true}`), &out); err == nil {
			t.Fatal("unknown field must be rejected")
		}
	})

	t.Run("rejects duplicate top-level keys", func(t *testing.T) {
		var out strictTarget
		if err := DecodeStrictObject([]byte(`{"name":"a","name":"b","n":1}`), &out); err == nil {
			t.Fatal("duplicate top-level key must be rejected")
		}
	})

	t.Run("rejects duplicate nested keys", func(t *testing.T) {
		type nested struct {
			Obj map[string]any `json:"obj"`
		}
		var out nested
		if err := DecodeStrictObject([]byte(`{"obj":{"x":1,"y":2,"x":3}}`), &out); err == nil {
			t.Fatal("duplicate nested object key must be rejected")
		}
	})

	t.Run("rejects duplicate keys inside array elements", func(t *testing.T) {
		type withList struct {
			Items []map[string]any `json:"items"`
		}
		var out withList
		if err := DecodeStrictObject([]byte(`{"items":[{"a":1},{"b":2,"b":3}]}`), &out); err == nil {
			t.Fatal("duplicate key inside an array element must be rejected")
		}
	})

	t.Run("rejects trailing content", func(t *testing.T) {
		var out strictTarget
		if err := DecodeStrictObject([]byte(`{"name":"a","n":1} {"extra":true}`), &out); err == nil {
			t.Fatal("trailing content after the value must be rejected")
		}
	})

	t.Run("rejects trailing garbage", func(t *testing.T) {
		var out strictTarget
		if err := DecodeStrictObject([]byte(`{"name":"a","n":1}garbage`), &out); err == nil {
			t.Fatal("trailing garbage must be rejected")
		}
	})

	t.Run("rejects malformed json", func(t *testing.T) {
		var out strictTarget
		if err := DecodeStrictObject([]byte(`{"name":`), &out); err == nil {
			t.Fatal("malformed json must be rejected")
		}
	})
}

func TestCanonical_SortedKeysNoWhitespaceNoEscapeHTML(t *testing.T) {
	v := map[string]any{
		"z": 1,
		"a": map[string]any{"y": 2, "b": 1},
		"m": "<script>&</script>",
	}
	got, err := Canonical(v)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	want := `{"a":{"b":1,"y":2},"m":"<script>&</script>","z":1}`
	if string(got) != want {
		t.Fatalf("canonical bytes:\n got: %s\nwant: %s", got, want)
	}
}

func TestCanonical_Deterministic(t *testing.T) {
	v1 := map[string]any{"b": 2, "a": 1}
	v2 := map[string]any{"a": 1, "b": 2}
	c1, err := Canonical(v1)
	if err != nil {
		t.Fatalf("canonical v1: %v", err)
	}
	c2, err := Canonical(v2)
	if err != nil {
		t.Fatalf("canonical v2: %v", err)
	}
	if string(c1) != string(c2) {
		t.Fatalf("canonical encoding must be independent of map insertion order: %s vs %s", c1, c2)
	}
}
