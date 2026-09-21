//go:build windows

package execpolicy

import "os"

// terminateGracefully degrades to a forced kill on Windows: the platform
// has no process-scoped graceful signal equivalent to SIGTERM.
func terminateGracefully(proc *os.Process) error {
	return terminateForcefully(proc)
}

// terminateForcefully force-kills the process (os.Process.Kill uses
// TerminateProcess on Windows).
func terminateForcefully(proc *os.Process) error {
	return proc.Kill()
}
