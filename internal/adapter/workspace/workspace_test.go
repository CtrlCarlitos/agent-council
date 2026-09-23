//go:build !windows

// POSIX enforcement evidence only: workspace fixtures use git (CRLF/autocrlf-dependent content assertions) and POSIX directory permission enforcement. Windows read-only enforcement requires an ACL model and is separate work.
package workspace_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
)

func TestWorkspace_SymmetricDisjointness(t *testing.T) {
	t.Run("rejects workspaceBaseDir inside stateDir", func(t *testing.T) {
		base := t.TempDir()
		stateDir := filepath.Join(base, "state")
		wsBaseDir := filepath.Join(stateDir, "workspaces")
		if err := os.MkdirAll(wsBaseDir, 0700); err != nil {
			t.Fatalf("failed to create test dirs: %v", err)
		}

		_, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
		if !errors.Is(err, workspace.ErrWorkspaceStateOverlap) {
			t.Fatalf("expected ErrWorkspaceStateOverlap, got: %v", err)
		}
	})

	t.Run("rejects stateDir inside workspaceBaseDir", func(t *testing.T) {
		base := t.TempDir()
		wsBaseDir := filepath.Join(base, "workspaces")
		stateDir := filepath.Join(wsBaseDir, "state")
		if err := os.MkdirAll(stateDir, 0700); err != nil {
			t.Fatalf("failed to create test dirs: %v", err)
		}

		_, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
		if !errors.Is(err, workspace.ErrWorkspaceStateOverlap) {
			t.Fatalf("expected ErrWorkspaceStateOverlap, got: %v", err)
		}
	})

	t.Run("rejects identical stateDir and workspaceBaseDir", func(t *testing.T) {
		base := t.TempDir()
		_, err := workspace.NewWorkspaceManager(base, base)
		if !errors.Is(err, workspace.ErrWorkspaceStateOverlap) {
			t.Fatalf("expected ErrWorkspaceStateOverlap, got: %v", err)
		}
	})

	t.Run("rejects overlap via symlink", func(t *testing.T) {
		base := t.TempDir()
		stateDir := filepath.Join(base, "state")
		wsRealDir := filepath.Join(stateDir, "nested-workspaces")
		if err := os.MkdirAll(wsRealDir, 0700); err != nil {
			t.Fatalf("failed to create dirs: %v", err)
		}
		symlinkWs := filepath.Join(base, "symlink-workspaces")
		if err := os.Symlink(wsRealDir, symlinkWs); err != nil {
			t.Fatalf("failed to create symlink: %v", err)
		}

		_, err := workspace.NewWorkspaceManager(stateDir, symlinkWs)
		if !errors.Is(err, workspace.ErrWorkspaceStateOverlap) {
			t.Fatalf("expected ErrWorkspaceStateOverlap via symlink, got: %v", err)
		}
	})

	t.Run("accepts disjoint directories", func(t *testing.T) {
		base := t.TempDir()
		stateDir := filepath.Join(base, "state")
		wsBaseDir := filepath.Join(base, "workspaces")

		mgr, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if mgr == nil {
			t.Fatal("expected non-nil WorkspaceManager")
		}
	})
}

func TestWorkspace_IdentifierValidation(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	wsBaseDir := filepath.Join(base, "workspaces")
	mgr, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
	if err != nil {
		t.Fatalf("failed to create WorkspaceManager: %v", err)
	}

	invalidIDs := []string{
		"../traversal",
		"nested/dir",
		"nested\\dir",
		"has space",
		"has$dollar",
		"has;semicolon",
		"has:colon",
		"",
		"has!excl",
		"has?question",
	}

	for _, badID := range invalidIDs {
		t.Run("invalid_run_id_"+badID, func(t *testing.T) {
			_, err := mgr.AllocateWorkspace(badID, "valid-session", "none", "", "")
			if !errors.Is(err, workspace.ErrInvalidIdentifier) {
				t.Fatalf("expected ErrInvalidIdentifier for runID %q, got: %v", badID, err)
			}
		})

		t.Run("invalid_session_id_"+badID, func(t *testing.T) {
			_, err := mgr.AllocateWorkspace("valid-run", badID, "none", "", "")
			if !errors.Is(err, workspace.ErrInvalidIdentifier) {
				t.Fatalf("expected ErrInvalidIdentifier for sessionID %q, got: %v", badID, err)
			}
		})
	}
}

func TestWorkspace_SymlinkEscape(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	wsBaseDir := filepath.Join(base, "workspaces")
	mgr, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
	if err != nil {
		t.Fatalf("failed to create WorkspaceManager: %v", err)
	}

	// Create an outside directory and symlink a run_id to it
	outsideDir := filepath.Join(base, "outside")
	if err := os.MkdirAll(outsideDir, 0700); err != nil {
		t.Fatalf("failed to create outside dir: %v", err)
	}
	symlinkRun := filepath.Join(wsBaseDir, "escaped-run")
	if err := os.MkdirAll(wsBaseDir, 0700); err != nil {
		t.Fatalf("failed to create wsBaseDir: %v", err)
	}
	if err := os.Symlink(outsideDir, symlinkRun); err != nil {
		t.Fatalf("failed to create run symlink: %v", err)
	}

	_, err = mgr.AllocateWorkspace("escaped-run", "session-1", "none", "", "")
	if !errors.Is(err, workspace.ErrWorkspaceEscapesBase) {
		t.Fatalf("expected ErrWorkspaceEscapesBase when run directory symlinks outside, got: %v", err)
	}
}

func createTestGitRepo(t *testing.T) (string, string) {
	t.Helper()
	repoDir := t.TempDir()

	runCmd := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git command %v failed: %v, output: %s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	runCmd("init", "-b", "main")
	runCmd("config", "user.name", "Council Test")
	runCmd("config", "user.email", "council@test.internal")

	testFile := filepath.Join(repoDir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("hello world\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	runCmd("add", "hello.txt")
	runCmd("commit", "-m", "initial commit")

	headCommit := runCmd("rev-parse", "HEAD")
	return repoDir, headCommit
}

func TestWorkspace_ModeNone(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	wsBaseDir := filepath.Join(base, "workspaces")
	mgr, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
	if err != nil {
		t.Fatalf("failed to create WorkspaceManager: %v", err)
	}

	paths, err := mgr.AllocateWorkspace("run-1", "session-none", "none", "", "")
	if err != nil {
		t.Fatalf("AllocateWorkspace failed: %v", err)
	}

	if paths.Mode != "none" {
		t.Errorf("expected mode 'none', got %q", paths.Mode)
	}
	if paths.Root != paths.Scratch {
		t.Errorf("expected Root == Scratch, got Root=%q, Scratch=%q", paths.Root, paths.Scratch)
	}
	if paths.Source != "" {
		t.Errorf("expected empty Source in none mode, got %q", paths.Source)
	}
	if paths.Worktree != "" {
		t.Errorf("expected empty Worktree in none mode, got %q", paths.Worktree)
	}

	// Verify scratch and config exist
	for _, dir := range []string{paths.Scratch, paths.Config} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("expected directory %s to exist: %v", dir, err)
		}
		if !fi.IsDir() {
			t.Fatalf("expected %s to be directory", dir)
		}
	}

	// Verify CloseWorkspace cleans up
	if err := mgr.CloseWorkspace("run-1", "session-none"); err != nil {
		t.Fatalf("CloseWorkspace failed: %v", err)
	}
	sessionDir := filepath.Dir(paths.Scratch)
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("expected session directory %s to be removed, got err: %v", sessionDir, err)
	}
}

func TestWorkspace_ModeReadonly(t *testing.T) {
	repoDir, headCommit := createTestGitRepo(t)

	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	wsBaseDir := filepath.Join(base, "workspaces")
	mgr, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
	if err != nil {
		t.Fatalf("failed to create WorkspaceManager: %v", err)
	}

	paths, err := mgr.AllocateWorkspace("run-1", "session-ro", "readonly", repoDir, headCommit)
	if err != nil {
		t.Fatalf("AllocateWorkspace failed: %v", err)
	}

	if paths.Mode != "readonly" {
		t.Errorf("expected mode 'readonly', got %q", paths.Mode)
	}
	if paths.Root != paths.Source {
		t.Errorf("expected Root == Source, got Root=%q, Source=%q", paths.Root, paths.Source)
	}
	if paths.Source == "" {
		t.Fatal("expected non-empty Source")
	}

	// Verify source content exists
	content, err := os.ReadFile(filepath.Join(paths.Source, "hello.txt"))
	if err != nil {
		t.Fatalf("failed to read hello.txt from source: %v", err)
	}
	if string(content) != "hello world\n" {
		t.Errorf("unexpected content: %q", string(content))
	}

	// Verify read-only enforcement: attempting to write to source must fail
	err = os.WriteFile(filepath.Join(paths.Source, "new_file.txt"), []byte("fail"), 0644)
	if err == nil {
		t.Fatal("expected write to read-only source directory to fail, but succeeded")
	}

	// Verify CloseWorkspace cleans up cleanly
	if err := mgr.CloseWorkspace("run-1", "session-ro"); err != nil {
		t.Fatalf("CloseWorkspace failed: %v", err)
	}
	sessionDir := filepath.Dir(paths.Source)
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("expected session directory %s to be removed, got err: %v", sessionDir, err)
	}
}

func TestWorkspace_ModeIsolatedBranch(t *testing.T) {
	repoDir, headCommit := createTestGitRepo(t)

	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	wsBaseDir := filepath.Join(base, "workspaces")
	mgr, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
	if err != nil {
		t.Fatalf("failed to create WorkspaceManager: %v", err)
	}

	paths, err := mgr.AllocateWorkspace("run-1", "session-wt", "isolated_branch", repoDir, headCommit)
	if err != nil {
		t.Fatalf("AllocateWorkspace failed: %v", err)
	}

	if paths.Mode != "isolated_branch" {
		t.Errorf("expected mode 'isolated_branch', got %q", paths.Mode)
	}
	if paths.Root != paths.Worktree {
		t.Errorf("expected Root == Worktree, got Root=%q, Worktree=%q", paths.Root, paths.Worktree)
	}
	if paths.Worktree == "" {
		t.Fatal("expected non-empty Worktree")
	}

	// Verify git branch in worktree is council/run-1/session-wt
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = paths.Worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to get worktree branch: %v, output: %s", err, string(out))
	}
	branch := strings.TrimSpace(string(out))
	expectedBranch := "council/run-1/session-wt"
	if branch != expectedBranch {
		t.Errorf("expected branch %q, got %q", expectedBranch, branch)
	}

	// Verify we can commit in worktree
	newFile := filepath.Join(paths.Worktree, "worker_work.txt")
	if err := os.WriteFile(newFile, []byte("work done\n"), 0644); err != nil {
		t.Fatalf("failed to write file in worktree: %v", err)
	}
	cmd = exec.Command("git", "add", "worker_work.txt")
	cmd.Dir = paths.Worktree
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add in worktree failed: %v, output: %s", err, string(out))
	}
	cmd = exec.Command("git", "commit", "-m", "worker commit")
	cmd.Dir = paths.Worktree
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit in worktree failed: %v, output: %s", err, string(out))
	}

	// CloseWorkspace must clean up worktree and directory tree
	if err := mgr.CloseWorkspace("run-1", "session-wt"); err != nil {
		t.Fatalf("CloseWorkspace failed: %v", err)
	}

	sessionDir := filepath.Dir(paths.Worktree)
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("expected session directory %s to be removed, got err: %v", sessionDir, err)
	}

	// Verify git worktree list in source repo no longer lists the worktree
	cmd = exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repoDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list failed: %v, output: %s", err, string(out))
	}
	if strings.Contains(string(out), paths.Worktree) {
		t.Errorf("expected worktree %s to be removed from git worktree list, but found:\n%s", paths.Worktree, string(out))
	}
}

func TestWorkspace_UnsupportedMode(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	wsBaseDir := filepath.Join(base, "workspaces")
	mgr, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
	if err != nil {
		t.Fatalf("failed to create WorkspaceManager: %v", err)
	}

	_, err = mgr.AllocateWorkspace("run-1", "session-1", "invalid_mode", "", "")
	if !errors.Is(err, workspace.ErrUnsupportedWorkspaceMode) {
		t.Fatalf("expected ErrUnsupportedWorkspaceMode, got: %v", err)
	}
}

func TestWorkspace_MissingSourceRepo(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	wsBaseDir := filepath.Join(base, "workspaces")
	mgr, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
	if err != nil {
		t.Fatalf("failed to create WorkspaceManager: %v", err)
	}

	for _, mode := range []string{"readonly", "isolated_branch"} {
		t.Run("empty_repo_"+mode, func(t *testing.T) {
			_, err := mgr.AllocateWorkspace("run-1", "session-1", mode, "", "HEAD")
			if !errors.Is(err, workspace.ErrInvalidSourceRepo) {
				t.Fatalf("expected ErrInvalidSourceRepo, got: %v", err)
			}
		})

		t.Run("nonexistent_repo_"+mode, func(t *testing.T) {
			_, err := mgr.AllocateWorkspace("run-1", "session-1", mode, filepath.Join(base, "nonexistent"), "HEAD")
			if !errors.Is(err, workspace.ErrInvalidSourceRepo) {
				t.Fatalf("expected ErrInvalidSourceRepo, got: %v", err)
			}
		})
	}
}

func TestWorkspace_SiblingIsolation(t *testing.T) {
	repoDir, headCommit := createTestGitRepo(t)

	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	wsBaseDir := filepath.Join(base, "workspaces")
	mgr, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
	if err != nil {
		t.Fatalf("failed to create WorkspaceManager: %v", err)
	}

	paths1, err := mgr.AllocateWorkspace("run-parallel", "session-worker-1", "isolated_branch", repoDir, headCommit)
	if err != nil {
		t.Fatalf("worker 1 allocation failed: %v", err)
	}
	defer mgr.CloseWorkspace("run-parallel", "session-worker-1")

	paths2, err := mgr.AllocateWorkspace("run-parallel", "session-worker-2", "isolated_branch", repoDir, headCommit)
	if err != nil {
		t.Fatalf("worker 2 allocation failed: %v", err)
	}
	defer mgr.CloseWorkspace("run-parallel", "session-worker-2")

	if paths1.Root == paths2.Root {
		t.Errorf("expected distinct roots, got %q and %q", paths1.Root, paths2.Root)
	}
	if paths1.Worktree == paths2.Worktree {
		t.Errorf("expected distinct worktrees, got %q and %q", paths1.Worktree, paths2.Worktree)
	}

	// Verify both worktrees operate on distinct branches
	cmd1 := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd1.Dir = paths1.Worktree
	out1, _ := cmd1.CombinedOutput()
	b1 := strings.TrimSpace(string(out1))

	cmd2 := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd2.Dir = paths2.Worktree
	out2, _ := cmd2.CombinedOutput()
	b2 := strings.TrimSpace(string(out2))

	if b1 != "council/run-parallel/session-worker-1" {
		t.Errorf("unexpected branch for worker 1: %q", b1)
	}
	if b2 != "council/run-parallel/session-worker-2" {
		t.Errorf("unexpected branch for worker 2: %q", b2)
	}
}

// Under a symlinked workspace base (the macOS /var -> /private/var
// shape), every mode must return the child-visible PHYSICAL root: the
// native child's getwd resolves symlinked ancestors, and the frozen
// workspace root must agree with it (AC-008 pins this root as cwd).
func TestWorkspace_PhysicalRootUnderSymlinkedBase(t *testing.T) {
	repoDir, headCommit := createTestGitRepo(t)

	dir := t.TempDir()
	realBase := filepath.Join(dir, "real-base")
	if err := os.MkdirAll(realBase, 0o700); err != nil {
		t.Fatalf("mkdir real base: %v", err)
	}
	linkBase := filepath.Join(dir, "link-base")
	if err := os.Symlink(realBase, linkBase); err != nil {
		t.Fatalf("symlink base: %v", err)
	}
	// The reference base must be physical too: on darwin t.TempDir()
	// itself lives behind /var -> /private/var.
	realBase, err = filepath.EvalSymlinks(realBase)
	if err != nil {
		t.Fatalf("resolve real base: %v", err)
	}
	stateDir := filepath.Join(dir, "state")

	for _, mode := range []string{"none", "readonly", "isolated_branch"} {
		t.Run(mode, func(t *testing.T) {
			mgr, err := workspace.NewWorkspaceManager(stateDir, linkBase)
			if err != nil {
				t.Fatalf("workspace manager: %v", err)
			}
			paths, err := mgr.AllocateWorkspace("run-sym", "sess-"+mode, mode, repoDir, headCommit)
			if err != nil {
				t.Fatalf("allocate %s: %v", mode, err)
			}
			// CloseWorkspace restores permissions so TempDir cleanup can
			// remove the read-only source.
			defer func() { _ = mgr.CloseWorkspace("run-sym", "sess-"+mode) }()
			// The returned root must already BE the physical path: a
			// second EvalSymlinks must be the identity.
			resolved, err := filepath.EvalSymlinks(paths.Root)
			if err != nil {
				t.Fatalf("resolve root: %v", err)
			}
			if resolved != paths.Root {
				t.Fatalf("%s root %s is lexical; the child-visible physical path is %s", mode, paths.Root, resolved)
			}
			// Physical containment under the resolved base.
			if !strings.HasPrefix(resolved, realBase+string(filepath.Separator)) {
				t.Fatalf("%s root %s must live under the physical base %s", mode, resolved, realBase)
			}
		})
	}
}
