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

type SessionLifecycle string

const (
	SessionActive   SessionLifecycle = "active"
	SessionArchived SessionLifecycle = "archived"
)

type TurnStatus string

const (
	TurnPending     TurnStatus = "pending"
	TurnRunning     TurnStatus = "running"
	TurnCompleted   TurnStatus = "completed"
	TurnInterrupted TurnStatus = "interrupted"
	TurnCancelling  TurnStatus = "cancelling"
	TurnCancelled   TurnStatus = "cancelled"
	TurnFailed      TurnStatus = "failed"
)

type ControllerConnection string

const (
	ControllerConnected    ControllerConnection = "connected"
	ControllerDisconnected ControllerConnection = "disconnected"
)

type ExecutionVisibility string

const (
	VisibilityReachable ExecutionVisibility = "reachable"
	VisibilityHostLost  ExecutionVisibility = "host_lost"
)

type TurnRecord struct {
	Key    string
	Prompt string
	Status TurnStatus
	Result string
}

// Session stores logical state. It is not concurrency-safe or durable; the future
// transactional store owns those responsibilities. Never use it as an auth boundary.
type Session struct {
	ID                 Contributor
	State              State
	Lifecycle          SessionLifecycle
	ControllerStatus   ControllerConnection
	Visibility         ExecutionVisibility
	ControllerLease    string
	Pending            map[string]string
	Active             string
	ActiveTurn         *TurnRecord
	Turns              map[string]*TurnRecord
	RecoveryContext    string
	RecoveryGeneration uint64
	ActiveRecoveryGen  uint64
}

func NewSession(id Contributor, lease string) (*Session, error) {
	if !ValidContributor(id) || strings.TrimSpace(lease) == "" {
		return nil, errors.New("invalid contributor or lease")
	}
	return &Session{
		ID:               id,
		State:            Parked,
		Lifecycle:        SessionActive,
		ControllerStatus: ControllerConnected,
		Visibility:       VisibilityReachable,
		ControllerLease:  lease,
		Pending:          map[string]string{},
		Turns:            map[string]*TurnRecord{},
	}, nil
}

func (s *Session) authorize(lease string) error {
	if lease == "" || lease != s.ControllerLease {
		return errors.New("controller lease mismatch")
	}
	if s.Lifecycle == SessionArchived || s.State == Archived {
		return errors.New("session archived")
	}
	if s.ControllerStatus == ControllerDisconnected {
		return errors.New("controller disconnected")
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
	if prior, exists := s.Turns[key]; exists {
		if prior.Prompt != prompt {
			return errors.New("retired turn identifier cannot be reused with different content")
		}
		return errors.New("turn identifier already retired")
	}
	if existingPrompt, ok := s.Pending[key]; ok {
		if existingPrompt == prompt {
			return nil // Idempotent submission of identical pending prompt.
		}
		return errors.New("duplicate pending key with different content")
	}
	s.Pending[key] = prompt
	return nil
}

func (s *Session) Release(lease, key string) (string, error) {
	if err := s.authorize(lease); err != nil {
		return "", err
	}
	if s.Visibility == VisibilityHostLost {
		return "", errors.New("host lost; reconciliation required before release")
	}
	if s.ActiveTurn != nil && s.ActiveTurn.Status == TurnCancelling {
		return "", errors.New("cancellation unresolved; release blocked")
	}
	if s.State != Parked {
		return "", errors.New("session already processing a turn")
	}
	prompt, ok := s.Pending[key]
	if !ok {
		return "", errors.New("pending prompt not found")
	}
	delete(s.Pending, key)
	turn := &TurnRecord{
		Key:    key,
		Prompt: prompt,
		Status: TurnRunning,
	}
	s.Turns[key] = turn
	s.ActiveTurn = turn
	s.Active, s.State = key, Running
	return prompt, nil
}

func (s *Session) Complete(key string) error {
	return s.CompleteWithResult(key, "")
}

func (s *Session) CompleteWithResult(key, result string) error {
	if prior, exists := s.Turns[key]; exists && prior.Status == TurnCompleted {
		return errors.New("turn already completed")
	}
	if s.State != Running || s.Active != key || s.ActiveTurn == nil {
		return errors.New("completion does not match active turn")
	}
	s.ActiveTurn.Status = TurnCompleted
	s.ActiveTurn.Result = result
	s.Active = ""
	s.ActiveTurn = nil
	s.State = Parked // Never auto-dispatch another pending prompt.
	return nil
}

func (s *Session) RequestCancel(lease string) error {
	if err := s.authorize(lease); err != nil {
		return err
	}
	if s.State != Running || s.ActiveTurn == nil || s.ActiveTurn.Status != TurnRunning {
		return errors.New("no active running turn to cancel")
	}
	s.ActiveTurn.Status = TurnCancelling
	return nil
}

func (s *Session) ConfirmCancel(key string) error {
	if s.Active != key || s.ActiveTurn == nil || s.ActiveTurn.Status != TurnCancelling {
		return errors.New("cancel confirmation does not match cancelling turn")
	}
	s.ActiveTurn.Status = TurnCancelled
	s.Active = ""
	s.ActiveTurn = nil
	s.State = Parked
	return nil
}

func (s *Session) Interrupt(key string) error {
	if s.Active != key || s.ActiveTurn == nil || (s.ActiveTurn.Status != TurnRunning && s.ActiveTurn.Status != TurnCancelling) {
		return errors.New("interruption does not match active turn")
	}
	s.ActiveTurn.Status = TurnInterrupted
	s.Active = ""
	s.ActiveTurn = nil
	s.State = Parked
	return nil
}

func (s *Session) Fail(key, reason string) error {
	if s.Active != key || s.ActiveTurn == nil {
		return errors.New("failure does not match active turn")
	}
	s.ActiveTurn.Status = TurnFailed
	s.ActiveTurn.Result = reason
	s.Active = ""
	s.ActiveTurn = nil
	s.State = Parked
	return nil
}

func (s *Session) RecordHostLoss() (uint64, error) {
	if s.Visibility == VisibilityHostLost {
		return s.ActiveRecoveryGen, nil
	}
	if s.State != Running || s.ActiveTurn == nil {
		return 0, errors.New("cannot record host loss on inactive session")
	}
	s.RecoveryGeneration++
	s.ActiveRecoveryGen = s.RecoveryGeneration
	s.Visibility = VisibilityHostLost
	s.RecoveryContext = s.Active
	return s.ActiveRecoveryGen, nil
}

func (s *Session) ReconcileHost(key string, gen uint64, status TurnStatus, result string) error {
	if s.Visibility != VisibilityHostLost {
		return errors.New("session host is not lost")
	}
	if key == "" {
		return errors.New("empty turn identity")
	}
	if gen == 0 || gen != s.ActiveRecoveryGen {
		return errors.New("stale or invalid recovery generation")
	}

	// Active turn exists
	if s.ActiveTurn != nil {
		if key != s.Active {
			if _, exists := s.Turns[key]; exists {
				return errors.New("stale reconciliation for past turn")
			}
			return errors.New("reconciliation key does not match active turn")
		}

		switch status {
		case TurnRunning:
			if s.ActiveTurn.Status != TurnCancelling {
				s.ActiveTurn.Status = TurnRunning
			}
			s.Visibility = VisibilityReachable
			s.RecoveryContext = ""
			s.ActiveRecoveryGen = 0
			s.State = Running
			return nil
		case TurnCancelling:
			s.ActiveTurn.Status = TurnCancelling
			s.Visibility = VisibilityReachable
			s.RecoveryContext = ""
			s.ActiveRecoveryGen = 0
			s.State = Running
			return nil
		case TurnCompleted, TurnCancelled, TurnFailed, TurnInterrupted:
			s.ActiveTurn.Status = status
			s.ActiveTurn.Result = result
			s.Active = ""
			s.ActiveTurn = nil
			s.State = Parked
			s.Visibility = VisibilityReachable
			s.RecoveryContext = ""
			s.ActiveRecoveryGen = 0
			return nil
		default:
			return errors.New("invalid reconciliation target status")
		}
	}

	// No active turn (e.g. terminal delivery arrived while host was lost)
	if key != s.RecoveryContext {
		if _, exists := s.Turns[key]; exists {
			return errors.New("stale reconciliation for past turn")
		}
		return errors.New("reconciliation key does not match recovery context")
	}

	switch status {
	case TurnCompleted, TurnCancelled, TurnFailed, TurnInterrupted:
		s.Visibility = VisibilityReachable
		s.RecoveryContext = ""
		s.ActiveRecoveryGen = 0
		return nil
	case TurnRunning, TurnCancelling:
		return errors.New("reconciliation status conflicts with recorded terminal outcome")
	case TurnPending:
		return errors.New("cannot reconcile with pending status")
	default:
		return errors.New("empty or unknown status")
	}
}

func (s *Session) DisconnectController() {
	s.ControllerStatus = ControllerDisconnected
}

func (s *Session) ReconnectController(lease string) error {
	if lease == "" || lease != s.ControllerLease {
		return errors.New("controller lease mismatch")
	}
	if s.Lifecycle == SessionArchived || s.State == Archived {
		return errors.New("session archived")
	}
	s.ControllerStatus = ControllerConnected
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

func (s *Session) Replace(lease, key, newPrompt string) error {
	if err := s.authorize(lease); err != nil {
		return err
	}
	if strings.TrimSpace(newPrompt) == "" {
		return errors.New("empty prompt")
	}
	if _, ok := s.Pending[key]; !ok {
		return errors.New("pending prompt not found")
	}
	s.Pending[key] = newPrompt
	return nil
}

func (s *Session) Archive(lease string) error {
	if err := s.authorize(lease); err != nil {
		return err
	}
	if s.State == Running || s.ActiveTurn != nil || s.Visibility == VisibilityHostLost {
		return errors.New("cannot archive a running turn")
	}
	s.State = Archived // Retains pending data; archive does not purge.
	s.Lifecycle = SessionArchived
	return nil
}

func (s *Session) String() string { return fmt.Sprintf("%s: %s", s.ID, s.State) }
