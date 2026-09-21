# AC-005: Freeze Tooling/Source Profiles and Prove Independent Worker Isolation — Design

- **Status:** Approved
- **Date:** 2026-09-20
- **Issue:** [#5 (AC-005)](https://github.com/CtrlCarlitos/agent-council/issues/5)
- **Dependencies:** AC-001 (#1, closed), AC-002 (#2, closed), AC-003 (#3, closed), AC-004 (#4, closed)
- **Target Packages:** `internal/council`, `internal/storage`, `internal/adapter`, `internal/service`, `internal/client`

---

## 1. Problem and Scope

Equal skill catalogs do not guarantee the same effective toolkit, and bare Git worktrees do not prevent sibling reads, shared indexes, network leakage, or credential disclosure across council workers.

AC-005 establishes the operational contract for immutable run profiles, isolated execution workspaces, unbypassable launch policies, service-owned artifact ingestion, and sealed proposal release before native contributor adapters (AC-007 through AC-010) are wired.

### In Scope:
1. **Profile, Brief, and Source Freezing**:
   - Storage and verification of the immutable inputs: raw brief text, canonical profile document, and source repository identity binding repo location, commit SHA, and tree SHA.
   - Deterministic canonical encoding (RFC 8785) with versioned digest prefixes for all three inputs: `brief_digest`, `source_digest`, and `profile_digest`.
2. **Session-Keyed Workspaces & Symmetric Disjointness**:
   - Workspace directories keyed by `{run_id}/{session_id}` (supporting replacement sessions and clean-room reviewers without collisions).
   - Operator-configured `workspace_base_dir` verified as strictly disjoint from `state_dir` in both directions: `state_dir` cannot reside inside `workspace_base_dir`, nor `workspace_base_dir` inside `state_dir`.
   - Three operational modes:
     - `none`: scratch directory for non-code deliberations; no repository access.
     - `readonly`: concrete read-only repository tree with platform-validated enforcement.
     - `isolated_branch`: single dedicated Git worktree on assigned branch `council/{run_id}/{session_id}`.
   - Symlink boundary validation and safe directory provisioning under the documented non-hostile-user threat model.
3. **Unbypassable Execution Policy Seam (`PolicyExecutor`)**:
   - A deep execution-policy module (`internal/adapter/execpolicy`) where process launching is encapsulated behind `PolicyExecutor.Start(ctx, LaunchRequest)`.
   - `Command`, `Args`, and `ExtraEnvAllowlist` are generated solely by trusted adapter code and checked against the frozen profile; untrusted model/worker output cannot populate them.
   - Private validated launch representation; production adapters cannot bypass policy or invoke `os/exec` directly.
   - Explicit `isolation_strictness` setting (`strict` vs `permissive_dev`) controlling whether degraded host isolation fails closed.
4. **Network Policy & Denial Evidence**:
   - Declarative network policy in profiles (`none`, `allowlist`, `unrestricted`).
   - Sibling and foreign endpoint isolation; managed transport proxy with DNS rebinding defenses, redirect controls, and OS-level network isolation.
   - Explicit denial evidence in isolation test fixtures.
5. **Worker Authority & Lifecycle-Tolerant Artifact Ingestion**:
   - Workers **never** hold or receive controller leases or operator bearer tokens.
   - Artifact creation is internal to the service supervisor and authorized via `ExecutionRef{SessionID, TurnKey, AttemptID}`.
   - Ingestion is valid while turns are active or upon terminal completion, matching AC-004's accepted-execution persistence rules.
   - Trusted internal read interfaces enforce author vs. peer reviewer visibility without allowing arbitrary caller-authored context.
6. **Atomic Sealing of Artifact Revisions**:
   - `ReleaseArtifacts` binds an explicit sorted set of `(artifact_id, revision, digest, author_session_id)` tuples.
   - Computes an authoritative `proposal_set_digest`. Atomic transaction transitions visibility from unreleased (`released = 0`) to sealed/released (`released = 1`).
7. **Native Authentication vs. Config Isolation**:
   - Opaque `NativeAuthMode` per harness (`inherited_host_keychain`, `harness_login_delegation`, `none`).
   - Minimal allowlist-based environment sanitization; synthetic secrets test verification.

### Out of Scope:
- Native harness CLI subprocess orchestration for specific commercial providers (OpenCode, Claude, Codex, Agy are implemented in AC-007–AC-010).
- Ballot tallying, preference scoring, and synthesis adjudication (AC-013).
- Token usage quotas, process timeouts, and financial budget accounting (AC-014).
- Hostile multi-tenant kernel virtualization (Docker/VM). AC-005 enforces local host-user runtime, filesystem path, and API isolation.

---

## 2. Invariants & Security Architecture

1. **Immutable Triad**:
   - Every run is permanently bound to `brief_digest`, `source_digest`, and `profile_digest`.
   - Underlying brief text and canonical profile JSON are stored permanently in content-addressed storage or database; initialization validates digests against stored artifacts.
2. **Strict Authority Separation**:
   - Worker processes run without operator tokens (`auth.token`) or controller leases.
   - Workers cannot query administration routes, cannot call `PublishArtifact` as a controller, and cannot alter their own workspace modes or permissions.
   - Artifact ingestion from a running worker is accepted only through internal supervisor coordination using a verified `ExecutionRef`.
3. **Workspace Boundary & Symlink Defense**:
   - Symmetric root disjointness: `state_dir` and `workspace_base_dir` can never overlap or enclose one another.
   - Workspace directories are keyed by session/allocation ID (`{run_id}/{session_id}`), ensuring replacement contributors or clean-room reviewers receive distinct, collision-free workspaces.
4. **Unbypassable Seam**:
   - Production adapters cannot instantiate raw subprocesses. All launches pass through `PolicyExecutor.Start`, which validates constraints, applies environment allowlists, and enforces directory pinning.
   - Launch parameters (`Command`, `Args`, `ExtraEnvAllowlist`) are constructed exclusively by trusted Go adapter code and checked against the canonical profile.
5. **Sealed Proposal Sets**:
   - Individual worker artifacts remain private to their creating session until a formal release.
   - A proposal set is sealed as an atomic collection of specific artifact revisions. The `proposal_set_digest` is calculated canonically by the service from the sorted tuple set.
6. **Native Auth Preservation**:
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
    algorithm_version TEXT NOT NULL, -- e.g. 'cprof-v1' or 'legacy-unverified'
    workspace_mode TEXT NOT NULL CHECK (workspace_mode IN ('none', 'readonly', 'isolated_branch')),
    isolation_strictness TEXT NOT NULL CHECK (isolation_strictness IN ('strict', 'permissive_dev')),
    network_mode TEXT NOT NULL CHECK (network_mode IN ('none', 'allowlist', 'unrestricted')),
    canonical_profile_json TEXT NOT NULL,
    source_repo_identity TEXT NOT NULL, -- normalized path or URI
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

### 3.2 Migration Backfill Policy (Honest Legacy State)
- Legacy (v1/v2) runs:
  - Backfilled with explicit unverified status:
    - `algorithm_version = 'legacy-unverified'`
    - `profile_digest = runs.profile_digest` (preserves existing record without fabricating a new digest)
    - `workspace_mode = 'none'`
    - `isolation_strictness = 'permissive_dev'`
    - `network_mode = 'unrestricted'`
    - `canonical_profile_json = ''`
    - `source_repo_identity = 'legacy'`
    - `source_commit = ''`
    - `source_tree = ''`
    - `brief_artifact_digest = ''`
  - Legacy rows cannot be used to provision new worktrees or claim isolation guarantees.
- Legacy artifact revisions:
  - Backfilled with `released = 1`, `proposal_set_digest = NULL`.
  - No synthetic records are inserted into `proposal_sets` or `proposal_set_members`. Existing historical artifacts remain readable via general inspection without claiming sealed proposal status.

---

## 4. Canonical Encoding & Versioned Digest Algorithms

### 4.1 Brief Digest (`brief_digest`)
- **Canonical Input**: Raw UTF-8 bytes of the brief text, trimmed of leading/trailing Unicode byte-order marks (BOM) and normalized to UTF-8 NFC.
- **Formula**:
  ```
  brief_digest = "cbrief-v1:sha256:" + hex(sha256(canonical_brief_bytes))
  ```

### 4.2 Source Digest (`source_digest`)
- **For `workspace_mode == "none"`**:
  ```
  source_digest = "csource-v1:none"
  ```
- **For `readonly` and `isolated_branch`**:
  The canonical source identity document specifies:
  ```json
  {
    "algo_version": "csource-v1",
    "commit": "40-character-hex-commit-sha",
    "repo_identity": "normalized-canonical-path-or-uri",
    "tree": "40-character-hex-tree-sha"
  }
  ```
  - `repo_identity`: Normalized canonical local path (`filepath.Clean`) or canonical remote URI (`https://...` or `git@...`).
  - `commit`: 40-character lowercase hexadecimal Git commit hash, verified to exist in the repository.
  - `tree`: 40-character lowercase hexadecimal Git tree hash corresponding to `commit`.
  - Keys sorted lexicographically; no extraneous whitespace (RFC 8785).
  - **Formula**:
    ```
    source_digest = "csource-v1:sha256:" + hex(sha256(canonical_source_json_bytes))
    ```

### 4.3 Profile Digest (`profile_digest`)
- **Canonical Document**:
  ```json
  {
    "algo_version": "cprof-v1",
    "code_index_scope": ["cmd/", "internal/"],
    "harnesses": {
      "agy": {
        "extra_env_allowlist": ["GEMINI_CLI_PROFILE"],
        "model": "gemini-2.5-pro",
        "native_auth_mode": "inherited_host_keychain"
      },
      "claude": {
        "extra_env_allowlist": [],
        "model": "claude-3-7-sonnet",
        "native_auth_mode": "inherited_host_keychain"
      },
      "codex": {
        "extra_env_allowlist": [],
        "model": "o3-mini",
        "native_auth_mode": "inherited_host_keychain"
      },
      "opencode": {
        "extra_env_allowlist": [],
        "model": "glm-4",
        "native_auth_mode": "inherited_host_keychain"
      }
    },
    "isolation_strictness": "strict",
    "network_allowlist": ["api.anthropic.com:443", "api.openai.com:443"],
    "network_mode": "allowlist",
    "tooling": ["git", "go", "test"],
    "workspace_mode": "isolated_branch"
  }
  ```
- **Normalization Rules (RFC 8785)**:
  1. Object keys sorted lexicographically by UTF-16 code units.
  2. No whitespace between tokens (`:`, `,`).
  3. Strings: UTF-8 NFC; tool names lowercase; paths normalized (`filepath.Clean`, forward slashes, trailing slashes stripped).
  4. Lists: `tooling`, `network_allowlist`, and `code_index_scope` deduplicated and sorted.
  5. Unknown fields rejected with `DisallowUnknownFields`.
  - **Formula**:
    ```
    profile_digest = "cprof-v1:sha256:" + hex(sha256(canonical_profile_json_bytes))
    ```

---

## 5. Workspace Management & Unbypassable Execution Seam

### 5.1 Symmetric Disjointness & Path Validation
The operator designates `workspace_base_dir`, completely separate from `state_dir`.
Initialization symmetrically evaluates real paths:
```go
realStateDir, err1 := filepath.EvalSymlinks(stateDir)
realWorkspaceBase, err2 := filepath.EvalSymlinks(workspaceBaseDir)
if strings.HasPrefix(realWorkspaceBase, realStateDir+string(filepath.Separator)) || realWorkspaceBase == realStateDir ||
   strings.HasPrefix(realStateDir, realWorkspaceBase+string(filepath.Separator)) || realStateDir == realWorkspaceBase {
    return ErrWorkspaceStateOverlap
}
```

### 5.2 Identifier Validation & Directory Provisioning
- Runtime identifiers (`run_id`, `session_id`, `turn_key`) must strictly match `^[a-zA-Z0-9_\-]+$`.
- Workspace directory layout:
  ```
  $WORKSPACE_BASE_DIR/
    {run_id}/
      {session_id}/
        scratch/
        config/
        source/
        worktree/
  ```
- **Symlink Defense & Threat Model**:
  - The nearest existing ancestor of `$WORKSPACE_BASE_DIR/{run_id}/{session_id}` is validated via `EvalSymlinks` before creation.
  - Directories are created with `os.MkdirAll` (mode `0700`).
  - The resulting leaf directory is verified via `EvalSymlinks` to confirm it strictly resolves beneath `realWorkspaceBase`.
  - *Threat Model Disclosure*: Under a same-user model without OS container isolation, symlink validation is best-effort against accidental traversal and misconfiguration. Protection against an actively malicious same-user adversary manipulating parent directories concurrently is deferred to OS-level containerization.

### 5.3 Workspace Modes
1. **`none` Mode**:
   - `workspace_root = scratch/`
   - Repository source is neither linked nor visible.
2. **`readonly` Mode**:
   - `workspace_root = source/`
   - Source is checked out detached at `source_commit`.
   - **Enforcement**:
     - Linux: Read-only bind mount (`mount --bind -o ro`) when user namespaces are available.
     - Fallback / Windows / macOS: Read-only file permission bitmask (`0500`/`0400`) / ACL lock on source checkout.
     - If `isolation_strictness == "strict"` and mount isolation is unavailable, launch fails closed with `ErrUnsupportedIsolationCapability`.
3. **`isolated_branch` Mode**:
   - `workspace_root = worktree/`
   - Dedicated Git worktree on branch `council/{run_id}/{session_id}` forked from `source_commit`.
   - Council-mediated Git tools enforce branch boundary: attempts to branch or push outside `council/{run_id}/{session_id}` are blocked.

### 5.4 Network Policy & Managed Transport Proxy
1. **Modes**:
   - `"none"`: Direct socket creation blocked; no outbound proxy configured.
   - `"allowlist"`: Direct socket creation blocked; outbound HTTP/HTTPS traffic must route through a Council-managed local forward proxy.
   - `"unrestricted"`: Outbound network unconstrained.
2. **Managed Forward Proxy Contract (`network_mode == "allowlist"`)**:
   - **Entry Format**: Canonical `<host>[:<port>]` (e.g. `api.anthropic.com:443`, `api.openai.com:443`). Subdomains permitted only if explicitly formatted as `*.domain.com`.
   - **Proxy Gating**:
     - Every `CONNECT` and HTTP request is intercepted and validated against the allowlist. Unlisted hosts return HTTP 403 Forbidden.
     - **Redirect Control**: HTTP redirects (`301`, `302`, `307`, `308`) are not followed automatically without validating the `Location` header against the allowlist.
     - **DNS Rebinding Defense**: The proxy resolves DNS internally and rejects addresses mapping to loopback or RFC 1918 private subnets (`127.0.0.0/8`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `::1`).
3. **Strict Mode Enforcement**:
   - Under `isolation_strictness == "strict"`, direct raw socket creation must be blocked by the OS (e.g., network namespaces on Linux) so traffic cannot bypass the proxy.
   - If OS network isolation is unavailable on the host and strict mode is requested, launch fails with `ErrUnsupportedIsolationCapability`.

### 5.5 Unbypassable Execution Policy Seam (`PolicyExecutor`)
Production adapters do not call `os/exec` or receive raw launch structs. They invoke:

```go
type LaunchRequest struct {
    SessionID           string
    TurnKey             string
    AttemptID           string
    Command             string
    Args                []string
    ExtraEnvAllowlist   []string
}

type PolicyExecutor interface {
    Start(ctx context.Context, req LaunchRequest) (ManagedProcess, error)
}

type ManagedProcess interface {
    Stdin() io.Writer
    Stdout() io.Reader
    Stderr() io.Reader
    Wait() (ProcessExit, error)
    Terminate(ctx context.Context) error
}
```

- **Trusted Input Generation**: `Command`, `Args`, and `ExtraEnvAllowlist` are constructed solely by trusted Go adapter code and checked against the frozen profile. No untrusted worker or model output may populate executable paths, command arguments, or environment variables.
- `PolicyExecutor` enforces:
  - Working directory pinned to validated canonical `workspace_root`.
  - Environment allowlist:
    - Base: `PATH`, `TMPDIR`, `TERM`, `LANG`, `LC_ALL`, `USER`, `HOME` (set to `config/`).
    - Worker: `COUNCIL_WORKSPACE_ROOT`, `COUNCIL_RUN_ID`, `COUNCIL_SESSION_ID`.
    - Profile-approved `extra_env_allowlist`.
    - Proxy configuration: `HTTP_PROXY`, `HTTPS_PROXY` (when `network_mode == "allowlist"`).
    - Hard rejection of any variable containing `AUTH_TOKEN`, `COUNCIL_SOCKET`, `COUNCIL_LEASE`.

---

## 6. Service-Owned Worker Artifact Ingestion & Visibility

### 6.1 Lifecycle-Tolerant Ingestion (`RecordObservedArtifact`)
Matching AC-004's accepted-execution authority:
```go
func (s *Store) RecordObservedArtifact(
    ctx context.Context, 
    ref ExecutionRef, 
    name string, 
    content []byte,
) (ArtifactMetadata, error)
```
1. **Attempt Validation**:
   - Validates `ref` (`SessionID`, `TurnKey`, `AttemptID`) matches the persisted turn attempt in `turns` and `dispatch_intents`.
   - Valid while the turn is `running` OR terminal (`completed`, `failed`, `cancelling`, `cancelled`, `interrupted`).
   - Rejects mismatched `AttemptID` with `ErrInvalidAttempt`.
2. **Idempotency**:
   - Exact duplicate `(ref, name, content)` returns existing metadata.
   - Conflicting content under the same `(ref, name)` returns `ErrConflictingArtifact`.
3. **Storage**:
   - Stored in CAS and `artifact_revisions` with `released = 0`, `proposal_set_digest = NULL`, `session_id = ref.SessionID`, `attempt_id = ref.AttemptID`.

### 6.2 Atomic Proposal Set Sealing (`ReleaseArtifacts`)
Controller invokes `ReleaseArtifacts` with explicit member references:
```go
type ProposalMemberRef struct {
    ArtifactID string `json:"artifact_id"`
    Revision   int64  `json:"revision"`
    Digest     string `json:"digest"`
}
```
1. **Preconditions**:
   - Caller lease validates current active controller authority.
   - Each referenced `(artifact_id, revision)` must exist and match `Digest`.
   - Exactly one proposal member per contributing session (or designated panel).
2. **Sealing**:
   - Members sorted canonically by `(author_contributor, artifact_id, revision)`.
   - Compute `proposal_set_digest = "propset-v1:sha256:" + hex(sha256(canonical_members_bytes))`.
   - In a single SQLite write transaction:
     - Insert into `proposal_sets`.
     - Insert all rows into `proposal_set_members`.
     - Update `artifact_revisions SET released = 1, proposal_set_digest = ? WHERE ...`.
     - Append audit journal record `artifacts_released`.

### 6.3 Trusted Internal Read Authority
Artifact reading is partitioned by trusted interfaces:
```go
// Author session reading its own drafts
func (s *Store) ReadAuthorArtifact(ctx context.Context, ref ExecutionRef, artifactID string) ([]byte, ArtifactMetadata, error)

// Peer reviewer reading released proposal set members
func (s *Store) ReadReleasedArtifact(ctx context.Context, reviewerSessionID string, proposalSetDigest string, artifactID string) ([]byte, ArtifactMetadata, error)
```
- `ReadAuthorArtifact` verifies `artifact_revisions.session_id == ref.SessionID`.
- `ReadReleasedArtifact` verifies `released = 1` AND row exists in `proposal_set_members` for `proposalSetDigest`.
- Sibling attempts to read unreleased artifacts fail with `ErrArtifactNotFound`.

---

## 7. Acceptance Evidence Matrix

### 7.1 Storage and Service Authorization Evidence
- **`TestAC005_FrozenProfileImmutability`**:
  - Run created with canonical profile, brief text, and repo identity.
  - Verifies exact computation of `brief_digest` (`cbrief-v1:...`), `source_digest` (`csource-v1:...`), and `profile_digest` (`cprof-v1:...`).
  - Attempting to update or mutate profile fields fails.
  - Mismatched digest or unapproved profile fails session initialization.
- **`TestAC005_WorkerArtifactIngestionMatchesExecutionRef`**:
  - Ingestion succeeds during `running` turn and immediately after terminal completion with matching `ExecutionRef`.
  - Ingestion fails with wrong `AttemptID`.
  - Re-ingesting matching artifact is idempotent; conflicting content returns error.
- **`TestAC005_AtomicProposalSetSealing`**:
  - Workers ingest unreleased drafts (`released = 0`).
  - Sibling session calling `ReadAuthorArtifact` or `ReadReleasedArtifact` gets `ErrArtifactNotFound`.
  - `ReleaseArtifacts` calculates `proposal_set_digest`, writes membership, and updates visibility atomically.
  - Peer reviewer calling `ReadReleasedArtifact` successfully retrieves content.
- **`TestAC005_MigrationV3_HonestLegacyBackfill`**:
  - Verified v2 database upgraded to v3.
  - Legacy runs marked `legacy-unverified` with `workspace_mode = 'none'`.
  - Legacy artifacts marked `released = 1` with `proposal_set_digest = NULL`.

### 7.2 Controlled Process-Policy & Filesystem Fixtures
- **`TestAC005_SymmetricWorkspaceDisjointness`**:
  - `workspace_base_dir` inside `state_dir` fails startup.
  - `state_dir` inside `workspace_base_dir` fails startup.
  - Sibling path traversal (`../{sibling_session}`) fails path resolution.
- **`TestAC005_PolicyExecutor_UnbypassableSeam`**:
  - Process launch via `PolicyExecutor` pins `cwd` to `workspace_root`.
  - Environment allowlist verified with synthetic canary secrets; Council tokens excluded.
  - Verifies launch inputs are generated by trusted adapter code; direct invocation bypassing `PolicyExecutor` is impossible.
- **`TestAC005_NetworkPolicyDenial`**:
  - Subprocess under `network_mode: "none"` produces explicit denial when attempting socket connection.
  - Managed forward proxy rejects unallowlisted destinations, blocks redirects to unallowlisted URLs, and prevents DNS rebinding to private IPs.
- **`TestAC005_WorkspaceModesProvisioning`**:
  - `none`: verified no Git repository or source files in `scratch/`.
  - `readonly`: verified source checkout exists; write operations fail.
  - `isolated_branch`: verified worktree exists on branch `council/{run_id}/{session_id}`.

### 7.3 Real Native & OS Enforcement Boundaries (Documented Disclosures)
- Real OS-level network namespaces and bind mounts require supported Linux host environments; where unavailable, behavior depends on `isolation_strictness` (`strict` fails closed; `permissive_dev` logs degradation in audit journal).
- Real provider execution arrives in AC-007–AC-010; AC-005 proves the policy engine, storage boundaries, and workspace contracts via controlled fixtures.

---

## 8. Boundaries and Limitations
- Same-user local processes on Linux/macOS without elevated privileges or user namespaces share kernel UID; Council-mediated execution policy and workspace separation enforce operational hygiene, but do not replace an OS hypervisor.
- Real provider execution requires the respective native adapter implementations (AC-007 through AC-010). Controlled fixtures and simulated harness adapters are used for AC-005 verification.
