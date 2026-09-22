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

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const (
	maxTemplateFileBytes = 1 << 20 // 1 MiB per file
	maxTemplateTotal     = 8 << 20 // 8 MiB total
	maxTemplateFiles     = 512
)

var runtimeGOOSWindows = runtime.GOOS == "windows"

// digestTree is an indirection so tests can inject a post-rename
// verification failure.
var digestTree = TemplateDigest

// ConfigRootPath derives the per-session config root path from the base
// (spec §3.2 layout: <base>/<run-id>/<session-id>/config).
func ConfigRootPath(base, runID, sessionID string) string {
	return filepath.Join(base, runID, sessionID, "config")
}

// MaterializeConfigRoot copies the operator template into
// <base>/<runID>/<sessionID>/config atomically (uniquely named temporary
// directory plus rename, reserved under the destination's parent so the
// rename stays on one filesystem) and returns the materialized root with
// its ctmpl-v1 digest. Fail closed: invalid templates and post-rename
// verification failures leave nothing published.
func MaterializeConfigRoot(templateDir, base, runID, sessionID string) (string, string, error) {
	// Validate the template before touching the destination.
	wantDigest, err := TemplateDigest(templateDir)
	if err != nil {
		return "", "", fmt.Errorf("validate template: %w", err)
	}

	if err := storage.ValidateSafeIdentifier("run ID", runID); err != nil {
		return "", "", fmt.Errorf("invalid run ID: %w", err)
	}
	if err := storage.ValidateSafeIdentifier("session ID", sessionID); err != nil {
		return "", "", fmt.Errorf("invalid session ID: %w", err)
	}

	finalRoot := filepath.Join(base, runID, sessionID, "config")
	if _, err := os.Lstat(finalRoot); err == nil {
		return "", "", fmt.Errorf("config root %s already exists", finalRoot)
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("stat config root: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalRoot), 0o700); err != nil {
		return "", "", fmt.Errorf("create parent directories: %w", err)
	}

	// UNIQUE per-call temporary directory under the destination's parent:
	// concurrent materializations never share or delete each other's
	// work, and the rename stays on one filesystem.
	tmpRoot, err := os.MkdirTemp(filepath.Dir(finalRoot), ".config-tmp-")
	if err != nil {
		return "", "", fmt.Errorf("create temporary root: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(tmpRoot)
		}
	}()

	if err := copyTree(templateDir, tmpRoot); err != nil {
		return "", "", fmt.Errorf("copy template: %w", err)
	}
	if runtimeGOOSWindows {
		// Windows cannot express the POSIX mode contract: the security
		// posture degrades and callers must treat the root as
		// unverified (spec §3.6 fail-closed statement).
		_ = os.Chmod(tmpRoot, 0o700)
	} else if err := os.Chmod(tmpRoot, 0o700); err != nil {
		return "", "", fmt.Errorf("secure temporary root: %w", err)
	}

	// Pre-rename verification: the temporary tree must already match.
	digest, err := digestTree(tmpRoot)
	if err != nil {
		return "", "", fmt.Errorf("digest temporary root: %w", err)
	}
	if digest != wantDigest {
		return "", "", fmt.Errorf("materialized root digest %q does not match the template %q", digest, wantDigest)
	}

	if err := os.Rename(tmpRoot, finalRoot); err != nil {
		return "", "", fmt.Errorf("publish config root: %w", err)
	}
	published = true

	// Defensive post-rename re-verification: a failure removes the
	// published root rather than leaving a bad tree in place.
	digest, err = digestTree(finalRoot)
	if err != nil {
		_ = os.RemoveAll(finalRoot)
		return "", "", fmt.Errorf("verify published config root: %w", err)
	}
	if digest != wantDigest {
		_ = os.RemoveAll(finalRoot)
		return "", "", fmt.Errorf("published root digest %q does not match the template %q; removed", digest, wantDigest)
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
