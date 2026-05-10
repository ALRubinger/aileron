package wrap

// Spec is the in-memory representation the YAML loader and the
// --help parser produce. The emitter turns one Spec into a complete
// connector source tree.
//
// The YAML schema mirrors this struct field-for-field; see
// LoadYAML for the entry point and emit_test.go for canonical YAML
// shapes.
type Spec struct {
	// Connector is the identity block emitted as the manifest's
	// [connector] section.
	Connector ConnectorSpec `yaml:"connector"`

	// Program is the local binary the connector wraps. Path must be
	// absolute or `~/`-anchored. Hash is optional; when set, the
	// runtime pins the binary's bytes.
	Program ProgramSpec `yaml:"program"`

	// Subcommands is the list of operations the connector exposes.
	// Each subcommand becomes one action.md plus one entry in the
	// manifest's argv_patterns.
	Subcommands []SubcommandSpec `yaml:"subcommands"`

	// EnvPassthrough is the closed set of environment keys the
	// runtime is allowed to forward to the subprocess. Common
	// example: forwarding `GIT_AUTHOR_NAME` to a git wrapper.
	EnvPassthrough []string `yaml:"env_passthrough,omitempty"`

	// Credential, when set, declares an [capabilities.credential]
	// block. The runtime injects the resolved secret as the named
	// env keys in CredentialEnvKeys; the connector never sees the
	// bytes.
	Credential *CredentialSpec `yaml:"credential,omitempty"`

	// CredentialEnvKeys is the list of env keys the runtime injects
	// the resolved credential into when the connector spawns. Each
	// key must also appear in EnvPassthrough.
	CredentialEnvKeys []string `yaml:"credential_env_keys,omitempty"`

	// FSRead and FSWrite are the filesystem scopes the connector
	// requests. Paths must be absolute or `~/`-anchored.
	FSRead  []string `yaml:"fs_read,omitempty"`
	FSWrite []string `yaml:"fs_write,omitempty"`

	// Cwd is the optional cwd policy the runtime enforces.
	Cwd string `yaml:"cwd,omitempty"`
}

// ConnectorSpec is the [connector] block.
type ConnectorSpec struct {
	// Name is the connector's fully-qualified URI
	// (e.g. github://acme/gitcrawl).
	Name string `yaml:"name"`

	// Version is a strict SemVer string.
	Version string `yaml:"version"`

	// Publisher is the human-readable publisher shown in install
	// consent UIs.
	Publisher string `yaml:"publisher,omitempty"`
}

// ProgramSpec names the binary the connector spawns.
type ProgramSpec struct {
	// Path is the absolute (or `~/`-anchored) filesystem path of
	// the binary.
	Path string `yaml:"path"`

	// Hash is the optional sha256: content hash. When set, the
	// runtime verifies the binary's bytes on every call.
	Hash string `yaml:"hash,omitempty"`
}

// SubcommandSpec describes one operation the connector exposes. One
// SubcommandSpec produces one action.md and one entry in the
// manifest's argv_patterns.
type SubcommandSpec struct {
	// Name is the operation name (e.g. "log", "status").
	Name string `yaml:"name"`

	// Description is a one-line human-readable summary used in the
	// generated action.md and surfaced in the LLM's tool list.
	Description string `yaml:"description,omitempty"`

	// Argv is the argv pattern the runtime allows. Tokens are
	// whitespace-separated; `{name}` placeholders accept any value
	// for the matching parameter at call time.
	Argv string `yaml:"argv"`

	// Params is the list of parameters the action exposes to the
	// agent. Each param maps to a placeholder in Argv.
	Params []ParamSpec `yaml:"params,omitempty"`
}

// ParamSpec describes one input the action accepts.
type ParamSpec struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Description string `yaml:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty"`
}

// CredentialSpec maps to [capabilities.credential]. v1 supports
// api_key (the common case for CLI wrappers like gh, slackdump). The
// oauth2 kind is intentionally absent here; a connector that needs
// OAuth declares it directly in the manifest after generation.
type CredentialSpec struct {
	Kind  string `yaml:"kind"`
	Scope string `yaml:"scope,omitempty"`
}
