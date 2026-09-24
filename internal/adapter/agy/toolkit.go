package agy

// Toolkit configured-state verification (AC-010 spec §3.2/§3.7), at
// production construction and before EVERY creation and turn launch:
//
//   - skills: the names of the REAL directories directly under
//     <expected_home>/antigravity-cli/skills must equal expected_skills
//     (a symlinked entry, or a symlinked skills root, is drift: a
//     skill Council cannot pin is never silently admitted);
//   - hooks: <expected_home>/config/hooks.json is parsed strictly (one
//     JSON value, no duplicate keys, no trailing content, an object),
//     re-encoded canonically (sorted keys, no insignificant whitespace,
//     number literals verbatim) and its sha256 must equal
//     hooks_config_digest; every required_hooks RFC 6901 pointer must
//     resolve to a non-empty value whose subtree carries no
//     "enabled": false / "disabled": true marker and whose
//     "command"/"handler" members, when present, are non-empty strings;
//   - plugins: `agy plugin list` is re-run through the executor (sealed,
//     verified, provider-free) and its live stdout strictly decoded,
//     canonicalized, and digest-compared with plugins_evidence_digest.
//
// Any failure is ErrToolkitDrift before the guarded process starts. The
// filesystem checks run first so a drifted home refuses without any
// child. What this proves is configured state only: that Agy ran a hook
// or loaded a skill on a given call is NOT claimed (spec §3.2 gap).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/evidence"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
)

// ErrToolkitDrift reports configured toolkit state (Component: "skills",
// "hooks", or "plugins") that differs from the frozen profile. No
// guarded process was started.
type ErrToolkitDrift struct {
	Component string
	Reason    string
}

func (e *ErrToolkitDrift) Error() string {
	return fmt.Sprintf("agy toolkit drift (%s): %s", e.Component, e.Reason)
}

// toolkitCaptureTimeout bounds one `plugin list` launch end to end.
var toolkitCaptureTimeout = 30 * time.Second

// maxHooksConfigBytes bounds the hooks.json read.
const maxHooksConfigBytes = 1 << 20

func skillsDir(expectedHome string) string {
	return filepath.Join(expectedHome, "antigravity-cli", "skills")
}

func hooksConfigPath(expectedHome string) string {
	return filepath.Join(expectedHome, "config", "hooks.json")
}

func sha256Digest(raw []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}

// ── skills ──────────────────────────────────────────────────────────────

// verifySkills requires the real skill directories to equal expected.
// An absent skills directory is the empty set.
func verifySkills(expectedHome string, expected []string) error {
	drift := func(format string, args ...any) error {
		return &ErrToolkitDrift{Component: "skills", Reason: fmt.Sprintf(format, args...)}
	}
	dir := skillsDir(expectedHome)
	var live []string
	st, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return drift("stat %s: %v", dir, err)
	case st.Mode()&os.ModeSymlink != 0 || !st.IsDir():
		return drift("%s is not a real directory", dir)
	default:
		entries, err := os.ReadDir(dir)
		if err != nil {
			return drift("list %s: %v", dir, err)
		}
		for _, e := range entries {
			switch {
			case e.Type()&os.ModeSymlink != 0:
				return drift("skills entry %q is a symlink; only real directories are admitted", e.Name())
			case e.IsDir():
				live = append(live, e.Name())
			}
		}
	}
	want := sortedUniqueTrimmed(expected)
	got := sortedUniqueTrimmed(live)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		return drift("live skills %v are not the frozen expected_skills %v", got, want)
	}
	return nil
}

// ── hooks ───────────────────────────────────────────────────────────────

// decodeHooksConfig strictly parses a hooks.json capture into a generic
// tree (numbers kept as their literal json.Number).
func decodeHooksConfig(raw []byte) (map[string]any, error) {
	var probe any
	if err := evidence.DecodeStrictObject(raw, &probe); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	obj, ok := tree.(map[string]any)
	if !ok {
		return nil, errors.New("hooks config must be a JSON object")
	}
	return obj, nil
}

// CanonicalHooksConfigDigest is the hooks_config_digest of a hooks.json
// capture (spec §3.7): sha256 over the canonical re-encoding of the
// strictly parsed document. Operator freeze tooling and the adapter's
// live check share this one derivation.
func CanonicalHooksConfigDigest(raw []byte) (string, error) {
	tree, err := decodeHooksConfig(raw)
	if err != nil {
		return "", err
	}
	canon, err := evidence.Canonical(tree)
	if err != nil {
		return "", err
	}
	return sha256Digest(canon), nil
}

// verifyHooksConfig re-parses the live hooks.json, compares its
// canonical digest, and checks every required pointer.
func verifyHooksConfig(expectedHome, wantDigest string, required []string) error {
	drift := func(format string, args ...any) error {
		return &ErrToolkitDrift{Component: "hooks", Reason: fmt.Sprintf(format, args...)}
	}
	path := hooksConfigPath(expectedHome)
	f, err := os.Open(path)
	if err != nil {
		return drift("read %s: %v", path, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxHooksConfigBytes+1))
	_ = f.Close()
	if err != nil {
		return drift("read %s: %v", path, err)
	}
	if len(raw) > maxHooksConfigBytes {
		return drift("%s exceeds %d bytes", path, maxHooksConfigBytes)
	}
	tree, err := decodeHooksConfig(raw)
	if err != nil {
		return drift("strict parse of %s: %v", path, err)
	}
	canon, err := evidence.Canonical(tree)
	if err != nil {
		return drift("canonicalize %s: %v", path, err)
	}
	if got := sha256Digest(canon); got != wantDigest {
		return drift("canonical digest %s is not the frozen hooks_config_digest %s", got, wantDigest)
	}
	for _, ptr := range required {
		v, err := resolvePointer(tree, ptr)
		if err != nil {
			return drift("required hook %q: %v", ptr, err)
		}
		if isEmptyValue(v) {
			return drift("required hook %q resolves to an empty value", ptr)
		}
		if err := checkHookSubtree(v, ptr); err != nil {
			return drift("required hook %q: %v", ptr, err)
		}
	}
	return nil
}

// resolvePointer resolves an RFC 6901 JSON pointer in tree.
func resolvePointer(tree any, ptr string) (any, error) {
	if ptr == "" {
		return tree, nil
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, fmt.Errorf("%q is not a JSON pointer", ptr)
	}
	cur := tree
	for _, tok := range strings.Split(ptr[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[tok]
			if !ok {
				return nil, fmt.Errorf("member %q does not exist", tok)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(node) || (len(tok) > 1 && tok[0] == '0') {
				return nil, fmt.Errorf("array index %q does not exist", tok)
			}
			cur = node[i]
		default:
			return nil, fmt.Errorf("cannot descend into a scalar at %q", tok)
		}
	}
	return cur, nil
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

// checkHookSubtree refuses a disabled marker anywhere in the subtree and
// an empty or non-string command/handler member.
func checkHookSubtree(v any, at string) error {
	switch node := v.(type) {
	case map[string]any:
		if b, ok := node["enabled"].(bool); ok && !b {
			return fmt.Errorf("%s carries \"enabled\": false", at)
		}
		if b, ok := node["disabled"].(bool); ok && b {
			return fmt.Errorf("%s carries \"disabled\": true", at)
		}
		for _, key := range []string{"command", "handler"} {
			raw, present := node[key]
			if !present {
				continue
			}
			s, ok := raw.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return fmt.Errorf("%s has an empty or non-string %q", at, key)
			}
		}
		keys := make([]string, 0, len(node))
		for k := range node {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := checkHookSubtree(node[k], at+"/"+k); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range node {
			if err := checkHookSubtree(item, fmt.Sprintf("%s/%d", at, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ── plugins ─────────────────────────────────────────────────────────────

// verifyPluginList strictly decodes and canonicalizes a live `plugin
// list` stdout and requires its digest to equal the frozen one.
func verifyPluginList(stdout []byte, wantDigest string) error {
	canon, err := canonicalPluginsBytes(bytes.TrimSpace(stdout))
	if err != nil {
		return &ErrToolkitDrift{Component: "plugins", Reason: "live plugin list is not the pinned shape: " + err.Error()}
	}
	if got := sha256Digest(canon); got != wantDigest {
		return &ErrToolkitDrift{Component: "plugins",
			Reason: fmt.Sprintf("live plugin list canonical digest %s is not the frozen plugins_evidence_digest %s", got, wantDigest)}
	}
	return nil
}

// capturePluginList runs one `plugin list` launch and verifies it.
func capturePluginList(executor execpolicy.PolicyExecutor, req execpolicy.LaunchRequest, wantDigest string) error {
	res, err := runCapture(executor, req, toolkitCaptureTimeout)
	if err != nil {
		return &ErrToolkitDrift{Component: "plugins", Reason: "plugin list capture: " + err.Error()}
	}
	if res.overflow {
		return &ErrToolkitDrift{Component: "plugins", Reason: "plugin list output exceeded the capture bound"}
	}
	if res.exitCode != 0 {
		return &ErrToolkitDrift{Component: "plugins", Reason: fmt.Sprintf("plugin list exited %d", res.exitCode)}
	}
	return verifyPluginList(res.stdout, wantDigest)
}

// verifyToolkitFiles runs the filesystem half (skills, hooks).
func verifyToolkitFiles(policy AgyLaunchPolicy) error {
	if err := verifySkills(policy.ExpectedHome, policy.ExpectedSkills); err != nil {
		return err
	}
	return verifyHooksConfig(policy.ExpectedHome, policy.HooksConfigDigest, policy.RequiredHooks)
}

// toolkitCheck is the adapter's per-launch pre-launch check: the
// filesystem half first (no child on drift), then the session-scoped
// `plugin list` launch through the adapter's launch source, validated
// like every launch.
func (a *AgyAdapter) toolkitCheck(ctx context.Context, sessionID adapter.SessionID) error {
	if err := verifyToolkitFiles(a.policy); err != nil {
		return err
	}
	req, err := a.launch.AgyTurnLaunch(ctx, sessionID, "", "", LaunchPluginList)
	if err != nil {
		return &ErrToolkitDrift{Component: "plugins", Reason: "plugin list launch: " + err.Error()}
	}
	if err := a.validateLaunch(ctx, req, sessionID, LaunchPluginList, "", a.policy.Model, ""); err != nil {
		return err
	}
	return capturePluginList(a.executor, req, a.policy.PluginsEvidenceDigest)
}
