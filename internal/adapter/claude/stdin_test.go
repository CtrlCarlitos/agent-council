package claude

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

// errAfterWriter fails after n successful bytes.
type errAfterWriter struct {
	n         int
	failAfter int
}

func (w *errAfterWriter) Write(p []byte) (int, error) {
	if w.n >= w.failAfter {
		return 0, errors.New("connection reset mid-write")
	}
	take := len(p)
	if remain := w.failAfter - w.n; take > remain {
		take = remain
	}
	w.n += take
	return take, nil
}

func (w *errAfterWriter) Close() error { return nil }

// A failure after any byte was transmitted marks the boundary: the
// dispatch must classify as ambiguous, never as a safe retry.
func TestStdinWriter_TransmissionBoundary(t *testing.T) {
	prompt := []byte("the entire prompt payload")

	t.Run("first byte succeeds then failure => began", func(t *testing.T) {
		w := NewStdinWriter(&errAfterWriter{failAfter: 1})
		err := w.WritePrompt(prompt)
		if err == nil {
			t.Fatal("the failing writer must surface an error")
		}
		if !w.TransmissionBegan() {
			t.Fatal("one transmitted byte must mark transmission begun")
		}
		if !IsPostTransmissionError(err) {
			t.Fatalf("post-transmission failure must be classified, got %v", err)
		}
	})

	t.Run("immediate failure => not began", func(t *testing.T) {
		w := NewStdinWriter(&errAfterWriter{failAfter: 0})
		err := w.WritePrompt(prompt)
		if err == nil {
			t.Fatal("the failing writer must surface an error")
		}
		if w.TransmissionBegan() {
			t.Fatal("no transmitted byte must leave transmission unbegun")
		}
		if IsPostTransmissionError(err) {
			t.Fatalf("pre-transmission failure must not be classified post-write, got %v", err)
		}
	})

	t.Run("clean write => began", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewStdinWriter(&nopCloser{&buf})
		if err := w.WritePrompt([]byte("hello")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if !w.TransmissionBegan() {
			t.Fatal("a clean write marks transmission begun")
		}
		if buf.String() != "hello" {
			t.Fatalf("prompt bytes must be written verbatim, got %q", buf.String())
		}
	})
}

// Multi-chunk prompts (larger than one write) still classify correctly:
// a mid-chunk failure is post-transmission.
func TestStdinWriter_LargePromptPartialTransmission(t *testing.T) {
	prompt := []byte(strings.Repeat("x", 4096))
	w := NewStdinWriter(&errAfterWriter{failAfter: 1000})
	err := w.WritePrompt(prompt)
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	if !w.TransmissionBegan() {
		t.Fatal("1000 transmitted bytes must mark transmission begun")
	}
	if !IsPostTransmissionError(err) {
		t.Fatalf("partial transmission failure must be post-write, got %v", err)
	}
}

// A concurrent-safe begun flag: checked atomically from other
// goroutines (observer paths).
func TestStdinWriter_FlagIsAtomic(t *testing.T) {
	var saw int32
	w := NewStdinWriter(&errAfterWriter{failAfter: 5})
	_ = w.WritePrompt([]byte(strings.Repeat("y", 64)))
	if w.TransmissionBegan() {
		atomic.StoreInt32(&saw, 1)
	}
	if atomic.LoadInt32(&saw) != 1 {
		t.Fatal("flag must be readable atomically after writes")
	}
}

type nopCloser struct{ *bytes.Buffer }

func (n *nopCloser) Close() error { return nil }

// zeroWriter returns (0, nil) from every Write: the writer must fail
// closed with io.ErrNoProgress instead of looping forever.
type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
func (zeroWriter) Close() error              { return nil }

func TestStdinWriter_ZeroProgressFailsClosed(t *testing.T) {
	w := NewStdinWriter(zeroWriter{})
	err := w.WritePrompt([]byte("prompt"))
	if err == nil {
		t.Fatal("(0, nil) writes must fail closed")
	}
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("expected io.ErrNoProgress, got %v", err)
	}
	if w.TransmissionBegan() {
		t.Fatal("no progress means no transmission")
	}
}
