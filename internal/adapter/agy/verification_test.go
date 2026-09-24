package agy

// Required-tool verification (AC-010 spec §3.5, issue #10 criterion):
// attribution is intersected with observation — for each denial entry,
// attributed = denial_map(entry) ∩ observed tool steps of THIS process;
// |1| ⇒ that tool is denied, |>1| ⇒ ambiguous (all attributed treated as
// not executed), |0| ⇒ unattributed, an unmapped pair ⇒ every observed
// tool is treated as not executed. Any missing required tool or any
// non-empty denial class flags verification_incomplete.

import (
	"reflect"
	"testing"
)

// v129Coverage is the committed 1.2.9 denial map shape (the tool
// capability half is irrelevant to verification).
func v129Coverage() CoverageMap {
	return CoverageMap{
		CLIVersion: "1.2.9",
		DenialMap: []DenialEntry{
			{Action: "command", DisplayName: "RunCommand", Tools: []string{"run_command", "send_command_input"}},
		},
	}
}

func eqStrings(t *testing.T, field string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %v, want %v", field, got, want)
	}
}

func TestVerification_AllRequiredExecutedIsComplete(t *testing.T) {
	v := ComputeVerification([]string{"view_file"}, []string{"view_file", "list_dir"}, nil, v129Coverage())
	eqStrings(t, "executed", v.Executed, []string{"list_dir", "view_file"})
	eqStrings(t, "missing", v.MissingRequired, nil)
	eqStrings(t, "denied", v.Denied, nil)
	if v.Incomplete {
		t.Fatalf("every required tool executed, no denials: must be complete, got %+v", v)
	}
}

func TestVerification_RequiredToolSkippedIsIncomplete(t *testing.T) {
	v := ComputeVerification([]string{"run_command", "view_file"}, []string{"view_file"}, nil, v129Coverage())
	eqStrings(t, "missing", v.MissingRequired, []string{"run_command"})
	eqStrings(t, "executed", v.Executed, []string{"view_file"})
	if !v.Incomplete {
		t.Fatalf("a silently skipped required tool must flag verification_incomplete, got %+v", v)
	}
}

func TestVerification_SingleAttributedDenial(t *testing.T) {
	v := ComputeVerification([]string{"run_command"}, []string{"run_command", "view_file"},
		[]DeniedAction{{Action: "command", DisplayName: "RunCommand"}}, v129Coverage())
	eqStrings(t, "denied", v.Denied, []string{"run_command"})
	eqStrings(t, "executed", v.Executed, []string{"view_file"})
	eqStrings(t, "missing", v.MissingRequired, []string{"run_command"})
	eqStrings(t, "ambiguous", v.Ambiguous, nil)
	if !v.Incomplete {
		t.Fatalf("a denied tool must flag verification_incomplete, got %+v", v)
	}
}

func TestVerification_AmbiguousDenialTreatsAllAttributedAsNotExecuted(t *testing.T) {
	v := ComputeVerification(nil, []string{"run_command", "send_command_input", "view_file"},
		[]DeniedAction{{Action: "command", DisplayName: "RunCommand"}}, v129Coverage())
	eqStrings(t, "ambiguous", v.Ambiguous, []string{"command/RunCommand"})
	eqStrings(t, "denied", v.Denied, []string{"run_command", "send_command_input"})
	eqStrings(t, "executed", v.Executed, []string{"view_file"})
	if !v.Incomplete {
		t.Fatalf("an ambiguous denial must flag verification_incomplete, got %+v", v)
	}
}

func TestVerification_UnattributedDenialNeverBlamesAnUnattemptedTool(t *testing.T) {
	v := ComputeVerification(nil, []string{"view_file"},
		[]DeniedAction{{Action: "command", DisplayName: "RunCommand"}}, v129Coverage())
	eqStrings(t, "unattributed", v.Unattributed, []string{"command/RunCommand"})
	eqStrings(t, "denied", v.Denied, nil)
	eqStrings(t, "executed", v.Executed, []string{"view_file"})
	if !v.Incomplete {
		t.Fatalf("an unattributed denial must flag verification_incomplete, got %+v", v)
	}
}

func TestVerification_UnmappedDenialTreatsEveryObservedToolAsNotExecuted(t *testing.T) {
	v := ComputeVerification([]string{"view_file"}, []string{"view_file", "list_dir"},
		[]DeniedAction{{Action: "browse", DisplayName: "OpenBrowser"}}, v129Coverage())
	eqStrings(t, "unmapped", v.Unmapped, []string{"browse/OpenBrowser"})
	eqStrings(t, "executed", v.Executed, nil)
	eqStrings(t, "missing", v.MissingRequired, []string{"view_file"})
	if !v.Incomplete {
		t.Fatalf("an unmapped denial must flag verification_incomplete, got %+v", v)
	}
}

func TestVerification_DuplicateObservationsCountOnce(t *testing.T) {
	v := ComputeVerification(nil, []string{"view_file", "view_file"}, nil, v129Coverage())
	eqStrings(t, "executed", v.Executed, []string{"view_file"})
}

func TestVerification_DenialClassesForEvents(t *testing.T) {
	observed := []string{"run_command"}
	classes := classifyDenials(observed, []DeniedAction{
		{Action: "command", DisplayName: "RunCommand"},
		{Action: "browse", DisplayName: "OpenBrowser"},
	}, v129Coverage())
	if len(classes) != 2 {
		t.Fatalf("one class per denial entry, got %+v", classes)
	}
	if classes[0].Kind != denialAttributed || !reflect.DeepEqual(classes[0].Tools, []string{"run_command"}) {
		t.Fatalf("first entry must be attributed to run_command, got %+v", classes[0])
	}
	if classes[1].Kind != denialUnmapped || classes[1].Pair != "browse/OpenBrowser" {
		t.Fatalf("second entry must be unmapped with its raw pair, got %+v", classes[1])
	}
}
