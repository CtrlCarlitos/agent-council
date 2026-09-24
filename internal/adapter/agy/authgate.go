package agy

// The `models` auth gate (AC-010 spec §3.2 gate step 2). `init` proves
// nothing about sign-in, so before the FIRST dispatch of a logical
// session — and again before the next dispatch after ResumeSession — the
// adapter runs the frozen `<binary> models` inventory launch through the
// executor (sealed, verified, the operator HOME; network-bound, no model
// call), closes its stdin at once, waits bounded, and classifies its
// output before any prompt exists:
//
//   - the live-verified not-signed-in text ⇒ ErrAgyAuthRequired;
//   - a parsed catalog lacking the frozen model ⇒ ErrProfileDrift;
//   - a parsed catalog containing it, exit 0 ⇒ pass;
//   - anything else (start failure, timeout, overflow, non-zero exit
//     without the sign-in text, empty or unparseable output) ⇒
//     ErrAgyAuthInconclusive (fail closed).
//
// Exit codes never make a pass: a non-zero exit is at best inconclusive.
// A pass is cached per session for authGateTTL (default 5 minutes) so
// consecutive turns do not spawn `models` each time; failures are never
// cached, and ResumeSession drops the session's entry.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
)

// ErrAgyAuthRequired reports the not-signed-in outcome of the `models`
// auth gate: the operator must sign in natively (Council never copies
// or relocates credentials). No turn process was started.
type ErrAgyAuthRequired struct {
	Detail string
}

func (e *ErrAgyAuthRequired) Error() string {
	return "agy auth required: the models gate reports no signed-in operator (" + e.Detail + ")"
}

// ErrAgyAuthInconclusive reports a `models` auth gate that could not
// establish sign-in (transport/network failure, timeout, non-zero exit,
// empty or unparseable catalog). Fail closed: no turn process started.
type ErrAgyAuthInconclusive struct {
	Reason string
}

func (e *ErrAgyAuthInconclusive) Error() string {
	return "agy auth inconclusive: " + e.Reason
}

var (
	// authGateTimeout bounds one `models` launch end to end.
	authGateTimeout = 30 * time.Second
)

// defaultAuthGateTTL is how long a passed gate stays valid for a
// session; each adapter copies it into its authTTL (tests may shorten
// it per adapter, 0 disables caching).
const defaultAuthGateTTL = 5 * time.Minute

// notSignedInMarkers are the recognized unauthenticated texts: the
// live-verified `models` text (spec §3.2) and the CLI's own
// "not logged into Antigravity" line (research §3, the fixture's text).
var notSignedInMarkers = []string{
	"Please sign in to view available models",
	"You are not logged into Antigravity",
}

// modelIDPattern is one catalog row's id (the first tab-separated
// field): no whitespace, no punctuation beyond the id alphabet.
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

// classifyModelsOutput applies the gate's three-outcome rule to one
// completed `models` launch.
func classifyModelsOutput(stdout, stderr []byte, exitCode int, model string) error {
	for _, stream := range [][]byte{stdout, stderr} {
		for _, marker := range notSignedInMarkers {
			if strings.Contains(string(stream), marker) {
				return &ErrAgyAuthRequired{Detail: marker}
			}
		}
	}
	if exitCode != 0 {
		return &ErrAgyAuthInconclusive{Reason: fmt.Sprintf("models exited %d without a recognizable sign-in text", exitCode)}
	}
	var ids []string
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		id := strings.TrimSpace(strings.SplitN(line, "\t", 2)[0])
		if !modelIDPattern.MatchString(id) {
			return &ErrAgyAuthInconclusive{Reason: fmt.Sprintf("models output is not a catalog (row %q)", clampPayload(line))}
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return &ErrAgyAuthInconclusive{Reason: "models printed an empty catalog"}
	}
	// A lone plain word (a header or an error/status word such as
	// "Models:", "Error" or "Loading" — no digit, no '-', unlike every
	// catalog id) proves no catalog was printed: inconclusive, never a
	// model drift.
	if len(ids) == 1 && ids[0] != model && !strings.ContainsAny(ids[0], "0123456789-") {
		return &ErrAgyAuthInconclusive{Reason: fmt.Sprintf("models printed a single plain word %q, not a catalog", clampPayload(ids[0]))}
	}
	for _, id := range ids {
		if id == model {
			return nil
		}
	}
	return &ErrProfileDrift{Field: "models catalog", Want: model, Have: strings.Join(ids, ",")}
}

// authGate runs (or reuses a cached pass of) the `models` gate for a
// session before its turn launch.
func (a *AgyAdapter) authGate(ctx context.Context, sessionID adapter.SessionID) error {
	a.authMu.Lock()
	passed, ok := a.authPassed[sessionID]
	now := a.now()
	a.authMu.Unlock()
	if ok && a.authTTL > 0 && now.Sub(passed) < a.authTTL {
		return nil
	}
	if err := a.runAuthGate(ctx, sessionID); err != nil {
		a.invalidateAuth(sessionID)
		return err
	}
	a.authMu.Lock()
	a.authPassed[sessionID] = a.now()
	a.authMu.Unlock()
	return nil
}

// invalidateAuth drops a session's cached pass (ResumeSession, failure).
func (a *AgyAdapter) invalidateAuth(sessionID adapter.SessionID) {
	a.authMu.Lock()
	delete(a.authPassed, sessionID)
	a.authMu.Unlock()
}

func (a *AgyAdapter) runAuthGate(ctx context.Context, sessionID adapter.SessionID) error {
	req, err := a.launch.AgyTurnLaunch(ctx, sessionID, "", "", LaunchModels)
	if err != nil {
		return &ErrAgyAuthInconclusive{Reason: "models launch: " + err.Error()}
	}
	if err := a.validateLaunch(ctx, req, sessionID, LaunchModels, "", a.policy.Model, ""); err != nil {
		return err
	}
	res, err := runCapture(a.executor, req, authGateTimeout)
	if err != nil {
		if err == errCaptureTimeout {
			return &ErrAgyAuthInconclusive{Reason: fmt.Sprintf("models did not finish within the %s bound", authGateTimeout)}
		}
		return &ErrAgyAuthInconclusive{Reason: err.Error()}
	}
	if res.overflow {
		return &ErrAgyAuthInconclusive{Reason: "models output exceeded the capture bound"}
	}
	return classifyModelsOutput(res.stdout, res.stderr, res.exitCode, a.policy.Model)
}
