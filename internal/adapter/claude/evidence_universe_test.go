package claude

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeUniverse(t *testing.T, root string, rel string, tools []string) (string, string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := map[string]any{
		"claude_code_version": "2.1.278",
		"tools":               tools,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p, "sha256:" + fmt.Sprintf("%x", sha256.Sum256(b))
}

func TestResolveUniverseEvidence_ValidResolution(t *testing.T) {
	root := t.TempDir()
	_, want := writeUniverse(t, root, "docs/superpowers/evidence/universe.json",
		[]string{"Task", "Bash", "Read"})

	u, err := ResolveUniverseEvidence(root,
		"docs/superpowers/evidence/universe.json", want)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if u.ClaudeCodeVersion != "2.1.278" {
		t.Fatalf("version: %q", u.ClaudeCodeVersion)
	}
	if len(u.Tools) != 3 || u.Tools[0] != "Task" {
		t.Fatalf("tools: %v", u.Tools)
	}
}

func TestResolveUniverseEvidence_ContainmentFailures(t *testing.T) {
	root := t.TempDir()
	_, wantDigest := writeUniverse(t, root, "docs/evidence/universe.json", []string{"Read"})

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "universe.json")
	if err := os.WriteFile(outsideFile, []byte(`{"tools":["Read"]}`), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	cases := []struct {
		name string
		rel  string
	}{
		{"parent traversal", "../evidence/universe.json"},
		{"absolute path", outsideFile},
		{"deep escape", "docs/../../outside/universe.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveUniverseEvidence(root, tc.rel, wantDigest); err == nil {
				t.Fatal("escape must be rejected")
			} else if !strings.Contains(err.Error(), "evidence root") {
				t.Fatalf("expected containment error, got %v", err)
			}
		})
	}
}

func TestResolveUniverseEvidence_SymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "universe.json")
	if err := os.WriteFile(outsideFile, []byte(`{"tools":["Read"]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(root, "link.jsonl")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rel, err := filepath.Rel(root, link)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	if _, err := ResolveUniverseEvidence(root, filepath.ToSlash(rel), "sha256:x"); err == nil {
		t.Fatal("symlinked evidence file must be rejected")
	}
}

func TestResolveUniverseEvidence_DigestMismatchRejected(t *testing.T) {
	root := t.TempDir()
	writeUniverse(t, root, "docs/evidence/universe.json", []string{"Read"})
	if _, err := ResolveUniverseEvidence(root, "docs/evidence/universe.json",
		"sha256:0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("digest mismatch must be rejected")
	}
}

func TestResolveUniverseEvidence_MalformedDocumentRejected(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "docs", "evidence", "universe.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(`{"claude_code_version":"2.1.278"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	d := "sha256:" + fmt.Sprintf("%x", sha256.Sum256([]byte(`{"claude_code_version":"2.1.278"}`)))
	if _, err := ResolveUniverseEvidence(root, "docs/evidence/universe.json", d); err == nil {
		t.Fatal("universe without tools must be rejected")
	}
}
