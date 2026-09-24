package codex

// pdig-v1 prompt-digest framing (spec §3.5): the durable turn-
// correlation digest binding the native thread id, turn key, attempt
// id, and exact prompt bytes in a fixed-field-order length-prefixed
// frame. NUL bytes in any field are rejected before encoding.

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"
)

// PromptDigest computes pdig-v1:sha256:<hex> over the canonical
// turn-identity frame. Fields are positional: native thread id, turn
// key, attempt id, prompt. The digest is stored on the attempt at
// launch and matched against rollout entries (spec §3.7).
func PromptDigest(nativeThreadID, turnKey, attemptID, prompt string) (string, error) {
	fields := []struct {
		name  string
		value string
	}{
		{"native thread id", nativeThreadID},
		{"turn key", turnKey},
		{"attempt id", attemptID},
		{"prompt", prompt},
	}
	for _, f := range fields {
		if strings.IndexByte(f.value, 0) >= 0 {
			return "", fmt.Errorf("pdig %s contains a NUL byte", f.name)
		}
		if !utf8.ValidString(f.value) {
			return "", fmt.Errorf("pdig %s is not valid UTF-8", f.name)
		}
	}

	buf := make([]byte, 0, 128)
	field := func(s string) {
		buf = append(buf, u32be(uint32(len(s)))...)
		buf = append(buf, s...)
	}
	// Fixed field order (spec §3.5): framing tag, native thread id,
	// turn key, attempt id, prompt.
	field("pdig-v1")
	field(nativeThreadID)
	field(turnKey)
	field(attemptID)
	field(prompt)

	return "pdig-v1:sha256:" + fmt.Sprintf("%x", sha256.Sum256(buf)), nil
}
