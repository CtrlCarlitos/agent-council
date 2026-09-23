//go:build unix

package codex

// Rollout baseline scanning for the codex adapter (AC-009 spec §3.7):
// the durable turn-correlation baseline is the rollout's
// (file-identity, byte size, entry count). The path is NOT derivable
// from the thread id alone (date components), so first acceptance
// resolves it by a bounded scan constrained to the exact UUID suffix,
// verifies session_meta.session_id == the native id, and enforces the
// path-integrity rules: no symlinks, contained under the expected
// sessions root. POSIX ownership/mode identity; Windows is a fail-closed
// platform statement (rollout_windows.go, §3.7 honest gap).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxRolloutScanEntries bounds the UUID-suffix scan: the sessions tree
// grows without bound in the operator home, so the walk aborts (fail
// closed) rather than scanning the whole tree.
const maxRolloutScanEntries = 4096

// locateRollout resolves the rollout path for a native thread id by a
// bounded scan under $CODEX_HOME/sessions constrained to the exact
// "-<uuid>.jsonl" filename suffix. Exactly one match must exist: zero
// means the rollout was not observed, more than one is an ambiguous
// evidence state — both fail closed.
func locateRollout(codexHome, nativeID string) (string, error) {
	root := filepath.Join(codexHome, "sessions")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("sessions root %s was not observed: %w", root, err)
	}
	suffix := "-" + nativeID + ".jsonl"
	var matches []string
	scanned := 0
	walkErr := filepath.WalkDir(resolvedRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		scanned++
		if scanned > maxRolloutScanEntries {
			return fmt.Errorf("rollout scan exceeded the %d-entry bound", maxRolloutScanEntries)
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("symlink %s inside the sessions root", path)
		}
		if strings.HasSuffix(d.Name(), suffix) {
			matches = append(matches, path)
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("rollout scan under %s: %w", resolvedRoot, walkErr)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("rollout for thread %s was not observed under %s", nativeID, resolvedRoot)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("rollout scan for thread %s is ambiguous: %d candidate files", nativeID, len(matches))
	}
}

// checkRolloutPath enforces the path-integrity rules for a recorded
// rollout path: a regular non-symlink file resolving inside the expected
// sessions root.
func checkRolloutPath(path, codexHome string) error {
	root := filepath.Join(codexHome, "sessions")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("sessions root %s was not observed: %w", root, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("rollout %s was not observed: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return fmt.Errorf("rollout %s is not a regular file", path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve rollout %s: %w", path, err)
	}
	if real != path && !strings.HasPrefix(real, resolvedRoot+string(os.PathSeparator)) {
		return fmt.Errorf("rollout %s resolves outside the sessions root", path)
	}
	return nil
}

// scanRolloutBaseline reads the rollout baseline: file identity (POSIX
// dev:ino), byte size, and parsed entry count. The FIRST entry must be
// the session_meta line carrying session_id == native id. A torn final
// line is ignored; a mid-file parse failure invalidates the read.
func scanRolloutBaseline(path, nativeID string) (identity string, size int64, entries int, err error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", 0, 0, fmt.Errorf("stat rollout: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return "", 0, 0, fmt.Errorf("rollout %s is not a regular file", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		identity = fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	}

	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, fmt.Errorf("open rollout: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), MaxFrameBytes)
	first := true
	for {
		if !sc.Scan() {
			break
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry rolloutEntry
		if uerr := json.Unmarshal(line, &entry); uerr != nil {
			if sc.Scan() {
				// Content follows the bad line: mid-file parse failure
				// invalidates the read (spec §3.7).
				return "", 0, 0, fmt.Errorf("rollout %s is corrupt at entry %d: %w", path, entries+1, uerr)
			}
			// Nothing followed: a torn final line is ignored.
			break
		}
		if first {
			first = false
			if entry.Type != "session_meta" || entry.Payload.SessionID != nativeID {
				return "", 0, 0, fmt.Errorf(
					"rollout %s session_meta does not match thread %s (type=%q session_id=%q)",
					path, nativeID, entry.Type, entry.Payload.SessionID)
			}
		}
		entries++
	}
	if serr := sc.Err(); serr != nil {
		return "", 0, 0, fmt.Errorf("read rollout %s: %w", path, serr)
	}
	if first {
		return "", 0, 0, fmt.Errorf("rollout %s carries no session_meta", path)
	}
	return identity, fi.Size(), entries, nil
}

// rolloutEntry is the subset of a rollout JSONL line the baseline reads.
type rolloutEntry struct {
	Type    string `json:"type"`
	Payload struct {
		SessionID string `json:"session_id"`
	} `json:"payload"`
}
