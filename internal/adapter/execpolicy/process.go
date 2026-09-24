package execpolicy

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
)

// ManagedProcess defines the interface for interacting with a council-supervised process.
type ManagedProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() (int, error)
	Terminate(ctx context.Context) error
}

type managedProcess struct {
	cmd       *exec.Cmd
	stdinPipe io.WriteCloser
	stdout    io.Reader
	stderr    io.Reader
	cleanup   func()

	mu       sync.Mutex
	waitDone chan struct{}
	exitCode int
	waitErr  error
}

func (p *managedProcess) Stdin() io.WriteCloser {
	return p.stdinPipe
}

func (p *managedProcess) Stdout() io.Reader {
	return p.stdout
}

func (p *managedProcess) Stderr() io.Reader {
	return p.stderr
}

func (p *managedProcess) waitDoneChan() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waitDone == nil {
		p.waitDone = make(chan struct{})
		go func() {
			err := p.cmd.Wait()
			p.mu.Lock()
			if p.cmd.ProcessState != nil {
				p.exitCode = p.cmd.ProcessState.ExitCode()
				p.waitErr = nil
			} else if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					p.exitCode = exitErr.ExitCode()
					p.waitErr = nil
				} else {
					p.exitCode = -1
					p.waitErr = err
				}
			} else {
				p.exitCode = 0
				p.waitErr = nil
			}
			if p.cleanup != nil {
				p.cleanup()
				p.cleanup = nil
			}
			p.mu.Unlock()
			close(p.waitDone)
		}()
	}
	return p.waitDone
}

func (p *managedProcess) Wait() (int, error) {
	ch := p.waitDoneChan()
	<-ch
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode, p.waitErr
}

func (p *managedProcess) Terminate(ctx context.Context) error {
	p.mu.Lock()
	proc := p.cmd.Process
	p.mu.Unlock()

	if proc == nil {
		return nil
	}

	// Attempt graceful termination first; on platforms without graceful
	// signals this degrades to an immediate kill.
	_ = terminateGracefully(proc)

	select {
	case <-p.waitDoneChan():
		return nil
	case <-ctx.Done():
		_ = terminateForcefully(proc)
		<-p.waitDoneChan()
		return ctx.Err()
	}
}
