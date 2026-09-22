//go:build unix

package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscriptPath_DerivationGolden(t *testing.T) {
	got, err := TranscriptPath("/base/run/sess/config", "/tmp/ws/root.dir", "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")
	if err != nil {
		t.Fatalf("transcript path: %v", err)
	}
	want := filepath.Join("/base/run/sess/config", "projects", "-tmp-ws-root-dir", "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d.jsonl")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTranscriptPath_RejectsMalformedNativeID(t *testing.T) {
	if _, err := TranscriptPath("/base", "/ws", "not-a-uuid"); err == nil {
		t.Fatal("malformed native id must be rejected")
	}
}

func TestInspectTranscript_Missing(t *testing.T) {
	dir := t.TempDir()
	if _, err := InspectTranscript(filepath.Join(dir, "absent.jsonl")); err == nil {
		t.Fatal("missing transcript must fail")
	}
}

func TestInspectTranscript_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.jsonl")
	os.WriteFile(real, []byte(`{"type":"user"}`+"\n"), 0o600)
	link := filepath.Join(dir, "bound.jsonl")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := InspectTranscript(link); err == nil {
		t.Fatal("symlinked transcript must be rejected")
	}
}

func TestInspectTranscript_ValidWithUserEntry(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bound.jsonl")
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"prompt"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"result","subtype":"success"}`,
		"",
	}, "\n")
	os.WriteFile(p, []byte(transcript), 0o600)
	ins, err := InspectTranscript(p)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if ins.Entries != 3 || len(ins.UserTexts) != 1 {
		t.Fatalf("unexpected inspection %+v", ins)
	}
}

func TestInspectTranscript_TornTailToleratedMidstreamCorruptionRejected(t *testing.T) {
	dir := t.TempDir()

	// A torn FINAL line is a tolerated tail.
	p := filepath.Join(dir, "torn.jsonl")
	torn := "{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"x\"}}\n{\"type\":\"assi"
	os.WriteFile(p, []byte(torn), 0o600)
	if ins, err := InspectTranscript(p); err != nil || ins.Entries != 1 {
		t.Fatalf("torn tail must be tolerated, got %+v err=%v", ins, err)
	}

	// Corruption before the final newline is a malformed entry.
	q := filepath.Join(dir, "corrupt.jsonl")
	os.WriteFile(q, []byte("{\"type\":\"user\"\nNOT JSON\n"), 0o600)
	if _, err := InspectTranscript(q); err == nil {
		t.Fatal("mid-stream corruption must be rejected")
	}
}

func TestInspectTranscript_NoUserEntryRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bound.jsonl")
	os.WriteFile(p, []byte(`{"type":"system","subtype":"init"}`+"\n"), 0o600)
	if _, err := InspectTranscript(p); err == nil {
		t.Fatal("transcript without a user entry must fail")
	}
}

func TestInspectTranscript_ParentComponentSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	realProjects := t.TempDir()
	linkProjects := filepath.Join(dir, "projects")
	if err := os.Symlink(realProjects, linkProjects); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	p := filepath.Join(linkProjects, "bound.jsonl")
	os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user","content":"x"}}`+"\n"), 0o600)
	if _, err := InspectTranscript(p); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked parent component must be rejected, got %v", err)
	}
}

func TestInspectTranscript_GroupReadableRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bound.jsonl")
	os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user","content":"x"}}`+"\n"), 0o600)
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := InspectTranscript(p); err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("group-readable transcript must be rejected, got %v", err)
	}
}

func TestInspectTranscript_OversizeFileRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bound.jsonl")
	// MaxTranscriptFileBytes is far too large to allocate; instead
	// verify the bound check directly with a temporarily lowered cap.
	defer func(old int64) { MaxTranscriptFileBytes = old }(MaxTranscriptFileBytes)
	MaxTranscriptFileBytes = 1 << 10
	os.WriteFile(p, []byte(strings.Repeat(`{"type":"user","message":{"role":"user","content":"x"}}`+"\n", 64)), 0o600)
	if _, err := InspectTranscript(p); err == nil || !strings.Contains(err.Error(), "file bound") {
		t.Fatalf("oversize transcript must be rejected, got %v", err)
	}
}

func TestInspectTranscript_OversizeLineRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bound.jsonl")
	defer func(old int) { MaxTranscriptLineBytes = old }(MaxTranscriptLineBytes)
	MaxTranscriptLineBytes = 1 << 10
	os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user","content":"`+strings.Repeat("x", 1<<11)+`"}}`+"\n"), 0o600)
	if _, err := InspectTranscript(p); err == nil || !strings.Contains(err.Error(), "line bound") {
		t.Fatalf("oversize entry must be rejected, got %v", err)
	}
}

func TestInspectTranscript_OwnedByAnotherUserRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("ownership check cannot be exercised as root")
	}
	st, err := os.Stat("/etc/passwd")
	if err != nil {
		t.Skip("no /etc/passwd")
	}
	if err := verifyTranscriptOwnership(st); err == nil {
		t.Fatal("foreign-owned transcript must fail ownership verification")
	}
}
