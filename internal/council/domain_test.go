package council

import "testing"

func ballots() []Ballot {
	return []Ballot{
		{OpenCode, Claude, "set-1", "clearer acceptance evidence", false},
		{Claude, Codex, "set-1", "best task coverage", true},
		{Codex, Claude, "set-1", "strong source verification", true},
		{Agy, Claude, "set-1", "best structured alternative", true},
	}
}
func TestTally(t *testing.T) {
	got, err := Tally(ballots(), "set-1")
	if err != nil || got[Claude] != 3 || got[Codex] != 1 {
		t.Fatalf("%v %v", got, err)
	}
}
func TestInvalidBallots(t *testing.T) {
	tests := map[string]func([]Ballot) []Ballot{
		"self":         func(b []Ballot) []Ballot { b[0].Choice = b[0].Voter; return b },
		"duplicate":    func(b []Ballot) []Ballot { b[3] = b[0]; return b },
		"stale":        func(b []Ballot) []Ballot { b[1].ProposalSet = "old"; return b },
		"unknown":      func(b []Ballot) []Ballot { b[0].Voter = "controller"; return b },
		"blank-reason": func(b []Ballot) []Ballot { b[0].Reason = "  "; return b },
		"missing":      func(b []Ballot) []Ballot { return b[:3] },
		"fifth-vote":   func(b []Ballot) []Ballot { return append(b, b[0]) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Tally(mutate(ballots()), "set-1"); err == nil {
				t.Fatal("accepted invalid ballots")
			}
		})
	}
}
func TestEmptyProposalSet(t *testing.T) {
	if _, err := Tally(ballots(), ""); err == nil {
		t.Fatal("missing proposal set accepted")
	}
}
func TestRosterNotMutable(t *testing.T) {
	r := Roster()
	r[0] = "bad"
	if !ValidContributor(OpenCode) {
		t.Fatal("mutable global roster")
	}
}
func TestEveryHarnessCanHaveAnIndependentSession(t *testing.T) {
	for _, id := range Roster() {
		s, err := NewSession(id, "controller-lease")
		if err != nil || s.State != Parked {
			t.Fatal(s, err)
		}
	}
}
func TestCompletionParksDespiteQueuedPrompt(t *testing.T) {
	s, _ := NewSession(OpenCode, "lease")
	if err := s.Queue("lease", "one", "first task"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Release("lease", "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Queue("lease", "two", "follow-up"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Release("lease", "two"); err == nil {
		t.Fatal("parallel turn permitted")
	}
	if err := s.Complete("one"); err != nil {
		t.Fatal(err)
	}
	if s.State != Parked || s.Pending["two"] != "follow-up" {
		t.Fatal("follow-up did not remain pending")
	}
	if _, err := s.Release("lease", "two"); err != nil {
		t.Fatal(err)
	}
}
func TestControllerLeaseRequired(t *testing.T) {
	s, _ := NewSession(Codex, "lease")
	if err := s.Queue("worker", "a", "task"); err == nil {
		t.Fatal("unauthorized enqueue")
	}
	_ = s.Queue("lease", "a", "task")
	if _, err := s.Release("old-lease", "a"); err == nil {
		t.Fatal("stale controller dispatch")
	}
	if err := s.Discard("other", "a"); err == nil {
		t.Fatal("unauthorized discard")
	}
}
func TestDuplicateQueueAndWrongCompletion(t *testing.T) {
	s, _ := NewSession(Agy, "lease")
	_ = s.Queue("lease", "a", "task")
	if err := s.Queue("lease", "a", "replacement"); err == nil {
		t.Fatal("silent replacement")
	}
	_, _ = s.Release("lease", "a")
	if err := s.Complete("another-attempt"); err == nil {
		t.Fatal("wrong completion")
	}
	if s.State != Running {
		t.Fatal("invalid completion mutated session")
	}
}
func TestArchiveRetainsPendingAndRejectsDispatch(t *testing.T) {
	s, _ := NewSession(Claude, "lease")
	_ = s.Queue("lease", "a", "task")
	if err := s.Archive("lease"); err != nil {
		t.Fatal(err)
	}
	if s.Pending["a"] != "task" {
		t.Fatal("archive purged pending data")
	}
	if _, err := s.Release("lease", "a"); err == nil {
		t.Fatal("archived session ran")
	}
}
func TestCannotArchiveActiveTurn(t *testing.T) {
	s, _ := NewSession(Claude, "lease")
	_ = s.Queue("lease", "a", "task")
	_, _ = s.Release("lease", "a")
	if err := s.Archive("lease"); err == nil {
		t.Fatal("archived running turn")
	}
}
func TestDiscardIsExplicit(t *testing.T) {
	s, _ := NewSession(OpenCode, "lease")
	_ = s.Queue("lease", "a", "task")
	if err := s.Discard("lease", "a"); err != nil {
		t.Fatal(err)
	}
	if len(s.Pending) != 0 || s.State != Parked {
		t.Fatal("unexpected state")
	}
}

func TestTurnKeyReuseRejected(t *testing.T) {
	s, err := NewSession(OpenCode, "lease")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Queue("lease", "turn-1", "task 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Release("lease", "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete("turn-1"); err != nil {
		t.Fatal(err)
	}

	// Reusing retired turn-1 for a different task must be rejected
	if err := s.Queue("lease", "turn-1", "different task"); err == nil {
		t.Fatal("retired turn key 'turn-1' was permitted to be re-queued with different task")
	}
}

func TestSubmissionIdempotencyAndTamperRejection(t *testing.T) {
	s, err := NewSession(Claude, "lease")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Queue("lease", "k1", "task content"); err != nil {
		t.Fatal(err)
	}

	// Repeated submission with identical content is idempotent
	if err := s.Queue("lease", "k1", "task content"); err != nil {
		t.Fatalf("identical repeated submission rejected: %v", err)
	}
	if len(s.Pending) != 1 || s.Pending["k1"] != "task content" {
		t.Fatal("pending queue corrupted by identical resubmission")
	}

	// Submission of same key with different content is rejected
	if err := s.Queue("lease", "k1", "different content"); err == nil {
		t.Fatal("duplicate pending key with altered content was accepted")
	}
	if s.Pending["k1"] != "task content" {
		t.Fatal("pending content was overwritten by rejected submission")
	}
}

func TestStaleCompletionDoesNotMutateNewTurn(t *testing.T) {
	s, err := NewSession(Agy, "lease")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Queue("lease", "turn-1", "task 1")
	_, _ = s.Release("lease", "turn-1")
	_ = s.CompleteWithResult("turn-1", "result 1")

	// Start turn-2
	_ = s.Queue("lease", "turn-2", "task 2")
	_, _ = s.Release("lease", "turn-2")

	// Old completion for turn-1 arrives while turn-2 is active
	err = s.CompleteWithResult("turn-1", "delayed stale result")
	if err == nil {
		t.Fatal("stale completion for turn-1 was accepted while turn-2 is active")
	}
	if s.Active != "turn-2" || s.State != Running {
		t.Fatalf("active turn mutated by stale completion: active=%q, state=%q", s.Active, s.State)
	}
	if s.Turns["turn-1"].Result != "result 1" {
		t.Fatalf("turn-1 result was overwritten by stale completion: %q", s.Turns["turn-1"].Result)
	}
}

func TestControllerDisconnectDuringActiveTurn(t *testing.T) {
	s, err := NewSession(OpenCode, "lease")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Queue("lease", "turn-1", "task 1")
	_ = s.Queue("lease", "turn-2", "task 2")
	_, _ = s.Release("lease", "turn-1")

	// Controller disconnects while turn-1 is active
	s.DisconnectController()

	if s.ControllerStatus != ControllerDisconnected {
		t.Fatal("expected controller to be disconnected")
	}
	if s.State != Running || s.Active != "turn-1" {
		t.Fatalf("disconnect altered active turn execution: state=%s, active=%s", s.State, s.Active)
	}
	if s.Pending["turn-2"] != "task 2" {
		t.Fatal("queued prompt lost on disconnect")
	}

	// Disconnected controller cannot release pending prompt
	if _, err := s.Release("lease", "turn-2"); err == nil {
		t.Fatal("release permitted while controller is disconnected")
	}

	// Active turn can still record its valid completion without active controller
	if err := s.CompleteWithResult("turn-1", "output 1"); err != nil {
		t.Fatalf("valid completion rejected after controller disconnect: %v", err)
	}
	if s.State != Parked || s.Active != "" {
		t.Fatalf("contributor did not park after completion: state=%s, active=%s", s.State, s.Active)
	}
	if s.Pending["turn-2"] != "task 2" {
		t.Fatal("queued prompt was auto-dispatched on park")
	}

	// Stale lease reconnect fails
	if err := s.ReconnectController("wrong-lease"); err == nil {
		t.Fatal("reconnect with invalid lease was permitted")
	}

	// Valid reconnect succeeds and allows explicit release
	if err := s.ReconnectController("lease"); err != nil {
		t.Fatalf("reconnect failed: %v", err)
	}
	if _, err := s.Release("lease", "turn-2"); err != nil {
		t.Fatalf("release failed after reconnect: %v", err)
	}
	if s.Active != "turn-2" || s.State != Running {
		t.Fatalf("turn-2 not running: active=%s, state=%s", s.Active, s.State)
	}
}

func TestCancellationKeepsContributorOccupiedUntilConfirmed(t *testing.T) {
	s, err := NewSession(Codex, "lease")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Queue("lease", "turn-1", "task 1")
	_ = s.Queue("lease", "turn-2", "task 2")
	_, _ = s.Release("lease", "turn-1")

	// Controller requests cancellation of active turn-1
	if err := s.RequestCancel("lease"); err != nil {
		t.Fatalf("cancel request failed: %v", err)
	}

	// Turn status is Cancelling, contributor remains occupied
	if s.ActiveTurn.Status != TurnCancelling || s.State != Running {
		t.Fatalf("contributor freed prematurely upon cancel request: turn_status=%s, state=%s", s.ActiveTurn.Status, s.State)
	}

	// Releasing another prompt must be BLOCKED while execution is unresolved
	if _, err := s.Release("lease", "turn-2"); err == nil {
		t.Fatal("release of turn-2 permitted while cancellation unresolved")
	}
	if s.Pending["turn-2"] != "task 2" {
		t.Fatal("pending queue corrupted during blocked release")
	}

	// Archiving must also be blocked while turn is cancelling
	if err := s.Archive("lease"); err == nil {
		t.Fatal("session archive permitted while turn is cancelling")
	}

	// Wrong confirmation key rejected
	if err := s.ConfirmCancel("turn-2"); err == nil {
		t.Fatal("cancel confirmation accepted with wrong key")
	}

	// Confirm cancellation of turn-1
	if err := s.ConfirmCancel("turn-1"); err != nil {
		t.Fatalf("confirm cancel failed: %v", err)
	}

	// Contributor now parks and turn-1 is retired
	if s.State != Parked || s.Active != "" || s.ActiveTurn != nil {
		t.Fatalf("contributor not parked after confirmation: state=%s, active=%s", s.State, s.Active)
	}
	if s.Turns["turn-1"].Status != TurnCancelled {
		t.Fatalf("turn-1 status not cancelled: %s", s.Turns["turn-1"].Status)
	}

	// Cancelled turn key cannot be reused with different content
	if err := s.Queue("lease", "turn-1", "altered task"); err == nil {
		t.Fatal("cancelled turn key permitted reuse with different content")
	}

	// Now turn-2 can be released
	if _, err := s.Release("lease", "turn-2"); err != nil {
		t.Fatalf("release of turn-2 failed after cancellation confirmed: %v", err)
	}
	if s.Active != "turn-2" || s.State != Running {
		t.Fatalf("turn-2 not active: active=%s, state=%s", s.Active, s.State)
	}
}

func TestHostUnreachableRecordsUncertaintyAndBlocksRelease(t *testing.T) {
	s, err := NewSession(Claude, "lease")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Queue("lease", "turn-1", "task 1")
	_ = s.Queue("lease", "turn-2", "task 2")
	_, _ = s.Release("lease", "turn-1")

	// Host contact is lost while turn-1 is running
	if err := s.RecordHostLoss(); err != nil {
		t.Fatalf("failed to record host loss: %v", err)
	}

	if s.Visibility != VisibilityHostLost {
		t.Fatalf("expected VisibilityHostLost, got %s", s.Visibility)
	}
	if s.State != Running || s.Active != "turn-1" {
		t.Fatalf("host loss freed contributor or reset active turn: state=%s, active=%s", s.State, s.Active)
	}

	// Release of turn-2 must be BLOCKED while execution outcome is uncertain
	if _, err := s.Release("lease", "turn-2"); err == nil {
		t.Fatal("replacement turn release permitted while host is uncertain")
	}
	if s.Pending["turn-2"] != "task 2" {
		t.Fatal("queued prompt corrupted during blocked release")
	}

	// Cannot archive while host is uncertain
	if err := s.Archive("lease"); err == nil {
		t.Fatal("archive permitted while host is uncertain")
	}

	// Reconcile host with failed status
	if err := s.ReconcileHost("turn-1", TurnFailed, "process exited during disconnect"); err != nil {
		t.Fatalf("host reconciliation failed: %v", err)
	}

	if s.Visibility != VisibilityReachable {
		t.Fatalf("expected VisibilityReachable after reconciliation, got %s", s.Visibility)
	}
	if s.State != Parked || s.Active != "" || s.ActiveTurn != nil {
		t.Fatalf("contributor not parked after reconciliation: state=%s, active=%s", s.State, s.Active)
	}
	if s.Turns["turn-1"].Status != TurnFailed {
		t.Fatalf("expected TurnFailed, got %s", s.Turns["turn-1"].Status)
	}

	// Now turn-2 can be released
	if _, err := s.Release("lease", "turn-2"); err != nil {
		t.Fatalf("failed to release turn-2 after reconciliation: %v", err)
	}
	if s.Active != "turn-2" || s.State != Running {
		t.Fatalf("turn-2 not running: active=%s, state=%s", s.Active, s.State)
	}
}

type turnSnapshot struct {
	key    string
	prompt string
	status TurnStatus
	result string
}

type sessionSnapshot struct {
	id               Contributor
	state            State
	lifecycle        SessionLifecycle
	controllerStatus ControllerConnection
	visibility       ExecutionVisibility
	lease            string
	active           string
	hasActiveTurn    bool
	activeTurn       turnSnapshot
	pending          map[string]string
	turns            map[string]turnSnapshot
	recoveryContext  string
}

func snapshotSession(s *Session) sessionSnapshot {
	p := make(map[string]string, len(s.Pending))
	for k, v := range s.Pending {
		p[k] = v
	}
	turns := make(map[string]turnSnapshot, len(s.Turns))
	for k, v := range s.Turns {
		if v != nil {
			turns[k] = turnSnapshot{
				key:    v.Key,
				prompt: v.Prompt,
				status: v.Status,
				result: v.Result,
			}
		}
	}
	var actSnap turnSnapshot
	hasAct := s.ActiveTurn != nil
	if hasAct {
		actSnap = turnSnapshot{
			key:    s.ActiveTurn.Key,
			prompt: s.ActiveTurn.Prompt,
			status: s.ActiveTurn.Status,
			result: s.ActiveTurn.Result,
		}
	}
	return sessionSnapshot{
		id:               s.ID,
		state:            s.State,
		lifecycle:        s.Lifecycle,
		controllerStatus: s.ControllerStatus,
		visibility:       s.Visibility,
		lease:            s.ControllerLease,
		active:           s.Active,
		hasActiveTurn:    hasAct,
		activeTurn:       actSnap,
		pending:          p,
		turns:            turns,
		recoveryContext:  s.RecoveryContext,
	}
}

func assertSnapshotEqual(t *testing.T, opName string, before, after sessionSnapshot) {
	t.Helper()
	if before.id != after.id || before.state != after.state ||
		before.lifecycle != after.lifecycle || before.controllerStatus != after.controllerStatus ||
		before.visibility != after.visibility || before.lease != after.lease ||
		before.active != after.active || before.hasActiveTurn != after.hasActiveTurn ||
		before.recoveryContext != after.recoveryContext {
		t.Fatalf("%s mutated session scalar state: before=%+v after=%+v", opName, before, after)
	}
	if before.hasActiveTurn {
		if before.activeTurn != after.activeTurn {
			t.Fatalf("%s mutated activeTurn: before=%+v after=%+v", opName, before.activeTurn, after.activeTurn)
		}
		// Consistency check between ActiveTurn and history map
		if before.activeTurn.key != before.active {
			t.Fatalf("%s inconsistent active key: active=%s turn.key=%s", opName, before.active, before.activeTurn.key)
		}
		if hist, ok := after.turns[after.active]; !ok || hist != after.activeTurn {
			t.Fatalf("%s active turn not consistent with turns history: active=%+v hist=%+v", opName, after.activeTurn, hist)
		}
	}
	if len(before.pending) != len(after.pending) {
		t.Fatalf("%s mutated pending length: before=%d after=%d", opName, len(before.pending), len(after.pending))
	}
	for k, v := range before.pending {
		if after.pending[k] != v {
			t.Fatalf("%s mutated pending[%q]: before=%q after=%q", opName, k, v, after.pending[k])
		}
	}
	if len(before.turns) != len(after.turns) {
		t.Fatalf("%s mutated turns length: before=%d after=%d", opName, len(before.turns), len(after.turns))
	}
	for k, v := range before.turns {
		act, ok := after.turns[k]
		if !ok || act != v {
			t.Fatalf("%s mutated turn[%q]: before=%+v after=%+v", opName, k, v, act)
		}
	}
}

func TestStatePreservationOnRejectedOperations(t *testing.T) {
	s, err := NewSession(Agy, "valid-lease")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Queue("valid-lease", "t1", "prompt 1")
	_ = s.Queue("valid-lease", "t2", "prompt 2")

	// 1. Stale controller attempts Queue
	snap := snapshotSession(s)
	if err := s.Queue("stale-lease", "t3", "prompt 3"); err == nil {
		t.Fatal("stale queue succeeded")
	}
	assertSnapshotEqual(t, "stale Queue", snap, snapshotSession(s))

	// 2. Stale controller attempts Release
	snap = snapshotSession(s)
	if _, err := s.Release("stale-lease", "t1"); err == nil {
		t.Fatal("stale release succeeded")
	}
	assertSnapshotEqual(t, "stale Release", snap, snapshotSession(s))

	// 3. Stale controller attempts Replace
	snap = snapshotSession(s)
	if err := s.Replace("stale-lease", "t1", "new prompt"); err == nil {
		t.Fatal("stale replace succeeded")
	}
	assertSnapshotEqual(t, "stale Replace", snap, snapshotSession(s))

	// 4. Stale controller attempts Discard
	snap = snapshotSession(s)
	if err := s.Discard("stale-lease", "t1"); err == nil {
		t.Fatal("stale discard succeeded")
	}
	assertSnapshotEqual(t, "stale Discard", snap, snapshotSession(s))

	// 5. Replace non-existent key
	snap = snapshotSession(s)
	if err := s.Replace("valid-lease", "non-existent", "new prompt"); err == nil {
		t.Fatal("replace non-existent key succeeded")
	}
	assertSnapshotEqual(t, "replace non-existent key", snap, snapshotSession(s))

	// 6. Discard non-existent key
	snap = snapshotSession(s)
	if err := s.Discard("valid-lease", "non-existent"); err == nil {
		t.Fatal("discard non-existent key succeeded")
	}
	assertSnapshotEqual(t, "discard non-existent key", snap, snapshotSession(s))

	// 7. ReconcileHost when host is not lost
	snap = snapshotSession(s)
	if err := s.ReconcileHost("t1", TurnCompleted, "res"); err == nil {
		t.Fatal("reconcile host succeeded when host not lost")
	}
	assertSnapshotEqual(t, "reconcile when not lost", snap, snapshotSession(s))

	// 8. Now release t1 legitimately
	_, _ = s.Release("valid-lease", "t1")

	// 9. Stale controller attempts RequestCancel on active turn
	snap = snapshotSession(s)
	if err := s.RequestCancel("stale-lease"); err == nil {
		t.Fatal("stale request cancel succeeded")
	}
	assertSnapshotEqual(t, "stale RequestCancel", snap, snapshotSession(s))

	// 10. Stale completion on active turn
	snap = snapshotSession(s)
	if err := s.CompleteWithResult("wrong-key", "result"); err == nil {
		t.Fatal("wrong key completion succeeded")
	}
	assertSnapshotEqual(t, "wrong Complete", snap, snapshotSession(s))

	// 11. Attempt Archive while turn is active
	snap = snapshotSession(s)
	if err := s.Archive("valid-lease"); err == nil {
		t.Fatal("archive active turn succeeded")
	}
	assertSnapshotEqual(t, "active Archive", snap, snapshotSession(s))

	// 12. Complete t1 legitimately
	_ = s.CompleteWithResult("t1", "result 1")

	// 13. Attempt Queue with retired key t1
	snap = snapshotSession(s)
	if err := s.Queue("valid-lease", "t1", "different task"); err == nil {
		t.Fatal("queue retired key succeeded")
	}
	assertSnapshotEqual(t, "queue retired key", snap, snapshotSession(s))
}

func TestArchiveRetainsFullHistoryWithoutPurge(t *testing.T) {
	s, err := NewSession(OpenCode, "lease")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Queue("lease", "t1", "prompt 1")
	_, _ = s.Release("lease", "t1")
	_ = s.CompleteWithResult("t1", "result 1")

	_ = s.Queue("lease", "t2", "queued follow-up")

	// Archive the session
	if err := s.Archive("lease"); err != nil {
		t.Fatalf("archive failed: %v", err)
	}

	if s.State != Archived || s.Lifecycle != SessionArchived {
		t.Fatalf("session not archived: state=%s, lifecycle=%s", s.State, s.Lifecycle)
	}

	// Verify all records and pending prompts remain intact (no implicit purge)
	if s.Pending["t2"] != "queued follow-up" {
		t.Fatal("pending prompt purged on archive")
	}
	if s.Turns["t1"] == nil || s.Turns["t1"].Status != TurnCompleted || s.Turns["t1"].Result != "result 1" {
		t.Fatal("turn history purged on archive")
	}

	// Subsequent operations on archived session must be rejected without mutating records
	snap := snapshotSession(s)

	if err := s.Queue("lease", "t3", "prompt 3"); err == nil {
		t.Fatal("queue permitted on archived session")
	}
	assertSnapshotEqual(t, "archived Queue", snap, snapshotSession(s))

	if _, err := s.Release("lease", "t2"); err == nil {
		t.Fatal("release permitted on archived session")
	}
	assertSnapshotEqual(t, "archived Release", snap, snapshotSession(s))

	if err := s.Replace("lease", "t2", "altered"); err == nil {
		t.Fatal("replace permitted on archived session")
	}
	assertSnapshotEqual(t, "archived Replace", snap, snapshotSession(s))

	if err := s.Discard("lease", "t2"); err == nil {
		t.Fatal("discard permitted on archived session")
	}
	assertSnapshotEqual(t, "archived Discard", snap, snapshotSession(s))

	if err := s.RequestCancel("lease"); err == nil {
		t.Fatal("cancel permitted on archived session")
	}
	assertSnapshotEqual(t, "archived RequestCancel", snap, snapshotSession(s))
}
