package opencode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
)

// ProbeLaunchTemplate is an operator-owned builder that produces fully
// validated LaunchRequest values for the adapter's Probe capability check.
// OpenCode-specific; stays out of execpolicy. The adapter appends only
// GeneratedServerEnv to the probe-server request; it never synthesizes
// Paths, Profile, or identifiers internally, and it never allocates
// directories: the scratch working directory is owned and allocated by
// the template (never a contributor workspace, never the Council state
// dir).
type ProbeLaunchTemplate interface {
	// VersionLaunch returns a validated LaunchRequest for
	// `opencode --version`.
	VersionLaunch(ctx context.Context) (execpolicy.LaunchRequest, error)
	// ProbeServeLaunch returns a validated LaunchRequest for a probe
	// `opencode serve` child. The request must be the exact approved
	// serve shape (including `--port 0`, so the child prints its listen
	// address), and Paths.Root must be the operator-allocated scratch
	// directory.
	ProbeServeLaunch(ctx context.Context) (execpolicy.LaunchRequest, error)
}

// Probe performs a provider-free executable capability check against a
// short-lived probe server in the template-owned scratch directory:
//
//  1. Verify the installed opencode binary/version through the
//     PolicyExecutor using the injected operator-owned launch template.
//  2. Start a probe `opencode serve` child (exact approved shape,
//     GeneratedServerEnv appended with freshly generated credentials).
//  3. Call only GET /api/health and GET /api/model — both authenticated
//     with the generated probe credentials through the typed client.
//  4. Terminate the probe server before returning.
//
// Both of the child's output pipes are drained for the child's lifetime.
// No native sessions are created or inspected; native authentication
// status is reported as unknown. The probe server is never registered as
// a contributor server.
func (a *OpenCodeAdapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	report := adapter.ProbeReport{
		Capabilities: adapter.AdapterCapabilities{
			StreamingObservation: adapter.CapabilitySupported,
			SessionResumption:    adapter.CapabilitySupported,
		},
	}

	// 1. Version check through the executor.
	versionReq, err := a.probeTemplate.VersionLaunch(ctx)
	if err != nil {
		return report, fmt.Errorf("probe version launch: %w", err)
	}
	versionProc, err := a.executor.Start(ctx, versionReq)
	if err != nil {
		return report, fmt.Errorf("probe version execution: %w", err)
	}
	// Drain stdout to capture the version string.
	var versionOut strings.Builder
	versionDone := make(chan struct{})
	go func() {
		defer close(versionDone)
		sc := bufio.NewScanner(versionProc.Stdout())
		for sc.Scan() {
			versionOut.WriteString(sc.Text() + "\n")
		}
	}()
	_, waitErr := versionProc.Wait()
	<-versionDone
	if waitErr != nil {
		return report, fmt.Errorf("opencode --version failed: %w", waitErr)
	}
	version := strings.TrimSpace(versionOut.String())
	if version != "" {
		report.HarnessVersion = adapter.UsageMetric[string]{
			Value:     version,
			Available: true,
		}
	}

	// 2. Probe serve child in the template-owned scratch directory. The
	// launch must be the exact approved serve shape before credentials
	// are generated.
	serveReq, err := a.probeTemplate.ProbeServeLaunch(ctx)
	if err != nil {
		return report, fmt.Errorf("probe serve launch: %w", err)
	}
	if !execpolicy.IsOpenCodeServeLaunch(serveReq) {
		return report, fmt.Errorf("%w: probe launch shape is %q %v",
			execpolicy.ErrServerEnvShape, serveReq.Command, serveReq.Args)
	}
	username, password, err := generateServerCredentials()
	if err != nil {
		return report, fmt.Errorf("generate probe credentials: %w", err)
	}
	serveReq.GeneratedServerEnv = &execpolicy.GeneratedServerEnv{Username: username, Password: password}

	serveProc, err := a.executor.Start(ctx, serveReq)
	if err != nil {
		return report, fmt.Errorf("probe serve start: %w", err)
	}

	// Drain the child's stdout into a buffer while polling for the printed
	// listen address; drain stderr for the child's lifetime.
	var stdoutMu sync.Mutex
	var stdoutBuf strings.Builder
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		sc := bufio.NewScanner(serveProc.Stdout())
		for sc.Scan() {
			stdoutMu.Lock()
			stdoutBuf.WriteString(sc.Text() + "\n")
			stdoutMu.Unlock()
		}
	}()
	go func() { _, _ = io.Copy(io.Discard, serveProc.Stderr()) }()

	// 3. Wait for the listen address on stdout, then call health + models
	// with the generated probe credentials.
	endpoint, err := waitProbeEndpoint(ctx, &stdoutMu, &stdoutBuf, stdoutDone, 15*time.Second)
	if err != nil {
		termCtx, termCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer termCancel()
		_ = serveProc.Terminate(termCtx)
		<-stdoutDone
		return report, fmt.Errorf("probe endpoint: %w", err)
	}

	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()
	client := newNativeClient(endpoint, username, password)
	healthErr := client.Health(probeCtx)
	models, modelErr := client.Models(probeCtx)

	// 4. Terminate the probe server before returning. It is never
	// registered in the server manager.
	termCtx, termCancel := context.WithTimeout(ctx, 5*time.Second)
	defer termCancel()
	if termErr := serveProc.Terminate(termCtx); termErr != nil {
		return report, fmt.Errorf("probe terminate: %w (health err: %v, model err: %v)", termErr, healthErr, modelErr)
	}
	<-stdoutDone

	if healthErr != nil {
		return report, fmt.Errorf("probe health: %w", healthErr)
	}
	if modelErr != nil {
		return report, fmt.Errorf("probe models: %w", modelErr)
	}
	report.ModelInventory = adapter.UsageMetric[[]string]{
		Value:     models,
		Available: true,
	}

	// Streaming via SSE is supported by the headless server.
	report.Capabilities.StreamingObservation = adapter.CapabilitySupported
	report.Capabilities.SessionResumption = adapter.CapabilitySupported
	return report, nil
}

// waitProbeEndpoint polls the drained stdout buffer for the printed
// listen address, returning the HTTP endpoint once found.
func waitProbeEndpoint(ctx context.Context, mu *sync.Mutex, buf *strings.Builder, done <-chan struct{}, timeout time.Duration) (string, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		mu.Lock()
		out := buf.String()
		mu.Unlock()
		if ep := scanEndpoint(out); ep != "" {
			return ep, nil
		}
		select {
		case <-done:
			mu.Lock()
			out := buf.String()
			mu.Unlock()
			if ep := scanEndpoint(out); ep != "" {
				return ep, nil
			}
			return "", errors.New("probe server exited before printing its endpoint")
		case <-deadline:
			return "", errors.New("probe server did not print its endpoint in time")
		case <-ticker.C:
		}
	}
}

func scanEndpoint(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if idx := strings.Index(line, "127.0.0.1:"); idx != -1 {
			fields := strings.Fields(line[idx:])
			if len(fields) > 0 {
				return "http://" + strings.Trim(fields[0], "\"'()[]")
			}
		}
	}
	return ""
}
