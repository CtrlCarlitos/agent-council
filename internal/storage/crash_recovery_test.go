package storage_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_CrashRecovery_MultiBoundaryMatrix(t *testing.T) {
	if os.Getenv("GO_TEST_SUBPROCESS") == "1" {
		runCrashSubprocess()
		return
	}

	boundaries := []string{
		"pre_commit_release",
		"post_intent_unacknowledged",
		"accepted_unrecorded_ack",
		"uncommitted_artifact_metadata",
	}

	for _, boundary := range boundaries {
		t.Run(boundary, func(t *testing.T) {
			tempDir := t.TempDir()

			// Initialize valid run and session records
			initStore, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
			if err != nil {
				t.Fatalf("init store: %v", err)
			}
			ctx := context.Background()
			_, err = initStore.CreateRun(ctx, "op-run-init", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
			adoptControllerForTest(t, initStore, "run-1", "lease-1")
			if err != nil {
				t.Fatalf("init create run: %v", err)
			}
			_, err = initStore.CreateSession(ctx, "op-sess-init", "lease-1", storage.SessionRecord{
				ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
			})
			if err != nil {
				t.Fatalf("init create session: %v", err)
			}
			_, err = initStore.QueuePrompt(ctx, "op-q-init", "lease-1", "sess-1", 1, storage.PendingPrompt{
				SessionID: "sess-1", TurnKey: "turn-init", Prompt: "Initial Prompt", CreatedAt: time.Now(),
			})
			if err != nil {
				t.Fatalf("init queue prompt: %v", err)
			}
			initStore.Close()

			// Run helper subprocess targeted to crash at boundary
			subCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			cmd := exec.CommandContext(subCtx, os.Args[0], "-test.run=^TestStore_CrashRecovery_MultiBoundaryMatrix$")
			cmd.Env = append(os.Environ(),
				"GO_TEST_SUBPROCESS=1",
				"SUBPROCESS_CRASH_BOUNDARY="+boundary,
				"SUBPROCESS_STATE_DIR="+tempDir,
			)

			stdoutPipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatalf("stdout pipe: %v", err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr

			if err := cmd.Start(); err != nil {
				t.Fatalf("start subprocess: %v", err)
			}

			// Deterministic checkpoint handshake via stdout scanner
			scanner := bufio.NewScanner(stdoutPipe)
			expectedCheckpoint := "CHECKPOINT:" + boundary
			var checkpointFound bool
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "ERROR:") {
					t.Fatalf("subprocess initialization error: %s", line)
				}
				if line == expectedCheckpoint {
					checkpointFound = true
					break
				}
			}

			if !checkpointFound {
				t.Fatalf("checkpoint %q not reached before subprocess exit; stderr: %s", expectedCheckpoint, stderr.String())
			}

			// Wait for deliberate crash exit and assert exit code 42
			waitErr := cmd.Wait()
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				if exitErr.ExitCode() != 42 {
					t.Fatalf("expected subprocess to exit with code 42, got %d", exitErr.ExitCode())
				}
			} else {
				t.Fatalf("expected ExitError with code 42, got %v", waitErr)
			}

			// Parent reopens store and asserts durable state invariant
			store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
			if err != nil {
				t.Fatalf("parent reopen: %v", err)
			}
			defer store.Close()

			hydrated, err := store.HydrateState(context.Background())
			if err != nil {
				t.Fatalf("hydrate after crash %s: %v", boundary, err)
			}

			sess := hydrated.Sessions["sess-1"]
			switch boundary {
			case "pre_commit_release":
				// Zero partial release: session still parked, prompt still queued, no turn or intent
				if sess.State != "parked" {
					t.Fatalf("pre-commit crash left partial release: state=%s", sess.State)
				}
				if len(sess.PendingPrompts) != 1 || sess.PendingPrompts["turn-init"].Prompt != "Initial Prompt" {
					t.Fatalf("pre-commit crash modified pending prompt: %+v", sess.PendingPrompts)
				}
				if sess.ActiveTurn != nil || len(sess.Turns) != 0 {
					t.Fatalf("pre-commit crash committed turn record: %+v", sess.Turns)
				}

			case "post_intent_unacknowledged":
				// Turn reserved and uncertain: phase == intent_recorded
				if sess.State != "running" || sess.ActiveIntent == nil || sess.ActiveIntent.Phase != "intent_recorded" {
					t.Fatalf("post-intent crash did not preserve uncertain reservation: %+v", sess)
				}
				if sess.ActiveTurn == nil || sess.ActiveTurn.Status != "running" {
					t.Fatalf("post-intent crash did not preserve running turn: %+v", sess.ActiveTurn)
				}

			case "accepted_unrecorded_ack":
				// External harness acceptance evidence verified independently
				evidenceFile := filepath.Join(tempDir, "external_harness_acceptance.json")
				data, err := os.ReadFile(evidenceFile)
				if err != nil {
					t.Fatalf("expected independent harness acceptance file: %v", err)
				}
				var ack map[string]string
				if err := json.Unmarshal(data, &ack); err != nil || ack["status"] != "accepted" {
					t.Fatalf("invalid harness acceptance evidence: %s", string(data))
				}

				// Uncertain reservation preserved in DB; ReleaseTurn on same session remains blocked
				_, err = store.ReleaseTurn(context.Background(), "op-illegal-rel", "lease-1", "sess-1", sess.RowVersion, "turn-2")
				if err == nil {
					t.Fatalf("expected ReleaseTurn to remain blocked on uncertain turn, but it succeeded")
				}

			case "uncommitted_artifact_metadata":
				// Blob on disk has exact bytes and sha256
				content := []byte("crash-recovery-artifact-body")
				sum := sha256.Sum256(content)
				digest := hex.EncodeToString(sum[:])
				blobPath := filepath.Join(tempDir, "artifacts", digest[:2], digest)
				diskBytes, err := os.ReadFile(blobPath)
				if err != nil {
					t.Fatalf("expected artifact blob on disk: %v", err)
				}
				if !bytes.Equal(diskBytes, content) {
					t.Fatalf("artifact blob mismatch: got %q, want %q", diskBytes, content)
				}

				// Zero logical artifact revisions hydrated in database
				var count int
				_ = store.DB().QueryRow("SELECT count(*) FROM artifact_revisions;").Scan(&count)
				if count != 0 {
					t.Fatalf("expected 0 logical artifacts committed, got %d", count)
				}

				// ReadArtifactRevision returns ErrArtifactNotFound
				_, _, err = store.ReadArtifactRevision(context.Background(), "art-1", 1)
				if err != storage.ErrArtifactNotFound {
					t.Fatalf("expected ErrArtifactNotFound, got %v", err)
				}
			}
		})
	}
}

func runCrashSubprocess() {
	boundary := os.Getenv("SUBPROCESS_CRASH_BOUNDARY")
	stateDir := os.Getenv("SUBPROCESS_STATE_DIR")

	hook := func(b string) {
		if b == boundary {
			fmt.Fprintln(os.Stdout, "CHECKPOINT:"+boundary)
			_ = os.Stdout.Sync()
			os.Exit(42)
		}
	}

	store, err := storage.Open(storage.StoreOptions{
		StateDir:             stateDir,
		TestHookBeforeCommit: hook,
	})
	if err != nil {
		fmt.Fprintf(os.Stdout, "ERROR:open store: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	switch boundary {
	case "pre_commit_release":
		// Invoke production ReleaseTurn, which invokes TestHookBeforeCommit("pre_commit_release") before tx.Commit()
		_, err := store.ReleaseTurn(ctx, "op-crash-rel", "lease-1", "sess-1", 2, "turn-init")
		if err != nil {
			fmt.Fprintf(os.Stdout, "ERROR:release turn: %v\n", err)
			os.Exit(1)
		}
		os.Exit(1)

	case "post_intent_unacknowledged":
		_, err := store.ReleaseTurn(ctx, "op-crash-rel", "lease-1", "sess-1", 2, "turn-init")
		if err != nil {
			fmt.Fprintf(os.Stdout, "ERROR:release turn: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "CHECKPOINT:post_intent_unacknowledged")
		_ = os.Stdout.Sync()
		os.Exit(42)

	case "accepted_unrecorded_ack":
		_, err := store.ReleaseTurn(ctx, "op-crash-rel2", "lease-1", "sess-1", 2, "turn-init")
		if err != nil {
			fmt.Fprintf(os.Stdout, "ERROR:release turn: %v\n", err)
			os.Exit(1)
		}

		// Dispatch via real FakeAdapter
		fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
		_, err = fake.CreateSession(ctx, adapter.CreateSessionRequest{
			SessionID:   adapter.SessionID("sess-1"),
			Contributor: council.Claude,
		})
		if err != nil {
			fmt.Fprintf(os.Stdout, "ERROR:fake create session: %v\n", err)
			os.Exit(1)
		}

		outcome, err := fake.Dispatch(ctx, adapter.TurnRef{
			SessionID: "sess-1",
			TurnKey:   "turn-init",
		}, "Initial Prompt")
		if err != nil || outcome.Status != adapter.DispatchAccepted {
			fmt.Fprintf(os.Stdout, "ERROR:fake dispatch: %v, status=%s\n", err, outcome.Status)
			os.Exit(1)
		}

		// External harness accepted: record independent receipt on disk
		evidence := map[string]string{
			"status":   string(outcome.Status),
			"turn_key": "turn-init",
			"harness":  "claude-fake",
		}
		evidenceBytes, _ := json.Marshal(evidence)
		_ = os.WriteFile(filepath.Join(stateDir, "external_harness_acceptance.json"), evidenceBytes, 0600)

		fmt.Fprintln(os.Stdout, "CHECKPOINT:accepted_unrecorded_ack")
		_ = os.Stdout.Sync()
		os.Exit(42)

	case "uncommitted_artifact_metadata":
		// Call production PublishArtifact, which invokes TestHookBeforeCommit("uncommitted_artifact_metadata") before tx.Commit()
		content := []byte("crash-recovery-artifact-body")
		meta := storage.ArtifactMetadata{
			ID:        "art-1",
			RunID:     "run-1",
			SessionID: "sess-1",
			TurnKey:   "turn-init",
			Name:      "patch.diff",
		}
		_, err = store.PublishArtifact(ctx, "op-art-crash", "lease-1", meta, content)
		if err != nil {
			fmt.Fprintf(os.Stdout, "ERROR:publish artifact: %v\n", err)
			os.Exit(1)
		}
		os.Exit(1)
	}
}
