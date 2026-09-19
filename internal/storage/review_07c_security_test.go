package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestReview07C_NestedCredentialKeysRejected verifies that nested keys containing sensitive
// terms (key, token, secret, password, etc.) are rejected even if the value is synthetic,
// while verifying that positive controls (safe identifiers, valid tool configurations, env allowlists) pass.
func TestReview07C_NestedCredentialKeysRejected(t *testing.T) {
	tempDir := t.TempDir()
	store, err := Open(StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "b", "s", "p", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	// 1. Synthetic examples identified in review
	syntheticDisallowed := []struct {
		name   string
		config string
	}{
		{
			name:   "nested ANTHROPIC_API_KEY inside tooling map",
			config: `{"tooling":{"ANTHROPIC_API_KEY":"synthetic-example-value"}}`,
		},
		{
			name:   "nested OPENAI_API_KEY inside tools array of objects",
			config: `{"tools":[{"OPENAI_API_KEY":"synthetic-example-value"}]}`,
		},
		{
			name:   "deeply nested password key",
			config: `{"tooling":{"nested":{"password":"synthetic-example-value"}}}`,
		},
		{
			name:   "secret token key in tools",
			config: `{"tools":[{"secret_token":"sample"}]}`,
		},
		{
			name:   "env_allowlist as map with values instead of array of names",
			config: `{"env_allowlist":{"PATH":"/bin","USER":"carlitos"}}`,
		},
		{
			name:   "env_allowlist containing sensitive key name",
			config: `{"env_allowlist":["PATH","GITHUB_TOKEN"]}`,
		},
	}

	for i, tc := range syntheticDisallowed {
		t.Run(tc.name, func(t *testing.T) {
			opID := fmt.Sprintf("op-bind-disallowed-%d", i)
			_, err := store.SetNativeBinding(ctx, opID, "lease-1", "sess-1", 1, NativeBinding{
				LogicalSessionID: "sess-1",
				NativeSessionID:  "nat-1",
				Harness:          "claude",
				Model:            "claude-3-5-sonnet",
				WorkspaceMode:    "shared",
				ToolingConfig:    tc.config,
			})
			if !errors.Is(err, ErrDisallowedToolingConfig) {
				t.Fatalf("expected ErrDisallowedToolingConfig for %s, got: %v", tc.name, err)
			}
		})
	}

	// 2. Positive controls: valid configurations MUST pass
	positiveControls := []struct {
		name   string
		config string
	}{
		{
			name:   "safe profile identifier",
			config: "profile-v1.2",
		},
		{
			name:   "standard tooling and tools arrays",
			config: `{"tooling":["bash","edit"],"permission_mode":"ask","timeout":300}`,
		},
		{
			name:   "safe env allowlist array of names",
			config: `{"workspace_root":"/workspace","model":"claude-3-7-sonnet","env_allowlist":["PATH","LANG","HOME"]}`,
		},
	}

	for i, tc := range positiveControls {
		t.Run(tc.name, func(t *testing.T) {
			sessID := fmt.Sprintf("sess-allowed-%d", i)
			_, err := store.CreateSession(ctx, fmt.Sprintf("op-sess-allowed-%d", i), "lease-1", SessionRecord{
				ID: sessID, RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: false, State: "parked", Visibility: "reachable",
			})
			if err != nil {
				t.Fatalf("create session %s: %v", sessID, err)
			}
			opID := fmt.Sprintf("op-bind-allowed-%d", i)
			_, err = store.SetNativeBinding(ctx, opID, "lease-1", sessID, 1, NativeBinding{
				LogicalSessionID: sessID,
				NativeSessionID:  fmt.Sprintf("nat-%d", i),
				Harness:          "claude",
				Model:            "claude-3-5-sonnet",
				WorkspaceMode:    "shared",
				ToolingConfig:    tc.config,
			})
			if err != nil {
				t.Fatalf("expected valid config %s to pass, got: %v", tc.name, err)
			}
		})
	}
}

// TestReview07C_RecursiveSymlinkProtection verifies that ensureNoSymlink recursively inspects
// all ancestor components and detects symbolic links pointing to directories, preventing symlink traversal.
func TestReview07C_RecursiveSymlinkProtection(t *testing.T) {
	tempBase := t.TempDir()
	actualDir := filepath.Join(tempBase, "actual")
	if err := os.MkdirAll(filepath.Join(actualDir, "state"), 0755); err != nil {
		t.Fatalf("mkdir actual/state: %v", err)
	}

	aliasDir := filepath.Join(tempBase, "alias")
	if err := os.Symlink(actualDir, aliasDir); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}

	// Helper-level reproduction: calling ensureNoSymlink on alias/state
	nestedState := filepath.Join(aliasDir, "state")
	err := ensureNoSymlink(nestedState)
	if !errors.Is(err, ErrSymlinkForbidden) {
		t.Fatalf("expected ErrSymlinkForbidden when checking nested ancestor symlink %s, got: %v", nestedState, err)
	}

	// Also checking a non-existent child inside the symlinked ancestor
	nestedDB := filepath.Join(aliasDir, "state", "state.db")
	err = ensureNoSymlink(nestedDB)
	if !errors.Is(err, ErrSymlinkForbidden) {
		t.Fatalf("expected ErrSymlinkForbidden when checking child %s inside symlinked ancestor, got: %v", nestedDB, err)
	}
}

// TestReview07C_ReadArtifactSymlinkProtection verifies that ReadArtifact rejects reading
// files whose digest path is reached through a symbolic link.
func TestReview07C_ReadArtifactSymlinkProtection(t *testing.T) {
	tempDir := t.TempDir()
	store, err := Open(StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "b", "s", "p", "lease-1")

	// Publish a valid artifact
	content := []byte("original verified artifact content")
	meta, err := store.PublishArtifact(ctx, "op-pub-1", "lease-1", ArtifactMetadata{
		ID:    "art-1",
		RunID: "run-1",
		Name:  "patch",
	}, content)
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	// Now create a symlink replacing the prefix directory or digest file
	h := sha256.Sum256(content)
	digest := hex.EncodeToString(h[:])
	prefixDir := filepath.Join(tempDir, "artifacts", digest[:2])
	digestPath := filepath.Join(prefixDir, digest)

	// Move original file outside store and create symlink to it
	externalDir := t.TempDir()
	externalFile := filepath.Join(externalDir, "external_blob")
	if err := os.Rename(digestPath, externalFile); err != nil {
		t.Fatalf("move blob: %v", err)
	}
	if err := os.Symlink(externalFile, digestPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	// ReadArtifact must reject the symlink
	_, err = store.ReadArtifact(meta.Digest)
	if err == nil || (!errors.Is(err, ErrSymlinkForbidden) && !errors.Is(err, ErrArtifactCorrupt)) {
		t.Fatalf("expected ErrSymlinkForbidden or ErrArtifactCorrupt when reading symlinked artifact, got: %v", err)
	}
}

// TestReview07C_WindowsACLVerification verifies the locale-independent ACL parser,
// ensuring unauthorized explicit grants and broad group SIDs are rejected while clean owner ACLs pass.
func TestReview07C_WindowsACLVerification(t *testing.T) {
	targetDir := "C:\\State\\agent-council"

	// 1. Clean authorized output
	cleanOutput := `C:\State\agent-council *S-1-3-4:(OI)(CI)(F)
                       NT AUTHORITY\SYSTEM:(OI)(CI)(F)
                       BUILTIN\Administrators:(OI)(CI)(F)
Successfully processed 1 files; Failed processing 0 files
`
	if err := parseAndVerifyIcaclsOutput(targetDir, cleanOutput); err != nil {
		t.Fatalf("expected clean ACL to pass, got: %v", err)
	}

	// 2. Unrelated explicit user grant
	unrelatedUserOutput := `C:\State\agent-council *S-1-3-4:(OI)(CI)(F)
                       NT AUTHORITY\SYSTEM:(OI)(CI)(F)
                       OtherUser:(F)
Successfully processed 1 files; Failed processing 0 files
`
	err := parseAndVerifyIcaclsOutput(targetDir, unrelatedUserOutput)
	if err == nil {
		t.Fatal("expected error for unrelated explicit user grant, got nil")
	}

	// 3. Disallowed Everyone group by name
	everyoneOutput := `C:\State\agent-council Everyone:(R)
                       *S-1-3-4:(OI)(CI)(F)
Successfully processed 1 files; Failed processing 0 files
`
	err = parseAndVerifyIcaclsOutput(targetDir, everyoneOutput)
	if err == nil {
		t.Fatal("expected error for Everyone group, got nil")
	}

	// 4. Disallowed Users group by well-known SID S-1-5-32-545
	sidOutput := `C:\State\agent-council *S-1-5-32-545:(OI)(CI)(R)
                       *S-1-3-4:(OI)(CI)(F)
Successfully processed 1 files; Failed processing 0 files
`
	err = parseAndVerifyIcaclsOutput(targetDir, sidOutput)
	if err == nil {
		t.Fatal("expected error for Users SID S-1-5-32-545, got nil")
	}

	// 5. Disallowed Authenticated Users SID S-1-5-11
	authUsersSIDOutput := `C:\State\agent-council *S-1-5-11:(R)
`
	err = parseAndVerifyIcaclsOutput(targetDir, authUsersSIDOutput)
	if err == nil {
		t.Fatal("expected error for Authenticated Users SID S-1-5-11, got nil")
	}
}

// TestReview07C_ArtifactPublicationAtomicLink verifies that artifact publication
// uses atomic link semantics and never leaves partial incomplete content on disk.
func TestReview07C_ArtifactPublicationAtomicLink(t *testing.T) {
	tempDir := t.TempDir()
	store, err := Open(StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "b", "s", "p", "lease-1")

	payload := []byte("payload for atomic publication test")
	meta, err := store.PublishArtifact(ctx, "op-pub-1", "lease-1", ArtifactMetadata{
		ID:    "art-atomic",
		RunID: "run-1",
		Name:  "patch",
	}, payload)
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	// Verify content read
	data, err := store.ReadArtifact(meta.Digest)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("payload mismatch")
	}

	// Verify staging tmp directory is clean (no orphaned staging files from successful link)
	tmpEntries, err := os.ReadDir(filepath.Join(tempDir, "artifacts", "tmp"))
	if err != nil {
		t.Fatalf("read tmp dir: %v", err)
	}
	if len(tmpEntries) != 0 {
		t.Fatalf("expected tmp directory to be clean, found %d files", len(tmpEntries))
	}
}
