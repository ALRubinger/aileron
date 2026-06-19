// This file defines the declarative capture-descriptor format: the
// "config, not code" half of credential acquisition. The capture.Driver
// (capture.go) is the tool-agnostic "code" half — it drives an
// interactive CLI login inside a container and reads the token back out,
// with every tool-specific value a field. This file supplies the missing
// declarative format so a CLI's acquisition knowledge (the login arg
// vector, the token-read arg vector, the optional config-dir env and
// BROWSER shim, the vault path, and the kind stamp) ships as YAML data
// rather than bespoke Go. `gh` is the one built-in descriptor; adding a
// second tool is a new YAML under defaults/, never new Go.
//
// The format is the capture-scoped sibling of internal/proxybinding's
// host->credential binding descriptor. It deliberately uses distinct
// identifiers (CaptureDescriptor, ParseCaptureDescriptor,
// CaptureSchemaVersion) so the two formats never blur together, even
// though they live in different packages. This package neither imports
// nor depends on proxybinding.
package capture

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// CaptureSchemaVersion is the only capture-descriptor schema version this
// loader understands. The format is versioned so it can evolve under
// 0.0.x without a silent misparse: a descriptor that names any other
// version is a load-time error rather than a best-effort decode against
// the wrong field set.
const CaptureSchemaVersion = "v1"

// CaptureDescriptor is a parsed, versioned capture-descriptor document.
// One document declares a single tool's credential-acquisition knowledge,
// keyed by Name. A CLI vendor or community profile ships a
// CaptureDescriptor; the loader merges the built-in and user layers into a
// single validated set keyed on Name.
//
// Every field is non-secret data. The descriptor carries no credential
// bytes: it names where the captured token is stored (StoreAt) and the
// kind stamp, but the actual capture, the resolved image, and the vault
// transport are supplied by the caller via [CaptureDescriptor.Apply].
type CaptureDescriptor struct {
	// Version is the schema version. It must equal [CaptureSchemaVersion];
	// any other value is rejected at parse time so the format can evolve
	// under 0.0.x without a silent misparse.
	Version string `yaml:"version"`

	// Name is the registry key, unique within a single layer. The registry
	// resolves a descriptor by this name (e.g. "gh"); across layers a later
	// layer's descriptor with the same Name overrides an earlier one.
	Name string `yaml:"name"`

	// Image is an optional pre-resolved container image. It is normally
	// empty: image / base-image resolution policy stays caller-side, and
	// [CaptureDescriptor.Apply] takes a resolved image string from the
	// caller. A non-empty value here is a descriptor-supplied default the
	// caller may override.
	Image string `yaml:"image"`

	// ContainerName is the deterministic name for the long-lived capture
	// container (e.g. "aileron-auth-github"). Required.
	ContainerName string `yaml:"container_name"`

	// LoginCmd is the interactive login command run inside the container,
	// minus the exec scaffolding — e.g.
	// {"gh","auth","login","--web"}. The driver prepends the exec/-i/-t/
	// --env tokens and the container name. Required and non-empty; maps to
	// Driver.LoginArgs.
	LoginCmd []string `yaml:"login_cmd"`

	// TokenCmd is the token-read command run inside the same container,
	// e.g. {"gh","auth","token"}. Required and non-empty; maps to
	// Driver.TokenArgs.
	TokenCmd []string `yaml:"token_cmd"`

	// BrowserShim, when non-empty, is passed as `--env=BROWSER=<shim>` on
	// the login exec only (e.g. "echo"), to turn a missing-browser open
	// into a clean no-op. Optional; maps to Driver.BrowserShim.
	BrowserShim string `yaml:"browser_shim"`

	// ConfigDir, when non-empty, is a single `K=V` env token (e.g.
	// "GH_CONFIG_DIR=/path") passed on BOTH execs so the token read sees
	// the same config home the login wrote to. Optional; omitted/empty when
	// unset; maps to Driver.ConfigDirEnv. `gh` leaves this empty (it writes
	// the default ~/.config/gh/hosts.yml), so setting it would diverge the
	// exec arg vectors from the bespoke flow.
	ConfigDir string `yaml:"config_dir"`

	// StoreAt is the vault path the captured credential is stored at (e.g.
	// "user/github"). Required; maps to Driver.StoreAt.
	StoreAt string `yaml:"store_at"`

	// Kind is the metadata Type stamped on the stored credential (e.g.
	// "user"). Required; maps to Driver.Kind.
	Kind string `yaml:"kind"`
}

// ParseCaptureDescriptor strictly decodes a single capture-descriptor
// document and validates it. Decoding is strict: an unknown YAML key is an
// error rather than a silently ignored field, so a typo in a descriptor
// fails fast instead of shipping an acquisition flow that does nothing.
// Malformed YAML, a wrong or missing version, and any field that fails
// [CaptureDescriptor.Validate] are all errors.
//
// ParseCaptureDescriptor never reads secret bytes; a descriptor carries
// only non-secret acquisition knowledge.
func ParseCaptureDescriptor(data []byte) (CaptureDescriptor, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var d CaptureDescriptor
	if err := dec.Decode(&d); err != nil {
		return CaptureDescriptor{}, fmt.Errorf("capture: parse descriptor: %w", err)
	}

	if d.Version != CaptureSchemaVersion {
		return CaptureDescriptor{}, fmt.Errorf("capture: unsupported descriptor version %q (want %q)", d.Version, CaptureSchemaVersion)
	}

	if err := d.Validate(); err != nil {
		return CaptureDescriptor{}, fmt.Errorf("capture: descriptor %q: %w", d.Name, err)
	}

	return d, nil
}

// Validate checks that a CaptureDescriptor's required fields are present.
// It enforces the non-empty name/container_name/store_at/kind and the
// non-empty login_cmd/token_cmd lists. It does not resolve the image or
// touch any secret.
func (d *CaptureDescriptor) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("missing required field: name")
	}
	if d.ContainerName == "" {
		return fmt.Errorf("missing required field: container_name")
	}
	if len(d.LoginCmd) == 0 {
		return fmt.Errorf("missing required field: login_cmd (non-empty list)")
	}
	if len(d.TokenCmd) == 0 {
		return fmt.Errorf("missing required field: token_cmd (non-empty list)")
	}
	if d.StoreAt == "" {
		return fmt.Errorf("missing required field: store_at")
	}
	if d.Kind == "" {
		return fmt.Errorf("missing required field: kind")
	}
	return nil
}

// Apply maps the descriptor's fields onto a *Driver, binding the
// caller-resolved image and the StoreFunc the descriptor never owns. It is
// the adapter from declarative data to the executable driver: every
// tool-specific field (LoginArgs/TokenArgs/ConfigDirEnv/BrowserShim/
// StoreAt/Kind/ContainerName/Image) comes from the descriptor, while the
// transport (Store) and the resolved image come from the caller.
//
// image is the caller-resolved container image. When image is empty the
// descriptor's own Image is used (normally empty for gh). store is the
// vault PUT seam; it is required. Apply leaves the Driver's Runner and
// RuntimeExe untouched so a caller may build the Driver via New (which
// defaults them) and then Apply the descriptor on top.
func (d *CaptureDescriptor) Apply(drv *Driver, image string, store StoreFunc) {
	if image != "" {
		drv.Image = image
	} else if d.Image != "" {
		drv.Image = d.Image
	}
	drv.ContainerName = d.ContainerName
	drv.LoginArgs = append([]string(nil), d.LoginCmd...)
	drv.TokenArgs = append([]string(nil), d.TokenCmd...)
	drv.ConfigDirEnv = d.ConfigDir
	drv.BrowserShim = d.BrowserShim
	drv.StoreAt = d.StoreAt
	drv.Kind = d.Kind
	drv.Store = store
}
