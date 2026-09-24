package service

import (
	"errors"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Regression evidence for PR #29 targeted re-review (head d93cf52):
// a saved object configuration containing both tools and tooling must be
// rejected. Selecting one alias would let an unsupported profile reference
// or a conflicting list disappear behind the other key instead of failing
// visibly.
func TestReviewD93_AmbiguousToolingAliasesRejected(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
	}{
		{"tools list hides profile reference", `{"tools":["git"],"tooling":"locked-profile"}`},
		{"tools list hides profile map", `{"tools":["git"],"tooling":{"profile":"locked-profile"}}`},
		{"tools list hides conflicting list", `{"tools":["git"],"tooling":["go"]}`},
		{"empty tools hides nonempty list", `{"tools":[],"tooling":["go"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sessionBindingFromStorage(storage.NativeBinding{
				LogicalSessionID: "sess-d93",
				NativeSessionID:  "native-sess-d93",
				ToolingConfig:    tc.cfg,
			}, "claude")
			if err == nil {
				t.Fatalf("ambiguous tooling configuration %q must fail visibly", tc.cfg)
			}
			if !errors.Is(err, ErrUnsupportedBindingConfig) {
				t.Fatalf("expected ErrUnsupportedBindingConfig, got %v", err)
			}
		})
	}
}

// Controls: exactly one representation reconstructs, and the equivalent
// unsupported values are still rejected when supplied alone.
func TestReviewD93_SingleAliasControls(t *testing.T) {
	okCases := []struct {
		name     string
		cfg      string
		wantTool []string
	}{
		{"tools only", `{"tools":["git","go"]}`, []string{"git", "go"}},
		{"tooling only", `{"tooling":["git","go"]}`, []string{"git", "go"}},
		{"object with workspace root and tools", `{"workspace_root":"/work","tools":["go"]}`, []string{"go"}},
	}
	for _, tc := range okCases {
		t.Run(tc.name, func(t *testing.T) {
			binding, err := sessionBindingFromStorage(storage.NativeBinding{
				LogicalSessionID: "sess-d93",
				NativeSessionID:  "native-sess-d93",
				ToolingConfig:    tc.cfg,
			}, "claude")
			if err != nil {
				t.Fatalf("single-alias configuration must convert: %v", err)
			}
			if len(binding.Config.Tooling) != len(tc.wantTool) {
				t.Fatalf("expected tooling %v, got %+v", tc.wantTool, binding.Config.Tooling)
			}
			for i, tool := range tc.wantTool {
				if binding.Config.Tooling[i] != tool {
					t.Fatalf("expected tooling %v, got %+v", tc.wantTool, binding.Config.Tooling)
				}
			}
		})
	}

	badCases := []struct {
		name string
		cfg  string
	}{
		{"profile reference alone", `{"tooling":"locked-profile"}`},
		{"profile map alone", `{"tooling":{"profile":"locked-profile"}}`},
		{"bare profile identifier", "locked-profile"},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sessionBindingFromStorage(storage.NativeBinding{
				LogicalSessionID: "sess-d93",
				NativeSessionID:  "native-sess-d93",
				ToolingConfig:    tc.cfg,
			}, "claude")
			if !errors.Is(err, ErrUnsupportedBindingConfig) {
				t.Fatalf("expected ErrUnsupportedBindingConfig for %q, got %v", tc.cfg, err)
			}
		})
	}
}
