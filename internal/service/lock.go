package service

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

var (
	ErrServiceAlreadyRunning = errors.New("service already running in this state directory")
	ErrUnsupportedPlatform   = errors.New("service lifetime management is not supported on this platform")
)

type ServiceLock struct {
	mu       sync.Mutex
	stateDir string
	file     *os.File
	held     bool
}

func (l *ServiceLock) StateDir() string {
	if l == nil {
		return ""
	}
	return l.stateDir
}

func (l *ServiceLock) Path() string {
	if l == nil {
		return ""
	}
	return filepath.Join(l.stateDir, "service.lock")
}

func (l *ServiceLock) IsHeld() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

func (l *ServiceLock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held {
		return nil
	}
	l.held = false
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

// CleanupDiscovery unlinks runtime discovery and credential files belonging to the service owner.
// Requires that the lock receiver is held by the caller.
func (l *ServiceLock) CleanupDiscovery() error {
	if l == nil || !l.IsHeld() {
		return errors.New("cannot cleanup discovery without holding service lock")
	}
	return CleanupDiscoveryFiles(l.stateDir)
}

func CleanupDiscoveryFiles(stateDir string) error {
	var firstErr error
	for _, name := range []string{"service.json", "auth.token", "council.sock"} {
		p := filepath.Join(stateDir, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
