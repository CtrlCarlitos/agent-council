package execpolicy

import (
	"bytes"
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
	GetSessionMetadata(ctx context.Context, sessionID string) (storage.SessionMetadata, error)
}

// NativeInvocation defines the command and arguments for invoking a native harness.
type NativeInvocation struct {
	Command           string
	Args              []string
	ExtraEnvAllowlist []string
}

// NativeInvocationBuilder constructs command line parameters for a native harness contributor.
type NativeInvocationBuilder func(contrib council.Contributor, spec storage.HarnessProfileSpec, prompt string) (NativeInvocation, error)

// BuildNativeInvocation determines the native CLI command and arguments for a contributor.
func BuildNativeInvocation(contrib council.Contributor, spec storage.HarnessProfileSpec, prompt string) (NativeInvocation, error) {
	if !council.ValidContributor(contrib) {
		return NativeInvocation{}, fmt.Errorf("invalid contributor: %q", contrib)
	}
	if strings.TrimSpace(spec.Model) == "" {
		return NativeInvocation{}, fmt.Errorf("missing model for contributor %q", contrib)
	}
	if strings.TrimSpace(spec.NativeAuthMode) == "" {
		return NativeInvocation{}, fmt.Errorf("missing native auth mode for contributor %q", contrib)
	}

	switch contrib {
	case council.Claude:
		return NativeInvocation{
			Command:           "claude",
			Args:              []string{"--model", spec.Model, "-p", prompt},
			ExtraEnvAllowlist: spec.ExtraEnvAllowlist,
		}, nil
	case council.Codex:
		return NativeInvocation{
			Command:           "codex",
			Args:              []string{"exec", "--model", spec.Model, prompt},
			ExtraEnvAllowlist: spec.ExtraEnvAllowlist,
		}, nil
	case council.OpenCode:
		return NativeInvocation{
			Command:           "opencode",
			Args:              []string{"run", "--model", spec.Model, prompt},
			ExtraEnvAllowlist: spec.ExtraEnvAllowlist,
		}, nil
	case council.Agy:
		return NativeInvocation{
			Command:           "agy",
			Args:              []string{"--model", spec.Model, "-p", prompt},
			ExtraEnvAllowlist: spec.ExtraEnvAllowlist,
		}, nil
	default:
		return NativeInvocation{}, fmt.Errorf("unsupported native contributor: %q", contrib)
	}
}

// ManagedWorkerAdapter connects Council's execution supervisor to native worker processes
// through the unbypassable PolicyExecutor and isolated WorkspaceManager seams.
type ManagedWorkerAdapter struct {
	wm       *workspace.WorkspaceManager
	executor PolicyExecutor
	store    RunProfileStore
	builder  NativeInvocationBuilder

	mu         sync.Mutex
	sessions   map[adapter.SessionID]adapter.SessionBinding
	dispatches map[adapter.TurnRef]*managedTurn
	launching  map[adapter.TurnRef]*launchReservation
}

// launchReservation reserves a turn identity for the duration of process
// creation so concurrent duplicate dispatches cannot launch the same turn
// twice (Gate 2 review finding 1). Waiters receive the launcher's verdict.
type launchReservation struct {
	ready   chan struct{}
	outcome adapter.DispatchOutcome
	err     error
}

func (r *launchReservation) complete(outcome adapter.DispatchOutcome, err error) {
	r.outcome, r.err = outcome, err
	close(r.ready)
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
// and unbypassable policy executor using standard native harness invocations.
func NewWorkerAdapter(wm *workspace.WorkspaceManager, executor PolicyExecutor, store RunProfileStore) *ManagedWorkerAdapter {
	return NewWorkerAdapterWithInvocation(wm, executor, store, BuildNativeInvocation)
}

// NewWorkerAdapterWithInvocation constructs a worker adapter with a custom native invocation builder.
func NewWorkerAdapterWithInvocation(
	wm *workspace.WorkspaceManager,
	executor PolicyExecutor,
	store RunProfileStore,
	builder NativeInvocationBuilder,
) *ManagedWorkerAdapter {
	if builder == nil {
		builder = BuildNativeInvocation
	}
	return &ManagedWorkerAdapter{
		wm:         wm,
		executor:   executor,
		store:      store,
		builder:    builder,
		sessions:   make(map[adapter.SessionID]adapter.SessionBinding),
		dispatches: make(map[adapter.TurnRef]*managedTurn),
		launching:  make(map[adapter.TurnRef]*launchReservation),
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

	if a.store == nil {
		return adapter.SessionBinding{}, errors.New("store is required for session creation")
	}

	sessionID := string(req.SessionID)
	meta, err := a.store.GetSessionMetadata(ctx, sessionID)
	if err != nil {
		return adapter.SessionBinding{}, fmt.Errorf("lookup session %s: %w", sessionID, err)
	}
	if meta.Contributor != string(req.Contributor) {
		return adapter.SessionBinding{}, fmt.Errorf("contributor mismatch for session %s: store has %s, req has %s", sessionID, meta.Contributor, req.Contributor)
	}

	rec, err := a.store.GetRunProfile(ctx, meta.RunID)
	if err != nil {
		return adapter.SessionBinding{}, fmt.Errorf("retrieve run profile for run %s: %w", meta.RunID, err)
	}
	profile := rec.Profile
	if profile.AlgoVersion == "" || len(profile.Harnesses) == 0 {
		return adapter.SessionBinding{}, errors.New("invalid or empty frozen run profile")
	}

	harnessSpec, ok := profile.Harnesses[string(req.Contributor)]
	if !ok {
		return adapter.SessionBinding{}, fmt.Errorf("contributor %s has no configured harness profile", req.Contributor)
	}
	if strings.TrimSpace(harnessSpec.Model) == "" {
		return adapter.SessionBinding{}, fmt.Errorf("contributor %s missing model in harness profile", req.Contributor)
	}
	if strings.TrimSpace(harnessSpec.NativeAuthMode) == "" {
		return adapter.SessionBinding{}, fmt.Errorf("contributor %s missing native auth mode in harness profile", req.Contributor)
	}

	var rootPath string
	if a.wm != nil {
		paths, err := a.wm.AllocateWorkspace(meta.RunID, sessionID, profile.WorkspaceMode, rec.SourceRepoIdentity, rec.SourceCommit)
		if err != nil {
			return adapter.SessionBinding{}, fmt.Errorf("allocate workspace: %w", err)
		}
		rootPath = paths.Root
	}

	cfg := req.Config
	if rootPath != "" {
		cfg.WorkspaceRoot = rootPath
	}
	cfg.Model = harnessSpec.Model
	cfg.Tooling = profile.Tooling

	binding := adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: fmt.Sprintf("managed://%s/%s", meta.RunID, sessionID),
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
	if err := ref.Validate(); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	// Reserve the turn identity under the lock BEFORE any process creation:
	// concurrent duplicate dispatches observe the reservation and receive
	// the launcher's verdict instead of launching a second worker
	// (Gate 2 review finding 1).
	a.mu.Lock()
	if res, ok := a.launching[ref]; ok {
		a.mu.Unlock()
		<-res.ready
		return res.outcome, res.err
	}
	if existing, ok := a.dispatches[ref]; ok {
		a.mu.Unlock()
		return existing.outcome, nil
	}
	res := &launchReservation{ready: make(chan struct{})}
	a.launching[ref] = res
	a.mu.Unlock()
	outcome, launchErr := func() (adapter.DispatchOutcome, error) {

		if a.store == nil {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: "store is required for dispatch",
			}, errors.New("store is required")
		}

		sessionID := string(ref.SessionID)
		meta, err := a.store.GetSessionMetadata(ctx, sessionID)
		if err != nil {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: fmt.Sprintf("session lookup failed: %v", err),
			}, fmt.Errorf("lookup session %s: %w", sessionID, err)
		}

		contributor := council.Contributor(meta.Contributor)
		if !council.ValidContributor(contributor) {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: fmt.Sprintf("invalid contributor %q", meta.Contributor),
			}, fmt.Errorf("invalid contributor %q", meta.Contributor)
		}

		runProfileRec, err := a.store.GetRunProfile(ctx, meta.RunID)
		if err != nil {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: fmt.Sprintf("failed to retrieve run profile: %v", err),
			}, fmt.Errorf("retrieve run profile for run %s: %w", meta.RunID, err)
		}

		profile := runProfileRec.Profile
		if profile.AlgoVersion == "" || len(profile.Harnesses) == 0 {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: "missing or empty frozen canonical profile",
			}, errors.New("missing or empty frozen canonical profile")
		}

		harnessSpec, ok := profile.Harnesses[string(contributor)]
		if !ok {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: fmt.Sprintf("contributor %q has no configured harness profile", contributor),
			}, fmt.Errorf("contributor %q has no configured harness profile in run %s", contributor, meta.RunID)
		}
		if strings.TrimSpace(harnessSpec.Model) == "" {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: fmt.Sprintf("contributor %q has empty model", contributor),
			}, fmt.Errorf("contributor %q has empty model in harness profile", contributor)
		}
		if strings.TrimSpace(harnessSpec.NativeAuthMode) == "" {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: fmt.Sprintf("contributor %q has empty native auth mode", contributor),
			}, fmt.Errorf("contributor %q has empty native auth mode in harness profile", contributor)
		}

		if a.wm == nil {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: "workspace manager required",
			}, errors.New("workspace manager required")
		}

		paths, ok := a.wm.GetPaths(meta.RunID, sessionID)
		if !ok {
			var err error
			paths, err = a.wm.AllocateWorkspace(meta.RunID, sessionID, profile.WorkspaceMode, runProfileRec.SourceRepoIdentity, runProfileRec.SourceCommit)
			if err != nil {
				return adapter.DispatchOutcome{
					Ref:    ref,
					Status: adapter.DispatchRejected,
					Reason: fmt.Sprintf("failed to allocate workspace: %v", err),
				}, fmt.Errorf("allocate workspace: %w", err)
			}
		}

		inv, err := a.builder(contributor, harnessSpec, prompt)
		if err != nil {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: fmt.Sprintf("failed to build native invocation: %v", err),
			}, fmt.Errorf("build native invocation: %w", err)
		}

		toolAllowed := false
		for _, t := range profile.Tooling {
			if t == inv.Command {
				toolAllowed = true
				break
			}
		}
		if !toolAllowed {
			return adapter.DispatchOutcome{
				Ref:    ref,
				Status: adapter.DispatchRejected,
				Reason: fmt.Sprintf("harness command %q is not in profile tooling allowlist", inv.Command),
			}, fmt.Errorf("harness command %q is not in profile tooling allowlist", inv.Command)
		}

		launchReq := LaunchRequest{
			RunID:             meta.RunID,
			SessionID:         sessionID,
			TurnKey:           ref.TurnKey,
			AttemptID:         "1",
			Command:           inv.Command,
			Args:              inv.Args,
			ExtraEnvAllowlist: inv.ExtraEnvAllowlist,
			Paths:             paths,
			Profile:           profile,
		}

		proc, err := a.executor.Start(ctx, launchReq)
		if err != nil {
			return adapter.DispatchOutcome{
				Ref:    ref,
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
			outcome: adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted},
		}

		a.mu.Lock()
		a.dispatches[ref] = turn
		a.mu.Unlock()

		go func() {
			defer close(turn.done)
			var copyWg sync.WaitGroup
			var stdoutBuf bytes.Buffer
			var stderrBuf bytes.Buffer

			if stdout := proc.Stdout(); stdout != nil {
				copyWg.Add(1)
				go func() {
					defer copyWg.Done()
					_, _ = io.Copy(&stdoutBuf, stdout)
				}()
			}
			if stderr := proc.Stderr(); stderr != nil {
				copyWg.Add(1)
				go func() {
					defer copyWg.Done()
					_, _ = io.Copy(&stderrBuf, stderr)
				}()
			}

			// Drain stdout/stderr before calling proc.Wait().
			// io.Copy blocks until the pipe write-end closes (process exits),
			// so copyWg.Wait() implicitly waits for process completion.
			// Calling proc.Wait() first causes cmd.Wait() to close the pipe
			// readers before our goroutines finish reading, losing output under
			// the race detector (and on any slow goroutine schedule).
			copyWg.Wait()
			code, waitErr := proc.Wait()

			outStr := stdoutBuf.String()
			errStr := stderrBuf.String()
			fullOutput := outStr
			if errStr != "" {
				if fullOutput != "" {
					fullOutput += "\n"
				}
				fullOutput += errStr
			}

			turn.err = waitErr
			now := time.Now().UTC()

			if waitErr != nil || code != 0 {
				if turn.err == nil {
					turn.err = fmt.Errorf("process failed with exit code %d", code)
				}
				_ = stream.Send(adapter.Event{
					Ref:       ref,
					Type:      adapter.EventTerminal,
					Status:    council.TurnFailed,
					Payload:   fmt.Sprintf("process failed with exit code %d: %s", code, fullOutput),
					Timestamp: now,
				})
				_ = stream.Close()

				turn.result = adapter.TurnResult{
					Ref:          ref,
					Status:       council.TurnFailed,
					ResultStatus: adapter.ResultFailed,
					Output:       fullOutput,
					CompletedAt:  now,
				}
				return
			}

			_ = stream.Send(adapter.Event{
				Ref:       ref,
				Type:      adapter.EventProgress,
				Status:    council.TurnRunning,
				Payload:   fullOutput,
				Timestamp: now,
			})
			_ = stream.Send(adapter.Event{
				Ref:       ref,
				Type:      adapter.EventTerminal,
				Status:    council.TurnCompleted,
				Payload:   fmt.Sprintf("exit code %d", code),
				Timestamp: now,
			})
			_ = stream.Close()

			turn.result = adapter.TurnResult{
				Ref:          ref,
				Status:       council.TurnCompleted,
				ResultStatus: adapter.ResultAvailable,
				Output:       fullOutput,
				CompletedAt:  now,
			}
		}()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
	}()
	if launchErr != nil {
		res.complete(outcome, launchErr)
		a.mu.Lock()
		delete(a.launching, ref)
		a.mu.Unlock()
		return outcome, launchErr
	}
	res.complete(outcome, nil)
	a.mu.Lock()
	delete(a.launching, ref)
	a.mu.Unlock()
	return outcome, nil
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
		return adapter.TurnResult{Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultPending}, ctx.Err()
	}
}

// Reconcile reports only what this adapter can actually verify (Gate 2
// review finding 2): a live in-memory execution for the turn is
// ReachableActive; a finished or in-flight-but-unverifiable execution is
// reported as uncertain with host visibility lost; a turn this process
// never dispatched is definitive absence — after a daemon restart the
// in-process worker (and any native execution it owned) is genuinely gone,
// and absence is never reported as a running worker.
func (a *ManagedWorkerAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	a.mu.Lock()
	turn, ok := a.dispatches[ref.TurnRef]
	a.mu.Unlock()
	if !ok {
		return adapter.ReconciliationOutcome{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationDefinitivelyMissing,
			Observed:     council.TurnFailed,
			Result:       "no in-memory execution record for this turn",
		}, nil
	}

	select {
	case <-turn.done:
		// The execution finished (or was torn down) without this adapter
		// being able to attribute a verified native outcome here.
		return adapter.ReconciliationOutcome{
			Ref:          ref,
			Reachability: council.VisibilityHostLost,
			Status:       adapter.ReconciliationUncertain,
			Observed:     council.TurnRunning,
		}, nil
	default:
		return adapter.ReconciliationOutcome{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableActive,
			Observed:     council.TurnRunning,
		}, nil
	}
}
