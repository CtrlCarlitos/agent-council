package storage

// schemaV3DDL is the immutable v3 migration body. v3 adds frozen run inputs and
// profiles (run_profiles), binds artifact revisions to originating sessions,
// attempts, and release status, and introduces sealed proposal sets and members.
const schemaV3DDL = `
CREATE TABLE IF NOT EXISTS run_profiles (
    run_id TEXT NOT NULL PRIMARY KEY REFERENCES runs(run_id) ON DELETE RESTRICT,
    profile_digest TEXT NOT NULL,
    algorithm_version TEXT NOT NULL,
    workspace_mode TEXT NOT NULL CHECK (workspace_mode IN ('none', 'readonly', 'isolated_branch')),
    isolation_strictness TEXT NOT NULL CHECK (isolation_strictness IN ('strict', 'permissive_dev')),
    network_mode TEXT NOT NULL CHECK (network_mode IN ('none', 'allowlist', 'unrestricted')),
    canonical_profile_json TEXT NOT NULL,
    source_repo_identity TEXT NOT NULL,
    source_commit TEXT NOT NULL,
    source_tree TEXT NOT NULL,
    brief_artifact_digest TEXT NOT NULL,
    created_at TEXT NOT NULL
);

ALTER TABLE artifact_revisions ADD COLUMN session_id TEXT REFERENCES sessions(session_id);
ALTER TABLE artifact_revisions ADD COLUMN attempt_id TEXT;
ALTER TABLE artifact_revisions ADD COLUMN released INTEGER NOT NULL DEFAULT 0 CHECK (released IN (0, 1));
ALTER TABLE artifact_revisions ADD COLUMN proposal_set_digest TEXT;

CREATE TABLE IF NOT EXISTS proposal_sets (
    proposal_set_digest TEXT NOT NULL PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    released_by_op_id TEXT NOT NULL,
    issuing_controller_generation INTEGER NOT NULL,
    sealed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS proposal_set_members (
    proposal_set_digest TEXT NOT NULL REFERENCES proposal_sets(proposal_set_digest) ON DELETE RESTRICT,
    artifact_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    digest TEXT NOT NULL,
    author_session_id TEXT NOT NULL,
    author_contributor TEXT NOT NULL,
    PRIMARY KEY (proposal_set_digest, artifact_id, revision)
);

CREATE INDEX IF NOT EXISTS idx_artifact_revisions_lookup
    ON artifact_revisions(run_id, released, proposal_set_digest);
`

// backfillRunProfilesAndArtifacts marks existing legacy runs as unverified
// without fabricating new digests or isolated workspaces, and backfills
// legacy artifact revisions with released = 1 and proposal_set_digest = NULL.
const backfillRunProfilesAndArtifacts = `
INSERT INTO run_profiles (
    run_id,
    profile_digest,
    algorithm_version,
    workspace_mode,
    isolation_strictness,
    network_mode,
    canonical_profile_json,
    source_repo_identity,
    source_commit,
    source_tree,
    brief_artifact_digest,
    created_at
)
SELECT
    run_id,
    profile_digest,
    'legacy-unverified',
    'none',
    'permissive_dev',
    'unrestricted',
    '',
    'legacy',
    '',
    '',
    '',
    created_at
FROM runs
WHERE run_id NOT IN (SELECT run_id FROM run_profiles);

UPDATE artifact_revisions
SET released = 1, proposal_set_digest = NULL;
`
