package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/flightplan/pull"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// installPullTimeout bounds the registry pull so an unreachable or hung
// registry fails the install rather than blocking the CLI indefinitely.
const installPullTimeout = 5 * time.Minute

// skillPullRun is a seam so tests exercise the OCI install flow (flag parsing,
// error mapping, store write, list) with an in-memory oras target and no
// network, mirroring publishRun = publish.Run.
var skillPullRun = pull.Run

// newInstallPublisherVerifier builds the keyring-backed publisher-trust gate
// for an OCI install. It reuses the exact keyringPublisherVerifier seam launch
// uses (cmd/aileron/skill_launch_publisher.go), so install and launch resolve
// publisher trust identically (#1900) against the same keyring. It is a
// package-level seam so CLI tests point path at a temp keyring and capture the
// diag line. An install is always a host action (never the image-boot
// re-entry), so it always wires the real verifier; the pull path still skips
// the gate for a plan that declares no publisher.
var newInstallPublisherVerifier = func(diag io.Writer) pull.PublisherVerifier {
	return keyringPublisherVerifier{path: cstore.DefaultKeyringPath(), op: "install", diag: diag}
}

// runSkillInstallOCI installs a published Flight Plan by OCI reference: it pulls
// the signed artifact, verifies its signature + content hash and the publisher
// trust gate, and writes it into the local store as a frozen version
// indistinguishable from a local freeze (umbrella #1898, the read half). It
// never pulls the image subject; the pin in the lock is carried into the store
// untouched and resolved lazily at launch (#1903).
func runSkillInstallOCI(ref string, stdin io.Reader, stdout, stderr io.Writer) int {
	s := store.New(skillStoreDir)

	// Bound the network pull and honor Ctrl-C so a hung or unreachable registry
	// fails fast instead of blocking the CLI indefinitely.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, installPullTimeout)
	defer cancel()

	res, err := skillPullRun(ctx, pull.Options{
		Ref:      ref,
		Verifier: newInstallPublisherVerifier(stderr),
		Stdout:   stdout,
		Stderr:   stderr,
	})
	if err != nil {
		switch {
		case errors.Is(err, pull.ErrMissingVersionTag):
			fmt.Fprintf(stderr, "error: %v; install a tagged reference like ghcr.io/owner/plan:<version>\n", err)
		case errors.Is(err, pull.ErrNotAnArtifact):
			// This ref resolves to a manifest with no Flight Plan artifactType:
			// an image, or a GHCR `sha256-...` OCI referrers fallback tag. Point
			// the operator at the signed plan's version coordinate instead.
			fmt.Fprintf(stderr, "error: %v\n", err)
			fmt.Fprintf(stderr, "hint: %q resolves to an image or an OCI referrers fallback tag (sha256-...), not the signed plan; "+
				"install the plan's version tag, e.g. ghcr.io/owner/plan:<16-hex> (or omit the tag for :latest). "+
				"The exact coordinate is the `artifact:` line printed by `aileron skill publish`.\n", ref)
		case errors.Is(err, pull.ErrMissingArtifact):
			fmt.Fprintf(stderr, "error: %v (no published Flight Plan at %q)\n", err, ref)
		case errors.Is(err, pull.ErrNoName):
			fmt.Fprintf(stderr, "error: %v\n", err)
		default:
			// Signature failure, untrusted publisher, or an unreachable /
			// unauthenticated registry surface here with their own precise message.
			fmt.Fprintf(stderr, "error: install %q: %v\n", ref, err)
		}
		return 1
	}

	if err := s.WriteFrozen(res.Name, res.Frozen); err != nil {
		fmt.Fprintf(stderr, "error: write frozen version %q: %v\n", res.Frozen.ID, err)
		return 1
	}

	// Record the install origin so launch (#1903) can pull and verify the
	// published image on a machine that never froze the plan. The sidecar is
	// non-signed provenance (where to fetch), never a verification trust anchor;
	// its presence is also the signal launch uses to take the registry-pull path
	// instead of the local-tag boot path. A locally-frozen version has no
	// sidecar. A sidecar write failure fails the install rather than leaving a
	// version launch cannot boot on this machine.
	if err := s.WriteOrigin(res.Name, res.Frozen.ID, store.Origin{
		Registry:   res.SourceRegistry,
		VersionTag: res.SourceTag,
	}); err != nil {
		fmt.Fprintf(stderr, "error: record install origin for %q: %v\n", res.Frozen.ID, err)
		return 1
	}

	fmt.Fprintf(stdout, "Installed frozen version %s of skill %q to %s\n",
		res.Frozen.ID, res.Name, s.FrozenDir(res.Name, res.Frozen.ID))

	// A frozen/OCI install carries a signed trust contract, so bind can derive
	// this plan's credential requirements right now. When attached to a terminal,
	// offer to chain straight into bind so the operator onboards credentials in
	// one step instead of remembering a second command. Non-interactive installs
	// (CI, piped stdin) fall through to the pointer and never block on input.
	if isTTYFn() {
		// Wrap stdin once and hand the same *bufio.Reader to both the offer
		// prompt and bind: promptLine reuses an existing *bufio.Reader instead of
		// buffering ahead into a throwaway, so the bytes bind reads next (access
		// key ids, etc.) survive the first ReadString rather than being consumed.
		br := bufio.NewReader(stdin)
		answer := promptLine(br, stdout,
			fmt.Sprintf("Bind %q's declared credentials now? [Y/n]: ", res.Name))
		if !strings.EqualFold(answer, "n") && !strings.EqualFold(answer, "no") {
			// Bind exactly the version just written, not merely the most recent,
			// so a concurrent freeze cannot redirect the onboarding. Bind reuses
			// the same stdin so its own prompts read from the operator's terminal.
			return runSkillBind([]string{res.Name, "--version", res.Frozen.ID}, br, stdout, stderr)
		}
	}

	fmt.Fprintf(stdout, "Run `aileron skill bind %q` to supply this plan's credentials\n", res.Name)
	return 0
}
