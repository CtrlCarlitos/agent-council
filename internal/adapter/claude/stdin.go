package claude

// Typed stdin transport (spec §3.7): the prompt is written to the
// process's stdin — never an argv element. The FIRST successfully
// transmitted byte marks transmission begun; from that point a transport
// failure is ambiguous (DispatchUnknown), never a safe retry. This is
// the AC-008 counterpart of AC-007's httptrace WroteRequest boundary.

import (
	"fmt"
	"io"
	"sync/atomic"
)

// ErrPostTransmission marks a stdin transport failure that occurred after
// at least one prompt byte was handed to the process: the turn may have
// been received, so the outcome is ambiguous.
type ErrPostTransmission struct{ Cause error }

func (e *ErrPostTransmission) Error() string {
	return fmt.Sprintf("prompt transmission began but failed: %v", e.Cause)
}
func (e *ErrPostTransmission) Unwrap() error { return e.Cause }

// IsPostTransmissionError reports whether err is a classified
// post-transmission stdin failure.
func IsPostTransmissionError(err error) bool {
	var pt *ErrPostTransmission
	return asPostTransmission(err, &pt)
}

func asPostTransmission(err error, target **ErrPostTransmission) bool {
	for err != nil {
		if pt, ok := err.(*ErrPostTransmission); ok {
			*target = pt
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// StdinWriter writes the prompt to a process's stdin and tracks whether
// transmission began. The flag is atomic: observers may read it while a
// write is in flight.
type StdinWriter struct {
	w     io.Writer
	began atomic.Bool
}

func NewStdinWriter(w io.Writer) *StdinWriter {
	return &StdinWriter{w: w}
}

// TransmissionBegan reports whether at least one prompt byte was handed
// to the transport.
func (w *StdinWriter) TransmissionBegan() bool { return w.began.Load() }

// WritePrompt writes the entire prompt. On failure, callers consult
// TransmissionBegan: true ⇒ ambiguous (post-transmission); false ⇒ the
// server cannot have seen the turn (rejected).
func (w *StdinWriter) WritePrompt(prompt []byte) error {
	for len(prompt) > 0 {
		n, err := w.w.Write(prompt)
		if n > 0 {
			w.began.Store(true)
			prompt = prompt[n:]
			continue
		}
		if err != nil {
			if w.began.Load() {
				return &ErrPostTransmission{Cause: err}
			}
			return err
		}
		// (0, nil) makes no progress: fail closed rather than spin.
		return io.ErrNoProgress
	}
	return nil
}
