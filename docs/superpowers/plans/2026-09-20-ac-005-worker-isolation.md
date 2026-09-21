# AC-005: Freeze Tooling/Source Profiles and Prove Independent Worker Isolation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement frozen profiles, session-keyed workspace allocation, unbypassable execution policy, network proxy gating, lifecycle-tolerant artifact ingestion, and atomic proposal-set sealing with comprehensive denial evidence.

**Architecture:** A deep execution policy module (`internal/adapter/execpolicy`) encapsulates process creation behind `PolicyExecutor.Start`, while `internal/adapter/workspace` manages session-keyed workspaces disjoint from `state_dir`. Profiles, briefs, and sources are frozen with versioned canonical digests in SQLite migration v3. Worker artifact ingestion is supervised with `ExecutionRef` across active and terminal states, and peer review visibility is unlocked atomically by controller-sealed proposal sets.

**Tech Stack:** Go 1.24, SQLite 3 (immediate transactions), RFC 8785 canonical JSON, Git worktrees, Unix domain sockets / local forward proxy.

**Spec:** `docs/superpowers/specs/2026-09-20-ac-005-worker-isolation-design.md`

## Global Constraints

- Never push directly to main, force-push, or auto-merge without explicit operator authorization.
- Initial brief/source/profile are frozen; no sibling access during generation.
- Contributor identity controls voting; stable session/allocation ID controls workspaces.
- Workers cannot acquire controller leases, operator credentials, increase budgets, or loosen permissions.
- Workspaces must reside under `workspace_base_dir`, strictly disjoint from `state_dir`.
- Native authentication stays native; no copied provider tokens or global permission relaxations.
- Fake adapters and controlled fixtures are test utilities, never production fallbacks.
- Unit fixtures do not prove live harness mediation or OS-level isolation. State unverified capabilities explicitly.

## Review Focus

1. **Proxy destination validation**: Must reject all non-public destination classes (loopback `127.0.0.0/8`, IPv4 private RFC 1918 `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, link-local `169.254.0.0/16`, IPv6 private `fc00::/7`, IPv6 link-local `fe80::/10`) to prevent local attack surface traversal.
2. **Redirect validation without TLS interception**: Must validate subsequent destination hops of HTTP redirects or CONNECT requests against the allowlist rather than attempting TLS termination.
3. **Attempt ID binding on terminal artifact ingestion**: Artifact ingestion following a turn reaching terminal status must validate the exact persisted attempt ID and reject obsolete attempts.
4. **Symmetric workspace disjointness**: Both `workspace_base_dir` inside `state_dir` and `state_dir` inside `workspace_base_dir` must be rejected on startup after resolving all symlinks.
5. **Canonical RFC 8785 profile normalization**: Profile digest must use strict JSON decoding, lexicographical key sorting, and lowercase normalized tools/paths so identical semantics always produce identical hashes.

---

### Task 1: Schema Migration v3, Canonical Encoding, and Frozen Input Storage

**Files:**
- Create: `internal/storage/schema_v3.go`
- Create: `internal/storage/canonical_profile.go`
- Modify: `internal/storage/migrations.go`
- Test: `internal/storage/canonical_profile_test.go`
- Test: `internal/storage/migration_v3_test.go`

**Interfaces:**
- Consumes: `schema.sql` (v1), `schemaV2DDL` (v2)
- Produces:
  ```go
  type CanonicalProfile struct {
      AlgoVersion         string                        `json:"algo_version"`
      WorkspaceMode       string                        `json:"workspace_mode"`
      IsolationStrictness string                        `json:"isolation_strictness"`
      NetworkMode         string                        `json:"network_mode"`
      NetworkAllowlist    []string                      `json:"network_allowlist"`
      CodeIndexScope      []string                      `json:"code_index_scope"`
      Tooling             []string                      `json:"tooling"`
      Harnesses           map[string]HarnessProfileSpec `json:"harnesses"`
  }
  func ComputeBriefDigest(brief string) (string, error)
  func ComputeSourceDigest(mode string, repoIdentity, commit, tree string) (string, error)
  func ComputeProfileDigest(profile CanonicalProfile) (string, []byte, error)
  ```

- [ ] **Step 1.1: Write failing test for canonical profile and digest computations**
  Create `internal/storage/canonical_profile_test.go` testing:
  - Exact formula output for `brief_digest` (`cbrief-v1:sha256:...`).
  - Exact formula output for `source_digest` (`csource-v1:none` for `none`; `csource-v1:sha256:...` binding repo, commit, tree).
  - Exact formula output for `profile_digest` (`cprof-v1:sha256:...`) with key reordering and normalization.
  - Rejection of unknown fields (`DisallowUnknownFields`).
  - Rejection of invalid algorithm versions.

- [ ] **Step 1.2: Run test to verify it fails**
  `CGO_ENABLED=0 go test ./internal/storage -run 'TestCanonicalProfile_'`
  Expected: FAIL (types and functions undefined).

- [ ] **Step 1.3: Implement canonical encoding and digest computation**
  Create `internal/storage/canonical_profile.go`:
  - Implement `CanonicalProfile`, `ComputeBriefDigest`, `ComputeSourceDigest`, `ComputeProfileDigest`.
  - Normalization: UTF-8 NFC, trimmed BOM, sorted arrays, deduplication, lowercase tool names, RFC 8785 JSON marshaling.

- [ ] **Step 1.4: Run test to verify it passes**
  `CGO_ENABLED=0 go test ./internal/storage -run 'TestCanonicalProfile_'`
  Expected: PASS.

- [ ] **Step 1.5: Write failing test for Migration v3**
  Create `internal/storage/migration_v3_test.go`:
  - Upgrades a verified v2 database to v3.
  - Verifies `schema_migrations` records version 3.
  - Verifies legacy runs are backfilled with `legacy-unverified`, `none` mode, and original `runs.profile_digest`.
  - Verifies legacy artifact revisions are backfilled with `released = 1` and `proposal_set_digest = NULL`.
  - Verifies repeat open is idempotent.
  - Verifies rollback on injected failure.

- [ ] **Step 1.6: Run test to verify it fails**
  `CGO_ENABLED=0 go test ./internal/storage -run 'TestAC005_MigrationV3_'`
  Expected: FAIL (v3 migration undefined).

- [ ] **Step 1.7: Implement Schema v3 and update migrations**
  Create `internal/storage/schema_v3.go` with `schemaV3DDL` and backfills:
  - Table `run_profiles`.
  - Columns `session_id`, `attempt_id`, `released`, `proposal_set_digest` on `artifact_revisions`.
  - Tables `proposal_sets` and `proposal_set_members`.
  - Update `internal/storage/migrations.go` to set `currentSchemaVersion = 3`, verify v2 checksum, and apply v3 transactionally.

- [ ] **Step 1.8: Run test to verify it passes**
  `CGO_ENABLED=0 go test ./internal/storage -run 'TestAC005_MigrationV3_'`
  Expected: PASS.

- [ ] **Step 1.9: Commit Task 1**
  `git add internal/storage/schema_v3.go internal/storage/canonical_profile.go internal/storage/migrations.go internal/storage/canonical_profile_test.go internal/storage/migration_v3_test.go`
  `git commit -m "feat(storage): schema migration v3 and canonical profile digest encoding"`

---

### Task 2: Workspace Management, Session Allocation, and Symmetric Disjointness

**Files:**
- Create: `internal/adapter/workspace/workspace.go`
- Test: `internal/adapter/workspace/workspace_test.go`

**Interfaces:**
- Consumes: `CanonicalProfile` from `internal/storage`
- Produces:
  ```go
  type WorkspaceManager struct { ... }
  func NewWorkspaceManager(stateDir, workspaceBaseDir string) (*WorkspaceManager, error)
  func (m *WorkspaceManager) AllocateWorkspace(runID, sessionID, mode, sourceRepo, commit string) (WorkspacePaths, error)
  func (m *WorkspaceManager) CloseWorkspace(runID, sessionID string) error
  type WorkspacePaths struct {
      Root    string
      Scratch string
      Config  string
      Source  string
      Worktree string
      Mode    string
  }
  ```

- [ ] **Step 2.1: Write failing test for symmetric disjointness and path validation**
  Create `internal/adapter/workspace/workspace_test.go`:
  - `NewWorkspaceManager` rejects `workspaceBaseDir` inside `stateDir`.
  - `NewWorkspaceManager` rejects `stateDir` inside `workspaceBaseDir`.
  - Rejects invalid identifiers (`run_id` or `session_id` containing `../`, slashes, or special characters).
  - Verifies paths resolve under `workspaceBaseDir` after symlink evaluation.

- [ ] **Step 2.2: Run test to verify it fails**
  `CGO_ENABLED=0 go test ./internal/adapter/workspace -run 'TestWorkspace_'`
  Expected: FAIL (package undefined).

- [ ] **Step 2.3: Implement NewWorkspaceManager and path validation**
  Create `internal/adapter/workspace/workspace.go`:
  - Validate symmetric disjointness with `filepath.EvalSymlinks`.
  - Implement identifier validation regex `^[a-zA-Z0-9_\-]+$`.
  - Implement safe parent resolution and leaf directory creation with mode `0700`.

- [ ] **Step 2.4: Write failing test for the three workspace modes**
  In `internal/adapter/workspace/workspace_test.go`:
  - `none` mode creates `scratch/` and `config/`; no repo files.
  - `readonly` mode creates detached source checkout; verifies file permissions prevent write.
  - `isolated_branch` mode creates Git worktree on `council/{run_id}/{session_id}`.
  - `CloseWorkspace` cleans up worktree and directory tree cleanly.

- [ ] **Step 2.5: Implement AllocateWorkspace and CloseWorkspace for all modes**
  In `internal/adapter/workspace/workspace.go`:
  - Support `none`, `readonly`, and `isolated_branch`.
  - For `isolated_branch`: invoke `git worktree add -b council/{runID}/{sessionID}`.
  - For `CloseWorkspace`: invoke `git worktree remove` and remove leaf directory.

- [ ] **Step 2.6: Run tests to verify they pass**
  `CGO_ENABLED=0 go test ./internal/adapter/workspace -run 'TestWorkspace_'`
  Expected: PASS.

- [ ] **Step 2.7: Commit Task 2**
  `git add internal/adapter/workspace/`
  `git commit -m "feat(adapter): session-keyed workspace manager with symmetric disjointness"`

---

### Task 3: Unbypassable Execution Policy Seam (`PolicyExecutor`)

**Files:**
- Create: `internal/adapter/execpolicy/executor.go`
- Create: `internal/adapter/execpolicy/process.go`
- Test: `internal/adapter/execpolicy/executor_test.go`

**Interfaces:**
- Consumes: `WorkspacePaths` from `internal/adapter/workspace`, `CanonicalProfile` from `internal/storage`
- Produces:
  ```go
  type LaunchRequest struct {
      SessionID         string
      TurnKey           string
      AttemptID         string
      Command           string
      Args              []string
      ExtraEnvAllowlist []string
      Paths             workspace.WorkspacePaths
      Profile           storage.CanonicalProfile
  }
  type PolicyExecutor interface {
      Start(ctx context.Context, req LaunchRequest) (ManagedProcess, error)
  }
  type ManagedProcess interface {
      Stdin() io.Writer
      Stdout() io.Reader
      Stderr() io.Reader
      Wait() (int, error)
      Terminate(ctx context.Context) error
  }
  ```

- [ ] **Step 3.1: Write failing test for PolicyExecutor command and environment policy**
  Create `internal/adapter/execpolicy/executor_test.go`:
  - Pinned `cwd` strictly equals `req.Paths.Root`.
  - Environment allowlist: `PATH`, `TMPDIR`, `TERM`, `LANG`, `LC_ALL`, `USER`, `HOME=req.Paths.Config`, `COUNCIL_WORKSPACE_ROOT`, `COUNCIL_RUN_ID`, `COUNCIL_SESSION_ID`.
  - Rejection of parent secrets: synthetic secret canary variables and `AUTH_TOKEN`, `COUNCIL_SOCKET`, `COUNCIL_LEASE` are stripped.
  - Gating of Git commands: `git checkout -b` or `git push origin main` rejected if Command is git.
  - Enforcement of `isolation_strictness`: `strict` fails closed if requested OS capability is unavailable.

- [ ] **Step 3.2: Run test to verify it fails**
  `CGO_ENABLED=0 go test ./internal/adapter/execpolicy -run 'TestPolicyExecutor_'`
  Expected: FAIL.

- [ ] **Step 3.3: Implement PolicyExecutor and ManagedProcess**
  Create `internal/adapter/execpolicy/executor.go` and `process.go`:
  - Validate launch parameters against canonical profile.
  - Construct sanitized allowlist environment.
  - Implement private validated process construction with `os/exec`.
  - Wrap process in `ManagedProcess` with standard I/O pipes and clean termination.

- [ ] **Step 3.4: Run test to verify it passes**
  `CGO_ENABLED=0 go test ./internal/adapter/execpolicy -run 'TestPolicyExecutor_'`
  Expected: PASS.

- [ ] **Step 3.5: Commit Task 3**
  `git add internal/adapter/execpolicy/executor.go internal/adapter/execpolicy/process.go internal/adapter/execpolicy/executor_test.go`
  `git commit -m "feat(adapter): unbypassable policy executor with environment and directory gating"`

---

### Task 4: Network Policy, Managed Forward Proxy, and Egress Denial

**Files:**
- Create: `internal/adapter/execpolicy/proxy.go`
- Modify: `internal/adapter/execpolicy/executor.go`
- Test: `internal/adapter/execpolicy/proxy_test.go`

**Interfaces:**
- Consumes: `LaunchRequest`, `network_mode`, `network_allowlist` from `storage.CanonicalProfile`
- Produces:
  ```go
  type NetworkProxy struct { ... }
  func StartNetworkProxy(allowlist []string) (*NetworkProxy, error)
  func (p *NetworkProxy) Endpoint() string
  func (p *NetworkProxy) Close() error
  ```

- [ ] **Step 4.1: Write failing test for NetworkProxy allowlist and security checks**
  Create `internal/adapter/execpolicy/proxy_test.go`:
  - Outbound CONNECT to allowlisted host succeeds.
  - Outbound CONNECT to unallowlisted host returns HTTP 403 Forbidden with typed error.
  - Outbound request to private / non-public IPs (loopback `127.0.0.1`, RFC 1918 `10.0.0.1`, `192.168.1.1`, link-local `169.254.169.254`, IPv6 `::1`, `fe80::1`) is blocked by DNS rebinding defense.
  - HTTP redirects to unallowlisted hosts are intercepted and rejected on subsequent hop.
  - In `network_mode: "none"`, all outbound attempts produce immediate failure.

- [ ] **Step 4.2: Run test to verify it fails**
  `CGO_ENABLED=0 go test ./internal/adapter/execpolicy -run 'TestNetworkProxy_'`
  Expected: FAIL.

- [ ] **Step 4.3: Implement NetworkProxy**
  Create `internal/adapter/execpolicy/proxy.go`:
  - Forward proxy implementation handling HTTP requests and HTTPS `CONNECT` tunnels.
  - Host validation against normalized allowlist entries.
  - IP resolution checking to reject private, loopback, and link-local ranges.
  - Inject `HTTP_PROXY` and `HTTPS_PROXY` into `PolicyExecutor` child process environment when `network_mode == "allowlist"`.

- [ ] **Step 4.4: Run test to verify it passes**
  `CGO_ENABLED=0 go test ./internal/adapter/execpolicy -run 'TestNetworkProxy_'`
  Expected: PASS.

- [ ] **Step 4.5: Commit Task 4**
  `git add internal/adapter/execpolicy/proxy.go internal/adapter/execpolicy/executor.go internal/adapter/execpolicy/proxy_test.go`
  `git commit -m "feat(adapter): managed network forward proxy with destination allowlisting and rebinding defense"`

---

### Task 5: Lifecycle-Tolerant Worker Artifact Ingestion and Trusted Reads

**Files:**
- Create: `internal/storage/worker_artifacts.go`
- Modify: `internal/storage/artifact_store.go`
- Test: `internal/storage/worker_artifacts_test.go`

**Interfaces:**
- Consumes: `ExecutionRef` from `internal/storage`
- Produces:
  ```go
  func (s *Store) RecordObservedArtifact(ctx context.Context, ref ExecutionRef, name string, content []byte) (ArtifactMetadata, error)
  func (s *Store) ReadAuthorArtifact(ctx context.Context, ref ExecutionRef, artifactID string) ([]byte, ArtifactMetadata, error)
  func (s *Store) ReadReleasedArtifact(ctx context.Context, reviewerSessionID string, proposalSetDigest string, artifactID string) ([]byte, ArtifactMetadata, error)
  ```

- [ ] **Step 5.1: Write failing test for RecordObservedArtifact and trusted reads**
  Create `internal/storage/worker_artifacts_test.go`:
  - `RecordObservedArtifact` succeeds during active turn execution.
  - `RecordObservedArtifact` succeeds immediately after terminal turn completion with matching `ExecutionRef`.
  - `RecordObservedArtifact` rejects mismatched `AttemptID`.
  - Duplicate ingestion is idempotent; conflicting digest returns `ErrConflictingArtifact`.
  - Author session calling `ReadAuthorArtifact` retrieves draft artifact.
  - Sibling session calling `ReadAuthorArtifact` or `ReadReleasedArtifact` before release receives `ErrArtifactNotFound`.

- [ ] **Step 5.2: Run test to verify it fails**
  `CGO_ENABLED=0 go test ./internal/storage -run 'TestWorkerArtifacts_'`
  Expected: FAIL.

- [ ] **Step 5.3: Implement RecordObservedArtifact and trusted reads**
  Create `internal/storage/worker_artifacts.go`:
  - Validate `ExecutionRef` inside write transaction against SQLite `turns` and `dispatch_intents`.
  - Save content in content-addressed storage.
  - Insert into `artifact_revisions` with `released = 0`, `proposal_set_digest = NULL`, `session_id = ref.SessionID`, `attempt_id = ref.AttemptID`.
  - Implement `ReadAuthorArtifact` verifying `session_id == ref.SessionID`.
  - Implement `ReadReleasedArtifact` verifying `released = 1` and membership in `proposal_set_members`.

- [ ] **Step 5.4: Run test to verify it passes**
  `CGO_ENABLED=0 go test ./internal/storage -run 'TestWorkerArtifacts_'`
  Expected: PASS.

- [ ] **Step 5.5: Commit Task 5**
  `git add internal/storage/worker_artifacts.go internal/storage/artifact_store.go internal/storage/worker_artifacts_test.go`
  `git commit -m "feat(storage): lifecycle-tolerant worker artifact ingestion and trusted read interfaces"`

---

### Task 6: Atomic Proposal Set Sealing and Administration Endpoint

**Files:**
- Create: `internal/storage/proposal_sets.go`
- Modify: `internal/service/controller_admin.go`
- Modify: `internal/service/server.go`
- Test: `internal/storage/proposal_sets_test.go`
- Test: `internal/service/controller_admin_proposal_test.go`

**Interfaces:**
- Consumes: `ProposalMemberRef`, `Store.ReleaseArtifacts`
- Produces:
  ```go
  type ProposalMemberRef struct {
      ArtifactID string `json:"artifact_id"`
      Revision   int64  `json:"revision"`
      Digest     string `json:"digest"`
  }
  type ProposalSetReceipt struct {
      ProposalSetDigest string    `json:"proposal_set_digest"`
      RunID             string    `json:"run_id"`
      SealedAt          time.Time `json:"sealed_at"`
  }
  func (s *Store) ReleaseArtifacts(ctx context.Context, opID string, callerLease string, runID string, members []ProposalMemberRef) (ProposalSetReceipt, error)
  ```
  HTTP Route: `POST /v1/runs/{run_id}/artifacts/release`

- [ ] **Step 6.1: Write failing test for ReleaseArtifacts atomic sealing**
  Create `internal/storage/proposal_sets_test.go`:
  - Controller lease verified; invalid or superseded lease fails.
  - Rejects missing, uncommitted, or digest-mismatched artifact revision members.
  - Canonically sorts members, computes `proposal_set_digest = "propset-v1:sha256:..."`.
  - Atomically records `proposal_sets`, `proposal_set_members`, and updates `artifact_revisions.released = 1`.
  - Verifies peer reviewer can now read released artifacts using `ReadReleasedArtifact`.

- [ ] **Step 6.2: Run test to verify it fails**
  `CGO_ENABLED=0 go test ./internal/storage -run 'TestProposalSets_'`
  Expected: FAIL.

- [ ] **Step 6.3: Implement ReleaseArtifacts in storage**
  Create `internal/storage/proposal_sets.go`:
  - Authorize active controller lease.
  - Validate member revisions.
  - Compute canonical proposal-set digest.
  - Atomic write transaction with journal entry `artifacts_released`.

- [ ] **Step 6.4: Run test to verify it passes**
  `CGO_ENABLED=0 go test ./internal/storage -run 'TestProposalSets_'`
  Expected: PASS.

- [ ] **Step 6.5: Write failing test for POST /v1/runs/{run_id}/artifacts/release**
  Create `internal/service/controller_admin_proposal_test.go`:
  - Operator transport bearer auth required (401 on missing/lease-as-bearer).
  - Controller lease required in payload.
  - Successfully seals proposal set and returns `ProposalSetReceipt`.

- [ ] **Step 6.6: Implement HTTP endpoint handleReleaseArtifacts**
  Modify `internal/service/controller_admin.go` and `server.go`:
  - Register `POST /v1/runs/{run_id}/artifacts/release`.
  - Validate request payload, invoke `store.ReleaseArtifacts`, return receipt.

- [ ] **Step 6.7: Run test to verify it passes**
  `CGO_ENABLED=0 go test ./internal/service -run 'TestAdminReleaseArtifacts_'`
  Expected: PASS.

- [ ] **Step 6.8: Commit Task 6**
  `git add internal/storage/proposal_sets.go internal/service/controller_admin.go internal/service/server.go internal/storage/proposal_sets_test.go internal/service/controller_admin_proposal_test.go`
  `git commit -m "feat(service,storage): atomic proposal set release endpoint and sealing transactions"`

---

### Task 7: Full Acceptance Matrix, Controlled Fixtures, and Cross-Platform Verification

**Files:**
- Create: `internal/client/isolation_story_test.go`
- Test: full repository test suite

**Steps:**
- [ ] **Step 7.1: Implement end-to-end acceptance story through intended interfaces**
  Create `internal/client/isolation_story_test.go`:
  - Creates run with frozen inputs (`cbrief-v1`, `csource-v1`, `cprof-v1`).
  - Provisions session workspaces across `none`, `readonly`, and `isolated_branch` modes.
  - Exercises `PolicyExecutor` launching simulated workers with environment allowlists.
  - Exercises `NetworkProxy` blocking unallowlisted domains and non-public IP ranges.
  - Ingests worker proposals with `ExecutionRef`.
  - Controller seals proposal set via `POST .../artifacts/release`.
  - Peer reviewers read released artifacts.
  - Sibling cross-directory reads, nested git branch attempts, and unsealed artifact access are explicitly denied and verified.
  - Controlled fixtures throughout, labeled `..._ControlledFixture`.

- [ ] **Step 7.2: Run Gate 2 verification suite**
  ```bash
  gofmt -l cmd internal
  go vet ./...
  CGO_ENABLED=0 go test ./... -count=1
  go test -race ./... -count=1
  GOOS=windows CGO_ENABLED=0 go vet ./... && GOOS=windows CGO_ENABLED=0 go test -c -o /dev/null ./internal/service/ ./cmd/council/
  GOOS=darwin  CGO_ENABLED=0 go vet ./... && GOOS=darwin  CGO_ENABLED=0 go test -c -o /dev/null ./internal/service/
  ```

- [ ] **Step 7.3: Commit Task 7**
  `git add internal/client/isolation_story_test.go`
  `git commit -m "test(client): complete AC-005 worker isolation acceptance story and verification matrix"`

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-09-20-ac-005-worker-isolation.md`. Please review the plan. Which execution approach would you prefer?

- **Subagent-driven** — A fresh subagent implements each task and a fresh reviewer checks it before the next one starts, then a whole-branch review at the end. Most thorough; costs a fresh context per task and per review.
- **Native** — I implement every task myself in this session, the way this harness runs work, then one fresh reviewer on the most capable model checks the whole branch. Cheapest and fastest; no independent review until the end. Runs well with a mid-tier session model, since the plan carries the design.

For this plan I recommend **Native**, because the tasks build directly on each other's storage and adapter seams in a single repository with clear interfaces, and inline execution with full-test verification will converge quickly.

Does the plan capture what you want, and which approach should we use?
