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
	return 0
}
