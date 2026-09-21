//go:build !linux

package execpolicy

import (
	"context"
	"fmt"
	"os/exec"
)

func checkPlatformCapabilities(ctx context.Context, req LaunchRequest) error {
	if req.Profile.IsolationStrictness == "strict" {
		return fmt.Errorf("%w: strict isolation requires Linux host capabilities", ErrUnsupportedIsolationCapability)
	}
	return nil
}

func configureSysProcAttr(req LaunchRequest, cmd *exec.Cmd) error {
	if req.Profile.IsolationStrictness == "strict" {
		return fmt.Errorf("%w: strict isolation requires Linux host capabilities", ErrUnsupportedIsolationCapability)
	}
	return nil
}
