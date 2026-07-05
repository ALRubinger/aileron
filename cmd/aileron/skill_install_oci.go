package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/flightplan/pull"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

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
	return keyringPublisherVerifier{path: cstore.DefaultKeyringPath(), diag: diag}
}

// runSkillInstallOCI installs a published Flight Plan by OCI reference: it pulls
// the signed artifact, verifies its signature + content hash and the publisher
// trust gate, and writes it into the local store as a frozen version
// indistinguishable from a local freeze (umbrella #1898, the read half). It
// never pulls the image subject; the pin in the lock is carried into the store
// untouched and resolved lazily at launch (#1903).
func runSkillInstallOCI(ref string, stdout, stderr io.Writer) int {
	s := store.New(skillStoreDir)

	res, err := skillPullRun(context.Background(), pull.Options{
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
			fmt.Fprintf(stderr, "error: %v\n", err)
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

	fmt.Fprintf(stdout, "Installed frozen version %s of skill %q to %s\n",
		res.Frozen.ID, res.Name, s.FrozenDir(res.Name, res.Frozen.ID))
	return 0
}
