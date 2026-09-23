package codex

// pdig-v1 prompt-digest framing tests: fixed golden vector, NUL-byte
// rejection, field-order sensitivity, and determinism (spec §3.5).

import (
	"strings"
	"testing"
)

const pdigGoldenThreadID = "0198a3b4-c5d6-7e89-f0a1-b2c3d4e5f6a7"
const pdigGoldenPrompt = "Propose: résumé ünïcode ✓ 明けましておめでとう"

// Independently computed fixed vector (python3 framing, spec §3.5 field
// order: pdig-v1 framing tag, native thread id, turn key, attempt id,
// prompt). Covers a unicode prompt and an empty attempt id; empty
// fields are legal, NUL bytes are not.
func TestPromptDigest_FixedGoldenVector(t *testing.T) {
	want := "pdig-v1:sha256:b436466f7667ce8b486abf3e0439e1c3b8d99141976c2a0a6c1f71c1ac738c86"
	got, err := PromptDigest(pdigGoldenThreadID, "turn-2", "", pdigGoldenPrompt)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if got != want {
		t.Fatalf("fixed golden vector mismatch:\n got %q\nwant %q", got, want)
	}
	if len(got) != len("pdig-v1:sha256:")+64 {
		t.Fatalf("digest must be prefix + 64 lowercase hex, got %q", got)
	}
}

func TestPromptDigest_Deterministic(t *testing.T) {
	d1, err := PromptDigest(pdigGoldenThreadID, "turn-2", "a1", pdigGoldenPrompt)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	d2, err := PromptDigest(pdigGoldenThreadID, "turn-2", "a1", pdigGoldenPrompt)
	if err != nil {
		t.Fatalf("second digest: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("identical framing must produce identical digests: %q vs %q", d1, d2)
	}
}

func TestPromptDigest_FieldOrderSensitivity(t *testing.T) {
	base, err := PromptDigest(pdigGoldenThreadID, "turn-2", "a1", pdigGoldenPrompt)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	// Swapping turn key and attempt id changes the digest: fields are
	// positional, not a set.
	swapped, err := PromptDigest(pdigGoldenThreadID, "a1", "turn-2", pdigGoldenPrompt)
	if err != nil {
		t.Fatalf("swapped digest: %v", err)
	}
	if swapped == base {
		t.Fatal("field-order swap must change the prompt digest")
	}
	changedPrompt, err := PromptDigest(pdigGoldenThreadID, "turn-2", "a1", pdigGoldenPrompt+"!")
	if err != nil {
		t.Fatalf("changed prompt digest: %v", err)
	}
	if changedPrompt == base {
		t.Fatal("prompt change must change the prompt digest")
	}
}

func TestPromptDigest_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		thread  string
		turn    string
		attempt string
		prompt  string
		wantErr string
	}{
		{"NUL in thread id", "0198a3b4\x00", "turn-2", "a1", "p", "NUL"},
		{"NUL in turn key", pdigGoldenThreadID, "turn\x00-2", "a1", "p", "NUL"},
		{"NUL in attempt id", pdigGoldenThreadID, "turn-2", "a\x001", "p", "NUL"},
		{"NUL in prompt", pdigGoldenThreadID, "turn-2", "a1", "pro\x00mpt", "NUL"},
		{"invalid UTF-8 in prompt", pdigGoldenThreadID, "turn-2", "a1", "bad \xff utf8", "UTF-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PromptDigest(tc.thread, tc.turn, tc.attempt, tc.prompt)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q in error, got %v", tc.wantErr, err)
			}
		})
	}
}
