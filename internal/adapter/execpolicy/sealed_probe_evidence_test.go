//go:build evidence && linux

package execpolicy_test

// AC-010 operator evidence helper (spec §3.7/§7: "Stage A compares
// sealed vs. path launches before any freeze"). NOT part of CI: it is
// compiled only with `-tags evidence` and runs only when the operator's
// evidence script (scripts/ac010-integration-evidence.sh) supplies the
// AC010_SEALED_PROBE_* environment. It launches ONE child through the
// production sealed path — execpolicy.NewSealedImage (memfd, seals,
// digest re-verified) + PolicyExecutor.Start (ptrace exec-stop identity
// check) — with exactly the argv the operator names, and records stdout,
// stderr, and the exit code for comparison with the script's own path
// launch under the same executor environment.
//
// It never chooses a binary, argv, home, or cwd on its own; forbidden
// agy flags are refused by the executor itself (agyForbiddenArg).
//
// Inputs (all required unless noted):
//   AC010_SEALED_PROBE_BIN      absolute path of the binary to seal (basename agy)
//   AC010_SEALED_PROBE_DIGEST   its "sha256:<hex>" digest
//   AC010_SEALED_PROBE_HOME     the child's HOME (the parent of expected_home)
//   AC010_SEALED_PROBE_CWD      an existing directory: the child's cwd
//   AC010_SEALED_PROBE_ARGS     the argv after argv0, fields joined by U+001F
//   AC010_SEALED_PROBE_OUT      output path prefix (.stdout/.stderr/.exit are written)
//   AC010_SEALED_PROBE_TIMEOUT  optional seconds (default 120); the child is
//                               terminated at the bound and the exit file says so

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const sealedProbeCaptureLimit = 8 << 20

type boundedBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := sealedProbeCaptureLimit - b.buf.Len(); room < len(p) {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.overflow = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func TestSealedProbe(t *testing.T) {
	bin := os.Getenv("AC010_SEALED_PROBE_BIN")
	if bin == "" {
		t.Skip("operator evidence helper: set AC010_SEALED_PROBE_* (scripts/ac010-integration-evidence.sh)")
	}
	env := func(k string) string {
		v := os.Getenv(k)
		if strings.TrimSpace(v) == "" {
			t.Fatalf("%s is required", k)
		}
		return v
	}
	digest, home, cwd, out := env("AC010_SEALED_PROBE_DIGEST"), env("AC010_SEALED_PROBE_HOME"),
		env("AC010_SEALED_PROBE_CWD"), env("AC010_SEALED_PROBE_OUT")
	var args []string
	if raw := os.Getenv("AC010_SEALED_PROBE_ARGS"); raw != "" {
		args = strings.Split(raw, "\x1f")
	}
	timeout := 120 * time.Second
	if raw := os.Getenv("AC010_SEALED_PROBE_TIMEOUT"); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			t.Fatalf("AC010_SEALED_PROBE_TIMEOUT must be a positive integer, got %q", raw)
		}
		timeout = time.Duration(secs) * time.Second
	}
	if !filepath.IsAbs(bin) || filepath.Base(bin) != "agy" {
		t.Fatalf("AC010_SEALED_PROBE_BIN must be an absolute path whose basename is agy, got %q", bin)
	}

	img, err := execpolicy.NewSealedImage(bin, digest)
	if err != nil {
		t.Fatalf("sealed image (digest re-verified through the descriptor): %v", err)
	}
	defer img.Close()

	req := execpolicy.LaunchRequest{
		RunID:     "ac010-sealed-probe",
		SessionID: "ac010-sealed-probe",
		Command:   img.ArgV0,
		Args:      args,
		Paths:     workspace.WorkspacePaths{Root: cwd, Scratch: cwd, Config: cwd},
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v4",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"agy"},
		},
		SealedImage: img,
		HomeDir:     filepath.Clean(home),
	}
	proc, err := execpolicy.New().Start(context.Background(), req)
	if err != nil {
		writeProbe(t, out+".exit", fmt.Sprintf("start_refused: %v\n", err))
		t.Fatalf("sealed launch refused: %v", err)
	}
	// Empty stdin: the provider-free probes never transmit a prompt.
	_ = proc.Stdin().Close()

	var stdout, stderr boundedBuffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&stdout, proc.Stdout()) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&stderr, proc.Stderr()) }()

	type waited struct {
		code int
		err  error
	}
	done := make(chan waited, 1)
	go func() {
		wg.Wait()
		code, err := proc.Wait()
		done <- waited{code, err}
	}()
	var status string
	select {
	case w := <-done:
		status = fmt.Sprintf("exit=%d err=%v\n", w.code, w.err)
	case <-time.After(timeout):
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = proc.Terminate(ctx)
		cancel()
		w := <-done
		status = fmt.Sprintf("timeout_after=%s exit=%d err=%v\n", timeout, w.code, w.err)
	}
	id := proc.ExecutableIdentity()
	status += fmt.Sprintf("exe_dev_ino=%s exe_digest=%s\n", id.DevIno, id.Digest)
	if stdout.overflow || stderr.overflow {
		status += fmt.Sprintf("capture_truncated_at=%d\n", sealedProbeCaptureLimit)
	}
	writeProbe(t, out+".stdout", stdout.buf.String())
	writeProbe(t, out+".stderr", stderr.buf.String())
	writeProbe(t, out+".exit", status)
	t.Logf("sealed probe %q: %s", strings.Join(args, " "), strings.TrimSpace(status))
}

func writeProbe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
