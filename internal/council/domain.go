// Package council implements a small in-memory domain kernel, not process supervision.
package council

import (
	"errors"
	"fmt"
	"strings"
)

type Contributor string

const (
	OpenCode Contributor = "opencode"
	Claude   Contributor = "claude"
	Codex    Contributor = "codex"
	Agy      Contributor = "agy"
)

func Roster() []Contributor { return []Contributor{OpenCode, Claude, Codex, Agy} }

func ValidContributor(id Contributor) bool {
	for _, v := range Roster() {
		if v == id {
			return true
		}
	}
	return false
}

type Ballot struct {
	Voter       Contributor
	Choice      Contributor
	ProposalSet string
	Reason      string
	Acceptable  bool // Preference is deliberately distinct from acceptance.
}

// Tally requires the full quartet, sealed against one proposal-set identifier.
// It does not infer a winner or approve a synthesized artifact.
func Tally(ballots []Ballot, proposalSet string) (map[Contributor]int, error) {
	if strings.TrimSpace(proposalSet) == "" {
		return nil, errors.New("missing proposal-set identifier")
	}
	if len(ballots) != 4 {
		return nil, errors.New("exactly four contributor ballots required")
	}
	out := map[Contributor]int{}
	seen := map[Contributor]bool{}
	for _, c := range Roster() {
		out[c] = 0
	}
	for _, b := range ballots {
		if !ValidContributor(b.Voter) || !ValidContributor(b.Choice) {
			return nil, errors.New("unknown contributor")
		}
		if b.Voter == b.Choice {
			return nil, errors.New("self-vote prohibited")
		}
		if seen[b.Voter] {
			return nil, errors.New("duplicate contributor ballot")
		}
		if b.ProposalSet != proposalSet {
			return nil, errors.New("stale proposal-set identifier")
		}
		if strings.TrimSpace(b.Reason) == "" {
			return nil, errors.New("a task-specific reason is required")
		}
		seen[b.Voter] = true
		out[b.Choice]++
	}
	return out, nil
}

type State string

const (
	Parked   State = "parked"
	Running  State = "running"
	Archived State = "archived"
)

// Session stores logical state. It is not concurrency-safe or durable; the future
// transactional store owns those responsibilities. Never use it as an auth boundary.
type Session struct {
	ID              Contributor
	State           State
	ControllerLease string
	Pending         map[string]string
	Active          string
}

func NewSession(id Contributor, lease string) (*Session, error) {
	if !ValidContributor(id) || strings.TrimSpace(lease) == "" {
		return nil, errors.New("invalid contributor or lease")
	}
	return &Session{ID: id, State: Parked, ControllerLease: lease, Pending: map[string]string{}}, nil
}

func (s *Session) authorize(lease string) error {
	if lease == "" || lease != s.ControllerLease {
		return errors.New("controller lease mismatch")
	}
	if s.State == Archived {
		return errors.New("session archived")
	}
	return nil
}

func (s *Session) Queue(lease, key, prompt string) error {
	if err := s.authorize(lease); err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" || strings.TrimSpace(prompt) == "" {
		return errors.New("empty prompt or key")
	}
	if s.Active == key {
		return errors.New("key already active")
	}
	if _, ok := s.Pending[key]; ok {
		return errors.New("duplicate pending key")
	}
	s.Pending[key] = prompt
	return nil
}

func (s *Session) Release(lease, key string) (string, error) {
	if err := s.authorize(lease); err != nil {
		return "", err
	}
	if s.State != Parked {
		return "", errors.New("session already processing a turn")
	}
	prompt, ok := s.Pending[key]
	if !ok {
		return "", errors.New("pending prompt not found")
	}
	delete(s.Pending, key)
	s.Active, s.State = key, Running
	return prompt, nil
}

func (s *Session) Complete(key string) error {
	if s.State != Running || s.Active != key {
		return errors.New("completion does not match active turn")
	}
	s.Active, s.State = "", Parked // Never auto-dispatch another pending prompt.
	return nil
}

func (s *Session) Discard(lease, key string) error {
	if err := s.authorize(lease); err != nil {
		return err
	}
	if _, ok := s.Pending[key]; !ok {
		return errors.New("pending prompt not found")
	}
	delete(s.Pending, key)
	return nil
}

func (s *Session) Archive(lease string) error {
	if err := s.authorize(lease); err != nil {
		return err
	}
	if s.State == Running {
		return errors.New("cannot archive a running turn")
	}
	s.State = Archived // Retains pending data; archive does not purge.
	return nil
}

func (s *Session) String() string { return fmt.Sprintf("%s: %s", s.ID, s.State) }
