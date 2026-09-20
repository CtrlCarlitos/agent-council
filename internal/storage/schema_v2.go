package storage

// schemaV2DDL is the immutable v2 migration body. v2 adds the controller
// lease history with its mandatory one-active-grant index, run-level
// adoption state, and the issuing-generation stamp on accepted execution
// intents (backfilled as generation 0 without modifying attempt IDs).
// Existing runs receive a generation-0 provenance row preserving their
// legacy credential — provenance, never an alternate controller grant.
const schemaV2DDL = `
CREATE TABLE IF NOT EXISTS controller_leases (
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    generation INTEGER NOT NULL CHECK (generation >= 0),
    harness TEXT,
    controller_ref TEXT NOT NULL,
    lease TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active','legacy','superseded','revoked')),
    granted_by_op_id TEXT NOT NULL,
    attachment_id TEXT,
    instance_id TEXT,
    connected INTEGER NOT NULL DEFAULT 0 CHECK (connected IN (0, 1)),
    attached_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, generation)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_controller_leases_active
    ON controller_leases(run_id) WHERE status = 'active';
ALTER TABLE runs ADD COLUMN controller_adopted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dispatch_intents ADD COLUMN issuing_controller_generation INTEGER NOT NULL DEFAULT 0;
`

// backfillControllerLeaseProvenance records each existing run's legacy
// credential as generation-0 provenance. Runs without a credential get no
// row; nothing here grants authority.
const backfillControllerLeaseProvenance = `
INSERT INTO controller_leases
    (run_id, generation, harness, controller_ref, lease, status, granted_by_op_id, attached_at, updated_at)
SELECT run_id, 0, NULL, 'legacy-v1', controller_lease, 'legacy', 'legacy-v1', ?, ?
FROM runs
WHERE controller_lease != '';
`
