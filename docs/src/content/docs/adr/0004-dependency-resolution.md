---
title: "ADR-0004: Dependency Resolution"
description: "How FQNs resolve to fetched bytes; content-addressed store layout; install, update, and verify pipelines; user-level sync over installed actions"
order: 4
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

[ADR-0002](/adr/0002-connector-model) establishes that connectors are content-addressed with FQN identity (`github://owner/repo/...`), pinned semantic versions, and binary hashes. [ADR-0003](/adr/0003-action-model) establishes that actions reference connectors by `(FQN, version, hash)` triples and that action templates are themselves FQN-identified artifacts copied into the user's `~/.aileron/actions/`.

Both ADRs explicitly deferred *how* this works in practice:

- *How does a `version` field on a github:// FQN map to a concrete fetch URL?*
- *Where do downloaded binaries live on disk, and how does that store handle two simultaneous installs of the same connector?*
- *How does `aileron connector check` know what newer versions exist?*
- *How are pre-releases surfaced — or hidden?*
- *What happens when the source is offline, or when the binary the source serves doesn't match the hash declared in the action file?*
- *How does `aileron action add <FQN>` walk the action's connector dependencies on its way to a working install?*

These are operational decisions, not architectural ones. They do not change *what* Aileron is. They do shape the developer experience deeply: the difference between "the install just worked" and "I'm reading a stack trace from a tarball extractor" lives entirely here.

This ADR ratifies the resolution model and the install pipeline.

## Decision

### Per-scheme resolution: FQN + version → fetch URL

Each scheme has a fixed mapping from `(FQN, version)` to a concrete fetch URL. The resolver knows these mappings as code; users do not configure them.

#### `github://` and `gitlab://`

For `<scheme>://<owner>/<repo>/<...subpath>/<artifact>@<version>`:

1. The first two segments after the scheme are the source repo: `<owner>/<repo>`.
2. Everything from the third segment onward is the **artifact path** within the repo.
3. The version maps to a **tag** in the source repo:
   - **Single-artifact repos** (no subpath): tag is `v<version>`. Example: `github://aileron/slack@1.2.0` → tag `v1.2.0` on `github.com/aileron/slack`.
   - **Multi-artifact repos** (with subpath): tag is `<subpath>/v<version>`. Example: `github://aileron/integrations/connectors/slack@1.2.0` → tag `connectors/slack/v1.2.0` on `github.com/aileron/integrations`.
4. The release at that tag carries one asset named `aileron.tar.gz` containing the connector binary, the manifest, and a detached signature.
5. The fetch URL is the standard release-asset URL for the host:
   - GitHub: `https://github.com/<owner>/<repo>/releases/download/<tag>/aileron.tar.gz`
   - GitLab: `https://gitlab.com/<owner>/<repo>/-/releases/<tag>/downloads/aileron.tar.gz`

This is the [Go modules tagging convention](https://go.dev/ref/mod#vcs-version) adapted for our purposes. Publishers who want their releases discoverable follow it; nothing prevents publication that doesn't follow it, but the resolver won't find such releases.

#### `hub://`

For `hub://<owner>/<...path>/<artifact>@<version>`:

The Hub serves artifacts directly through its HTTP API:
- Manifest: `GET https://hub.aileron.dev/api/v1/<owner>/<...path>/<artifact>/<version>/manifest`
- Tarball: `GET https://hub.aileron.dev/api/v1/<owner>/<...path>/<artifact>/<version>/aileron.tar.gz`
- Available versions: `GET https://hub.aileron.dev/api/v1/<owner>/<...path>/<artifact>/versions`

The Hub stores artifacts by `(owner, path, artifact, version)` and is responsible for immutability (a published version cannot be re-pushed with different bytes). This is the only strong difference between `hub://` and git-host schemes: the Hub guarantees immutability server-side; git-host schemes rely on the client-side hash check to catch tag re-pushes.

#### Adding a scheme

A new scheme requires code in Aileron's resolver implementing four operations:

| Operation | Input | Output |
|---|---|---|
| Resolve fetch URL | FQN, version | URL of the tarball |
| Resolve manifest URL | FQN, version | URL of the manifest (may be inside the tarball) |
| Verify signature | binary bytes, signature, FQN authority | accept / reject |
| List available versions | FQN | sorted list of SemVer strings |

A scheme that cannot implement all four is not a viable Aileron source. An unknown scheme in an action file is a hard error at install — the resolver does not silently fall back.

### Tarball layout

Every release tarball has the same internal layout:

```
aileron.tar.gz
├── connector.wasm          (or connector.bin for OS-process connectors)
├── manifest.toml
└── signature.sig           (detached signature over connector.* + manifest.toml)
```

The hash declared in action files (and in the connector manifest's `provenance_hash` field) is the SHA-256 of the *uncompressed concatenation* of `connector.*` followed by `manifest.toml` (signature excluded). This makes the hash deterministic regardless of how the tarball was compressed, while still covering both the executable and its declared capabilities.

For *action templates*, the layout is simpler:

```
aileron.tar.gz
├── action.md              (the action file itself)
└── signature.sig
```

The hash is the SHA-256 of `action.md`.

### Content-addressed store layout

Aileron maintains a single content-addressed store per machine, by default at `~/.aileron/store/`, overridable by environment variable.

Layout:

```
~/.aileron/store/
├── connectors/
│   └── sha256/
│       └── <hash>/
│           ├── connector.wasm
│           ├── manifest.toml
│           └── signature.sig
├── actions/
│   └── sha256/
│       └── <hash>/
│           ├── action.md
│           └── signature.sig
└── index/
    ├── connectors.json    (FQN+version → hash mapping; cache only)
    └── actions.json       (FQN+version → hash mapping; cache only)
```

**Properties:**

- **Lookup is by hash, not by name.** Two installs of the same `(FQN, version)` that produce the same bytes share one store entry. Two installs that disagree on bytes (e.g., a publisher re-tagged) produce two distinct entries; the runtime uses the one whose hash matches the requesting action file.
- **Atomic writes.** Install downloads to a temp directory, computes hash, verifies it matches expected, then renames the temp directory into `sha256/<hash>/`. A partial download or failed verification leaves the temp directory; the store remains consistent.
- **Concurrent installs of the same hash are safe.** If two processes race to install the same `(FQN, version)`, both compute the same hash, both rename to the same destination — at most one rename succeeds and the other becomes a no-op. The losing process verifies the existing directory's contents and proceeds.
- **The index files are caches, never sources of truth.** They map FQN+version → hash for fast lookup during action loading. If they go missing or get corrupted, Aileron rebuilds them from the on-disk store contents.
- **The store is shared across all of the user's actions.** A connector installed once is available to every action that references it without redundant downloads. This works because identity is the hash, not a per-action install location.

The store is intentionally not a content-addressable filesystem like git's object database — it stores extracted artifacts, not packed objects — but the trust property is the same: the hash *is* the address.

### Install pipeline

`aileron connector install <FQN>@<version>` and the implicit installs that happen during `aileron action add` follow the same pipeline:

```
 1. Parse FQN; identify scheme; reject if scheme is unknown.
 2. Resolve fetch URL via the scheme's resolver.
 3. Download tarball to ~/.aileron/store/tmp/<random>/.
 4. Extract tarball.
 5. Verify signature against keys associated with the FQN's authority.
 6. Compute SHA-256 over the canonical hash input.
 7. Compare computed hash:
       - First install (no expected hash): record the computed hash.
       - Subsequent install (action file declares a hash): MUST match.
 8. Rename tmp directory to sha256/<hash>/ (atomic).
 9. Update the index cache.
10. Done.
```

Any failure in steps 3 through 7 leaves the temp directory in place (for debugging) and aborts the install. The store remains untouched.

For `aileron action add`, an additional pre-step walks the fetched action's `[[requires.connectors]]` blocks. Each missing connector triggers its own pipeline. The action file is written to `~/.aileron/actions/` only after every declared connector is present in the store.

### Update discovery

`aileron connector check` walks the user's installed actions in `~/.aileron/actions/`, collects every unique connector FQN, and queries each FQN's scheme resolver for available versions. Output is a per-FQN list of newer versions:

```
$ aileron connector check
You have 5 connectors in use:

  github://aileron/slack            1.2.0  (current)  →  1.3.0  (available)
  github://aileron/git              2.1.0  (current)  →  no updates
  github://aileron/linear           0.4.2  (current)  →  0.5.0, 0.5.1  (available)
  hub://aileron/gcal                3.0.0  (current)  →  no updates
  github://acme/stripe              1.0.0  (current)  →  1.0.1, 1.1.0-rc.1  (available)
                                                       (1.1.0-rc.1 is a pre-release;
                                                        pass --include-prerelease to apply)
```

`aileron action update <name>` (mentioned in [ADR-0003](/adr/0003-action-model)) does the same query for the action template's source FQN.

**Pre-release handling.** Versions whose SemVer carries a pre-release suffix (`-rc.1`, `-alpha.2`, `-beta`) are surfaced as informational but never offered as the recommended update. The user must explicitly opt in with `--include-prerelease` for pre-releases to be installable through update tooling. (Direct install of a pinned pre-release version always works: `aileron connector install github://acme/stripe@1.1.0-rc.1` is unambiguous and proceeds.)

**Discovery is read-only.** `check` queries the source but does not download anything. It is safe to run frequently and offline-failing (a source being unreachable produces a "could not check <FQN>" line, not a global error).

### `aileron sync` — verify and repair the user's local state

`aileron sync` is the primitive for "make sure my installed actions can actually run." It:

1. Walks every `~/.aileron/actions/*.md` file the user has installed.
2. Collects the union of `[[requires.connectors]]` references.
3. For each `(FQN, version, hash)` triple not already in the local store: runs the install pipeline.
4. Reports any unbound capability bindings (handing off to the install-consent / capability-binding flows defined in their own ADRs).

`sync` is idempotent. Running it when everything is already installed is a fast no-op (all index lookups hit). Running it after restoring `~/.aileron/actions/` from a backup, on a fresh machine where the user copies their action files over, or after a `~/.aileron/store/` corruption completes the gaps.

`sync` does not modify action files. It does not bump versions, does not "follow latest," does not auto-merge anything. The action files are the source of truth; `sync` is there to make the local store match them.

### Offline behavior

- **Action invocation is fully offline once installed.** The runtime needs only the local store and the user's action files. No network access is required, ever, for executing an installed action.
- **Install requires network.** Downloading a binary necessarily reaches the source.
- **Update discovery requires network.** `check` queries each scheme's API.
- **Reinstall of an already-stored hash is offline.** If `(FQN, version, hash)` resolves to an entry already in the store, the install pipeline short-circuits at step 7 — no download, no signature check on the already-trusted bytes.

The store is the substrate. Once a connector is in it, the user is independent of the source.

### Failure modes

| Condition | Behavior |
|---|---|
| Unknown scheme | Hard fail at install. Action files referencing the FQN fail to load. |
| Source unreachable during install | Hard fail. No fallback, no warning-and-continue. |
| Tag not found (version doesn't exist) | Hard fail. Resolver lists available versions in the error. |
| Signature verification fails | Hard fail. No "warning, you are about to install an unsigned binary" downgrade. |
| Hash mismatch (action declares X, source serves Y) | Hard fail. Action files are not modified. The user must investigate (publisher re-tagged? supply chain attack?) and explicitly update the hash if appropriate. |
| Tarball malformed | Hard fail. |
| Concurrent install race | Both processes converge; the second observes the entry already present and proceeds. |
| Store directory unwritable (permissions) | Hard fail with actionable error. |

The general rule: silent failure is worse than loud failure. Every failure mode produces a structured error with an audit ID; the runtime never falls back to a "best guess" connector or skips a hash check.

## Alternatives Considered

### Lock file (rejected)

A generated `aileron.lock` file records the resolved versions and hashes for every connector and action the user has installed. The runtime reads the lock file rather than walking action files.

Rejected because action files are *already* self-describing — each action file declares the connectors it uses with version and hash inline. A separate lock file duplicates this information and creates two sources of truth that can disagree. Lock files are useful when you have version ranges to resolve (`^1.2.x`); we don't have ranges (per [ADR-0002](/adr/0002-connector-model)), so there's nothing to resolve.

### Server-side resolution (rejected)

A central Aileron service (the Hub or a sibling) accepts an action file and returns the fully-resolved set of binaries and metadata. The client just downloads what the server tells it to.

Rejected because it introduces a hard runtime dependency for installs and creates a central coordination point where there doesn't need to be one. Resolution per scheme is well-defined and small enough to run client-side. Pushing it to a server adds a single point of failure, additional latency, and a privacy concern (the server learns every user's full dependency manifest on every sync).

The Hub is a *catalog and serving surface* for `hub://` artifacts. It is not a resolution authority for other schemes.

### Auto-update on `aileron sync` (rejected)

`aileron sync` not only installs missing connectors but also pulls the latest matching versions for connectors already installed, updating action files as it goes.

Rejected because it breaks the "action file is the contract Aileron executes" invariant from [ADR-0003](/adr/0003-action-model). Auto-update would mean a `sync` could change what installed actions do — silently, without the user reviewing the change. Updates must always go through explicit, visible action-file edits. `check` surfaces what's available; the user decides what to apply.

### Implicit fallback when source is unreachable (rejected)

If the source is down at install time, fall back to a community mirror or a locally-cached "last known good" version.

Rejected because it makes the trust chain ambiguous. The user explicitly asked to install `github://aileron/slack@1.2.0` from GitHub; if GitHub can't serve it, the right behavior is to surface that fact, not to substitute a different artifact. Mirrors and offline caches are real needs but should be explicit (a `mirror://` scheme, an air-gapped store sync command), not implicit.

### Per-action store (rejected)

Each action has its own private connector store rather than a shared one at `~/.aileron/store/`.

Rejected because it duplicates downloads — a user with ten actions all using `github://aileron/slack@1.2.0` would download the same connector ten times. A shared store with content-addressed lookup gives every action independent verification (the hash check is per-action, not per-store) while sharing the bytes.

The shared store does mean a corrupted store affects every action. Mitigations: (a) the store is rebuildable from action files via `aileron sync`; (b) integrity verification on startup is cheap (the store is hash-indexed); (c) a future `aileron store gc` / `aileron store verify` will provide explicit maintenance commands.

### Versioned APIs as the resolution unit (rejected)

Instead of (or in addition to) versioning connector binaries, version the *capability surface* — `slack@chat-v2`, `slack@chat-v1` — so actions depend on stable interfaces rather than specific binaries.

Rejected as out of scope. Aileron does not have a capability abstraction layer (per [ADR-0002](/adr/0002-connector-model)); capability declarations are connector-specific. Layering an interface-versioning concept on top would re-introduce the abstraction layer that [ADR-0002](/adr/0002-connector-model) explicitly rejected.

## Consequences

### For users

- `aileron sync` brings the local store up to date with whatever actions are in `~/.aileron/actions/`. Useful after restoring action files from a backup, on a fresh machine where actions have been copied over, or after store corruption.
- Multiple versions of the same connector coexist transparently. Migrating one action from `slack@1.2.0` to `slack@2.0.0` does not require touching any other action.
- A connector is reusable across all of the user's actions with no duplicate downloads — the shared store handles caching.
- Update discovery is a separate, opt-in operation (`check`) from update application (an explicit edit to action files). The default is "things stay still until the developer decides otherwise."

### For Aileron implementation

- The resolver is a pluggable component: each scheme implements four well-defined operations (resolve URL, fetch, verify signature, list versions). Adding `oci://`, `https://`, or any future scheme is a contained change.
- The store is a flat directory with deterministic layout. There is no database, no index server, no metadata store. Everything is rebuildable from on-disk contents.
- The install pipeline is a sequence of local file operations after the initial download. It is testable with a fake resolver and a tmp directory.
- Concurrency safety comes from atomic rename, not from a lockfile or a coordinating daemon. Multiple `aileron` processes can run installs simultaneously without coordinating.

### For publishers

- Tag conventions (Go modules-style: `v<SemVer>` or `<subpath>/v<SemVer>`) are documented and enforced by the resolver. Publishers who follow them are discoverable; those who don't are not.
- Each release ships a single tarball with a fixed internal layout. The publisher decides nothing about packaging beyond what goes inside the tarball (binary, manifest, signature).
- Pre-release tagging (`-rc.1`, `-alpha.2`) is supported and is the recommended way to ship test versions without surprising users running `connector check`.

### For the Hub

- The Hub is one resolver among several. It is privileged in user experience (search, browse, reviews) but not in identity or trust.
- `hub://` artifacts go through the same install pipeline as `github://` artifacts. Same hash check, same signature verification, same atomic store write.
- The Hub additionally enforces server-side immutability: a `(FQN, version)` published to the Hub is permanent. Tag re-pushing — possible on git hosts — is impossible by Hub policy.

### For audit and security

- Every install records: requesting action (if any), FQN, version, hash, signature verification result, source response time, audit ID. The audit log is the per-install ledger.
- A hash mismatch never silently passes. The runtime refuses to execute a connector whose store entry no longer matches the action's declared hash (e.g., if the store directory was tampered with after install).
- Network access during install is limited to scheme-resolved fetch URLs. The pipeline does not follow redirects to arbitrary hosts.

### Open implementation questions (deferred to subsequent ADRs)

- *What does the install consent flow show, and what does the user actually click?* — [ADR-0007](/adr/0007-install-consent).
- *How does a user bind a capability declared by a freshly-installed connector to a concrete vault entry?* — [ADR-0006](/adr/0006-capability-binding-ux).
- *Where is the signing-key authority for each FQN scheme stored, and how does key rotation work?* — referenced in [ADR-0002](/adr/0002-connector-model); signing-keys-and-rotation will be its own implementation note rather than a fresh ADR.

## Examples

### Resolution of a github FQN with a subpath

Action file declares:

```toml
[[requires.connectors]]
name = "github://aileron/integrations/connectors/slack"
version = "1.2.0"
hash = "sha256:abc123..."
```

Resolver steps:

```
1. Scheme: github
2. Repo: github.com/aileron/integrations
3. Subpath: connectors/slack
4. Tag: connectors/slack/v1.2.0
5. Fetch URL: https://github.com/aileron/integrations/releases/download/
              connectors%2Fslack%2Fv1.2.0/aileron.tar.gz
```

(Tag-name URL encoding is the resolver's job; users never see escaped slashes in action files.)

### Store layout after installing two connectors

After installing actions that use `github://aileron/slack@1.2.0` and `github://acme/stripe@1.0.0`:

```
~/.aileron/store/
├── connectors/
│   └── sha256/
│       ├── abc123def456.../
│       │   ├── connector.wasm
│       │   ├── manifest.toml
│       │   └── signature.sig
│       └── 789xyz012abc.../
│           ├── connector.wasm
│           ├── manifest.toml
│           └── signature.sig
└── index/
    └── connectors.json
```

The index file maps:

```json
{
  "github://aileron/slack@1.2.0": "sha256:abc123def456...",
  "github://acme/stripe@1.0.0": "sha256:789xyz012abc..."
}
```

### `aileron sync` on a fresh machine

The user has copied their `~/.aileron/actions/` over from another machine (or restored from backup) but the local store is empty:

```
$ aileron sync
Reading actions...
  ✓ ~/.aileron/actions/ship-update.md   (uses 2 connectors)
  ✓ ~/.aileron/actions/file-bug.md      (uses 1 connector)
  ✓ ~/.aileron/actions/reply-to-pm.md   (uses 1 connector)

Resolving 4 unique connectors...
  ↓ github://aileron/slack@1.2.0          fetching (398 KB)
  ↓ github://aileron/git@2.1.0            fetching (412 KB)
  ↓ github://aileron/linear@0.4.2         fetching (689 KB)
  ↓ hub://aileron/gmail@1.5.0             fetching (502 KB)

  ✓ All connectors verified and stored.

Capability bindings:
  ✗ oauth2/slack/work       NOT BOUND  →  run: aileron binding setup
  ✗ api_key/linear/team     NOT BOUND  →  run: aileron binding setup
  ✗ oauth2/gmail/personal   NOT BOUND  →  run: aileron binding setup

Local environment ready. Bind credentials before first action invocation.
```

### Hash mismatch during install

```
$ aileron connector install github://aileron/slack@1.2.0
Resolving github://aileron/slack@1.2.0...
  Tag: v1.2.0 on github.com/aileron/slack
  Fetching aileron.tar.gz...
  Verifying signature...           ✓
  Computing hash...                sha256:zzz999...
  Action file declares hash...     sha256:abc123...

ERROR: hash mismatch for github://aileron/slack@1.2.0
  Expected: sha256:abc123...
  Got:      sha256:zzz999...

This means the source republished the same version with different bytes,
or the source was compromised. The install was aborted; nothing was
written to the store.

Investigate before updating the hash in your action file.
```
