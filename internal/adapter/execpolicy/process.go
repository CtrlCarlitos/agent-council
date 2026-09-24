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
	// ExecutableIdentity returns the identity captured for the launched
	// executable: a command path for an ordinary launch, or a
	// device:inode pair and content digest for a sealed-image launch
	// verified at the ptrace exec-stop.
	ExecutableIdentity() ExeIdentity
	// Interrupt sends a cooperative interrupt (SIGINT on POSIX) to the
	// child, distinct from Terminate's graceful-then-forced shutdown
	// sequence. Returns ErrInterruptUnsupported where the platform has
	// no equivalent signal.
	Interrupt() error
}

type managedProcess struct {
	cmd         *exec.Cmd
	stdinPipe   io.WriteCloser
	stdout      io.Reader
	stderr      io.Reader
	cleanup     func()
	exeIdentity ExeIdentity
	// killGroup is set for sealed launches, whose child leads its own
	// process group (Setpgid): the forced path of Terminate then also
	// SIGKILLs that group so descendants the child spawned cannot
	// outlive it. Path launches leave it false (unchanged behavior).
	killGroup bool

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

func (p *managedProcess) ExecutableIdentity() ExeIdentity {
	return p.exeIdentity
}

func (p *managedProcess) Interrupt() error {
	p.mu.Lock()
	proc := p.cmd.Process
	p.mu.Unlock()

	if proc == nil {
		return nil
	}
	return interruptProcess(proc)
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
		if p.killGroup {
			// Group kill FIRST, while the leader is certainly unreaped:
			// its pid still names this process group, so no member can
			// outlive the leader's SIGKILL (killProcessGroup keeps the
			// pgid <= 1 guard).
			_ = killProcessGroup(proc.Pid)
		}
		_ = terminateForcefully(proc)
		<-p.waitDoneChan()
		return ctx.Err()
	}
}
