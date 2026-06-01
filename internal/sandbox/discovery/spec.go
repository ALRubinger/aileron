package discovery

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
)

const specOperationEndpoint = "/connector-operations/run"

// SpecConnectorTool is one spec-backed connector command surfaced inside the
// sandbox.
type SpecConnectorTool struct {
	Name        string
	FQN         string
	Description string
	Operations  []SpecOperationHelp
}

// SpecOperationHelp is the help text payload for one spec operation.
type SpecOperationHelp struct {
	Name        string
	Summary     string
	Description string
	Method      string
	Path        string
	Idempotency string
	Approval    string
	Credential  string
	Inputs      []InputHelp
}

// SpecToolsText renders /etc/aileron/tools.txt entries from connector specs.
func SpecToolsText(specs []connectorspec.Spec) ([]byte, error) {
	tools, err := SpecConnectorTools(specs)
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	for _, tool := range tools {
		operationNames := make([]string, 0, len(tool.Operations))
		for _, operation := range tool.Operations {
			operationNames = append(operationNames, operation.Name)
		}
		out.WriteString(tool.Name)
		out.WriteString("\t")
		out.WriteString(tool.FQN)
		out.WriteString(" -- Aileron connector operations: ")
		out.WriteString(strings.Join(operationNames, ", "))
		out.WriteString("\n")
	}
	return out.Bytes(), nil
}

// SpecShimScripts renders static connector operation shim scripts keyed by
// command name.
func SpecShimScripts(specs []connectorspec.Spec) (map[string][]byte, error) {
	tools, err := SpecConnectorTools(specs)
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		return nil, nil
	}
	scripts := make(map[string][]byte, len(tools))
	for _, tool := range tools {
		scripts[tool.Name] = specShimScript(tool)
	}
	return scripts, nil
}

// SpecConnectorTools derives sandbox-visible connector tools from specs.
func SpecConnectorTools(specs []connectorspec.Spec) ([]SpecConnectorTool, error) {
	tools := []SpecConnectorTool{}
	used := map[string]string{}
	for _, spec := range specs {
		fqn := strings.TrimSpace(spec.Connector.FQN)
		for _, rawTool := range spec.Tools {
			name := sanitizeToolName(rawTool.Name)
			if name == "" {
				return nil, fmt.Errorf("connector spec %s declares an empty tool name", fqn)
			}
			if owner, ok := used[name]; ok {
				return nil, fmt.Errorf("connector spec tool name conflict %q between %s and %s", name, owner, fqn)
			}
			used[name] = fqn
			operations := make([]SpecOperationHelp, 0, len(rawTool.Operations))
			for _, operation := range rawTool.Operations {
				operations = append(operations, specOperationHelp(operation))
			}
			sort.Slice(operations, func(i, j int) bool {
				return operations[i].Name < operations[j].Name
			})
			tools = append(tools, SpecConnectorTool{
				Name:        name,
				FQN:         fqn,
				Description: strings.TrimSpace(rawTool.Description),
				Operations:  operations,
			})
		}
	}
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].Name == tools[j].Name {
			return tools[i].FQN < tools[j].FQN
		}
		return tools[i].Name < tools[j].Name
	})
	return tools, nil
}

func specOperationHelp(operation connectorspec.Operation) SpecOperationHelp {
	help := SpecOperationHelp{
		Name:        strings.TrimSpace(operation.Name),
		Summary:     strings.TrimSpace(operation.Summary),
		Description: strings.TrimSpace(operation.Description),
		Method:      strings.TrimSpace(operation.Method),
		Path:        strings.TrimSpace(operation.Path),
		Idempotency: strings.TrimSpace(operation.Idempotency),
		Approval:    strings.TrimSpace(operation.Approval),
		Credential:  strings.TrimSpace(operation.Credential),
	}
	for _, input := range operation.Inputs {
		name := strings.TrimSpace(input.Name)
		if name == "" {
			continue
		}
		help.Inputs = append(help.Inputs, InputHelp{
			Name:        name,
			Type:        strings.TrimSpace(input.Type),
			Required:    input.Required,
			Description: strings.TrimSpace(input.Description),
		})
	}
	return help
}

func specShimScript(tool SpecConnectorTool) []byte {
	var help bytes.Buffer
	fmt.Fprintf(&help, "Aileron connector shim: %s\n", tool.Name)
	fmt.Fprintf(&help, "Connector: %s\n", tool.FQN)
	if tool.Description != "" {
		fmt.Fprintf(&help, "%s\n", tool.Description)
	}
	fmt.Fprintf(&help, "\nOperations:\n")
	for _, operation := range tool.Operations {
		summary := operation.Summary
		if summary == "" {
			summary = operation.Description
		}
		if summary == "" {
			summary = "Aileron connector operation"
		}
		fmt.Fprintf(&help, "  %s - %s\n", operation.Name, summary)
		if operation.Method != "" || operation.Path != "" {
			fmt.Fprintf(&help, "    target: %s %s\n", operation.Method, operation.Path)
		}
		if operation.Credential != "" || operation.Approval != "" || operation.Idempotency != "" {
			fmt.Fprintf(&help, "    policy:")
			if operation.Credential != "" {
				fmt.Fprintf(&help, " credential=%s", operation.Credential)
			}
			if operation.Approval != "" {
				fmt.Fprintf(&help, " approval=%s", operation.Approval)
			}
			if operation.Idempotency != "" {
				fmt.Fprintf(&help, " idempotency=%s", operation.Idempotency)
			}
			fmt.Fprintf(&help, "\n")
		}
		if len(operation.Inputs) > 0 {
			fmt.Fprintf(&help, "    arguments:\n")
			for _, input := range operation.Inputs {
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
	fmt.Fprintf(&help, "\nRun `%s <operation-name> --args '<json-object>'` to execute a connector operation through the Aileron daemon API.\n", tool.Name)

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
	out.WriteString("operation_name=$1\n")
	out.WriteString("shift\n")
	out.WriteString("case \"$operation_name\" in\n")
	for _, operation := range tool.Operations {
		fmt.Fprintf(&out, "  %s)\n", shellQuote(operation.Name))
		out.WriteString("    ;;\n")
	}
	out.WriteString("  *)\n")
	fmt.Fprintf(&out, "    echo 'Unknown operation for %s: '\"$operation_name\" >&2\n", shellSingleQuote(tool.Name))
	out.WriteString("    echo 'Run with --help to list connector operations.' >&2\n")
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
	out.WriteString(" <operation-name> [--args <json-object>] [--json]' >&2\n")
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
	out.WriteString("body='{\"connector_fqn\":\"")
	out.WriteString(jsonStringLiteralContent(tool.FQN))
	out.WriteString("\",\"tool\":\"")
	out.WriteString(jsonStringLiteralContent(tool.Name))
	out.WriteString("\",\"operation\":\"'\"$operation_name\"'\",\"args\":'\"$args_json\"'}'\n")
	out.WriteString("url=${AILERON_API_URL%/}")
	out.WriteString(specOperationEndpoint)
	out.WriteString("\n")
	out.WriteString("set -- -T 30 -t 3 -q -O - --header 'Content-Type: application/json'\n")
	out.WriteString("if [ -n \"${AILERON_TOKEN:-}\" ]; then\n")
	out.WriteString("  set -- \"$@\" --header \"Authorization: Bearer $AILERON_TOKEN\"\n")
	out.WriteString("fi\n")
	out.WriteString("if [ -n \"${AILERON_SESSION_ID:-}\" ]; then\n")
	out.WriteString("  set -- \"$@\" --header \"X-Aileron-Session-Id: $AILERON_SESSION_ID\"\n")
	out.WriteString("fi\n")
	out.WriteString("exec wget \"$@\" --post-data \"$body\" \"$url\"\n")
	return out.Bytes()
}

func jsonStringLiteralContent(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}
