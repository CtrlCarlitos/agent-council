package storage

// Crash-boundary regressions for the Agy adapter durable state (AC-010):
// every crash gap between durable transitions is verified across a
// store reopen — the classification must survive the restart. Agy
// differs structurally from its Claude/Codex siblings in having NO
// redispatch branch: launch_count is capped at 1 (0→1 only), and once
// consumed it never returns to 0 — not even after a definitive
// start_failed. A caller that wants to try again dispatches a NEW
// attempt, not a second reservation on this one.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func agyAttemptFixture(attemptID, sessionID, turnKey string) AgyTurnAttempt {
	return AgyTurnAttempt{
		AttemptID:    attemptID,
		SessionID:    sessionID,
		TurnKey:      turnKey,
		PromptDigest: "pdig-v1:sha256:fixed-test-digest",
	}
}

func agyAttemptFixtureWithTools(attemptID, sessionID, turnKey string, tools []string) AgyTurnAttempt {
	a := agyAttemptFixture(attemptID, sessionID, turnKey)
	a.RequiredTools = tools
	return a
}

func insertAgyBinding(t *testing.T, store *Store, sessionID, nativeID string) {
	t.Helper()
	if err := store.InsertAgySessionBinding(context.Background(), AgySessionBinding{
		SessionID: sessionID, NativeID: nativeID,
		Model: "agy-1", Workspace: "/tmp/ws",
		ProfileDigest: "aprof-v1:sha256:pd",
	}); err != nil {
		t.Fatalf("insert binding: %v", err)
	}
}

// Crash boundary 1/5: before reservation. Durable state proves no
// process was authorized — this is pre-start evidence, so the attempt
// remains safely dispatchable.
func TestAgyState_CrashBeforeLaunchReservation(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-a", "sess-a", "t-a")); err != nil {
		t.Fatalf("insert: %v", err)
	}

	reopened := reopenStore(t, store)

	a, err := reopened.GetAgyTurnAttempt(ctx, "att-a")
	if err != nil || a == nil {
		t.Fatalf("attempt must survive reopen, got %+v err=%v", a, err)
	}
	if a.LaunchCount != 0 {
		t.Fatalf("launch_count must be 0 (process never started), got %d", a.LaunchCount)
	}
	seq, err := reopened.ReserveAgyLaunch(ctx, "att-a", "fixture")
	if err != nil || seq != 1 {
		t.Fatalf("pre-start attempt must remain safely dispatchable, got seq=%d err=%v", seq, err)
	}
}

// Crash boundary 2/5: after reservation, before start. The reserved
// row survives with state=reserved; no redispatch branch exists at
// all, so a second reservation attempt always fails regardless of the
// (unknown) fate of the first.
func TestAgyState_CrashAfterReservationBeforeStart(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-b", "sess-b", "t-b")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveAgyLaunch(ctx, "att-b", "fixture")
	if err != nil || seq != 1 {
		t.Fatalf("reserve: seq=%d err=%v", seq, err)
	}

	reopened := reopenStore(t, store)

	a, err := reopened.GetAgyTurnAttempt(ctx, "att-b")
	if err != nil || a == nil {
		t.Fatalf("attempt must survive reopen: %v", err)
	}
	if a.LaunchCount != 1 {
		t.Fatalf("launch_count must be 1, got %d", a.LaunchCount)
	}
	if _, err := reopened.ReserveAgyLaunch(ctx, "att-b", "fixture"); err == nil {
		t.Fatal("a second reservation must fail: agy has no redispatch branch")
	}
	states, err := reopened.AgyAttemptLaunchStates(ctx, "att-b")
	if err != nil || len(states) != 1 || states[0] != "reserved" {
		t.Fatalf("exactly one retained reserved row expected, got %v err=%v", states, err)
	}
}

// Crash boundary 3/5: after start, before the first stdin byte. The
// process is possibly running; the attempt stays uncertain and no
// second reservation is authorized. The exe identity captured at start
// (sealed-image executor launch) is durable across the reopen.
func TestAgyState_CrashAfterStartBeforeFirstByte(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-c", "sess-c", "t-c")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveAgyLaunch(ctx, "att-c", "fixture")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	exe := &AgyExeIdentity{DevIno: "8,1:123456", Digest: "sha256:" + strings.Repeat("ab", 32)}
	if err := store.RecordAgyLaunchState(ctx, "att-c", seq, "started", nil, exe); err != nil {
		t.Fatalf("started: %v", err)
	}

	reopened := reopenStore(t, store)
	a, err := reopened.GetAgyTurnAttempt(ctx, "att-c")
	if err != nil || a == nil {
		t.Fatalf("attempt must survive: %v", err)
	}
	if a.ObservedStatus != "uncertain" {
		t.Fatalf("possibly-running attempt must be uncertain, got %q", a.ObservedStatus)
	}
	if a.Accepted != nil {
		t.Fatalf("accepted must still be NULL before any user-input evidence, got %v", *a.Accepted)
	}
	if _, err := reopened.ReserveAgyLaunch(ctx, "att-c", "fixture"); err == nil {
		t.Fatal("no second reservation is authorized while possibly running")
	}

	var devIno, digest string
	if err := reopened.DB().QueryRow(
		`SELECT exe_dev_ino, exe_digest FROM agy_attempt_launches WHERE attempt_id = ? AND reservation_seq = ?`,
		"att-c", seq).Scan(&devIno, &digest); err != nil {
		t.Fatalf("query exe identity: %v", err)
	}
	if devIno != exe.DevIno || digest != exe.Digest {
		t.Fatalf("exe identity must be recorded at start and survive reopen, got dev_ino=%q digest=%q", devIno, digest)
	}
}

// Crash boundary 4/5: after the first stdin byte, before user_input is
// confirmed accepted. accepted stays NULL and native_step_index stays
// NULL until the native step index arrives — at which point both flip
// atomically, since the step index IS the crash-safe acceptance
// evidence.
func TestAgyState_CrashAfterFirstByteBeforeUserInput(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-d", "sess-d", "t-d")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveAgyLaunch(ctx, "att-d", "fixture")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordAgyLaunchState(ctx, "att-d", seq, "started", nil, nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := store.RecordAgyStdinTransmitted(ctx, "att-d", seq); err != nil {
		t.Fatalf("record stdin: %v", err)
	}

	reopened := reopenStore(t, store)
	a, err := reopened.GetAgyTurnAttempt(ctx, "att-d")
	if err != nil || a == nil {
		t.Fatalf("attempt must survive: %v", err)
	}
	if a.Accepted != nil {
		t.Fatalf("accepted must be NULL (unknown) until the native step index arrives, got %v", *a.Accepted)
	}
	if a.NativeStepIndex != nil {
		t.Fatalf("native_step_index must be NULL, got %v", *a.NativeStepIndex)
	}
	if a.ObservedStatus != "uncertain" {
		t.Fatalf("status must be uncertain, got %q", a.ObservedStatus)
	}

	// The boundary resolves: the native step index arrives and accepted
	// flips atomically with it.
	if err := reopened.RecordAgyNativeStepIndex(ctx, "att-d", 0); err != nil {
		t.Fatalf("record native step index: %v", err)
	}
	a2, err := reopened.GetAgyTurnAttempt(ctx, "att-d")
	if err != nil || a2 == nil {
		t.Fatalf("get after step index: %v", err)
	}
	if a2.Accepted == nil || !*a2.Accepted {
		t.Fatalf("accepted must flip true together with the native step index, got %v", a2.Accepted)
	}
	if a2.NativeStepIndex == nil || *a2.NativeStepIndex != 0 {
		t.Fatalf("native_step_index must be 0, got %v", a2.NativeStepIndex)
	}

	// Once-only: a replay of the same index is idempotent; a
	// conflicting rebinding fails closed.
	if err := reopened.RecordAgyNativeStepIndex(ctx, "att-d", 0); err != nil {
		t.Fatalf("idempotent replay of the same index must succeed: %v", err)
	}
	if err := reopened.RecordAgyNativeStepIndex(ctx, "att-d", 1); err == nil {
		t.Fatal("a conflicting native step index must be refused")
	}
}

// Crash boundary 5/5: after the terminal write. The exactly-once guard
// and the recorded verification evidence both survive the reopen; a
// second terminal write is still rejected.
func TestAgyState_CrashAfterTerminalWrite(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixtureWithTools("att-e", "sess-e", "t-e", []string{"shell", "edit_file"})); err != nil {
		t.Fatalf("insert: %v", err)
	}
	verification := AgyVerification{
		Executed:        []string{"shell"},
		MissingRequired: []string{"edit_file"},
		Incomplete:      true,
	}
	if err := store.SetAgyAttemptTerminal(ctx, "att-e", "completed", "first result", `{"cost":1}`, verification); err != nil {
		t.Fatalf("terminal: %v", err)
	}

	reopened := reopenStore(t, store)
	a, err := reopened.GetAgyTurnAttempt(ctx, "att-e")
	if err != nil || a == nil {
		t.Fatalf("get: %v", err)
	}
	if !a.Terminal || a.ObservedStatus != "completed" {
		t.Fatalf("expected terminal completed, got terminal=%v status=%q", a.Terminal, a.ObservedStatus)
	}
	if a.ResultPayload == nil || *a.ResultPayload != "first result" {
		t.Fatalf("result payload must survive reopen, got %v", a.ResultPayload)
	}
	if len(a.RequiredTools) != 2 || a.RequiredTools[0] != "shell" || a.RequiredTools[1] != "edit_file" {
		t.Fatalf("the dispatched required-tools baseline must survive reopen, got %v", a.RequiredTools)
	}
	if len(a.ExecutedTools) != 1 || a.ExecutedTools[0] != "shell" {
		t.Fatalf("executed tools must survive reopen, got %v", a.ExecutedTools)
	}
	if len(a.MissingRequiredTools) != 1 || a.MissingRequiredTools[0] != "edit_file" {
		t.Fatalf("missing required tools must survive reopen, got %v", a.MissingRequiredTools)
	}
	if !a.VerificationIncomplete {
		t.Fatal("verification_incomplete must survive reopen")
	}

	if err := reopened.SetAgyAttemptTerminal(ctx, "att-e", "completed", "second result", `{"cost":2}`, verification); err == nil {
		t.Fatal("a second terminal write must fail with the exactly-once guard")
	}
	again, err := reopened.GetAgyTurnAttempt(ctx, "att-e")
	if err != nil || again == nil || again.ResultPayload == nil || *again.ResultPayload != "first result" {
		t.Fatalf("the first result must be preserved after a rejected second write, got %+v err=%v", again, err)
	}
}

// The schema CHECK itself rejects launch_count = 2: the cap is
// enforced at the storage level, not just in application logic.
func TestAgyState_LaunchCountCheckRejectsTwo(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-cap", "sess-cap", "t-cap")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE agy_turn_attempts SET launch_count = 2 WHERE attempt_id = ?`, "att-cap"); err == nil {
		t.Fatal("the schema CHECK must reject launch_count = 2")
	}
}

// A definitive start failure classifies the attempt as missing —
// positive pre-start evidence — but, unlike codex/claude, does NOT
// free the launch slot: there is no redispatch branch, so a further
// reservation on the SAME attempt is refused even though the process
// never started.
func TestAgyState_StartFailedClassifiesMissingWithoutFreeingSlot(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	insertAgyBinding(t, store, "sess-miss", "0195f7a1-2b3c-4def-9abc-def012345680")
	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-miss", "sess-miss", "t-miss")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveAgyLaunch(ctx, "att-miss", "fixture")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordAgyLaunchState(ctx, "att-miss", seq, "start_failed", nil, nil); err != nil {
		t.Fatalf("start_failed: %v", err)
	}

	a, err := store.GetAgyTurnAttempt(ctx, "att-miss")
	if err != nil || a == nil {
		t.Fatalf("get: %v", err)
	}
	if a.ObservedStatus != "missing" || a.Terminal {
		t.Fatalf("start failure must classify missing, got status=%q terminal=%v", a.ObservedStatus, a.Terminal)
	}
	if a.LaunchCount != 1 {
		t.Fatalf("the launch slot must NOT be freed (no redispatch branch), got launch_count=%d", a.LaunchCount)
	}

	blocked, err := store.HasAgyUnresolvedAttempts(ctx, "0195f7a1-2b3c-4def-9abc-def012345680")
	if err != nil {
		t.Fatalf("unresolved query: %v", err)
	}
	if blocked {
		t.Fatal("a missing attempt must not count as unresolved")
	}

	if _, err := store.ReserveAgyLaunch(ctx, "att-miss", "fixture"); err == nil {
		t.Fatal("start_failed must not authorize a further reservation on the same attempt")
	}
}

// Only completed|failed|cancelled are valid terminal observed statuses;
// anything else (including 'missing' and 'uncertain', which are never
// terminal outcomes) is rejected.
func TestAgyState_TerminalRejectsNonTerminalStatus(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-bad-status", "sess-bad-status", "t-bad-status")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for _, bad := range []string{"missing", "uncertain", "error_something"} {
		if err := store.SetAgyAttemptTerminal(ctx, "att-bad-status", bad, "r", "{}", AgyVerification{}); err == nil {
			t.Fatalf("observed status %q must be rejected as a terminal status", bad)
		}
	}
	if err := store.SetAgyAttemptTerminal(ctx, "att-bad-status", "cancelled", "r", "{}", AgyVerification{}); err != nil {
		t.Fatalf("cancelled must be a valid terminal status: %v", err)
	}
}

// A duplicate stdin-transmission call does not overwrite the original
// ambiguity boundary timestamp.
func TestAgyState_StdinTransmissionFirstWriteWins(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-stdin-wins", "sess-stdin-wins", "t-stdin-wins")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveAgyLaunch(ctx, "att-stdin-wins", "fixture")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordAgyLaunchState(ctx, "att-stdin-wins", seq, "started", nil, nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := store.RecordAgyStdinTransmitted(ctx, "att-stdin-wins", seq); err != nil {
		t.Fatalf("first stdin: %v", err)
	}
	var first string
	if err := store.DB().QueryRow(
		`SELECT first_stdin_byte_at FROM agy_attempt_launches WHERE attempt_id = ? AND reservation_seq = ?`,
		"att-stdin-wins", seq).Scan(&first); err != nil {
		t.Fatalf("read boundary: %v", err)
	}
	// A duplicate call fails (the WHERE clause requires the boundary to
	// be unset), and the timestamp is unchanged either way.
	if err := store.RecordAgyStdinTransmitted(ctx, "att-stdin-wins", seq); err == nil {
		t.Fatal("a duplicate stdin-transmission call must fail (first-write-wins)")
	}
	var second string
	if err := store.DB().QueryRow(
		`SELECT first_stdin_byte_at FROM agy_attempt_launches WHERE attempt_id = ? AND reservation_seq = ?`,
		"att-stdin-wins", seq).Scan(&second); err != nil {
		t.Fatalf("read boundary 2: %v", err)
	}
	if first != second {
		t.Fatalf("duplicate stdin call must not change the boundary, got %q then %q", first, second)
	}
}

// Session bindings round-trip: insert unmaterialized, duplicate native
// identity rejected, non-UUIDv4 native ids rejected by the schema
// CHECK, and materialization records the conversation path and file
// identity exactly once.
func TestAgyState_SessionBindingLifecycle(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	native := "0195f7a1-2b3c-4def-9abc-def012345690"
	insertAgyBinding(t, store, "sess-bind", native)

	b, err := store.GetAgySessionBinding(ctx, "sess-bind")
	if err != nil || b == nil {
		t.Fatalf("get binding: %v", err)
	}
	if b.NativeID != native || b.Materialized {
		t.Fatalf("unexpected binding: %+v", b)
	}
	if b.ConversationPath != nil || b.FileIdentity != nil || b.FirstPromptDigest != nil {
		t.Fatalf("conversation_path, file_identity, and first_prompt_digest must start NULL, got %v %v %v",
			b.ConversationPath, b.FileIdentity, b.FirstPromptDigest)
	}

	missing, err := store.GetAgySessionBinding(ctx, "sess-missing")
	if err != nil || missing != nil {
		t.Fatalf("missing binding must be nil, got %+v err=%v", missing, err)
	}

	// Duplicate native identity is rejected (UNIQUE).
	err = store.InsertAgySessionBinding(ctx, AgySessionBinding{
		SessionID: "sess-other", NativeID: native,
		Model: "agy-1", Workspace: "/tmp/ws",
		ProfileDigest: "aprof-v1:sha256:pd",
	})
	if err == nil {
		t.Fatal("duplicate native id must be rejected")
	}

	// A UUIDv7-shaped id (version nibble 7, not 4) is rejected by the
	// schema CHECK.
	err = store.InsertAgySessionBinding(ctx, AgySessionBinding{
		SessionID: "sess-bad", NativeID: "0195f7a1-2b3c-7def-9abc-def012345691",
		Model: "agy-1", Workspace: "/tmp/ws",
		ProfileDigest: "aprof-v1:sha256:pd",
	})
	if err == nil {
		t.Fatal("a non-UUIDv4 native id must be rejected by the schema CHECK")
	}

	conversationPath := "/home/op/.agy/conversations/0195f7a1-2b3c-4def-9abc-def012345690.jsonl"
	fileIdentity := "1234:5678"
	if err := store.MarkAgySessionMaterialized(ctx, "sess-bind", conversationPath, fileIdentity); err != nil {
		t.Fatalf("mark materialized: %v", err)
	}
	// A second call is a no-op (only transitions false → true).
	if err := store.MarkAgySessionMaterialized(ctx, "sess-bind", "/other/path", "9999:0000"); err != nil {
		t.Fatalf("second mark: %v", err)
	}
	b2, err := store.GetAgySessionBinding(ctx, "sess-bind")
	if err != nil || b2 == nil {
		t.Fatalf("get binding 2: %v", err)
	}
	if !b2.Materialized {
		t.Fatal("binding must be materialized")
	}
	if b2.ConversationPath == nil || *b2.ConversationPath != conversationPath {
		t.Fatalf("conversation path must be recorded at materialization, got %v", b2.ConversationPath)
	}
	if b2.FileIdentity == nil || *b2.FileIdentity != fileIdentity {
		t.Fatalf("file identity must be recorded at materialization, got %v", b2.FileIdentity)
	}
}

// The latest attempt for a turn is returned in insertion order.
func TestAgyState_LatestAttemptPerTurn(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-l1", "sess-l", "t-l")); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	latest, err := store.GetLatestAgyTurnAttempt(ctx, "sess-l", "t-l")
	if err != nil || latest == nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.AttemptID != "att-l1" {
		t.Fatalf("expected att-l1, got %q", latest.AttemptID)
	}

	none, err := store.GetLatestAgyTurnAttempt(ctx, "sess-l", "t-none")
	if err != nil || none != nil {
		t.Fatalf("missing turn must be nil, got %+v err=%v", none, err)
	}
}

// Unresolved uncertain attempts without a disposition are reported.
func TestAgyState_UnresolvedAttemptsReported(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	insertAgyBinding(t, store, "sess-un", "0195f7a1-2b3c-4def-9abc-def012345692")
	if err := store.InsertAgyTurnAttempt(ctx, agyAttemptFixture("att-un", "sess-un", "t-un")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seq, err := store.ReserveAgyLaunch(ctx, "att-un", "fixture")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.RecordAgyLaunchState(ctx, "att-un", seq, "started", nil, nil); err != nil {
		t.Fatalf("started: %v", err)
	}

	unresolved, err := store.HasAgyUnresolvedAttempts(ctx, "0195f7a1-2b3c-4def-9abc-def012345692")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !unresolved {
		t.Fatal("started-without-result attempt must be unresolved")
	}
}

// The protection-attestation lookup must match the FULL frozen tuple —
// (agy version, platform, manifest digest, profile digest); a row that
// disagrees on the manifest digest alone never satisfies it.
func TestAgyState_FindProtectionAttestationRequiresManifestDigest(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	const (
		version  = "0.1.0"
		platform = "linux/amd64"
		profile  = "aprof-v1:sha256:pd"
	)
	trueManifest := "sha256:" + strings.Repeat("aa", 32)
	otherManifest := "sha256:" + strings.Repeat("bb", 32)
	insert := func(id, manifest string) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO agy_protection_attestations
	(attestation_id, agy_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, ?, ?, ?, ?, '[]', '2026-09-24T00:00:00Z', 'op')`,
			id, version, platform, manifest, profile); err != nil {
			t.Fatalf("insert attestation: %v", err)
		}
	}
	trueID := "cprot-v2:sha256:" + strings.Repeat("ab", 32)
	insert(trueID, trueManifest)

	got, err := store.FindAgyProtectionAttestation(ctx, version, platform, trueManifest, profile)
	if err != nil || got != trueID {
		t.Fatalf("the full-tuple match must find the attestation: %q err=%v", got, err)
	}
	got, err = store.FindAgyProtectionAttestation(ctx, version, platform, otherManifest, profile)
	if err != nil || got != "" {
		t.Fatalf("a manifest digest mismatch must fail the lookup, got %q err=%v", got, err)
	}
}

// The probe-results seam reads the durable cprot-v2 record frame by
// attestation id: present rows return the exact bytes; a missing row
// returns nil, never an error.
func TestAgyState_ProtectionProbeResults(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	id := "cprot-v2:sha256:" + strings.Repeat("cd", 32)
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO agy_protection_attestations
	(attestation_id, agy_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, '0.1.0', 'test', 'sha256:md', 'aprof-v1:sha256:pd', '[]', '2026-09-24T00:00:00Z', 'op')`,
		id); err != nil {
		t.Fatalf("insert attestation: %v", err)
	}

	raw, err := store.AgyProtectionProbeResults(ctx, id)
	if err != nil {
		t.Fatalf("probe results: %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("probe_results must round-trip verbatim, got %q", raw)
	}

	missing, err := store.AgyProtectionProbeResults(ctx, "cprot-v2:sha256:absent")
	if err != nil || missing != nil {
		t.Fatalf("a missing attestation row must be nil,nil, got %q err=%v", missing, err)
	}
	empty, err := store.AgyProtectionProbeResults(ctx, "")
	if err != nil || empty != nil {
		t.Fatalf("an empty attestation id must be nil,nil, got %q err=%v", empty, err)
	}
}

// required_tools_json round-trips end to end: QueuePrompt persists the
// dispatched allow-list; ReleaseTurn carries it from pending_prompts to
// dispatch_intents (the pending_prompts row is deleted in the same
// transaction); GetTurnDetails reads it back on DispatchIntent.
func TestAgyState_RequiredToolsRoundTripThroughQueuePromptAndGetTurnDetails(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()

	ver, err := store.GetSessionVersion(ctx, agyUncSession)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	tools := []string{"shell", "edit_file"}
	if _, err := store.QueuePrompt(ctx, "op-q-tools", agyUncLease, agyUncSession, ver, PendingPrompt{
		SessionID: agyUncSession, TurnKey: "t-tools", Prompt: "do the thing", RequiredTools: tools,
	}); err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	ver, err = store.GetSessionVersion(ctx, agyUncSession)
	if err != nil {
		t.Fatalf("get version after queue: %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-tools", agyUncLease, agyUncSession, ver, "t-tools"); err != nil {
		t.Fatalf("release turn: %v", err)
	}

	details, err := store.GetTurnDetails(ctx, agyUncSession, "t-tools")
	if err != nil || details == nil {
		t.Fatalf("get turn details: %v", err)
	}
	if details.DispatchIntent == nil {
		t.Fatal("dispatch intent must be recorded")
	}
	if len(details.DispatchIntent.RequiredTools) != 2 ||
		details.DispatchIntent.RequiredTools[0] != "shell" || details.DispatchIntent.RequiredTools[1] != "edit_file" {
		t.Fatalf("required tools must round-trip through queue_prompt -> release_turn -> GetTurnDetails, got %v",
			details.DispatchIntent.RequiredTools)
	}

	// The pending_prompts row is gone (promoted to turns/dispatch_intents).
	var count int
	if err := store.DB().QueryRow(
		`SELECT count(*) FROM pending_prompts WHERE session_id = ? AND turn_key = ?`,
		agyUncSession, "t-tools").Scan(&count); err != nil {
		t.Fatalf("count pending prompts: %v", err)
	}
	if count != 0 {
		t.Fatalf("the pending prompt must be deleted once released, got %d rows", count)
	}
}

// A caller that never sets RequiredTools keeps working unchanged: the
// column defaults to '[]', which decodes to an empty slice throughout
// the round trip.
func TestAgyState_RequiredToolsDefaultsEmptyWhenUnset(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()

	ver, err := store.GetSessionVersion(ctx, agyUncSession)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-notools", agyUncLease, agyUncSession, ver, PendingPrompt{
		SessionID: agyUncSession, TurnKey: "t-notools", Prompt: "no tools needed",
	}); err != nil {
		t.Fatalf("queue prompt without tools: %v", err)
	}
	ver, err = store.GetSessionVersion(ctx, agyUncSession)
	if err != nil {
		t.Fatalf("get version after queue: %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-notools", agyUncLease, agyUncSession, ver, "t-notools"); err != nil {
		t.Fatalf("release turn without tools: %v", err)
	}
	details, err := store.GetTurnDetails(ctx, agyUncSession, "t-notools")
	if err != nil || details == nil || details.DispatchIntent == nil {
		t.Fatalf("get turn details without tools: %+v err=%v", details, err)
	}
	if len(details.DispatchIntent.RequiredTools) != 0 {
		t.Fatalf("an unset required-tools list must decode empty, got %v", details.DispatchIntent.RequiredTools)
	}
}

// GetAgyProtectionAttestation returns the whole durable row (tuple and
// frame) by attestation id so the adapter can re-compare the stored
// tuple against the policy in force before validating coverage (AC-010
// Task 6); a missing row is nil, never an error.
func TestAgyState_GetProtectionAttestation(t *testing.T) {
	store := openAgyStore(t)
	ctx := context.Background()

	id := "cprot-v2:sha256:" + strings.Repeat("ef", 32)
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO agy_protection_attestations
	(attestation_id, agy_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, '1.2.9', 'linux/unix', 'sha256:md', 'cprof-v4:sha256:pd', 'frame', '2026-09-24T00:00:00Z', 'op')`,
		id); err != nil {
		t.Fatalf("insert attestation: %v", err)
	}
	rec, err := store.GetAgyProtectionAttestation(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("get attestation: %+v err=%v", rec, err)
	}
	want := AgyProtectionAttestationRecord{AttestationID: id, AgyVersion: "1.2.9", Platform: "linux/unix",
		ManifestDigest: "sha256:md", ProfileDigest: "cprof-v4:sha256:pd", ProbeResults: "frame",
		ProbedAt: "2026-09-24T00:00:00Z", Actor: "op"}
	if *rec != want {
		t.Fatalf("row = %+v, want %+v", *rec, want)
	}
	missing, err := store.GetAgyProtectionAttestation(ctx, "cprot-v2:sha256:"+strings.Repeat("00", 32))
	if err != nil || missing != nil {
		t.Fatalf("a missing row is nil without error, got %+v err=%v", missing, err)
	}
	if blank, err := store.GetAgyProtectionAttestation(ctx, "  "); err != nil || blank != nil {
		t.Fatalf("a blank id is nil without error, got %+v err=%v", blank, err)
	}
}

// required_tools are validated at queue time and IMMUTABLE thereafter
// (AC-010 spec §3.5): replacing a queued prompt's text keeps the
// journaled set (the dispatch intent still carries it), and a replace
// that names a DIFFERENT set is refused rather than silently ignored.
func TestAgyState_ReplacePendingPromptKeepsImmutableRequiredTools(t *testing.T) {
	store := newAgyUncertaintyFixture(t)
	ctx := context.Background()
	version := func() int64 {
		t.Helper()
		v, err := store.GetSessionVersion(ctx, agyUncSession)
		if err != nil {
			t.Fatalf("get version: %v", err)
		}
		return v
	}
	tools := []string{"view_file", "run_command"}
	if _, err := store.QueuePrompt(ctx, "op-q-rt", agyUncLease, agyUncSession, version(), PendingPrompt{
		SessionID: agyUncSession, TurnKey: "t-rt", Prompt: "first text", RequiredTools: tools,
	}); err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	// A replace naming a different set is refused, nothing changes.
	if _, err := store.ReplacePendingPrompt(ctx, "op-r-rt-diff", agyUncLease, agyUncSession, version(), PendingPrompt{
		SessionID: agyUncSession, TurnKey: "t-rt", Prompt: "other text", RequiredTools: []string{"view_file"},
	}); err == nil || !errors.Is(err, ErrRequiredToolsImmutable) {
		t.Fatalf("a replace that changes required_tools must be refused typed, got %v", err)
	}
	// Restating the same set is accepted.
	if _, err := store.ReplacePendingPrompt(ctx, "op-r-rt-same", agyUncLease, agyUncSession, version(), PendingPrompt{
		SessionID: agyUncSession, TurnKey: "t-rt", Prompt: "second text", RequiredTools: tools,
	}); err != nil {
		t.Fatalf("a replace restating the same set: %v", err)
	}
	// Omitting the set keeps the journaled one.
	if _, err := store.ReplacePendingPrompt(ctx, "op-r-rt-omit", agyUncLease, agyUncSession, version(), PendingPrompt{
		SessionID: agyUncSession, TurnKey: "t-rt", Prompt: "third text",
	}); err != nil {
		t.Fatalf("a replace that omits the set: %v", err)
	}

	if _, err := store.ReleaseTurn(ctx, "op-rel-rt", agyUncLease, agyUncSession, version(), "t-rt"); err != nil {
		t.Fatalf("release turn: %v", err)
	}
	details, err := store.GetTurnDetails(ctx, agyUncSession, "t-rt")
	if err != nil || details == nil || details.DispatchIntent == nil {
		t.Fatalf("turn details: %+v err=%v", details, err)
	}
	if details.Prompt != "third text" {
		t.Fatalf("the replaced prompt text must be dispatched, got %q", details.Prompt)
	}
	got := details.DispatchIntent.RequiredTools
	if len(got) != 2 || got[0] != "view_file" || got[1] != "run_command" {
		t.Fatalf("the queue-time required_tools must survive every replace into the dispatch intent, got %v", got)
	}
}
