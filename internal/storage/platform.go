package storage

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
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
			return fmt.Errorf("icacls read-back failed for %s: %w", dir, err)
		}
		return parseAndVerifyIcaclsOutput(dir, string(out))
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
			return fmt.Errorf("icacls read-back failed for %s: %w", file, err)
		}
		return parseAndVerifyIcaclsOutput(file, string(out))
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

// parseAndVerifyIcaclsOutput inspects icacls output lines to ensure that only authorized
// principals (current user, Owner Rights, System, Administrators) have permissions,
// and rejects any unauthorized or broad group grants.
func parseAndVerifyIcaclsOutput(targetPath string, outStr string) error {
	lines := strings.Split(outStr, "\n")

	disallowedSIDs := []string{
		"S-1-1-0",      // Everyone
		"S-1-5-32-545", // Users
		"S-1-5-11",     // Authenticated Users
		"S-1-5-4",      // Interactive
		"S-1-5-7",      // Anonymous
		"S-1-5-32-546", // Guests
	}
	disallowedNames := []string{
		"everyone",
		"builtin\\users",
		"users",
		"nt authority\\authenticated users",
		"authenticated users",
		"nt authority\\interactive",
		"interactive",
		"nt authority\\anonymous logon",
		"guests",
	}

	cleanTarget := filepath.Clean(targetPath)
	baseTarget := filepath.Base(targetPath)

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "Successfully processed") || strings.HasPrefix(line, "Failed processing") {
			continue
		}

		idx := strings.Index(line, ":(")
		if idx == -1 {
			continue
		}

		principalPart := strings.TrimSpace(line[:idx])

		// Strip leading path component if present
		if strings.HasPrefix(principalPart, cleanTarget) {
			principalPart = strings.TrimSpace(strings.TrimPrefix(principalPart, cleanTarget))
		} else if strings.HasPrefix(principalPart, targetPath) {
			principalPart = strings.TrimSpace(strings.TrimPrefix(principalPart, targetPath))
		} else if strings.HasPrefix(principalPart, baseTarget) {
			principalPart = strings.TrimSpace(strings.TrimPrefix(principalPart, baseTarget))
		}

		lower := strings.ToLower(principalPart)
		for _, ds := range disallowedSIDs {
			if strings.Contains(principalPart, ds) {
				return fmt.Errorf("insecure ACL on Windows path %s: contains disallowed SID %s", targetPath, ds)
			}
		}
		for _, dn := range disallowedNames {
			if strings.Contains(lower, dn) {
				return fmt.Errorf("insecure ACL on Windows path %s: contains disallowed principal %s", targetPath, dn)
			}
		}

		if !isAuthorizedWindowsPrincipal(principalPart) {
			return fmt.Errorf("insecure ACL on Windows path %s: unauthorized principal %q", targetPath, principalPart)
		}
	}
	return nil
}

func isAuthorizedWindowsPrincipal(p string) bool {
	clean := strings.TrimPrefix(strings.TrimSpace(p), "*")
	lower := strings.ToLower(clean)

	if clean == "S-1-3-4" || strings.Contains(lower, "owner rights") {
		return true
	}
	if clean == "S-1-5-18" || lower == "system" || strings.Contains(lower, "nt authority\\system") {
		return true
	}
	if clean == "S-1-5-32-544" || strings.Contains(lower, "administrators") {
		return true
	}

	if u, err := user.Current(); err == nil {
		if u.Uid != "" && strings.EqualFold(clean, u.Uid) {
			return true
		}
		if u.Username != "" && (strings.EqualFold(clean, u.Username) || strings.EqualFold(lower, strings.ToLower(u.Username))) {
			return true
		}
	}

	if envUser := os.Getenv("USERNAME"); envUser != "" {
		if strings.EqualFold(lower, strings.ToLower(envUser)) {
			return true
		}
		if envDomain := os.Getenv("USERDOMAIN"); envDomain != "" {
			full := strings.ToLower(envDomain + "\\" + envUser)
			if strings.EqualFold(lower, full) {
				return true
			}
		}
	}

	return false
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
