package storage

import (
	"regexp"
)

var (
	githubTokenRegex   = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{16,}`)
	anthropicKeyRegex  = regexp.MustCompile(`sk-ant-(?:api03-)?[A-Za-z0-9_\-]{20,}`)
	bearerTokenRegex   = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9_\-\.]{8,}`)
	genericApiKeyRegex = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`)
	slackTokenRegex    = regexp.MustCompile(`xox[baprs]-[A-Za-z0-9_\-]+`)
)

// SanitizeText redacts sensitive tokens before persistence.
func SanitizeText(input string) string {
	out := githubTokenRegex.ReplaceAllString(input, "[REDACTED]")
	out = anthropicKeyRegex.ReplaceAllString(out, "[REDACTED]")
	out = bearerTokenRegex.ReplaceAllString(out, "[REDACTED]")
	out = genericApiKeyRegex.ReplaceAllString(out, "[REDACTED]")
	out = slackTokenRegex.ReplaceAllString(out, "[REDACTED]")
	return out
}

// containsDisallowedCredential reports whether the input string contains sensitive tokens.
func containsDisallowedCredential(input string) bool {
	return githubTokenRegex.MatchString(input) ||
		anthropicKeyRegex.MatchString(input) ||
		bearerTokenRegex.MatchString(input) ||
		genericApiKeyRegex.MatchString(input) ||
		slackTokenRegex.MatchString(input)
}
