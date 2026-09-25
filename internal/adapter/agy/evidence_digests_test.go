//go:build evidence

package agy

// AC-010 operator evidence helper (Stage A of
// scripts/ac010-integration-evidence.sh). NOT part of CI: compiled only
// with `-tags evidence`, and a no-op unless the script supplies the
// AC010_DIGEST_* environment. It derives the freeze-time digests with
// the EXACT functions the adapter re-derives at every launch, so the
// operator never hand-computes a canonical form:
//
//   - plugins: canonicalPluginsBytes(<raw `agy plugin list` stdout>) is
//     written to AC010_DIGEST_PLUGINS_CANONICAL (the candidate committed
//     evidence file) and its sha256 is the plugins_evidence_digest;
//   - hooks: CanonicalHooksConfigDigest(<hooks.json bytes>) is the
//     hooks_config_digest. Only the digest and the top-level key names
//     are reported — the hooks file content is never copied.
//
// Inputs: AC010_DIGEST_PLUGINS_RAW (optional path), AC010_DIGEST_HOOKS
// (optional path), AC010_DIGEST_OUT (report path, required when either
// input is set), AC010_DIGEST_PLUGINS_CANONICAL (required with
// AC010_DIGEST_PLUGINS_RAW).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestEvidenceCanonicalDigests(t *testing.T) {
	pluginsRaw, hooksPath := os.Getenv("AC010_DIGEST_PLUGINS_RAW"), os.Getenv("AC010_DIGEST_HOOKS")
	if pluginsRaw == "" && hooksPath == "" {
		t.Skip("operator evidence helper: set AC010_DIGEST_* (scripts/ac010-integration-evidence.sh)")
	}
	out := os.Getenv("AC010_DIGEST_OUT")
	if out == "" {
		t.Fatal("AC010_DIGEST_OUT is required")
	}
	var report strings.Builder
	if pluginsRaw != "" {
		canonPath := os.Getenv("AC010_DIGEST_PLUGINS_CANONICAL")
		if canonPath == "" {
			t.Fatal("AC010_DIGEST_PLUGINS_CANONICAL is required with AC010_DIGEST_PLUGINS_RAW")
		}
		raw, err := os.ReadFile(pluginsRaw)
		if err != nil {
			t.Fatalf("read plugin list capture: %v", err)
		}
		canon, err := canonicalPluginsBytes(bytes.TrimSpace(raw))
		if err != nil {
			fmt.Fprintf(&report, "plugins_canonical=REFUSED (%v)\n", err)
		} else {
			if err := os.WriteFile(canonPath, canon, 0o600); err != nil {
				t.Fatalf("write canonical plugins: %v", err)
			}
			fmt.Fprintf(&report, "plugins_evidence_digest=%s\n", sha256Digest(canon))
		}
	}
	if hooksPath != "" {
		raw, err := os.ReadFile(hooksPath)
		switch {
		case os.IsNotExist(err):
			fmt.Fprintf(&report, "hooks_config=ABSENT\n")
		case err != nil:
			t.Fatalf("read hooks config: %v", err)
		default:
			digest, derr := CanonicalHooksConfigDigest(raw)
			if derr != nil {
				fmt.Fprintf(&report, "hooks_config_digest=REFUSED (%v)\n", derr)
				break
			}
			fmt.Fprintf(&report, "hooks_config_digest=%s\n", digest)
			var top map[string]json.RawMessage
			if json.Unmarshal(raw, &top) == nil {
				keys := make([]string, 0, len(top))
				for k := range top {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				fmt.Fprintf(&report, "hooks_top_level_keys=%s\n", strings.Join(keys, ","))
			}
		}
	}
	if err := os.WriteFile(out, []byte(report.String()), 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Log(report.String())
}
