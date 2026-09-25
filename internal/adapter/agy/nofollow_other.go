//go:build !unix

package agy

import "os"

// openNoFollow opens path read-only. Off unix there is no O_NOFOLLOW;
// the caller's Lstat regular-file check is the symlink refusal (agy
// production is Linux-only, spec §3.12).
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
