package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrNulByteInIdentifier reports a NUL byte in a TurnRef identifier that
// would break the length-prefixed canonical encoding.
var ErrNulByteInIdentifier = errors.New("identifier contains NUL byte")

// NativeMessageID builds the deterministic native message ID for a turn
// attempt: msg_council_ + hex(SHA-256(canonical encoding of sessionID,
// turnKey, attempt))[:32]. The canonical encoding length-prefixes each
// field (decimal byte length + '\n' + field bytes) so distinct tuples can
// never produce identical input bytes. NUL bytes in any identifier are
// rejected before encoding.
func NativeMessageID(sessionID, turnKey, attempt string) (string, error) {
	for name, field := range map[string]string{
		"sessionID": sessionID,
		"turnKey":   turnKey,
		"attempt":   attempt,
	} {
		if strings.Contains(field, "\x00") {
			return "", fmt.Errorf("%w: %s contains NUL byte", ErrNulByteInIdentifier, name)
		}
	}
	h := sha256.New()
	encodeField := func(field string) {
		fmt.Fprintf(h, "%d\n", len(field))
		h.Write([]byte(field))
	}
	encodeField(sessionID)
	encodeField(turnKey)
	encodeField(attempt)
	return "msg_council_" + hex.EncodeToString(h.Sum(nil))[:32], nil
}

// UserMessageID builds the deterministic user-message ID (the prompt
// submission) for a turn attempt, using the same canonical encoding.
func UserMessageID(sessionID, turnKey, attempt string) (string, error) {
	id, err := NativeMessageID(sessionID, turnKey, attempt)
	if err != nil {
		return "", err
	}
	return strings.Replace(id, "msg_council_", "msg_user_council_", 1), nil
}
