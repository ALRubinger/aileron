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
- A composed-tools plan built at freeze. A composed image is built for both `linux/amd64` and `linux/arm64` at freeze into a local OCI image-layout directory. Publish reads that layout and pushes the whole multi-architecture manifest list to the registry directly, so it does not go through `docker push` and does not need Docker's containerd image store. A plan that froze successfully is already push-ready.

## Publishing

```sh
aileron skill publish <name> --registry <ref> [--version <id>]
```

- `<name>` is the installed skill name.
- `--registry` is the destination OCI repository, for example `ghcr.io/acme/my-plan`.
- `--version` pins a specific frozen version id. Omit it to publish the newest.

The published artifact is tagged three ways. The 16-hex content-hash slug (the version id) is the canonical immutable coordinate, so the shareable reference is `<registry>:<version>`. Publish also (re)points the mutable `latest` tag at this newest artifact, and, when the frozen version carries a semver label (from `aileron skill freeze --version`), tags under that label too. A second operator can install by any of the three. Publish prints a copyable install hint with the content-hash coordinate:

```
published weekly-metrics-digest
  image:    ghcr.io/acme/my-plan@sha256:…
  artifact: ghcr.io/acme/my-plan:v1a2b3c4d5e6f (sha256:…)
  binding:  config-digest
Install with:
  aileron skill install ghcr.io/acme/my-plan:v1a2b3c4d5e6f
```

If the semver label carries build metadata (a `+build` suffix) or any other character outside the OCI tag grammar, publish skips the semver tag with a warning and still publishes the content-hash and `latest` tags.

## Publish makes this version launch from the registry

Publishing also records where the version now lives, so `aileron skill launch <name>` boots the copy you just pushed rather than a local build. On success publish writes a small install-origin note next to the frozen version pointing launch at `<registry>` and the version tag. Launch then pulls the published, content-addressed image and verifies it against the signed lock, exactly as it does on a machine that installed the plan by OCI reference.

This matters because a composed-tools plan's local image tag is a shared, content-derived daemon tag. A later freeze of any plan with the same base and tools rebuilds and repoints that tag, which would strand this version's signed lock on a now-stale local digest. Booting from the published registry image keeps launch pinned to the immutable copy you published. You do not need to install the plan locally after publishing it: the machine that published a version can launch it straight from the registry.

## Launching a plan you just froze or published

When you freeze a plan with `--publisher`, launch gates on the plan's own signing key (the `--signing-key` you froze with). If you signed with your own key rather than the repository's committed `keys/publisher.pub`, `aileron keyring trust <publisher>` will not unblock you: that command fetches the repo's committed key, a different key, and no-ops when your keyring already trusts the owner. Trust the plan's own signing key instead, with no network fetch:

```
aileron keyring trust --plan <name>
```

This reads the plan's `signing-key.pub` from your local frozen store and grants it at the plan's declared per-repo publisher authority (`github://owner/repo`), which is exactly what launch resolves. It does not widen owner-level trust to a key your organization never committed. Publishing a plan that declares a publisher prints this exact command on success, and a launch that refuses for an untrusted publisher suggests it in the refusal.

## Two binding kinds

The digest that lets launch verify the pulled image against the signed lock depends on the plan's pin type. Publish branches automatically.

- **Composed-tools plans.** The image is built locally at freeze for every supported architecture and pinned by a per-architecture set of config content digests. Publish verifies every architecture's config content digest against the signed lock before any bytes leave your machine, pushes the whole multi-architecture manifest list to the registry, then re-verifies every pushed architecture against the lock. Any mismatched, missing, or unattested architecture fails closed. This is the `config-digest` binding.
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
