---
title: "ADR-0013: Connector Hub and Trust Distribution"
description: "The Hub points to connectors at their canonical github://owner/repo FQNs without hosting artifacts or vetting publishers. Trust granularity is per-repo for v0.x with per-publisher (Microsoft model) as the v2 direction. Daemon-fronted, multi-client. Sigstore deferred."
order: 13
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-05-06</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/488">#488</a> (umbrella), <a href="https://github.com/ALRubinger/aileron/issues/408">#408</a> (trust granularity)</td></tr>
</table>
</div>

## Context

[ADR-0002](/adr/0002-connector-model) ratifies the connector model: sandboxed binaries identified by FQN, content hash, and signature. PR #407 shipped the v0.x trust mechanism — per-repo keyring entries that authorize a publisher's signing key for one specific FQN (`github://owner/repo`).

Three questions [ADR-0002](/adr/0002-connector-model) left open or got wrong:

1. **What is the Hub, and what does it do?** [ADR-0002](/adr/0002-connector-model) originally described the Hub as a registry that hosts binaries, validates manifests, and owns its own scheme (`hub://`). None of that survived design. The Hub is being built as a discovery layer that points to connectors at their canonical `github://` FQNs.

2. **How does a user trust a publisher who ships multiple connectors?** Per-repo trust (one keyring entry per FQN) scales badly as soon as a publisher ships their second connector. Issue #408 catalogued the design space: per-repo, per-publisher, or hybrid. [ADR-0002](/adr/0002-connector-model) didn't address granularity at all.

3. **Does Aileron vouch for any of this?** [ADR-0002](/adr/0002-connector-model) implies vetting ("The Hub validates manifests at publish time"). Vetting is rejected. Aileron has no editorial role.

This ADR ratifies the resolved set of decisions. It supersedes the Hub-related portions of [ADR-0002](/adr/0002-connector-model).

## Decision

### The Hub points; it does not host

The Hub is a public GitHub repository at `aileron-connectors-hub`. Each connector entry is a YAML file recording the FQN of an existing repo, its publisher, the URL of the publisher's signing key, and metadata for discovery. The Hub stores no binaries, no manifests, no signed artifacts.

When a user discovers a connector via the Hub and runs `aileron connector install <fqn>`, the install pipeline fetches the binary from `<fqn>` (typically a GitHub release artifact) exactly as it does today. The Hub is not a fetch dependency for installs; it is a discovery and metadata source. A connector installable by FQN today remains installable when the Hub is offline.

There is no `hub://` FQN scheme. Connectors carry their canonical `github://owner/repo` (or `gitlab://owner/repo`) FQN whether they are listed in the Hub or not.

### Anyone publishes; Aileron does not vet

A publisher gets listed in the Hub by opening a PR to `aileron-connectors-hub` with their entry YAML. The Hub repo's CI validates the entry against a JSON schema. Maintainers merge per the repo's own contribution rules. Aileron does not perform editorial review of the connector itself, does not verify that the publisher controls the FQN they claim, and does not audit the connector binary.

The reasoning is twofold. First, Aileron is not yet mature enough to operate a vetting program responsibly: defining "approved" without arbitrary or politicized criteria, scaling review to community submissions, handling appeals. Second, vetting raises the contribution threshold and inhibits the early connector-author community Aileron needs to grow.

Discovery and trust stay decoupled. A connector appearing in the Hub is not an endorsement. Users still verify trust per their own judgment via the keyring or the install-time prompt.

### Trust is per-repo for v0.x

Keyring entries authorize a specific FQN. PR #407 shipped this in `~/.aileron/keyring.json` with `version: 1`. The Microsoft-model alternative (one publisher key trusted for any of their connectors) is the long-term direction but not the v0.x answer.

The deferral is deliberate. With one connector per publisher today, per-repo trust is fine; the user trusts the publisher's key for that one FQN. Once a publisher ships a second connector, per-repo trust gets clumsy: the user must trust the same key again under a different FQN. The Hub making it easy to find a publisher's other connectors is the moment per-publisher trust becomes the natural shape.

### Per-publisher trust is the v2 direction

The keyring's loader explicitly checks `version: 1` and rejects anything else (`internal/cstore/keyring_config.go`), reserving the version field for a future v2 schema that supports per-publisher entries alongside or instead of per-repo entries.

The v2 schema is **not** ratified here. What this ADR commits to is the *direction*: per-publisher trust, keyed by GitHub identity, with the Hub providing the enumeration that makes "trust this publisher" mean something concrete. The exact schema shape (pure per-publisher vs hybrid coexistence with v1 per-repo entries), the migration path, and the revocation scope are tracked in #408 and will be resolved when the install-time UX work in #487 surfaces concrete needs.

The closest prior art is `packages.microsoft.com`: one publisher, one key, many products, central discovery. The Hub plays the role of the central discovery surface; the publisher's GitHub identity plays the role of the trust anchor.

### Daemon owns Hub data; CLI and webapp are thin clients

Per [ADR-0012](/adr/0012-local-daemon-architecture), the local daemon is the runtime process. Hub data follows the same model. The daemon shallow-clones the Hub repo per query (no persisted cache, no `api.github.com` calls) and exposes `/v1/hub/*` HTTP endpoints. CLI and webapp are thin clients that read from the daemon API.

This keeps the trust decision coherent across surfaces. Both `aileron connector install <fqn>` (terminal y/N) and the webapp install modal consume the same `/v1/hub/install-decision/{fqn}` payload from the daemon. The user sees the same fingerprint, the same publisher footprint, the same risk indicators regardless of where they are running.

The daemon owns:

- Fetching the Hub repo (shallow clone per query, no persisted state).
- Pre-computation of the install-decision payload (fingerprint, publisher's other Hub connectors, current keyring state for the authority).

#486 settled the no-cache, no-GitHub-API shape for v0.x. A server-side metadata service that would re-introduce caching and popularity signals is tracked as a future improvement in [#614](https://github.com/ALRubinger/aileron/issues/614).

### Sigstore deferred; static keys with three known limitations accepted

The current trust chain anchors on a long-lived ed25519 key the publisher commits to `keys/publisher.pub` in their repo. The user imports the key into their keyring or accepts it at install time. The connector binary's signature must verify against that key.

Sigstore-style identity verification (Fulcio CA + Rekor transparency log + GitHub Actions OIDC) would replace long-lived keys with ephemeral per-signing certs and add a public audit trail. That model is technically stronger but introduces additional central infrastructure (Fulcio, Rekor) that, paradoxically, is less aligned with Aileron's no-central-authority posture than per-repo static keys are.

Deferring Sigstore is a conscious trade-off. The cost is three accepted limitations of the static-key model:

1. **Leaked keys retroactively compromise past versions.** A publisher's signing key, once leaked, can be used to sign a backdated binary indistinguishable from a legitimate past release.
2. **No transparency log.** Backdated or unauthorized signatures leave no public record. Detection depends on the publisher noticing.
3. **Weak identity binding.** The trust chain is "this key happens to live at a path in the repo." With Sigstore + GitHub OIDC, the chain becomes "this binary was signed by a CI run authenticated as github.com/ALRubinger," verifiable independently.

Revisit when v1 stability work begins. Until then, these limitations are documented and lived with.

### No popularity signals and no Aileron-operated telemetry in v0.x

Popularity signals (stars, last-commit recency, release download counts) are out of scope for v0.x. With a handful of connectors and no users, popularity adds little value and forces every user's daemon to hit `api.github.com` against a 60/hr unauthenticated budget. A server-side metadata service that would re-introduce popularity is tracked as a future improvement in [#614](https://github.com/ALRubinger/aileron/issues/614).

Opt-out telemetry (PostHog or similar analytics fired on every install) was considered and rejected for v0.x. The position "Aileron assumes no responsibility for vetting" is hard to reconcile with "Aileron collects analytics on what you install." Opt-in telemetry remains a future option if precise install counts ever become a real driver.

## Alternatives Considered

### Hub-as-registry (Aileron hosts artifacts) — rejected

The Hub serves connector binaries directly. Connectors carry `hub://` FQNs. The Hub is the canonical fetch source.

Rejected for v0.x. Hub-as-registry creates hosting and abuse-moderation responsibilities Aileron is not equipped to operate. It also forces every published connector to live on Aileron's infrastructure, which would inhibit early adoption. NPM, PyPI, and similar registries grew from pointer-style models into hosting; that path remains open if scale demands.

### Per-publisher trust as v0.x default — rejected

The keyring stores per-publisher (owner-level, not per-repo) entries from day one. `aileron keyring trust github://owner` covers any repo under that owner.

Rejected for v0.x. With one connector per publisher today, per-publisher trust without Hub-driven enumeration means the user trusts a key for a publisher they cannot enumerate. The Microsoft model needs a discovery layer to be coherent. Per-repo trust is the right answer until the Hub is real and a publisher ships a second connector.

### Aileron-vetted Hub — rejected

Maintainers review submitted connectors for security, quality, or policy compliance before merging entries. Aileron's reputation backs every listed connector.

Rejected. Vetting concentrates editorial responsibility on Aileron, scales poorly with community contributions, and creates a permanent two-tier ecosystem (vetted/blessed vs unvetted/community). It also misframes the trust model. The user's decision is the trust gate, and the install-time prompt makes that explicit.

### Sigstore for v0.x — rejected, accepted as v1 direction

Use ephemeral signing keys via Fulcio + Rekor. Connector publishers sign in CI with GitHub Actions OIDC. The transparency log is the trust anchor.

Rejected for v0.x. The infrastructure dependency is meaningful (Fulcio CA, Rekor log, OIDC plumbing) and the three known limitations of the static-key model are acceptable for early adoption. Revisit when Aileron is ready to commit to that infrastructure as part of its trust story.

### PostHog opt-out telemetry — rejected

Fire an install event from every CLI install. Aggregate centrally. Show install counts in Hub UIs.

Rejected for v0.x. Inconsistent with Aileron's no-central-tracking posture. Opt-in telemetry remains a future option.

## Consequences

### For [ADR-0002](/adr/0002-connector-model)

The Hub-related portions of [ADR-0002](/adr/0002-connector-model) are superseded by this ADR. Specifically:

- The `hub://` scheme is removed from the initial scheme set.
- "The Hub indexes connectors and serves their binaries plus manifests" no longer holds. The Hub is a discovery layer over `github://` and `gitlab://` FQNs.
- "The Hub validates manifests at publish time" no longer holds. The Hub repo's CI validates the entry schema, not the connector itself.

[ADR-0002](/adr/0002-connector-model) is amended in place to reflect these changes, per the pre-MVP editing convention noted in the ADR index.

### For the keyring

`internal/cstore/keyring_config.go` reserves `version: 2` as the migration target. v0.x ships per-repo trust at v1. Per-publisher v2 ships when #408 settles the schema shape.

The install-time prompt becomes the moderation moment. With no Aileron vetting, the user's decision at first-install is the only trust gate. The prompt design is in #487.

### For connector publishers

A publisher who wants to be discoverable opens a PR to `aileron-connectors-hub` with a YAML entry pointing at their `github://owner/repo` FQN. Aileron does not host the binary. The publisher continues to sign their releases with the key at `keys/publisher.pub` in their repo. Sigstore-style signing is not required for v0.x.

### For consumers

Discovery is via the Hub: `aileron hub search`, the webapp's Hub page, or organic browsing of the `aileron-connectors-hub` repo. Trust is a separate decision: `aileron keyring trust` ahead of time, or accept the install-time prompt. The Hub does not bypass the trust step.

### For the daemon

The daemon gains responsibility for Hub data: shallow-clone-per-query of the Hub repo and install-decision payload pre-computation. The `/v1/hub/*` HTTP endpoint set is added to the daemon's API surface.

## Examples

### Hub entry

```yaml
fqn: github://ALRubinger/aileron-connector-google
description: Google Workspace connector (Calendar, Drive, Gmail)
publisher_github: ALRubinger
key_url: https://raw.githubusercontent.com/ALRubinger/aileron-connector-google/main/keys/publisher.pub
release_pattern: v*
```

### Daemon HTTP API

```
GET  /v1/hub/connectors                  # list all entries
GET  /v1/hub/connectors?q=<query>        # keyword search
GET  /v1/hub/connectors/{fqn}            # single entry
GET  /v1/hub/install-decision/{fqn}      # pre-computed install-decision payload
```

### Install-decision payload (sketch)

The payload shape is finalized in #487. At minimum:

```json
{
  "fqn": "github://ALRubinger/aileron-connector-google",
  "description": "...",
  "publisher_github": "ALRubinger",
  "fingerprint": "sha256:i4l2kuD8q++d5b9v8/LLI1",
  "trust_state": "unknown",
  "publisher_footprint": ["github://ALRubinger/aileron-connector-slack"],
  "risk_indicators": [
    "First connector by this publisher you've installed"
  ]
}
```

The CLI renders this in a terminal y/N prompt; the webapp renders it in a modal. Both surfaces consume the same payload.
