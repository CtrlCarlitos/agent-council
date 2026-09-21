package storage

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestCanonicalProfile_BriefDigest(t *testing.T) {
	// Empty brief
	emptyDigest, err := ComputeBriefDigest("")
	if err != nil {
		t.Fatalf("unexpected error for empty brief: %v", err)
	}
	expectedEmpty := fmt.Sprintf("cbrief-v1:sha256:%x", sha256.Sum256([]byte("")))
	if emptyDigest != expectedEmpty {
		t.Fatalf("expected empty brief digest %s, got %s", expectedEmpty, emptyDigest)
	}

	// Normal text with BOM stripped
	rawText := "Task brief content for Council"
	textWithBOM := "\ufeff" + rawText + "\ufeff"
	d1, err := ComputeBriefDigest(rawText)
	if err != nil {
		t.Fatalf("brief digest: %v", err)
	}
	d2, err := ComputeBriefDigest(textWithBOM)
	if err != nil {
		t.Fatalf("brief digest with BOM: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("BOM was not trimmed: %s != %s", d1, d2)
	}

	expectedPrefix := "cbrief-v1:sha256:"
	if !strings.HasPrefix(d1, expectedPrefix) {
		t.Fatalf("expected prefix %s, got %s", expectedPrefix, d1)
	}

	// NFC Unicode normalization equivalence
	// e + combining acute accent (\u0065\u0301) vs composed é (\u00e9)
	decomposed := "caf\u0065\u0301"
	composed := "caf\u00e9"
	dDecomp, err := ComputeBriefDigest(decomposed)
	if err != nil {
		t.Fatalf("decomposed: %v", err)
	}
	dComp, err := ComputeBriefDigest(composed)
	if err != nil {
		t.Fatalf("composed: %v", err)
	}
	if dDecomp != dComp {
		t.Fatalf("NFC normalization failed: %s != %s", dDecomp, dComp)
	}

	// Invalid UTF-8
	if _, err := ComputeBriefDigest("\xff\xfe"); err == nil {
		t.Fatal("expected error for invalid UTF-8 in brief")
	}
}

func TestCanonicalProfile_SourceDigest(t *testing.T) {
	// mode == "none"
	noneDigest, err := ComputeSourceDigest("none", "", "", "")
	if err != nil {
		t.Fatalf("ComputeSourceDigest(none): %v", err)
	}
	if noneDigest != "csource-v1:none" {
		t.Fatalf("expected csource-v1:none, got %s", noneDigest)
	}

	// mode == "none" with dummy args still produces "csource-v1:none"
	noneDigest2, err := ComputeSourceDigest("none", "/some/path", "d670460b4b4aece5915caf5c68d12f560a9fe3e4", "2b66236b2803b9b47e85c2c77d48dc9e414cbeeb")
	if err != nil {
		t.Fatalf("ComputeSourceDigest(none with args): %v", err)
	}
	if noneDigest2 != "csource-v1:none" {
		t.Fatalf("expected csource-v1:none, got %s", noneDigest2)
	}

	// Valid readonly & isolated_branch
	commit := "d670460b4b4aece5915caf5c68d12f560a9fe3e4"
	tree := "2b66236b2803b9b47e85c2c77d48dc9e414cbeeb"
	repo := "/home/user/repo"

	roDigest, err := ComputeSourceDigest("readonly", repo, commit, tree)
	if err != nil {
		t.Fatalf("ComputeSourceDigest(readonly): %v", err)
	}
	branchDigest, err := ComputeSourceDigest("isolated_branch", repo, commit, tree)
	if err != nil {
		t.Fatalf("ComputeSourceDigest(isolated_branch): %v", err)
	}
	if roDigest != branchDigest {
		t.Fatalf("readonly and isolated_branch with same inputs should produce same source_digest: %s vs %s", roDigest, branchDigest)
	}

	// Check exact formula output:
	// Canonical source JSON: {"algo_version":"csource-v1","commit":"d670460b4b4aece5915caf5c68d12f560a9fe3e4","repo_identity":"/home/user/repo","tree":"2b66236b2803b9b47e85c2c77d48dc9e414cbeeb"}
	expectedJSON := `{"algo_version":"csource-v1","commit":"` + commit + `","repo_identity":"` + repo + `","tree":"` + tree + `"}`
	expectedDigest := fmt.Sprintf("csource-v1:sha256:%x", sha256.Sum256([]byte(expectedJSON)))
	if roDigest != expectedDigest {
		t.Fatalf("expected %s, got %s", expectedDigest, roDigest)
	}

	// Validation failures
	if _, err := ComputeSourceDigest("invalid_mode", repo, commit, tree); err == nil {
		t.Fatal("expected error for invalid mode")
	}
	if _, err := ComputeSourceDigest("readonly", "", commit, tree); err == nil {
		t.Fatal("expected error for empty repo_identity")
	}
	if _, err := ComputeSourceDigest("readonly", repo, "short_sha", tree); err == nil {
		t.Fatal("expected error for invalid commit sha")
	}
	if _, err := ComputeSourceDigest("readonly", repo, commit, "invalid_tree_sha!"); err == nil {
		t.Fatal("expected error for invalid tree sha")
	}
}

func TestCanonicalProfile_ProfileDigest(t *testing.T) {
	prof1 := CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "isolated_branch",
		IsolationStrictness: "strict",
		NetworkMode:         "allowlist",
		NetworkAllowlist:    []string{"api.openai.com:443", "api.anthropic.com:443"},
		CodeIndexScope:      []string{"internal/", "cmd/"},
		Tooling:             []string{"test", "git", "go"},
		Harnesses: map[string]HarnessProfileSpec{
			"codex":    {Model: "o3-mini", NativeAuthMode: "inherited_host_keychain", ExtraEnvAllowlist: []string{}},
			"agy":      {Model: "gemini-2.5-pro", NativeAuthMode: "inherited_host_keychain", ExtraEnvAllowlist: []string{"GEMINI_CLI_PROFILE"}},
			"claude":   {Model: "claude-3-7-sonnet", NativeAuthMode: "inherited_host_keychain", ExtraEnvAllowlist: []string{}},
			"opencode": {Model: "glm-4", NativeAuthMode: "inherited_host_keychain", ExtraEnvAllowlist: []string{}},
		},
	}

	// prof2 has reversed slices, duplicate items, and uppercase tool names
	prof2 := CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "isolated_branch",
		IsolationStrictness: "strict",
		NetworkMode:         "allowlist",
		NetworkAllowlist:    []string{"api.anthropic.com:443", "api.openai.com:443", "api.anthropic.com:443"},
		CodeIndexScope:      []string{"cmd/", "internal/", "cmd/"},
		Tooling:             []string{"GO", "git", "TEST", "go"},
		Harnesses: map[string]HarnessProfileSpec{
			"opencode": {Model: "glm-4", NativeAuthMode: "inherited_host_keychain", ExtraEnvAllowlist: []string{}},
			"claude":   {Model: "claude-3-7-sonnet", NativeAuthMode: "inherited_host_keychain", ExtraEnvAllowlist: []string{}},
			"agy":      {Model: "gemini-2.5-pro", NativeAuthMode: "inherited_host_keychain", ExtraEnvAllowlist: []string{"GEMINI_CLI_PROFILE"}},
			"codex":    {Model: "o3-mini", NativeAuthMode: "inherited_host_keychain", ExtraEnvAllowlist: []string{}},
		},
	}

	digest1, json1, err := ComputeProfileDigest(prof1)
	if err != nil {
		t.Fatalf("ComputeProfileDigest(prof1): %v", err)
	}
	digest2, json2, err := ComputeProfileDigest(prof2)
	if err != nil {
		t.Fatalf("ComputeProfileDigest(prof2): %v", err)
	}

	if digest1 != digest2 {
		t.Fatalf("normalized digests must match: %s vs %s", digest1, digest2)
	}
	if string(json1) != string(json2) {
		t.Fatalf("normalized JSON must match:\n1: %s\n2: %s", string(json1), string(json2))
	}

	if !strings.HasPrefix(digest1, "cprof-v1:sha256:") {
		t.Fatalf("expected prefix cprof-v1:sha256:, got %s", digest1)
	}

	// Verify key ordering in canonical json (RFC 8785 / lexicographical)
	expectedJSON := `{"algo_version":"cprof-v1","code_index_scope":["cmd","internal"],"harnesses":{"agy":{"extra_env_allowlist":["GEMINI_CLI_PROFILE"],"model":"gemini-2.5-pro","native_auth_mode":"inherited_host_keychain"},"claude":{"extra_env_allowlist":[],"model":"claude-3-7-sonnet","native_auth_mode":"inherited_host_keychain"},"codex":{"extra_env_allowlist":[],"model":"o3-mini","native_auth_mode":"inherited_host_keychain"},"opencode":{"extra_env_allowlist":[],"model":"glm-4","native_auth_mode":"inherited_host_keychain"}},"isolation_strictness":"strict","network_allowlist":["api.anthropic.com:443","api.openai.com:443"],"network_mode":"allowlist","tooling":["git","go","test"],"workspace_mode":"isolated_branch"}`
	if string(json1) != expectedJSON {
		t.Fatalf("canonical JSON mismatch:\ngot:      %s\nexpected: %s", string(json1), expectedJSON)
	}
	expectedDigest := fmt.Sprintf("cprof-v1:sha256:%x", sha256.Sum256([]byte(expectedJSON)))
	if digest1 != expectedDigest {
		t.Fatalf("digest mismatch: got %s, expected %s", digest1, expectedDigest)
	}

	// Rejection of invalid algorithm version
	badAlgo := prof1
	badAlgo.AlgoVersion = "cprof-v2"
	if _, _, err := ComputeProfileDigest(badAlgo); err == nil {
		t.Fatal("expected error for invalid AlgoVersion")
	}

	// Rejection of invalid workspace_mode
	badMode := prof1
	badMode.WorkspaceMode = "invalid_mode"
	if _, _, err := ComputeProfileDigest(badMode); err == nil {
		t.Fatal("expected error for invalid WorkspaceMode")
	}

	// Rejection of invalid isolation_strictness
	badStrict := prof1
	badStrict.IsolationStrictness = "loose"
	if _, _, err := ComputeProfileDigest(badStrict); err == nil {
		t.Fatal("expected error for invalid IsolationStrictness")
	}

	// Rejection of invalid network_mode
	badNet := prof1
	badNet.NetworkMode = "all"
	if _, _, err := ComputeProfileDigest(badNet); err == nil {
		t.Fatal("expected error for invalid NetworkMode")
	}
}

func TestCanonicalProfile_DisallowUnknownFields(t *testing.T) {
	validJSON := `{
		"algo_version": "cprof-v1",
		"workspace_mode": "none",
		"isolation_strictness": "permissive_dev",
		"network_mode": "unrestricted",
		"network_allowlist": [],
		"code_index_scope": [],
		"tooling": ["git"],
		"harnesses": {}
	}`

	prof, err := ParseCanonicalProfileJSON([]byte(validJSON))
	if err != nil {
		t.Fatalf("ParseCanonicalProfileJSON valid: %v", err)
	}
	if prof.AlgoVersion != "cprof-v1" {
		t.Fatalf("expected cprof-v1, got %s", prof.AlgoVersion)
	}

	// JSON with unknown field must be rejected
	unknownFieldJSON := `{
		"algo_version": "cprof-v1",
		"workspace_mode": "none",
		"isolation_strictness": "permissive_dev",
		"network_mode": "unrestricted",
		"network_allowlist": [],
		"code_index_scope": [],
		"tooling": ["git"],
		"harnesses": {},
		"unexpected_key": "not_allowed"
	}`

	if _, err := ParseCanonicalProfileJSON([]byte(unknownFieldJSON)); err == nil {
		t.Fatal("expected error for unknown field in profile JSON")
	}
}
