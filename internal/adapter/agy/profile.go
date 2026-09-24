// Package agy is provider-free validation of the frozen Agy harness
// block: the Agy adapter requires cprof-v4 with a complete
// harnesses.agy block, re-hashes the committed init/tool-coverage/
// plugins evidence against their frozen digests, and derives the exact
// cprot-v2 record set the isolation attestation must cover (AC-010
// spec §3.2/§3.7). Everything fails closed before any process starts.
package agy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/evidence"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ErrUnsupportedProfile reports a profile that is not eligible for the
// Agy adapter (wrong algo_version, or a v4 profile without a complete
// agy harness block, or evidence that fails any freeze/construction
// rule).
type ErrUnsupportedProfile struct {
	AlgoVersion string
	Reason      string
}

func (e *ErrUnsupportedProfile) Error() string {
	return fmt.Sprintf("profile algorithm %q is unsupported for this adapter: %s", e.AlgoVersion, e.Reason)
}

// AgyLaunchPolicy is the verified frozen Agy policy for a run, ready for
// the launch seam.
type AgyLaunchPolicy struct {
	CLIVersion string
	// Model is the frozen harnesses.agy model (the --model pin; init.model
	// must echo it).
	Model                 string
	BinaryPath            string
	BinaryDigest          string
	ExpectedHome          string
	PlatformOS            string
	PlatformFamily        string
	PermissionMode        string
	ExecutionMode         string
	Sandbox               bool
	PrintTimeoutBackstop  time.Duration
	ExpectedTools         []string
	ExpectedSkills        []string
	DefaultRequiredTools  []string
	RequiredHooks         []string
	HooksConfigDigest     string
	PluginsEvidenceDigest string
	Coverage              CoverageMap
}

var agySemverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// ValidateAgyHarness validates the frozen agy harness block of a run
// profile against the committed evidence rooted at evidenceRoot: the
// profile must be cprof-v4 with a complete block, the shape/enum rules
// of spec §3.7 hold, and init_evidence_path/tool_coverage_path/
// plugins_evidence_path are re-hashed, strictly decoded, and checked
// against the frozen block. Any incompleteness fails closed with
// ErrUnsupportedProfile.
func ValidateAgyHarness(profile storage.CanonicalProfile, evidenceRoot string) (AgyLaunchPolicy, error) {
	if profile.AlgoVersion != "cprof-v4" {
		return AgyLaunchPolicy{}, &ErrUnsupportedProfile{
			AlgoVersion: profile.AlgoVersion,
			Reason:      "the Agy adapter requires cprof-v4 with a complete agy harness block",
		}
	}
	spec, ok := profile.Harnesses["agy"]
	if !ok || spec.Agy == nil {
		return AgyLaunchPolicy{}, &ErrUnsupportedProfile{
			AlgoVersion: profile.AlgoVersion,
			Reason:      "cprof-v4 profile lacks the required harnesses.agy block",
		}
	}
	a := spec.Agy

	unsupported := func(reason string) (AgyLaunchPolicy, error) {
		return AgyLaunchPolicy{}, &ErrUnsupportedProfile{AlgoVersion: profile.AlgoVersion, Reason: reason}
	}

	if profile.ToolkitManifest == nil {
		return unsupported("profile requires toolkit_manifest")
	}
	model := strings.TrimSpace(spec.Model)
	if model == "" {
		return unsupported("agy harness requires a frozen model")
	}

	// Shape/enum rules (spec §3.7), mirrored from the storage freeze
	// validator so this adapter fails closed even for an in-memory
	// profile that was never run through storage.ComputeProfileDigest.
	if !agySemverPattern.MatchString(strings.TrimSpace(a.CLIVersion)) {
		return unsupported(fmt.Sprintf("cli_version %q must be MAJOR.MINOR.PATCH", a.CLIVersion))
	}
	binaryPath := storage.NormalizePathScalar(a.BinaryPath)
	if binaryPath == "" || !strings.HasPrefix(binaryPath, "/") {
		return unsupported(fmt.Sprintf("binary_path %q must be absolute", a.BinaryPath))
	}
	if err := storage.ValidateSHA256Digest(a.BinaryDigest); err != nil {
		return unsupported("binary_digest: " + err.Error())
	}
	expectedHome := storage.NormalizePathScalar(a.ExpectedHome)
	if expectedHome == "" || !strings.HasPrefix(expectedHome, "/") {
		return unsupported(fmt.Sprintf("expected_home %q must be absolute", a.ExpectedHome))
	}
	if strings.TrimSpace(a.Platform.OS) == "" {
		return unsupported("agy block lacks the platform os")
	}
	if strings.TrimSpace(a.Platform.Family) == "" {
		return unsupported("agy block lacks the platform family")
	}
	switch a.PermissionMode {
	case "request-review", "strict":
	default:
		return unsupported(fmt.Sprintf("unsupported permission_mode %q (expected request-review or strict)", a.PermissionMode))
	}
	switch a.ExecutionMode {
	case "default", "accept-edits", "plan":
	default:
		return unsupported(fmt.Sprintf("unsupported execution_mode %q (expected default, accept-edits, or plan)", a.ExecutionMode))
	}
	if a.PrintTimeoutBackstopSeconds < 60 {
		return unsupported(fmt.Sprintf("print_timeout_backstop_seconds %d must be >= 60", a.PrintTimeoutBackstopSeconds))
	}
	if len(a.ExpectedTools) == 0 {
		return unsupported("agy block requires a non-empty expected_tools inventory")
	}
	if err := requireUnique("expected_tools", a.ExpectedTools); err != nil {
		return unsupported(err.Error())
	}
	if len(a.ExpectedMCPServers) > 0 || len(a.ExpectedMCPTools) > 0 || len(a.ExpectedPluginTools) > 0 {
		return unsupported("expected_mcp_servers, expected_mcp_tools, and expected_plugin_tools must be empty: no committed evidence path proves a non-empty native tool inventory, so this profile is not launchable")
	}
	if err := requireUnique("default_required_tools", a.DefaultRequiredTools); err != nil {
		return unsupported(err.Error())
	}
	toolSet := make(map[string]struct{}, len(a.ExpectedTools))
	for _, t := range a.ExpectedTools {
		toolSet[t] = struct{}{}
	}
	for _, t := range a.DefaultRequiredTools {
		if _, ok := toolSet[t]; !ok {
			return unsupported(fmt.Sprintf("default_required_tools entry %q is not in expected_tools", t))
		}
	}
	if err := requireUnique("expected_skills", a.ExpectedSkills); err != nil {
		return unsupported(err.Error())
	}
	for _, s := range a.ExpectedSkills {
		if s == "" || strings.ContainsAny(s, "/\\") {
			return unsupported(fmt.Sprintf("expected_skills entry %q must be a bare directory name", s))
		}
	}
	if err := storage.ValidateSHA256Digest(a.HooksConfigDigest); err != nil {
		return unsupported("hooks_config_digest: " + err.Error())
	}
	if len(a.RequiredHooks) == 0 {
		return unsupported("agy block requires a non-empty required_hooks list")
	}
	if err := requireUnique("required_hooks", a.RequiredHooks); err != nil {
		return unsupported(err.Error())
	}
	for i, ptr := range a.RequiredHooks {
		if ptr != "" && !strings.HasPrefix(ptr, "/") {
			return unsupported(fmt.Sprintf("required_hooks entry %q is not an RFC 6901 JSON pointer", ptr))
		}
		if i > 0 && a.RequiredHooks[i-1] > ptr {
			return unsupported("required_hooks must be sorted")
		}
	}
	if len(a.HooksEvidence.Verified) == 0 && len(a.HooksEvidence.Unverifiable) == 0 {
		return unsupported("hooks_evidence requires at least one of verified/unverifiable to be present")
	}

	// Evidence-root containment + strict decode + digest-bound rehash
	// (spec §3.7): init_evidence_path, tool_coverage_path,
	// plugins_evidence_path.
	if err := storage.ValidateSHA256Digest(a.InitEvidenceDigest); err != nil {
		return unsupported("init_evidence_digest: " + err.Error())
	}
	initRaw, err := evidence.ReadFile(evidenceRoot, a.InitEvidencePath, a.InitEvidenceDigest, "init evidence", "init_evidence_path")
	if err != nil {
		return AgyLaunchPolicy{}, err
	}
	if err := validateInitCapture(initRaw, a); err != nil {
		return unsupported(err.Error())
	}

	if err := storage.ValidateSHA256Digest(a.ToolCoverageDigest); err != nil {
		return unsupported("tool_coverage_digest: " + err.Error())
	}
	covRaw, err := evidence.ReadFile(evidenceRoot, a.ToolCoveragePath, a.ToolCoverageDigest, "tool coverage evidence", "tool_coverage_path")
	if err != nil {
		return AgyLaunchPolicy{}, err
	}
	coverageMap, err := decodeCoverageEvidence(covRaw)
	if err != nil {
		return unsupported("tool coverage evidence is not the pinned shape: " + err.Error())
	}
	if coverageMap.CLIVersion != strings.TrimSpace(a.CLIVersion) {
		return unsupported(fmt.Sprintf("tool coverage evidence is for agy %q but the profile pins %q", coverageMap.CLIVersion, a.CLIVersion))
	}
	for _, tool := range a.ExpectedTools {
		caps, ok := coverageMap.Tools[tool]
		if !ok || len(caps) == 0 {
			return unsupported(fmt.Sprintf("expected_tools entry %q is not classified in the tool coverage evidence", tool))
		}
		for _, c := range caps {
			if c == CapabilityUncovered {
				return unsupported(fmt.Sprintf("expected_tools entry %q is classified uncovered by the tool coverage evidence: production eligibility requires the operator to deny it natively first", tool))
			}
		}
	}

	if err := storage.ValidateSHA256Digest(a.PluginsEvidenceDigest); err != nil {
		return unsupported("plugins_evidence_digest: " + err.Error())
	}
	pluginsRaw, err := evidence.ReadFile(evidenceRoot, a.PluginsEvidencePath, a.PluginsEvidenceDigest, "plugins evidence", "plugins_evidence_path")
	if err != nil {
		return AgyLaunchPolicy{}, err
	}
	if err := validatePluginsCanonicalBytes(pluginsRaw); err != nil {
		return unsupported("plugins evidence is not canonical: " + err.Error())
	}

	return AgyLaunchPolicy{
		CLIVersion:            a.CLIVersion,
		Model:                 model,
		BinaryPath:            binaryPath,
		BinaryDigest:          strings.ToLower(strings.TrimSpace(a.BinaryDigest)),
		ExpectedHome:          expectedHome,
		PlatformOS:            a.Platform.OS,
		PlatformFamily:        a.Platform.Family,
		PermissionMode:        a.PermissionMode,
		ExecutionMode:         a.ExecutionMode,
		Sandbox:               a.Sandbox,
		PrintTimeoutBackstop:  time.Duration(a.PrintTimeoutBackstopSeconds) * time.Second,
		ExpectedTools:         append([]string(nil), a.ExpectedTools...),
		ExpectedSkills:        append([]string(nil), a.ExpectedSkills...),
		DefaultRequiredTools:  append([]string(nil), a.DefaultRequiredTools...),
		RequiredHooks:         append([]string(nil), a.RequiredHooks...),
		HooksConfigDigest:     strings.ToLower(strings.TrimSpace(a.HooksConfigDigest)),
		PluginsEvidenceDigest: strings.ToLower(strings.TrimSpace(a.PluginsEvidenceDigest)),
		Coverage:              coverageMap,
	}, nil
}

func requireUnique(field string, items []string) error {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, dup := seen[item]; dup {
			return fmt.Errorf("%s contains duplicate entry %q", field, item)
		}
		seen[item] = struct{}{}
	}
	return nil
}

// initCapture is the strict shape of the committed init evidence
// capture (spec §3.7): a single {"event":"init",…} object whose
// init.tools equals expected_tools (as a set) and whose permission_mode
// equals the frozen mode. conversation_id and init.cwd are redacted
// placeholders in the committed research capture; their values are not
// otherwise compared.
type initCapture struct {
	ConversationID string `json:"conversation_id"`
	Event          string `json:"event"`
	Init           struct {
		CWD            string   `json:"cwd"`
		PermissionMode string   `json:"permission_mode"`
		Tools          []string `json:"tools"`
	} `json:"init"`
}

func validateInitCapture(raw []byte, a *storage.AgyHarnessSpec) error {
	var cap initCapture
	if err := evidence.DecodeStrictObject(raw, &cap); err != nil {
		return fmt.Errorf("decode init evidence: %w", err)
	}
	if cap.Event != "init" {
		return fmt.Errorf("init evidence event %q must be %q", cap.Event, "init")
	}
	if cap.Init.PermissionMode != a.PermissionMode {
		return fmt.Errorf("init evidence permission_mode %q does not match the frozen %q", cap.Init.PermissionMode, a.PermissionMode)
	}
	got := sortedUniqueTrimmed(cap.Init.Tools)
	want := sortedUniqueTrimmed(a.ExpectedTools)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("init evidence init.tools %v does not equal the frozen expected_tools %v", got, want)
	}
	return nil
}

// pluginImport is one entry of the plugins evidence "imports" array
// (spec §3.2).
type pluginImport struct {
	Name       string   `json:"name"`
	Source     string   `json:"source"`
	ImportedAt string   `json:"importedAt"`
	Components []string `json:"components"`
}

type pluginsEvidence struct {
	Imports []pluginImport `json:"imports"`
}

var allowedPluginComponents = map[string]struct{}{
	"hooks": {}, "skills": {}, "mcp": {}, "commands": {}, "agents": {},
}

// validatePluginsCanonicalBytes strictly decodes the plugins evidence,
// validates its shape (importedAt RFC3339, components a non-empty
// subset of the pinned enum), re-encodes it canonically (imports sorted
// by name, components sorted/deduplicated, object keys sorted, no
// insignificant whitespace), and requires the canonical bytes to equal
// raw EXACTLY (spec §3.2 canonical-bytes rule).
func validatePluginsCanonicalBytes(raw []byte) error {
	var doc pluginsEvidence
	if err := evidence.DecodeStrictObject(raw, &doc); err != nil {
		return fmt.Errorf("decode plugins evidence: %w", err)
	}
	imports := append([]pluginImport(nil), doc.Imports...)
	sort.Slice(imports, func(i, j int) bool { return imports[i].Name < imports[j].Name })
	canonicalImports := make([]any, 0, len(imports))
	seenNames := make(map[string]struct{}, len(imports))
	for _, imp := range imports {
		if strings.TrimSpace(imp.Name) == "" {
			return fmt.Errorf("plugins evidence import has an empty name")
		}
		if _, dup := seenNames[imp.Name]; dup {
			return fmt.Errorf("plugins evidence import %q is duplicated", imp.Name)
		}
		seenNames[imp.Name] = struct{}{}
		if strings.TrimSpace(imp.Source) == "" {
			return fmt.Errorf("plugins evidence import %q has an empty source", imp.Name)
		}
		if _, err := time.Parse(time.RFC3339, imp.ImportedAt); err != nil {
			return fmt.Errorf("plugins evidence import %q importedAt must be RFC3339: %w", imp.Name, err)
		}
		if len(imp.Components) == 0 {
			return fmt.Errorf("plugins evidence import %q has no components", imp.Name)
		}
		comps := sortedUniqueTrimmed(imp.Components)
		for _, c := range comps {
			if _, ok := allowedPluginComponents[c]; !ok {
				return fmt.Errorf("plugins evidence import %q has unknown component %q", imp.Name, c)
			}
		}
		compsAny := make([]any, len(comps))
		for i, c := range comps {
			compsAny[i] = c
		}
		canonicalImports = append(canonicalImports, map[string]any{
			"components": compsAny,
			"importedAt": imp.ImportedAt,
			"name":       imp.Name,
			"source":     imp.Source,
		})
	}
	canonicalDoc := map[string]any{"imports": canonicalImports}
	canonBytes, err := evidence.Canonical(canonicalDoc)
	if err != nil {
		return fmt.Errorf("canonicalize plugins evidence: %w", err)
	}
	if string(canonBytes) != string(raw) {
		return fmt.Errorf("committed plugins evidence bytes are not the canonical encoding")
	}
	return nil
}

// coverageEvidenceDoc is the typed shape of the tool_coverage_path
// evidence file (spec §3.7): only the three known keys (cli_version,
// tools, denial_map). evidence.DecodeStrictObject enforces the shape
// rules generic to every evidence file (single value, no unknown or
// duplicate keys at any nesting level, no trailing content); the rules
// below are the ones specific to THIS shape that a generic decoder
// cannot express.
type coverageEvidenceDoc struct {
	CLIVersion string                  `json:"cli_version"`
	Tools      map[string][]Capability `json:"tools"`
	DenialMap  []DenialEntry           `json:"denial_map"`
}

// decodeCoverageEvidence decodes the tool_coverage_path evidence shape
// (spec §3.7) by composing evidence.DecodeStrictObject (single value, no
// unknown/duplicate keys, no trailing content — the same guarantees
// codex's evidence decoding relies on) with the ordering/enum rules that
// decoder does not know about this shape: "tools" keys must be
// encountered in strictly ascending order in the file (Go's map decode
// discards that order, so it is recovered separately by
// toolKeysInFileOrder — everything else about the shape is already
// covered by DecodeStrictObject by the time that runs); each tool's
// capability array must be non-empty, in the closed enum's canonical
// order, deduplicated, and uncovered exclusive of every other value;
// "denial_map" entries — whose order IS preserved by ordinary JSON-array
// decoding — must be ascending by (action, display_name) with no
// duplicate pair, and each entry's tools array must be sorted,
// deduplicated, non-empty, and (checked after decoding) a subset of the
// "tools" keys.
func decodeCoverageEvidence(raw []byte) (CoverageMap, error) {
	var doc coverageEvidenceDoc
	if err := evidence.DecodeStrictObject(raw, &doc); err != nil {
		return CoverageMap{}, fmt.Errorf("decode tool coverage evidence: %w", err)
	}
	if strings.TrimSpace(doc.CLIVersion) == "" {
		return CoverageMap{}, fmt.Errorf("cli_version must not be empty")
	}
	if doc.Tools == nil {
		return CoverageMap{}, fmt.Errorf("missing key %q", "tools")
	}
	if doc.DenialMap == nil {
		return CoverageMap{}, fmt.Errorf("missing key %q", "denial_map")
	}

	for tool, caps := range doc.Tools {
		if tool == "" {
			return CoverageMap{}, fmt.Errorf("tools carries an empty tool name")
		}
		if len(caps) == 0 {
			return CoverageMap{}, fmt.Errorf("tools[%q] carries no capabilities", tool)
		}
		lastRank := -1
		for _, c := range caps {
			rank := capabilityRank(c)
			if rank < 0 {
				return CoverageMap{}, fmt.Errorf("tools[%q] carries unknown capability %q", tool, c)
			}
			if rank <= lastRank {
				return CoverageMap{}, fmt.Errorf("tools[%q] capabilities must be sorted in enum order without duplicates", tool)
			}
			lastRank = rank
			if c == CapabilityUncovered && len(caps) != 1 {
				return CoverageMap{}, fmt.Errorf("tools[%q]: uncovered must be exclusive of every other capability", tool)
			}
		}
	}

	toolKeys, err := toolKeysInFileOrder(raw)
	if err != nil {
		return CoverageMap{}, fmt.Errorf("recover tools key order: %w", err)
	}
	for i := 1; i < len(toolKeys); i++ {
		if toolKeys[i] <= toolKeys[i-1] {
			return CoverageMap{}, fmt.Errorf("tools keys must be sorted and unique: %q does not follow %q", toolKeys[i], toolKeys[i-1])
		}
	}

	var lastAction, lastDisplay string
	for i, entry := range doc.DenialMap {
		if entry.Action == "" {
			return CoverageMap{}, fmt.Errorf("denial_map entry action is empty")
		}
		if entry.DisplayName == "" {
			return CoverageMap{}, fmt.Errorf("denial_map entry display_name is empty")
		}
		if len(entry.Tools) == 0 {
			return CoverageMap{}, fmt.Errorf("denial_map entry %q/%q has no tools", entry.Action, entry.DisplayName)
		}
		var lastT string
		for j, t := range entry.Tools {
			if t == "" {
				return CoverageMap{}, fmt.Errorf("denial_map tools entry is empty")
			}
			if j > 0 && t <= lastT {
				return CoverageMap{}, fmt.Errorf("denial_map tools must be sorted and deduplicated: %q does not follow %q", t, lastT)
			}
			lastT = t
		}
		if i > 0 {
			if entry.Action == lastAction && entry.DisplayName == lastDisplay {
				return CoverageMap{}, fmt.Errorf("denial_map has duplicate (action,display_name) pair (%q,%q)", entry.Action, entry.DisplayName)
			}
			if entry.Action < lastAction || (entry.Action == lastAction && entry.DisplayName < lastDisplay) {
				return CoverageMap{}, fmt.Errorf("denial_map must be sorted by (action,display_name)")
			}
		}
		lastAction, lastDisplay = entry.Action, entry.DisplayName
	}

	// Cross-validate after decoding (denial_map may precede tools in the
	// canonical byte order: cli_version < denial_map < tools).
	for _, entry := range doc.DenialMap {
		for _, t := range entry.Tools {
			if _, ok := doc.Tools[t]; !ok {
				return CoverageMap{}, fmt.Errorf("denial_map entry (%q,%q) names tool %q which is not in tools", entry.Action, entry.DisplayName, t)
			}
		}
	}

	return CoverageMap{CLIVersion: doc.CLIVersion, Tools: doc.Tools, DenialMap: doc.DenialMap}, nil
}

// toolKeysInFileOrder recovers the "tools" object's key encounter order
// exactly as written in the file — the one property of this shape that
// encoding/json's ordinary map decode discards and that
// evidence.DecodeStrictObject has no reason to preserve (it enforces
// strictness, not field order). It assumes raw already passed
// evidence.DecodeStrictObject (single well-formed value, no duplicate
// keys anywhere), so it does no strictness checking of its own — it is
// a pure order-extraction pass over the same bytes, not a second
// validator.
func toolKeysInFileOrder(raw []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil { // top-level '{'
		return nil, err
	}
	var keys []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyTok.(string)
		if key != "tools" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := dec.Token(); err != nil { // "tools" object's '{'
			return nil, err
		}
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kTok.(string)
			keys = append(keys, k)
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
		}
		if _, err := dec.Token(); err != nil { // "tools" object's '}'
			return nil, err
		}
	}
	return keys, nil
}
