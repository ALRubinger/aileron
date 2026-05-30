// Package discovery renders the sandbox-side discovery surfaces agents read
// inside container sessions.
package discovery

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// ConnectorTool is one connector command surfaced inside the sandbox.
type ConnectorTool struct {
	Name    string
	FQN     string
	Actions []ActionHelp
}

// ActionHelp is the help text payload for one installed action that uses a
// connector.
type ActionHelp struct {
	Name   string
	Intent string
	Inputs []InputHelp
}

// InputHelp is one action argument rendered into shim --help output.
type InputHelp struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

// ToolsText renders /etc/aileron/tools.txt from installed action manifests.
// The watcher added later can call the same renderer when manifests change.
func ToolsText(actions []action.LoadedAction) []byte {
	tools := ConnectorTools(actions)
	if len(tools) == 0 {
		return nil
	}

	var out bytes.Buffer
	for _, tool := range tools {
		actionNames := make([]string, 0, len(tool.Actions))
		for _, action := range tool.Actions {
			actionNames = append(actionNames, action.Name)
		}
		out.WriteString(tool.Name)
		out.WriteString("\t")
		out.WriteString(tool.FQN)
		out.WriteString(" -- installed Aileron connector used by actions: ")
		out.WriteString(strings.Join(actionNames, ", "))
		out.WriteString("\n")
	}
	return out.Bytes()
}

// ShimScripts renders static connector shim scripts keyed by command name.
func ShimScripts(actions []action.LoadedAction) map[string][]byte {
	tools := ConnectorTools(actions)
	if len(tools) == 0 {
		return nil
	}
	scripts := make(map[string][]byte, len(tools))
	for _, tool := range tools {
		scripts[tool.Name] = shimScript(tool)
	}
	return scripts
}

// ConnectorTools derives the sandbox-visible connector tools from installed
// action manifests.
func ConnectorTools(actions []action.LoadedAction) []ConnectorTool {
	byConnector := map[string]map[string]ActionHelp{}
	for _, loaded := range actions {
		if loaded.Manifest == nil {
			continue
		}
		actionName := strings.TrimSpace(loaded.Manifest.Name)
		if actionName == "" {
			continue
		}
		help := actionHelp(loaded.Manifest)
		for _, connector := range loaded.Manifest.Requires.Connectors {
			fqn := strings.TrimSpace(connector.Name)
			if fqn == "" {
				continue
			}
			if byConnector[fqn] == nil {
				byConnector[fqn] = map[string]ActionHelp{}
			}
			byConnector[fqn][actionName] = help
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

	tools := make([]ConnectorTool, 0, len(fqns))
	usedNames := map[string]int{}
	for _, fqn := range fqns {
		actionNames := sortedKeys(byConnector[fqn])
		actions := make([]ActionHelp, 0, len(actionNames))
		for _, name := range actionNames {
			actions = append(actions, byConnector[fqn][name])
		}
		name := uniqueToolName(toolName(fqn), usedNames)
		tools = append(tools, ConnectorTool{
			Name:    name,
			FQN:     fqn,
			Actions: actions,
		})
	}
	return tools
}

func actionHelp(manifest *action.Manifest) ActionHelp {
	help := ActionHelp{
		Name:   strings.TrimSpace(manifest.Name),
		Intent: strings.TrimSpace(manifest.Match.Intent),
	}
	for _, input := range manifest.Inputs {
		name := strings.TrimSpace(input.Name)
		if name == "" {
			continue
		}
		help.Inputs = append(help.Inputs, InputHelp{
			Name:        name,
			Type:        strings.TrimSpace(input.Type),
			Required:    input.IsRequired(),
			Description: strings.TrimSpace(input.Description),
		})
	}
	return help
}

func sortedKeys[T any](values map[string]T) []string {
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

func uniqueToolName(name string, used map[string]int) string {
	if name == "" {
		name = "connector"
	}
	count := used[name]
	used[name] = count + 1
	if count == 0 {
		return name
	}
	return fmt.Sprintf("%s-%d", name, count+1)
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

func shimScript(tool ConnectorTool) []byte {
	var help bytes.Buffer
	fmt.Fprintf(&help, "Aileron connector shim: %s\n", tool.Name)
	fmt.Fprintf(&help, "Connector: %s\n\n", tool.FQN)
	fmt.Fprintf(&help, "Installed actions:\n")
	for _, action := range tool.Actions {
		summary := action.Intent
		if summary == "" {
			summary = "installed Aileron action"
		}
		fmt.Fprintf(&help, "  %s - %s\n", action.Name, summary)
		if len(action.Inputs) > 0 {
			fmt.Fprintf(&help, "    arguments:\n")
			for _, input := range action.Inputs {
				required := "optional"
				if input.Required {
					required = "required"
				}
				typ := input.Type
				if typ == "" {
					typ = "value"
				}
				description := input.Description
				if description != "" {
					description = " - " + description
				}
				fmt.Fprintf(&help, "      --%s <%s> (%s)%s\n", input.Name, typ, required, description)
			}
		}
	}
	fmt.Fprintf(&help, "\nRun `%s <action-name> --args '<json-object>'` to execute an installed action through the Aileron daemon API.\n", tool.Name)

	var out bytes.Buffer
	out.WriteString("#!/bin/sh\n")
	out.WriteString("set -u\n")
	out.WriteString("case \"${1:-}\" in\n")
	out.WriteString("  \"\"|\"-h\"|\"--help\"|\"help\")\n")
	out.WriteString("    cat <<'AILERON_HELP'\n")
	out.Write(help.Bytes())
	out.WriteString("AILERON_HELP\n")
	out.WriteString("    exit 0\n")
	out.WriteString("    ;;\n")
	out.WriteString("esac\n")
	out.WriteString("action_name=$1\n")
	out.WriteString("shift\n")
	out.WriteString("case \"$action_name\" in\n")
	for _, action := range tool.Actions {
		fmt.Fprintf(&out, "  %s)\n", shellQuote(action.Name))
		out.WriteString("    ;;\n")
	}
	out.WriteString("  *)\n")
	fmt.Fprintf(&out, "    echo 'Unknown action for %s: '\"$action_name\" >&2\n", shellSingleQuote(tool.Name))
	out.WriteString("    echo 'Run with --help to list installed actions.' >&2\n")
	out.WriteString("    exit 64\n")
	out.WriteString("    ;;\n")
	out.WriteString("esac\n")
	out.WriteString("args_json='{}'\n")
	out.WriteString("while [ \"$#\" -gt 0 ]; do\n")
	out.WriteString("  case \"$1\" in\n")
	out.WriteString("    --args)\n")
	out.WriteString("      shift\n")
	out.WriteString("      if [ \"$#\" -eq 0 ]; then\n")
	out.WriteString("        echo 'Missing value for --args' >&2\n")
	out.WriteString("        exit 64\n")
	out.WriteString("      fi\n")
	out.WriteString("      args_json=$1\n")
	out.WriteString("      ;;\n")
	out.WriteString("    --json)\n")
	out.WriteString("      ;;\n")
	out.WriteString("    -h|--help|help)\n")
	out.WriteString("      exec \"$0\" --help\n")
	out.WriteString("      ;;\n")
	out.WriteString("    *)\n")
	out.WriteString("      echo 'Unsupported argument: '\"$1\" >&2\n")
	out.WriteString("      echo 'Usage: ")
	out.WriteString(shellSingleQuote(tool.Name))
	out.WriteString(" <action-name> [--args <json-object>] [--json]' >&2\n")
	out.WriteString("      exit 64\n")
	out.WriteString("      ;;\n")
	out.WriteString("  esac\n")
	out.WriteString("  shift\n")
	out.WriteString("done\n")
	out.WriteString("if [ -z \"${AILERON_API_URL:-}\" ]; then\n")
	out.WriteString("  echo 'AILERON_API_URL is not set; cannot reach the Aileron daemon API.' >&2\n")
	out.WriteString("  exit 69\n")
	out.WriteString("fi\n")
	out.WriteString("if ! command -v wget >/dev/null 2>&1; then\n")
	out.WriteString("  echo 'wget is required for Aileron connector shims in this sandbox image.' >&2\n")
	out.WriteString("  exit 69\n")
	out.WriteString("fi\n")
	out.WriteString("body='{\"args\":'\"$args_json\"'}'\n")
	out.WriteString("url=${AILERON_API_URL%/}/actions/$action_name/run\n")
	out.WriteString("if [ -n \"${AILERON_TOKEN:-}\" ]; then\n")
	out.WriteString("  exec wget -T 30 -t 3 -q -O - --header 'Content-Type: application/json' --header \"Authorization: Bearer $AILERON_TOKEN\" --post-data \"$body\" \"$url\"\n")
	out.WriteString("fi\n")
	out.WriteString("exec wget -T 30 -t 3 -q -O - --header 'Content-Type: application/json' --post-data \"$body\" \"$url\"\n")
	return out.Bytes()
}

func shellQuote(value string) string {
	return "'" + shellSingleQuote(value) + "'"
}

func shellSingleQuote(value string) string {
	return strings.ReplaceAll(value, "'", "'\"'\"'")
}
