package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"

	"github.com/ALRubinger/aileron/internal/auth/capture"
	"github.com/ALRubinger/aileron/internal/cli/unitloader"
	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/version"
)

// The metadata Type stamped on the user/github vault entry ("user") is
// no longer a constant in core: it is the descriptor's `kind` field,
// supplied to the store closure as the kind argument. It must equal the
// first segment of the host-binding credential-ref (`user/github`) so the
// daemon's VaultResolver kind check passes when a github.com /
// api.github.com request is sealed at the TLS boundary (ADR-0019, #1195).
// That coupling now lives in the shipped gh descriptor (store_at:
// user/github, kind: user), not in compiled Go.

// userCredentialsBody is the wire shape the user-credential PUT
// marshals: the secret bytes plus optional non-secret metadata. It is a
// local subset of api.AgentCredentials (cmd/aileron does not depend on
// internal/api/gen, matching the agentCredentialsBody precedent).
type userCredentialsBody struct {
	Value    []byte                   `json:"value"`
	Metadata *userCredentialsMetadata `json:"metadata,omitempty"`
}

// userCredentialsMetadata is the local subset of
// api.AgentCredentialsMetadata the user-credential PUT sets. Only Type
// is carried today; the daemon binding resolver keys on it.
type userCredentialsMetadata struct {
	Type string `json:"type,omitempty"`
}

// resolveDeviceFlowImage returns the container image for the capture
// flow: the caller's --image override when set, otherwise the sandbox
// base image. The base image ships gh (#1146) and is agent-independent,
// which matches the user-level (not per-agent) nature of this
// credential. edge for dev builds, latest for releases (#1141). Image /
// base-image resolution policy stays caller-side; the capture package
// takes a fully-resolved image string.
func resolveDeviceFlowImage(override string) string {
	if override != "" {
		return override
	}
	return sandboxcomposition.BaseImage(version.Version)
}

// userCredentialStore returns the production capture.StoreFunc: it
// marshals the captured bytes plus the descriptor-supplied kind stamp
// and PUTs them to the daemon vault at /vault/<storeAt>/credentials. The
// HTTP status → message mapping stays caller-side (internal cannot import
// the cmd-side vault transport), so the closure translates the daemon's
// status codes into the sentinel errors runAuthCapture maps to messages.
//
// storeAt is the descriptor's logical vault namespace (e.g. "user/github");
// the full route is derived here, not re-derived inside the driver.
func userCredentialStore(ctx context.Context, storeAt, kind string, value []byte) error {
	// Marshaling a struct with a []byte field and a string field cannot
	// fail, so the error is discarded (matching runAuthImport's precedent).
	body, _ := json.Marshal(userCredentialsBody{
		Value:    value,
		Metadata: &userCredentialsMetadata{Type: kind},
	})
	status, respBody, err := vaultDoRequest(http.MethodPut,
		"/vault/"+storeAt+"/credentials", body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent:
		return nil
	case http.StatusLocked:
		return errVaultLocked
	case http.StatusServiceUnavailable:
		return errNoVault
	default:
		return fmt.Errorf("server returned %d: %s", status, string(respBody))
	}
}

// errVaultLocked and errNoVault are the sentinel store errors the
// production StoreFunc returns for the daemon's 423 / 503 statuses so
// runAuthCapture can map them to the same operator-facing messages the
// bespoke flow used. Any other non-204 status surfaces verbatim via the
// store closure's default branch.
var (
	errVaultLocked = errors.New("vault is locked; unlock it first")
	errNoVault     = errors.New("daemon is not configured with a vault")
)

// captureVerbDescriptor maps a user-facing `aileron auth <verb>` to the
// capture descriptor name that drives it. The verb is the CLI surface
// spelling (e.g. "github"); the descriptor is the registry key the shipped
// YAML declares (e.g. "gh"). This is the only caller-side coupling between
// a verb and a tool: it is a spelling alias, not provider knowledge — the
// login/token commands, image, vault path, and kind all live in the
// descriptor YAML, never here. A verb whose spelling already equals its
// descriptor name needs no entry.
var captureVerbDescriptor = map[string]string{
	"github": "gh",
}

// descriptorForVerb resolves a `auth <verb>` to its descriptor name. When
// the verb has no alias entry it is used as the descriptor name directly,
// so a future tool whose verb equals its descriptor name needs no wiring.
func descriptorForVerb(verb string) string {
	if name, ok := captureVerbDescriptor[verb]; ok {
		return name
	}
	return verb
}

// captureRegistry constructs the capture descriptor registry the dispatch
// predicate reads. As of #1323 gh no longer ships as a central embedded
// default; its capture descriptor comes from the sandbox base image's gh CLI
// Feature unit (the image inspected for acquisition is the base image, which
// is where `aileron auth github` runs gh). So the dispatch registry is built
// off the default device-flow image's unit layer additively over the embedded
// defaults and the user layer, exactly like the driver builder
// (imageCaptureRegistry). Resolving the default image keeps the dispatch and
// the driver reading the same descriptor source.
//
// It is a package var so a test can force the registry-load-error branch: a
// registry that cannot load must fail closed (no crash, no mis-dispatch)
// rather than treating an unresolvable verb as a capture descriptor.
var captureRegistry = func() (*capture.Registry, error) {
	return imageCaptureRegistry(sandboxcontainer.DefaultRuntime, resolveDeviceFlowImage(""))
}

// isCaptureDescriptor reports whether name resolves to a capture
// descriptor in the embedded + user registry. runAuth uses it to dispatch
// a descriptor verb (e.g. "github" → the "gh" tool) before falling through
// to the <agent> --import-from-host path, so no provider knowledge is
// compiled into core: the set of acquisition verbs is exactly the
// registered descriptors. A registry load error returns false (and the
// caller falls through), matching the fail-closed posture — a malformed
// descriptor must not silently shadow an agent name.
//
// It is a package var so tests can exercise the dispatch without depending
// on the shipped descriptor set.
var isCaptureDescriptor = func(name string) bool {
	registry, err := captureRegistry()
	if err != nil {
		return false
	}
	_, ok := registry.Resolve(name)
	return ok
}

// imageCaptureLayers resolves the image-derived CLI-unit capture layer
// (#1322) for the resolved acquisition image. Each CLI Feature in the image
// carries its acquisition as data in the image's devcontainer.metadata label;
// this fans those units into a capture-descriptor layer additive to the
// embedded defaults (built-in < unit-derived < user). An image whose label
// cannot be read (absent locally, no label) is a clean no-op returning a nil
// layer, preserving the embedded-defaults-only registry. A present-but-
// malformed unit is a loud error so a broken Feature fails the command rather
// than silently shipping nothing. It is a package var so a test substitutes a
// fake without a container runtime.
var imageCaptureLayers = func(runtime, image string) ([]capture.CaptureDescriptor, error) {
	captureLayer, _, err := unitloader.LayersFromImage(
		context.Background(), sandboxcontainer.DefaultRunner(), runtime, image)
	return captureLayer, err
}

// imageCaptureRegistry builds a capture registry that additively includes the
// image-derived unit layer for the resolved acquisition image, on top of the
// embedded defaults and the standard user layer. It is the production driver
// constructor's registry: the unit layer is read for the same image the
// driver will run against. It is a package var so tests substitute a fake.
var imageCaptureRegistry = func(runtime, image string) (*capture.Registry, error) {
	extra, err := imageCaptureLayers(runtime, image)
	if err != nil {
		return nil, err
	}
	opts := capture.DefaultCaptureLoadOptions()
	opts.ExtraDescriptors = extra
	return capture.NewRegistry(opts)
}

// newCaptureDriver builds a ready capture.Driver for the named descriptor.
// It is a package var so tests substitute a fake container runner without
// driving a real container or reaching the tool's login backend; the
// production implementation resolves the runtime, the image, and the
// descriptor from the embedded + user registry plus the image-derived unit
// layer (#1322) for the resolved image, then binds the store seam.
var newCaptureDriver = func(descriptorName, runtime, image string, store capture.StoreFunc) (*capture.Driver, error) {
	registry, err := imageCaptureRegistry(runtime, image)
	if err != nil {
		return nil, err
	}
	return registry.Bind(descriptorName, runtime, image, store)
}

// runAuthCapture implements a descriptor-driven acquisition verb such as
// `aileron auth github`. The descriptor (resolved by name from the
// capture registry) supplies the container image base, the login and
// token-read commands, the optional BROWSER shim, the vault path, and the
// kind stamp; this caller resolves the --runtime / --image flags and the
// image, wires the daemon vault transport as the store seam, and maps the
// driver's error back to the operator-facing messages and exit codes the
// bespoke flow used.
func runAuthCapture(descriptorName string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("auth "+descriptorName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtime := flags.String("runtime", sandboxcontainer.DefaultRuntime,
		"Container runtime: auto or docker")
	image := flags.String("image", "",
		"Override the container image (defaults to the gh-bearing sandbox base image)")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "usage: aileron auth %s [--runtime <auto|docker>] [--image <ref>]\n", descriptorName)
		return 1
	}

	driver, err := newCaptureDriver(descriptorName, *runtime, resolveDeviceFlowImage(*image), userCredentialStore)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	switch err := driver.Acquire(context.Background()); {
	case err == nil:
		fmt.Fprintln(stdout, "Stored "+driver.StoreAt)
		return 0
	case errors.Is(err, capture.ErrEmptyToken):
		fmt.Fprintln(stderr, "error: login returned an empty token; login did not complete")
		return 1
	case errors.Is(err, errVaultLocked):
		fmt.Fprintln(stderr, "error: vault is locked; unlock it first")
		return 1
	case errors.Is(err, errNoVault):
		fmt.Fprintln(stderr, "error: daemon is not configured with a vault")
		return 1
	default:
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
}
