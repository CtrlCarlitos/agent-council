package storage_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func setupProposalTestFixture(t *testing.T, lease string) (*storage.Store, string, storage.ExecutionRef, storage.ExecutionRef) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	runID := "run-propset-test"
	if _, err := store.CreateRun(ctx, "op-run-1", runID, "brief-sha", "src-sha", "prof-sha", "bootstrap-"+lease); err != nil {
		t.Fatalf("create run: %v", err)
	}
	// Author session
	_, err = store.CreateSession(ctx, "op-sess-author", "bootstrap-"+lease, storage.SessionRecord{
		ID: "sess-author", RunID: runID, Contributor: "claude", Role: "worker",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create author session: %v", err)
	}
	// Sibling session
	_, err = store.CreateSession(ctx, "op-sess-sibling", "bootstrap-"+lease, storage.SessionRecord{
		ID: "sess-sibling", RunID: runID, Contributor: "codex", Role: "worker",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create sibling session: %v", err)
	}

	if _, err := store.AdoptController(ctx, "op-adopt", runID, "claude", "ctrl-ref", "bootstrap-"+lease, nil, lease); err != nil {
		t.Fatalf("adopt controller: %v", err)
	}
	if _, err := store.ConnectRunController(ctx, "op-conn", runID, lease, 1, "inst-1"); err != nil {
		t.Fatalf("connect controller: %v", err)
	}

	// Release turn for author session
	ver, err := store.GetSessionVersion(ctx, "sess-author")
	if err != nil {
		t.Fatalf("get author session version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-auth", lease, "sess-author", ver, storage.PendingPrompt{
		SessionID: "sess-author", TurnKey: "turn-auth-1", Prompt: "work on task",
	}); err != nil {
		t.Fatalf("queue author prompt: %v", err)
	}
	ver, _ = store.GetSessionVersion(ctx, "sess-author")
	relAuth, err := store.ReleaseTurn(ctx, "op-rel-auth", lease, "sess-author", ver, "turn-auth-1")
	if err != nil {
		t.Fatalf("release author turn: %v", err)
	}
	authorRef := storage.ExecutionRef{
		SessionID: "sess-author",
		TurnKey:   "turn-auth-1",
		AttemptID: relAuth.Receipt.AttemptID,
	}

	// Release turn for sibling session
	sibVer, err := store.GetSessionVersion(ctx, "sess-sibling")
	if err != nil {
		t.Fatalf("get sibling session version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-sib", lease, "sess-sibling", sibVer, storage.PendingPrompt{
		SessionID: "sess-sibling", TurnKey: "turn-sib-1", Prompt: "work on sibling task",
	}); err != nil {
		t.Fatalf("queue sibling prompt: %v", err)
	}
	sibVer, _ = store.GetSessionVersion(ctx, "sess-sibling")
	relSib, err := store.ReleaseTurn(ctx, "op-rel-sib", lease, "sess-sibling", sibVer, "turn-sib-1")
	if err != nil {
		t.Fatalf("release sibling turn: %v", err)
	}
	siblingRef := storage.ExecutionRef{
		SessionID: "sess-sibling",
		TurnKey:   "turn-sib-1",
		AttemptID: relSib.Receipt.AttemptID,
	}

	return store, runID, authorRef, siblingRef
}

func TestProposalSets_ControllerLeaseVerification(t *testing.T) {
	lease := "lease-gen-1"
	store, runID, authorRef, _ := setupProposalTestFixture(t, lease)
	defer store.Close()
	ctx := context.Background()

	meta, err := store.RecordObservedArtifact(ctx, authorRef, "proposal.md", []byte("# Claude Proposal"))
	if err != nil {
		t.Fatalf("record observed artifact: %v", err)
	}

	members := []storage.ProposalMemberRef{
		{ArtifactID: meta.ID, Revision: meta.Revision, Digest: meta.Digest},
	}

	// 1. Empty caller lease fails
	_, err = store.ReleaseArtifacts(ctx, "op-rel-empty", "", runID, members)
	if !errors.Is(err, storage.ErrAdoptionRequired) {
		t.Fatalf("expected ErrAdoptionRequired for empty lease, got %v", err)
	}

	// 2. Unauthorized caller lease fails
	_, err = store.ReleaseArtifacts(ctx, "op-rel-wrong", "wrong-lease", runID, members)
	if !errors.Is(err, storage.ErrUnauthorizedOperation) {
		t.Fatalf("expected ErrUnauthorizedOperation for wrong lease, got %v", err)
	}

	// 3. Handoff to generation 2 supersedes lease-gen-1
	handoffGrant, err := store.HandoffController(ctx, "op-handoff", runID, lease, 1, "claude", "ctrl-ref-2", "lease-gen-2")
	if err != nil {
		t.Fatalf("handoff controller: %v", err)
	}

	// 4. Superseded lease fails with ErrLeaseSuperseded
	_, err = store.ReleaseArtifacts(ctx, "op-rel-stale", lease, runID, members)
	if !errors.Is(err, storage.ErrLeaseSuperseded) {
		t.Fatalf("expected ErrLeaseSuperseded for superseded lease, got %v", err)
	}

	// 5. Active lease succeeds
	receipt, err := store.ReleaseArtifacts(ctx, "op-rel-active", handoffGrant.LeaseSecret, runID, members)
	if err != nil {
		t.Fatalf("expected active lease to succeed, got %v", err)
	}
	if receipt.RunID != runID {
		t.Fatalf("expected runID %s, got %s", runID, receipt.RunID)
	}
}

func TestProposalSets_MemberValidation(t *testing.T) {
	lease := "lease-gen-1"
	store, runID, authorRef, _ := setupProposalTestFixture(t, lease)
	defer store.Close()
	ctx := context.Background()

	meta, err := store.RecordObservedArtifact(ctx, authorRef, "claude-proposal.md", []byte("# Claude Proposal"))
	if err != nil {
		t.Fatalf("record observed artifact: %v", err)
	}

	// 1. Empty members list
	_, err = store.ReleaseArtifacts(ctx, "op-val-empty", lease, runID, []storage.ProposalMemberRef{})
	if err == nil {
		t.Fatal("expected error for empty members list, got nil")
	}

	// 2. Duplicate member
	dupMembers := []storage.ProposalMemberRef{
		{ArtifactID: meta.ID, Revision: meta.Revision, Digest: meta.Digest},
		{ArtifactID: meta.ID, Revision: meta.Revision, Digest: meta.Digest},
	}
	_, err = store.ReleaseArtifacts(ctx, "op-val-dup", lease, runID, dupMembers)
	if err == nil {
		t.Fatal("expected error for duplicate member, got nil")
	}

	// 3. Missing artifact ID
	missingArtMembers := []storage.ProposalMemberRef{
		{ArtifactID: "non-existent.md", Revision: 1, Digest: meta.Digest},
	}
	_, err = store.ReleaseArtifacts(ctx, "op-val-missing-art", lease, runID, missingArtMembers)
	if !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound for non-existent artifact, got %v", err)
	}

	// 4. Missing revision
	missingRevMembers := []storage.ProposalMemberRef{
		{ArtifactID: meta.ID, Revision: 999, Digest: meta.Digest},
	}
	_, err = store.ReleaseArtifacts(ctx, "op-val-missing-rev", lease, runID, missingRevMembers)
	if !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound for non-existent revision, got %v", err)
	}

	// 5. Digest mismatch
	mismatchMembers := []storage.ProposalMemberRef{
		{ArtifactID: meta.ID, Revision: meta.Revision, Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	}
	_, err = store.ReleaseArtifacts(ctx, "op-val-mismatch", lease, runID, mismatchMembers)
	if err == nil {
		t.Fatal("expected error for digest mismatch, got nil")
	}

	// 6. Artifact from another run
	otherRunID := "run-other-run"
	if _, err := store.CreateRun(ctx, "op-run-other", otherRunID, "b", "s", "p", "boot-other"); err != nil {
		t.Fatalf("create other run: %v", err)
	}
	_, err = store.CreateSession(ctx, "op-sess-other", "boot-other", storage.SessionRecord{
		ID: "sess-other", RunID: otherRunID, Contributor: "opencode", Role: "worker",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}
	if _, err := store.AdoptController(ctx, "op-adopt-other", otherRunID, "opencode", "ctrl-ref", "boot-other", nil, "lease-other"); err != nil {
		t.Fatalf("adopt other controller: %v", err)
	}
	if _, err := store.ConnectRunController(ctx, "op-conn-other", otherRunID, "lease-other", 1, "inst-other"); err != nil {
		t.Fatalf("connect other controller: %v", err)
	}
	ver, err := store.GetSessionVersion(ctx, "sess-other")
	if err != nil {
		t.Fatalf("get session version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-oth", "lease-other", "sess-other", ver, storage.PendingPrompt{
		SessionID: "sess-other", TurnKey: "turn-1", Prompt: "work",
	}); err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	ver, _ = store.GetSessionVersion(ctx, "sess-other")
	relOth, err := store.ReleaseTurn(ctx, "op-rel-oth", "lease-other", "sess-other", ver, "turn-1")
	if err != nil {
		t.Fatalf("release other turn: %v", err)
	}
	otherRef := storage.ExecutionRef{SessionID: "sess-other", TurnKey: "turn-1", AttemptID: relOth.Receipt.AttemptID}
	otherMeta, err := store.RecordObservedArtifact(ctx, otherRef, "other-proposal.md", []byte("other"))
	if err != nil {
		t.Fatalf("record other artifact: %v", err)
	}

	crossRunMembers := []storage.ProposalMemberRef{
		{ArtifactID: otherMeta.ID, Revision: otherMeta.Revision, Digest: otherMeta.Digest},
	}
	_, err = store.ReleaseArtifacts(ctx, "op-val-cross-run", lease, runID, crossRunMembers)
	if !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound for artifact from another run, got %v", err)
	}
}

func TestProposalSets_CanonicalSortingSealingAndReplay(t *testing.T) {
	lease := "lease-gen-1"
	store, runID, authorRef, siblingRef := setupProposalTestFixture(t, lease)
	defer store.Close()
	ctx := context.Background()

	// Add 2 more sessions to test all 4 contributors: claude, codex, opencode, agy
	for _, s := range []struct {
		id          string
		contributor string
	}{
		{"sess-opencode", "opencode"},
		{"sess-agy", "agy"},
	} {
		_, err := store.CreateSession(ctx, "op-sess-"+s.id, lease, storage.SessionRecord{
			ID: s.id, RunID: runID, Contributor: s.contributor, Role: "worker",
			IsActiveContributor: true, State: "parked", Visibility: "reachable",
		})
		if err != nil {
			t.Fatalf("create session %s: %v", s.id, err)
		}
		ver, err := store.GetSessionVersion(ctx, s.id)
		if err != nil {
			t.Fatalf("get session version: %v", err)
		}
		_, err = store.QueuePrompt(ctx, "op-q-"+s.id, lease, s.id, ver, storage.PendingPrompt{
			SessionID: s.id, TurnKey: "t1", Prompt: "work",
		})
		if err != nil {
			t.Fatalf("queue prompt: %v", err)
		}
		ver, _ = store.GetSessionVersion(ctx, s.id)
		rel, err := store.ReleaseTurn(ctx, "op-rel-"+s.id, lease, s.id, ver, "t1")
		if err != nil {
			t.Fatalf("release turn: %v", err)
		}
		execRef := storage.ExecutionRef{SessionID: s.id, TurnKey: "t1", AttemptID: rel.Receipt.AttemptID}
		_, err = store.RecordObservedArtifact(ctx, execRef, s.contributor+"-art.md", []byte("Content from "+s.contributor))
		if err != nil {
			t.Fatalf("record artifact: %v", err)
		}
	}

	// Also record artifacts for author (claude) and sibling (codex)
	metaClaude, err := store.RecordObservedArtifact(ctx, authorRef, "claude-art.md", []byte("Content from claude"))
	if err != nil {
		t.Fatalf("record claude artifact: %v", err)
	}
	metaCodex, err := store.RecordObservedArtifact(ctx, siblingRef, "codex-art.md", []byte("Content from codex"))
	if err != nil {
		t.Fatalf("record codex artifact: %v", err)
	}

	// Read metadata for the other two
	var metaOpencode, metaAgy storage.ArtifactMetadata
	row := store.DB().QueryRowContext(ctx, "SELECT revision, digest FROM artifact_revisions WHERE artifact_id = 'opencode-art.md';")
	if err := row.Scan(&metaOpencode.Revision, &metaOpencode.Digest); err != nil {
		t.Fatalf("scan opencode meta: %v", err)
	}
	metaOpencode.ID = "opencode-art.md"

	row = store.DB().QueryRowContext(ctx, "SELECT revision, digest FROM artifact_revisions WHERE artifact_id = 'agy-art.md';")
	if err := row.Scan(&metaAgy.Revision, &metaAgy.Digest); err != nil {
		t.Fatalf("scan agy meta: %v", err)
	}
	metaAgy.ID = "agy-art.md"

	// Pass members in non-canonical order: opencode, codex, claude, agy
	inputMembersOrder1 := []storage.ProposalMemberRef{
		{ArtifactID: metaOpencode.ID, Revision: metaOpencode.Revision, Digest: metaOpencode.Digest},
		{ArtifactID: metaCodex.ID, Revision: metaCodex.Revision, Digest: metaCodex.Digest},
		{ArtifactID: metaClaude.ID, Revision: metaClaude.Revision, Digest: metaClaude.Digest},
		{ArtifactID: metaAgy.ID, Revision: metaAgy.Revision, Digest: metaAgy.Digest},
	}

	opID := "op-seal-propset-1"
	receipt1, err := store.ReleaseArtifacts(ctx, opID, lease, runID, inputMembersOrder1)
	if err != nil {
		t.Fatalf("ReleaseArtifacts failed: %v", err)
	}

	// Verify digest format: "propset-v1:sha256:" + 64 hex characters
	prefix := "propset-v1:sha256:"
	if !strings.HasPrefix(receipt1.ProposalSetDigest, prefix) {
		t.Fatalf("expected prefix %q, got %q", prefix, receipt1.ProposalSetDigest)
	}
	hexPart := strings.TrimPrefix(receipt1.ProposalSetDigest, prefix)
	if len(hexPart) != 64 {
		t.Fatalf("expected 64 hex chars in digest, got %d (%q)", len(hexPart), hexPart)
	}
	if receipt1.RunID != runID {
		t.Fatalf("expected runID %s, got %s", runID, receipt1.RunID)
	}
	if receipt1.SealedAt.IsZero() {
		t.Fatal("expected non-zero SealedAt")
	}

	// Verify database state: proposal_sets
	var storedOpID, storedSealedAt string
	var storedGen int
	err = store.DB().QueryRowContext(ctx, `
SELECT run_id, released_by_op_id, issuing_controller_generation, sealed_at
FROM proposal_sets WHERE proposal_set_digest = ?;`, receipt1.ProposalSetDigest).Scan(&runID, &storedOpID, &storedGen, &storedSealedAt)
	if err != nil {
		t.Fatalf("query proposal_sets: %v", err)
	}
	if storedOpID != opID {
		t.Fatalf("expected released_by_op_id %q, got %q", opID, storedOpID)
	}
	if storedGen != 1 {
		t.Fatalf("expected issuing_controller_generation 1, got %d", storedGen)
	}

	// Verify database state: proposal_set_members
	var memberCount int
	err = store.DB().QueryRowContext(ctx, `
SELECT count(*) FROM proposal_set_members WHERE proposal_set_digest = ?;`, receipt1.ProposalSetDigest).Scan(&memberCount)
	if err != nil {
		t.Fatalf("count proposal_set_members: %v", err)
	}
	if memberCount != 4 {
		t.Fatalf("expected 4 proposal_set_members, got %d", memberCount)
	}

	// Verify database state: artifact_revisions updated to released = 1
	var unreleasedCount int
	err = store.DB().QueryRowContext(ctx, `
SELECT count(*) FROM artifact_revisions WHERE run_id = ? AND (released = 0 OR proposal_set_digest IS NULL);`, runID).Scan(&unreleasedCount)
	if err != nil {
		t.Fatalf("count unreleased: %v", err)
	}
	if unreleasedCount != 0 {
		t.Fatalf("expected 0 unreleased artifact revisions, got %d", unreleasedCount)
	}

	// Verify idempotency on replay with same opID and different order of members
	inputMembersOrder2 := []storage.ProposalMemberRef{
		{ArtifactID: metaAgy.ID, Revision: metaAgy.Revision, Digest: metaAgy.Digest},
		{ArtifactID: metaClaude.ID, Revision: metaClaude.Revision, Digest: metaClaude.Digest},
		{ArtifactID: metaOpencode.ID, Revision: metaOpencode.Revision, Digest: metaOpencode.Digest},
		{ArtifactID: metaCodex.ID, Revision: metaCodex.Revision, Digest: metaCodex.Digest},
	}
	receipt2, err := store.ReleaseArtifacts(ctx, opID, lease, runID, inputMembersOrder2)
	if err != nil {
		t.Fatalf("replaying ReleaseArtifacts failed: %v", err)
	}
	if receipt2.ProposalSetDigest != receipt1.ProposalSetDigest {
		t.Fatalf("expected same digest %q on replay, got %q", receipt1.ProposalSetDigest, receipt2.ProposalSetDigest)
	}
	if receipt2.RunID != receipt1.RunID {
		t.Fatalf("expected same runID on replay")
	}

	// Verify idempotency conflict with same opID and different members
	subsetMembers := []storage.ProposalMemberRef{
		{ArtifactID: metaAgy.ID, Revision: metaAgy.Revision, Digest: metaAgy.Digest},
	}
	_, err = store.ReleaseArtifacts(ctx, opID, lease, runID, subsetMembers)
	if !errors.Is(err, storage.ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict on mismatching parameters replay, got %v", err)
	}
}

func TestProposalSets_PeerReviewerReadVisibility(t *testing.T) {
	lease := "lease-gen-1"
	store, runID, authorRef, siblingRef := setupProposalTestFixture(t, lease)
	defer store.Close()
	ctx := context.Background()

	content := []byte("# Isolated Draft Proposal")
	meta, err := store.RecordObservedArtifact(ctx, authorRef, "draft.md", content)
	if err != nil {
		t.Fatalf("record observed artifact: %v", err)
	}

	// 1. Before release: Author CAN read its own draft
	data, authMeta, err := store.ReadAuthorArtifact(ctx, authorRef, "draft.md")
	if err != nil {
		t.Fatalf("author read draft: %v", err)
	}
	if !bytes.Equal(data, content) || authMeta.Digest != meta.Digest {
		t.Fatal("author read data or digest mismatch")
	}

	// 2. Before release: Sibling CANNOT read draft via ReadAuthorArtifact
	_, _, err = store.ReadAuthorArtifact(ctx, siblingRef, "draft.md")
	if !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound for sibling ReadAuthorArtifact, got %v", err)
	}

	// 3. Before release: Sibling CANNOT read draft via ReadReleasedArtifact
	_, _, err = store.ReadReleasedArtifact(ctx, siblingRef.SessionID, "propset-unreleased", "draft.md")
	if !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound for sibling ReadReleasedArtifact before release, got %v", err)
	}

	// 4. Release artifacts
	members := []storage.ProposalMemberRef{
		{ArtifactID: meta.ID, Revision: meta.Revision, Digest: meta.Digest},
	}
	receipt, err := store.ReleaseArtifacts(ctx, "op-release-draft", lease, runID, members)
	if err != nil {
		t.Fatalf("release artifacts failed: %v", err)
	}

	// 5. After release: Sibling CAN read released artifact via ReadReleasedArtifact
	relData, relMeta, err := store.ReadReleasedArtifact(ctx, siblingRef.SessionID, receipt.ProposalSetDigest, "draft.md")
	if err != nil {
		t.Fatalf("sibling read released artifact failed: %v", err)
	}
	if !bytes.Equal(relData, content) {
		t.Fatal("released content mismatch")
	}
	if relMeta.Digest != meta.Digest {
		t.Fatal("released digest mismatch")
	}

	// 6. Reading with incorrect proposal_set_digest fails
	_, _, err = store.ReadReleasedArtifact(ctx, siblingRef.SessionID, "propset-v1:sha256:wrong", "draft.md")
	if !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound for wrong proposal set digest, got %v", err)
	}
}
