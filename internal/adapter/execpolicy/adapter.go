package execpolicy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// RunProfileStore defines the subset of storage operations needed by the worker adapter.
type RunProfileStore interface {
	GetRunProfile(ctx context.Context, runID string) (storage.RunProfileRecord, error)
	GetSessionRunID(ctx context.Context, sessionID string) (string, error)
}

// ManagedWorkerAdapter connects Council's execution supervisor to native worker processes
// through the unbypassable PolicyExecutor and isolated WorkspaceManager seams.
type ManagedWorkerAdapter struct {
	wm       *workspace.WorkspaceManager
	executor PolicyExecutor
	store    RunProfileStore

	mu         sync.Mutex
	sessions   map[adapter.SessionID]adapter.SessionBinding
	dispatches map[adapter.TurnRef]*managedTurn
}

type managedTurn struct {
	ref     adapter.TurnRef
	proc    ManagedProcess
	stream  *adapter.BufferedStream
	done    chan struct{}
	outcome adapter.DispatchOutcome
	result  adapter.TurnResult
	err     error
}

// NewWorkerAdapter constructs a production adapter backed by the isolated workspace manager
// and unbypassable policy executor.
func NewWorkerAdapter(wm *workspace.WorkspaceManager, executor PolicyExecutor, store RunProfileStore) *ManagedWorkerAdapter {
	return &ManagedWorkerAdapter{
		wm:         wm,
		executor:   executor,
		store:      store,
		sessions:   make(map[adapter.SessionID]adapter.SessionBinding),
		dispatches: make(map[adapter.TurnRef]*managedTurn),
	}
}

func (a *ManagedWorkerAdapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	return adapter.ProbeReport{
		HarnessVersion: adapter.UsageMetric[string]{Value: "1.0.0", Available: true},
		Capabilities: adapter.AdapterCapabilities{
			MidTurnCancellation:  adapter.CapabilitySupported,
			StreamingObservation: adapter.CapabilitySupported,
		},
	}, nil
}

func (a *ManagedWorkerAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}

	a.mu.Lock()
	if b, ok := a.sessions[req.SessionID]; ok {
		a.mu.Unlock()
		return b, nil
	}
	a.mu.Unlock()

	runID := "default"
	sessionID := string(req.SessionID)

	// Allocate workspace through WorkspaceManager
	var rootPath string
	if a.wm != nil {
		mode := "none"
		commit := ""
		sourceRepo := ""

		if a.store != nil {
			rec, err := a.store.GetRunProfile(ctx, runID)
			if err == nil {
				mode = rec.WorkspaceMode
				commit = rec.SourceCommit
				sourceRepo = rec.SourceRepoIdentity
			}
		}

		paths, err := a.wm.AllocateWorkspace(runID, sessionID, mode, sourceRepo, commit)
		if err != nil {
			return adapter.SessionBinding{}, fmt.Errorf("allocate workspace: %w", err)
		}
		rootPath = paths.Root
	}

	cfg := req.Config
	if rootPath != "" {
		cfg.WorkspaceRoot = rootPath
	}

	binding := adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: fmt.Sprintf("managed://%s/%s", runID, sessionID),
		Config:          cfg,
	}

	a.mu.Lock()
	a.sessions[req.SessionID] = binding
	a.mu.Unlock()

	return binding, nil
}

func (a *ManagedWorkerAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.sessions[binding.SessionID]; !ok {
		a.sessions[binding.SessionID] = binding
	}
	return nil
}

func (a *ManagedWorkerAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	a.mu.Lock()
	if existing, ok := a.dispatches[ref]; ok {
		a.mu.Unlock()
		return existing.outcome, nil
	}
	a.mu.Unlock()

	sessionID := string(ref.SessionID)
	runID := "default"
	if a.store != nil {
		if rID, err := a.store.GetSessionRunID(ctx, sessionID); err == nil && rID != "" {
			runID = rID
		}
	}
	if runID == "default" {
		a.mu.Lock()
		if b, ok := a.sessions[ref.SessionID]; ok {
			parts := strings.Split(strings.TrimPrefix(b.NativeSessionID, "managed://"), "/")
			if len(parts) >= 2 {
				runID = parts[0]
			}
		}
		a.mu.Unlock()
	}

	var profile storage.CanonicalProfile
	sourceRepo := ""
	sourceCommit := ""
	if a.store != nil {
		rec, err := a.store.GetRunProfile(ctx, runID)
		if err == nil {
			profile = rec.Profile
			sourceRepo = rec.SourceRepoIdentity
			sourceCommit = rec.SourceCommit
		}
	}

	var paths workspace.WorkspacePaths
	if a.wm != nil {
		var ok bool
		paths, ok = a.wm.GetPaths(runID, sessionID)
		if !ok {
			var err error
			mode := profile.WorkspaceMode
			if mode == "" {
				mode = "none"
			}
			paths, err = a.wm.AllocateWorkspace(runID, sessionID, mode, sourceRepo, sourceCommit)
			if err != nil {
				return adapter.DispatchOutcome{
					Ref:    ref,
					Status: adapter.DispatchRejected,
					Reason: fmt.Sprintf("failed to allocate workspace: %v", err),
				}, err
			}
		}
	}
	if profile.AlgoVersion == "" {
		profile = storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       paths.Mode,
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"sh", "echo", "cat", "git"},
		}
	}

	command := "echo"
	var args []string
	if len(profile.Tooling) > 0 {
		command = profile.Tooling[0]
	}

	launchReq := LaunchRequest{
		RunID:     runID,
		SessionID: sessionID,
		TurnKey:   ref.TurnKey,
		AttemptID: "1",
		Command:   command,
		Args:      args,
		Paths:     paths,
		Profile:   profile,
	}

	proc, err := a.executor.Start(ctx, launchReq)
	if err != nil {
		return adapter.DispatchOutcome{
			Status: adapter.DispatchRejected,
			Reason: fmt.Sprintf("policy execution rejected: %v", err),
		}, err
	}

	stream := adapter.NewBufferedStream(ref, 100)
	turn := &managedTurn{
		ref:     ref,
		proc:    proc,
		stream:  stream,
		done:    make(chan struct{}),
		outcome: adapter.DispatchOutcome{Status: adapter.DispatchAccepted},
	}

	a.mu.Lock()
	a.dispatches[ref] = turn
	a.mu.Unlock()

	if stdin := proc.Stdin(); stdin != nil {
		_, _ = io.WriteString(stdin, prompt)
		if closer, ok := stdin.(io.Closer); ok {
			_ = closer.Close()
		}
	}

	go func() {
		defer close(turn.done)
		var buf strings.Builder
		stdout := proc.Stdout()
		if stdout != nil {
			b, _ := io.ReadAll(stdout)
			buf.Write(b)
		}

		code, waitErr := proc.Wait()
		turn.err = waitErr

		_ = stream.Send(adapter.Event{
			Ref:     ref,
			Type:    adapter.EventProgress,
			Status:  council.TurnRunning,
			Payload: buf.String(),
		})
		_ = stream.Send(adapter.Event{
			Ref:     ref,
			Type:    adapter.EventTerminal,
			Status:  council.TurnCompleted,
			Payload: fmt.Sprintf("exit code %d", code),
		})
		_ = stream.Close()

		turn.result = adapter.TurnResult{
			Ref:          ref,
			Status:       council.TurnCompleted,
			ResultStatus: adapter.ResultAvailable,
			Output:       buf.String(),
			CompletedAt:  time.Now().UTC(),
		}
	}()

	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
}

func (a *ManagedWorkerAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	a.mu.Lock()
	turn, ok := a.dispatches[ref]
	a.mu.Unlock()
	if !ok {
		return nil, errors.New("turn not dispatched")
	}
	return turn.stream, nil
}

func (a *ManagedWorkerAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	a.mu.Lock()
	turn, ok := a.dispatches[ref]
	a.mu.Unlock()
	if !ok {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelRejected, Reason: "turn not found"}, nil
	}

	if err := turn.proc.Terminate(ctx); err != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelRejected, Reason: err.Error()}, err
	}
	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed}, nil
}

func (a *ManagedWorkerAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	a.mu.Lock()
	turn, ok := a.dispatches[ref]
	a.mu.Unlock()
	if !ok {
		return adapter.TurnResult{}, errors.New("turn not dispatched")
	}

	select {
	case <-turn.done:
		return turn.result, turn.err
	case <-ctx.Done():
		return adapter.TurnResult{}, ctx.Err()
	}
}

func (a *ManagedWorkerAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	return adapter.ReconciliationOutcome{
		Ref:          ref,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableActive,
		Observed:     council.TurnRunning,
	}, nil
}
