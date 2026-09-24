//go:build !linux

package execpolicy

import "context"

// NewSealedImage is unsupported off Linux: kernel memfd sealing and
// /proc/<pid>/exe verification are Linux-only primitives.
func NewSealedImage(path, wantDigest string) (*SealedImage, error) {
	return nil, ErrSealedLaunchUnsupported
}

// Close is a no-op on platforms where a *SealedImage can never hold a
// live kernel resource (NewSealedImage always fails here).
func (s *SealedImage) Close() error {
	return nil
}

// startSealed is unsupported off Linux. The caller (Start) is
// responsible for invoking cleanup on this error path, exactly as it
// does for every other Start failure.
func startSealed(ctx context.Context, req LaunchRequest, env []string, cleanup func()) (ManagedProcess, error) {
	return nil, ErrSealedLaunchUnsupported
}
