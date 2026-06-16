package discovery

import (
	"fmt"
	"sort"
	"strings"

	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
)

// SpecConnectorTool is one spec-backed connector tool derived for
// data-plane operation validation.
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
	Hosts       []string
	Idempotency string
	Approval    string
	Credential  string
	Inputs      []InputHelp
}

// SpecConnectorTools derives connector tools from specs for data-plane
// operation validation.
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
		Hosts:       trimNonEmptyStrings(operation.Hosts),
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

func trimNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
