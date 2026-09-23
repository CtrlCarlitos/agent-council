//go:build windows

package codex

// Windows platform statement for the codex rollout baseline (AC-009
// spec §3.7): POSIX ownership/mode checks and dev:ino file identity are
// UNVERIFIED on this platform — the honest gap fails closed instead of
// producing an unreadable baseline. The codex adapter is POSIX-scoped
// until the platform evidence exists.

import "fmt"

// locateRollout fails closed on the unverified platform.
func locateRollout(codexHome, nativeID string) (string, error) {
	return "", fmt.Errorf("codex rollout scanning is not verified on windows (spec §3.7 fail-closed platform statement)")
}

// checkRolloutPath fails closed on the unverified platform.
func checkRolloutPath(path, codexHome string) error {
	return fmt.Errorf("codex rollout verification is not verified on windows (spec §3.7 fail-closed platform statement)")
}

// scanRolloutBaseline fails closed on the unverified platform.
func scanRolloutBaseline(path, nativeID string) (identity string, size int64, entries int, err error) {
	return "", 0, 0, fmt.Errorf("codex rollout baseline is not verified on windows (spec §3.7 fail-closed platform statement)")
}
