//go:build !linux

package execpolicy

import "errors"

// waitExitedNoReap is unavailable off Linux (sealed launches, its only
// users, are Linux-only): the caller skips the post-exit group kill.
func waitExitedNoReap(pid int) error {
	return errors.New("non-reaping wait is unsupported on this platform")
}
