package execpolicy

import "errors"

var (
	// ErrSealedLaunchUnsupported is returned when a sealed-image launch
	// (kernel-sealed memfd copy of the executable, verified at the
	// ptrace exec-stop) is requested on a platform that cannot provide
	// it.
	ErrSealedLaunchUnsupported = errors.New("sealed-image launch is not supported on this platform")

	// ErrSealedImageMismatch is returned when the sealed image's seals,
	// content digest, or the exec'd child's /proc/<pid>/exe identity do
	// not match what was pinned, at any verification point (image
	// construction, pre-launch re-verification, or the post exec-stop
	// check).
	ErrSealedImageMismatch = errors.New("sealed image identity mismatch")

	// ErrSealedLaunch is returned for sealed-image launch failures that
	// are not an identity mismatch (memfd/seal/ptrace machinery
	// failures).
	ErrSealedLaunch = errors.New("sealed-image launch failed")

	// ErrInterruptUnsupported is returned when ManagedProcess.Interrupt
	// is called on a platform without a graceful interrupt signal.
	ErrInterruptUnsupported = errors.New("interrupt is not supported on this platform")
)

// SealedImage is a kernel-sealed (memfd, F_SEAL_SHRINK|F_SEAL_GROW|
// F_SEAL_WRITE|F_SEAL_SEAL) copy of a pinned executable. It is built once
// by NewSealedImage and may be used to launch the same pinned binary
// multiple times: every launch re-verifies the seals and content digest
// before spawning, and re-verifies the child's /proc/<pid>/exe identity
// at the ptrace exec-stop before the child runs a single instruction.
type SealedImage struct {
	// ArgV0 is the original executable path passed to NewSealedImage
	// (cleaned, absolute). It is what the launched child sees as its
	// own argv[0], so the child's view of its own identity looks
	// normal even though the kernel actually execs the sealed memfd
	// copy via /proc/self/fd/<n>.
	ArgV0 string

	// Digest is the sha256 content digest of the pinned executable, in
	// the repo-standard "sha256:<64 lowercase hex>" form.
	Digest string

	// fd is the sealed memfd holding the pinned executable's bytes.
	// Platform-specific launch code dups it per-launch; it is closed by
	// Close.
	fd int
}

// ExeIdentity is the executable identity captured for a launched
// process. For an ordinary (non-sealed) path launch it carries the
// launch-time command path with an empty Digest. For a sealed-image
// launch it carries the device:inode pair and content digest of the
// process image verified at the exec-stop, with an empty Path.
type ExeIdentity struct {
	// Path is the command path used for an ordinary path launch.
	Path string
	// DevIno is the "<dev>:<ino>" pair of the executable image
	// observed at /proc/<pid>/exe for a sealed-image launch.
	DevIno string
	// Digest is the sha256 content digest ("sha256:<64 lowercase
	// hex>") of the executable image, when known.
	Digest string
}
