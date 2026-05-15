package wrap

import (
	"regexp"
	"sort"
	"strings"
)

// DetectCredentialEnvKeys parses `--help` output for environment
// variables that look like credentials and returns them sorted
// and deduplicated. Intended for `aileron cli add` (issue #750):
// the introspector runs the help command through the platform
// sandbox, then this heuristic picks out the env keys whose
// shape strongly suggests a secret.
//
// Recognized name suffixes — all matched against POSIX-shape
// uppercase tokens:
//
//   - `_TOKEN` / `_API_TOKEN` / `_AUTH_TOKEN` / `_ACCESS_TOKEN`
//   - `_KEY` / `_API_KEY`
//   - `_SECRET` / `_CLIENT_SECRET`
//   - `_PASSWORD`
//   - `_PAT` (Personal Access Token, the GitHub convention)
//   - `_AUTH` (broad; used by some HTTP-Auth wrappers)
//   - the literal names `TOKEN`, `API_KEY`, `API_TOKEN`,
//     `SECRET`, `PASSWORD` standing on their own (cobra
//     environment-only docs sometimes use these bare)
//
// The heuristic is intentionally conservative: false positives
// (the user is prompted for a value that isn't actually a
// credential and just declines) are preferable to silent misses
// (the user thinks they configured their CLI, then `aileron
// launch` fails opaque at first call). Tests pin the patterns
// against the validation CLIs in PrintingPress's catalog —
// `linear` (LINEAR_API_TOKEN), `sentry` (SENTRY_AUTH_TOKEN),
// `coingecko` (COINGECKO_API_KEY).
//
// Returns nil for empty input or input with no candidates.
func DetectCredentialEnvKeys(helpText string) []string {
	if helpText == "" {
		return nil
	}
	seen := map[string]struct{}{}
	for _, tok := range envTokenRe.FindAllString(helpText, -1) {
		if looksLikeCredentialEnvName(tok) {
			seen[tok] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// envTokenRe matches a POSIX-shape uppercase environment
// variable token: leading letter or underscore, then upper
// alphanumerics + underscore, length ≥ 3. The lower bound is
// load-bearing — without it the scanner pulls noise like "A"
// out of inline ASCII tables in --help output.
//
// Word boundaries on both sides keep `LINEAR_API_TOKEN` from
// matching as the substring of `MY_LINEAR_API_TOKEN_ALIAS` (the
// caller would still get the alias as its own token).
var envTokenRe = regexp.MustCompile(`\b[A-Z_][A-Z0-9_]{2,}\b`)

// credentialSuffixes pins the substring tail any env-var token
// must end with to be flagged as a credential. The list is the
// closed v1 set — adding patterns requires bumping the docs
// and corresponding tests so the heuristic's footprint stays
// reviewable.
var credentialSuffixes = []string{
	"_TOKEN",
	"_KEY",
	"_SECRET",
	"_PASSWORD",
	"_PAT",
	"_AUTH",
	"_PASS",     // shell-y alternate spelling some CLIs use
	"_API_AUTH", // canonical form even though "_AUTH" subsumes it
}

// credentialBareNames is the closed set of literal env names
// the heuristic accepts without a suffix. These are the bare
// shapes some CLIs document — typically tools whose env var is
// literally `TOKEN` rather than `$VENDOR_TOKEN`.
var credentialBareNames = map[string]struct{}{
	"TOKEN":     {},
	"API_KEY":   {},
	"API_TOKEN": {},
	"SECRET":    {},
	"PASSWORD": {},
}

// looksLikeCredentialEnvName encodes the recognition rule. The
// helper is exported via [DetectCredentialEnvKeys]; isolated
// here so individual cases stay testable.
func looksLikeCredentialEnvName(tok string) bool {
	if _, ok := credentialBareNames[tok]; ok {
		return true
	}
	for _, suffix := range credentialSuffixes {
		if strings.HasSuffix(tok, suffix) && tok != suffix {
			return true
		}
	}
	return false
}
