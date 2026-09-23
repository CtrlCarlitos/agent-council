//go:build unix

package claude

import (
	"fmt"
	"os"
	"syscall"
)

// transcriptTrustSupported reports whether the platform can enforce
// the §3.6 transcript trust checks (ownership, mode).
func transcriptTrustSupported() bool { return true }

// verifyTranscriptOwnership requires the transcript to be owned by the
// current user: a transcript owned by anyone else is not this
// session's evidence.
func verifyTranscriptOwnership(st os.FileInfo) error {
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("bound transcript %s: ownership cannot be verified on this platform; failing closed", st.Name())
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("bound transcript %s is owned by uid %d, not the current uid %d", st.Name(), stat.Uid, os.Geteuid())
	}
	return nil
}
