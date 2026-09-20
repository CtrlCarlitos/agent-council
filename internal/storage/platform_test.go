package storage_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_PermissionsAndGitignore(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Verify .gitignore exists and contains *
	gi, err := os.ReadFile(filepath.Join(tempDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if string(gi) != "*\n" {
		t.Fatalf("expected .gitignore to contain '*\\n', got %q", string(gi))
	}

	if runtime.GOOS != "windows" {
		// Verify POSIX directory mode 0700
		info, err := os.Stat(tempDir)
		if err != nil {
			t.Fatalf("stat tempDir: %v", err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("expected directory perm 0700, got %o", info.Mode().Perm())
		}

		// Verify POSIX db file mode 0600
		dbInfo, err := os.Stat(filepath.Join(tempDir, "state.db"))
		if err != nil {
			t.Fatalf("stat state.db: %v", err)
		}
		if dbInfo.Mode().Perm() != 0600 {
			t.Fatalf("expected db file perm 0600, got %o", dbInfo.Mode().Perm())
		}
	}
}

func TestStore_Permissions_TightenMorePermissiveDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission test")
	}

	tempDir := t.TempDir()
	// Deliberately create directory with permissive 0777
	if err := os.Chmod(tempDir, 0777); err != nil {
		t.Fatalf("chmod 0777: %v", err)
	}

	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	info, err := os.Stat(tempDir)
	if err != nil {
		t.Fatalf("stat tempDir: %v", err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("expected directory to be tightened to 0700, got %o", info.Mode().Perm())
	}
}
