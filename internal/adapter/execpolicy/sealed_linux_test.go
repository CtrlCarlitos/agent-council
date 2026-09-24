//go:build linux

package execpolicy

import (
	"bytes"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// fixtureSource is a tiny Go program built once in TestMain. Its
// behavior is selected by argv[1]:
//   - "fast" (default): prints a line and exits immediately.
//   - "slow": sleeps 300ms, then prints a line carrying a wall-clock
//     marker (its own time.Now().UnixNano()) so a caller can assert the
//     marker is later than some point it captured itself, instead of
//     racing a short non-blocking read.
//   - "sleep": sleeps 10s — long enough that a test can reliably act on
//     the tracee (EXITKILL, Interrupt) well before it would exit on its
//     own.
//   - "fds": lists /proc/self/fd link targets, one per line (used by
//     the CLOEXEC test: no target should start with "/memfd:").
const fixtureSource = `package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	mode := "fast"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "slow":
		time.Sleep(300 * time.Millisecond)
		fmt.Println("slow-done", time.Now().UnixNano())
	case "sleep":
		time.Sleep(10 * time.Second)
		fmt.Println("sleep-done")
	case "fds":
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			fmt.Println("fds-error:", err)
			return
		}
		for _, e := range entries {
			target, lerr := os.Readlink("/proc/self/fd/" + e.Name())
			if lerr != nil {
				continue
			}
			fmt.Println(target)
		}
	default:
		fmt.Println("fast-done")
	}
}
`

var (
	fixtureDir      string
	fixturePath     string
	fixtureDigest   string
	fixtureBuildErr error
)

// TestMain builds the fixture binary once for the whole package test
// run (skipped in the AGY_SEALED_HELPER re-exec, which receives the
// fixture path from its parent instead).
func TestMain(m *testing.M) {
	if os.Getenv("AGY_SEALED_HELPER") == "" {
		dir, err := os.MkdirTemp("", "execpolicy-sealed-fixture-*")
		if err != nil {
			fixtureBuildErr = err
		} else {
			fixtureDir = dir
			fixturePath, fixtureBuildErr = buildFixture(dir)
			if fixtureBuildErr == nil {
				data, rerr := os.ReadFile(fixturePath)
				if rerr != nil {
					fixtureBuildErr = rerr
				} else {
					fixtureDigest = sha256Hex(data)
				}
			}
		}
	}

	code := m.Run()

	if fixtureDir != "" {
		_ = os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

func buildFixture(dir string) (string, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return "", fmt.Errorf("go toolchain unavailable: %w", err)
	}
	srcPath := filepath.Join(dir, "fixture_main.go")
	if err := os.WriteFile(srcPath, []byte(fixtureSource), 0o600); err != nil {
		return "", err
	}
	binPath := filepath.Join(dir, "fixture_bin")
	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build fixture: %w: %s", err, out)
	}
	return binPath, nil
}

func requireFixture(t *testing.T) {
	t.Helper()
	if fixtureBuildErr != nil {
		t.Skipf("sealed-image fixture unavailable (go toolchain?): %v", fixtureBuildErr)
	}
}

func sealedTestPaths(t *testing.T) workspace.WorkspacePaths {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "worktree")
	config := filepath.Join(tmp, "config")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	return workspace.WorkspacePaths{Root: root, Config: config, Worktree: root, Mode: "isolated_branch"}
}

func sealedTestProfile(tooling []string) storage.CanonicalProfile {
	return storage.CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "isolated_branch",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             tooling,
	}
}

func pollGone(t *testing.T, pid int) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := unix.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// --- seals ---------------------------------------------------------------

func TestSealedImage_SealsPreventWriteAndTruncate(t *testing.T) {
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	seals, err := unix.FcntlInt(uintptr(img.fd), unix.F_GET_SEALS, 0)
	if err != nil {
		t.Fatalf("F_GET_SEALS: %v", err)
	}
	if seals != requiredSeals {
		t.Fatalf("seals = %#x, want %#x", seals, requiredSeals)
	}

	if _, err := unix.Write(img.fd, []byte("x")); err == nil {
		t.Fatalf("expected write to sealed memfd to be refused")
	}
	if err := unix.Ftruncate(img.fd, 1); err == nil {
		t.Fatalf("expected truncate of sealed memfd to be refused")
	}
}

func TestSealedImage_MissingSealRefusedAtLaunch(t *testing.T) {
	requireFixture(t)

	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fd, err := unix.MemfdCreate("agy-test-partial-seal", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatalf("memfd_create: %v", err)
	}
	defer unix.Close(fd)
	if err := writeAllFd(fd, data); err != nil {
		t.Fatalf("write memfd: %v", err)
	}
	// Seal everything except F_SEAL_WRITE: the image is still writable,
	// which must be refused at launch even though shrink/grow/reseal
	// are blocked.
	partial := unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, partial); err != nil {
		t.Fatalf("add partial seals: %v", err)
	}

	img := &SealedImage{ArgV0: fixturePath, Digest: fixtureDigest, fd: fd}
	req := LaunchRequest{
		SessionID:   "sess-missing-seal",
		Command:     fixturePath,
		Args:        []string{"fast"},
		Paths:       sealedTestPaths(t),
		Profile:     sealedTestProfile([]string{fixturePath}),
		SealedImage: img,
	}

	_, _, err = buildSealedCmd(context.Background(), req, os.Environ())
	if !errors.Is(err, ErrSealedImageMismatch) {
		t.Fatalf("expected ErrSealedImageMismatch, got %v", err)
	}
}

// --- digest ----------------------------------------------------------------

func TestNewSealedImage_DigestMismatchRefused(t *testing.T) {
	requireFixture(t)

	wrong := "sha256:" + strings.Repeat("0", 64)
	img, err := NewSealedImage(fixturePath, wrong)
	if img != nil {
		defer img.Close()
		t.Fatalf("expected no image on digest mismatch")
	}
	if !errors.Is(err, ErrSealedImageMismatch) {
		t.Fatalf("expected ErrSealedImageMismatch, got %v", err)
	}
}

func TestBuildSealedCmd_DigestMismatchRefusedAtLaunch(t *testing.T) {
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()
	img.Digest = "sha256:" + strings.Repeat("1", 64) // tamper the pinned digest after construction

	req := LaunchRequest{
		SessionID:   "sess-digest-mismatch",
		Command:     img.ArgV0,
		Args:        []string{"fast"},
		Paths:       sealedTestPaths(t),
		Profile:     sealedTestProfile([]string{img.ArgV0}),
		SealedImage: img,
	}
	_, _, err = buildSealedCmd(context.Background(), req, os.Environ())
	if !errors.Is(err, ErrSealedImageMismatch) {
		t.Fatalf("expected ErrSealedImageMismatch, got %v", err)
	}
}

// TestBuildSealedCmd_MergesPtraceIntoStrictSysProcAttr guards the
// controller ruling that a sealed launch must MERGE Ptrace/Setpgid into
// whatever configureSysProcAttr produced for a strict, network_mode=none
// profile (new user+net namespaces), never replace it.
func TestBuildSealedCmd_MergesPtraceIntoStrictSysProcAttr(t *testing.T) {
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	profile := sealedTestProfile([]string{img.ArgV0})
	profile.IsolationStrictness = "strict"
	profile.NetworkMode = "none"

	req := LaunchRequest{
		SessionID:   "sess-merge-sysprocattr",
		Command:     img.ArgV0,
		Args:        []string{"fast"},
		Paths:       sealedTestPaths(t),
		Profile:     profile,
		SealedImage: img,
	}
	cmd, closeDup, err := buildSealedCmd(context.Background(), req, os.Environ())
	if err != nil {
		t.Fatalf("buildSealedCmd: %v", err)
	}
	defer closeDup()

	if cmd.SysProcAttr == nil {
		t.Fatalf("expected SysProcAttr to be set")
	}
	if !cmd.SysProcAttr.Ptrace {
		t.Fatalf("expected Ptrace to be true")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatalf("expected Setpgid to be true")
	}
	wantCloneflags := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET)
	if cmd.SysProcAttr.Cloneflags != wantCloneflags {
		t.Fatalf("expected strict isolation Cloneflags %#x to survive the merge, got %#x", wantCloneflags, cmd.SysProcAttr.Cloneflags)
	}
}

// --- different image (argv0 shape check + real exe verification) --------

func TestStart_SealedImage_DifferentCommandPathRefused(t *testing.T) {
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	copyPath := fixturePath + ".copy"
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	data = append(data, 0x00) // differ the content/digest from the original
	if err := os.WriteFile(copyPath, data, 0o755); err != nil {
		t.Fatalf("write copy: %v", err)
	}
	defer os.Remove(copyPath)

	req := LaunchRequest{
		SessionID:   "sess-diff-image-argv0",
		Command:     copyPath,
		Args:        []string{"fast"},
		Paths:       sealedTestPaths(t),
		Profile:     sealedTestProfile([]string{copyPath}),
		SealedImage: img,
	}
	proc, err := New().Start(context.Background(), req)
	if proc != nil {
		t.Fatalf("expected no process for command/argv0 mismatch")
	}
	if !errors.Is(err, ErrInvalidLaunchRequest) {
		t.Fatalf("expected ErrInvalidLaunchRequest, got %v", err)
	}
}

// TestVerifyExecIdentity_DifferentImageKillsChild exercises the
// /proc/<pid>/exe verification itself (not just the argv0 shape check
// above): a plain path launch of the fixture binary — a real on-disk
// file, not the sealed memfd — must be refused because its exe identity
// does not match the sealed image, and the tracee must be killed and
// reaped, not left running or read from.
func TestVerifyExecIdentity_DifferentImageKillsChild(t *testing.T) {
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	var stdout bytes.Buffer
	cmd := exec.Command(fixturePath, "fast")
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true, Setpgid: true}
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard

	type result struct {
		ident ExeIdentity
		err   error
		pid   int
	}
	resCh := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ident, terr := runSealedTracer(cmd, img, sealedPtraceHooks, true)
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
		resCh <- result{ident, terr, pid}
	}()
	r := <-resCh

	if !errors.Is(r.err, ErrSealedImageMismatch) {
		t.Fatalf("expected ErrSealedImageMismatch, got %v", r.err)
	}
	if r.pid == 0 {
		t.Fatalf("expected a tracee pid to have been observed")
	}
	if !pollGone(t, r.pid) {
		t.Fatalf("expected tracee %d to be killed and reaped after exe mismatch", r.pid)
	}
	// The child was killed at the exec-stop, before it ran a single
	// instruction: it must never have gotten far enough to write
	// "fast-done" to stdout.
	if stdout.Len() != 0 {
		t.Fatalf("expected no stdout to have been produced before the dev:ino mismatch killed the child, got %q", stdout.String())
	}
}

// TestVerifyExecIdentity_DigestMismatchKillsChild exercises the digest
// branch of verifyExecIdentity specifically: dev:ino and the memfd
// readlink prefix both pass (the child really is the sealed image, exec'd
// through the sealed fd via buildSealedCmd), but the SealedImage handed to
// the verification call has a tampered Digest, so only the final content-hash
// comparison can catch it. This is unreachable through TestVerifyExecIdentity_DifferentImageKillsChild,
// which fails at the earlier dev:ino check.
func TestVerifyExecIdentity_DigestMismatchKillsChild(t *testing.T) {
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	req := LaunchRequest{
		SessionID:   "sess-digest-branch",
		Command:     img.ArgV0,
		Args:        []string{"fast"},
		Paths:       sealedTestPaths(t),
		Profile:     sealedTestProfile([]string{img.ArgV0}),
		SealedImage: img,
	}

	type result struct {
		pid    int
		stdout string
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// Build and start the launch normally, against the real
		// (untampered) image, so dev:ino and the memfd readlink prefix
		// both genuinely pass.
		cmd, closeDup, berr := buildSealedCmd(context.Background(), req, os.Environ())
		if berr != nil {
			resCh <- result{err: berr}
			return
		}
		defer closeDup()
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = io.Discard

		if serr := cmd.Start(); serr != nil {
			resCh <- result{err: serr}
			return
		}
		pid := cmd.Process.Pid

		ws, werr := waitForStopRetryingEINTR(pid, sealedPtraceHooks)
		if werr != nil {
			killAndReap(cmd, sealedPtraceHooks)
			resCh <- result{pid: pid, err: werr}
			return
		}
		if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
			killAndReap(cmd, sealedPtraceHooks)
			resCh <- result{pid: pid, err: fmt.Errorf("not a SIGTRAP stop: %v", ws)}
			return
		}
		if soerr := sealedPtraceHooks.setOptions(pid, unix.PTRACE_O_EXITKILL); soerr != nil {
			killAndReap(cmd, sealedPtraceHooks)
			resCh <- result{pid: pid, err: soerr}
			return
		}

		// Only the verification call gets a tampered Digest: dev:ino
		// and the readlink prefix are checked against the real memfd
		// (same fd underlying both img and tampered), so only the
		// content-hash comparison can fail here.
		tampered := *img
		tampered.Digest = "sha256:" + strings.Repeat("f", 64)
		_, verr := verifyExecIdentity(pid, &tampered)
		killAndReap(cmd, sealedPtraceHooks)
		resCh <- result{pid: pid, stdout: stdout.String(), err: verr}
	}()
	r := <-resCh

	if !errors.Is(r.err, ErrSealedImageMismatch) {
		t.Fatalf("expected ErrSealedImageMismatch (digest branch), got %v", r.err)
	}
	if r.pid == 0 {
		t.Fatalf("expected a tracee pid to have been observed")
	}
	if !pollGone(t, r.pid) {
		t.Fatalf("expected tracee %d to be killed and reaped after digest mismatch", r.pid)
	}
	if r.stdout != "" {
		t.Fatalf("expected no stdout before the digest mismatch killed the child, got %q", r.stdout)
	}
}

// TestIsMemfdExeTarget unit-tests the readlink-prefix comparison
// verifyExecIdentity relies on, including the "(deleted)" suffix the
// kernel appends once a memfd has no directory links (always true for a
// sealed image), independent of any real ptrace/exec machinery.
func TestIsMemfdExeTarget(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{"plain memfd target", "/memfd:agy", true},
		{"deleted-suffix memfd target accepted", "/memfd:agy (deleted)", true},
		{"regular on-disk exe rejected", "/usr/bin/agy", false},
		{"deleted regular file rejected", "/usr/bin/agy (deleted)", false},
		{"empty target rejected", "", false},
		{"memfd-like prefix without colon rejected", "/memfdagy", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isMemfdExeTarget(c.target); got != c.want {
				t.Fatalf("isMemfdExeTarget(%q) = %v, want %v", c.target, got, c.want)
			}
		})
	}
}

// --- successful launches ---------------------------------------------------

func TestStart_SealedImage_FastChildVerifiedAndReleased(t *testing.T) {
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	req := LaunchRequest{
		SessionID:   "sess-fast",
		Command:     img.ArgV0,
		Args:        []string{"fast"},
		Paths:       sealedTestPaths(t),
		Profile:     sealedTestProfile([]string{img.ArgV0}),
		SealedImage: img,
	}
	proc, err := New().Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ident := proc.ExecutableIdentity()
	if ident.Digest != img.Digest {
		t.Fatalf("ExecutableIdentity.Digest = %q, want %q", ident.Digest, img.Digest)
	}
	if ident.DevIno == "" {
		t.Fatalf("expected non-empty DevIno for a sealed-image launch")
	}

	out, err := io.ReadAll(proc.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if strings.TrimSpace(string(out)) != "fast-done" {
		t.Fatalf("stdout = %q, want %q", out, "fast-done")
	}
	if _, err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestStart_SealedImage_SlowChildVerifiedBeforeOutput(t *testing.T) {
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	req := LaunchRequest{
		SessionID:   "sess-slow",
		Command:     img.ArgV0,
		Args:        []string{"slow"},
		Paths:       sealedTestPaths(t),
		Profile:     sealedTestProfile([]string{img.ArgV0}),
		SealedImage: img,
	}

	// Structural, not timing-based: capture the instant Start returns,
	// then require the child's own wall-clock marker (printed only
	// after its 300ms sleep, from its own process) to be later than
	// that instant. That can only hold if exec-stop verification
	// finished, and Start returned, before the child ran past its
	// sleep — a non-blocking-read timing window would flake under
	// -race on a loaded runner; this cannot.
	proc, err := New().Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	startReturned := time.Now()

	out, err := io.ReadAll(proc.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 2 || fields[0] != "slow-done" {
		t.Fatalf("stdout = %q, want %q", out, "slow-done <marker>")
	}
	markerNanos, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("parse child wall-clock marker %q: %v", fields[1], err)
	}
	childPrinted := time.Unix(0, markerNanos)
	if !childPrinted.After(startReturned) {
		t.Fatalf("child printed at %v, which is not after Start returned at %v; exec-stop verification must complete (and Start must return) before the child runs its sleep and prints", childPrinted, startReturned)
	}

	if _, err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// --- CLOEXEC ---------------------------------------------------------------

func TestStart_SealedImage_NoLeakedMemfdDescriptor(t *testing.T) {
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	req := LaunchRequest{
		SessionID:   "sess-cloexec",
		Command:     img.ArgV0,
		Args:        []string{"fds"},
		Paths:       sealedTestPaths(t),
		Profile:     sealedTestProfile([]string{img.ArgV0}),
		SealedImage: img,
	}
	proc, err := New().Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	out, err := io.ReadAll(proc.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "/memfd:") {
			t.Fatalf("child inherited a memfd descriptor: %q\nfull listing:\n%s", line, out)
		}
	}
	if _, err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// --- tracer failure ---------------------------------------------------------

// fakePtraceOps overrides one ptraceOps method (the first call only,
// for wait4) while delegating everything else, including the reap that
// follows a failure, to a real tracer so the injected fault never
// leaves a zombie behind.
type fakePtraceOps struct {
	real ptraceOps

	mu         sync.Mutex
	wait4Calls int

	wait4Fn      func(pid int, ws *syscall.WaitStatus, options int) (int, error)
	setOptionsFn func(pid int, options int) error
}

func (f *fakePtraceOps) wait4(pid int, ws *syscall.WaitStatus, options int) (int, error) {
	f.mu.Lock()
	f.wait4Calls++
	n := f.wait4Calls
	f.mu.Unlock()
	if n == 1 && f.wait4Fn != nil {
		return f.wait4Fn(pid, ws, options)
	}
	return f.real.wait4(pid, ws, options)
}

func (f *fakePtraceOps) setOptions(pid int, options int) error {
	if f.setOptionsFn != nil {
		return f.setOptionsFn(pid, options)
	}
	return f.real.setOptions(pid, options)
}

func (f *fakePtraceOps) detach(pid int) error { return f.real.detach(pid) }
func (f *fakePtraceOps) kill(pid int) error   { return f.real.kill(pid) }

func TestStart_SealedImage_TracerFailure(t *testing.T) {
	requireFixture(t)

	t.Run("non_sigtrap_stop", func(t *testing.T) {
		var capturedPID int
		fake := &fakePtraceOps{
			real: realPtraceOps{},
			wait4Fn: func(pid int, ws *syscall.WaitStatus, options int) (int, error) {
				capturedPID = pid
				*ws = syscall.WaitStatus(0) // reports "exited", not a SIGTRAP stop
				return pid, nil
			},
		}
		runSealedTracerFailureCase(t, fake, &capturedPID)
	})

	t.Run("setoptions_fails", func(t *testing.T) {
		var capturedPID int
		fake := &fakePtraceOps{
			real: realPtraceOps{},
			setOptionsFn: func(pid int, options int) error {
				capturedPID = pid
				return fmt.Errorf("injected PTRACE_SETOPTIONS failure")
			},
		}
		runSealedTracerFailureCase(t, fake, &capturedPID)
	})
}

func runSealedTracerFailureCase(t *testing.T, fake *fakePtraceOps, capturedPID *int) {
	t.Helper()
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	prev := sealedPtraceHooks
	sealedPtraceHooks = fake
	defer func() { sealedPtraceHooks = prev }()

	req := LaunchRequest{
		SessionID:   "sess-tracer-fail",
		Command:     img.ArgV0,
		Args:        []string{"fast"},
		Paths:       sealedTestPaths(t),
		Profile:     sealedTestProfile([]string{img.ArgV0}),
		SealedImage: img,
	}

	// fd-count regression guard for Important-2: the failure path must
	// reap via cmd.Wait() (which closes the parent stdin/stdout/stderr
	// pipe ends and releases the ctx-watcher goroutine's grip on the
	// pidfd), not a raw wait4 that drops cmd and leaks all three. The
	// only fd this call is expected to hold open across the failed
	// Start is img's own sealed memfd (deferred Close above), which is
	// open in both snapshots, so before == after exactly.
	before := openFDCount(t)

	proc, err := New().Start(context.Background(), req)
	if proc != nil {
		t.Fatalf("expected no process on tracer failure")
	}
	if !errors.Is(err, ErrSealedLaunch) {
		t.Fatalf("expected ErrSealedLaunch, got %v", err)
	}
	if *capturedPID == 0 {
		t.Fatalf("expected the fake to have observed a pid")
	}
	if !pollGone(t, *capturedPID) {
		t.Fatalf("expected pid %d to be gone (no zombie) after tracer failure", *capturedPID)
	}

	after := openFDCount(t)
	if after != before {
		t.Fatalf("open fd count regression: before=%d after=%d; failed sealed launch leaked fds (expected the stdin/stdout/stderr pipe parent ends and the ctx-watcher's pidfd grip to be released by cmd.Wait())", before, after)
	}
}

// openFDCount counts this process's currently open file descriptors via
// /proc/self/fd. Used to catch fd leaks (pipes, pidfds) on the sealed
// launch failure path without depending on any particular fd number.
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}

// --- PTRACE_O_EXITKILL -------------------------------------------------

// TestHelperSealedTracer is not a real test: invoked only via re-exec
// with AGY_SEALED_HELPER=1, it builds a sealed image, starts the
// trampoline, reaches the exec-stop, arms PTRACE_O_EXITKILL, prints the
// tracee's pid, and exits WITHOUT detaching — so the kernel kills the
// tracee once this tracer process dies. See
// TestPtraceExitKillOnTracerDeath.
func TestHelperSealedTracer(t *testing.T) {
	if os.Getenv("AGY_SEALED_HELPER") != "1" {
		t.Skip("not invoked as the sealed-tracer helper")
	}

	fixture := os.Getenv("AGY_SEALED_FIXTURE_PATH")
	digest := os.Getenv("AGY_SEALED_FIXTURE_DIGEST")

	img, err := NewSealedImage(fixture, digest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: NewSealedImage: %v\n", err)
		os.Exit(1)
	}

	req := LaunchRequest{
		SessionID: "sess-exitkill-helper",
		Command:   img.ArgV0,
		// "sleep" (long-running), not "fast": without PTRACE_O_EXITKILL
		// the kernel's tracer-exit path detaches and wakes the stopped
		// tracee, and a "fast" tracee would print and exit 0 on its
		// own within milliseconds, making the outer test's reap check
		// pass whether or not EXITKILL actually fired. "sleep" cannot
		// exit on its own inside the poll window, so a positive
		// SIGKILL reap is only possible if EXITKILL did its job.
		Args:        []string{"sleep"},
		Paths:       workspace.WorkspacePaths{Root: filepath.Dir(fixture), Config: filepath.Dir(fixture), Worktree: filepath.Dir(fixture)},
		Profile:     sealedTestProfile([]string{img.ArgV0}),
		SealedImage: img,
	}

	cmd, closeDup, err := buildSealedCmd(context.Background(), req, os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: buildSealedCmd: %v\n", err)
		os.Exit(1)
	}
	_ = closeDup // deliberately not called: this process exits abruptly, no cleanup.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	runtime.LockOSThread()
	// detach=false: leave the tracee ptrace-stopped and still attached
	// so that PTRACE_O_EXITKILL fires when this process exits below.
	_, err = runSealedTracer(cmd, img, sealedPtraceHooks, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: runSealedTracer: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "%d\n", cmd.Process.Pid)
	os.Exit(0)
}

func TestPtraceExitKillOnTracerDeath(t *testing.T) {
	requireFixture(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable to build the re-exec helper")
	}

	// Become a subreaper so the tracee — orphaned when the helper
	// process below exits — reparents to us and we can positively
	// confirm it was reaped, not merely guess from kill(pid,0).
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("PR_SET_CHILD_SUBREAPER unavailable: %v", err)
	}
	defer func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0) }()

	helper := exec.Command(os.Args[0], "-test.run=^TestHelperSealedTracer$")
	helper.Env = append(os.Environ(),
		"AGY_SEALED_HELPER=1",
		"AGY_SEALED_FIXTURE_PATH="+fixturePath,
		"AGY_SEALED_FIXTURE_DIGEST="+fixtureDigest,
	)
	var stdout, stderr bytes.Buffer
	helper.Stdout = &stdout
	helper.Stderr = &stderr

	if err := helper.Run(); err != nil {
		t.Fatalf("helper process failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	pidStr := strings.TrimSpace(stdout.String())
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("could not parse tracee pid from helper stdout %q: %v", stdout.String(), err)
	}

	if !pollTraceeReaped(t, pid) {
		t.Fatalf("expected tracee %d to be killed by PTRACE_O_EXITKILL once its tracer exited", pid)
	}
}

// pollTraceeReaped requires a positive wait4 reap of pid whose exit
// status is a SIGKILL signal death — proof PTRACE_O_EXITKILL actually
// fired, not merely that the pid is no longer visible. The caller
// (TestPtraceExitKillOnTracerDeath) has made itself a
// PR_SET_CHILD_SUBREAPER, so the orphaned tracee always reparents to it
// and is always reapable here; a bare ESRCH ("no such process") success
// path — the previous version of this helper — would also be produced
// by an EXITKILL-less tracee that simply ran to completion and exited
// 0 on its own, which is exactly the bug this test exists to catch, so
// ESRCH/ECHILD/any wait error is treated as failure, not success.
func pollTraceeReaped(t *testing.T, pid int) bool {
	t.Helper()
	// The "sleep" fixture used by the helper sleeps 10s; give a broken
	// (unarmed) EXITKILL time to let it run to completion on its own
	// before concluding it never got reaped at all.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if err != nil && err != syscall.EINTR {
			// ECHILD here would mean the subreaper never saw pid as
			// its child at all — a real failure, not a race to retry.
			return false
		}
		if wpid == pid {
			return ws.Signaled() && ws.Signal() == syscall.SIGKILL
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// --- ManagedProcess.Interrupt / ExecutableIdentity coverage --------------

// TestManagedProcess_Interrupt_SealedImage launches the long-running
// "sleep" fixture through a sealed-image Start, calls Interrupt(), and
// confirms the child dies of SIGINT — the fixture installs no signal
// handler, so an unhandled SIGINT terminates it via the OS default
// disposition.
func TestManagedProcess_Interrupt_SealedImage(t *testing.T) {
	requireFixture(t)

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	req := LaunchRequest{
		SessionID:   "sess-interrupt",
		Command:     img.ArgV0,
		Args:        []string{"sleep"},
		Paths:       sealedTestPaths(t),
		Profile:     sealedTestProfile([]string{img.ArgV0}),
		SealedImage: img,
	}
	proc, err := New().Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = proc.Terminate(ctx)
	})

	if err := proc.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	if _, err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	mp, ok := proc.(*managedProcess)
	if !ok {
		t.Fatalf("expected *managedProcess, got %T", proc)
	}
	ws, ok := mp.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("expected syscall.WaitStatus, got %T", mp.cmd.ProcessState.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Fatalf("expected the child to have been killed by SIGINT, got wait status %v", ws)
	}
}

// TestManagedProcess_ExecutableIdentity_PathLaunch exercises the
// ordinary (non-sealed) path-launch branch of ExecutableIdentity: it
// must report {Path: req.Command, Digest: ""}, not the sealed-launch
// {DevIno, Digest} shape.
func TestManagedProcess_ExecutableIdentity_PathLaunch(t *testing.T) {
	requireFixture(t)

	req := LaunchRequest{
		SessionID: "sess-path-identity",
		Command:   fixturePath,
		Args:      []string{"fast"},
		Paths:     sealedTestPaths(t),
		Profile:   sealedTestProfile([]string{fixturePath}),
	}
	proc, err := New().Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = proc.Terminate(ctx)
	})

	ident := proc.ExecutableIdentity()
	if ident.Path != fixturePath {
		t.Fatalf("ExecutableIdentity.Path = %q, want %q", ident.Path, fixturePath)
	}
	if ident.Digest != "" {
		t.Fatalf("ExecutableIdentity.Digest = %q, want empty for a path launch", ident.Digest)
	}
	if ident.DevIno != "" {
		t.Fatalf("ExecutableIdentity.DevIno = %q, want empty for a path launch", ident.DevIno)
	}

	if _, err := io.ReadAll(proc.Stdout()); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if _, err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// --- controller ruling: strict isolation + sealed image ------------------

// strictNetworkNoneUnavailable probes, with a real trial launch, whether
// this host can actually create the new user+net namespaces strict
// network_mode=none isolation requires — the availability check baked
// into checkPlatformCapabilities (os.Stat on /proc/self/ns/user|net)
// only confirms the kernel exposes namespaces at all, not that
// unprivileged user-namespace creation is allowed (e.g. distros/hosts
// that set kernel.unprivileged_userns_clone=0, or containers/sandboxes
// that block CLONE_NEWUSER outright).
func strictNetworkNoneUnavailable(t *testing.T) string {
	t.Helper()
	trial := exec.Command(fixturePath, "fast")
	trial.Stdout = io.Discard
	trial.Stderr = io.Discard
	trial.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		GidMappingsEnableSetgroups: false,
	}
	if err := trial.Run(); err != nil {
		return err.Error()
	}
	return ""
}

// TestStart_SealedImage_StrictNetworkNone is the controller-ruling test:
// a sealed-image launch under a strict, network_mode=none profile
// (request shape copied from executor_linux_test.go's
// TestPolicyExecutor_LinuxStrictKernelIsolation) must still pass sealed
// verification and read fully — the strict Cloneflags merge
// (TestBuildSealedCmd_MergesPtraceIntoStrictSysProcAttr) must actually
// produce a working launch end to end, not just a correctly-shaped
// SysProcAttr. Skip-guarded: not hard-required, since some hosts refuse
// unprivileged user namespaces outright.
func TestStart_SealedImage_StrictNetworkNone(t *testing.T) {
	requireFixture(t)

	if msg := strictNetworkNoneUnavailable(t); msg != "" {
		t.Skipf("user/network namespaces unavailable for strict network_mode=none: %s", msg)
	}

	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	defer img.Close()

	profile := sealedTestProfile([]string{img.ArgV0})
	profile.IsolationStrictness = "strict"
	profile.NetworkMode = "none"
	profile.Harnesses = map[string]storage.HarnessProfileSpec{
		"agy": {Model: "gemini-2.5-pro", NativeAuthMode: "inherited_host_keychain"},
	}

	req := LaunchRequest{
		SessionID:   "sess-strict-none-sealed",
		Command:     img.ArgV0,
		Args:        []string{"fast"},
		Paths:       sealedTestPaths(t),
		Profile:     profile,
		SealedImage: img,
	}
	proc, err := New().Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = proc.Terminate(ctx)
	})

	ident := proc.ExecutableIdentity()
	if ident.Digest != img.Digest {
		t.Fatalf("ExecutableIdentity.Digest = %q, want %q", ident.Digest, img.Digest)
	}
	if ident.DevIno == "" {
		t.Fatalf("expected non-empty DevIno for a sealed-image launch")
	}

	out, err := io.ReadAll(proc.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if strings.TrimSpace(string(out)) != "fast-done" {
		t.Fatalf("stdout = %q, want %q", out, "fast-done")
	}
	if _, err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// sanity: sha256Hex matches the standard library for a trivial input,
// guarding the digest helper shared by NewSealedImage/buildSealedCmd.
func TestSha256Hex(t *testing.T) {
	sum := sha256.Sum256([]byte("hello"))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := sha256Hex([]byte("hello")); got != want {
		t.Fatalf("sha256Hex = %q, want %q", got, want)
	}
}
