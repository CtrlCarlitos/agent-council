package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var (
	// ErrWorkspaceStateOverlap is returned when workspace_base_dir and state_dir overlap.
	ErrWorkspaceStateOverlap = errors.New("workspace_base_dir and state_dir must be symmetrically disjoint")
	// ErrInvalidIdentifier is returned when a run_id or session_id contains invalid characters.
	ErrInvalidIdentifier = errors.New("invalid identifier: must match ^[a-zA-Z0-9_\\-]+$")
	// ErrWorkspaceEscapesBase is returned when a workspace path resolves outside workspace_base_dir.
	ErrWorkspaceEscapesBase = errors.New("workspace path escapes workspace base directory")
	// ErrUnsupportedWorkspaceMode is returned when an unrecognized mode is specified.
	ErrUnsupportedWorkspaceMode = errors.New("unsupported workspace mode")
	// ErrInvalidSourceRepo is returned when sourceRepo is required but invalid or missing.
	ErrInvalidSourceRepo = errors.New("valid source repository is required for this workspace mode")
	// ErrSessionNotFound is returned when attempting to operate on an unknown session.
	ErrSessionNotFound = errors.New("workspace session not found")
)

var idRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

// ValidateIdentifier validates that an identifier contains only safe characters.
func ValidateIdentifier(id string) error {
	if !idRegex.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidIdentifier, id)
	}
	return nil
}

// WorkspacePaths defines the directories provisioned for a worker session.
type WorkspacePaths struct {
	Root     string
	Scratch  string
	Config   string
	Source   string
	Worktree string
	Mode     string
}

// WorkspaceManager manages isolated filesystem allocations for council workers.
type WorkspaceManager struct {
	stateDir         string
	workspaceBaseDir string
	realWorkspaceDir string
	mu               sync.Mutex
	sessions         map[string]*allocatedSession
}

type allocatedSession struct {
	runID      string
	sessionID  string
	mode       string
	sourceRepo string
	branch     string
	paths      WorkspacePaths
}

func sessionKey(runID, sessionID string) string {
	return runID + "/" + sessionID
}

// NewWorkspaceManager creates and validates a WorkspaceManager ensuring symmetric disjointness
// between stateDir and workspaceBaseDir.
func NewWorkspaceManager(stateDir, workspaceBaseDir string) (*WorkspaceManager, error) {
	if strings.TrimSpace(stateDir) == "" || strings.TrimSpace(workspaceBaseDir) == "" {
		return nil, errors.New("state_dir and workspace_base_dir must not be empty")
	}

	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create state_dir: %w", err)
	}
	if err := os.MkdirAll(workspaceBaseDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create workspace_base_dir: %w", err)
	}

	realStateDir, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve state_dir symlinks: %w", err)
	}
	realWorkspaceBase, err := filepath.EvalSymlinks(workspaceBaseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve workspace_base_dir symlinks: %w", err)
	}

	realStateDir = filepath.Clean(realStateDir)
	realWorkspaceBase = filepath.Clean(realWorkspaceBase)

	sep := string(filepath.Separator)
	if realWorkspaceBase == realStateDir ||
		strings.HasPrefix(realWorkspaceBase, realStateDir+sep) ||
		strings.HasPrefix(realStateDir, realWorkspaceBase+sep) {
		return nil, ErrWorkspaceStateOverlap
	}

	return &WorkspaceManager{
		stateDir:         realStateDir,
		workspaceBaseDir: workspaceBaseDir,
		realWorkspaceDir: realWorkspaceBase,
		sessions:         make(map[string]*allocatedSession),
	}, nil
}

// AllocateWorkspace allocates session directories under $WORKSPACE_BASE_DIR/{run_id}/{session_id}.
func (m *WorkspaceManager) AllocateWorkspace(runID, sessionID, mode, sourceRepo, commit string) (paths WorkspacePaths, err error) {
	if err := ValidateIdentifier(runID); err != nil {
		return WorkspacePaths{}, err
	}
	if err := ValidateIdentifier(sessionID); err != nil {
		return WorkspacePaths{}, err
	}

	key := sessionKey(runID, sessionID)
	m.mu.Lock()
	if _, exists := m.sessions[key]; exists {
		m.mu.Unlock()
		return WorkspacePaths{}, fmt.Errorf("session %s/%s is already allocated", runID, sessionID)
	}
	m.sessions[key] = &allocatedSession{} // placeholder
	m.mu.Unlock()

	defer func() {
		if err != nil {
			m.mu.Lock()
			delete(m.sessions, key)
			m.mu.Unlock()
		}
	}()

	// Validate mode early
	switch mode {
	case "none", "readonly", "isolated_branch":
	default:
		return WorkspacePaths{}, fmt.Errorf("%w: %q", ErrUnsupportedWorkspaceMode, mode)
	}

	// Validate nearest ancestor symlinks before creation
	runDir := filepath.Join(m.workspaceBaseDir, runID)
	sessionDir := filepath.Join(runDir, sessionID)

	ancestor := nearestExistingAncestor(sessionDir)
	realAncestor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return WorkspacePaths{}, fmt.Errorf("failed resolving ancestor symlinks: %w", err)
	}
	realAncestor = filepath.Clean(realAncestor)
	if realAncestor != m.realWorkspaceDir && !strings.HasPrefix(realAncestor, m.realWorkspaceDir+string(filepath.Separator)) {
		return WorkspacePaths{}, ErrWorkspaceEscapesBase
	}

	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return WorkspacePaths{}, fmt.Errorf("failed creating session directory: %w", err)
	}

	realSessionDir, err := filepath.EvalSymlinks(sessionDir)
	if err != nil {
		return WorkspacePaths{}, fmt.Errorf("failed resolving session directory symlinks: %w", err)
	}
	realSessionDir = filepath.Clean(realSessionDir)
	if !isStrictSubdirectory(m.realWorkspaceDir, realSessionDir) {
		return WorkspacePaths{}, ErrWorkspaceEscapesBase
	}

	// Always provision scratch/ and config/ with mode 0700. They are
	// built on the RESOLVED session directory: the native child's getwd
	// resolves symlinked ancestors (e.g. macOS /var -> /private/var),
	// and every correlation (init cwd, transcript derivation) must use
	// the physical path the child experiences.
	scratchDir := filepath.Join(realSessionDir, "scratch")
	if err := os.MkdirAll(scratchDir, 0700); err != nil {
		return WorkspacePaths{}, fmt.Errorf("failed creating scratch directory: %w", err)
	}
	configDir := filepath.Join(realSessionDir, "config")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return WorkspacePaths{}, fmt.Errorf("failed creating config directory: %w", err)
	}

	var branch string

	switch mode {
	case "none":
		paths = WorkspacePaths{
			Root:    scratchDir,
			Scratch: scratchDir,
			Config:  configDir,
			Mode:    "none",
		}

	case "readonly":
		if strings.TrimSpace(sourceRepo) == "" {
			return WorkspacePaths{}, ErrInvalidSourceRepo
		}
		if _, err := os.Stat(sourceRepo); err != nil {
			return WorkspacePaths{}, fmt.Errorf("%w: %v", ErrInvalidSourceRepo, err)
		}

		// Built on the RESOLVED session directory: the native child's
		// getwd resolves symlinked ancestors, so every mode must pin the
		// physical path (same contract as the none-mode scratch/config).
		sourceDir := filepath.Join(realSessionDir, "source")
		targetCommit := commit
		if targetCommit == "" {
			targetCommit = "HEAD"
		}

		if isGitRepo(sourceRepo) {
			cmd := exec.Command("git", "clone", "--no-hardlinks", sourceRepo, sourceDir)
			if out, err := cmd.CombinedOutput(); err != nil {
				return WorkspacePaths{}, fmt.Errorf("git clone failed for readonly mode: %w (output: %s)", err, string(out))
			}
			cmd = exec.Command("git", "-C", sourceDir, "checkout", "--detach", targetCommit)
			if out, err := cmd.CombinedOutput(); err != nil {
				return WorkspacePaths{}, fmt.Errorf("git checkout --detach failed for readonly mode: %w (output: %s)", err, string(out))
			}
		} else {
			if err := copyDir(sourceRepo, sourceDir); err != nil {
				return WorkspacePaths{}, fmt.Errorf("failed copying source for readonly mode: %w", err)
			}
		}

		// Enforce read-only permissions on sourceDir
		if err := makeReadOnly(sourceDir); err != nil {
			return WorkspacePaths{}, fmt.Errorf("failed setting read-only permissions on source: %w", err)
		}

		paths = WorkspacePaths{
			Root:    sourceDir,
			Scratch: scratchDir,
			Config:  configDir,
			Source:  sourceDir,
			Mode:    "readonly",
		}

	case "isolated_branch":
		if strings.TrimSpace(sourceRepo) == "" {
			return WorkspacePaths{}, ErrInvalidSourceRepo
		}
		if _, err := os.Stat(sourceRepo); err != nil {
			return WorkspacePaths{}, fmt.Errorf("%w: %v", ErrInvalidSourceRepo, err)
		}

		branch = fmt.Sprintf("council/%s/%s", runID, sessionID)
		worktreeDir := filepath.Join(realSessionDir, "worktree")

		targetCommit := commit
		if targetCommit == "" {
			targetCommit = "HEAD"
		}

		cmd := exec.Command("git", "-C", sourceRepo, "worktree", "add", "-b", branch, worktreeDir, targetCommit)
		if out, err := cmd.CombinedOutput(); err != nil {
			return WorkspacePaths{}, fmt.Errorf("git worktree add failed: %w (output: %s)", err, string(out))
		}

		paths = WorkspacePaths{
			Root:     worktreeDir,
			Scratch:  scratchDir,
			Config:   configDir,
			Worktree: worktreeDir,
			Mode:     "isolated_branch",
		}
	}

	m.mu.Lock()
	m.sessions[sessionKey(runID, sessionID)] = &allocatedSession{
		runID:      runID,
		sessionID:  sessionID,
		mode:       mode,
		sourceRepo: sourceRepo,
		branch:     branch,
		paths:      paths,
	}
	m.mu.Unlock()

	return paths, nil
}

// GetPaths returns the allocated workspace paths for an active session, if allocated.
func (m *WorkspaceManager) GetPaths(runID, sessionID string) (WorkspacePaths, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionKey(runID, sessionID)]
	if !ok {
		return WorkspacePaths{}, false
	}
	return sess.paths, true
}

// CloseWorkspace closes and cleans up a session workspace.
func (m *WorkspaceManager) CloseWorkspace(runID, sessionID string) error {
	if err := ValidateIdentifier(runID); err != nil {
		return err
	}
	if err := ValidateIdentifier(sessionID); err != nil {
		return err
	}

	key := sessionKey(runID, sessionID)
	m.mu.Lock()
	sess := m.sessions[key]
	m.mu.Unlock()

	sessionDir := filepath.Join(m.workspaceBaseDir, runID, sessionID)
	runDir := filepath.Join(m.workspaceBaseDir, runID)

	worktreeDir := filepath.Join(sessionDir, "worktree")
	if _, err := os.Stat(worktreeDir); err == nil {
		repo := ""
		branch := ""
		if sess != nil {
			repo = sess.sourceRepo
			branch = sess.branch
		}
		if repo == "" {
			repo = findGitWorktreeRepo(worktreeDir)
		}
		if repo != "" {
			_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", worktreeDir).Run()
			_ = exec.Command("git", "-C", repo, "worktree", "prune").Run()
			if branch != "" {
				_ = exec.Command("git", "-C", repo, "branch", "-D", branch).Run()
			}
		}
	}

	// Make entire session tree writable before removing to handle readonly source trees
	if _, err := os.Stat(sessionDir); err == nil {
		makeWritable(sessionDir)
		if err := os.RemoveAll(sessionDir); err != nil {
			return fmt.Errorf("failed to remove session directory %s: %w", sessionDir, err)
		}
	}

	// Best-effort cleanup of empty runDir
	_ = os.Remove(runDir)

	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()

	return nil
}

func nearestExistingAncestor(path string) string {
	curr := filepath.Clean(path)
	for {
		if _, err := os.Lstat(curr); err == nil {
			return curr
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			return curr
		}
		curr = parent
	}
}

func isStrictSubdirectory(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == "." {
		return false
	}
	return true
}

func isGitRepo(dir string) bool {
	gitPath := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitPath); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		if _, err := os.Stat(filepath.Join(dir, "objects")); err == nil {
			return true
		}
	}
	return false
}

func findGitWorktreeRepo(worktreeDir string) string {
	gitFile := filepath.Join(worktreeDir, ".git")
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if strings.HasPrefix(line, "gitdir:") {
		gitdir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		worktreesDir := filepath.Dir(gitdir)
		gitDir := filepath.Dir(worktreesDir)
		return filepath.Dir(gitDir)
	}
	return ""
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func makeReadOnly(dir string) error {
	var paths []string
	var isDir []bool
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, p)
		isDir = append(isDir, info.IsDir())
		return nil
	})
	if err != nil {
		return err
	}

	// Apply read-only mode to files first (0400), then directories (0500)
	for i := len(paths) - 1; i >= 0; i-- {
		p := paths[i]
		if isDir[i] {
			if err := os.Chmod(p, 0500); err != nil {
				return err
			}
		} else {
			if err := os.Chmod(p, 0400); err != nil {
				return err
			}
		}
	}
	return nil
}

func makeWritable(dir string) {
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chmod(p, 0700)
		}
		return nil
	})
}
