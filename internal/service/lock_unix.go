//go:build !windows

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func AcquireServiceLock(stateDir string) (*ServiceLock, error) {
	if stateDir == "" {
		return nil, errors.New("stateDir cannot be empty")
	}

	info, err := os.Stat(stateDir)
	if err != nil {
		return nil, fmt.Errorf("stat stateDir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("stateDir %s is not a directory", stateDir)
	}

	lockPath := filepath.Join(stateDir, "service.lock")
	// Open file with O_RDWR|O_CREATE|O_CLOEXEC, mode 0600
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("open service.lock: %w", err)
	}

	// Try non-blocking exclusive flock
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = syscall.Close(fd)
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrServiceAlreadyRunning
		}
		return nil, fmt.Errorf("flock service.lock: %w", err)
	}

	// Enforce 0600 mode on the file
	_ = syscall.Fchmod(fd, 0600)

	file := os.NewFile(uintptr(fd), lockPath)
	return &ServiceLock{
		stateDir: stateDir,
		file:     file,
		held:     true,
	}, nil
}
