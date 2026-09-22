//go:build windows

package claude

import (
	"fmt"
	"os"
)

// transcriptTrustSupported reports whether the platform can enforce
// the §3.6 transcript trust checks. Windows has no ACL-equivalent
// capability yet, so transcript trust fails closed.
func transcriptTrustSupported() bool { return false }

func verifyTranscriptOwnership(os.FileInfo) error {
	return fmt.Errorf("transcript ownership verification is unavailable on this platform; failing closed (§3.6)")
}
