package claude

// Per-session config-root materialization (spec §3.2): the operator
// template is copied ATOMICALLY into <base>/<run>/<session>/config with
// secure permissions; invalid templates fail closed and leave nothing
// behind.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	maxTemplateFileBytes = 1 << 20 // 1 MiB per file
	maxTemplateTotal     = 8 << 20 // 8 MiB total
	maxTemplateFiles     = 512
)

var runtimeGOOSWindows = runtime.GOOS == "windows"

// MaterializeConfigRoot copies the operator template into
// <base>/<runID>/<sessionID>/config atomically (temporary directory plus
// rename) and returns the materialized root with its ctmpl-v1 digest.
// Fail closed: invalid templates leave nothing behind.
func MaterializeConfigRoot(templateDir, base, runID, sessionID string) (string, string, error) {
	// Validate the template before touching the destination.
	wantDigest, err := TemplateDigest(templateDir)
	if err != nil {
		return "", "", fmt.Errorf("validate template: %w", err)
	}

	if strings.TrimSpace(runID) == "" || strings.TrimSpace(sessionID) == "" {
		return "", "", fmt.Errorf("run and session IDs are required to materialize a config root")
	}
	if strings.Contains(runID, "/") || strings.Contains(sessionID, "/") ||
		runID == "." || runID == ".." || sessionID == "." || sessionID == ".." {
		return "", "", fmt.Errorf("run/session IDs must be plain path components")
	}

	finalRoot := filepath.Join(base, runID, sessionID, "config")
	if _, err := os.Lstat(finalRoot); err == nil {
		return "", "", fmt.Errorf("config root %s already exists", finalRoot)
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("stat config root: %w", err)
	}

	tmpRoot := finalRoot + ".tmp"
	_ = os.RemoveAll(tmpRoot)
	if err := os.MkdirAll(filepath.Dir(tmpRoot), 0o700); err != nil {
		return "", "", fmt.Errorf("create parent directories: %w", err)
	}
	if err := os.Mkdir(tmpRoot, 0o700); err != nil {
		return "", "", fmt.Errorf("create temporary root: %w", err)
	}

	if err := copyTree(templateDir, tmpRoot); err != nil {
		_ = os.RemoveAll(tmpRoot)
		return "", "", fmt.Errorf("copy template: %w", err)
	}
	if runtimeGOOSWindows {
		// Windows cannot express the POSIX mode contract: the security
		// posture degrades and callers must treat the root as
		// unverified (spec §3.6 fail-closed statement).
		_ = os.Chmod(tmpRoot, 0o700)
	} else if err := os.Chmod(tmpRoot, 0o700); err != nil {
		_ = os.RemoveAll(tmpRoot)
		return "", "", fmt.Errorf("secure temporary root: %w", err)
	}

	if err := os.Rename(tmpRoot, finalRoot); err != nil {
		_ = os.RemoveAll(tmpRoot)
		return "", "", fmt.Errorf("publish config root: %w", err)
	}

	digest, err := TemplateDigest(finalRoot)
	if err != nil {
		return "", "", fmt.Errorf("digest materialized root: %w", err)
	}
	if digest != wantDigest {
		return "", "", fmt.Errorf("materialized root digest %q does not match the template %q", digest, wantDigest)
	}
	return finalRoot, digest, nil
}

func copyTree(src, dst string) error {
	var total int64
	count := 0
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("symlink in template: %s", path)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("non-regular file in template: %s", path)
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(data) > maxTemplateFileBytes {
			return fmt.Errorf("template file %s exceeds %d bytes", rel, maxTemplateFileBytes)
		}
		total += int64(len(data))
		if total > maxTemplateTotal {
			return fmt.Errorf("template exceeds %d total bytes", maxTemplateTotal)
		}
		count++
		if count > maxTemplateFiles {
			return fmt.Errorf("template exceeds %d files", maxTemplateFiles)
		}
		if runtimeGOOSWindows {
			return os.WriteFile(target, data, 0o600)
		}
		return os.WriteFile(target, data, 0o600)
	})
}
