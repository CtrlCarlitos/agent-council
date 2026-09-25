//go:build linux

package execpolicy

import (
	"errors"

	"golang.org/x/sys/unix"
)

// waitExitedNoReap blocks until the child pid has exited WITHOUT reaping
// it (waitid P_PID, WEXITED|WNOWAIT): the zombie keeps the pid — and so
// the process group id it leads — from being reused until the real reap.
func waitExitedNoReap(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
