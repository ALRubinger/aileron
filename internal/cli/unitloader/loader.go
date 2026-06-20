// Package unitloader is the host-side bridge from a sandbox image's
// devcontainer.metadata OCI label to the two existing credential consumers:
// the internal/auth/capture registry and the internal/proxybinding host
// table (umbrella #1319, sub-issue #1322).
//
// The devcontainer CLI stamps a built image with a devcontainer.metadata
// label whose value is a JSON array of per-Feature metadata objects. Each
// element may carry a customizations.aileron.cli block declaring one CLI
// tool's complete credential story as data (acquisition + sealing). This
// package reads that label, parses each present cli block through the
// canonical internal/cli.Parse, and projects the resulting units additively
// into a capture-descriptor layer and a proxybinding-entry layer. Both
// layers sit between the embedded built-in defaults and the user override
// layer in their respective loaders.
//
// This package owns no field semantics. It re-uses internal/cli's Unit type
// and its two conversion adapters (ToCaptureDescriptor, ToSealingEntries),
// and it reads the image label through internal/sandbox/container's
// ImageMetadataLabel. There is no new decoder, no new descriptor format, and
// no change to any consumer's internals.
//
// Failure posture has two distinct modes by design. An image whose label is
// absent or carries no customizations.aileron.cli is a clean no-op: the
// consumers ship the embedded defaults alone, preserving today's behavior. A
// label that is present but malformed (invalid JSON, or a cli block that
// fails the canonical unit validation) is a loud error: a present-but-broken
// unit must not silently ship nothing.
package unitloader

import (
	"context"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/ALRubinger/aileron/internal/auth/capture"
	"github.com/ALRubinger/aileron/internal/cli"
	"github.com/ALRubinger/aileron/internal/proxybinding"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// featureMetadata is the minimal projection of one devcontainer.metadata
// array element this package reads. Each element carries arbitrary
// devcontainer fields; only customizations.aileron.cli is load-bearing here.
// The cli block is captured as json.RawMessage so a present block is
// distinguishable from an absent one and re-marshalled losslessly for the
// canonical parser.
type featureMetadata struct {
	Customizations struct {
		Aileron struct {
			CLI json.RawMessage `json:"cli"`
		} `json:"aileron"`
	} `json:"customizations"`
}

// UnitsFromMetadata parses a devcontainer.metadata label value into zero or
// more CLI units. The label value is the devcontainer CLI's merged metadata
// array: a JSON array of per-Feature objects. For each element carrying a
// customizations.aileron.cli block, the block is re-marshalled and parsed
// through the canonical internal/cli.Parse (JSON is a YAML subset, so the
// JSON sub-document parses with the same decoder, no new code path).
//
// An empty or whitespace-only label is a clean no-op returning nil units and
// nil error, matching ImageMetadataLabel's "" sentinel for an unlabeled
// image. An array element with no customizations.aileron.cli contributes no
// unit. Malformed JSON, or a present cli block that fails canonical
// validation, is an error: a present-but-broken unit fails loudly rather
// than silently shipping nothing.
func UnitsFromMetadata(metadata []byte) ([]cli.Unit, error) {
	if len(metadata) == 0 {
		return nil, nil
	}

	var elements []featureMetadata
	if err := json.Unmarshal(metadata, &elements); err != nil {
		return nil, fmt.Errorf("unitloader: parse devcontainer.metadata array: %w", err)
	}

	var units []cli.Unit
	for i := range elements {
		raw := elements[i].Customizations.Aileron.CLI
		if len(raw) == 0 {
			continue
		}
		// Re-marshal the JSON sub-document so the canonical parser receives
		// the exact block bytes. cli.Parse decodes via yaml.v3 and YAML is a
		// JSON superset, so the JSON re-marshal parses with no new decoder.
		// Routing through a generic value preserves the field shape the
		// parser expects without coupling to JSON-vs-YAML quirks, mirroring
		// the gh Feature acceptance test.
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return nil, fmt.Errorf("unitloader: decode customizations.aileron.cli at element %d: %w", i, err)
		}
		yamlBytes, err := yaml.Marshal(generic)
		if err != nil {
			return nil, fmt.Errorf("unitloader: re-marshal cli block at element %d: %w", i, err)
		}
		unit, err := cli.Parse(yamlBytes)
		if err != nil {
			return nil, fmt.Errorf("unitloader: parse cli unit at element %d: %w", i, err)
		}
		units = append(units, unit)
	}
	return units, nil
}

// CaptureLayer projects units into the capture-descriptor layer fed to the
// capture registry between the embedded defaults and the user layer. It
// calls the canonical internal/cli.(*Unit).ToCaptureDescriptor adapter on
// each unit and aggregates the results in order. A unit that fails
// conversion (a malformed key) surfaces the error rather than dropping a
// descriptor. Nil units yields nil descriptors.
func CaptureLayer(units []cli.Unit) ([]capture.CaptureDescriptor, error) {
	if len(units) == 0 {
		return nil, nil
	}
	out := make([]capture.CaptureDescriptor, 0, len(units))
	for i := range units {
		desc, err := units[i].ToCaptureDescriptor()
		if err != nil {
			return nil, fmt.Errorf("unitloader: project capture descriptor for unit %q: %w", units[i].Name, err)
		}
		out = append(out, desc)
	}
	return out, nil
}

// SealingLayer projects units into the proxybinding-entry layer fed to the
// host binding table between the embedded defaults and the user layer. It
// calls the canonical internal/cli.(*Unit).ToSealingEntries adapter on each
// unit and aggregates the results in order. A unit that fails conversion (a
// re-declared credential_ref) surfaces the error rather than dropping a
// binding. Nil units yields nil entries.
func SealingLayer(units []cli.Unit) ([]proxybinding.Entry, error) {
	if len(units) == 0 {
		return nil, nil
	}
	var out []proxybinding.Entry
	for i := range units {
		entries, err := units[i].ToSealingEntries()
		if err != nil {
			return nil, fmt.Errorf("unitloader: project sealing entries for unit %q: %w", units[i].Name, err)
		}
		out = append(out, entries...)
	}
	return out, nil
}

// LayersFromImage is the one-call convenience both consumers use: it reads
// the resolved image's devcontainer.metadata label through container's
// ImageMetadataLabel, parses the units, and projects both layers. It wires
// Unit 1's reader to the projection so a caller threads in a runtime + image
// and receives the two additive layers.
//
// An image whose label read returns "" (unlabeled, absent locally, or an
// inspect failure) is a clean no-op: nil layers, nil error, preserving
// today's defaults-only behavior. A present-but-malformed label is a loud
// error so a broken unit fails construction rather than silently shipping
// nothing. A nil runner degrades to the production exec runner via
// ImageMetadataLabel.
func LayersFromImage(ctx context.Context, runner sandboxcontainer.Runner, runtimeName, image string) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
	metadata := sandboxcontainer.ImageMetadataLabel(ctx, runner, runtimeName, image)
	if metadata == "" {
		return nil, nil, nil
	}
	units, err := UnitsFromMetadata([]byte(metadata))
	if err != nil {
		return nil, nil, err
	}
	captureLayer, err := CaptureLayer(units)
	if err != nil {
		return nil, nil, err
	}
	sealingLayer, err := SealingLayer(units)
	if err != nil {
		return nil, nil, err
	}
	return captureLayer, sealingLayer, nil
}
