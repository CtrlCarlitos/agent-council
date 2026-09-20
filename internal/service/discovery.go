package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type DiscoveryMeta struct {
	ProtocolVersion int       `json:"protocol_version"`
	InstanceID      string    `json:"instance_id"`
	PID             int       `json:"pid"`
	Transport       string    `json:"transport"`
	Endpoint        string    `json:"endpoint"`
	StateDir        string    `json:"state_dir"`
	StartedAt       time.Time `json:"started_at"`
}

func GenerateAuthToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random auth token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func PublishDiscovery(stateDir string, meta DiscoveryMeta, token string) error {
	if stateDir == "" {
		return errors.New("stateDir cannot be empty")
	}
	if token == "" {
		return errors.New("token cannot be empty")
	}

	info, err := os.Stat(stateDir)
	if err != nil {
		return fmt.Errorf("stat stateDir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("stateDir %s is not a directory", stateDir)
	}

	// Remove stale socket if present, ensuring it's not a directory or symlink
	sockPath := filepath.Join(stateDir, "council.sock")
	if fi, err := os.Lstat(sockPath); err == nil {
		if fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("stale socket path %s is a directory or symlink", sockPath)
		}
		if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	}

	// 1. Atomically write auth.token (mode 0600)
	tokenPath := filepath.Join(stateDir, "auth.token")
	tmpTokenPath := filepath.Join(stateDir, "auth.token.tmp")
	if err := os.WriteFile(tmpTokenPath, []byte(token), 0600); err != nil {
		return fmt.Errorf("write auth.token.tmp: %w", err)
	}
	if err := os.Rename(tmpTokenPath, tokenPath); err != nil {
		_ = os.Remove(tmpTokenPath)
		return fmt.Errorf("rename auth.token: %w", err)
	}

	// 2. Atomically write service.json (mode 0600) excluding token
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal discovery metadata: %w", err)
	}
	metaPath := filepath.Join(stateDir, "service.json")
	tmpMetaPath := filepath.Join(stateDir, "service.json.tmp")
	if err := os.WriteFile(tmpMetaPath, metaJSON, 0600); err != nil {
		return fmt.Errorf("write service.json.tmp: %w", err)
	}
	if err := os.Rename(tmpMetaPath, metaPath); err != nil {
		_ = os.Remove(tmpMetaPath)
		return fmt.Errorf("rename service.json: %w", err)
	}

	return nil
}
