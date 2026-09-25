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
	// process group (Setpgid, pgid == the leader's pid). Then:
	//   - Terminate's graceful path SIGTERMs the whole group (not only
	//     the leader), and its forced path SIGKILLs the group;
	//   - whenever the leader exits (on its own, on SIGTERM or on
	//     SIGKILL), the wait path peeks at the exit WITHOUT reaping
	//     (waitid WNOWAIT), SIGKILLs the group, and only then reaps —
	//     while the leader is an unreaped zombie its pid (= the pgid)
	//     cannot be reused, so the group kill can only hit members of
	//     this group.
	// A descendant that leaves the group (setsid/setpgid) escapes; that
	// is a documented limit. Path launches leave it false (unchanged
	// behavior).
	killGroup bool
	// reapMu orders every group signal against the reap; reaped is set
	// (under reapMu) once the leader has been reaped, after which the
	// pgid may name an unrelated group and is never signalled again.
	reapMu sync.Mutex
	reaped bool

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
			err := p.waitAndReap()
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

// waitAndReap reaps the leader. For a sealed launch it first waits for
// the leader's exit without reaping it, SIGKILLs the leader's process
// group (pgid > 1 guarded), and only then reaps, so no same-group
// descendant outlives the leader and the pgid cannot have been reused
// by the time of the group kill. If the non-reaping wait is unavailable
// or fails, the group kill is skipped (never sent to a pgid that may
// have been reused) and the leader is reaped as for a path launch.
func (p *managedProcess) waitAndReap() error {
	if !p.killGroup || p.cmd.Process == nil {
		return p.cmd.Wait()
	}
	pid := p.cmd.Process.Pid
	peekErr := waitExitedNoReap(pid)
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if peekErr == nil {
		_ = killProcessGroup(pid)
	}
	err := p.cmd.Wait()
	p.reaped = true
	return err
}

// signalGroup applies send to the sealed launch's process group while
// the leader is still unreaped (its pid still names the group); after
// the reap it does nothing.
func (p *managedProcess) signalGroup(pid int, send func(pgid int) error) {
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if !p.reaped {
		_ = send(pid)
	}
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
	// signals this degrades to an immediate kill. A sealed launch's
	// whole process group gets the SIGTERM too.
	if p.killGroup {
		p.signalGroup(proc.Pid, terminateProcessGroup)
	}
	_ = terminateGracefully(proc)

	select {
	case <-p.waitDoneChan():
		// For a sealed launch the wait path has already SIGKILLed the
		// group between the leader's exit and its reap.
		return nil
	case <-ctx.Done():
		if p.killGroup {
			// Group kill before the leader's SIGKILL, and only while the
			// leader is unreaped (signalGroup holds reapMu, which the
			// wait path holds across its own group kill and reap): the
			// pid still names this group. The wait path SIGKILLs the
			// group again after the leader exits.
			p.signalGroup(proc.Pid, killProcessGroup)
		}
		_ = terminateForcefully(proc)
		<-p.waitDoneChan()
		return ctx.Err()
	}
}
