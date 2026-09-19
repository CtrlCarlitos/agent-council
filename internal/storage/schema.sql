-- Schema Migration Tracking
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER NOT NULL PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL -- ISO 8601 UTC (RFC3339Nano)
);

-- Council Runs (Top-level governance scope)
CREATE TABLE IF NOT EXISTS runs (
    run_id TEXT NOT NULL PRIMARY KEY,
    brief_digest TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    profile_digest TEXT NOT NULL,
    controller_lease TEXT NOT NULL, -- Sole authoritative run-level controller lease
    lifecycle TEXT NOT NULL CHECK (lifecycle IN ('active', 'archived')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Contributor Sessions (Logical session identities distinct from contributor roles)
CREATE TABLE IF NOT EXISTS sessions (
    session_id TEXT NOT NULL PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    contributor TEXT NOT NULL CHECK (contributor IN ('opencode', 'claude', 'codex', 'agy')),
    is_active_contributor INTEGER NOT NULL DEFAULT 0 CHECK (is_active_contributor IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('parked', 'running', 'archived')),
    lifecycle TEXT NOT NULL CHECK (lifecycle IN ('active', 'archived')),
    controller_status TEXT NOT NULL CHECK (controller_status IN ('connected', 'disconnected')),
    visibility TEXT NOT NULL CHECK (visibility IN ('reachable', 'host_lost')),
    active_key TEXT,
    recovery_context TEXT,
    recovery_gen INTEGER NOT NULL CHECK (recovery_gen >= 0 AND recovery_gen <= 9223372036854775807),
    active_recovery_gen INTEGER NOT NULL CHECK (active_recovery_gen >= 0 AND active_recovery_gen <= 9223372036854775807),
    row_version INTEGER NOT NULL DEFAULT 1 CHECK (row_version >= 1),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- At most one active primary session per contributor per run for active voting/participation
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_active_contributor 
    ON sessions(run_id, contributor) WHERE is_active_contributor = 1;

-- Native Harness Bindings (Mapping logical sessions to external harness conversations)
CREATE TABLE IF NOT EXISTS native_bindings (
    session_id TEXT NOT NULL PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
    native_session_id TEXT NOT NULL,
    harness TEXT NOT NULL,
    model TEXT NOT NULL,
    workspace_mode TEXT NOT NULL,
    config_json TEXT NOT NULL, -- Allowlisted configuration; no provider credentials
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Pending Prompts (Queued prompts awaiting authorized controller release)
CREATE TABLE IF NOT EXISTS pending_prompts (
    session_id TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    turn_key TEXT NOT NULL,
    prompt TEXT NOT NULL,
    queued_at TEXT NOT NULL,
    PRIMARY KEY (session_id, turn_key)
);

-- Turn History and Active Turns
CREATE TABLE IF NOT EXISTS turns (
    session_id TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    turn_key TEXT NOT NULL,
    prompt TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'completed', 'interrupted', 'cancelling', 'cancelled', 'failed')),
    result TEXT NOT NULL DEFAULT '',
    attempt_id TEXT NOT NULL, -- Immutable execution-attempt identity for this released turn
    created_at TEXT NOT NULL,
    completed_at TEXT,
    PRIMARY KEY (session_id, turn_key)
);

-- Dual-Phase Dispatch Intent Logging (Boundary between DB reservation and external harness)
CREATE TABLE IF NOT EXISTS dispatch_intents (
    session_id TEXT NOT NULL,
    turn_key TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (phase IN ('intent_recorded', 'receipt_acknowledged', 'acceptance_unknown', 'resolved')),
    recorded_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (session_id, turn_key),
    FOREIGN KEY (session_id, turn_key) REFERENCES turns(session_id, turn_key) ON DELETE CASCADE
);

-- Append-Only Audit Journal (Accompanies accepted state transitions; not for event-sourcing replay)
CREATE TABLE IF NOT EXISTS journal_entries (
    seq INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    op_id TEXT NOT NULL UNIQUE, -- Idempotency key
    command_type TEXT NOT NULL, -- Command kind (e.g. 'release_turn', 'queue_prompt')
    command_fingerprint TEXT NOT NULL, -- Hash of canonical command input parameters
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    session_id TEXT REFERENCES sessions(session_id) ON DELETE RESTRICT,
    turn_key TEXT,
    event_kind TEXT NOT NULL,
    payload_version INTEGER NOT NULL DEFAULT 1,
    payload_json TEXT NOT NULL, -- Bounded JSON payload and committed receipt
    created_at TEXT NOT NULL
);

-- Immutable Artifact Revisions
CREATE TABLE IF NOT EXISTS artifact_revisions (
    artifact_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision >= 1),
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    kind TEXT NOT NULL, -- 'proposal', 'ballot', 'synthesis', 'finding', 'patch'
    digest TEXT NOT NULL, -- SHA-256 hex digest of file bytes
    byte_size INTEGER NOT NULL CHECK (byte_size >= 0),
    created_at TEXT NOT NULL,
    PRIMARY KEY (artifact_id, revision)
);

CREATE INDEX IF NOT EXISTS idx_turns_session_status ON turns(session_id, status);
CREATE INDEX IF NOT EXISTS idx_journal_run_seq ON journal_entries(run_id, seq);
CREATE INDEX IF NOT EXISTS idx_artifacts_digest ON artifact_revisions(digest);
