//go:build windows

package main

import (
	"errors"
)

var ErrUnsupportedPlatform = errors.New("detached service startup is not supported on Windows")

func startDetachedService(stateDir string) error {
	return ErrUnsupportedPlatform
}
