// Package composition defines the sandbox image-composition contract.
package composition

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultDevcontainerPath is the canonical project-local devcontainer file.
	DefaultDevcontainerPath = ".devcontainer/devcontainer.json"
	// DefaultDockerfilePath is the conventional Dockerfile path Discover assumes
	// for a hand-authored Tier 1 devcontainer that declares a build without
	// naming a dockerfile. `sandbox init` no longer scaffolds this file.
	DefaultDockerfilePath = ".devcontainer/Dockerfile"
	// DefaultBaseImageRepository is the image Aileron owns for Tier 0 and Tier 1.
	// It is published to GHCR by the sandbox-base CI workflow and mirrors the
	// DefaultFeatureRepository prefix style.
	DefaultBaseImageRepository = "ghcr.io/alrubinger/aileron-sandbox-base"
	// DefaultFeatureRepository is the registry path that hosts Aileron's
	// per-agent devcontainer Features. The scaffold composes one of these
	// Features (see FeatureReference) into the starter devcontainer.json, and
	// the same Features are baked into the prebuilt per-agent images (#965).
	DefaultFeatureRepository = "ghcr.io/alrubinger/aileron-features"
)

// Tier describes how a sandbox image should be composed.
type Tier string

const (
	TierBase         Tier = "base"
	TierDevcontainer Tier = "devcontainer"
	TierBYOImage     Tier = "byo_image"
)

// Plan is the normalized sandbox-composition plan consumed by launch/runtime
// code in later sandbox launch/runtime work.
type Plan struct {
	Tier             Tier
	BaseImage        string
	Image            string
	DevcontainerPath string
	DockerfilePath   string
	BuildArgs        map[string]string
	Mounts           []Mount
	Aileron          AileronCustomization
	// Features is the parsed devcontainer `features` block, keyed by Feature
	// reference. The value is the raw option payload (`{}`, `true`, a string,
	// or an object) carried verbatim — Aileron does not interpret Feature
	// options, it forwards them to @devcontainers/cli. Nil when the
	// devcontainer.json declares no features. A non-empty Features map on a
	// Tier 1 (devcontainer) plan routes the build through @devcontainers/cli
	// rather than raw `docker build`; on a Tier 2 (BYO image) plan it is inert
	// (carried only for plan inspection).
	Features map[string]json.RawMessage
}

// Mount is the subset of devcontainer mount configuration Aileron needs to
// preserve in the launch plan. For this first slice, string mounts are carried
// verbatim and object mounts are normalized when possible.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
	Raw      string
}

// AileronCustomization is the customizations.aileron extension block.
type AileronCustomization struct {
	Image           string `json:"image,omitempty"`
	Mediation       string `json:"mediation,omitempty"`
	ApprovalSurface string `json:"approval_surface,omitempty"`
}

type devcontainerConfig struct {
	Image          string                     `json:"image,omitempty"`
	Dockerfile     string                     `json:"dockerFile,omitempty"`
	Build          *devcontainerBuild         `json:"build,omitempty"`
	Mounts         []json.RawMessage          `json:"mounts,omitempty"`
	Features       map[string]json.RawMessage `json:"features,omitempty"`
	Customizations devcontainerCustomizations `json:"customizations,omitempty"`
}

type devcontainerBuild struct {
	Dockerfile string            `json:"dockerfile,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
}

type devcontainerCustomizations struct {
	Aileron AileronCustomization `json:"aileron,omitempty"`
}

// Discover returns the sandbox image-composition plan for workDir.
func Discover(workDir, version string) (Plan, error) {
	if workDir == "" {
		workDir = "."
	}
	baseImage := BaseImage(version)
	devcontainerPath := filepath.Join(workDir, DefaultDevcontainerPath)
	b, err := os.ReadFile(devcontainerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Plan{Tier: TierBase, BaseImage: baseImage, Image: baseImage}, nil
		}
		return Plan{}, fmt.Errorf("read %s: %w", devcontainerPath, err)
	}

	cfg, err := parseDevcontainer(b)
	if err != nil {
		return Plan{}, fmt.Errorf("parse %s: %w", devcontainerPath, err)
	}
	plan := Plan{
		Tier:             TierDevcontainer,
		BaseImage:        baseImage,
		Image:            baseImage,
		DevcontainerPath: DefaultDevcontainerPath,
		DockerfilePath:   resolveDockerfilePath(cfg),
		BuildArgs:        map[string]string{},
		Aileron:          cfg.Customizations.Aileron,
	}
	if cfg.Build != nil {
		for k, v := range cfg.Build.Args {
			plan.BuildArgs[k] = v
		}
	}
	if len(plan.BuildArgs) == 0 {
		plan.BuildArgs = nil
	}
	for _, raw := range cfg.Mounts {
		mount, err := parseMount(raw)
		if err != nil {
			return Plan{}, err
		}
		plan.Mounts = append(plan.Mounts, mount)
	}
	if len(cfg.Features) > 0 {
		plan.Features = make(map[string]json.RawMessage, len(cfg.Features))
		for ref, opts := range cfg.Features {
			// Preserve the raw option payload verbatim. Aileron does not
			// interpret Feature options; @devcontainers/cli does.
			plan.Features[ref] = append(json.RawMessage(nil), opts...)
		}
	}
	if cfg.Customizations.Aileron.Image != "" {
		plan.Tier = TierBYOImage
		plan.Image = cfg.Customizations.Aileron.Image
		plan.DockerfilePath = ""
		// Features are carried for plan inspection but inert on a BYO image.
		return plan, nil
	}
	if cfg.Image != "" {
		plan.Image = cfg.Image
	}
	if plan.DockerfilePath == "" {
		plan.DockerfilePath = DefaultDockerfilePath
	}
	return plan, nil
}

func parseDevcontainer(b []byte) (devcontainerConfig, error) {
	var cfg devcontainerConfig
	stripped, err := stripJSONComments(b)
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(bytes.NewReader(stripped))
	if err := dec.Decode(&cfg); err != nil {
		// The devcontainer `features` block must be a JSON object keyed by
		// Feature reference. A non-object (e.g. an array) is a contract error;
		// name the field so the message is actionable rather than a bare
		// "cannot unmarshal array into Go struct field".
		if strings.Contains(err.Error(), "field devcontainerConfig.features") {
			return cfg, fmt.Errorf("devcontainer features must be a JSON object keyed by feature reference: %w", err)
		}
		return cfg, err
	}
	if cfg.Customizations.Aileron.ApprovalSurface != "" {
		switch cfg.Customizations.Aileron.ApprovalSurface {
		case "webapp", "tui", "both":
		default:
			return cfg, fmt.Errorf("customizations.aileron.approval_surface must be webapp, tui, or both")
		}
	}
	return cfg, nil
}

// BaseImage returns the Aileron-owned sandbox-base image for version.
func BaseImage(version string) string {
	tag := strings.TrimSpace(version)
	if tag == "" || tag == "dev" {
		tag = "latest"
	}
	return DefaultBaseImageRepository + ":" + tag
}

func resolveDockerfilePath(cfg devcontainerConfig) string {
	if cfg.Build != nil && cfg.Build.Dockerfile != "" {
		return cfg.Build.Dockerfile
	}
	if cfg.Dockerfile != "" {
		return cfg.Dockerfile
	}
	return ""
}

func parseMount(raw json.RawMessage) (Mount, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return Mount{Raw: s}, nil
	}
	var obj struct {
		Source   string `json:"source"`
		Target   string `json:"target"`
		ReadOnly bool   `json:"readonly"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Mount{}, fmt.Errorf("parse devcontainer mount: %w", err)
	}
	return Mount{Source: obj.Source, Target: obj.Target, ReadOnly: obj.ReadOnly}, nil
}

func stripJSONComments(in []byte) ([]byte, error) {
	out := make([]byte, 0, len(in))
	inString := false
	escaped := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(in) {
			switch in[i+1] {
			case '/':
				i += 2
				for i < len(in) && in[i] != '\n' && in[i] != '\r' {
					i++
				}
				if i < len(in) {
					out = append(out, in[i])
				}
				continue
			case '*':
				i += 2
				closed := false
				for i+1 < len(in) {
					if in[i] == '*' && in[i+1] == '/' {
						closed = true
						i++
						break
					}
					if in[i] == '\n' || in[i] == '\r' {
						out = append(out, in[i])
					}
					i++
				}
				if !closed {
					return nil, fmt.Errorf("unterminated block comment")
				}
				continue
			}
		}
		out = append(out, c)
	}
	return out, nil
}
