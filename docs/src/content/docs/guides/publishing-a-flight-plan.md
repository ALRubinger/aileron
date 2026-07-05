---
title: "Publishing a Flight Plan"
description: "How to push a frozen Flight Plan's composed image and signed artifact to an OCI registry with aileron skill publish, so another operator can install and launch it without re-freezing."
---

This guide covers `aileron skill publish`: sharing a **frozen Flight Plan** across machines by pushing its image and signed artifact to an OCI registry. It is distinct from the two other publish flows in Aileron. [Publishing a Connector](/guides/publishing-a-connector) releases a sandboxed connector binary and its action templates. [Publishing to the Hub](/guides/publishing-to-the-hub) lists a connector in the discovery index. This guide is about a **frozen plan**: the sealed, digest-pinned, signed unit that `aileron skill freeze` produces.

Freezing stays local and offline. Publishing is the separate, explicit act that touches the network. You freeze once, then publish when you want a second operator to run the exact same plan as themselves.

## What publish pushes

A published plan travels as one OCI reference. Publish pushes two things to the destination repository:

1. The plan's **image** (the composed-tools image, or the base image for an image-only plan).
2. The **signed artifact** (`SKILL.md` + `aileron.lock` + `signature.sig` + `signing-key.pub`), attached as an OCI referrers manifest whose subject is that image.

A consumer follows the single reference, verifies the signature and the publisher, pulls the image by its attested digest, and launches. The ed25519 signature over the artifact remains the root of trust. Publish is a pass-through push of the already-signed bytes; it never re-signs.

## Prerequisites

- A frozen version in your store (`aileron skill freeze <name>` first; see `aileron skill list`).
- Push access to the destination OCI repository. Publish uses your existing Docker/OCI credentials (the ones `docker login` writes), so authenticate to the registry the usual way before publishing.
- A working Docker daemon. A composed-tools plan is pushed straight from your local image with `docker push`, so no special image-store configuration is required.

## Publishing

```sh
aileron skill publish <name> --registry <ref> [--version <id>]
```

- `<name>` is the installed skill name.
- `--registry` is the destination OCI repository, for example `ghcr.io/acme/my-plan`.
- `--version` pins a specific frozen version id. Omit it to publish the newest.

The published artifact is tagged at the version id, so the shareable reference is `<registry>:<version>`. That is the reference a second operator installs.

## Two binding kinds

The digest that lets launch verify the pulled image against the signed lock depends on the plan's pin type. Publish branches automatically.

- **Composed-tools plans.** The image is built locally at freeze and pinned by its config digest. Publish verifies the local image's config digest against the signed lock, then pushes it with `docker push`, and fails closed on any mismatch before the image leaves your machine. This is the `config-digest` binding.
- **Image-only or custom-base plans.** The signed lock pins the base image's registry manifest digest. Publish copies the exact bytes from the source registry into your destination registry, preserving that manifest digest. It does not route these through a local re-export, because re-encoding would change the manifest digest and break verification. This is the `manifest-digest` binding.

The binding kind is derived from the signed lock, not from any registry annotation, so a tampered annotation cannot change what launch verifies.

## Idempotent by design

Re-publishing the same frozen version pushes identical content and produces identical digests. The artifact manifest is packed with a fixed `created` timestamp and content-addressed layers, so a second publish of the same version is a no-op rather than a divergent write.

## Trust and identity, on the consumer side

Publish carries no credential. The signed artifact holds the plan and its publisher attribution; it never holds secrets. When a second operator installs and launches your plan, they trust your publisher key through the keyring (`aileron keyring trust <publisher>`, the same keyring connectors use), wire their own vault credential, and run as themselves. The audit record attributes the launching operator, not you.

Installing and launching a published plan on another machine is covered by `aileron skill install <oci-ref>` and `aileron skill launch`.

## Failure modes

Publish returns a precise error and pushes nothing when:

- the registry rejects the push because you are not authenticated,
- the named skill has no frozen version,
- a composed plan's local image is missing, or
- a composed image's pushed config digest does not match the signed lock (a tampered or mislabeled image).
