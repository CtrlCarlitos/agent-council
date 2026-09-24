//go:build linux

package execpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// requiredSeals is the exact seal set a sealed image must carry before it
// may be launched: no shrinking, no growing, no writing, and the seal set
// itself may never be extended again. Equality (not "at least") is
// checked everywhere this is used, so a host that additionally forces
// MFD_NOEXEC_SEAL (adding F_SEAL_EXEC/F_SEAL_FUTURE_WRITE to the memfd
// unasked) fails closed instead of silently accepting a differently
// sealed image.
const requiredSeals = unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE | unix.F_SEAL_SEAL

// createSealedMemfd creates the memfd that will hold the pinned
// executable's bytes. It asks for MFD_EXEC first: without it, a kernel
// built with CONFIG_MEMFD_CREATE's default-noexec behavior (or one
// where the sysctl vm.memfd_noexec forces it) marks the memfd
// non-executable regardless of MFD_ALLOW_SEALING, which would make the
// later exec of /proc/self/fd/<dup> fail with ENOEXEC/EACCES even
// though every seal check passed. MFD_EXEC was added in Linux 6.3
// (golang.org/x/sys v0.47.0's unix.MFD_EXEC); older kernels reject the
// flag combination with EINVAL, so on that specific error we retry
// once without MFD_EXEC — a kernel that predates MFD_EXEC also
// predates the noexec default, so the memfd is executable there
// regardless.
func createSealedMemfd() (int, error) {
	fd, err := unix.MemfdCreate("agy", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_EXEC)
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, unix.EINVAL) {
		return -1, err
	}
	return unix.MemfdCreate("agy", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
}

// NewSealedImage reads the executable at path, verifies its content
// digest matches wantDigest, copies it into a sealed memfd (kernel-backed,
// unwritable, unshrinkable, ungrowable, and permanently sealed against
// further seal changes), and re-verifies the copy by re-hashing the bytes
// back out of the memfd.
func NewSealedImage(path, wantDigest string) (*SealedImage, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve path %q: %v", ErrSealedLaunch, path, err)
	}
	absPath = filepath.Clean(absPath)

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read %q: %v", ErrSealedLaunch, absPath, err)
	}
	digest := sha256Hex(data)
	if digest != wantDigest {
		return nil, fmt.Errorf("%w: %q digest %s does not match expected %s", ErrSealedImageMismatch, absPath, digest, wantDigest)
	}

	fd, err := createSealedMemfd()
	if err != nil {
		return nil, fmt.Errorf("%w: memfd_create: %v", ErrSealedLaunch, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = unix.Close(fd)
		}
	}()

	if err := writeAllFd(fd, data); err != nil {
		return nil, fmt.Errorf("%w: write sealed image: %v", ErrSealedLaunch, err)
	}

	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, requiredSeals); err != nil {
		return nil, fmt.Errorf("%w: add seals: %v", ErrSealedLaunch, err)
	}

	seals, err := unix.FcntlInt(uintptr(fd), unix.F_GET_SEALS, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: get seals: %v", ErrSealedLaunch, err)
	}
	if seals != requiredSeals {
		return nil, fmt.Errorf("%w: sealed image missing required seals (got %#x want %#x)", ErrSealedImageMismatch, seals, requiredSeals)
	}

	rehashed, err := readAllFd(fd)
	if err != nil {
		return nil, fmt.Errorf("%w: re-hash sealed image through fd: %v", ErrSealedLaunch, err)
	}
	if got := sha256Hex(rehashed); got != digest {
		return nil, fmt.Errorf("%w: sealed memfd content %s does not match source %s", ErrSealedImageMismatch, got, digest)
	}

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("%w: fstat sealed memfd: %v", ErrSealedLaunch, err)
	}

	ok = true
	return &SealedImage{
		ArgV0:  absPath,
		Digest: digest,
		fd:     fd,
	}, nil
}

// Close releases the sealed memfd. Safe to call on a nil *SealedImage or
// more than once.
func (s *SealedImage) Close() error {
	// fd <= 0 covers both "already closed" (fd set to -1 below) and a
	// zero-value *SealedImage built outside NewSealedImage: fd 0 is
	// always stdin in a running process, never a real memfd, so there
	// is nothing of ours to close.
	if s == nil || s.fd <= 0 {
		return nil
	}
	err := unix.Close(s.fd)
	s.fd = -1
	return err
}

func (s *SealedImage) devIno() (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(s.fd, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

// startSealed launches req.SealedImage as a pinned, ptrace-verified
// child: it dups the sealed fd for this one launch, re-verifies the
// seals and content digest, execs the dup via /proc/self/fd/<n> under
// ptrace, waits for the exec-stop, verifies /proc/<pid>/exe identity
// against the sealed image before the child runs a single instruction,
// and detaches. env and cleanup are threaded through unchanged from the
// caller (Start): env is the same scrubbed environment an ordinary
// launch would use, and cleanup is invoked when the returned process's
// Wait completes (the network proxy teardown closure), exactly as for a
// non-sealed launch.
func startSealed(ctx context.Context, req LaunchRequest, env []string, cleanup func()) (ManagedProcess, error) {
	img := req.SealedImage

	cmd, closeDup, err := buildSealedCmd(ctx, req, env)
	if err != nil {
		return nil, err
	}
	defer closeDup()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdinPipe.Close()
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdinPipe.Close()
		_ = stdoutPipe.Close()
		return nil, fmt.Errorf("create stderr pipe: %w", err)
	}

	type traceResult struct {
		ident ExeIdentity
		err   error
	}
	resCh := make(chan traceResult, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ident, err := runSealedTracer(cmd, img, sealedPtraceHooks, true)
		resCh <- traceResult{ident, err}
	}()
	res := <-resCh
	if res.err != nil {
		return nil, res.err
	}

	return &managedProcess{
		cmd:       cmd,
		stdinPipe: stdinPipe,
		stdout:    stdoutPipe,
		stderr:    stderrPipe,
		cleanup:   cleanup,
		exeIdentity: ExeIdentity{
			DevIno: res.ident.DevIno,
			Digest: res.ident.Digest,
		},
	}, nil
}

// buildSealedCmd dups req.SealedImage's fd for a single launch (CLOEXEC:
// the kernel opens the file to exec before the dup is closed at exec
// time, so CLOEXEC is exactly correct here), re-verifies its seals and
// content digest, and returns an *exec.Cmd wired to exec that dup via
// /proc/self/fd/<n> with Args[0] set back to the image's original path
// so the child's own view of argv[0] looks normal. The returned closer
// closes the dup; callers should defer it after a successful build (it
// is also invoked internally on every error path).
func buildSealedCmd(ctx context.Context, req LaunchRequest, env []string) (*exec.Cmd, func(), error) {
	img := req.SealedImage

	dupFd, err := unix.FcntlInt(uintptr(img.fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, func() {}, fmt.Errorf("%w: dup sealed image fd: %v", ErrSealedLaunch, err)
	}
	closeDup := func() { _ = unix.Close(dupFd) }

	seals, err := unix.FcntlInt(uintptr(dupFd), unix.F_GET_SEALS, 0)
	if err != nil {
		closeDup()
		return nil, func() {}, fmt.Errorf("%w: get seals at launch: %v", ErrSealedLaunch, err)
	}
	if seals != requiredSeals {
		closeDup()
		return nil, func() {}, fmt.Errorf("%w: sealed image missing required seals at launch (got %#x want %#x)", ErrSealedImageMismatch, seals, requiredSeals)
	}

	data, err := readAllFd(dupFd)
	if err != nil {
		closeDup()
		return nil, func() {}, fmt.Errorf("%w: re-hash sealed image at launch: %v", ErrSealedLaunch, err)
	}
	if got := sha256Hex(data); got != img.Digest {
		closeDup()
		return nil, func() {}, fmt.Errorf("%w: sealed image digest %s does not match pinned %s at launch", ErrSealedImageMismatch, got, img.Digest)
	}

	execPath := fmt.Sprintf("/proc/self/fd/%d", dupFd)
	cmd := exec.CommandContext(ctx, execPath, req.Args...)
	cmd.Args[0] = img.ArgV0
	cmd.Dir = req.Paths.Root
	cmd.Env = env

	if err := configureSysProcAttr(req, cmd); err != nil {
		closeDup()
		return nil, func() {}, fmt.Errorf("configure isolation sysprocattr: %w", err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Ptrace = true
	cmd.SysProcAttr.Setpgid = true

	return cmd, closeDup, nil
}

// ptraceOps seams every ptrace-adjacent syscall used while tracing a
// sealed-image launch through the exec-stop, so tests can inject a
// misbehaving tracer without touching real kernel state.
type ptraceOps interface {
	wait4(pid int, ws *syscall.WaitStatus, options int) (int, error)
	setOptions(pid int, options int) error
	detach(pid int) error
	kill(pid int) error
}

type realPtraceOps struct{}

func (realPtraceOps) wait4(pid int, ws *syscall.WaitStatus, options int) (int, error) {
	return syscall.Wait4(pid, ws, options, nil)
}

func (realPtraceOps) setOptions(pid int, options int) error {
	return unix.PtraceSetOptions(pid, options)
}

func (realPtraceOps) detach(pid int) error {
	return unix.PtraceDetach(pid)
}

func (realPtraceOps) kill(pid int) error {
	return unix.Kill(pid, syscall.SIGKILL)
}

// sealedPtraceHooks is the production ptraceOps implementation. Tests in
// this package may swap it out for the duration of a single (serial)
// subtest.
var sealedPtraceHooks ptraceOps = realPtraceOps{}

// runSealedTracer starts cmd (which must already be configured for a
// ptrace'd launch via buildSealedCmd), waits for the exec-stop, arms
// PTRACE_O_EXITKILL, verifies the child's /proc/<pid>/exe identity
// against img, and — when detach is true — detaches so the child runs
// as an ordinary process from here on. On any failure the child is
// killed and reaped before returning; on success with detach false the
// child is left ptrace-stopped and still attached (used by the
// PTRACE_O_EXITKILL test helper, which then exits without detaching so
// the kernel kills the child on tracer death).
//
// Every ptrace call goes through hooks, and cmd.Start() itself is called
// from here: on Linux, PTRACE_TRACEME binds the tracee to the specific
// OS thread that forked it, so the caller must have already called
// runtime.LockOSThread before invoking this function and must keep the
// thread locked until it returns.
func runSealedTracer(cmd *exec.Cmd, img *SealedImage, hooks ptraceOps, detach bool) (ExeIdentity, error) {
	if err := cmd.Start(); err != nil {
		return ExeIdentity{}, fmt.Errorf("%w: start sealed trampoline: %v", ErrSealedLaunch, err)
	}
	pid := cmd.Process.Pid

	ws, err := waitForStopRetryingEINTR(pid, hooks)
	if err != nil {
		killAndReap(cmd, hooks)
		return ExeIdentity{}, fmt.Errorf("%w: wait4 for exec-stop: %v", ErrSealedLaunch, err)
	}
	if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
		killAndReap(cmd, hooks)
		return ExeIdentity{}, fmt.Errorf("%w: exec-stop wait status %v is not a SIGTRAP stop", ErrSealedLaunch, ws)
	}

	if err := hooks.setOptions(pid, unix.PTRACE_O_EXITKILL); err != nil {
		killAndReap(cmd, hooks)
		return ExeIdentity{}, fmt.Errorf("%w: PTRACE_O_EXITKILL: %v", ErrSealedLaunch, err)
	}

	ident, err := verifyExecIdentity(pid, img)
	if err != nil {
		killAndReap(cmd, hooks)
		return ExeIdentity{}, err
	}

	if detach {
		if err := hooks.detach(pid); err != nil {
			killAndReap(cmd, hooks)
			return ExeIdentity{}, fmt.Errorf("%w: ptrace detach: %v", ErrSealedLaunch, err)
		}
	}

	return ident, nil
}

// waitForStopRetryingEINTR waits for pid's next ptrace stop using
// syscall.WALL, as the brief specifies, retrying on EINTR rather than
// surfacing it — the same retry rule killAndReap's reap applies.
func waitForStopRetryingEINTR(pid int, hooks ptraceOps) (syscall.WaitStatus, error) {
	var ws syscall.WaitStatus
	for {
		_, err := hooks.wait4(pid, &ws, syscall.WALL)
		if err == syscall.EINTR {
			continue
		}
		return ws, err
	}
}

// killAndReap SIGKILLs cmd's process and reaps it via cmd.Wait(). Unlike
// a raw wait4, cmd.Wait() also closes the parent ends of the stdin/
// stdout/stderr pipes exec.Cmd opened for this launch (its
// "close-after-wait" set) and releases the exec.CommandContext
// ctx-watcher goroutine tied to cmd.Process — a raw reap leaves both
// leaked. SIGKILL clears a ptrace-stop synchronously (the kernel never
// defers SIGKILL for a traced task, even mid-stop), and a traced
// child's exit is reapable from any thread in the tracer's thread
// group, so calling cmd.Wait() here — from the same
// runtime.LockOSThread-pinned goroutine that called cmd.Start() — is
// safe and loops internally until the child is Exited() or Signaled().
// Used on every sealed-launch failure path after cmd.Start() has
// succeeded; never called more than once per cmd (a successful
// runSealedTracer return hands cmd to a managedProcess, whose own
// Wait() owns cmd.Wait() from then on, and Cmd.Wait() itself rejects a
// second call).
func killAndReap(cmd *exec.Cmd, hooks ptraceOps) {
	_ = hooks.kill(cmd.Process.Pid)
	_ = cmd.Wait()
}

// isMemfdExeTarget reports whether target — the os.Readlink of a
// /proc/<pid>/exe symlink — points at a memfd-backed image. The kernel
// appends " (deleted)" once a memfd has no remaining directory links,
// which is immediately true for a sealed image (it is never linked
// into any directory), so that suffix is accepted by design.
func isMemfdExeTarget(target string) bool {
	return strings.HasPrefix(target, "/memfd:")
}

// verifyExecIdentity checks, at the ptrace exec-stop (before the tracee
// has run a single instruction), that /proc/<pid>/exe is exactly the
// sealed image: same device:inode as the sealed memfd, a memfd-backed
// symlink target, and matching content digest.
func verifyExecIdentity(pid int, img *SealedImage) (ExeIdentity, error) {
	exePath := fmt.Sprintf("/proc/%d/exe", pid)

	if _, err := os.Lstat(exePath); err != nil {
		return ExeIdentity{}, fmt.Errorf("%w: lstat %s: %v", ErrSealedImageMismatch, exePath, err)
	}

	fi, err := os.Stat(exePath)
	if err != nil {
		return ExeIdentity{}, fmt.Errorf("%w: stat %s: %v", ErrSealedImageMismatch, exePath, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ExeIdentity{}, fmt.Errorf("%w: unsupported stat_t for %s", ErrSealedImageMismatch, exePath)
	}
	gotDevIno := fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	wantDevIno, err := img.devIno()
	if err != nil {
		return ExeIdentity{}, fmt.Errorf("%w: fstat sealed image for comparison: %v", ErrSealedLaunch, err)
	}
	if gotDevIno != wantDevIno {
		return ExeIdentity{}, fmt.Errorf("%w: %s dev:ino %s does not match sealed image %s", ErrSealedImageMismatch, exePath, gotDevIno, wantDevIno)
	}

	target, err := os.Readlink(exePath)
	if err != nil {
		return ExeIdentity{}, fmt.Errorf("%w: readlink %s: %v", ErrSealedImageMismatch, exePath, err)
	}
	if !isMemfdExeTarget(target) {
		return ExeIdentity{}, fmt.Errorf("%w: %s target %q is not a sealed memfd image", ErrSealedImageMismatch, exePath, target)
	}

	f, err := os.Open(exePath)
	if err != nil {
		return ExeIdentity{}, fmt.Errorf("%w: open %s: %v", ErrSealedImageMismatch, exePath, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ExeIdentity{}, fmt.Errorf("%w: hash %s: %v", ErrSealedImageMismatch, exePath, err)
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if digest != img.Digest {
		return ExeIdentity{}, fmt.Errorf("%w: %s digest %s does not match sealed image %s", ErrSealedImageMismatch, exePath, digest, img.Digest)
	}

	return ExeIdentity{DevIno: gotDevIno, Digest: digest}, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeAllFd(fd int, data []byte) error {
	off := 0
	for off < len(data) {
		n, err := unix.Write(fd, data[off:])
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("short write: wrote %d of %d bytes", off, len(data))
		}
		off += n
	}
	return nil
}

func readAllFd(fd int) ([]byte, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	buf := make([]byte, st.Size)
	var off int64
	for off < st.Size {
		n, err := unix.Pread(fd, buf[off:], off)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		off += int64(n)
	}
	if off != st.Size {
		return nil, fmt.Errorf("short read: got %d of %d bytes", off, st.Size)
	}
	return buf, nil
}
