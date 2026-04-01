// Package registry provides a client for the official MCP Registry.
package registry

// RegistryResponse is the top-level response from the MCP Registry API.
type RegistryResponse struct {
	Servers []RegistryServer `json:"servers"`
}

// RegistryServer represents a single server entry from the MCP Registry.
type RegistryServer struct {
	// Canonical fields from the registry.
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Repository  *RepoInfo `json:"repository,omitempty"`
	VersionDetail
}

// VersionDetail holds the latest version information for a server.
type VersionDetail struct {
	Version  string    `json:"version"`
	Packages []Package `json:"packages,omitempty"`
	Remotes  []Remote  `json:"remotes,omitempty"`
}

// RepoInfo is the repository metadata for a server.
type RepoInfo struct {
	URL string `json:"url"`
}

// Remote represents a remote (HTTP/SSE) transport endpoint.
type Remote struct {
	TransportType string            `json:"transportType"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers,omitempty"`
}

// Package represents a distributable package for an MCP server.
type Package struct {
	RegistryType string        `json:"registryType"` // "npm", "pypi", "docker", etc.
	Name         string        `json:"name"`
	Version      string        `json:"version,omitempty"`
	Runtime      RuntimeConfig `json:"runtime"`
	EnvVars      []EnvVar      `json:"environmentVariables,omitempty"`
}

// RuntimeConfig describes how to launch a package.
type RuntimeConfig struct {
	Type    string   `json:"type"`              // "node", "python", "docker"
	Command string   `json:"command,omitempty"` // e.g. "npx"
	Args    []string `json:"args,omitempty"`
}

// EnvVar describes an environment variable required by an MCP server.
type EnvVar struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
}
