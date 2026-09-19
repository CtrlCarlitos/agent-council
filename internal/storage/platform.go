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

// parseAndVerifyIcaclsOutput inspects icacls output lines to ensure that:
// 1. At least one valid security descriptor entry exists (empty or summary-only output is rejected).
// 2. Every non-empty, non-summary line is a syntactically valid permission entry (unparsed/malformed lines are rejected).
// 3. Only explicitly authorized identities (exact SIDs or canonical well-known names) are granted access.
func parseAndVerifyIcaclsOutput(targetPath string, outStr string) error {
	trimmed := strings.TrimSpace(outStr)
	if trimmed == "" {
		return fmt.Errorf("insecure ACL on Windows path %s: empty icacls inspection output", targetPath)
	}

	lines := strings.Split(outStr, "\n")

	cleanTarget := filepath.Clean(targetPath)
	baseTarget := filepath.Base(targetPath)

	verifiedEntries := 0

	for lineNum, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		// Skip standard icacls summary lines
		if strings.HasPrefix(line, "Successfully processed") || strings.HasPrefix(line, "Failed processing") {
			continue
		}

		// Look for permission delimiter ":("
		idx := strings.Index(line, ":(")
		if idx == -1 {
			return fmt.Errorf("insecure ACL on Windows path %s: unparsed or malformed entry on line %d: %q", targetPath, lineNum+1, line)
		}

		// Candidate principal string is before ":("
		principalPart := strings.TrimSpace(line[:idx])

		// Strip leading path component if present (e.g. on first line)
		lowerPrincipal := strings.ToLower(principalPart)
		if strings.HasPrefix(lowerPrincipal, strings.ToLower(cleanTarget)) {
			principalPart = strings.TrimSpace(principalPart[len(cleanTarget):])
		} else if strings.HasPrefix(lowerPrincipal, strings.ToLower(targetPath)) {
			principalPart = strings.TrimSpace(principalPart[len(targetPath):])
		} else if strings.HasPrefix(lowerPrincipal, strings.ToLower(baseTarget)) {
			principalPart = strings.TrimSpace(principalPart[len(baseTarget):])
		}

		principalPart = strings.TrimSpace(principalPart)
		if principalPart == "" {
			return fmt.Errorf("insecure ACL on Windows path %s: missing principal on line %d: %q", targetPath, lineNum+1, line)
		}

		// Verify permission rights string after ":"
		rightsPart := strings.TrimSpace(line[idx+1:])
		if !strings.HasPrefix(rightsPart, "(") || !strings.HasSuffix(rightsPart, ")") {
			return fmt.Errorf("insecure ACL on Windows path %s: malformed permission rights on line %d: %q", targetPath, lineNum+1, line)
		}

		// Check disallowed broad group SIDs and names
		if isDisallowedWindowsPrincipal(principalPart) {
			return fmt.Errorf("insecure ACL on Windows path %s: contains disallowed principal %q", targetPath, principalPart)
		}

		// Verify that principal is an explicitly authorized exact identity
		if !isAuthorizedWindowsPrincipal(principalPart) {
			return fmt.Errorf("insecure ACL on Windows path %s: unauthorized principal %q", targetPath, principalPart)
		}

		verifiedEntries++
	}

	if verifiedEntries == 0 {
		return fmt.Errorf("insecure ACL on Windows path %s: no security descriptor entries were verified", targetPath)
	}

	return nil
}

func isDisallowedWindowsPrincipal(p string) bool {
	clean := strings.TrimPrefix(strings.TrimSpace(p), "*")
	lower := strings.ToLower(clean)

	disallowedSIDs := []string{
		"s-1-1-0",      // Everyone
		"s-1-5-32-545", // Users
		"s-1-5-11",     // Authenticated Users
		"s-1-5-4",      // Interactive
		"s-1-5-7",      // Anonymous
		"s-1-5-32-546", // Guests
	}
	for _, ds := range disallowedSIDs {
		if strings.EqualFold(clean, ds) || strings.Contains(lower, ds) {
			return true
		}
	}

	disallowedExactNames := []string{
		"everyone",
		"builtin\\users",
		"users",
		"nt authority\\authenticated users",
		"authenticated users",
		"nt authority\\interactive",
		"interactive",
		"nt authority\\anonymous logon",
		"anonymous logon",
		"guests",
		"builtin\\guests",
	}
	for _, dn := range disallowedExactNames {
		if strings.EqualFold(lower, dn) {
			return true
		}
	}
	return false
}

func isAuthorizedWindowsPrincipal(p string) bool {
	clean := strings.TrimPrefix(strings.TrimSpace(p), "*")
	lower := strings.ToLower(clean)

	// 1. Owner Rights: exact SID S-1-3-4 or exact canonical name
	if clean == "S-1-3-4" || lower == "nt authority\\owner rights" || lower == "owner rights" {
		return true
	}

	// 2. Local System: exact SID S-1-5-18 or exact canonical name
	if clean == "S-1-5-18" || lower == "nt authority\\system" || lower == "system" {
		return true
	}

	// 3. Builtin Administrators: exact SID S-1-5-32-544 or exact canonical name
	if clean == "S-1-5-32-544" || lower == "builtin\\administrators" || lower == "administrators" {
		return true
	}

	// 4. Current user: exact UID (SID) or exact username
	if u, err := user.Current(); err == nil {
		if u.Uid != "" && strings.EqualFold(clean, u.Uid) {
			return true
		}
		if u.Username != "" {
			if strings.EqualFold(clean, u.Username) || strings.EqualFold(lower, strings.ToLower(u.Username)) {
				return true
			}
			parts := strings.Split(u.Username, "\\")
			if len(parts) == 2 && strings.EqualFold(lower, strings.ToLower(parts[1])) {
				return true
			}
		}
	}

	// 5. Current user environment variables (exact match only)
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
