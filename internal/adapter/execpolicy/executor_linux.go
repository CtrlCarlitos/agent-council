//go:build linux

package execpolicy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func checkPlatformCapabilities(ctx context.Context, req LaunchRequest) error {
	if req.Profile.IsolationStrictness != "strict" {
		return nil
	}

	if req.Profile.WorkspaceMode == "readonly" {
		// Kernel read-only bind mount cannot be established without unprivileged mount namespace
		// capabilities and helper tooling. Reject strict execution when mount isolation cannot be configured.
		return fmt.Errorf("%w: strict readonly workspace requires OS-level mount isolation", ErrUnsupportedIsolationCapability)
	}

	if req.Profile.NetworkMode == "allowlist" {
		// Strict allowlist requires OS-level socket filtering (e.g. eBPF/cgroups) to prevent direct socket bypass.
		// Standard unprivileged process execution cannot guarantee direct socket prevention.
		return fmt.Errorf("%w: strict network allowlist requires OS-level socket redirection", ErrUnsupportedIsolationCapability)
	}

	if req.Profile.NetworkMode == "none" {
		if _, err := os.Stat("/proc/self/ns/user"); err != nil {
			return fmt.Errorf("%w: user namespace unavailable for network isolation", ErrUnsupportedIsolationCapability)
		}
		if _, err := os.Stat("/proc/self/ns/net"); err != nil {
			return fmt.Errorf("%w: network namespace unavailable", ErrUnsupportedIsolationCapability)
		}
	}

	return nil
}

func configureSysProcAttr(req LaunchRequest, cmd *exec.Cmd) error {
	if req.Profile.IsolationStrictness != "strict" {
		return nil
	}

	if req.Profile.NetworkMode == "none" {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
			UidMappings: []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: os.Getuid(), Size: 1},
			},
			GidMappings: []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: os.Getgid(), Size: 1},
			},
			GidMappingsEnableSetgroups: false,
		}
	}

	return nil
}
