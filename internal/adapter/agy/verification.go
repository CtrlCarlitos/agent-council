package agy

// Required-tool verification (AC-010 spec §3.5, issue #10 criterion).
// Native tool names are the single namespace; result.denied_actions
// speaks a different vocabulary, so the version-pinned denial_map
// translates each (action, display_name) pair to the native tools it CAN
// deny, and attribution is intersected with what THIS process was
// observed to attempt:
//
//	attributed = denial_map(entry) ∩ observed tool steps
//	|attributed| = 1  ⇒ that tool is denied
//	|attributed| > 1  ⇒ ambiguous: all attributed tools not executed
//	|attributed| = 0  ⇒ unattributed (never blamed on an unattempted tool)
//	pair not in map   ⇒ unmapped: every observed tool not executed
//
// executed(T) ⇔ T observed AND not denied/ambiguous AND no unmapped
// denial. verification_incomplete ⇔ a required tool was not executed or
// any denial class is non-empty. Exit 0 / SUCCESS never clears it.

import (
	"sort"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// AgyVerification is the tool-verification evidence recorded once at the
// attempt's terminal (the storage shape, re-exported for callers).
type AgyVerification = storage.AgyVerification

type denialKind int

const (
	denialAttributed denialKind = iota + 1
	denialAmbiguous
	denialUnattributed
	denialUnmapped
)

// denialClass is one classified result.denied_actions entry: the raw
// pair ("action/display_name") and, when attributable, the native tools.
type denialClass struct {
	Pair  string
	Kind  denialKind
	Tools []string
}

func denialPair(d DeniedAction) string {
	return d.Action + "/" + d.DisplayName
}

// classifyDenials classifies each denial entry in input order.
func classifyDenials(observedToolSteps []string, denied []DeniedAction, m CoverageMap) []denialClass {
	observed := make(map[string]struct{}, len(observedToolSteps))
	for _, t := range observedToolSteps {
		observed[t] = struct{}{}
	}
	out := make([]denialClass, 0, len(denied))
	for _, d := range denied {
		c := denialClass{Pair: denialPair(d)}
		entry, ok := lookupDenial(m, d)
		if !ok {
			c.Kind = denialUnmapped
			out = append(out, c)
			continue
		}
		for _, tool := range entry.Tools {
			if _, seen := observed[tool]; seen {
				c.Tools = append(c.Tools, tool)
			}
		}
		c.Tools = sortedUnique(c.Tools)
		switch len(c.Tools) {
		case 0:
			c.Kind = denialUnattributed
		case 1:
			c.Kind = denialAttributed
		default:
			c.Kind = denialAmbiguous
		}
		out = append(out, c)
	}
	return out
}

func lookupDenial(m CoverageMap, d DeniedAction) (DenialEntry, bool) {
	for _, e := range m.DenialMap {
		if e.Action == d.Action && e.DisplayName == d.DisplayName {
			return e, true
		}
	}
	return DenialEntry{}, false
}

// ComputeVerification derives the §3.5 verification evidence for one
// process from the attempt's required tools, the tool_name of every tool
// step observed DONE in that process, the result's denied_actions, and
// the pinned coverage map. Empty classes are nil (storage encodes both
// nil and empty as "[]").
func ComputeVerification(required []string, observedToolSteps []string, denied []DeniedAction, m CoverageMap) AgyVerification {
	var v AgyVerification
	notExecuted := map[string]struct{}{}
	unmapped := false
	for _, c := range classifyDenials(observedToolSteps, denied, m) {
		switch c.Kind {
		case denialAttributed:
			v.Denied = append(v.Denied, c.Tools...)
		case denialAmbiguous:
			v.Ambiguous = append(v.Ambiguous, c.Pair)
			v.Denied = append(v.Denied, c.Tools...)
		case denialUnattributed:
			v.Unattributed = append(v.Unattributed, c.Pair)
		case denialUnmapped:
			v.Unmapped = append(v.Unmapped, c.Pair)
			unmapped = true
		}
	}
	v.Denied = sortedUnique(v.Denied)
	for _, t := range v.Denied {
		notExecuted[t] = struct{}{}
	}
	if !unmapped {
		for _, t := range sortedUnique(observedToolSteps) {
			if _, no := notExecuted[t]; !no {
				v.Executed = append(v.Executed, t)
			}
		}
	}
	executed := make(map[string]struct{}, len(v.Executed))
	for _, t := range v.Executed {
		executed[t] = struct{}{}
	}
	for _, t := range sortedUnique(required) {
		if _, ok := executed[t]; !ok {
			v.MissingRequired = append(v.MissingRequired, t)
		}
	}
	v.Ambiguous = sortedUnique(v.Ambiguous)
	v.Unattributed = sortedUnique(v.Unattributed)
	v.Unmapped = sortedUnique(v.Unmapped)
	v.Incomplete = len(v.MissingRequired) > 0 || len(v.Denied) > 0 || len(v.Ambiguous) > 0 ||
		len(v.Unattributed) > 0 || len(v.Unmapped) > 0
	return v
}

// sortedUnique returns the sorted distinct values of in (nil for empty).
func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
