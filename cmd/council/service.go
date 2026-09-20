package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/client"
	"github.com/CtrlCarlitos/agent-council/internal/service"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func defaultStateDir() string {
	if dir := os.Getenv("COUNCIL_STATE_DIR"); dir != "" {
		return dir
	}
	return ".agent-council"
}

func handleServiceCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: council service <run|start|status|stop> [flags]")
	}

	switch args[0] {
	case "run":
		return runServiceCmd(args[1:])
	case "start":
		return startServiceCmd(args[1:])
	case "status":
		return statusServiceCmd(args[1:])
	case "stop":
		return stopServiceCmd(args[1:])
	default:
		return fmt.Errorf("unknown service subcommand: %s (expected run, start, status, or stop)", args[0])
	}
}

func runServiceCmd(args []string) error {
	fs := flag.NewFlagSet("service run", flag.ContinueOnError)
	stateDirFlag := fs.String("state-dir", defaultStateDir(), "path to state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	stateDir, err := filepath.Abs(*stateDirFlag)
	if err != nil {
		return fmt.Errorf("abs state-dir: %w", err)
	}

	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return fmt.Errorf("mkdir state-dir: %w", err)
	}

	lock, err := service.AcquireServiceLock(stateDir)
	if err != nil {
		return fmt.Errorf("acquire service lock: %w", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer store.Close()

	instanceID := fmt.Sprintf("inst-%d", time.Now().UnixNano())
	authToken, err := service.GenerateAuthToken()
	if err != nil {
		return fmt.Errorf("generate auth token: %w", err)
	}

	cfg := service.ServerConfig{
		StateDir:   stateDir,
		InstanceID: instanceID,
		AuthToken:  authToken,
	}

	srv, err := service.NewServer(store, lock, cfg)
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}

	meta := service.DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      instanceID,
		PID:             os.Getpid(),
		Transport:       "unix",
		Endpoint:        srv.SocketPath(),
		StateDir:        stateDir,
		StartedAt:       time.Now().UTC(),
	}

	if err := service.PublishDiscovery(stateDir, meta, authToken); err != nil {
		return fmt.Errorf("publish discovery: %w", err)
	}

	if err := srv.Start(); err != nil {
		_ = lock.CleanupDiscovery()
		return fmt.Errorf("start server: %w", err)
	}
	defer srv.Close()

	srv.StartSignalHandler()
	_ = srv.WaitForShutdown(context.Background())
	return nil
}

func startServiceCmd(args []string) error {
	fs := flag.NewFlagSet("service start", flag.ContinueOnError)
	stateDirFlag := fs.String("state-dir", defaultStateDir(), "path to state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	stateDir, err := filepath.Abs(*stateDirFlag)
	if err != nil {
		return fmt.Errorf("abs state-dir: %w", err)
	}

	return startDetachedService(stateDir)
}

func statusServiceCmd(args []string) error {
	fs := flag.NewFlagSet("service status", flag.ContinueOnError)
	stateDirFlag := fs.String("state-dir", defaultStateDir(), "path to state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	stateDir, err := filepath.Abs(*stateDirFlag)
	if err != nil {
		return fmt.Errorf("abs state-dir: %w", err)
	}

	c, err := client.New(stateDir)
	if err != nil {
		return fmt.Errorf("service not running or discovery unavailable: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := c.GetStatus(ctx)
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}

	out, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("format status JSON: %w", err)
	}

	fmt.Println(string(out))
	return nil
}

func stopServiceCmd(args []string) error {
	fs := flag.NewFlagSet("service stop", flag.ContinueOnError)
	stateDirFlag := fs.String("state-dir", defaultStateDir(), "path to state directory")
	drainFlag := fs.Bool("drain", false, "drain active turns before stopping")
	timeoutFlag := fs.Duration("timeout", 10*time.Second, "maximum wait duration for service stop")
	if err := fs.Parse(args); err != nil {
		return err
	}

	stateDir, err := filepath.Abs(*stateDirFlag)
	if err != nil {
		return fmt.Errorf("abs state-dir: %w", err)
	}

	c, err := client.New(stateDir)
	if err != nil {
		return fmt.Errorf("service not running: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
	defer cancel()

	status, err := c.GetStatus(ctx)
	if err != nil {
		return fmt.Errorf("get status before stop: %w", err)
	}

	if _, err := c.StopService(ctx, status.InstanceID, *drainFlag); err != nil {
		return fmt.Errorf("stop request: %w", err)
	}

	// Poll until service has cleaned up discovery and stopped
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %v waiting for service to stop", *timeoutFlag)
		case <-ticker.C:
			// Check if discovery files are gone
			sockPath := filepath.Join(stateDir, "council.sock")
			tokenPath := filepath.Join(stateDir, "auth.token")
			if _, err := os.Stat(sockPath); os.IsNotExist(err) {
				if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
					return nil
				}
			}
		}
	}
}
