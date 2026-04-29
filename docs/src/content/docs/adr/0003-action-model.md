---
title: "ADR-0003: Action Model"
description: "Atomic actions; depend on connectors with explicit version + hash + capability subsets; copied into the project on install"
order: 3
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

Connectors (ADR-0002) bring the ability to talk to a specific external service: an OAuth flow, an HTTP request shape, the right error handling. Actions are the layer above connectors — they describe a *purpose* the agent can fulfill. "Post a ship-update to Slack." "File a bug in Linear." "Send an email reply." Each action composes one or more connector operations into a unit the agent can invoke and the user can reason about.

The architectural questions for actions are different from those for connectors:

- Where does the action manifest *live*? In a registry the runtime fetches from, or in the developer's own repo?
- Who *owns* the action after it's installed — the publisher who wrote it, or the developer who installed it?
- How do actions *compose* — through an internal dependency graph, or by emergent agent orchestration?
- What does the action declare beyond "use this connector" — does it pin a capability subset, or inherit the connector's full grant?

These are not decisions about runtime mechanics; they are decisions about the contract between Aileron, the action author, the developer, and the agent. They shape what code lives where, what gets reviewed in PRs, and what surprises are possible at runtime.

This ADR ratifies that contract.

## Decision

### An action is a single declarative file owned by the developer

An action is a single file: TOML frontmatter declaring the contract, Markdown body documenting the action and serving as the LLM-facing function description (per ADR-0001). It lives in the developer's project, by convention under `actions/`, and is checked into git like any other source file.

```
project/
├── actions/
│   ├── ship-update.md
│   ├── reply-to-pm.md
│   └── file-bug.md
├── aileron.toml
└── src/
```

The action file is *the* contract Aileron executes. There is no separate manifest, no generated artifact, no lock file. Reading the file tells you exactly what will run, against which connector versions, with which capabilities. PR review is meaningful by construction — the diff *is* the change in execution.

### Actions are copied on install, not registered

`aileron action add ship-update` fetches a template from the Hub, *copies* it into the developer's project, and exits. From that moment the developer owns the file. They can edit it, restructure it, retitle it, change the connector versions it references, modify the prompts and triggers — and none of it requires anyone's permission or a republish to the Hub.

```
$ aileron action add ship-update
→ Fetching action 'ship-update' from Aileron Hub...
✓ Action file written to actions/ship-update.md
  Source: hub://aileron/ship-update@1.0.0
```

This is the ShadCN distribution model applied to actions: the Hub is a curated catalog of *starting-point templates*, not a runtime registry. Aileron does not phone the Hub at runtime to "load an action"; it reads the local file. A project can be reproduced from git alone, with no Hub access.

The action file records its provenance (`source = "hub://..."`) so update tooling can later offer to fetch a newer template version, but the local copy remains canonical until the developer accepts a diff.

### Actions are atomic — no inter-action dependencies

An action does one thing. It does not declare dependencies on other actions. There is no action-to-action import mechanism, no "extends," no shared lifecycle, no compositional language for chaining actions.

When a developer wants a compound operation — "ship-update + create-followup-ticket + block-calendar" — they have two options:

1. **Let the agent orchestrate.** The agent already calls actions; chaining `ship-update` then `create-followup-ticket` then `block-calendar` in conversation is its native mode. This is the default and almost always the right choice.
2. **Write a new action that performs all three.** The new action declares dependencies on `slack`, `linear`, and `gcal` connectors (rather than on three other actions) and orchestrates the connector calls itself.

Both options keep the dependency graph at depth 1: actions depend on connectors. Connectors depend on nothing inside Aileron.

This is a deliberate simplification, not a deferred decision. Action-to-action dependencies would introduce a transitive dependency graph (versioning, conflict resolution, lifecycle ordering, partial-failure semantics, debugging across action boundaries) for marginal value. The two existing composition paths cover the use cases without the graph.

### Action files declare connector dependencies and capability subsets

Each action file lists the connectors it uses. For each connector, it pins the exact version, the content hash (per ADR-0002), and the *subset* of the connector's declared capabilities that the action actually exercises:

```toml
[[requires.connectors]]
name = "slack"
version = "1.2.0"
hash = "sha256:abc123..."
capabilities = ["chat:write", "channels:read"]

[[requires.connectors]]
name = "git"
version = "2.1.0"
hash = "sha256:def456..."
capabilities = ["read"]
```

The `capabilities` field is the action's declared subset of what the connector is *capable* of doing. The Slack connector might also expose `chat:read`, `users:read`, and `files:upload`; if `ship-update` only needs `chat:write` and `channels:read`, that's all the action declares.

This is enforced at runtime. If `ship-update` at execution time tries to invoke `chat:read`, the runtime denies the call at the *action* boundary — even if the connector itself is capable of the operation. Two boundaries, two checks: the connector cannot exceed its manifest, the action cannot exceed its declared subset.

Defense in depth, with two practical benefits:

- **Audit is precise without reading the execution body.** A reviewer can scan the `[[requires.connectors]]` blocks at the top of the file and know exactly what the action will touch.
- **Capability creep is visible.** If a developer or upstream Hub update adds a new capability to an action, the change shows up as a TOML diff in the same file. There is no hidden expansion of what the action is allowed to do.

### The Markdown body is the documentation and the LLM-facing description

Per ADR-0001, the body of the action file is Markdown. It serves three readers simultaneously:

- The **developer** reading the action to understand what it does and how to maintain it.
- The **Hub** rendering the action as a documentation page when browsing.
- The **LLM**, which receives the body (or its first paragraph, or a designated section) as the `description` of the function when Aileron exposes the action as a tool to the agent.

Authors write one piece of prose. There is no separate "LLM hint" field to keep in sync with the human documentation.

### Updates are visible, never silent

When the Hub publishes a new version of `ship-update`, the developer's local file does not change. `aileron action update ship-update` fetches the new template and produces a diff against the local file. The developer accepts, rejects, or merges manually. Updates always go through git review.

Aileron will not silently update an installed action. There is no "auto-follow latest" mode. If an upstream connector publishes a security fix, the developer (or their tooling) must explicitly bump the connector version in the action files that reference it. The runtime will not switch transitively.

## Alternatives Considered

### Actions as a hosted catalog the runtime fetches at execution time (rejected)

The Hub serves actions as a live registry. The runtime fetches `slack/ship-update@1.0.0` at execution time and runs the freshly fetched code.

Rejected because it inverts the trust model. Runtime fetch means the action's behavior at any moment is whatever the Hub serves at that moment, with no local artifact to review. PR review becomes meaningless (the diff is the file path, not the code). Reproducibility from git is broken (the same commit produces different behavior across time as the Hub updates). And it introduces a hard runtime dependency on the Hub's availability and integrity. Local-first execution is non-negotiable for this trust profile.

### Actions as imported library functions (rejected)

Actions are functions exposed by a published library that the developer imports into application code. Aileron loads the library and invokes the functions.

Rejected because it removes the declarative property. An action defined in code is opaque to PR review (the reviewer must read the function body and reason about its behavior); it can take arbitrary runtime branches; it can read state outside its declaration. A declarative action file is auditable from its frontmatter alone — capabilities, connectors, and execution steps are all visible without running anything.

### Actions with inter-action dependencies (rejected)

Actions can declare `requires.actions = ["other-action@1.0.0"]` and Aileron resolves a dependency graph at install time, much like a package manager.

Rejected on a cost-benefit basis. The graph would require: version-range resolution, conflict detection between transitively-pulled versions, install ordering, lifecycle hooks for cleanup, partial-failure semantics across the graph, and a debug story when something goes wrong three levels deep. The benefit — reusable building blocks — is already addressed by either letting the agent compose actions in conversation or by writing a new action that exercises the underlying connectors directly. The cost-to-value ratio doesn't justify the complexity.

### Actions inherit the connector's full capability grant (rejected)

The action file declares which connectors it uses; the runtime lets the action invoke any operation the connector is *capable* of performing. The connector is the only enforcement boundary.

Rejected because it removes defense in depth. A bug or compromise in an action's execution path could exercise more of the connector's grant than intended without any second check. By requiring the action to also declare its capability subset, capability creep is *visible* (in the TOML diff) and *enforced* (at the action boundary). The cost — three lines of TOML per connector reference — is trivial.

### Built-in catalog of actions in Aileron core (rejected)

Aileron ships with first-party actions for common tasks ("send-email", "post-to-slack", "create-calendar-event") that developers don't need to install.

Rejected because it ossifies behavior at a layer that should remain flexible. A first-party `send-email` action implies one canonical authoring style, one default tone, one canonical set of trigger phrases. Real users want their actions to reflect their voice, their team's conventions, their project's preferences. The ShadCN model — install a template, then own and edit the file — is the right shape for this layer. The Hub provides starting points; developers customize.

## Consequences

### For developers

- Actions live in `actions/` (or wherever the project chooses) alongside the rest of the project's source. They are checked into git, reviewed in PRs, diffed like any other code.
- The set of actions a project can perform is fully determined by reading its action files. No hidden registries, no runtime resolution surprises.
- Customizing an installed action is just editing a file. The Hub's template is a starting point; the developer's copy is canonical from install onward.
- Compound operations are either composed in agent conversation or written as a new action. Either is a normal authoring activity, not a special framework feature.

### For action authors (publishing to the Hub)

- An action is a single file. Publishing is uploading that file to the Hub.
- Authors do not control how their action is used after install. The developer who runs `aileron action add` owns the resulting file outright.
- An action that wants new capability or a new connector dependency requires republishing a new template version. The developer chooses whether and when to apply the diff.

### For Aileron runtime

- The runtime reads action files from the local filesystem on startup (or on action invocation; either is acceptable). It does not phone home, does not consult a registry, does not authenticate to an external service to load an action.
- Capability enforcement runs at *both* boundaries: the connector's manifest grant (per ADR-0002) and the action's declared subset. A violation at either boundary terminates the call.
- The runtime parses the TOML frontmatter and extracts the Markdown body separately; the body becomes the LLM-facing function description when actions are surfaced to the agent.

### For the Hub

- The Hub is a curated catalog of action templates and connector binaries. It is *not* a runtime dependency.
- Action discovery, search, and browse happen on the Hub. Installation is a copy operation; the Hub is not consulted again until the developer asks for an update.
- The Hub validates action templates at publish time: TOML frontmatter parses; declared connectors exist at the named version+hash; declared capability subsets are valid against the connector's manifest; Markdown body parses.

### For composition and agent orchestration

- Compound flows are emergent from agent conversation. The agent calls action A, sees the result, decides whether to call action B. This is the natural mode.
- Authors who want a specific compound flow as a single tool write a new atomic action that exercises multiple connectors. There is no special "compose two actions" primitive.
- The atomicity rule keeps the dependency graph at depth 1 forever: actions → connectors → primitive capabilities. No transitive dependency resolution exists in the system.

### Open implementation questions (deferred to subsequent ADRs)

- *How does `aileron action add` resolve missing connector dependencies and walk the developer through bindings?* — deferred to the dependency-resolution and install-consent ADRs.
- *How does the runtime match agent intent to a specific installed action?* — deferred to the intent-matching ADR.
- *How does an action behave when one of its connector calls fails partway through?* — deferred to the failure-handling ADR.
- *How are action files synced across teammates with different credential bindings?* — deferred to the project-portability ADR.

## Examples

### A complete action file (`actions/ship-update.md`)

````markdown
+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "slack"
version = "1.2.0"
hash = "sha256:abc123..."
capabilities = ["chat:write", "channels:read"]

[[requires.connectors]]
name = "git"
version = "2.1.0"
hash = "sha256:def456..."
capabilities = ["read"]

[match]
intent = "tell team I shipped"

[[execute]]
id = "recent_merge"
connector = "git"
op = "read_recent_merge"

[[execute]]
id = "post"
connector = "slack"
op = "post_message"

[execute.inputs]
channel = "${args.channel}"
message = "${recent_merge.summary} → ${recent_merge.pr_url}"
+++

# Ship Update

Posts a "shipped" announcement to a Slack channel with the merged PR link.

## When it fires

Triggered when the user tells their agent things like:

- "tell team I shipped the migration"
- "post a ship update to #engineering"
- "let the team know I merged the PR"

## What it does

1. Reads the most recent merge commit from local git.
2. Extracts the PR URL from the commit body.
3. Formats a message and posts it to the specified Slack channel.
````

### Capability denial at the action boundary

`ship-update` declares `slack: capabilities = ["chat:write", "channels:read"]`. At runtime, an `[[execute]]` step attempts `slack.list_users`. The Slack connector's manifest *does* permit `users:read`, but the action did not declare it. The runtime denies the call at the action boundary before it reaches the connector:

```json
{
  "error": {
    "class": "capability_denied",
    "boundary": "action",
    "action": "ship-update@1.0.0",
    "connector": "slack@1.2.0",
    "requested": "users:read",
    "declared_subset": ["chat:write", "channels:read"],
    "audit_id": "audit-9c2a..."
  }
}
```

The action fails fast and visibly. The developer sees the boundary that denied it (action vs. connector), which makes the fix obvious: declare the capability if it's intended, or remove the call if it's not.

### Composing two actions

Compound flow: post a ship-update *and* file a follow-up ticket. Two paths:

**Agent-orchestrated (default).** The agent calls `ship-update`, observes the result, then calls `file-followup-ticket`. No new code is written.

**A new atomic action.** The developer writes `actions/ship-and-followup.md` declaring connectors for both Slack and Linear; the action's own `[[execute]]` steps perform the post and the ticket-creation. The new action does *not* declare a dependency on `ship-update` or `file-followup-ticket`. It exists as a sibling action, exercising the same connectors directly.

Either path keeps the action dependency graph flat.
