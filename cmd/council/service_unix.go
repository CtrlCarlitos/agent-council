//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/client"
)

func startDetachedService(stateDir string) error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devNull.Close()

	logPath := filepath.Join(stateDir, "service.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open service.log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(bin, "service", "run", "--state-dir", stateDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached service: %w", err)
	}

	// Poll readiness until ready or timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return errors.New("timed out waiting for service readiness")
		case <-ticker.C:
			// Check if process has died
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				return fmt.Errorf("detached service process exited prematurely: %w", err)
			}

			c, err := client.New(stateDir)
			if err != nil {
				continue
			}
			ready, err := c.GetReadiness(ctx)
			if err == nil && ready.Status == "ready" {
				_ = cmd.Process.Release()
				return nil
			}
		}
	}
}
