package claude

import (
	"crypto/sha256"
	"fmt"
)

// PromptDigest derives the deterministic prompt hash used for durable
// acceptance correlation (spec §3.5): sha256 over the prompt text
// concatenated with the attempt identity.
func PromptDigest(prompt, attemptID string) string {
	sum := sha256.Sum256([]byte(attemptID + "\x00" + prompt))
	return fmt.Sprintf("sha256:%x", sum)
}
