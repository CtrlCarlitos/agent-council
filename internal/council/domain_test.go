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
