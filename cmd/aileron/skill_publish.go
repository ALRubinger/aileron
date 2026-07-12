package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/publish"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// publishRun is a seam so tests can exercise the publish command's flag
// parsing, version resolution, and error mapping without a live registry.
var publishRun = publish.Run

// runSkillPublish implements `aileron skill publish <name> [--version <id>]
// --registry <ref>`: push a frozen version's composed (or base) image and its
// signed artifact to an OCI registry so another operator can install and launch
// it without re-freezing (umbrella #1898). Freeze stays local; publish is the
// explicit network-touching act. Registry auth uses the operator's existing
// Docker/OCI credentials.
func runSkillPublish(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("skill publish", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("version", "", "frozen version id to publish (default: newest)")
	registryRef := flags.String("registry", "", "destination OCI repository, e.g. ghcr.io/acme/plan")
	quiet := flags.Bool("quiet", false, "Suppress the live push progress feedback (the publish summary still prints)")
	positionals, err := parseInterspersedFlags(flags, args)
	if err != nil {
		return 1
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, skillUsage)
		return 1
	}
	name := positionals[0]
	if *registryRef == "" {
		fmt.Fprintln(stderr, "error: --registry <ref> is required")
		return 1
	}

	s := store.New(skillStoreDir)
	id, count, err := resolveFrozenVersion(s, name, *version)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if *version == "" && count > 1 {
		fmt.Fprintf(stdout, "publishing %s (newest of %d; use --version to pin)\n", id, count)
	}

	fv, err := s.ReadFrozen(name, id)
	if err != nil {
		fmt.Fprintf(stderr, "error: read frozen version %q: %v\n", id, err)
		return 1
	}
	lock, err := freeze.ParseLockfile(fv.Lockfile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	_, err = publishRun(context.Background(), publish.Options{
		Name:      name,
		VersionID: id,
		Registry:  *registryRef,
		Frozen:    fv,
		Lock:      lock,
		Stdout:    stdout,
		Stderr:    stderr,
		Quiet:     *quiet,
	})
	if err != nil {
		switch {
		case errors.Is(err, publish.ErrNoImage):
			fmt.Fprintf(stderr, "error: %v (nothing to publish for an instruction-only plan)\n", err)
		case errors.Is(err, publish.ErrConfigContentDigestMismatch):
			fmt.Fprintf(stderr, "error: %v\n", err)
		default:
			fmt.Fprintf(stderr, "error: publish %q: %v\n", name, err)
		}
		return 1
	}

	// Record the install origin sidecar so a later `skill launch` of this
	// just-published version takes the registry pull-and-verify boot path
	// (internal/flightplan/runtime/image.go), not the mutable local-tag path.
	// The composed local tag `aileron/sandbox-tools:<hex>` is content-derived
	// purely from base+featureRefs and is a SHARED daemon tag any later freeze of
	// any plan with the same environment rebuilds/repoints, which would leave this
	// plan's signed lock attesting a stale digest and trip the #1863
	// config-content-digest guard. The published image is content-addressed and
	// verified against the same signed lock pin, so pointing launch at the
	// registry resolves the dead-end. This mirrors the OCI-install path
	// (cmd/aileron/skill_install_oci.go): the sidecar is non-signed provenance
	// (where to fetch), never a trust anchor; every fetched byte is still verified
	// against the signed lock. A sidecar write failure fails the publish rather
	// than leaving a version whose launch silently stays on the local-tag path.
	if err := s.WriteOrigin(name, id, store.Origin{
		Registry:   *registryRef,
		VersionTag: id,
	}); err != nil {
		fmt.Fprintf(stderr, "error: record publish origin for %q: %v\n", id, err)
		return 1
	}

	// A plan frozen with a private --signing-key gates `skill launch` on that
	// key, which is NOT the repo's committed keys/publisher.pub. `keyring trust
	// <publisher>` fetches the committed key and no-ops when an owner grant
	// already exists, so it never unblocks self-launch. Print the exact working
	// command (#2136) so the author can trust the plan's OWN signing key. This is
	// an opt-in prompt, never a silent auto-register: the operator still runs it
	// to keep the trust decision explicit.
	if lock.Publisher != "" {
		fmt.Fprintf(stdout, "\nTo launch this plan yourself, trust its signing key:\n")
		fmt.Fprintf(stdout, "  aileron keyring trust --plan %s\n", name)
	}
	return 0
}
