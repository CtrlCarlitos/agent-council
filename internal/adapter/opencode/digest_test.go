package opencode

import (
	"strings"
	"testing"
)

// Digest determinism: distinct tuples produce distinct digests; the same
// tuple is deterministic; length-prefixed encoding prevents boundary
// ambiguity; NUL bytes are rejected.
func TestGate2Review_DigestDeterministic(t *testing.T) {
	a1, _ := NativeMessageID("sess-1", "turn-1", "att-1")
	a2, _ := NativeMessageID("sess-1", "turn-1", "att-1")
	if a1 != a2 {
		t.Fatal("same tuple must produce the same digest")
	}
	b, _ := NativeMessageID("sess-1", "turn-1", "att-2")
	if a1 == b {
		t.Fatal("different attempts must produce different digests")
	}
	c, _ := NativeMessageID("sess-1", "turn-11", "att-1")
	if a1 == c {
		t.Fatal("length-prefixed encoding must prevent boundary ambiguity between (turn-1,att-1) and (turn-11,att-…)")
	}
}

func TestGate2Review_DigestNulRejected(t *testing.T) {
	_, err := NativeMessageID("sess\x00", "turn", "att")
	if err == nil {
		t.Fatal("NUL in sessionID must be rejected")
	}
	_, err = NativeMessageID("sess", "tu\x00rn", "att")
	if err == nil {
		t.Fatal("NUL in turnKey must be rejected")
	}
}

func TestGate2Review_DigestPrefixAndLength(t *testing.T) {
	id, err := NativeMessageID("s", "t", "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != len("msg_council_")+32 {
		t.Fatalf("expected msg_council_ prefix + 32 hex chars, got %d", len(id))
	}
	if !strings.HasPrefix(id, "msg_council_") {
		t.Fatalf("expected msg_council_ prefix, got %q", id)
	}
}
