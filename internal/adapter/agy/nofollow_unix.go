//go:build unix

package agy

import (
	"os"
	"syscall"
)

// openNoFollow opens path read-only, refusing a symlink at the leaf
// (O_NOFOLLOW: ELOOP instead of following the link).
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
