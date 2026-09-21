# AC-005: Freeze Tooling/Source Profiles and Prove Independent Worker Isolation — Design

- **Status:** Proposed
- **Date:** 2026-09-20
- **Issue:** [#5 (AC-005)](https://github.com/CtrlCarlitos/agent-council/issues/5)
- **Dependencies:** AC-001 (#1, closed), AC-002 (#2, closed), AC-003 (#3, closed), AC-004 (#4, closed)
- **Target Packages:** `internal/council`, `internal/storage`, `internal/adapter`, `internal/service`, `internal/client`

---

## 1. Problem and Scope

Equal skill catalogs do not guarantee the same effective toolkit, and bare Git worktrees do not prevent sibling reads, shared indexes, or credential leakage across council workers.

AC-005 establishes the operational contract for immutable run profiles, isolated execution workspaces, defense-in-depth launch policies, service-owned artifact ingestion, and sealed proposal release before native contributor adapters (AC-007 through AC-010) are wired.

### In Scope:
1. **Profile, Brief, and Source Freezing**:
   - Specification and storage of the immutable input artifacts: the raw brief text, the canonical profile document, and the validated source repository identity with commit/tree anchors.
   - Deterministic, versioned canonical encoding and digest computation (`brief_digest`, `source_digest`, `profile_digest`).
2. **Three-Tier Workspace Allocation & Session-Keyed Workspaces**:
   - Workspace directories keyed by `{run_id}/{session_id}` (supporting replacement sessions and clean-room reviewers without collisions).
   - Placement under an operator-configured `workspace_base_dir` strictly disjoint from the protected `state_dir` (which contains `auth.token`, `council.sock`, SQLite databases, and raw storage blobs).
   - Three operational modes:
     - `none`: empty scratch directory for non-code deliberation.
     - `readonly`: concrete read-only repository tree with platform-validated enforcement.
     - `isolated_branch`: single dedicated Git worktree on assigned branch `council/{run_id}/{session_id}`.
3. **Execution Policy & Launch Specification Seam**:
   - An explicit policy module producing a `ValidatedLaunchSpec` (canonical executable, arguments, resolved `cwd`, allowlisted environment, and file access constraints).
   - Clear distinction between **Council-mediated policy enforcement** (command gating, path traversal checks, Git branch restrictions) and **underlying OS enforcement** (namespaces/mounts/jail). Unsupported capabilities fail closed with explicit errors.
4. **Worker Authority & Service-Owned Artifact Ingestion**:
   - Workers **never** hold or receive controller leases or operator bearer tokens.
   - Artifact creation from workers is internal to the execution supervisor and authorized exclusively via `ExecutionRef{SessionID, TurnKey, AttemptID}` (aligning with AC-004).
5. **Atomic Sealing of Artifact Revisions**:
   - `ReleaseArtifacts` binds an explicit sorted set of `(artifact_id, revision, digest, author_session_id)` tuples.
   - Computes an authoritative `proposal_set_digest`. Atomic transaction transitions visibility from unreleased (`released = 0`) to sealed/released (`released = 1`).
   - Sibling contributors during first-pass generation receive no access to unreleased proposals or controller conversation history; peer reviewers receive released proposals via controller-dispatched turn prompts or typed adapter inputs.
6. **Native Authentication vs. Config Isolation**:
   - Model an opaque `NativeAuthMode` per harness (`inherited_host_keychain`, `harness_login_delegation`, `none`).
   - Council neither copies nor inspects native provider tokens.
   - Environment sanitization uses strict allowlists (never blanket keyword removal), ensuring native auth sockets function while Council credentials are excluded.

### Out of Scope:
- Native harness CLI subprocess orchestration for specific commercial providers (OpenCode, Claude, Codex, Agy are implemented in AC-007–AC-010).
- Ballot tallying, preference scoring, and synthesis adjudication (AC-013).
- Token usage quotas, process timeouts, and financial budget accounting (AC-014).
- Hostile multi-tenant kernel virtualization (Docker/VM). AC-005 enforces local host-user runtime, filesystem path, and API isolation.

---

## 2. Invariants & Security Architecture

1. **Immutable Triad**:
   - Every run is permanently bound to `brief_digest`, `source_digest`, and `profile_digest`.
   - The underlying brief text and canonical profile JSON are stored permanently in the content-addressed store (CAS) or database; a run cannot be initialized without validating that the digests match the stored artifacts.
2. **Strict Authority Separation**:
   - Worker processes run without operator tokens (`auth.token`) or controller leases.
   - Workers cannot query administration routes, cannot call `PublishArtifact` as a controller, and cannot alter their own workspace modes or permissions.
   - Artifact ingestion from a running worker is accepted only through internal supervisor coordination using a verified `ExecutionRef`.
3. **Workspace Boundary & Symlink Defense**:
   - All workspace paths are resolved using `filepath.EvalSymlinks`. Any path traversing outside the assigned `workspace_root` is rejected with `ErrInvalidPath`.
   - Workspaces reside outside `state_dir`.
   - Workspace directories are keyed by session/allocation ID (`{run_id}/{session_id}`), ensuring replacement contributors or clean-room reviewers receive distinct, collision-free workspaces.
4. **Sealed Proposal Sets**:
   - Individual worker artifacts remain private to their creating session until a formal release.
   - A proposal set is sealed as an atomic collection of specific artifact revisions. The `proposal_set_digest` is calculated canonically by the service from the sorted tuple set.
5. **Native Auth Preservation**:
   - Provider credentials are not duplicated or managed by Council.
   - Council credentials (`auth.token`, internal socket paths) are strictly excluded from child process environments.

---

## 3. Data Model & Migration v3

### 3.1 Migration v3 Definition
Fresh databases apply `v1 -> v2 -> v3` sequentially. Upgrades from v2 verify the frozen v1 and v2 migration checksums before applying v3.

```sql
-- Migration v3 DDL

-- 1. Frozen Run Inputs and Profiles
CREATE TABLE IF NOT EXISTS run_profiles (
    run_id TEXT NOT NULL PRIMARY KEY REFERENCES runs(run_id) ON DELETE RESTRICT,
    profile_digest TEXT NOT NULL,
    algorithm_version TEXT NOT NULL, -- e.g. 'cprof-v1'
    workspace_mode TEXT NOT NULL CHECK (workspace_mode IN ('none', 'readonly', 'isolated_branch')),
    canonical_profile_json TEXT NOT NULL,
    source_repo_identity TEXT NOT NULL, -- e.g. local path or repo URI
    source_commit TEXT NOT NULL,         -- Git commit SHA or empty for 'none'
    source_tree TEXT NOT NULL,           -- Git tree SHA or empty for 'none'
    brief_artifact_digest TEXT NOT NULL, -- CAS digest of the raw brief text
    created_at TEXT NOT NULL
);

-- 2. Enhanced Artifact Revisions with Provenance and Release Sealing
ALTER TABLE artifact_revisions ADD COLUMN session_id TEXT REFERENCES sessions(session_id);
ALTER TABLE artifact_revisions ADD COLUMN attempt_id TEXT;
ALTER TABLE artifact_revisions ADD COLUMN released INTEGER NOT NULL DEFAULT 0 CHECK (released IN (0, 1));
ALTER TABLE artifact_revisions ADD COLUMN proposal_set_digest TEXT;

-- 3. Proposal Sets Table (Atomic collection of sealed artifact revisions)
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
```

### 3.2 Migration Backfill Policy
- Legacy (v1/v2) runs:
  - Backfilled with a synthetic `run_profiles` record: `workspace_mode = 'isolated_branch'`, `algorithm_version = 'cprof-v1'`, `canonical_profile_json = '{}'`, `source_repo_identity = 'legacy'`, `source_commit = ''`, `source_tree = ''`, `brief_artifact_digest = ''`.
- Legacy artifact revisions:
  - Backfilled as `released = 1`, `proposal_set_digest = 'legacy-proposal-set'`. Existing historical artifacts remain visible to avoid breaking existing runs.

---

## 4. Canonical Profile Encoding & Digest Algorithm

### 4.1 Specification of `CanonicalProfile`
The canonical profile document specifies:
```json
{
  "algo_version": "cprof-v1",
  "workspace_mode": "isolated_branch",
  "code_index_scope": ["cmd/", "internal/"],
  "tooling": ["git", "go", "test"],
  "harnesses": {
    "agy": {
      "model": "gemini-2.5-pro",
      "native_auth_mode": "inherited_host_keychain",
      "extra_env_allowlist": ["GEMINI_CLI_PROFILE"]
    },
    "claude": {
      "model": "claude-3-7-sonnet",
      "native_auth_mode": "inherited_host_keychain",
      "extra_env_allowlist": []
    },
    "codex": {
      "model": "o3-mini",
      "native_auth_mode": "inherited_host_keychain",
      "extra_env_allowlist": []
    },
    "opencode": {
      "model": "glm-4",
      "native_auth_mode": "inherited_host_keychain",
      "extra_env_allowlist": []
    }
  }
}
```

### 4.2 Canonical Normalization Rules (RFC 8785)
1. **Key Ordering**: All object keys sorted lexicographically by UTF-16 code units.
2. **Whitespace**: No whitespace between tokens (`:`, `,`).
3. **Strings**: Normal UTF-8; tool names lowercase; paths normalized (`filepath.Clean`, forward slashes, trailing slashes stripped).
4. **Lists**: `tooling` and `code_index_scope` entries deduplicated and sorted.
5. **Unknown Fields**: Strict JSON decoding with `DisallowUnknownFields`. Any unrecognized field causes immediate rejection with `ErrDisallowedToolingConfig`.
6. **Digest Computation**:
   ```
   profile_digest = "cprof-v1:sha256:" + hex(sha256(canonical_json_bytes))
   ```

---

## 5. Workspace Management & Execution Policy Seam

### 5.1 Root Placement & Path Validation
The operator designates a `workspace_base_dir` (e.g. `/var/run/council-workspaces` or `~/.council-workspaces`), strictly disjoint from `state_dir`.
If `workspace_base_dir` is inside `state_dir` or is a symlink resolving into `state_dir`, service initialization fails immediately.

Worker directory structure:
```
$WORKSPACE_BASE_DIR/
  {run_id}/
    {session_id}/
      scratch/           # Local scratch directory (writable in all modes)
      config/            # Harness configuration directory (private dotfiles/cache)
      source/            # Read-only source checkout (in readonly mode)
      worktree/          # Dedicated git worktree (in isolated_branch mode)
```

Path validation rule:
Every workspace path component is validated against regex `^[a-zA-Z0-9_\-]+$`.
Before executing any tool or operation, the target path is resolved using `filepath.EvalSymlinks`.
```go
if !strings.HasPrefix(resolvedPath, resolvedWorkspaceRoot+string(filepath.Separator)) && resolvedPath != resolvedWorkspaceRoot {
    return ErrInvalidPath
}
```

### 5.2 Workspace Mode Implementation & Enforcement
1. **`none` Mode**:
   - `workspace_root = $WORKSPACE_BASE_DIR/{run_id}/{session_id}/scratch`
   - Repository source is neither linked nor visible.
2. **`readonly` Mode**:
   - `workspace_root = $WORKSPACE_BASE_DIR/{run_id}/{session_id}/source`
   - Source is checked out detached at `source_commit`.
   - **Enforcement**:
     - Linux: Read-only bind mount (`mount --bind -o ro`) when unprivileged user namespaces or root mounts are available; fallback to filesystem permissions `0500` (directories) / `0400` (files).
     - Windows/macOS: Read-only attribute bitmask / ACL lock on the source checkout; scratch directory remains separate and writable.
     - Where read-only enforcement cannot be verified, the execution policy marks `ReadonlyEnforcement: "degraded_file_permissions"` or fails closed if strict isolation was configured.
3. **`isolated_branch` Mode**:
   - `workspace_root = $WORKSPACE_BASE_DIR/{run_id}/{session_id}/worktree`
   - A dedicated Git worktree is created on branch `council/{run_id}/{session_id}` forked from `source_commit`.
   - **Git Command Policy**:
     - Git commands executed through Council tool wrappers enforce branch restriction: `checkout -b`, `push origin main`, and `merge` outside the assigned branch are rejected at the command wrapper level.
     - Worktree removal and pruning are cleanly managed by `WorkspaceManager.CloseSession()`.

### 5.3 Execution Policy Seam (`ValidatedLaunchSpec`)
The adapter layer does not directly execute raw commands. It invokes an `ExecutionPolicy` validator that produces:

```go
type ValidatedLaunchSpec struct {
    Executable          string
    Args                []string
    Cwd                 string            // Validated canonical workspace root
    Env                 map[string]string // Strict allowlist; no Council credentials
    ConfigDir           string            // Isolated config path
    ReadonlyEnforcement string            // "mount", "permissions", "none"
    AllowedPathPrefix   string            // Resolved workspace root prefix
}
```

### 5.4 Environment Allowlist
The child process environment is constructed strictly from an allowlist:
- Standard minimal system variables: `PATH`, `TMPDIR`, `TERM`, `LANG`, `LC_ALL`, `USER`, `HOME` (pointed to worker `config/`).
- Council worker variables: `COUNCIL_WORKSPACE_ROOT`, `COUNCIL_RUN_ID`, `COUNCIL_SESSION_ID`.
- Harness-specific non-secret variables explicitly approved in the canonical profile's `extra_env_allowlist`.
- **Forbidden**: Any variable containing `AUTH_TOKEN`, `COUNCIL_SOCKET`, `COUNCIL_LEASE`, or any variable not in the allowlist.

---

## 6. Service-Owned Worker Artifact Ingestion

### 6.1 Worker Ingestion Flow
Workers do not call HTTP endpoints with controller leases. Instead:
1. When a worker finishes a turn or writes an artifact, the adapter or supervisor calls an internal storage method:
   ```go
   RecordObservedArtifact(ctx context.Context, ref ExecutionRef, name string, content []byte) (ArtifactMetadata, error)
   ```
2. The storage engine verifies `ref` (`SessionID`, `TurnKey`, `AttemptID`) matches the active dispatch intent in SQLite.
3. Content is stored in content-addressed storage, and `artifact_revisions` is inserted with:
   - `session_id = ref.SessionID`
   - `attempt_id = ref.AttemptID`
   - `released = 0`
   - `proposal_set_digest = NULL`
4. The artifact is completely unreleased and private to `ref.SessionID`.

### 6.2 Atomic Proposal Set Sealing (`ReleaseArtifacts`)
When the controller determines that the contribution phase is complete:
1. The controller calls:
   ```go
   ReleaseArtifacts(ctx context.Context, opID string, callerLease string, runID string, members []ProposalMemberRef) (ProposalSetReceipt, error)
   ```
2. **Preconditions**:
   - `callerLease` is verified against current active controller authority.
   - `members` must contain at least one artifact reference and at most one per active contributor.
   - Each `(artifact_id, revision)` must exist, match the author session, and have matching content digest.
3. **Sealing**:
   - Sort members canonically by `(author_contributor, artifact_id, revision)`.
   - Compute `proposal_set_digest = "propset-v1:sha256:" + hex(sha256(canonical_members_bytes))`.
   - In a single SQLite write transaction:
     - Insert into `proposal_sets`.
     - Insert all rows into `proposal_set_members`.
     - Update `artifact_revisions SET released = 1, proposal_set_digest = ? WHERE ...`.
     - Append audit journal record `artifacts_released`.

### 6.3 Visibility Matrix
| Requestor Context | Artifact State | Visibility |
| :--- | :--- | :--- |
| Author Session (Turn Active / Parked) | `released = 0` | **Visible** (can read its own drafts) |
| Sibling Worker (Turn Active) | `released = 0` | **Denied** (`ErrArtifactNotFound`) |
| Peer Reviewer Session | `released = 1` | **Visible** (only artifacts in sealed proposal set) |
| Controller / Operator Inspection | `released = 0` or `1` | **Visible** (redacted audit record) |
| Archived Run | Any | **Frozen** (read-only audit, no new releases) |

---

## 7. Acceptance Evidence Matrix

The test suite must provide explicit, partitioned evidence across three categories:

### 7.1 Storage and Service Authorization Evidence
- **`TestAC005_FrozenProfileImmutability`**:
  - Run created with canonical profile, brief text, and repo identity.
  - Attempting to update or mutate the profile fails with `ErrUnauthorizedOperation`.
  - Initializing a session with a mismatched `profile_digest` or unapproved model/tool is rejected.
- **`TestAC005_WorkerArtifactIngestionRequiresExecutionRef`**:
  - Attempting to ingest an artifact without a valid, active `ExecutionRef` fails.
  - Ingestion does not accept or require a controller lease.
- **`TestAC005_AtomicProposalSetRelease`**:
  - Two workers ingest unreleased artifacts (`released = 0`).
  - Sibling session querying the artifact gets `ErrArtifactNotFound`.
  - `ReleaseArtifacts` binds both artifact revisions, calculates `proposal_set_digest`, and commits visibility in a single transaction.
  - Peer reviewer session can now retrieve the released artifacts.

### 7.2 Controlled Process-Policy & Filesystem Fixtures
- **`TestAC005_WorkspaceBaseDirDisjointFromStateDir`**:
  - Configuring `workspace_base_dir` inside `state_dir` fails startup with an explicit error.
- **`TestAC005_PathTraversalRejection`**:
  - Symlinks pointing out of workspace, `../` relative traversal, and absolute paths outside `workspace_root` return `ErrInvalidPath`.
- **`TestAC005_EnvironmentSanitization`**:
  - Subprocess launch spec strips parent `AUTH_TOKEN`, `COUNCIL_SOCKET`, and unapproved environment variables.
  - Verified with synthetic canary environment secrets.
- **`TestAC005_WorkspaceModesProvisioning`**:
  - `none`: verified that no git repo or source files exist in scratch.
  - `readonly`: verified that source checkout exists and write attempts are blocked.
  - `isolated_branch`: verified that Git worktree is on `council/{run_id}/{session_id}`.
- **`TestAC005_GitBranchPolicyGate`**:
  - Attempting to execute `git checkout -b` or `git push origin main` through Council's Git command policy wrapper fails with a policy rejection.

### 7.3 Real Native & OS Enforcement Boundaries (Documented Disclosures)
- Real OS-level user namespace and seccomp/sandbox-exec isolation is documented as dependent on host platform capabilities and verified in native adapter work (AC-007–AC-010).
- Where OS-level read-only mount enforcement is unavailable (e.g. non-root Windows/Darwin), the degraded mode (file permission bitmask) is explicitly recorded in test reports.

---

## 8. Boundaries and Limitations
- Same-user local processes on Linux/macOS without elevated privileges or namespaces share kernel user ID; Council-mediated execution policy and workspace separation enforce operational hygiene, but do not replace an OS hypervisor.
- Real provider execution requires the respective native adapter implementations (AC-007 through AC-010). Controlled fixtures and simulated harness adapters are used for AC-005 verification.
