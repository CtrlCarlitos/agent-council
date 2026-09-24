//go:build windows

package service

import (
	"errors"
	"testing"
)

// The service lifetime backend (flock + unix domain sockets) is deliberately
// unsupported on native Windows until named-pipe IPC and LockFileEx
// exclusivity are implemented and verified. Storage and adapter contracts
// remain cross-platform and are tested elsewhere.
func TestWindows_ServiceLifetimeUnsupported(t *testing.T) {
	_, err := AcquireServiceLock(t.TempDir())
	if err == nil {
		t.Fatal("expected unsupported-platform error from AcquireServiceLock on Windows")
	}
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("expected ErrUnsupportedPlatform, got %v", err)
	}
}
