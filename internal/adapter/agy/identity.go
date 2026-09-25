package agy

// Turn and session identity for the agy adapter (AC-010 spec §3.3/§3.5):
// the pdig-v1 prompt digest is codex's framing reused verbatim (never
// duplicated), native conversation ids are UUIDv4-shaped exactly like
// the agy_session_bindings schema CHECK, and the service-owned seams
// that hand the adapter a turn's attempt identity, its frozen required
// tools, and the production-eligibility attestation.

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
)

// AttemptIdentitySource supplies the persisted attempt identity for a
// TurnRef before dispatch (the AC-007/AC-008/AC-009 seam).
type AttemptIdentitySource interface {
	AttemptFor(ctx context.Context, ref adapter.TurnRef) (attempt string, ok bool)
}

// RequiredToolsSource supplies the required_tools set journaled with the
// queued prompt (spec §3.5: validated at queue time, immutable
// thereafter). err != nil — a storage read failure, or a turn with no
// durable dispatch intent (dispatch cannot legitimately reach the lookup
// without one) — makes the adapter refuse the dispatch before any
// reservation (*ErrRequiredToolsUnavailable, DispatchRejected). ok=false
// with a nil error means ONLY "the intent is present and its set is
// empty": the frozen default_required_tools apply.
type RequiredToolsSource interface {
	RequiredToolsFor(ctx context.Context, ref adapter.TurnRef) (tools []string, ok bool, err error)
}

// ErrRequiredToolsUnavailable is the pre-transmission refusal of a
// dispatch whose journaled required_tools could not be read: the adapter
// never substitutes the frozen defaults for an unreadable set.
type ErrRequiredToolsUnavailable struct {
	SessionID adapter.SessionID
	TurnKey   string
	Err       error
}

func (e *ErrRequiredToolsUnavailable) Error() string {
	return fmt.Sprintf("required_tools for %s/%s are unavailable; refusing the dispatch before any reservation: %v",
		e.SessionID, e.TurnKey, e.Err)
}

func (e *ErrRequiredToolsUnavailable) Unwrap() error { return e.Err }

// AttestationLookup reports a valid isolation attestation for the exact
// frozen tuple (spec §3.2 gate step 1). Production construction requires
// it; there is no inference of eligibility from absent evidence.
type AttestationLookup func() (attestationID string, ok bool)

// promptDigest is the pdig-v1 framing over (native conversation id,
// turn key, attempt id, prompt) — codex.PromptDigest reused directly.
func promptDigest(nativeID, turnKey, attemptID, prompt string) (string, error) {
	return codex.PromptDigest(nativeID, turnKey, attemptID, prompt)
}

// isValidUUIDv4 mirrors the agy_session_bindings.native_id CHECK: the
// canonical lowercase 8-4-4-4-12 hex shape with version nibble 4.
func isValidUUIDv4(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		case 14:
			if c != '4' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// conversationPath is the durable native conversation file for id under
// the frozen agy home (spec §3.4).
func conversationPath(home, nativeID string) string {
	return filepath.Join(home, "antigravity-cli", "conversations", nativeID+".db")
}
