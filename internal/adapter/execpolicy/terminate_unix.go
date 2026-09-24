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
