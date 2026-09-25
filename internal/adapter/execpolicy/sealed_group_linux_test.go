//go:build linux

package execpolicy

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startSealedSpawner launches the sealed fixture in mode and returns the
// process with the pid of the descendant it spawned into its group.
func startSealedSpawner(t *testing.T, mode string) (ManagedProcess, int) {
	t.Helper()
	requireFixture(t)
	img, err := NewSealedImage(fixturePath, fixtureDigest)
	if err != nil {
		t.Fatalf("NewSealedImage: %v", err)
	}
	t.Cleanup(func() { _ = img.Close() })
	proc, err := New().Start(context.Background(), LaunchRequest{
		SessionID:   "sess-" + mode,
		Command:     img.ArgV0,
		Args:        []string{mode},
		Paths:       sealedTestPaths(t),
		Profile:     sealedTestProfile([]string{img.ArgV0}),
		SealedImage: img,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	line, err := bufio.NewReader(proc.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 1 {
		t.Fatalf("descendant pid line %q: %v", line, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return proc, pid
}

// descendantGone reports whether pid no longer runs (absent, or a
// killed not-yet-reaped zombie).
func descendantGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	fields := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

func requireDescendantGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !descendantGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived %s", pid, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A sealed leader that ends NORMALLY (no Terminate) leaves no same-group
// descendant: the wait path peeks at the exit without reaping,
// SIGKILLs the group, then reaps. (The descendant ignores SIGTERM.)
func TestSealedLaunch_NormalExitKillsGroupDescendants(t *testing.T) {
	proc, pid := startSealedSpawner(t, "spawn-exit")
	if descendantGone(pid) {
		t.Fatal("the descendant must be alive before the leader is waited")
	}
	code, err := proc.Wait()
	if err != nil || code != 0 {
		t.Fatalf("the leader exits 0 on its own, got %d err=%v", code, err)
	}
	requireDescendantGone(t, pid, "its sealed leader's normal exit")
}

// A graceful Terminate (the leader exits on SIGTERM before the deadline)
// leaves no same-group descendant, even one that ignores SIGTERM.
func TestSealedLaunch_GracefulTerminateKillsGroupDescendants(t *testing.T) {
	proc, pid := startSealedSpawner(t, "spawn-wait")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := proc.Terminate(ctx); err != nil {
		t.Fatalf("the leader exits on SIGTERM (graceful path), got %v", err)
	}
	requireDescendantGone(t, pid, "a graceful Terminate of its sealed leader")
}

// The forced path (already-expired context) still kills the group.
func TestSealedLaunch_ForcedTerminateKillsGroupDescendants(t *testing.T) {
	proc, pid := startSealedSpawner(t, "spawn-wait")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = proc.Terminate(ctx)
	requireDescendantGone(t, pid, "a forced Terminate of its sealed leader")
}
