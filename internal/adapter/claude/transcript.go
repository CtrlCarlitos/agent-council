package claude

// §3.6 transcript path derivation and local integrity inspection. The
// path is derived ONLY from the binding's own fields — never accepted
// from the native side or callers. Inspection is local evidence for
// ResumeSession (§3.4): the transcript must exist and pass integrity
// checks for a materialized binding. Prompt-digest correlation against
// the attempt record (§3.4 step 3) requires the §3.5 acceptance
// machinery and lands with the protected-mode work.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// TranscriptPath derives the bound transcript path for a session:
// <config-root>/projects/<munged-cwd>/<native-id>.jsonl (spec §3.6).
// The native ID must be a valid UUIDv4; the workspace is munged with
// the native rule (every non-alphanumeric byte becomes '-').
func TranscriptPath(configRoot, workspace, nativeID string) (string, error) {
	if !isValidUUIDv4(nativeID) {
		return "", fmt.Errorf("transcript path requires a valid UUIDv4 native id, got %q", nativeID)
	}
	if filepath.Base(configRoot) == "" || configRoot == "" {
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
	UserEntry bool
}

// InspectTranscript validates the local transcript: it must exist as a
// regular file (symlinks rejected), contain only valid NDJSON entries
// (a torn final line is tolerated and discarded, per §3.9), and carry
// at least one `user` entry proving the prompt crossed the native
// session.
func InspectTranscript(path string) (*TranscriptInspection, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("bound transcript is missing: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("bound transcript %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open bound transcript: %w", err)
	}
	defer f.Close()

	ins := &TranscriptInspection{Path: path}
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var entry struct {
				Type string `json:"type"`
			}
			if jsonErr := json.Unmarshal(trimmed, &entry); jsonErr != nil {
				if err == nil {
					return nil, fmt.Errorf("bound transcript has a malformed entry")
				}
				// Torn final line without newline: tolerated tail.
				break
			}
			ins.Entries++
			if entry.Type == "user" {
				ins.UserEntry = true
			}
		}
		if err != nil {
			break
		}
	}
	if ins.Entries == 0 {
		return nil, fmt.Errorf("bound transcript %s carries no entries", path)
	}
	if !ins.UserEntry {
		return nil, fmt.Errorf("bound transcript %s carries no user entry", path)
	}
	return ins, nil
}
