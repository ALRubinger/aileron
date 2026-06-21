---
title: "The Keyring"
description: "How Aileron decides which publishers it will run code from. A local, fail-closed list of trusted ed25519 signing keys that every connector and action install is verified against before anything reaches disk."
order: 6
---

The **keyring** is where Aileron records which publishers you trust to ship code that runs on your machine. Every [connector](/concepts/connectors/) and every [action](/concepts/actions/) you install is signed by its publisher with an ed25519 private key. Before Aileron writes a binary to disk, it verifies that signature against the public keys in your keyring. No matching key means no install.

If the [vault](/concepts/the-vault/) is what keeps your credentials safe once code is running, the keyring is what decides whether that code is allowed to run at all. The vault answers "where do these credentials go." The keyring answers "whose code do I trust to ask for them."

## Fail closed by design

Aileron ships with an empty keyring. There are no pre-trusted publishers. Every publisher you install from has to be added explicitly, by you.

This is deliberate. When the keyring has no key that verifies an artifact's signature, the install fails closed with the class `signature_failure`, before any byte reaches disk. An unsigned binary, a binary signed by a key you have not trusted, and a binary whose signature does not match the trusted key all fail the same way. The default answer is no.

The keyring lives in a single file at `~/.aileron/keyring.json`. You can edit it by hand, but the `aileron keyring` subcommands manage it for you.

## Owner-level trust and per-repo trust

A publisher is identified by the owner segment of a connector's fully-qualified name (FQN). The FQN `github://ALRubinger/aileron-connector-google` has owner `ALRubinger` and repo `aileron-connector-google`. The keyring records trust at two granularities.

- **Owner-level trust** (`github://ALRubinger`) authorizes every connector that publisher ships, present and future. This is what the `keyring trust` command writes, and what an accepted install-time prompt writes. One grant covers the whole publisher.
- **Per-repo trust** (`github://ALRubinger/aileron-connector-google`) authorizes a single repository. You would place one of these by hand to pin trust narrowly to one connector without trusting everything the owner ships.

In the on-disk v2 schema these land in separate buckets: owner-level grants in an `owners` map and per-repo grants in a `publishers` map. Install verification resolves them as a **positive-only union**. A signature is accepted when the owner-level grant or the per-repo grant has a key that verifies it. There is no per-repo deny that overrides an owner-level grant. An owner-level grant authorizes every repo under that owner, and the only way to narrow it is to revoke the owner grant and add per-repo grants instead. The union is fail-closed: when neither scope has a matching key, the install fails with `signature_failure`.

The design rationale for owner-granularity trust lives in [ADR-0013: Connector Hub and Trust Distribution](/adr/0013-connector-hub-and-trust-distribution/), and the signing-and-verification model it builds on is [ADR-0002: Connector Model](/adr/0002-connector-model/).

## The three subcommands

### `keyring trust`

Trusting a publisher fetches their public signing key and writes an owner-level grant. The command accepts two forms.

A full per-repo FQN fetches the key from that connector's own `keys/publisher.pub` file on the repository's default branch:

```
aileron keyring trust github://ALRubinger/aileron-connector-google
```

A bare owner resolves the key through the [Connector Hub](/guides/discovering-connectors/) catalog, using the `key_url` recorded for a connector that owner publishes:

```
aileron keyring trust github://ALRubinger
```

Either form writes a single owner-level grant. On success the command prints the owner authority, the key fingerprint, and the keyring path:

```
✓ Trusted publisher github://ALRubinger
  This covers every connector this publisher ships.
  Fingerprint: sha256:Hh3Kd8h2bQp7nQ0r9aTcWx
  Keyring: /Users/you/.aileron/keyring.json
```

Trusting a key the keyring already holds is a no-op that reports the existing fingerprint and exits zero, so re-running the command is always safe. The bare-owner form errors with guidance to trust via a specific connector when the Hub carries no resolvable `key_url` for that owner.

### `keyring list`

Listing prints the trusted publishers grouped by owner. Each owner header carries the count of owner-level keys and their fingerprints. Per-repo grants nest underneath the owner they belong to. Owners are printed in sorted order, and the per-repo entries under each owner are sorted too:

```
Trusted publishers (2):
  github://ALRubinger  (1 owner key)
    sha256:Hh3Kd8h2bQp7nQ0r9aTcWx
  github://acme  (0 owner keys)
    github://acme/widget-connector  (1 key)
      sha256:Zq1Lm4v8pR2sN6tY0wB3eK
```

An owner that has only per-repo grants still gets a header, with a zero owner-key count, so a narrowly-pinned publisher is never dropped from the listing. Listing reads from disk only and never reaches the network.

### `keyring revoke`

Revocation has two mutually-exclusive forms, and supplying both or neither is a usage error.

Revoking by authority removes a whole grant. A bare owner authority drops the owner-level grant. An exact per-repo authority drops that one per-repo grant:

```
aileron keyring revoke github://ALRubinger
aileron keyring revoke github://acme/widget-connector
```

Revoking by fingerprint removes one key everywhere it appears, across every owner-level and per-repo grant that registered it, while leaving each authority's other keys intact:

```
aileron keyring revoke --key sha256:Hh3Kd8h2bQp7nQ0r9aTcWx
```

The by-fingerprint form is the path for retiring a single compromised or rotated key. After any revocation, installs that depended on the removed grant fail closed until you trust the publisher again.

### Fingerprints

A fingerprint is a short, stable identifier for a public key. Aileron computes it as `sha256:` followed by the first 22 base64 characters of the SHA-256 hash of the key. It is long enough to disambiguate keys at a glance and short enough to fit on one terminal line. You compare a fingerprint against the publisher's release notes, or against the `keys/publisher.pub` they commit, before you decide to trust.

## The install-time prompt

You rarely run `keyring trust` by hand. The common path is to let an install prompt you. When you run `aileron action add`, `aileron action add-suite`, or `aileron connector install` against a publisher you have not trusted yet, Aileron offers to trust them first:

```
Publisher github://ALRubinger/aileron-connector-google is not yet trusted.
  Aileron will fetch keys/publisher.pub on the publisher's default branch
  and trust github://ALRubinger to verify signed installs from every connector
  this publisher ships.
Trust publisher github://ALRubinger/aileron-connector-google? [y/N]:
```

The default is no. On `y`, Aileron fetches the publisher's key once, writes an owner-level grant, and proceeds with the install. On anything else, the install aborts.

The prompt fires at most once per owner across a single command. When a suite install pulls in five connectors from the same publisher, you confirm that publisher once and every sibling connector in the run is covered, because the accepted grant is owner-level. A publisher you decline once is skipped silently for the rest of that run, and a fresh invocation starts the decision over.

## Key rotation and re-trusting

A publisher rotates their signing key by generating a new keypair and committing the new public key. To keep installing from them, you trust the new key. Because multiple keys can be registered for the same owner, the new key is added alongside the old one, and artifacts signed by either continue to verify. When you are confident the old key is retired, remove it with `keyring revoke --key <fingerprint>`.

**Key divergence** is the case worth understanding. Trust is evaluated at owner granularity, so a publisher you trust once is expected to sign everything they ship with the same key. When the key fetched for a new install does not match the owner-level grant already on your keyring, that is a divergence. The legitimate cause is a rotation the publisher has not announced to you yet. The illegitimate causes are a man-in-the-middle on the fetch or an impersonation attempt. Aileron surfaces the divergence as an informational signal and does not block on it by itself. The right response is to run `aileron keyring list`, compare the trusted fingerprint against the publisher's release notes or their committed `keys/publisher.pub`, and decide before you confirm.

## See also

The keyring is the trust primitive these task guides exercise. Each guide walks its own slice of the install flow end-to-end; this page is the canonical description of the trust model underneath them.

- [Discovering Connectors](/guides/discovering-connectors/) — the Hub trust panel and the install-decision prompt that read the keyring.
- [Installing a Connector](/guides/installing-a-connector/) — `keyring trust` and the standalone connector install pipeline.
- [Installing an Action](/guides/installing-an-action/) — the trust prompt that fires when an action's publisher is new.
- [Publishing a Connector](/guides/publishing-a-connector/) — the publisher side: generating a keypair and committing `keys/publisher.pub`.
- [ADR-0013: Connector Hub and Trust Distribution](/adr/0013-connector-hub-and-trust-distribution/) — the design behind owner-granularity trust and the v2 keyring schema.
- [ADR-0002: Connector Model](/adr/0002-connector-model/) — sandboxing, signing, and the verification model the keyring serves.
