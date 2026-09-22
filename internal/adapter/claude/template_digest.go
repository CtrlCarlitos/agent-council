package claude

// ctmpl-v1 template digest and per-session config-root trust helpers.
// Digest framing (spec §3.11): uint32-BE(entry count), then per entry in
// byte-wise lexicographic normalized-path order:
//   uint32-BE(len(path-bytes)) || path-bytes ||
//   uint32-BE(len(file-bytes)) || raw file bytes
// Paths are relative forward-slash UTF-8; symlinks and non-regular files
// are rejected; duplicate normalized paths are rejected.

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

func u32be(n uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return b
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

// runtimeTranscriptDir is reserved by §3.6: transcripts live under
// projects/ inside the per-session config root, owned by the runtime
// and the transcript trust model. An operator template may not define
// it — otherwise the frozen digest (whole template) and the copied
// root digest (excluding the runtime subtree) could never agree.
const runtimeTranscriptDir = "projects"

// validateTemplateReservesRuntimeDirs rejects a template that defines
// the reserved runtime transcript directory.
func validateTemplateReservesRuntimeDirs(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read template tree %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.Name() == runtimeTranscriptDir {
			return fmt.Errorf("template defines %q, which is reserved for runtime transcripts (§3.6)", runtimeTranscriptDir)
		}
	}
	return nil
}

// TemplateDigest computes ctmpl-v1:sha256:<hex> over the template tree.
func TemplateDigest(dir string) (string, error) {
	return TemplateDigestExcluding(dir, nil)
}

// TemplateDigestExcluding computes ctmpl-v1:sha256:<hex> over the tree,
// skipping any top-level directory named in excludeRel. The §3.6
// runtime transcript subtree (projects/) lives inside the per-session
// config root and is owned by the transcript trust model, so the
// frozen-digest comparison of a materialized root excludes it.
func TemplateDigestExcluding(dir string, excludeRel []string) (string, error) {
	excluded := make(map[string]struct{}, len(excludeRel))
	for _, rel := range excludeRel {
		excluded[rel] = struct{}{}
	}
	type entry struct {
		rel  string
		data []byte
	}
	var entries []entry
	seen := make(map[string]struct{})

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir {
				if rel, rerr := filepath.Rel(dir, path); rerr == nil {
					top, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
					if _, skip := excluded[top]; skip {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("template contains symlink: %s", path)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("template contains non-regular file: %s", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !utf8.ValidString(rel) {
			return fmt.Errorf("template path is not valid UTF-8: %q", path)
		}
		if rel == "." || rel == "" || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("template path escapes the tree: %s", rel)
		}
		if _, dup := seen[rel]; dup {
			return fmt.Errorf("duplicate normalized template path: %s", rel)
		}
		seen[rel] = struct{}{}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel: rel, data: data})
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("template tree is empty")
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].rel < entries[j].rel
	})

	buf := make([]byte, 0, 64)
	buf = append(buf, u32be(uint32(len(entries)))...)
	for _, e := range entries {
		buf = append(buf, u32be(uint32(len(e.rel)))...)
		buf = append(buf, e.rel...)
		buf = append(buf, u32be(uint32(len(e.data)))...)
		buf = append(buf, e.data...)
	}
	return "ctmpl-v1:sha256:" + sha256Hex(buf), nil
}
