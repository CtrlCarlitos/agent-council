package execpolicy

// Typed Claude config-dir extension (AC-008 spec §3.7): the adapter may
// attach a config directory ONLY to the exact Claude launch shape. It is
// never an inherited-environment allowlist entry and is rejected on any
// other launch shape.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrClaudeConfigDirShape reports ClaudeConfigDir on a launch that is not
// the exact approved `claude -p` invocation.
var ErrClaudeConfigDirShape = errors.New("ClaudeConfigDir is only permitted on exact `claude -p` launches")

// ClaudeLaunchSessionFlag is the identity flag slot in the exact
// contract; the CLI requires exactly one of --session-id (new) or
// --resume (existing).
const claudeLaunchCommand = "claude"

// claudeArgsTemplateIdx indexes into the exact argument contract.
const (
	idxPrint           = 0 // "-p"
	idxOutputFormat    = 1 // "--output-format"
	idxStreamJSON      = 2 // "stream-json"
	idxVerbose         = 3 // "--verbose"
	idxSessionFlag     = 4 // "--session-id" | "--resume"
	idxSessionID       = 5 // uuid
	idxModelFlag       = 6 // "--model"
	idxModelValue      = 7
	idxMaxTurnsFlag    = 8 // "--max-turns"
	idxMaxTurnsValue   = 9
	baseClaudeArgCount = 10
)

// forbiddenClaudeFlags never appear in an approved Claude launch.
var forbiddenClaudeFlags = map[string]struct{}{
	"--bare":                               {},
	"--continue":                           {},
	"-c":                                   {},
	"--fork-session":                       {},
	"--no-session-persistence":             {},
	"--dangerously-skip-permissions":       {},
	"--allow-dangerously-skip-permissions": {},
	"--permission-mode":                    {},
	"--append-system-prompt":               {},
	"--append-system-prompt-file":          {},
	"--system-prompt":                      {},
	"--system-prompt-file":                 {},
}

// IsClaudeLaunch reports whether the request matches the exact approved
// `claude -p` invocation contract: fixed prefix, exactly one session
// identity flag with a valid UUIDv4, model and positive max-turns, and
// optional tool-list pairs. Any unknown, reordered, duplicated, or extra
// argument is rejected.
func IsClaudeLaunch(req LaunchRequest) bool {
	base := req.Command
	if idx := strings.LastIndex(base, "/"); idx != -1 {
		base = base[idx+1:]
	}
	if base != claudeLaunchCommand {
		return false
	}
	args := req.Args
	if len(args) < baseClaudeArgCount {
		return false
	}
	if args[idxPrint] != "-p" ||
		args[idxOutputFormat] != "--output-format" || args[idxStreamJSON] != "stream-json" ||
		args[idxVerbose] != "--verbose" {
		return false
	}
	if args[idxSessionFlag] != "--session-id" && args[idxSessionFlag] != "--resume" {
		return false
	}
	if !isUUIDv4(args[idxSessionID]) {
		return false
	}
	if args[idxModelFlag] != "--model" || strings.TrimSpace(args[idxModelValue]) == "" {
		return false
	}
	if args[idxMaxTurnsFlag] != "--max-turns" || !isPositiveInt(args[idxMaxTurnsValue]) {
		return false
	}
	for _, a := range args {
		if _, bad := forbiddenClaudeFlags[a]; bad {
			return false
		}
	}
	// Optional trailing tool-list pairs: --allowedTools <v...> and
	// --disallowedTools <v...>, in that order, values never flagged.
	rest := args[baseClaudeArgCount:]
	i := 0
	for _, flag := range []string{"--allowedTools", "--disallowedTools"} {
		if i >= len(rest) {
			continue
		}
		if rest[i] != flag {
			return false
		}
		i++
		values := 0
		for i < len(rest) && !strings.HasPrefix(rest[i], "--") {
			if strings.TrimSpace(rest[i]) == "" {
				return false
			}
			i++
			values++
		}
		if values == 0 {
			return false
		}
	}
	return i == len(rest)
}

// validateClaudeConfigDir gates the typed extension: when set, the
// launch MUST be the exact Claude shape AND the config dir must resolve
// inside the trusted ClaudeConfigBaseDir carried on the request
// (symlink-safe, verified against the real filesystem). Unset is a
// no-op (launches without the extension are not this adapter's
// concern).
func validateClaudeConfigDir(req LaunchRequest) error {
	if strings.TrimSpace(req.ClaudeConfigDir) == "" {
		return nil
	}
	if !IsClaudeLaunch(req) {
		return fmt.Errorf("%w: launch is %q %v", ErrClaudeConfigDirShape, req.Command, req.Args)
	}
	base := strings.TrimSpace(req.ClaudeConfigBaseDir)
	if base == "" {
		return fmt.Errorf("%w: ClaudeConfigBaseDir is required with ClaudeConfigDir", ErrClaudeConfigDirShape)
	}
	if err := validateContainedDir(base, req.ClaudeConfigDir); err != nil {
		return fmt.Errorf("%w: config dir containment failed: %v", ErrClaudeConfigDirShape, err)
	}
	return nil
}

// validateContainedDir verifies, symlink-safe, that candidate is an
// existing directory inside base: the base is fully resolved, every
// candidate component below the nearest existing ancestor must be a
// real (non-symlink) directory, and the fully resolved candidate must
// remain within the resolved base.
func validateContainedDir(base, candidate string) error {
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("resolve base: %w", err)
	}
	fi, err := os.Lstat(candidate)
	if err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("config dir is a symlink")
	}
	if !fi.IsDir() {
		return fmt.Errorf("config dir is not a directory")
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return fmt.Errorf("resolve config dir: %w", err)
	}
	if !strings.HasPrefix(real, resolvedBase+string(os.PathSeparator)) {
		return fmt.Errorf("config dir %q resolves outside the claude config base %q", candidate, base)
	}
	return nil
}

func isPositiveInt(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != "0"
}

// isUUIDv4 validates RFC 4122 UUIDv4 form: 8-4-4-4-12 hex with version
// nibble 4 and RFC variant bits.
func isUUIDv4(s string) bool {
	if len(s) != 36 {
		return false
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	hexAt := func(i int) bool {
		c := s[i]
		return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
	}
	for _, i := range []int{0, 1, 2, 3, 4, 5, 6, 7, 9, 10, 11, 12, 14, 15, 16, 17, 19, 20, 21, 22, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35} {
		if !hexAt(i) {
			return false
		}
	}
	if s[14] != '4' { // version nibble
		return false
	}
	variant := s[19]
	return variant == '8' || variant == '9' || variant == 'a' || variant == 'b'
}

// uuidFromArgs extracts the session UUID from the exact contract (test
// helper and validator).
func uuidFromArgs(args []string) (string, error) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--session-id" || args[i] == "--resume" {
			if !isUUIDv4(args[i+1]) {
				return "", fmt.Errorf("invalid session uuid %q", args[i+1])
			}
			return args[i+1], nil
		}
	}
	return "", fmt.Errorf("no session flag found")
}
