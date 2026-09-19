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
)

type QueryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

type StoreOptions struct {
	StateDir string
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
