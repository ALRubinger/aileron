// Package discovery derives connector tools from installed connector
// specs for the daemon's HTTPS data-plane operation validation. It also
// provides the shared tool-name sanitizer used when mapping connector
// tool names to safe identifiers.
package discovery

import (
	"strings"
	"unicode"
)

// InputHelp is one operation argument surfaced in connector tool
// metadata consumed by data-plane operation validation.
type InputHelp struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

// sanitizeToolName normalizes a connector tool name to lowercase
// letters, digits, underscores, dots, and single dashes, trimming any
// leading or trailing dashes.
func sanitizeToolName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var out strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '_', r == '.':
			out.WriteRune(r)
			lastDash = false
		case r == '-':
			if !lastDash {
				out.WriteByte('-')
				lastDash = true
			}
		default:
			if !lastDash {
				out.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(out.String(), "-")
}
