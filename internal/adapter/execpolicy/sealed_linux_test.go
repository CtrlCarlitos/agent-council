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
//   - "slow": sleeps 300ms, then prints a line.
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
		fmt.Println("slow-done")
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

	cmd := exec.Command(fixturePath, "fast")
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true, Setpgid: true}
	cmd.Stdout = io.Discard
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

	start := time.Now()
	proc, err := New().Start(context.Background(), req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Start took %v; expected exec-stop verification to complete well before the 300ms sleep", elapsed)
	}

	out, err := io.ReadAll(proc.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if strings.TrimSpace(string(out)) != "slow-done" {
		t.Fatalf("stdout = %q, want %q", out, "slow-done")
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
		SessionID:   "sess-exitkill-helper",
		Command:     img.ArgV0,
		Args:        []string{"fast"},
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

func pollTraceeReaped(t *testing.T, pid int) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var ws syscall.WaitStatus
		if wpid, _ := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); wpid == pid {
			return true
		}
		if err := unix.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
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
