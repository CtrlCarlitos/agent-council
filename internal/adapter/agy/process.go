package agy

// Process-per-turn I/O plumbing (AC-010 spec §3.1/§3.9): one strict
// NDJSON reader over the child's stdout (oversized/malformed/unknown ⇒
// the stream is poisoned), stderr drained from start into a bounded tail
// and scanned for the two exact markers, and bounded exit/terminate
// helpers. The adapter never sees a pid: every signal goes through the
// executor's ManagedProcess.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
)

// streamItem is one decoded stdout event with its verbatim line.
type streamItem struct {
	ev  Event
	raw []byte
}

var errReaderDetached = errors.New("agy stdout reader detached")

// maxStderrLineBytes bounds one scanned stderr line; a longer line
// makes the stderr scan inconclusive (a marker could be hidden in it).
const maxStderrLineBytes = 1 << 20

// agyProcess is one launched agy child with its readers armed.
type agyProcess struct {
	proc execpolicy.ManagedProcess

	items    chan streamItem
	stop     chan struct{}
	stopOnce sync.Once
	// readErr is the reader's terminal error; valid once items is closed.
	readErr error

	stderrTail     *boundedBuffer
	stderrDone     chan struct{}
	printTimeout   atomic.Bool
	ignoredInput   atomic.Bool
	stderrOverflow atomic.Bool

	waitOnce sync.Once
	waitDone chan struct{}
	exitCode int
}

// watchProcess arms the stdout reader and the stderr drain. The reader
// is armed BEFORE anything is written to stdin (the init event is read
// first, spec §3.1).
func watchProcess(proc execpolicy.ManagedProcess) *agyProcess {
	p := &agyProcess{
		proc:       proc,
		items:      make(chan streamItem),
		stop:       make(chan struct{}),
		stderrTail: newStderrTail(),
		stderrDone: make(chan struct{}),
		waitDone:   make(chan struct{}),
	}
	go p.drainStderr()
	go p.readStdout()
	return p
}

func (p *agyProcess) readStdout() {
	stdout := p.proc.Stdout()
	err := readEventLines(stdout, func(ev Event, raw []byte) error {
		it := streamItem{ev: ev, raw: append([]byte(nil), raw...)}
		select {
		case p.items <- it:
			return nil
		case <-p.stop:
			return errReaderDetached
		}
	}, p.stderrTail)
	p.readErr = err
	close(p.items)
	if err != nil {
		// Detached or poisoned: keep the pipe drained so the child can
		// never block on a full stdout while it is being terminated.
		_, _ = io.Copy(io.Discard, stdout)
	}
}

func (p *agyProcess) drainStderr() {
	defer close(p.stderrDone)
	stderr := p.proc.Stderr()
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 4096), maxStderrLineBytes)
	for sc.Scan() {
		line := sc.Text()
		_, _ = p.stderrTail.Write([]byte(line + "\n"))
		switch StderrMarker(line) {
		case MarkerPrintTimeout:
			p.printTimeout.Store(true)
		case MarkerIgnoredInput:
			p.ignoredInput.Store(true)
		}
	}
	if sc.Err() != nil {
		p.stderrOverflow.Store(true)
		_, _ = io.Copy(p.stderrTail, stderr)
	}
}

// detach stops delivering stdout events (the consumer abandoned them).
func (p *agyProcess) detach() {
	p.stopOnce.Do(func() { close(p.stop) })
}

// awaitInit waits (bounded) for the first stdout event.
func (p *agyProcess) awaitInit(timeout time.Duration) (streamItem, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case it, ok := <-p.items:
		if !ok {
			if p.readErr != nil {
				return streamItem{}, p.readErr
			}
			return streamItem{}, errors.New("agy child exited before emitting init")
		}
		return it, nil
	case <-timer.C:
		return streamItem{}, errors.New("agy init not observed within the bound")
	}
}

// kill terminates the child (SIGTERM → kill, bounded) and detaches the
// reader.
func (p *agyProcess) kill() {
	p.detach()
	ctx, cancel := context.WithTimeout(context.Background(), terminateTimeout)
	defer cancel()
	_ = p.proc.Terminate(ctx)
}

// wait reaps the child exactly once and reports its exit code; call it
// only after stdout reached EOF (or the child was killed).
func (p *agyProcess) wait() int {
	p.waitOnce.Do(func() {
		go func() {
			code, _ := p.proc.Wait()
			p.exitCode = code
			close(p.waitDone)
		}()
	})
	<-p.waitDone
	return p.exitCode
}

// settle waits (bounded) for stderr to drain and the child to exit after
// stdout ended, killing it if it lingers; it returns the exit code.
func (p *agyProcess) settle(bound time.Duration) int {
	deadline := time.Now().Add(bound)
	select {
	case <-p.stderrDone:
	case <-time.After(bound):
		p.kill()
		select {
		case <-p.stderrDone:
		case <-time.After(terminateTimeout):
			// A descendant may still hold stderr open: the marker scan
			// is inconclusive, which classifies like an overflow.
			p.stderrOverflow.Store(true)
		}
	}
	exited := make(chan int, 1)
	go func() { exited <- p.wait() }()
	select {
	case code := <-exited:
		return code
	case <-time.After(time.Until(deadline)):
		p.kill()
		return <-exited
	}
}

// ── Gate captures (`models`, `plugin list`) ─────────────────────────────

// captureLimit bounds each captured stream of a gate launch; output
// beyond it is drained and the capture reported as overflowed.
const captureLimit = 1 << 20

// captureResult is one gate launch's bounded stdout/stderr and exit code.
type captureResult struct {
	stdout, stderr []byte
	overflow       bool
	exitCode       int
}

// errCaptureTimeout reports a gate launch that did not finish within
// its bound; the child was terminated.
var errCaptureTimeout = errors.New("agy gate launch did not finish within the bound")

// limitedBuffer keeps the first captureLimit bytes and drains the rest.
type limitedBuffer struct {
	buf      []byte
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := captureLimit - len(b.buf); room > 0 {
		if len(p) > room {
			b.buf = append(b.buf, p[:room]...)
			b.overflow = true
		} else {
			b.buf = append(b.buf, p...)
		}
	} else if len(p) > 0 {
		b.overflow = true
	}
	return len(p), nil
}

// runCapture starts one provider-free gate launch through the executor
// (sealed and verified like every launch), closes its stdin at once
// (no input ever exists for these subcommands), reads both streams to
// EOF with a bounded buffer, and reaps the child — all within bound. On
// timeout the child is terminated and errCaptureTimeout returned. The
// caller's context never bounds the child (adapter-owned bounds only).
func runCapture(executor execpolicy.PolicyExecutor, req execpolicy.LaunchRequest, bound time.Duration) (captureResult, error) {
	proc, err := executor.Start(context.Background(), req)
	if err != nil {
		return captureResult{}, fmt.Errorf("gate launch start: %w", err)
	}
	_ = proc.Stdin().Close()

	var out, errOut limitedBuffer
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(&out, proc.Stdout()); done <- struct{}{} }()
	go func() { _, _ = io.Copy(&errOut, proc.Stderr()); done <- struct{}{} }()

	timer := time.NewTimer(bound)
	defer timer.Stop()
	terminate := func() (captureResult, error) {
		ctx, cancel := context.WithTimeout(context.Background(), terminateTimeout)
		defer cancel()
		_ = proc.Terminate(ctx)
		return captureResult{}, errCaptureTimeout
	}
	for pending := 2; pending > 0; {
		select {
		case <-done:
			pending--
		case <-timer.C:
			return terminate()
		}
	}
	exited := make(chan int, 1)
	go func() { code, _ := proc.Wait(); exited <- code }()
	select {
	case code := <-exited:
		return captureResult{stdout: out.buf, stderr: errOut.buf, overflow: out.overflow || errOut.overflow, exitCode: code}, nil
	case <-timer.C:
		return terminate()
	}
}
