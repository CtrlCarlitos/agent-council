package storage

import (
	"os/exec"
	"os/user"
	"runtime"
	"testing"
)

// TestReviewE232_WindowsACLSecurityChecks tests:
// 1. Environment variable spoofing resistance (identity strictly from OS token context via user.Current()).
// 2. Strict path stripping limited exclusively to line 0 with trailing whitespace (continuation lines are never stripped).
// 3. Permission rights parsing (rejection of empty, malformed, nested, and invalid tokens).
// 4. Verification controls (authorized identities pass; broad groups and malformed outputs fail).
func TestReviewE232_WindowsACLSecurityChecks(t *testing.T) {
	targetDir := `C:\State\cache`

	// 1. Environment Variable Spoofing: Setting USERNAME or USERDOMAIN must NOT grant authorization
	t.Run("EnvironmentVariableSpoofing", func(t *testing.T) {
		spoofedUser := "ReviewExternalIdentity"
		spoofedDomain := "REVIEWLAB"
		t.Setenv("USERNAME", spoofedUser)
		t.Setenv("USERDOMAIN", spoofedDomain)

		// Direct check: isAuthorizedWindowsPrincipal must return false for spoofed env identities
		if isAuthorizedWindowsPrincipal(spoofedUser) {
			t.Fatalf("SECURITY VIOLATION: spoofed USERNAME %q was accepted by isAuthorizedWindowsPrincipal", spoofedUser)
		}
		if isAuthorizedWindowsPrincipal(spoofedDomain + "\\" + spoofedUser) {
			t.Fatalf("SECURITY VIOLATION: spoofed USERDOMAIN\\USERNAME %q was accepted by isAuthorizedWindowsPrincipal", spoofedDomain+"\\"+spoofedUser)
		}

		// Full output check: icacls output granting access to the spoofed identity must be rejected
		spoofedOutputs := []struct {
			name   string
			output string
		}{
			{
				name: "spoofed bare username",
				output: targetDir + " *S-1-3-4:(OI)(CI)(F)\n" +
					"                      " + spoofedUser + ":(OI)(CI)(F)\n" +
					"Successfully processed 1 files; Failed processing 0 files\n",
			},
			{
				name: "spoofed domain username",
				output: targetDir + " *S-1-3-4:(OI)(CI)(F)\n" +
					"                      " + spoofedDomain + "\\" + spoofedUser + ":(OI)(CI)(F)\n" +
					"Successfully processed 1 files; Failed processing 0 files\n",
			},
		}

		for _, tc := range spoofedOutputs {
			t.Run(tc.name, func(t *testing.T) {
				err := parseAndVerifyIcaclsOutput(targetDir, tc.output)
				if err == nil {
					t.Fatalf("SECURITY VIOLATION: spoofed environment identity %q was accepted by parseAndVerifyIcaclsOutput", tc.name)
				}
			})
		}
	})

	// 2. Path Stripping on Continuation Lines:
	// Basename prefix stripping must NOT occur on continuation lines.
	// Principals like cacheSYSTEM, cacheAdministrators, cacheOwner Rights, cacheBUILTIN\Administrators
	// must remain unstripped and be rejected as unauthorized.
	t.Run("ContinuationLinePathStrippingResistance", func(t *testing.T) {
		corruptedPrincipals := []struct {
			name      string
			principal string
		}{
			{
				name:      "cacheSYSTEM",
				principal: "cacheSYSTEM",
			},
			{
				name:      "cacheAdministrators",
				principal: "cacheAdministrators",
			},
			{
				name:      "cacheOwner Rights",
				principal: "cacheOwner Rights",
			},
			{
				name:      "cacheBUILTIN\\Administrators",
				principal: "cacheBUILTIN\\Administrators",
			},
			{
				name:      "cacheNT AUTHORITY\\SYSTEM",
				principal: "cacheNT AUTHORITY\\SYSTEM",
			},
			{
				name:      "cacheS-1-3-4",
				principal: "cacheS-1-3-4",
			},
		}

		for _, tc := range corruptedPrincipals {
			t.Run(tc.name, func(t *testing.T) {
				// Line 0 is a valid authorized principal with targetDir prefix.
				// Line 1 contains the candidate principal which starts with "cache" (the basename of targetDir).
				output := targetDir + " *S-1-3-4:(OI)(CI)(F)\n" +
					"                      " + tc.principal + ":(OI)(CI)(F)\n" +
					"Successfully processed 1 files; Failed processing 0 files\n"

				err := parseAndVerifyIcaclsOutput(targetDir, output)
				if err == nil {
					t.Fatalf("SECURITY VIOLATION: continuation line principal %q was stripped and unauthorized principal accepted", tc.principal)
				}
			})
		}

		// Also verify line 0 without trailing whitespace does NOT strip path prefix
		t.Run("Line0WithoutTrailingWhitespaceRejected", func(t *testing.T) {
			output := targetDir + "BUILTIN\\Administrators:(OI)(CI)(F)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n"
			err := parseAndVerifyIcaclsOutput(targetDir, output)
			if err == nil {
				t.Fatal("SECURITY VIOLATION: line 0 without whitespace delimiter was path-stripped")
			}
		})

		// Positive control: valid line 0 with whitespace and valid continuation line are accepted
		t.Run("ValidLine0AndContinuationAccepted", func(t *testing.T) {
			output := targetDir + " NT AUTHORITY\\SYSTEM:(OI)(CI)(F)\n" +
				"                      BUILTIN\\Administrators:(OI)(CI)(F)\n" +
				"Successfully processed 1 files; Failed processing 0 files\n"
			if err := parseAndVerifyIcaclsOutput(targetDir, output); err != nil {
				t.Fatalf("expected valid line 0 and continuation line to pass, got: %v", err)
			}
		})
	})

	// 3. Permission Rights Syntax and Token Validation
	t.Run("PermissionRightsParsing", func(t *testing.T) {
		invalidRights := []struct {
			name   string
			rights string
		}{
			{
				name:   "empty parens",
				rights: "()",
			},
			{
				name:   "nested parens",
				rights: "((F))",
			},
			{
				name:   "unclosed paren start",
				rights: "(F",
			},
			{
				name:   "unclosed paren sequence",
				rights: "(OI)(CI",
			},
			{
				name:   "invalid token NOT_A_PERMISSION",
				rights: "(NOT_A_PERMISSION)",
			},
			{
				name:   "valid token followed by invalid token",
				rights: "(OI)(CI)(BOGUS)",
			},
			{
				name:   "empty token between valid tokens",
				rights: "(OI)()(F)",
			},
			{
				name:   "trailing non-paren text",
				rights: "(F)EXTRA",
			},
			{
				name:   "empty rights string",
				rights: "",
			},
		}

		for _, tc := range invalidRights {
			t.Run(tc.name, func(t *testing.T) {
				output := targetDir + " *S-1-3-4:" + tc.rights + "\n" +
					"Successfully processed 1 files; Failed processing 0 files\n"
				err := parseAndVerifyIcaclsOutput(targetDir, output)
				if err == nil {
					t.Fatalf("SECURITY VIOLATION: invalid permission rights %q was accepted", tc.rights)
				}
			})
		}

		// Positive controls: standard valid rights token combinations pass
		validRights := []struct {
			name   string
			rights string
		}{
			{"Full access", "(F)"},
			{"Inheritance and Full access", "(OI)(CI)(F)"},
			{"Inherited container and Full access", "(I)(OI)(CI)(F)"},
			{"Inherit only Full access", "(OI)(CI)(IO)(F)"},
			{"Modify access", "(M)"},
			{"Read and execute", "(RX)"},
			{"Read only", "(R)"},
			{"Write only", "(W)"},
			{"Delete access", "(D)"},
			{"Specific rights combination", "(WDAC)(WO)(RC)(S)"},
		}

		for _, tc := range validRights {
			t.Run(tc.name, func(t *testing.T) {
				output := targetDir + " *S-1-3-4:" + tc.rights + "\n" +
					"Successfully processed 1 files; Failed processing 0 files\n"
				if err := parseAndVerifyIcaclsOutput(targetDir, output); err != nil {
					t.Fatalf("expected valid rights %q to pass, got: %v", tc.rights, err)
				}
			})
		}
	})

	// 4. Verification Controls: Authorized identities pass; broad groups and malformed outputs fail
	t.Run("VerificationControls", func(t *testing.T) {
		// Positive controls: exact authorized identities
		authorizedPrincipals := []string{
			"*S-1-3-4",
			"*S-1-5-18",
			"*S-1-5-32-544",
			"NT AUTHORITY\\SYSTEM",
			"BUILTIN\\Administrators",
			"NT AUTHORITY\\Owner Rights",
			"Owner Rights",
			"System",
			"Administrators",
		}

		if curUser, err := user.Current(); err == nil {
			if curUser.Uid != "" {
				authorizedPrincipals = append(authorizedPrincipals, "*"+curUser.Uid)
				authorizedPrincipals = append(authorizedPrincipals, curUser.Uid)
			}
			if curUser.Username != "" {
				authorizedPrincipals = append(authorizedPrincipals, curUser.Username)
			}
		}

		for _, p := range authorizedPrincipals {
			t.Run("authorized_"+p, func(t *testing.T) {
				output := targetDir + " " + p + ":(OI)(CI)(F)\n" +
					"Successfully processed 1 files; Failed processing 0 files\n"
				if err := parseAndVerifyIcaclsOutput(targetDir, output); err != nil {
					t.Fatalf("expected authorized principal %q to pass, got: %v", p, err)
				}
			})
		}

		// Negative controls: broad group identities must be rejected
		broadGroups := []string{
			"Everyone",
			"*S-1-1-0",
			"Users",
			"BUILTIN\\Users",
			"*S-1-5-32-545",
			"Authenticated Users",
			"NT AUTHORITY\\Authenticated Users",
			"*S-1-5-11",
			"Interactive",
			"NT AUTHORITY\\Interactive",
			"*S-1-5-4",
			"Guests",
			"BUILTIN\\Guests",
			"*S-1-5-32-546",
		}

		for _, bg := range broadGroups {
			t.Run("disallowed_"+bg, func(t *testing.T) {
				output := targetDir + " " + bg + ":(OI)(CI)(F)\n" +
					"Successfully processed 1 files; Failed processing 0 files\n"
				err := parseAndVerifyIcaclsOutput(targetDir, output)
				if err == nil {
					t.Fatalf("SECURITY VIOLATION: broad group %q was accepted", bg)
				}
			})
		}

		// Negative controls: malformed / empty outputs
		malformedOutputs := []struct {
			name   string
			output string
		}{
			{"empty", ""},
			{"whitespace only", "   \n\t  \n  "},
			{"summary only", "Successfully processed 1 files; Failed processing 0 files\n"},
			{"missing principal", targetDir + " :(OI)(CI)(F)\nSuccessfully processed 1 files; Failed processing 0 files\n"},
			{"missing colon", targetDir + " *S-1-3-4(OI)(CI)(F)\nSuccessfully processed 1 files; Failed processing 0 files\n"},
		}

		for _, mo := range malformedOutputs {
			t.Run("malformed_"+mo.name, func(t *testing.T) {
				err := parseAndVerifyIcaclsOutput(targetDir, mo.output)
				if err == nil {
					t.Fatalf("SECURITY VIOLATION: malformed output %q was accepted", mo.name)
				}
			})
		}
	})
}

// TestReviewE232_WindowsIntegration_EnvironmentSpoofingAndImmutability executes real icacls
// commands on Windows platforms to establish that environment variable modification does NOT
// affect ACL enforcement or permit unauthorized access.
func TestReviewE232_WindowsIntegration_EnvironmentSpoofingAndImmutability(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("skipping Windows integration test on non-Windows platform")
	}

	dir := t.TempDir()

	// 1. Ensure directory has strict permissions
	if err := EnsureDirectoryPermissions(dir); err != nil {
		t.Fatalf("EnsureDirectoryPermissions failed: %v", err)
	}

	// 2. Initial verification passes
	if err := VerifyDirectoryPermissions(dir); err != nil {
		t.Fatalf("VerifyDirectoryPermissions failed on initial setup: %v", err)
	}

	// 3. Spoof USERNAME and USERDOMAIN environment variables
	t.Setenv("USERNAME", "SpoofedExternalIdentity")
	t.Setenv("USERDOMAIN", "SPOOFEDDOMAIN")

	// 4. VerifyDirectoryPermissions must still evaluate identity via OS token, not spoofed env
	if err := VerifyDirectoryPermissions(dir); err != nil {
		t.Fatalf("VerifyDirectoryPermissions failed under spoofed environment: %v", err)
	}

	// 5. Add an unauthorized explicit grant to Users (*S-1-5-32-545) via icacls
	grantCmd := exec.Command("icacls", dir, "/grant", "*S-1-5-32-545:(OI)(CI)R")
	if out, err := grantCmd.CombinedOutput(); err != nil {
		t.Fatalf("grant command failed: %v, out: %s", err, string(out))
	}
	defer func() {
		// Clean up grant
		_ = exec.Command("icacls", dir, "/remove", "*S-1-5-32-545").Run()
	}()

	// 6. VerifyDirectoryPermissions MUST detect and reject the unauthorized explicit grant
	if err := VerifyDirectoryPermissions(dir); err == nil {
		t.Fatal("SECURITY VIOLATION: VerifyDirectoryPermissions accepted unauthorized explicit grant to Users")
	}
}
