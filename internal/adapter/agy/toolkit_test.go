//go:build unix

package agy

// Toolkit configured-state verification (AC-010 spec §3.2/§3.7): the
// live `plugin list` output is strictly decoded, canonicalized and
// digest-compared; the real skill directories must equal
// expected_skills; the operator hooks.json is strictly parsed, its
// canonical digest compared, and every required pointer must resolve to
// a non-empty, not-disabled entry. Any failure is ErrToolkitDrift before
// any process starts — at construction and before every launch.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
)

func requireToolkitDrift(t *testing.T, err error, component string) {
	t.Helper()
	var drift *ErrToolkitDrift
	if !errors.As(err, &drift) {
		t.Fatalf("want *ErrToolkitDrift, got %T: %v", err, err)
	}
	if drift.Component != component {
		t.Fatalf("drift component = %q, want %q (%v)", drift.Component, component, drift)
	}
}

// ── hooks ───────────────────────────────────────────────────────────────

func hooksHome(t *testing.T, raw string) (home, digest string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), ".gemini")
	return home, stageHooks(t, home, raw)
}

func writeHooks(t *testing.T, home, raw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config", "hooks.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write hooks.json: %v", err)
	}
}

func TestHooksConfig_Accepts(t *testing.T) {
	home, digest := hooksHome(t, defaultHooksJSON)
	if err := verifyHooksConfig(home, digest, []string{"/guardrail"}); err != nil {
		t.Fatalf("the frozen hooks must verify: %v", err)
	}
	// Byte-different, semantically identical (key order, whitespace):
	// the canonical digest is unchanged.
	writeHooks(t, home, "{\n  \"guardrail\" : {\"event\":\"PreToolUse\",\"enabled\":true,\n\"command\":\"guardrail.sh\"}\n}\n")
	if err := verifyHooksConfig(home, digest, []string{"/guardrail"}); err != nil {
		t.Fatalf("a whitespace/key-order variant must verify: %v", err)
	}
}

func TestHooksConfig_CanonicalDigestPreservesNumbers(t *testing.T) {
	a, err := CanonicalHooksConfigDigest([]byte(`{"g":{"timeout":1.50,"command":"x"}}`))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	b, err := CanonicalHooksConfigDigest([]byte(`{"g":{"command":"x","timeout":1.5}}`))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if a == b {
		t.Fatal("number literals are canonicalized verbatim (1.50 != 1.5), like the profile encoder")
	}
}

func TestHooksConfig_Drift(t *testing.T) {
	cases := map[string]struct {
		live     string
		required []string
	}{
		"canonical_bytes_mismatch": {`{"guardrail": {"command": "other.sh", "enabled": true, "event": "PreToolUse"}}`, []string{"/guardrail"}},
		"enabled_false":            {`{"guardrail": {"command": "guardrail.sh", "enabled": false, "event": "PreToolUse"}}`, []string{"/guardrail"}},
		"duplicate_key":            {`{"guardrail": {"command": "guardrail.sh", "enabled": true, "event": "PreToolUse", "event": "x"}}`, []string{"/guardrail"}},
		"trailing_content":         {defaultHooksJSON + ` {}`, []string{"/guardrail"}},
		"not_an_object":            {`[1]`, []string{"/guardrail"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home, digest := hooksHome(t, defaultHooksJSON)
			writeHooks(t, home, tc.live)
			requireToolkitDrift(t, verifyHooksConfig(home, digest, tc.required), "hooks")
		})
	}
	t.Run("missing_file", func(t *testing.T) {
		home, digest := hooksHome(t, defaultHooksJSON)
		if err := os.Remove(filepath.Join(home, "config", "hooks.json")); err != nil {
			t.Fatal(err)
		}
		requireToolkitDrift(t, verifyHooksConfig(home, digest, []string{"/guardrail"}), "hooks")
	})
}

// The required-pointer rules hold even when the frozen digest matches
// (a disabled entry frozen by mistake is still refused).
func TestHooksConfig_RequiredPointerRules(t *testing.T) {
	cases := map[string]struct {
		raw      string
		required []string
		ok       bool
	}{
		"missing_pointer":       {`{"guardrail": {"command": "g"}}`, []string{"/other"}, false},
		"empty_object":          {`{"guardrail": {}}`, []string{"/guardrail"}, false},
		"empty_string":          {`{"guardrail": ""}`, []string{"/guardrail"}, false},
		"null":                  {`{"guardrail": null}`, []string{"/guardrail"}, false},
		"nested_disabled_true":  {`{"guardrail": {"hooks": [{"command": "g", "disabled": true}]}}`, []string{"/guardrail"}, false},
		"nested_enabled_false":  {`{"guardrail": {"hooks": [{"command": "g", "enabled": false}]}}`, []string{"/guardrail"}, false},
		"empty_command":         {`{"guardrail": {"command": ""}}`, []string{"/guardrail"}, false},
		"empty_handler":         {`{"guardrail": {"handler": "  "}}`, []string{"/guardrail"}, false},
		"non_string_command":    {`{"guardrail": {"command": null, "x": 1}}`, []string{"/guardrail"}, false},
		"array_index":           {`{"hooks": [{"command": "a"}, {"command": "b"}]}`, []string{"/hooks/1"}, true},
		"array_index_oob":       {`{"hooks": [{"command": "a"}]}`, []string{"/hooks/1"}, false},
		"escaped_tokens":        {`{"a/b": {"c~d": {"command": "x"}}}`, []string{"/a~1b/c~0d"}, true},
		"disabled_false_ok":     {`{"guardrail": {"command": "g", "disabled": false, "enabled": true}}`, []string{"/guardrail"}, true},
		"unrelated_disabled_ok": {`{"guardrail": {"command": "g"}, "other": {"enabled": false}}`, []string{"/guardrail"}, true},
		"two_pointers_one_bad":  {`{"a": {"command": "x"}, "b": {"enabled": false, "command": "y"}}`, []string{"/a", "/b"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home, digest := hooksHome(t, tc.raw)
			err := verifyHooksConfig(home, digest, tc.required)
			if tc.ok {
				if err != nil {
					t.Fatalf("must verify: %v", err)
				}
				return
			}
			requireToolkitDrift(t, err, "hooks")
		})
	}
}

// ── skills ──────────────────────────────────────────────────────────────

func skillsHome(t *testing.T, dirs ...string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".gemini")
	root := filepath.Join(home, "antigravity-cli", "skills")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestSkills_SetEquality(t *testing.T) {
	home := skillsHome(t, "research", "tdd")
	if err := verifySkills(home, []string{"tdd", "research"}); err != nil {
		t.Fatalf("equal sets must verify: %v", err)
	}
	// A regular file is not a skill directory and is ignored.
	if err := os.WriteFile(filepath.Join(home, "antigravity-cli", "skills", "README"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySkills(home, []string{"research", "tdd"}); err != nil {
		t.Fatalf("a regular file is not a skill: %v", err)
	}
	requireToolkitDrift(t, verifySkills(home, []string{"research"}), "skills")
	requireToolkitDrift(t, verifySkills(home, []string{"research", "tdd", "extra"}), "skills")
}

func TestSkills_SymlinkRefused(t *testing.T) {
	home := skillsHome(t, "research")
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(home, "antigravity-cli", "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	requireToolkitDrift(t, verifySkills(home, []string{"research"}), "skills")
	requireToolkitDrift(t, verifySkills(home, []string{"linked", "research"}), "skills")
}

func TestSkills_SymlinkedSkillsRootRefused(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	if err := os.MkdirAll(filepath.Join(home, "antigravity-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	real := t.TempDir()
	if err := os.Symlink(real, filepath.Join(home, "antigravity-cli", "skills")); err != nil {
		t.Fatal(err)
	}
	requireToolkitDrift(t, verifySkills(home, nil), "skills")
}

func TestSkills_AbsentDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	if err := verifySkills(home, []string{}); err != nil {
		t.Fatalf("no skills directory is the empty set: %v", err)
	}
	requireToolkitDrift(t, verifySkills(home, []string{"research"}), "skills")
}

// ── plugin list ─────────────────────────────────────────────────────────

const committedPluginList = `{"imports":[{"components":["hooks","skills"],"importedAt":"2026-09-02T19:59:12Z","name":"superpowers","source":"gemini-cli"}]}`

func TestPluginList_CanonicalRederivation(t *testing.T) {
	want := digestOf([]byte(committedPluginList))
	if err := verifyPluginList([]byte(committedPluginList+"\n"), want); err != nil {
		t.Fatalf("the committed bytes must verify: %v", err)
	}
	variant := "{ \"imports\": [ {\"name\":\"superpowers\", \"source\":\"gemini-cli\",\n \"importedAt\":\"2026-09-02T19:59:12Z\", \"components\":[\"skills\",\"hooks\"]} ] }\n"
	if err := verifyPluginList([]byte(variant), want); err != nil {
		t.Fatalf("a byte-different, semantically identical live output must verify: %v", err)
	}
	for name, live := range map[string]string{
		"drift_text":      "plugin list drifted: no imports",
		"empty":           "",
		"semantic_change": strings.Replace(committedPluginList, "2026-09-02T19:59:12Z", "2026-09-03T00:00:00Z", 1),
		"extra_import":    `{"imports":[{"components":["hooks"],"importedAt":"2026-09-02T19:59:12Z","name":"a","source":"s"},{"components":["hooks","skills"],"importedAt":"2026-09-02T19:59:12Z","name":"superpowers","source":"gemini-cli"}]}`,
		"unknown_key":     `{"imports":[],"extra":1}`,
		"trailing":        committedPluginList + " {}",
	} {
		t.Run(name, func(t *testing.T) {
			requireToolkitDrift(t, verifyPluginList([]byte(live), want), "plugins")
		})
	}
}

// ── adapter integration (fixture) ───────────────────────────────────────

func TestToolkit_PluginListDriftRefusesCreationPreChild(t *testing.T) {
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "`+testNativeID+`"}`, `{"plugin_list_drift": "plugin list drifted: no imports"}`)
	_, err := h.adapter.CreateSession(context.Background(), h.createRequest())
	requireToolkitDrift(t, err, "plugins")
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("toolkit drift refuses before the creation child starts, launches=%d", n)
	}
	if got := h.gateLines("plugin\x1flist"); len(got) != 1 {
		t.Fatalf("the live plugin list is re-derived through the executor once, got %v", got)
	}
}

func TestToolkit_RunsBeforeEveryLaunch(t *testing.T) {
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "` + testNativeID + `"}`)
	if _, err := h.adapter.CreateSession(context.Background(), h.createRequest()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	h.persist(testNativeID)
	h.turnScenario(userInputDone, successResult("ok"))
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	h.waitIdle(h.ref("t1"))
	if got := h.gateLines("plugin\x1flist"); len(got) != 2 {
		t.Fatalf("plugin list re-derived before the creation AND the turn launch, got %v", got)
	}
	for _, env := range h.fixtureFile(".agy-fixture-gate-env") {
		if env != "HOME="+filepath.Dir(h.policy.ExpectedHome) {
			t.Fatalf("gate launches inherit the operator home, got %q", env)
		}
	}
}

func TestToolkit_HooksDriftRefusesDispatchPreReservation(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(userInputDone, successResult("ok"))
	writeHooks(t, h.home, `{"guardrail": {"command": "guardrail.sh", "enabled": false, "event": "PreToolUse"}}`)

	out, err := h.dispatch("t1", "p")
	requireToolkitDrift(t, err, "hooks")
	if out.Status != adapter.DispatchRejected {
		t.Fatalf("toolkit drift is a clean pre-write rejection, got %+v", out)
	}
	if n := h.exec.starts.Load() + h.exec.gateStarts.Load(); n != 0 {
		t.Fatalf("filesystem drift refuses before any process, launches=%d", n)
	}
	if a, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1"); a != nil {
		t.Fatalf("nothing durable is recorded for a refused launch, got %+v", a)
	}
}

func TestToolkit_SkillsDriftRefusesLaterLaunch(t *testing.T) {
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "` + testNativeID + `"}`)
	if _, err := h.adapter.CreateSession(context.Background(), h.createRequest()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	h.persist(testNativeID)
	if err := os.MkdirAll(filepath.Join(h.home, "antigravity-cli", "skills", "unfrozen"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.turnScenario(userInputDone, successResult("ok"))
	_, err := h.dispatch("t1", "p")
	requireToolkitDrift(t, err, "skills")
	if n := h.exec.starts.Load(); n != 1 {
		t.Fatalf("only the creation child ran, launches=%d", n)
	}
}
