//go:build windows

package service

func AcquireServiceLock(stateDir string) (*ServiceLock, error) {
	return nil, ErrUnsupportedPlatform
}
