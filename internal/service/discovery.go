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

func writePrivateFile(path string, data []byte) error {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write to symlink at %s", path)
		}
		_ = os.Remove(path)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create private file %s: %w", path, err)
	}
	defer f.Close()

	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("chmod private file %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write private file %s: %w", path, err)
	}
	return f.Sync()
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

	// Remove stale socket if present, ensuring it is strictly a socket
	sockPath := filepath.Join(stateDir, "council.sock")
	if fi, err := os.Lstat(sockPath); err == nil {
		if fi.Mode().Type() != os.ModeSocket {
			return fmt.Errorf("unexpected file at %s: expected socket, found mode %v", sockPath, fi.Mode())
		}
		if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	}

	// 1. Atomically write auth.token (mode 0600)
	tokenPath := filepath.Join(stateDir, "auth.token")
	tmpTokenPath := filepath.Join(stateDir, "auth.token.tmp")
	if err := writePrivateFile(tmpTokenPath, []byte(token)); err != nil {
		return fmt.Errorf("write auth.token.tmp: %w", err)
	}
	if fi, err := os.Lstat(tokenPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(tmpTokenPath)
		return fmt.Errorf("target token path %s is an unexpected symlink", tokenPath)
	}
	if err := os.Rename(tmpTokenPath, tokenPath); err != nil {
		_ = os.Remove(tmpTokenPath)
		return fmt.Errorf("rename auth.token: %w", err)
	}
	_ = os.Chmod(tokenPath, 0600)

	// 2. Atomically write service.json (mode 0600) excluding token
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal discovery metadata: %w", err)
	}
	metaPath := filepath.Join(stateDir, "service.json")
	tmpMetaPath := filepath.Join(stateDir, "service.json.tmp")
	if err := writePrivateFile(tmpMetaPath, metaJSON); err != nil {
		return fmt.Errorf("write service.json.tmp: %w", err)
	}
	if fi, err := os.Lstat(metaPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(tmpMetaPath)
		return fmt.Errorf("target meta path %s is an unexpected symlink", metaPath)
	}
	if err := os.Rename(tmpMetaPath, metaPath); err != nil {
		_ = os.Remove(tmpMetaPath)
		return fmt.Errorf("rename service.json: %w", err)
	}
	_ = os.Chmod(metaPath, 0600)

	return nil
}
