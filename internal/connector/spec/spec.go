// Package spec defines the machine-readable connector operation spec used by
// sandbox discovery and generated HTTPS shims.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ALRubinger/aileron/internal/cstore"
)

const (
	// SchemaVersion is the current connector spec schema version.
	SchemaVersion = "aileron.connector.v1"
	// FileName is the connector package file loaded from installed entries.
	FileName = "aileron.connector.v1.json"
)

// Spec is the root connector operation spec.
type Spec struct {
	SchemaVersion string    `json:"schema_version"`
	Connector     Connector `json:"connector"`
	Tools         []Tool    `json:"tools"`
	SourcePath    string    `json:"-"`
	SourceHash    string    `json:"-"`
}

// Connector identifies the connector package providing the operations.
type Connector struct {
	FQN     string `json:"fqn"`
	Version string `json:"version,omitempty"`
}

// Tool describes one sandbox-visible command rendered as a shim.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Operations  []Operation `json:"operations"`
}

// Operation describes one operation exposed by a generated HTTPS shim.
type Operation struct {
	Name        string       `json:"name"`
	Summary     string       `json:"summary,omitempty"`
	Description string       `json:"description,omitempty"`
	Method      string       `json:"method,omitempty"`
	Path        string       `json:"path,omitempty"`
	Idempotency string       `json:"idempotency,omitempty"`
	Approval    string       `json:"approval,omitempty"`
	Credential  string       `json:"credential,omitempty"`
	Inputs      []Input      `json:"inputs,omitempty"`
	Audit       []AuditField `json:"audit,omitempty"`
}

// Input describes one operation argument rendered into shim help.
type Input struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// AuditField describes one audit metadata field the data plane should record.
type AuditField struct {
	Name        string `json:"name"`
	Source      string `json:"source,omitempty"`
	Redact      bool   `json:"redact,omitempty"`
	Description string `json:"description,omitempty"`
}

// LoadInstalled loads connector specs from a cstore root.
func LoadInstalled(root string) ([]Spec, error) {
	if root == "" {
		root = cstore.DefaultRoot()
	}
	shaRoot := filepath.Join(root, "connectors", "sha256")
	entries, err := os.ReadDir(shaRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read connector specs: %w", err)
	}
	specs := make([]Spec, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(shaRoot, entry.Name(), FileName)
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read connector spec %s: %w", path, err)
		}
		loaded, err := Parse(path, data)
		if err != nil {
			return nil, err
		}
		loaded.SourceHash = "sha256:" + entry.Name()
		specs = append(specs, loaded)
	}
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].Connector.FQN == specs[j].Connector.FQN {
			return specs[i].SourceHash < specs[j].SourceHash
		}
		return specs[i].Connector.FQN < specs[j].Connector.FQN
	})
	return specs, nil
}

// Parse decodes and validates one connector spec.
func Parse(path string, data []byte) (Spec, error) {
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("parse connector spec %s: %w", path, err)
	}
	spec.SourcePath = path
	if err := Validate(spec); err != nil {
		return Spec{}, fmt.Errorf("validate connector spec %s: %w", path, err)
	}
	return spec, nil
}

// Validate checks the required schema, identity, tool, and operation fields.
func Validate(spec Spec) error {
	if strings.TrimSpace(spec.SchemaVersion) != SchemaVersion {
		return fmt.Errorf("schema_version must be %q", SchemaVersion)
	}
	fqn := strings.TrimSpace(spec.Connector.FQN)
	if fqn == "" {
		return fmt.Errorf("connector.fqn is required")
	}
	if _, err := cstore.ParseFQN(fqn); err != nil {
		return fmt.Errorf("connector.fqn: %w", err)
	}
	if len(spec.Tools) == 0 {
		return fmt.Errorf("at least one tool is required")
	}
	toolNames := map[string]struct{}{}
	for _, tool := range spec.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return fmt.Errorf("tool name is required")
		}
		if !validName(name) {
			return fmt.Errorf("tool name %q contains unsupported characters; use letters, digits, dots, dashes, underscores, or colons", name)
		}
		if _, ok := toolNames[name]; ok {
			return fmt.Errorf("duplicate tool name %q", name)
		}
		toolNames[name] = struct{}{}
		if len(tool.Operations) == 0 {
			return fmt.Errorf("tool %q must declare at least one operation", name)
		}
		operationNames := map[string]struct{}{}
		for _, operation := range tool.Operations {
			opName := strings.TrimSpace(operation.Name)
			if opName == "" {
				return fmt.Errorf("tool %q operation name is required", name)
			}
			if !validName(opName) {
				return fmt.Errorf("tool %q operation name %q contains unsupported characters; use letters, digits, dots, dashes, underscores, or colons", name, opName)
			}
			if _, ok := operationNames[opName]; ok {
				return fmt.Errorf("tool %q has duplicate operation %q", name, opName)
			}
			operationNames[opName] = struct{}{}
			if err := validateNamedFields("input", operation.Inputs, func(input Input) string { return input.Name }, name, opName); err != nil {
				return err
			}
			if err := validateNamedFields("audit field", operation.Audit, func(field AuditField) string { return field.Name }, name, opName); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateNamedFields[T any](kind string, fields []T, name func(T) string, toolName, operationName string) error {
	names := map[string]struct{}{}
	for _, field := range fields {
		fieldName := strings.TrimSpace(name(field))
		if fieldName == "" {
			return fmt.Errorf("tool %q operation %q %s name is required", toolName, operationName, kind)
		}
		if !validName(fieldName) {
			return fmt.Errorf("tool %q operation %q %s name %q contains unsupported characters; use letters, digits, dots, dashes, underscores, or colons", toolName, operationName, kind, fieldName)
		}
		if _, ok := names[fieldName]; ok {
			return fmt.Errorf("tool %q operation %q has duplicate %s name %q", toolName, operationName, kind, fieldName)
		}
		names[fieldName] = struct{}{}
	}
	return nil
}

func validName(name string) bool {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == ':':
		default:
			return false
		}
	}
	return name != ""
}
