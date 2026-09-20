package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestReview56E_WindowsACLIdentityVerification tests exact identity verification and ensures
// that substring matching does not grant trust to unrelated accounts containing privileged substrings.
func TestReview56E_WindowsACLIdentityVerification(t *testing.T) {
	targetDir := `C:\State\agent-council`

	// 1. Adverse cases: Unrelated principals containing privileged substrings MUST be rejected
	adverseSubstrings := []struct {
		name      string
		principal string
	}{
		{
			name:      "LAB\\DeploymentAdministrators",
			principal: "LAB\\DeploymentAdministrators",
		},
		{
			name:      "LAB\\not-administrators",
			principal: "LAB\\not-administrators",
		},
		{
			name:      "LAB\\Owner Rights Auditors",
			principal: "LAB\\Owner Rights Auditors",
		},
		{
			name:      "LAB\\owner rights backup",
			principal: "LAB\\owner rights backup",
		},
	}

	for _, tc := range adverseSubstrings {
		t.Run(tc.name, func(t *testing.T) {
			output := "C:\\State\\agent-council *S-1-3-4:(OI)(CI)(F)\n" +
				"                      " + tc.principal + ":(OI)(CI)(F)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n"
			err := parseAndVerifyIcaclsOutput(targetDir, output)
			if err == nil {
				t.Fatalf("SECURITY VIOLATION: unrelated principal %q was accepted by substring match", tc.principal)
			}
		})
	}

	// 2. Adverse cases: Missing, unparsed, or malformed inspection output MUST be rejected
	missingOrMalformed := []struct {
		name   string
		output string
	}{
		{
			name:   "empty output",
			output: "",
		},
		{
			name:   "only whitespace",
			output: "   \n\t  \n  ",
		},
		{
			name:   "success summary without descriptor",
			output: "Successfully processed 1 files; Failed processing 0 files\n",
		},
		{
			name:   "unreadable output",
			output: "Error: The system cannot find the file specified.\n",
		},
		{
			name:   "malformed permission entry without closing parenthesis",
			output: "C:\\State\\agent-council *S-1-3-4:(OI)(CI\nSuccessfully processed 1 files; Failed processing 0 files\n",
		},
		{
			name: "valid authorized entry followed by unparsed entry for another principal",
			output: "C:\\State\\agent-council *S-1-3-4:(OI)(CI)(F)\n" +
				"                      UnparsedGarbageLineWithoutColon\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
	}

	for _, tc := range missingOrMalformed {
		t.Run(tc.name, func(t *testing.T) {
			err := parseAndVerifyIcaclsOutput(targetDir, tc.output)
			if err == nil {
				t.Fatalf("SECURITY VIOLATION: missing or malformed output %q was accepted", tc.name)
			}
		})
	}

	// 3. Positive controls: Exact authorized SIDs and canonical names MUST be accepted
	positiveControls := []struct {
		name   string
		output string
	}{
		{
			name: "Owner Rights exact SID S-1-3-4",
			output: "C:\\State\\agent-council *S-1-3-4:(OI)(CI)(F)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
		{
			name: "Local System exact SID S-1-5-18",
			output: "C:\\State\\agent-council *S-1-5-18:(OI)(CI)(F)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
		{
			name: "Builtin Administrators exact SID S-1-5-32-544",
			output: "C:\\State\\agent-council *S-1-5-32-544:(OI)(CI)(F)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
		{
			name: "Canonical NT AUTHORITY\\SYSTEM",
			output: "C:\\State\\agent-council NT AUTHORITY\\SYSTEM:(OI)(CI)(F)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
		{
			name: "Canonical BUILTIN\\Administrators",
			output: "C:\\State\\agent-council BUILTIN\\Administrators:(OI)(CI)(F)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
		{
			name: "Canonical NT AUTHORITY\\Owner Rights",
			output: "C:\\State\\agent-council NT AUTHORITY\\Owner Rights:(OI)(CI)(F)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
	}

	for _, tc := range positiveControls {
		t.Run(tc.name, func(t *testing.T) {
			err := parseAndVerifyIcaclsOutput(targetDir, tc.output)
			if err != nil {
				t.Fatalf("expected valid control %q to pass, got err: %v", tc.name, err)
			}
		})
	}

	// 4. Control rejection: ordinary unrelated account or broad groups must be rejected
	controlRejections := []struct {
		name   string
		output string
	}{
		{
			name: "unrelated user account",
			output: "C:\\State\\agent-council OtherUser:(F)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
		{
			name: "broad group Everyone S-1-1-0",
			output: "C:\\State\\agent-council *S-1-1-0:(R)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
		{
			name: "broad group Users S-1-5-32-545",
			output: "C:\\State\\agent-council *S-1-5-32-545:(R)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
		{
			name: "broad group Authenticated Users S-1-5-11",
			output: "C:\\State\\agent-council *S-1-5-11:(R)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n",
		},
	}

	for _, tc := range controlRejections {
		t.Run(tc.name, func(t *testing.T) {
			err := parseAndVerifyIcaclsOutput(targetDir, tc.output)
			if err == nil {
				t.Fatalf("expected control %q to be rejected, got nil", tc.name)
			}
		})
	}
}

// TestReview56E_WindowsIntegration_UnrelatedExplicitGrant executes real icacls verification
// on Windows platforms to establish actual OS enforcement.
func TestReview56E_WindowsIntegration_UnrelatedExplicitGrant(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("skipping Windows integration test on non-Windows platform")
	}

	dir := t.TempDir()

	// 1. EnsureDirectoryPermissions sets strict owner permissions
	if err := EnsureDirectoryPermissions(dir); err != nil {
		t.Fatalf("EnsureDirectoryPermissions failed: %v", err)
	}

	// 2. VerifyDirectoryPermissions must pass on initial restricted setup
	if err := VerifyDirectoryPermissions(dir); err != nil {
		t.Fatalf("VerifyDirectoryPermissions failed on initial setup: %v", err)
	}

	// 3. Add an unrelated explicit grant to Users via icacls
	grantCmd := exec.Command("icacls", dir, "/grant", "*S-1-5-32-545:(OI)(CI)R")
	if out, err := grantCmd.CombinedOutput(); err != nil {
		t.Fatalf("grant command failed: %v, out: %s", err, string(out))
	}

	// 4. VerifyDirectoryPermissions must detect the unrelated explicit grant and fail
	err := VerifyDirectoryPermissions(dir)
	if err == nil {
		t.Fatal("expected VerifyDirectoryPermissions to reject unrelated explicit grant to Users, but got nil")
	}

	// 5. Remove the explicit grant and verify it passes again
	removeCmd := exec.Command("icacls", dir, "/remove", "*S-1-5-32-545")
	if out, err := removeCmd.CombinedOutput(); err != nil {
		t.Fatalf("remove command failed: %v, out: %s", err, string(out))
	}
	if err := VerifyDirectoryPermissions(dir); err != nil {
		t.Fatalf("VerifyDirectoryPermissions failed after removing grant: %v", err)
	}
}

// TestReview56E_SimulatedArtifactLossDetection verifies simulated missing artifact detection
// and preservation of historical database records.
// (Label: simulated loss detection, not power-loss durability evidence).
func TestReview56E_SimulatedArtifactLossDetection(t *testing.T) {
	tempDir := t.TempDir()
	store, err := Open(StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-loss", "run-loss", "b", "s", "p", "lease-1")

	payload := []byte("artifact content for simulated loss detection")
	meta, err := store.PublishArtifact(ctx, "op-pub-loss", "lease-1", ArtifactMetadata{
		ID:    "art-loss-1",
		RunID: "run-loss",
		Name:  "patch",
	}, payload)
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	// Verify file is readable immediately
	data, err := store.ReadArtifact(meta.Digest)
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("expected initial read to succeed: %v", err)
	}

	// Simulate missing file after publication (e.g. lost link)
	blobPath := filepath.Join(tempDir, "artifacts", meta.Digest[:2], meta.Digest)
	if err := os.Remove(blobPath); err != nil {
		t.Fatalf("simulate loss by removing blob: %v", err)
	}

	// 1. ReadArtifact returns ErrArtifactNotFound
	_, err = store.ReadArtifact(meta.Digest)
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound on simulated missing blob, got: %v", err)
	}

	// 2. ReadArtifactRevision returns ErrArtifactNotFound without exposing unverified bytes
	_, _, err = store.ReadArtifactRevision(ctx, meta.ID, meta.Revision)
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound on ReadArtifactRevision for missing blob, got: %v", err)
	}

	// 3. Historical relational records remain preserved in database
	var count int
	err = store.readDB.QueryRowContext(ctx, "SELECT count(*) FROM artifact_revisions WHERE artifact_id = ? AND revision = ?;", meta.ID, meta.Revision).Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("expected artifact_revisions row to remain preserved, count=%d, err=%v", count, err)
	}

	var jCount int
	err = store.readDB.QueryRowContext(ctx, "SELECT count(*) FROM journal_entries WHERE op_id = ?;", "op-pub-loss").Scan(&jCount)
	if err != nil || jCount != 1 {
		t.Fatalf("expected journal_entries row to remain preserved, count=%d, err=%v", jCount, err)
	}
}
