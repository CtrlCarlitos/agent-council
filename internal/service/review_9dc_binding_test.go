package service

import (
	"errors"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Regression evidence for PR #29 review round 3 (head 9dc6c5d):
// recovery must reconstruct the saved adapter configuration faithfully.
// Storage accepts object-form tooling configuration (workspace_root, model,
// tools/tooling); the resume path previously decoded the whole value as
// []string and silently returned nil, and substituted the workspace *mode*
// for the workspace *root*.

func TestReview9DC_ObjectToolingConfigRoundTrips(t *testing.T) {
	binding, err := sessionBindingFromStorage(storage.NativeBinding{
		LogicalSessionID: "sess-b",
		NativeSessionID:  "native-sess-b",
		Harness:          "claude",
		Model:            "model-x",
		WorkspaceMode:    "branch",
		ToolingConfig:    `{"workspace_root":"/work/repo","model":"model-x","tools":["git","go"]}`,
	}, "claude")
	if err != nil {
		t.Fatalf("object tooling config must convert: %v", err)
	}
	if string(binding.SessionID) != "sess-b" || binding.NativeSessionID != "native-sess-b" {
		t.Fatalf("native identity not preserved: %+v", binding)
	}
	if string(binding.Contributor) != "claude" {
		t.Fatalf("contributor not preserved: %+v", binding)
	}
	if binding.Config.WorkspaceRoot != "/work/repo" {
		t.Fatalf("workspace root not reconstructed from stored configuration: %q", binding.Config.WorkspaceRoot)
	}
	if binding.Config.Model != "model-x" {
		t.Fatalf("model not reconstructed: %q", binding.Config.Model)
	}
	if len(binding.Config.Tooling) != 2 || binding.Config.Tooling[0] != "git" || binding.Config.Tooling[1] != "go" {
		t.Fatalf("tooling not reconstructed: %+v", binding.Config.Tooling)
	}
}

func TestReview9DC_ArrayToolingConfigRoundTrips(t *testing.T) {
	binding, err := sessionBindingFromStorage(storage.NativeBinding{
		LogicalSessionID: "sess-a",
		NativeSessionID:  "native-sess-a",
		ToolingConfig:    `["git","go"]`,
	}, "claude")
	if err != nil {
		t.Fatalf("array tooling config must convert: %v", err)
	}
	if len(binding.Config.Tooling) != 2 || binding.Config.Tooling[0] != "git" || binding.Config.Tooling[1] != "go" {
		t.Fatalf("tooling not reconstructed: %+v", binding.Config.Tooling)
	}
	if binding.Config.WorkspaceRoot != "" || binding.Config.Model != "" {
		t.Fatalf("unexpected reconstructed fields: %+v", binding.Config)
	}
}

func TestReview9DC_EmptyToolingConfigRoundTrips(t *testing.T) {
	for _, cfg := range []string{"", "{}"} {
		binding, err := sessionBindingFromStorage(storage.NativeBinding{
			LogicalSessionID: "sess-e",
			NativeSessionID:  "native-sess-e",
			ToolingConfig:    cfg,
		}, "claude")
		if err != nil {
			t.Fatalf("empty config %q must convert: %v", cfg, err)
		}
		if len(binding.Config.Tooling) != 0 || binding.Config.WorkspaceRoot != "" || binding.Config.Model != "" {
			t.Fatalf("empty config must produce empty session config: %+v", binding.Config)
		}
	}
}

func TestReview9DC_UnsupportedToolingConfigFailsVisibly(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
	}{
		{"profile identifier", "full"},
		{"tooling profile map", `{"tooling":{"git":"allow"}}`},
		{"mixed tool array", `{"tools":["git",3]}`},
		{"non-string workspace root", `{"workspace_root":7}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sessionBindingFromStorage(storage.NativeBinding{
				LogicalSessionID: "sess-u",
				NativeSessionID:  "native-sess-u",
				ToolingConfig:    tc.cfg,
			}, "claude")
			if err == nil {
				t.Fatalf("unsupported configuration %q must fail visibly", tc.cfg)
			}
			if !errors.Is(err, ErrUnsupportedBindingConfig) {
				t.Fatalf("expected ErrUnsupportedBindingConfig, got %v", err)
			}
		})
	}
}
