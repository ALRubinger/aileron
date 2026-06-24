package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// schemaLocation is the $id the embedded schema declares. The compiler
// keys the resource by this URI.
const schemaLocation = "https://withaileron.ai/schema/flight-plan-manifest.schema.json"

var (
	compiledSchema     *jsonschema.Schema
	compiledSchemaErr  error
	compiledSchemaOnce sync.Once
)

// schema compiles the embedded Flight Plan manifest schema once and
// returns the cached result on subsequent calls.
func schema() (*jsonschema.Schema, error) {
	compiledSchemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(embeddedSchema))
		if err != nil {
			compiledSchemaErr = fmt.Errorf("manifest: parse embedded schema JSON: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(schemaLocation, doc); err != nil {
			compiledSchemaErr = fmt.Errorf("manifest: add embedded schema resource: %w", err)
			return
		}
		sch, err := c.Compile(schemaLocation)
		if err != nil {
			compiledSchemaErr = fmt.Errorf("manifest: compile embedded schema: %w", err)
			return
		}
		compiledSchema = sch
	})
	return compiledSchema, compiledSchemaErr
}

// validateFrontmatter validates a SKILL.md YAML frontmatter block against
// the embedded schema. The schema validates the whole top-level object;
// the `aileron` key is optional at the top level (lossless-if-stripped),
// so this returns nil for a stripped manifest. When the `aileron` block is
// present but malformed, it returns a field-level validation error.
func validateFrontmatter(front []byte) error {
	sch, err := schema()
	if err != nil {
		return err
	}

	var m map[string]any
	if err := yaml.Unmarshal(front, &m); err != nil {
		return fmt.Errorf("manifest: parse frontmatter YAML: %w", err)
	}

	inst, err := jsonify(m)
	if err != nil {
		return err
	}

	if err := sch.Validate(inst); err != nil {
		return fmt.Errorf("manifest: aileron block failed schema validation: %w", err)
	}
	return nil
}

// jsonify round-trips a YAML-decoded value through JSON so the validator
// sees the value model it expects (string keys, float64 numbers, []any and
// map[string]any containers). YAML decodes maps as map[string]any here
// because the frontmatter envelope uses string keys, but nested values may
// still carry YAML-native types, so the round-trip normalizes them.
func jsonify(m map[string]any) (any, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(m); err != nil {
		return nil, fmt.Errorf("manifest: re-encode frontmatter to JSON: %w", err)
	}
	out, err := jsonschema.UnmarshalJSON(&buf)
	if err != nil {
		return nil, fmt.Errorf("manifest: decode frontmatter as JSON: %w", err)
	}
	return out, nil
}
