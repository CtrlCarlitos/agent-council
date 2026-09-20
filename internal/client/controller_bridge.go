package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/CtrlCarlitos/agent-council/internal/service"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ErrBridgeEscalation reports an attempt to use the restricted controller
// interface for operations outside its capability.
var ErrBridgeEscalation = errors.New("operation is not available through the controller interface")

// ControllerBridge is a minimal restricted invocation path (AC-004 §8.1,
// the seed of the AC-011 bridge). It holds the operator credential
// internally — outside controller-supplied arguments and responses — and
// exposes only the defined controller commands, bound to one run and one
// controller lease. Administration (adoption, handoff, revocation,
// credential recovery) and arbitrary HTTP routes are rejected through this
// interface. Real harness integration is later work; this boundary is
// endpoint-level.
type ControllerBridge struct {
	c     *Client
	runID string
	lease string
}

// NewControllerBridge reads the operator transport credential itself; the
// controller conversation supplies only its run and lease.
func NewControllerBridge(stateDir, runID, lease string) (*ControllerBridge, error) {
	c, err := New(stateDir)
	if err != nil {
		return nil, fmt.Errorf("controller bridge transport: %w", err)
	}
	return &ControllerBridge{c: c, runID: runID, lease: lease}, nil
}

// RunID identifies the bridge's bound run.
func (b *ControllerBridge) RunID() string { return b.runID }

// QueuePrompt queues a prompt for a session in the bridge's run.
func (b *ControllerBridge) QueuePrompt(ctx context.Context, opID, sessionID, turnKey, prompt string, expectedVersion int64) (*storage.OperationReceipt, error) {
	req := service.QueuePromptRequest{
		OpID:            opID,
		ControllerLease: b.lease,
		ExpectedVersion: expectedVersion,
		TurnKey:         turnKey,
		Prompt:          prompt,
	}
	var resp service.QueuePromptResponse
	if err := b.c.do(ctx, "POST", fmt.Sprintf("/v1/runs/%s/sessions/%s/prompts/queue", b.runID, sessionID), req, &resp); err != nil {
		return nil, err
	}
	return &resp.Receipt, nil
}

// ReleaseTurn releases an approved queued prompt in the bridge's run.
func (b *ControllerBridge) ReleaseTurn(ctx context.Context, opID, sessionID, turnKey string, expectedVersion int64) (*service.ReleaseResponse, error) {
	req := service.ReleaseRequest{
		OpID:            opID,
		ControllerLease: b.lease,
		ExpectedVersion: expectedVersion,
	}
	var resp service.ReleaseResponse
	if err := b.c.do(ctx, "POST", fmt.Sprintf("/v1/runs/%s/sessions/%s/turns/%s/release", b.runID, sessionID, turnKey), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetTurnDetails retrieves authoritative turn state in the bridge's run.
func (b *ControllerBridge) GetTurnDetails(ctx context.Context, sessionID, turnKey string) (*storage.TurnDetails, error) {
	return b.c.GetTurnDetails(ctx, b.runID, sessionID, turnKey)
}

// Connect attaches the bridge's controller to the service instance.
func (b *ControllerBridge) Connect(ctx context.Context, opID string, expectedGeneration uint64) (*service.ControllerConnectRunResponse, error) {
	return b.c.ConnectRunController(ctx, b.runID, opID, b.lease, expectedGeneration)
}

// Disconnect ends the bridge controller's current attachment episode.
func (b *ControllerBridge) Disconnect(ctx context.Context, opID string, expectedGeneration uint64) error {
	rec, err := b.c.GetControllerRecord(ctx, b.runID)
	if err != nil {
		return err
	}
	req := service.ControllerDisconnectRunRequest{
		OpID:               opID,
		ControllerLease:    b.lease,
		ExpectedGeneration: expectedGeneration,
		AttachmentID:       rec.AttachmentID,
	}
	var resp service.ControllerDisconnectRunResponse
	return b.c.do(ctx, "POST", fmt.Sprintf("/v1/runs/%s/controller/disconnect", b.runID), req, &resp)
}

// GetControllerRecord reads the bridge run's redacted controller record.
func (b *ControllerBridge) GetControllerRecord(ctx context.Context) (*service.ControllerRecordResponse, error) {
	var resp service.ControllerRecordResponse
	if err := b.c.do(ctx, "GET", fmt.Sprintf("/v1/runs/%s/controller", b.runID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Administration and arbitrary-route operations are structurally denied:
// the bridge exposes no method that reaches them, and these stubs make the
// denial explicit and testable.

func (b *ControllerBridge) Adopt(ctx context.Context, opID, harness, controllerRef, bootstrapLease string) error {
	return fmt.Errorf("%w: adoption is operator administration", ErrBridgeEscalation)
}

func (b *ControllerBridge) Handoff(ctx context.Context, opID string, expectedGeneration uint64, harness, controllerRef string) error {
	return fmt.Errorf("%w: handoff is operator administration", ErrBridgeEscalation)
}

func (b *ControllerBridge) Revoke(ctx context.Context, opID string) error {
	return fmt.Errorf("%w: revocation is operator administration", ErrBridgeEscalation)
}

func (b *ControllerBridge) RecoverCredential(ctx context.Context, opID, sourceOpID string, expectedGeneration uint64) error {
	return fmt.Errorf("%w: credential recovery is operator administration", ErrBridgeEscalation)
}

func (b *ControllerBridge) RawRequest(ctx context.Context, method, path string, body any) error {
	return fmt.Errorf("%w: arbitrary routes are not available", ErrBridgeEscalation)
}
