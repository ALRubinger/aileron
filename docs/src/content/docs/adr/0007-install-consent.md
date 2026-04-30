---
title: "ADR-0007: Install Consent Flow"
description: "A single binary decision — install or cancel. No partial install, no per-capability denial. All-or-nothing keeps the invariant that an installed artifact has all of its declared capabilities."
order: 7
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

[ADR-0002](/adr/0002-connector-model) makes the user the sole authority for granting capabilities. A connector's manifest declares what it needs; the runtime grants nothing not declared; the user decides whether the manifest is acceptable in the first place. [ADR-0003](/adr/0003-action-model) extends this: actions declare which connectors they use and which capability subsets they exercise.

Both ADRs leave the user-facing question open: *what does it look like to actually agree to install a connector or action, and what does the user see at the moment they decide?*

The decision shapes the entire trust model. If the install prompt is a wall of jargon nobody reads, the security claim collapses to "we asked." If it overloads the user with knobs ("install but deny network access; install but only grant chat:write"), the result is configurable confusion: every install becomes a fresh policy debate, and the invariant *what's declared in the manifest is what the connector is allowed to do* breaks the moment a user starts redacting it.

This ADR ratifies the install consent flow.

## Decision

### Install is a single binary decision: accept the manifest, or don't install

The user has exactly two choices at install time: **Install** or **Cancel**. There is no "install but deny capability X" option. There is no "install with reduced permissions." There is no per-capability checkbox.

This is the load-bearing rule. It preserves the invariant from [ADR-0002](/adr/0002-connector-model) and [ADR-0003](/adr/0003-action-model): *an installed artifact has all of its declared capabilities, exactly as written in the manifest, no more and no less.* Letting the user redact the manifest at install time would mean the connector's code can no longer rely on its own declared interface. The connector would have to ask permission for every operation, every time, against an unpredictable subset of its own capabilities. That's not capability-based security; that's a bespoke capability negotiation per install.

If the user finds a connector's declared capabilities unacceptable, the answer is to not install it. There is no middle ground — a connector that needs `oauth2(scope=*)` is not made safer by users who install it with reduced scope and hope it works.

This is the same posture iOS App Store, macOS App Sandbox, and Android (via the modern permissions UI) converged on after years of trying tiered consent flows. Tiered prompts produced lower install rates *and* lower security: users who could deny capabilities denied them and then complained when apps broke; developers responded by asking for capabilities only at the point of need (defeating the up-front review). The cleanest contract is also the most honest: the manifest is what's installing, take it or leave it.

### What the user sees at the install prompt

The install prompt presents:

| Field | Source | Meaning |
|---|---|---|
| Artifact FQN | command argument | What is being installed and where it came from |
| Version | command argument | Which version |
| Hash (short form) | manifest / fetch | The content hash; identifies these specific bytes |
| Publisher (FQN authority) | derived from FQN | Who published this artifact |
| Signature status | install pipeline | Whether the binary was signed by a key associated with the FQN's authority |
| Declared capabilities | manifest | Network hosts, credential kinds + scopes, runtime imports |
| Description | manifest | Brief prose explaining what the connector or action does |
| Connector dependencies (actions only) | manifest | If installing an action, the list of connectors the action requires, each shown with its own capability summary |

Capabilities are shown grouped and human-readable, not as raw TOML:

```
This connector will be allowed to:

  Network
    • slack.com:443

  Credentials (you'll be asked to bind these later)
    • OAuth2  scopes: chat:write, channels:read

  Runtime functions
    • wasi:http/outgoing-handler  (make HTTP requests)
    • aileron:audit/emit           (write to your audit log)
```

The format mirrors how iOS shows app entitlements at install: structured, scannable, no dense jargon. A user who knows what `wasi:http/outgoing-handler` means gets the precise import name; a user who doesn't gets the parenthetical gloss.

### Signature status is *information*, not *authorization*

The prompt shows whether the artifact's signature verified against keys associated with the FQN's authority. This is purely informational. It is *not* a gate that bypasses the consent flow.

A signed-and-verified connector and an unsigned connector both go through the *same* prompt, with the same set of fields. The difference is which one of these lines appears at the top:

```
✓ Signed and verified  (key matches publisher's release config)
⚠ Unsigned             (the publisher has not published a signing identity for this source)
✗ Signature failed     (the binary is signed, but by a key not authorized for this source)
```

The third state is a **hard fail**, not a warning. Install is refused — the user is not given the option to override. Signature failure means a binary claims to be from the FQN's authority but isn't, which is exactly the supply-chain attack the signature exists to detect.

The first two states both proceed to the full consent prompt. The user reads the manifest and decides. A trusted publisher with a clean signature gets no special treatment; the manifest is still the contract being agreed to.

### Action installs cascade transparent connector consent

When `aileron action add <FQN>` runs, the action template is fetched, its `[[requires.connectors]]` blocks are parsed, and any connector not already in the local store is queued for installation. Each connector gets its **own** install prompt; the user explicitly consents to each new connector, even if they're being installed in the same flow.

The prompts are stacked, not collapsed. A user adding an action that uses two new connectors sees:

1. Action consent — what the action does, which connectors it uses (named), what capability subsets it exercises, plus the connector summary.
2. Connector A consent — full manifest review.
3. Connector B consent — full manifest review.

In sequence. Cancel at any step aborts the entire install — the action file is not written, no connectors are stored, the user's local state is unchanged. There is no partial state.

This stays-the-same-as-explicit pattern matters: a user installing an action whose author bundles a malicious dependency *sees* that dependency arrive. There is no "I trusted the action so I trust everything it pulls in" implicit grant. Each new connector is a new trust decision.

### Already-installed connectors don't re-prompt

If an action's connector dependencies are already present in the local store at the exact `(FQN, version, hash)` triple, no consent prompt fires for that connector. Consent was given previously; the bytes are unchanged; nothing new is being installed.

If the action declares a *different* hash for an FQN+version that's already in the store at a different hash — that's a hash mismatch, which fails per [ADR-0004](/adr/0004-dependency-resolution). The user is not asked to choose between bytes.

### Updates show capability diffs

Installing a newer version of a connector or action the user already has produces a consent prompt with a **diff** of changes from the installed version, not a full manifest re-review:

```
Updating: github://aileron/slack
  1.2.0 → 1.3.0

Capability changes:
  + Network: slack.com:443/api/v2  (new in 1.3.0)
  + Credentials: oauth2 scope:files:write  (new in 1.3.0)
    Unchanged: chat:write, channels:read

This update adds 1 new credential scope.

[Update]  [Cancel]
```

The diff is the load-bearing element of the update prompt. A new version that adds capability is a different trust decision from "the same connector you already approved." A new version that adds nothing is a much smaller decision — bug fixes, dependency bumps, performance work.

### Headless installs require explicit opt-in

Install consent is interactive by definition. For automation — CI pipelines, scripted setup, agent-driven configuration — the user opts into headless mode:

```sh
aileron action add github://aileron/ship-update@1.0.0 --yes
aileron sync --yes
```

The `--yes` flag is **explicit per command**. There is no `aileron config set always_yes true`. There is no environment variable that defaults the answer to yes. Each invocation that wants to skip the prompt has to say so, every time, in the visible command line.

This is deliberate friction. A `--yes` in a CI pipeline is a code review concern: someone reading the pipeline can see what's being auto-approved. A persistent global "always yes" would let auto-approval drift from the user's awareness.

`--yes` does not bypass the *signature failure* hard fail. A binary whose signature doesn't verify is refused regardless of `--yes`.

### Cancel always aborts cleanly

At any point in a multi-step install (action with multiple new connector dependencies), cancel is available. Cancel:

- Does not write the action file to `~/.aileron/actions/`
- Does not move any binaries from the temp staging area into the content-addressed store
- Does not create any binding state
- Does not modify the audit log beyond a `consent_cancelled` event for the partial flow

The user's local state is exactly as it was before the install command ran. Cancel is the safe-by-default exit from any consent flow.

## Alternatives Considered

### Per-capability granting (tiered consent) (rejected)

The install prompt shows each declared capability as a checkbox. The user can install with all, some, or no capabilities granted, and the connector runs only with the granted subset.

Rejected because it breaks the manifest-as-contract invariant. A connector's code is written against its declared interface — `wasi:http/outgoing-handler` is either available or it isn't, and code that needs it cannot defensively code around its absence at runtime. Tiered consent forces every connector to handle "what if my declared capability isn't actually granted," which (a) duplicates capability-checking logic into every connector, (b) creates a UX nightmare of "this works for some users but not others," and (c) historically has been worse for security than not having the option at all (see iOS, macOS, Android trajectory).

If the user finds a capability unacceptable, the right answer is to not install. The right place to express "this connector wants too much" is a code review of the manifest, not a runtime knob.

### Trust based on publisher signing alone (skip consent for signed connectors) (rejected)

If the binary is signed by a key on a configured allowlist, install proceeds without showing the consent prompt.

Rejected because signing tells the user *who* signed; it does not tell them *what* the binary will do. Two connectors signed by the same publisher can have wildly different capability footprints. Skipping consent for a "trusted" publisher means the user could install a connector that requests every credential scope, signed by the same key as a connector that requested none. The signature is information; the manifest is the contract; the consent is on the contract, not the signer.

A user who wants to skip consent for known-good artifacts has `--yes`. The user opts into that bypass per command, in the visible command line.

### Auto-install transitive dependencies without separate consent (rejected)

When `aileron action add` runs, the action's connector dependencies are installed automatically; the user only sees consent for the action itself.

Rejected because it inverts the trust model. A malicious action author could specify a malicious connector dependency; the user would consent to "this action" without realizing they were also consenting to the connector's manifest. Each new artifact entering the system requires its own consent. Stacking the prompts (rather than collapsing them) keeps every introduction visible.

### Defer consent to first use (rejected)

The install command is silent — bytes go to the local store with no consent. The first time the action is invoked, a consent prompt fires for both the action and its dependencies.

Rejected because by the time first use happens, the bytes are already on disk. A connector that the user would have rejected at install has already had a chance to be tampered with, fingerprinted, or analyzed. Consent must happen *before* the bytes commit to the system, not after.

### Skip consent for `hub://` artifacts (privileged source) (rejected)

Connectors and actions from the Aileron Hub are pre-vetted and skip the consent prompt; only `github://` and other sources require explicit consent.

Rejected because it would re-create the first-party / second-party tier hierarchy [ADR-0002](/adr/0002-connector-model) explicitly avoided. The Hub is one source among many. A `hub://aileron/slack` connector goes through the same consent flow as a `github://acme/stripe` connector. The Hub may surface higher-quality artifacts through curation and review, but it does not bypass user consent.

## Consequences

### For users

- Every install is a binary decision: install or cancel. There are no settings to tune, no per-capability checkboxes, no policy file to author.
- The install prompt shows everything relevant to the trust decision in one screen. Users who want to skim see the headline; users who want to verify see the detail.
- Updates show what changed, not a full re-review. Bug-fix updates are quick to approve; capability-adding updates are scrutinized.
- `--yes` exists for automation; it has to be explicit per command, every time.

### For action and connector authors

- The manifest is the contract being agreed to. A connector that asks for `oauth2(scope=*)` will see lower install rates than one that asks for narrower scopes; the prompt makes the cost legible.
- Update prompts surface every new capability. A connector author adding scopes between minor versions creates friction in user adoption — which is the right pressure on capability discipline.
- A connector that's signed and verified gets a green check at the top of the prompt; the rest of the prompt is identical to an unsigned connector. Signing is reassurance, not a gate.

### For Aileron runtime and CLI

- `aileron connector install` and `aileron action add` are the entry points to the consent flow. Both pause for prompt unless `--yes` is passed.
- The prompt is rendered through the user-channel mechanism (interactive TUI when attached to a terminal; OS-native dialog or web UI per [ADR-0009](/adr/0009-user-channel)).
- Hash mismatch and signature failure are hard fails before the prompt ever renders. Cancel after the prompt is a soft cancel — no state changes.
- Multi-step installs (action plus new connector deps) stack prompts; partial cancel aborts the entire chain.

### For the Hub

- The Hub's curation and reviews provide value through *what shows up in browse views*, not through bypassing consent. Surfacing well-reviewed connectors and surfacing poorly-reviewed ones is the Hub's job.
- The Hub may add review badges, install counts, and maintainer activity to its listings. None of that changes the install consent flow itself.

### For audit and security

- Every consent decision (install, cancel, update, --yes auto-approval) is logged with: artifact FQN, version, hash, signature status, declared capabilities at consent time, decision, audit ID.
- A user reviewing their audit log can answer: "what did I install, and what did I agree it could do?" without consulting any other source.
- An action's install event references the consent events for all of its connector dependencies; the chain is reconstructible.

### Open implementation questions (deferred)

- *Which surfaces does the consent prompt use (TUI, OS dialog, web UI), and how does the channel choice interact with the headless path?* — [ADR-0009](/adr/0009-user-channel).
- *What does the consent flow look like during agent-driven installs (the agent recommends adding an action; the user must approve)?* — partially user-channel, partially within the agent host integration; will be ratified once that integration takes shape.

## Examples

### Single-connector install

```
$ aileron connector install github://aileron/slack@1.2.0
Resolving github://aileron/slack@1.2.0...
  Tag: v1.2.0 on github.com/aileron/slack
  Fetching aileron.tar.gz...                ✓
  Verifying signature...                    ✓ (key matches publisher's release config)
  Computing hash...                         sha256:abc123...

╭─────────────────────────────────────────────────────────────╮
│  Install connector: github://aileron/slack@1.2.0            │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  Hash       sha256:abc123...                                │
│  Publisher  aileron (verified GitHub organization)          │
│  Signed     ✓ key matches publisher's release config        │
│                                                             │
│  This connector posts messages and reads channel info       │
│  from a Slack workspace.                                    │
│                                                             │
│  This connector will be allowed to:                         │
│                                                             │
│    Network                                                  │
│      • slack.com:443                                        │
│                                                             │
│    Credentials (you'll be asked to bind these later)        │
│      • OAuth2  scopes: chat:write, channels:read            │
│                                                             │
│    Runtime functions                                        │
│      • wasi:http/outgoing-handler                           │
│      • aileron:audit/emit                                   │
│                                                             │
╰─────────────────────────────────────────────────────────────╯

  [Install]  [Cancel]

> Install
✓ Stored at sha256:abc123...
```

### Action install with new connector dependencies

```
$ aileron action add hub://aileron/ship-update@1.0.0
Resolving hub://aileron/ship-update@1.0.0...
  Fetching template...                      ✓
  Verifying signature...                    ✓

This action depends on 2 connectors. You will review each:
  • github://aileron/slack@1.2.0   (not installed)
  • github://aileron/git@2.1.0     (not installed)

╭─── Step 1 of 3: action ─────────────────────────────────────╮
│  Add action: hub://aileron/ship-update@1.0.0                │
│                                                             │
│  Posts a "shipped" announcement to a Slack channel with     │
│  the merged PR link.                                        │
│                                                             │
│  This action exercises:                                     │
│    • github://aileron/slack@1.2.0  → chat:write,            │
│                                       channels:read         │
│    • github://aileron/git@2.1.0    → read                   │
│                                                             │
│  File will be written to: ~/.aileron/actions/ship-update.md │
╰─────────────────────────────────────────────────────────────╯

  [Continue]  [Cancel]
> Continue

╭─── Step 2 of 3: connector ──────────────────────────────────╮
│  Install connector: github://aileron/slack@1.2.0            │
│  ...                                                        │
╰─────────────────────────────────────────────────────────────╯

  [Install]  [Cancel]
> Install

╭─── Step 3 of 3: connector ──────────────────────────────────╮
│  Install connector: github://aileron/git@2.1.0              │
│  ...                                                        │
╰─────────────────────────────────────────────────────────────╯

  [Install]  [Cancel]
> Install

✓ Action written to ~/.aileron/actions/ship-update.md
✓ 2 connectors stored
```

### Update with capability diff

```
$ aileron connector update github://aileron/slack
Available: 1.2.0 → 1.3.0

╭─────────────────────────────────────────────────────────────╮
│  Update connector: github://aileron/slack                   │
│  1.2.0 → 1.3.0                                              │
│                                                             │
│  Capability changes:                                        │
│    + Network: slack.com:443/api/v2  (new in 1.3.0)          │
│    + Credentials: oauth2 scope:files:write  (new)           │
│      Unchanged: chat:write, channels:read                   │
│                                                             │
│  This update adds 1 new network destination and 1 new       │
│  credential scope.                                          │
╰─────────────────────────────────────────────────────────────╯

  [Update]  [Cancel]
> Update
✓ Updated. Action files referencing github://aileron/slack@1.2.0
  remain pinned; run `aileron connector check` to see what's
  available.
```

### Signature-failure hard fail

```
$ aileron connector install github://aileron/slack@1.2.0
Resolving github://aileron/slack@1.2.0...
  Tag: v1.2.0 on github.com/aileron/slack
  Fetching aileron.tar.gz...                ✓
  Verifying signature...                    ✗

ERROR: signature verification failed for github://aileron/slack@1.2.0
  The binary is signed, but by a key not authorized for the
  github://aileron source. This may indicate a supply-chain
  attack or a publisher-side configuration error.

The install was refused. Nothing was written to the local store.

If you believe this is a publisher-side error, contact the publisher
and confirm the signing-key configuration in their source repo.
```

### Headless install in CI

```
$ aileron sync --yes
Reading actions...                          ✓
Resolving 4 connectors...
  ✓ github://aileron/slack@1.2.0   (cached)
  ↓ github://aileron/git@2.1.0     fetching
  ↓ github://aileron/linear@0.4.2  fetching
  ✓ hub://aileron/gmail@1.5.0      (cached)

Auto-approved 2 new connectors (--yes):
  ✓ github://aileron/git@2.1.0     installed
  ✓ github://aileron/linear@0.4.2  installed

  All connectors verified, signed, and stored.

(Audit log: 2 consent_auto_approve events recorded.)
```
