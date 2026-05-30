// Package discovery renders the sandbox-side discovery surfaces agents read
// inside container sessions.
package discovery

import (
	"bytes"
	"sort"
	"strings"
	"unicode"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// ToolsText renders /etc/aileron/tools.txt from installed action manifests.
// The watcher added later can call the same renderer when manifests change.
func ToolsText(actions []action.LoadedAction) []byte {
	byConnector := map[string]map[string]bool{}
	for _, loaded := range actions {
		if loaded.Manifest == nil {
			continue
		}
		actionName := strings.TrimSpace(loaded.Manifest.Name)
		if actionName == "" {
			continue
		}
		for _, connector := range loaded.Manifest.Requires.Connectors {
			fqn := strings.TrimSpace(connector.Name)
			if fqn == "" {
				continue
			}
			if byConnector[fqn] == nil {
				byConnector[fqn] = map[string]bool{}
			}
			byConnector[fqn][actionName] = true
		}
	}
	if len(byConnector) == 0 {
		return nil
	}

	fqns := make([]string, 0, len(byConnector))
	for fqn := range byConnector {
		fqns = append(fqns, fqn)
	}
	sort.Strings(fqns)

	var out bytes.Buffer
	for _, fqn := range fqns {
		actions := sortedKeys(byConnector[fqn])
		out.WriteString(toolName(fqn))
		out.WriteString("\t")
		out.WriteString(fqn)
		out.WriteString(" -- installed Aileron connector used by actions: ")
		out.WriteString(strings.Join(actions, ", "))
		out.WriteString("\n")
	}
	return out.Bytes()
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func toolName(fqn string) string {
	parsed, err := cstore.ParseFQN(fqn)
	if err != nil {
		return sanitizeToolName(fqn)
	}
	name := parsed.Repo
	if parsed.Subpath != "" {
		parts := strings.Split(parsed.Subpath, "/")
		name = parts[len(parts)-1]
	}
	name = strings.TrimPrefix(name, "aileron-connector-")
	name = strings.TrimPrefix(name, "connector-")
	return sanitizeToolName(name)
}

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
