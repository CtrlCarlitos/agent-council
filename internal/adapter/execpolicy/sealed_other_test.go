//go:build !linux

package execpolicy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewSealedImage_UnsupportedOffLinux(t *testing.T) {
	digest := "sha256:" + strings.Repeat("0", 64)
	img, err := NewSealedImage("/nonexistent/agy", digest)
	if img != nil {
		t.Fatalf("expected nil image, got %+v", img)
	}
	if !errors.Is(err, ErrSealedLaunchUnsupported) {
		t.Fatalf("expected ErrSealedLaunchUnsupported, got %v", err)
	}
}

func TestStartSealed_UnsupportedOffLinux(t *testing.T) {
	proc, err := startSealed(context.Background(), LaunchRequest{}, nil, func() {})
	if proc != nil {
		t.Fatalf("expected nil process, got %+v", proc)
	}
	if !errors.Is(err, ErrSealedLaunchUnsupported) {
		t.Fatalf("expected ErrSealedLaunchUnsupported, got %v", err)
	}
}

func TestSealedImage_CloseIsNoOpOffLinux(t *testing.T) {
	img := &SealedImage{ArgV0: "/nonexistent/agy", Digest: "sha256:" + strings.Repeat("0", 64)}
	if err := img.Close(); err != nil {
		t.Fatalf("expected Close to be a no-op, got %v", err)
	}
}
