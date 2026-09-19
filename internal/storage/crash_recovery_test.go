package storage_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
			_, err = initStore.CreateRun(ctx, "op-run-init", "run-1", "lease-1")
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
				SessionID: "sess-1", Prompt: "Initial Prompt", CreatedAt: time.Now(),
			})
			if err != nil {
				t.Fatalf("init queue prompt: %v", err)
			}
			initStore.Close()

			// Run helper subprocess targeted to crash at boundary
			subCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			cmd := exec.CommandContext(subCtx, os.Args[0], "-test.run=^TestStore_CrashRecovery_MultiBoundaryMatrix$")
			cmd.Env = append(os.Environ(),
				"GO_TEST_SUBPROCESS=1",
				"SUBPROCESS_CRASH_BOUNDARY="+boundary,
				"SUBPROCESS_STATE_DIR="+tempDir,
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			_ = cmd.Run() // Subprocess deliberately crashes/exits with code 1

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
				if sess.State != "parked" || sess.PendingPrompt == nil || sess.ActiveTurn != nil {
					t.Fatalf("pre-commit crash left partial release: state=%s, prompt=%v, turn=%v", sess.State, sess.PendingPrompt, sess.ActiveTurn)
				}
			case "post_intent_unacknowledged":
				// Turn reserved and uncertain: phase == intent_recorded
				if sess.State != "running" || sess.ActiveIntent == nil || sess.ActiveIntent.Phase != "intent_recorded" {
					t.Fatalf("post-intent crash did not preserve uncertain reservation: %+v", sess)
				}
			case "accepted_unrecorded_ack":
				// Uncertain reservation preserved; ReleaseTurn on same session remains blocked
				_, err := store.ReleaseTurn(context.Background(), "op-illegal-rel", "lease-1", "sess-1", sess.RowVersion, "turn-2")
				if err == nil {
					t.Fatalf("expected ReleaseTurn to remain blocked on uncertain turn, but it succeeded")
				}
			case "uncommitted_artifact_metadata":
				// Zero logical artifacts hydrated in database
				var count int
				_ = store.DB().QueryRow("SELECT count(*) FROM artifact_revisions;").Scan(&count)
				if count != 0 {
					t.Fatalf("expected 0 logical artifacts committed, got %d", count)
				}
			}
		})
	}
}

func runCrashSubprocess() {
	boundary := os.Getenv("SUBPROCESS_CRASH_BOUNDARY")
	stateDir := os.Getenv("SUBPROCESS_STATE_DIR")
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		os.Exit(1)
	}

	ctx := context.Background()
	switch boundary {
	case "pre_commit_release":
		// Begin write tx, modify tables, crash before commit
		tx, _ := store.BeginWrite(ctx)
		_, _ = tx.Tx().Exec("UPDATE sessions SET state = 'running' WHERE session_id = 'sess-1';")
		// Abrupt exit before tx.Commit()
		os.Exit(1)
	case "post_intent_unacknowledged":
		_, _ = store.ReleaseTurn(ctx, "op-crash-rel", "lease-1", "sess-1", 2, "turn-crash-1")
		os.Exit(1)
	case "accepted_unrecorded_ack":
		_, _ = store.ReleaseTurn(ctx, "op-crash-rel2", "lease-1", "sess-1", 2, "turn-crash-2")
		// External harness accepted, but before RecordDispatchObservation committed, crash
		os.Exit(1)
	case "uncommitted_artifact_metadata":
		// Install blob into artifacts/ dir, but crash before database transaction
		blobDir := filepath.Join(stateDir, "artifacts", "ab")
		_ = os.MkdirAll(blobDir, 0700)
		_ = os.WriteFile(filepath.Join(blobDir, "abcdef123456"), []byte("data"), 0600)
		os.Exit(1)
	}
}
