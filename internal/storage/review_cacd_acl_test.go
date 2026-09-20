package storage

import (
	"testing"
)

func TestReviewCACD_WindowsACLSummaryParsingGuard(t *testing.T) {
	// Scenario 1: Forbidden first entry under summary-prefixed target paths (6 cases)
	// If a target path begins with "Failed processing" or "Successfully processed",
	// the first entry line must NOT be skipped as a summary line.
	t.Run("ForbiddenFirstEntryUnderSummaryPrefixedTarget", func(t *testing.T) {
		targetPaths := []string{
			"Failed processing cache",
			"Successfully processed cache",
		}
		forbiddenPrincipals := []struct {
			name      string
			principal string
		}{
			{"disallowed Everyone", "*S-1-1-0"},
			{"disallowed Users", "*S-1-5-32-545"},
			{"unauthorized user", "OtherUser"},
		}

		for _, target := range targetPaths {
			for _, fp := range forbiddenPrincipals {
				testName := target + "_" + fp.name
				t.Run(testName, func(t *testing.T) {
					output := target + " " + fp.principal + ":(OI)(CI)(F)\n" +
						"    *S-1-3-4:(OI)(CI)(F)\n" +
						"Successfully processed 1 files; Failed processing 0 files\n"

					err := parseAndVerifyIcaclsOutput(target, output)
					if err == nil {
						t.Fatalf("SECURITY VIOLATION: forbidden first entry with principal %q under target %q was bypassed and accepted", fp.principal, target)
					}
				})
			}
		}
	})

	// Scenario 2: Unauthorized continuation entry with a summary-like name (2 cases)
	// If a continuation entry's principal begins with "Successfully processed" or "Failed processing",
	// it must NOT be skipped as a summary line.
	t.Run("UnauthorizedContinuationEntryWithSummaryLikeName", func(t *testing.T) {
		summaryLikePrincipals := []string{
			"Successfully processed account",
			"Failed processing account",
		}
		target := `C:\State\cache`

		for _, p := range summaryLikePrincipals {
			t.Run(p, func(t *testing.T) {
				output := target + " *S-1-3-4:(OI)(CI)(F)\n" +
					"    " + p + ":(OI)(CI)(F)\n" +
					"Successfully processed 1 files; Failed processing 0 files\n"

				err := parseAndVerifyIcaclsOutput(target, output)
				if err == nil {
					t.Fatalf("SECURITY VIOLATION: unauthorized continuation principal %q was skipped as summary and accepted", p)
				}
			})
		}
	})

	// Scenario 3: Unrecognized summary-like line after a valid entry (2 cases)
	// Only the exact complete successful summary "Successfully processed 1 files; Failed processing 0 files"
	// is ignorable. Failure summaries or unexpected summaries must NOT be skipped and must fail validation.
	t.Run("UnrecognizedSummaryLikeLineAfterValidEntry", func(t *testing.T) {
		unrecognizedSummaries := []struct {
			name    string
			summary string
		}{
			{"failure summary", "Successfully processed 0 files; Failed processing 1 files"},
			{"multi-file summary", "Successfully processed 2 files; Failed processing 0 files"},
		}
		target := `C:\State\cache`

		for _, tc := range unrecognizedSummaries {
			t.Run(tc.name, func(t *testing.T) {
				output := target + " *S-1-3-4:(OI)(CI)(F)\n" +
					tc.summary + "\n"

				err := parseAndVerifyIcaclsOutput(target, output)
				if err == nil {
					t.Fatalf("SECURITY VIOLATION: unrecognized summary line %q was ignored and accepted", tc.summary)
				}
			})
		}
	})

	// Scenario 4: Authorized and disallowed controls (6 cases)
	t.Run("Controls", func(t *testing.T) {
		controls := []struct {
			name        string
			target      string
			output      string
			expectError bool
		}{
			{
				name:   "authorized entry with Failed processing target",
				target: "Failed processing cache",
				output: "Failed processing cache *S-1-3-4:(OI)(CI)(F)\n" +
					"Successfully processed 1 files; Failed processing 0 files\n",
				expectError: false,
			},
			{
				name:   "authorized entry with Successfully processed target",
				target: "Successfully processed cache",
				output: "Successfully processed cache *S-1-3-4:(OI)(CI)(F)\n" +
					"Successfully processed 1 files; Failed processing 0 files\n",
				expectError: false,
			},
			{
				name:   "authorized entry with standard path",
				target: `C:\State\cache`,
				output: `C:\State\cache *S-1-3-4:(OI)(CI)(F)` + "\n" +
					"Successfully processed 1 files; Failed processing 0 files\n",
				expectError: false,
			},
			{
				name:   "forbidden single entry with Failed processing target",
				target: "Failed processing cache",
				output: "Failed processing cache *S-1-1-0:(OI)(CI)(F)\n" +
					"Successfully processed 1 files; Failed processing 0 files\n",
				expectError: true,
			},
			{
				name:   "forbidden single entry with Successfully processed target",
				target: "Successfully processed cache",
				output: "Successfully processed cache *S-1-1-0:(OI)(CI)(F)\n" +
					"Successfully processed 1 files; Failed processing 0 files\n",
				expectError: true,
			},
			{
				name:   "forbidden single entry with standard path",
				target: `C:\State\cache`,
				output: `C:\State\cache *S-1-1-0:(OI)(CI)(F)` + "\n" +
					"Successfully processed 1 files; Failed processing 0 files\n",
				expectError: true,
			},
		}

		for _, tc := range controls {
			t.Run(tc.name, func(t *testing.T) {
				err := parseAndVerifyIcaclsOutput(tc.target, tc.output)
				if tc.expectError && err == nil {
					t.Fatalf("expected error for control %q, but got nil", tc.name)
				}
				if !tc.expectError && err != nil {
					t.Fatalf("expected success for control %q, but got: %v", tc.name, err)
				}
			})
		}
	})
}
