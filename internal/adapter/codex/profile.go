package codex

// Provider-free validation of the frozen codex harness block: the codex
// adapter requires cprof-v3 with a complete harnesses.codex block and
// re-hashes the committed event universe against its frozen digest
// (AC-009 spec §3.8). Everything fails closed before any process is
// started.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/evidence"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ErrUnsupportedProfile reports a profile that is not eligible for the
// codex adapter (wrong algo_version, or a v3 profile without a complete
// codex harness block).
type ErrUnsupportedProfile struct {
	AlgoVersion string
	Reason      string
}

func (e *ErrUnsupportedProfile) Error() string {
	return fmt.Sprintf("profile algorithm %q is unsupported for this adapter: %s", e.AlgoVersion, e.Reason)
}

// CodexLaunchPolicy is the verified frozen codex policy for a run, ready
// for the launch seam. ApprovalPolicyCanonical is the canonical JSON
// encoding of the frozen approval policy (string literal or granular
// object); effective-config comparison (§3.5) treats any byte
// difference against the native side as drift. ManifestDigest is the
// canonical toolkit-manifest digest (storage.ComputeToolkitManifestDigest)
// frozen at validation — the same value the §3.7 protection-attestation
// journal operation stores, so the protected-evidence tuple match
// (version, platform, manifest, profile) is exact.
type CodexLaunchPolicy struct {
	AppServerVersion           string
	ModelProvider              string
	ExpectedCodexHome          string
	PlatformOS                 string
	PlatformFamily             string
	SandboxType                string
	WritableRoots              []string
	NetworkAccess              bool
	ApprovalPolicyCanonical    string
	ApprovalsReviewer          string
	ExpectedMCPServers         []string
	ExpectedInstructionSources []string
	RulesVerified              []string
	RulesUnverifiable          []string
	EventUniversePath          string
	EventUniverseDigest        string
	ManifestDigest             string
	// ExpectedMCPTools is the frozen EXACT MCP tool-path inventory
	// ("<server>/<tool>", trimmed, deduplicated, byte-wise sorted): the
	// MCP half of the attestation coverage universe (coverage.go).
	ExpectedMCPTools []string
	// PluginTools is the frozen EXACT skill/plugin-contributed tool
	// inventory: the plugin half of the coverage universe.
	PluginTools []string
}

// ValidateCodexHarness validates the frozen codex harness block of a run
// profile against the committed evidence rooted at evidenceRoot: the
// profile must be cprof-v3 with a complete block, approvals must be
// answered by the user, and the event universe file is re-hashed and
// must match the frozen digest. Any incompleteness fails closed with
// ErrUnsupportedProfile.
func ValidateCodexHarness(profile storage.CanonicalProfile, evidenceRoot string) (CodexLaunchPolicy, error) {
	// cprof-v4 is additive (AC-010 spec §3.7 compatibility matrix): the
	// Codex adapter accepts it too, with the codex block validated
	// exactly as under cprof-v3.
	if profile.AlgoVersion != "cprof-v3" && profile.AlgoVersion != "cprof-v4" {
		return CodexLaunchPolicy{}, &ErrUnsupportedProfile{
			AlgoVersion: profile.AlgoVersion,
			Reason:      "the Codex adapter requires cprof-v3 or cprof-v4 with a complete codex harness block",
		}
	}
	spec, ok := profile.Harnesses["codex"]
	if !ok || spec.Codex == nil {
		return CodexLaunchPolicy{}, &ErrUnsupportedProfile{
			AlgoVersion: profile.AlgoVersion,
			Reason:      "cprof-v3 profile lacks the required harnesses.codex block",
		}
	}
	c := spec.Codex

	unsupported := func(reason string) (CodexLaunchPolicy, error) {
		return CodexLaunchPolicy{}, &ErrUnsupportedProfile{AlgoVersion: profile.AlgoVersion, Reason: reason}
	}

	// cprof-v3 requires the toolkit manifest; its canonical digest is
	// part of the frozen launch tuple (§3.7 protected-evidence match).
	if profile.ToolkitManifest == nil {
		return unsupported("profile requires toolkit_manifest")
	}
	manifestDigest, err := storage.ComputeToolkitManifestDigest(profile.ToolkitManifest.ToolkitManifest)
	if err != nil {
		return unsupported("toolkit manifest digest: " + err.Error())
	}

	if strings.TrimSpace(c.AppServerVersion) == "" {
		return unsupported("codex block lacks the app_server_version")
	}
	if strings.TrimSpace(c.ModelProvider) == "" {
		return unsupported("codex block lacks the model_provider")
	}
	if strings.TrimSpace(c.ExpectedCodexHome) == "" {
		return unsupported("codex block lacks the expected_codex_home")
	}
	if strings.TrimSpace(c.Platform.OS) == "" {
		return unsupported("codex block lacks the platform os")
	}
	if strings.TrimSpace(c.Platform.Family) == "" {
		return unsupported("codex block lacks the platform family")
	}
	if strings.TrimSpace(c.SandboxPolicy.Type) == "" {
		return unsupported("codex block lacks the sandbox_policy type")
	}
	switch c.SandboxPolicy.Type {
	case "read-only", "workspace-write":
	default:
		return unsupported(fmt.Sprintf("sandbox_policy type %q is forbidden: only read-only and workspace-write are launchable (spec §5)", c.SandboxPolicy.Type))
	}
	if len(c.SandboxPolicy.WritableRoots) == 0 {
		return unsupported("codex block sandbox_policy requires writable_roots")
	}
	if strings.TrimSpace(c.ApprovalsReviewer) != "user" {
		return unsupported("approvals_reviewer must be \"user\": only the user answers approvals inside Council's visibility")
	}
	if c.ExpectedMCPServers == nil {
		return unsupported("codex block lacks expected_mcp_servers")
	}
	if c.ExpectedMCPTools == nil {
		return unsupported("codex block lacks expected_mcp_tools (the exact MCP tool inventory; [] when no server exposes tools)")
	}
	mcpServers := make(map[string]struct{}, len(c.ExpectedMCPServers))
	for _, s := range c.ExpectedMCPServers {
		mcpServers[strings.TrimSpace(s)] = struct{}{}
	}
	seenMCPTools := make(map[string]struct{}, len(c.ExpectedMCPTools))
	for _, path := range c.ExpectedMCPTools {
		path = strings.TrimSpace(path)
		server, tool, ok := strings.Cut(path, "/")
		if !ok || strings.TrimSpace(server) == "" || strings.TrimSpace(tool) == "" {
			return unsupported(fmt.Sprintf("expected_mcp_tools entry %q is not <server>/<tool>", path))
		}
		if _, known := mcpServers[server]; !known {
			return unsupported(fmt.Sprintf("expected_mcp_tools entry %q names a server outside expected_mcp_servers", path))
		}
		if _, dup := seenMCPTools[path]; dup {
			return unsupported(fmt.Sprintf("expected_mcp_tools entry %q is duplicated", path))
		}
		seenMCPTools[path] = struct{}{}
	}
	if c.ExpectedPluginTools == nil {
		return unsupported("codex block lacks expected_plugin_tools (the exact plugin tool inventory; [] when none)")
	}
	seenPlugins := make(map[string]struct{}, len(c.ExpectedPluginTools))
	for _, name := range c.ExpectedPluginTools {
		name = strings.TrimSpace(name)
		if name == "" {
			return unsupported("expected_plugin_tools carries an empty tool name")
		}
		if _, dup := seenPlugins[name]; dup {
			return unsupported(fmt.Sprintf("expected_plugin_tools entry %q is duplicated", name))
		}
		seenPlugins[name] = struct{}{}
	}
	if c.ExpectedInstructionSources == nil {
		return unsupported("codex block lacks expected_instruction_sources")
	}
	if c.RulesEvidence.Verified == nil && c.RulesEvidence.Unverifiable == nil {
		return unsupported("codex block lacks rules_evidence")
	}
	if strings.TrimSpace(c.EventUniversePath) == "" {
		return unsupported("codex block lacks the event_universe_path")
	}
	if err := storage.ValidateSHA256Digest(c.EventUniverseDigest); err != nil {
		return unsupported("event_universe_digest: " + err.Error())
	}

	policyCanonical, err := canonicalApprovalPolicyEncoding(c.ApprovalPolicy)
	if err != nil {
		return unsupported(err.Error())
	}

	if err := rehashEventUniverse(evidenceRoot, c.EventUniversePath, c.EventUniverseDigest); err != nil {
		return CodexLaunchPolicy{}, err
	}
	// Inventory-evidence gate (fail closed, honest gap): no evidence path
	// exists yet that PROVES a non-empty MCP/plugin inventory equals the
	// installed native inventory — the mcpServerStatus/list response
	// shape is not pinned by committed schema evidence, so no
	// deterministic parser can bind the raw response, and no native
	// plugin/skill inventory surface is pinned at all. A committed
	// Council-shaped capture is operator-edited JSON, not native
	// evidence. Until a schema-pinned raw-response derivation (or an
	// operator attestation binding the raw-response digest with a
	// verified native plugin source) exists, a profile enabling ANY MCP
	// server or plugin tool is not launchable. Empty inventories are
	// unaffected: the dispatch-time server-level drift check keeps them
	// affirmatively empty.
	if len(c.ExpectedMCPServers) > 0 || len(c.ExpectedMCPTools) > 0 || len(c.ExpectedPluginTools) > 0 {
		return unsupported("the profile enables MCP servers or plugin tools, but no committed evidence path proves a non-empty native tool inventory (the mcpServerStatus/list response shape and the native plugin inventory surface are not schema-pinned); production is not launchable for MCP/plugin-enabled profiles until that evidence exists — only empty inventories are eligible")
	}
	if err := verifyToolInventoryEvidence(evidenceRoot, c); err != nil {
		return CodexLaunchPolicy{}, &ErrUnsupportedProfile{AlgoVersion: profile.AlgoVersion, Reason: err.Error()}
	}

	return CodexLaunchPolicy{
		AppServerVersion:           c.AppServerVersion,
		ModelProvider:              c.ModelProvider,
		ExpectedCodexHome:          c.ExpectedCodexHome,
		PlatformOS:                 c.Platform.OS,
		PlatformFamily:             c.Platform.Family,
		SandboxType:                c.SandboxPolicy.Type,
		WritableRoots:              append([]string(nil), c.SandboxPolicy.WritableRoots...),
		NetworkAccess:              c.SandboxPolicy.NetworkAccess,
		ApprovalPolicyCanonical:    policyCanonical,
		ApprovalsReviewer:          c.ApprovalsReviewer,
		ExpectedMCPServers:         append([]string(nil), c.ExpectedMCPServers...),
		ExpectedInstructionSources: append([]string(nil), c.ExpectedInstructionSources...),
		RulesVerified:              append([]string(nil), c.RulesEvidence.Verified...),
		RulesUnverifiable:          append([]string(nil), c.RulesEvidence.Unverifiable...),
		EventUniversePath:          c.EventUniversePath,
		EventUniverseDigest:        c.EventUniverseDigest,
		ManifestDigest:             manifestDigest,
		ExpectedMCPTools:           sortedUniqueTrimmed(c.ExpectedMCPTools),
		PluginTools:                sortedUniqueTrimmed(c.ExpectedPluginTools),
	}, nil
}

// canonicalApprovalPolicyEncoding renders the frozen approval policy as
// canonical JSON (sorted keys, no insignificant whitespace, number
// literals verbatim) for byte-exact effective-config comparison.
func canonicalApprovalPolicyEncoding(p storage.CodexApprovalPolicy) (string, error) {
	switch p.Kind {
	case storage.CodexApprovalKindString:
		b, err := json.Marshal(p.String)
		if err != nil {
			return "", fmt.Errorf("encode approval_policy: %w", err)
		}
		return string(b), nil
	case storage.CodexApprovalKindGranular:
		if p.Granular == nil {
			return "", fmt.Errorf("approval_policy granular kind carries no granular object")
		}
		raws := map[string]json.RawMessage{
			"mcp_elicitations":    p.Granular.McpElicitations,
			"request_permissions": p.Granular.RequestPermissions,
			"rules":               p.Granular.Rules,
			"sandbox_approval":    p.Granular.SandboxApproval,
			"skill_approval":      p.Granular.SkillApproval,
		}
		out := make(map[string]any, len(raws))
		for key, raw := range raws {
			if len(raw) == 0 || string(raw) == "null" {
				return "", fmt.Errorf("granular approval_policy is missing required key %q", key)
			}
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				return "", fmt.Errorf("granular approval_policy key %q: %w", key, err)
			}
			out[key] = v
		}
		b, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("encode granular approval_policy: %w", err)
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("approval_policy must be a string or a granular object")
	}
}

// rehashEventUniverse resolves the repo-relative universe path inside the
// trusted evidence root (symlink-safe containment), re-hashes the raw
// bytes, and requires an exact digest match.
func rehashEventUniverse(evidenceRoot, relPath, wantDigest string) error {
	_, err := evidence.ReadFile(evidenceRoot, relPath, wantDigest, "event universe", "event_universe_path")
	return err
}

// NativeToolInventory is the provider-free native tool-inventory capture
// (Council-defined JSON, committed under the evidence root): the MCP
// servers with the tools each exposes, and the skill/plugin-contributed
// tools, for one installed codex version. It is the evidence that
// PROVES the frozen expected_mcp_tools / expected_plugin_tools lists
// complete — a profile can no longer omit an enabled tool and still
// pass exact-set coverage.
type NativeToolInventory struct {
	CodexCLIVersion string              `json:"codex_cli_version"`
	MCPServers      map[string][]string `json:"mcp_servers"`
	PluginTools     []string            `json:"plugin_tools"`
}

// verifyToolInventoryEvidence is the inventory-equality rule the
// evidence path will be held to once a native-derived capture can be
// bound (ValidateCodexHarness currently refuses every non-empty
// inventory before reaching it — see the gate there): whenever the
// profile enables any MCP server or plugin tool, the digest-bound
// capture is required, must re-hash exactly, must be for the pinned
// app-server version, must decode STRICTLY (one JSON value, no unknown
// keys, no duplicate keys, no duplicate tools), and its server set,
// "<server>/<tool>" set, and plugin-tool set must EQUAL the frozen
// lists. A profile enabling nothing may omit the capture; when present
// it is verified the same way (a capture exposing servers the profile
// hides is refused).
func verifyToolInventoryEvidence(evidenceRoot string, c *storage.CodexHarnessSpec) error {
	path := strings.TrimSpace(c.ToolInventoryPath)
	digest := strings.TrimSpace(c.ToolInventoryDigest)
	enabled := len(c.ExpectedMCPServers) > 0 || len(c.ExpectedPluginTools) > 0 || len(c.ExpectedMCPTools) > 0
	if path == "" && digest == "" {
		if enabled {
			return errors.New("the profile enables MCP servers or plugin tools but carries no tool_inventory_path/tool_inventory_digest: production eligibility requires the digest-bound native tool inventory that proves expected_mcp_tools and expected_plugin_tools complete")
		}
		return nil
	}
	if path == "" || digest == "" {
		return errors.New("tool_inventory_path and tool_inventory_digest must both be set")
	}
	if err := storage.ValidateSHA256Digest(digest); err != nil {
		return fmt.Errorf("tool_inventory_digest: %w", err)
	}
	raw, err := evidence.ReadFile(evidenceRoot, path, digest, "tool inventory", "tool_inventory_path")
	if err != nil {
		return err
	}
	inv, err := decodeNativeToolInventory(raw)
	if err != nil {
		return fmt.Errorf("tool inventory evidence is not the Council capture shape: %w", err)
	}
	if strings.TrimSpace(inv.CodexCLIVersion) != strings.TrimSpace(c.AppServerVersion) {
		return fmt.Errorf("tool inventory evidence is for codex %q but the profile pins %q", inv.CodexCLIVersion, c.AppServerVersion)
	}
	servers := make([]string, 0, len(inv.MCPServers))
	tools := make([]string, 0)
	for server, list := range inv.MCPServers {
		servers = append(servers, server)
		for _, tool := range list {
			tools = append(tools, server+"/"+tool)
		}
	}
	for _, check := range []struct {
		label            string
		frozen, observed []string
	}{
		{"expected_mcp_servers", c.ExpectedMCPServers, servers},
		{"expected_mcp_tools", c.ExpectedMCPTools, tools},
		{"expected_plugin_tools", c.ExpectedPluginTools, inv.PluginTools},
	} {
		frozen, observed := sortedUniqueTrimmed(check.frozen), sortedUniqueTrimmed(check.observed)
		if strings.Join(frozen, "\x00") != strings.Join(observed, "\x00") {
			return fmt.Errorf("%s %v does not equal the native tool inventory evidence %v (frozen inventories must be proven complete)",
				check.label, frozen, observed)
		}
	}
	return nil
}

// decodeNativeToolInventory decodes the Council capture STRICTLY at the
// token level, so the evidence file has exactly one canonical meaning:
// one top-level object followed by EOF; only the three known keys, each
// at most once and all present; server names, tool names, and plugin
// names non-empty after trimming; no duplicate server key, no duplicate
// tool within a server, no duplicate plugin tool. encoding/json's map
// decoding would silently collapse duplicate keys and accept trailing
// values, so it is not used for the shape.
func decodeNativeToolInventory(raw []byte) (NativeToolInventory, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	expectDelim := func(want json.Delim) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); !ok || d != want {
			return fmt.Errorf("expected %q, got %v", want, tok)
		}
		return nil
	}
	stringToken := func(what string) (string, error) {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		s, ok := tok.(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string, got %v", what, tok)
		}
		if strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("%s must not be empty", what)
		}
		return strings.TrimSpace(s), nil
	}
	stringList := func(what string) ([]string, error) {
		if err := expectDelim('['); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		seen := make(map[string]struct{})
		var out []string
		for dec.More() {
			s, err := stringToken(what + " entry")
			if err != nil {
				return nil, err
			}
			if _, dup := seen[s]; dup {
				return nil, fmt.Errorf("%s entry %q is duplicated", what, s)
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
		if err := expectDelim(']'); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		if out == nil {
			out = []string{}
		}
		return out, nil
	}

	var inv NativeToolInventory
	if err := expectDelim('{'); err != nil {
		return inv, err
	}
	seenKeys := make(map[string]struct{}, 3)
	for dec.More() {
		key, err := stringToken("object key")
		if err != nil {
			return inv, err
		}
		if _, dup := seenKeys[key]; dup {
			return inv, fmt.Errorf("key %q is duplicated", key)
		}
		seenKeys[key] = struct{}{}
		switch key {
		case "codex_cli_version":
			v, err := stringToken("codex_cli_version")
			if err != nil {
				return inv, err
			}
			inv.CodexCLIVersion = v
		case "mcp_servers":
			if err := expectDelim('{'); err != nil {
				return inv, fmt.Errorf("mcp_servers: %w", err)
			}
			inv.MCPServers = make(map[string][]string)
			for dec.More() {
				server, err := stringToken("mcp_servers server name")
				if err != nil {
					return inv, err
				}
				if _, dup := inv.MCPServers[server]; dup {
					return inv, fmt.Errorf("mcp_servers server %q is duplicated", server)
				}
				list, err := stringList("mcp_servers." + server + " tool")
				if err != nil {
					return inv, err
				}
				inv.MCPServers[server] = list
			}
			if err := expectDelim('}'); err != nil {
				return inv, fmt.Errorf("mcp_servers: %w", err)
			}
		case "plugin_tools":
			list, err := stringList("plugin_tools")
			if err != nil {
				return inv, err
			}
			inv.PluginTools = list
		default:
			return inv, fmt.Errorf("unknown key %q", key)
		}
	}
	if err := expectDelim('}'); err != nil {
		return inv, err
	}
	for _, required := range []string{"codex_cli_version", "mcp_servers", "plugin_tools"} {
		if _, ok := seenKeys[required]; !ok {
			return inv, fmt.Errorf("missing key %q", required)
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		return inv, errors.New("trailing content after the capture object")
	}
	return inv, nil
}
