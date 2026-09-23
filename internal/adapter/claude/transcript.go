package claude

// §3.6 transcript path derivation and local trust inspection. The path
// is derived ONLY from the binding's own fields — never accepted from
// the native side or callers. Inspection is local evidence for
// ResumeSession (§3.4): for a materialized binding the transcript must
// exist, pass integrity checks (containment across ALL path
// components, regular-file mode, ownership, size bounds), carry a user
// entry, and correlate with the attempt records' prompt digests —
// proof THIS transcript is THIS session's.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// verifyPathComponentSymlinks rejects symlink components on the
// transcript's path from the file itself up to and including the trust
// root (the per-session config root, §3.6 containment). Components
// ABOVE the trust root are operator/platform reality (e.g. macOS
// /var -> /private/var) and are not evidence; if the walk reaches the
// filesystem root without meeting the trust root, the transcript is
// not contained in the config root and fails closed.
func verifyPathComponentSymlinks(path, trustRoot string) error {
	current := path
	for {
		st, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("bound transcript path component %s is missing: %w", current, err)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("bound transcript path component %s is a symlink; containment violated", current)
		}
		if current == trustRoot {
			return nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("bound transcript %s is not contained in the config root %s", path, trustRoot)
		}
		current = parent
	}
}

var (
	// MaxTranscriptFileBytes bounds the transcript file size; larger
	// files fail closed rather than being parsed.
	MaxTranscriptFileBytes = int64(64 << 20)
	// MaxTranscriptLineBytes bounds a single NDJSON entry; the tail
	// beyond the bound fails closed.
	MaxTranscriptLineBytes = 1 << 20
)

// TranscriptPath derives the bound transcript path for a session:
// <config-root>/projects/<munged-cwd>/<native-id>.jsonl (spec §3.6).
// The native ID must be a valid UUIDv4; the workspace is munged with
// the native rule (every non-alphanumeric byte becomes '-'). The rule
// itself is fixture-verified only until Task 7's operator-run
// integration evidence.
func TranscriptPath(configRoot, workspace, nativeID string) (string, error) {
	if !isValidUUIDv4(nativeID) {
		return "", fmt.Errorf("transcript path requires a valid UUIDv4 native id, got %q", nativeID)
	}
	if configRoot == "" {
		return "", fmt.Errorf("transcript path requires a config root")
	}
	if workspace == "" {
		return "", fmt.Errorf("transcript path requires the bound workspace")
	}
	return filepath.Join(configRoot, "projects", mungeCWD(workspace), nativeID+".jsonl"), nil
}

// mungeCWD applies the native cwd-munging rule: every byte that is not
// ASCII alphanumeric becomes '-'.
func mungeCWD(p string) string {
	munged := []byte(p)
	for i, b := range munged {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') {
			continue
		}
		munged[i] = '-'
	}
	return string(munged)
}

// TranscriptInspection reports what local transcript evidence exists.
type TranscriptInspection struct {
	Path      string
	Entries   int
	UserTexts []string
}

// InspectTranscript validates the bound transcript end to end:
//
//   - every path component from the config root down is a real
//     directory/file, never a symlink (§3.6 containment);
//   - the transcript is a regular file owned by the current user with
//     no group/other permission bits (0600 family); on Windows the
//     inspection fails closed (no ACL-equivalent capability yet);
//   - the file and each NDJSON entry are within the size bounds;
//   - every complete entry is valid JSON; a torn FINAL line without a
//     newline is a tolerated tail (§3.9);
//   - at least one `user` entry carries prompt text.
func InspectTranscript(path, trustRoot string) (*TranscriptInspection, error) {
	if !transcriptTrustSupported() {
		return nil, fmt.Errorf("transcript trust inspection is not supported on %s; failing closed (§3.6)", runtime.GOOS)
	}
	if err := verifyPathComponentSymlinks(path, trustRoot); err != nil {
		return nil, err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("bound transcript is missing: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("bound transcript %s is not a regular file", path)
	}
	if err := verifyTranscriptOwnership(st); err != nil {
		return nil, err
	}
	// §3.6: a regular transcript file with 0600 semantics — owner
	// read/write exactly, no group or other access, no executable or
	// write-only variants.
	if st.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("bound transcript %s must have 0600 permissions, got %o", path, st.Mode().Perm())
	}
	if st.Size() > MaxTranscriptFileBytes {
		return nil, fmt.Errorf("bound transcript %s exceeds the %d-byte file bound", path, MaxTranscriptFileBytes)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open bound transcript: %w", err)
	}
	defer f.Close()

	ins := &TranscriptInspection{Path: path}
	reader := bufio.NewReader(io.LimitReader(f, MaxTranscriptFileBytes+1))
	total := int64(0)
	for {
		line, readErr := readBoundedTranscriptLine(reader, &total, path)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			// Size-bound violation: fail closed, never tolerate.
			return nil, readErr
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var entry struct {
				Type    string `json:"type"`
				Message struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if jsonErr := json.Unmarshal(trimmed, &entry); jsonErr != nil {
				if readErr == nil {
					return nil, fmt.Errorf("bound transcript %s has a malformed entry", path)
				}
				// Torn final line without newline: tolerated tail.
				break
			}
			ins.Entries++
			if entry.Type == "user" {
				if text := userEntryText(entry.Message); strings.TrimSpace(text) != "" {
					ins.UserTexts = append(ins.UserTexts, text)
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	if ins.Entries == 0 {
		return nil, fmt.Errorf("bound transcript %s carries no entries", path)
	}
	if len(ins.UserTexts) == 0 {
		return nil, fmt.Errorf("bound transcript %s carries no user entry", path)
	}
	return ins, nil
}

// readBoundedTranscriptLine reads one newline-terminated entry,
// failing closed when a single entry exceeds MaxTranscriptLineBytes or
// the file exceeds MaxTranscriptFileBytes.
func readBoundedTranscriptLine(reader *bufio.Reader, total *int64, path string) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		*total += int64(len(chunk))
		if *total > MaxTranscriptFileBytes {
			return nil, fmt.Errorf("bound transcript %s exceeds the %d-byte file bound", path, MaxTranscriptFileBytes)
		}
		if len(line) > MaxTranscriptLineBytes {
			return nil, fmt.Errorf("bound transcript %s has an entry exceeding the %d-byte line bound", path, MaxTranscriptLineBytes)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if len(line) > 0 && line[len(line)-1] != '\n' && (err == nil || err == io.EOF) {
			// Torn tail: return what exists; the caller tolerates it.
			return line, err
		}
		return line, err
	}
}

// userEntryText extracts the prompt text from a transcript user
// entry's message content: either a plain string or an array of
// content blocks (first text block wins).
func userEntryText(msg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}) string {
	var text string
	if err := json.Unmarshal(msg.Content, &text); err == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" {
				return b.Text
			}
		}
	}
	return ""
}

// CorrelateTranscriptPrompt proves THIS transcript is THIS session's
// (§3.4 step 3): an AUTHORITATIVE attempt of this session — explicitly
// accepted (protected mode) or carrying a verified completed terminal —
// must have a user entry in the transcript whose prompt text hashes,
// under that attempt's identity, to the attempt's stored prompt
// digest. Entries belonging to uncertain or missing attempts are never
// authoritative.
func CorrelateTranscriptPrompt(path, trustRoot string, attempts []*storage.ClaudeTurnAttempt) error {
	ins, err := InspectTranscript(path, trustRoot)
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		if !attemptPromptAuthoritative(attempt) {
			continue
		}
		for _, text := range ins.UserTexts {
			if PromptDigest(text, attempt.AttemptID) == attempt.PromptDigest {
				return nil
			}
		}
	}
	return fmt.Errorf(
		"bound transcript %s carries no accepted user entry matching any recorded prompt digest of this session", path)
}

// attemptPromptAuthoritative reports whether the attempt's recorded
// prompt digest is trustworthy session evidence: only an explicitly
// accepted attempt (protected mode) or one with a VERIFIED terminal
// result qualifies — completed or failed. A verified failed terminal
// (e.g. error_max_turns) still proves the native process accepted that
// prompt, so a materialized failed first turn can resume.
func attemptPromptAuthoritative(a *storage.ClaudeTurnAttempt) bool {
	if a == nil || strings.TrimSpace(a.PromptDigest) == "" {
		return false
	}
	if a.Accepted != nil && *a.Accepted {
		return true
	}
	if !a.Terminal {
		return false
	}
	return a.ObservedStatus == "completed" || a.ObservedStatus == "failed"
}
