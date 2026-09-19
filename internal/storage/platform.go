package storage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// EnsureDirectoryPermissions ensures directory permissions are restricted to owner (0700 on POSIX).
func EnsureDirectoryPermissions(dir string) error {
	if runtime.GOOS == "windows" {
		// Restrict ACL to current user if icacls is available
		cmd := exec.Command("icacls", dir, "/inheritance:r", "/grant:r", "*S-1-3-4:(OI)(CI)F")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("icacls dir failed: %w", err)
		}
		return VerifyDirectoryPermissions(dir)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	return VerifyDirectoryPermissions(dir)
}

// EnsureFilePermissions ensures file permissions are restricted to owner (0600 on POSIX).
func EnsureFilePermissions(file string) error {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("icacls", file, "/inheritance:r", "/grant:r", "*S-1-3-4:F")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("icacls file failed: %w", err)
		}
		return VerifyFilePermissions(file)
	}
	if err := os.Chmod(file, 0600); err != nil {
		return err
	}
	return VerifyFilePermissions(file)
}

// VerifyDirectoryPermissions checks that directory permissions are restricted to owner (0700 on POSIX, no broad groups on Windows).
func VerifyDirectoryPermissions(dir string) error {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("icacls", dir)
		out, err := cmd.Output()
		if err != nil {
			return nil // skip if icacls unavailable
		}
		outStr := string(out)
		disallowed := []string{"Everyone", "BUILTIN\\Users", "NT AUTHORITY\\Authenticated Users"}
		for _, p := range disallowed {
			if strings.Contains(outStr, p) {
				return fmt.Errorf("insecure ACL on Windows directory %s: contains %s", dir, p)
			}
		}
		return nil
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("insecure directory permissions: %v", fi.Mode().Perm())
	}
	return nil
}

// VerifyFilePermissions checks that file permissions are restricted to owner (0600 on POSIX, no broad groups on Windows).
func VerifyFilePermissions(file string) error {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("icacls", file)
		out, err := cmd.Output()
		if err != nil {
			return nil
		}
		outStr := string(out)
		disallowed := []string{"Everyone", "BUILTIN\\Users", "NT AUTHORITY\\Authenticated Users"}
		for _, p := range disallowed {
			if strings.Contains(outStr, p) {
				return fmt.Errorf("insecure ACL on Windows file %s: contains %s", file, p)
			}
		}
		return nil
	}
	fi, err := os.Stat(file)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("insecure file permissions: %v", fi.Mode().Perm())
	}
	return nil
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
