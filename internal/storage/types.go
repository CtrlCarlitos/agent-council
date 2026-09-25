package storage

import (
	"database/sql"
	"errors"
	"time"
)

var (
	ErrStaleUpdate                = errors.New("stale update: row_version mismatch")
	ErrIdempotencyConflict        = errors.New("idempotency conflict: operation id exists with different parameters")
	ErrUnauthorizedOperation      = errors.New("unauthorized operation: caller lease mismatch")
	ErrInconsistentStorage        = errors.New("inconsistent storage: database records violate domain invariants")
	ErrArtifactNotFound           = errors.New("artifact not found")
	ErrArtifactCorrupt            = errors.New("artifact corrupt: sha256 digest mismatch")
	ErrMigrationChecksumMismatch  = errors.New("migration checksum mismatch: schema file has been modified")
	ErrUnsupportedSchemaVersion   = errors.New("unsupported schema version: database version is newer than binary supports")
	ErrRecoveryGenerationOverflow = errors.New("recovery generation overflow: exceeds maximum int64 range")
	ErrHostLost                   = errors.New("host lost: session visibility is host lost")
	ErrInvalidPath                = errors.New("invalid path: path traversal detected")
	ErrSessionArchived            = errors.New("session archived")
	ErrControllerDisconnected     = errors.New("controller disconnected")
	ErrPromptNotQueued            = errors.New("prompt not queued")
	// ErrRequiredToolsImmutable: a queued prompt's required_tools are
	// validated at queue time and immutable thereafter (AC-010 §3.5).
	ErrRequiredToolsImmutable     = errors.New("required_tools of a queued prompt are immutable")
	ErrInvalidExpectedVersion     = errors.New("invalid expected version: must be positive")
	ErrArtifactOversized          = errors.New("artifact exceeds maximum permitted size")
	ErrDisallowedToolingConfig    = errors.New("tooling configuration violates security policy")
	ErrTurnAlreadyExists          = errors.New("turn identifier already exists")
	ErrSymlinkForbidden           = errors.New("symlink state directory not permitted")
	ErrConflictingTerminalOutcome = errors.New("conflicting terminal outcome on already-terminal turn")
	ErrSessionNotFound            = errors.New("session not found")
	ErrRunSessionMismatch         = errors.New("session does not belong to run")
	ErrInvalidAttempt             = errors.New("invalid execution attempt")
	ErrConflictingArtifact        = errors.New("conflicting artifact content")
	ErrRunNotFound                = errors.New("run not found")
)

type QueryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

type StoreOptions struct {
	StateDir             string
	TestHookBeforeCommit func(boundary string)
}

type DecisionRecord struct {
	OpID            string    `json:"op_id"`
	RunID           string    `json:"run_id"`
	ArtifactID      string    `json:"artifact_id"`
	Revision        int64     `json:"revision"`
	Digest          string    `json:"digest"`
	DecisionPayload string    `json:"decision_payload"`
	CreatedAt       time.Time `json:"created_at"`
}

type OperationReceipt struct {
	OpID             string    `json:"op_id"`
	CommandType      string    `json:"command_type"`
	SessionID        string    `json:"session_id"`
	TurnKey          string    `json:"turn_key,omitempty"`
	CommittedVersion int64     `json:"committed_version"`
	CreatedAt        time.Time `json:"created_at"`
	Payload          string    `json:"payload,omitempty"`
}

type ReleaseReceipt struct {
	OperationReceipt
	SanitizedPrompt string `json:"sanitized_prompt"`
	TurnKey         string `json:"turn_key"`
	AttemptID       string `json:"attempt_id"`
}

type ReleaseDisposition string

const (
	ReleaseDispositionNew      ReleaseDisposition = "new"
	ReleaseDispositionReplayed ReleaseDisposition = "replayed"
)

type ReleaseResult struct {
	Receipt     ReleaseReceipt
	Disposition ReleaseDisposition
}

type DiagnosticCounts struct {
	ActiveRuns       []string
	ReservedTurns    int
	UnresolvedTurns  int
	RecoveryBlockers int
}
