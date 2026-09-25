//go:build !windows

package execpolicy

import (
	"os"
	"syscall"
)

// terminateGracefully attempts cooperative termination via SIGTERM.
func terminateGracefully(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

// terminateForcefully force-kills a process that ignored graceful
// termination.
func terminateForcefully(proc *os.Process) error {
	return proc.Kill()
}

// killProcessGroup SIGKILLs every member of the process group led by
// pgid (a sealed launch's child is its group leader via Setpgid).
func killProcessGroup(pgid int) error {
	if pgid <= 1 {
		return nil
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

// interruptProcess sends SIGINT, the cooperative interrupt signal used by
// ManagedProcess.Interrupt (distinct from Terminate's SIGTERM/SIGKILL
// shutdown sequence).
func interruptProcess(proc *os.Process) error {
	return proc.Signal(syscall.SIGINT)
}

// terminateProcessGroup SIGTERMs every member of the process group led
// by pgid (the graceful path of a sealed launch's Terminate).
func terminateProcessGroup(pgid int) error {
	if pgid <= 1 {
		return nil
	}
	return syscall.Kill(-pgid, syscall.SIGTERM)
}
