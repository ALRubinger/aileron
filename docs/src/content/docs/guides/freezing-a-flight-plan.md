---
title: "Freezing a Flight Plan"
description: "How aileron skill freeze seals an installed skill into a signed, digest-pinned, immutable Flight Plan version."
---

Freeze is the author-time step that seals an installed Aileron skill into a Flight Plan ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill)). A skill before freeze names its execution environment by a movable tag. A Flight Plan after freeze pins every image to a content-addressed digest, records a lockfile, content-addresses the whole unit, and carries a detached signature. The frozen version is written immutably into the canonical skill store. This guide walks through running freeze and explains the guarantees it produces.

## What freeze does

Freeze runs a fixed sequence over one `SKILL.md` document.

1. It parses and lints the manifest. The lint rejects any step that could reach an LLM outside the marked `llm-seam` (see [the manifest spec](/development/flight-plan-manifest-spec)).
2. It resolves the declared environment to one image pin. Declared `environment.tools` are resolved to their catalog devcontainer Features and composed onto the Aileron-provided runner base. The composed image is built for both `linux/amd64` and `linux/arm64`, and the pin records one content digest per architecture, so the same Flight Plan launches on an amd64 or an arm64 host. A custom `environment.image` is resolved to its `sha256:` digest; when `tools` are declared alongside it, they compose onto that custom base. The plan runs in one container, so this resolves to a single pin. A plan that declares no environment pins nothing.
3. It builds the lockfile. The lockfile records the single resolved image pin, the resolved capability set, the step-keyed sealed trust reach for each tool step that declared a trust contract, the content hash, and the semver label.
4. It content-addresses the unit. The content hash is a `sha256` over the canonical frozen manifest bytes plus the lockfile bytes.
5. It signs the content with a detached ed25519 signature.
6. It writes the frozen version into the store as an immutable directory.

Freeze pins by digest, never by tag. A resolver that returns a tag rather than a digest is a hard error.

## Prerequisites for a composed-tools plan

A plan that declares `environment.tools` composes a real multi-architecture image at freeze, so a few build prerequisites apply. A plan with no environment, or with only a custom `environment.image`, needs none of these.

- Docker with buildx. Freeze builds the composed image for both `linux/amd64` and `linux/arm64` through `docker buildx`. Freeze preflights buildx before building and aborts with a remediation message if it is missing.
- QEMU emulators for cross-architecture builds. Building an architecture your host does not run natively needs the QEMU binfmt emulators registered. Install them once with `docker run --privileged --rm tonistiigi/binfmt --install all`. Freeze aborts with this exact command in the error when an emulator is missing, rather than silently producing a single-architecture pin.
- The containerd image store, for the local daemon load. Freeze writes the multi-architecture build to a local OCI image layout and also loads the composed image into your Docker daemon so a later publish can verify it. Loading a multi-architecture image needs Docker's containerd image store enabled (Docker Desktop: Settings, then General, then "Use containerd for pulling and storing images").

A faster alternative for CI or a release pipeline is to build each architecture natively on its own runner and merge the results with `docker buildx imagetools create`, rather than emulating the non-native architecture on one host. The freeze pin binds by config content digest, so a natively-built per-architecture image and an emulated one produce the same pin.

## Running freeze

Freeze takes the name of an installed skill or a path to a `SKILL.md`.

```sh
aileron skill freeze weekly-metrics-digest \
  --signing-key ~/.aileron/keys/author.pem \
  --version 1.0.0
```

The signing key is a PEM-encoded ed25519 private key. Pass it with `--signing-key` or set `AILERON_SIGNING_KEY` to the key path. The key is read for signing only. It is never copied into the frozen artifact or the store.

The `--version` flag records a human-facing semver label in the lock. It is optional.

Freeze prints the skill name, the version label, the content hash, and the on-disk location of the frozen version.

## The frozen version

A frozen version lives under the skill in the store at `~/.aileron/skills/<name>/versions/<id>/`. The version id is a short content-derived slug of the content hash. Each version directory holds four files.

| File | Contents |
|---|---|
| `SKILL.md` | The frozen manifest with the populated `lock` block injected. |
| `aileron.lock` | The standalone lockfile, the durable pin record. |
| `signature.sig` | The detached ed25519 signature over the content bytes. |
| `signing-key.pub` | The author public key the signature verifies against. |

The pre-freeze installed `SKILL.md` at the skill name root is never touched. Freeze adds versions beside it.

## Guarantees

Freeze is reproducible. Running freeze twice over byte-identical input with the same key produces the same content hash, the same frozen bytes, and the same signature. ed25519 is deterministic for a fixed key and message.

Frozen versions are immutable. Re-freezing changed content writes a new version directory and leaves every prior version byte-identical. Re-freezing identical content to an existing version id is a verified no-op. Writing different content to an existing version id is rejected.

The signature is verifiable at launch. A verifier needs only the signature and the public key, both stored beside the version. The launch-time runtime ([#1511](https://github.com/ALRubinger/aileron/issues/1511)) verifies the signature before it runs a Flight Plan.

The signing private key never enters the store. Only the public key and the detached signature are persisted.

A frozen version is multi-identity. The seal carries no credential binding and no identity. The identical frozen version launches under different vault-bound identities with different authorizations, and the audit trail records who ran it. The credential binding lives on the launching operator's machine, so the same signed bytes serve many operators without carrying any one operator's identity.
