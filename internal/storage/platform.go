package storage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// EnsureDirectoryPermissions ensures directory permissions are restricted to owner (0700 on POSIX).
func EnsureDirectoryPermissions(dir string) error {
	if runtime.GOOS == "windows" {
		// Restrict ACL to current user if icacls is available
		cmd := exec.Command("icacls", dir, "/inheritance:r", "/grant:r", "*S-1-3-4:(OI)(CI)F")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("icacls dir failed: %w", err)
		}
		return nil
	}
	return os.Chmod(dir, 0700)
}

// EnsureFilePermissions ensures file permissions are restricted to owner (0600 on POSIX).
func EnsureFilePermissions(file string) error {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("icacls", file, "/inheritance:r", "/grant:r", "*S-1-3-4:F")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("icacls file failed: %w", err)
		}
		return nil
	}
	return os.Chmod(file, 0600)
}

// TightenStateDirPermissions ensures the state directory and its database/sidecar files have strict permissions.
func (s *Store) TightenStateDirPermissions() error {
	if err := EnsureDirectoryPermissions(s.stateDir); err != nil {
		return err
	}
	// Check and tighten state.db and sidecar files
	files, err := os.ReadDir(s.stateDir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if !f.IsDir() {
			path := filepath.Join(s.stateDir, f.Name())
			if err := EnsureFilePermissions(path); err != nil {
				return err
			}
		}
	}
	return nil
}
