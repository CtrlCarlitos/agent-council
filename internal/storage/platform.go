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

var validRightsTokens = map[string]bool{
	// Inheritance flags
	"OI": true, // Object inherit
	"CI": true, // Container inherit
	"IO": true, // Inherit only
	"NP": true, // Do not propagate inherit
	"I":  true, // Inherited from parent container

	// Standard permissions
	"F":  true, // Full access
	"M":  true, // Modify access
	"RX": true, // Read and execute
	"R":  true, // Read-only
	"W":  true, // Write-only
	"D":  true, // Delete

	// Advanced / specific rights
	"WDAC": true, // Write DAC
	"WO":   true, // Write owner
	"RC":   true, // Read control
	"S":    true, // Synchronize
	"AS":   true, // Access system security
	"MA":   true, // Maximum allowed
	"GR":   true, // Generic read
	"GW":   true, // Generic write
	"GE":   true, // Generic execute
	"GA":   true, // Generic all
	"RD":   true, // Read data / list directory
	"WD":   true, // Write data / add file
	"AD":   true, // Append data / add subdirectory
	"REA":  true, // Read extended attributes
	"WEA":  true, // Write extended attributes
	"X":    true, // Execute file / traverse directory
	"DC":   true, // Delete child
	"RA":   true, // Read attributes
	"WA":   true, // Write attributes
}

// parseRightsTokens parses and validates parenthesized rights tokens like "(OI)(CI)(F)".
func parseRightsTokens(rightsStr string) ([]string, error) {
	trimmed := strings.TrimSpace(rightsStr)
	if trimmed == "" {
		return nil, fmt.Errorf("empty permission rights")
	}

	if !strings.HasPrefix(trimmed, "(") || !strings.HasSuffix(trimmed, ")") {
		return nil, fmt.Errorf("malformed permission rights syntax: %q", rightsStr)
	}

	var tokens []string
	pos := 0
	for pos < len(trimmed) {
		if trimmed[pos] != '(' {
			return nil, fmt.Errorf("malformed permission rights token at pos %d: %q", pos, rightsStr)
		}
		closeIdx := strings.IndexByte(trimmed[pos:], ')')
		if closeIdx == -1 {
			return nil, fmt.Errorf("unclosed parenthesis in permission rights: %q", rightsStr)
		}
		token := trimmed[pos+1 : pos+closeIdx]
		if token == "" {
			return nil, fmt.Errorf("empty permission token '()' in %q", rightsStr)
		}
		if strings.ContainsAny(token, "()") {
			return nil, fmt.Errorf("nested parenthesis in permission token %q", rightsStr)
		}
		if !validRightsTokens[strings.ToUpper(token)] {
			return nil, fmt.Errorf("unsupported access right token %q in %q", token, rightsStr)
		}
		tokens = append(tokens, token)
		pos += closeIdx + 1
	}

	if len(tokens) == 0 {
		return nil, fmt.Errorf("no valid permission tokens in %q", rightsStr)
	}
	return tokens, nil
}

// parseAndVerifyIcaclsOutput inspects icacls output lines to ensure that:
// 1. At least one valid security descriptor entry exists (empty or summary-only output is rejected).
// 2. Every non-empty, non-summary line is a syntactically valid permission entry (unparsed/malformed lines are rejected).
// 3. Only explicitly authorized identities (exact SIDs or canonical well-known names) are granted access.
// 4. Path stripping is strictly limited to line 0 with trailing whitespace, preventing principal corruption.
func parseAndVerifyIcaclsOutput(targetPath string, outStr string) error {
	trimmed := strings.TrimSpace(outStr)
	if trimmed == "" {
		return fmt.Errorf("insecure ACL on Windows path %s: empty icacls inspection output", targetPath)
	}

	lines := strings.Split(outStr, "\n")

	cleanTarget := filepath.Clean(targetPath)
	verifiedEntries := 0
	firstEntrySeen := false

	for lineNum, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		// Verify*Permissions inspects one object. Only this complete successful
		// summary is ignorable; a pathname or principal can share its prefix.
		// Unsupported/malformed output and permission entries must be parsed below.
		if line == "Successfully processed 1 files; Failed processing 0 files" {
			continue
		}

		// Look for permission delimiter ":("
		idx := strings.Index(line, ":(")
		if idx == -1 {
			return fmt.Errorf("insecure ACL on Windows path %s: unparsed or malformed entry on line %d: %q", targetPath, lineNum+1, line)
		}

		// On the very first entry line, icacls prefixes the output with the target pathname,
		// followed by whitespace (e.g. "C:\dir *S-1-3-4:(OI)(CI)(F)").
		// Continuation lines are indented and NEVER contain a pathname.
		// We only strip the pathname on the first entry line, and ONLY when followed by whitespace.
		if !firstEntrySeen {
			firstEntrySeen = true
			lowerLine := strings.ToLower(line)
			lowerClean := strings.ToLower(cleanTarget)
			lowerTarget := strings.ToLower(targetPath)
			trimmedTarget := strings.TrimRight(targetPath, "/\\")
			lowerTrimmed := strings.ToLower(trimmedTarget)

			if strings.HasPrefix(lowerLine, lowerClean+" ") || strings.HasPrefix(lowerLine, lowerClean+"\t") {
				line = strings.TrimSpace(line[len(cleanTarget):])
			} else if strings.HasPrefix(lowerLine, lowerTarget+" ") || strings.HasPrefix(lowerLine, lowerTarget+"\t") {
				line = strings.TrimSpace(line[len(targetPath):])
			} else if strings.HasPrefix(lowerLine, lowerTrimmed+" ") || strings.HasPrefix(lowerLine, lowerTrimmed+"\t") {
				line = strings.TrimSpace(line[len(trimmedTarget):])
			}
			idx = strings.Index(line, ":(")
			if idx == -1 {
				return fmt.Errorf("insecure ACL on Windows path %s: malformed line 0 after path stripping: %q", targetPath, line)
			}
		}

		principalPart := strings.TrimSpace(line[:idx])
		if principalPart == "" {
			return fmt.Errorf("insecure ACL on Windows path %s: missing principal on line %d: %q", targetPath, lineNum+1, line)
		}

		rightsPart := strings.TrimSpace(line[idx+1:]) // starts with '('
		rightsTokens, err := parseRightsTokens(rightsPart)
		if err != nil {
			return fmt.Errorf("insecure ACL on Windows path %s on line %d: %w", targetPath, lineNum+1, err)
		}
		_ = rightsTokens

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

	// 4. Current user: exact UID (SID) or exact username from OS token context via user.Current().
	// Never use USERNAME or USERDOMAIN environment variables, which can be modified or spoofed.
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
