//go:build unix

package agy

// POSIX conversation-file inspection (AC-010 spec §3.4/§3.12): the
// durable native conversation file must be a regular file (no symlink),
// mode 0600, owned by the current uid, with the recorded device:inode
// identity. Stat only — the file's contents are never read.

import (
	"fmt"
	"os"
	"syscall"
)

// conversationFileIdentity returns the "<dev>:<ino>" identity of a
// regular conversation file.
func conversationFileIdentity(path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("conversation file %s is not a regular file", path)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("conversation file %s has no POSIX identity", path)
	}
	return fmt.Sprintf("%d:%d", uint64(sys.Dev), uint64(sys.Ino)), nil // Dev/Ino widths differ per platform
}

// verifyConversationFile enforces the §3.4 local resume checks.
func verifyConversationFile(path, wantIdentity string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("conversation file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("conversation file %s is not a regular file", path)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		return fmt.Errorf("conversation file %s has mode %#o, want 0600", path, perm)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("conversation file %s has no POSIX identity", path)
	}
	if int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("conversation file %s is owned by uid %d, not the current uid", path, sys.Uid)
	}
	if got := fmt.Sprintf("%d:%d", uint64(sys.Dev), uint64(sys.Ino)); got != wantIdentity {
		return fmt.Errorf("conversation file %s identity %s differs from the recorded %s", path, got, wantIdentity)
	}
	return nil
}
