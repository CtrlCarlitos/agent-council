package storage

import (
	"regexp"
)

var (
	githubTokenRegex = regexp.MustCompile(`ghp_[A-Za-z0-9_]{36}`)
	apiKeyRegex      = regexp.MustCompile(`sk-[A-Za-z0-9_]{32,}`)
	slackTokenRegex  = regexp.MustCompile(`xox[baprs]-[A-Za-z0-9_]+`)
)

// SanitizeText redacts sensitive tokens before persistence.
func SanitizeText(input string) string {
	out := githubTokenRegex.ReplaceAllString(input, "[REDACTED]")
	out = apiKeyRegex.ReplaceAllString(out, "[REDACTED]")
	out = slackTokenRegex.ReplaceAllString(out, "[REDACTED]")
	return out
}
