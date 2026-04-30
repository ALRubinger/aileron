---
title: "ADR-0003: Action Model"
description: "Atomic actions; depend on connectors with explicit version + hash + capability subsets; copied into the user's home directory on install"
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

Connectors ([ADR-0002](/adr/0002-connector-model)) bring the ability to talk to a specific external service: an OAuth flow, an HTTP request shape, the right error handling. Actions are the layer above connectors — they describe a *purpose* the agent can fulfill. "Post a ship-update to Slack." "File a bug in Linear." "Send an email reply." Each action composes one or more connector operations into a unit the agent can invoke and the user can reason about.

The architectural questions for actions are different from those for connectors:

- Where does the action manifest *live*? In a registry the runtime fetches from, or in the developer's own repo?
- Who *owns* the action after it's installed — the publisher who wrote it, or the developer who installed it?
- How do actions *compose* — through an internal dependency graph, or by emergent agent orchestration?
- What does the action declare beyond "use this connector" — does it pin a capability subset, or inherit the connector's full grant?

These are not decisions about runtime mechanics; they are decisions about the contract between Aileron, the action author, the developer, and the agent. They shape what code lives where, what gets reviewed in PRs, and what surprises are possible at runtime.

This ADR ratifies that contract.

## Decision

### An action is a single declarative file owned by the user

An action is a single file: TOML frontmatter declaring the contract, Markdown body documenting the action and serving as the LLM-facing function description (per [ADR-0001](/adr/0001-manifest-format)). It lives in the user's home directory at `~/.aileron/actions/`. Actions are personal capability extensions for the user, not project-coordination state.

```
~/.aileron/
├── actions/
│   ├── ship-update.md
│   ├── reply-to-pm.md
│   └── file-bug.md
├── store/
└── vault/
```

This is the same shape as user-level CLAUDE.md, shell aliases, or editor extensions: I install the actions I want for my own use; my teammates install whichever actions they want. There is no team-coordination layer in v1.

**Project-level actions are post-MVP.** A future ADR may introduce a `<project>/actions/` surface for teams that want to commit a shared action set to their repo. v1 does not implement that surface — actions are exclusively user-level. Defer until a concrete team asks for it.

The action file is *the* contract Aileron executes. There is no separate manifest, no generated artifact, no lock file. Reading the file tells you exactly what will run, against which connector versions, with which capabilities.

### Actions are copied on install, not registered

`aileron action add <FQN>` fetches a template from any supported source, *copies* it into the user's `~/.aileron/actions/`, and exits. From that moment the user owns the file. They can edit it, restructure it, retitle it, change the connector versions it references, modify the prompts and triggers — and none of it requires anyone's permission or a republish.

```
$ aileron action add hub://aileron/ship-update@1.0.0
→ Fetching action from hub://aileron/ship-update@1.0.0...
✓ Action file written to ~/.aileron/actions/ship-update.md
  Source: hub://aileron/ship-update@1.0.0

$ aileron action add github://acme/templates/actions/file-bug@2.1.0
→ Fetching action from github://acme/templates/actions/file-bug@2.1.0...
✓ Action file written to ~/.aileron/actions/file-bug.md
  Source: github://acme/templates/actions/file-bug@2.1.0
```

This is the ShadCN distribution model applied to actions: a source is a curated catalog of *starting-point templates*, not a runtime registry. Aileron does not phone the source at runtime to "load an action"; it reads the local file. A user's installed action set is reproducible from the action files in `~/.aileron/actions/` — they can be backed up, dotfiles-managed, or version-controlled in a personal repo, but they are not part of any project the user works on.

**Action templates are hostable from any scheme.** Actions and connectors are symmetric in their distribution model: both are FQN-identified, both can live on `github://`, `gitlab://`, `hub://`, or any future scheme the resolver knows. The Hub is a curated catalog with discoverability and review, not a privileged source. Publishers who prefer to host their own action templates on GitHub or GitLab can do so; install commands and provenance fields use the FQN of the chosen source.

Monorepo support carries over from [ADR-0002](/adr/0002-connector-model). A single repo can host multiple action templates at distinct subpaths: `github://aileron/integrations/actions/ship-update`, `github://aileron/integrations/actions/file-bug`. The Hub similarly allows organizational subpaths within an owner's namespace.

**Versioning inherits the rules from [ADR-0002](/adr/0002-connector-model).** Action templates are pinned to strict SemVer; the `source` field includes `@<version>`; the local action file records the exact source FQN+version it was copied from. The local copy is canonical from install onward; the source version is provenance, not a runtime dependency.

The action file records its provenance with a fully-qualified URI in the `source` field. Update tooling uses this to offer newer template versions when they become available, but the local copy remains canonical until the user accepts a diff.

Note the asymmetry between the action's *own* `name` and its references to other artifacts:

- The action's `name` field is a **bare local handle** (e.g., `name = "ship-update"`). The user owns the file post-install and chooses the name; FQN doesn't apply to a file the user owns.
- The `source` field is a **fully-qualified URI** indicating where the template came from.
- The `[[requires.connectors]]` blocks reference connectors by their **fully-qualified URI** (per [ADR-0002](/adr/0002-connector-model)), since connectors are shared, sandboxed binaries that need unambiguous identification across the ecosystem.

### Actions are atomic — no inter-action dependencies

An action does one thing. It does not declare dependencies on other actions. There is no action-to-action import mechanism, no "extends," no shared lifecycle, no compositional language for chaining actions.

When a user wants a compound operation — "ship-update + create-followup-ticket + block-calendar" — they have two options:

1. **Let the agent orchestrate.** The agent already calls actions; chaining `ship-update` then `create-followup-ticket` then `block-calendar` in conversation is its native mode. This is the default and almost always the right choice.
2. **Write a new action that performs all three.** The new action declares dependencies on the relevant connectors (e.g., `github://aileron/slack`, `github://aileron/linear`, `github://aileron/gcal`) rather than on three other actions, and orchestrates the connector calls itself.

Both options keep the dependency graph at depth 1: actions depend on connectors. Connectors depend on nothing inside Aileron.

This is a deliberate simplification, not a deferred decision. Action-to-action dependencies would introduce a transitive dependency graph (versioning, conflict resolution, lifecycle ordering, partial-failure semantics, debugging across action boundaries) for marginal value. The two existing composition paths cover the use cases without the graph.

### Action files declare connector dependencies and capability subsets

Each action file lists the connectors it uses. For each connector, it pins the fully-qualified URI name (per [ADR-0002](/adr/0002-connector-model)), the exact version, the content hash, and the *subset* of the connector's declared capabilities that the action actually exercises:

```toml
[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc123..."
capabilities = ["chat:write", "channels:read"]

[[requires.connectors]]
name = "github://aileron/git"
version = "2.1.0"
hash = "sha256:def456..."
capabilities = ["read"]
```

The `capabilities` field is the action's declared subset of what the connector is *capable* of doing. The Slack connector might also expose `chat:read`, `users:read`, and `files:upload`; if `ship-update` only needs `chat:write` and `channels:read`, that's all the action declares.

This is enforced at runtime. If `ship-update` at execution time tries to invoke `chat:read`, the runtime denies the call at the *action* boundary — even if the connector itself is capable of the operation. Two boundaries, two checks: the connector cannot exceed its manifest, the action cannot exceed its declared subset.

Defense in depth, with two practical benefits:

- **Audit is precise without reading the execution body.** Anyone reading the action file can scan the `[[requires.connectors]]` blocks at the top and know exactly what the action will touch.
- **Capability creep is visible.** If the user or an upstream template update adds a new capability to an action, the change shows up as a TOML diff in the same file. There is no hidden expansion of what the action is allowed to do.

### Action inputs are declared in `[[inputs]]` blocks

An action accepts call-time arguments — the values the agent (or the LLM) supplies when invoking the action. Each argument is declared in an `[[inputs]]` block:

```toml
[[inputs]]
name = "channel"
type = "string"
description = "Slack channel to post to (e.g. '#engineering')."

[[inputs]]
name = "max_lines"
type = "integer"
required = false
description = "Maximum lines of context to include in the post."
```

Fields:

- `name` — the argument's identifier. Must match `^[a-z][a-z0-9_]*$`. Referenced in `[[execute]]` step inputs as `${args.<name>}`.
- `type` — one of `string`, `integer`, `number`, `boolean`. Maps directly to the corresponding JSON Schema primitive.
- `required` — optional, defaults to `true`. Set to `false` to make the argument optional.
- `description` — required. Becomes the field-level description the LLM sees when the action is exposed as a tool.

The `[[inputs]]` block is the contract between the action and the LLM (per [ADR-0008](/adr/0008-intent-matching)). When Aileron augments the agent's tool catalog with an action, it derives a JSON Schema `parameters` object directly from the inputs:

```json
{
  "type": "object",
  "properties": {
    "channel":   { "type": "string",  "description": "Slack channel to post to..." },
    "max_lines": { "type": "integer", "description": "Maximum lines of context..." }
  },
  "required": ["channel"]
}
```

The shape is what the LLM uses to choose arguments at tool-call time. Authors who want the LLM to pick the right values write tight `description` prose and pick the narrowest type that fits; the LLM does the rest.

Validation runs at parse time:

- Every `[[execute]]` step's `${args.<name>}` reference must resolve to a declared input.
- An action with no `[[inputs]]` and no `${args.*}` references is valid; its `parameters` schema is the empty object `{ "type": "object" }`.
- Duplicate input names are rejected.

### The Markdown body is the documentation and the LLM-facing description

Per [ADR-0001](/adr/0001-manifest-format), the body of the action file is Markdown. It serves three readers simultaneously:

- The **user** reading the action to understand what it does and how to maintain it.
- The **Hub** rendering the action as a documentation page when browsing.
- The **LLM**, which receives the body (or its first paragraph, or a designated section) as the `description` of the function when Aileron exposes the action as a tool to the agent.

Authors write one piece of prose. There is no separate "LLM hint" field to keep in sync with the human documentation.

### Updates are visible, never silent

When the source publishes a new version of `ship-update`, the user's local file does not change. `aileron action update ship-update` fetches the new template (from the FQN recorded in `source`) and produces a diff against the local file. The user accepts, rejects, or merges manually.

Aileron will not silently update an installed action. There is no "auto-follow latest" mode. If an upstream connector publishes a security fix, the user must explicitly bump the connector version in the action files that reference it. The runtime will not switch transitively.

## Alternatives Considered

### Actions as a hosted catalog the runtime fetches at execution time (rejected)

The source (Hub, GitHub, or any other) serves actions as a live registry. The runtime fetches the action FQN at execution time and runs the freshly fetched code.

Rejected because it inverts the trust model. Runtime fetch means the action's behavior at any moment is whatever the source serves at that moment, with no local artifact to review. The user cannot read the file they have to know what will run; they have to trust the source served the same bytes they last reviewed. And it introduces a hard runtime dependency on the source's availability and integrity. Local-first execution is non-negotiable for this trust profile.

### Actions as imported library functions (rejected)

Actions are functions exposed by a published library that the developer imports into application code. Aileron loads the library and invokes the functions.

Rejected because it removes the declarative property. An action defined in code is opaque (the reader must read the function body and reason about its behavior); it can take arbitrary runtime branches; it can read state outside its declaration. A declarative action file is auditable from its frontmatter alone — capabilities, connectors, and execution steps are all visible without running anything.

### Actions with inter-action dependencies (rejected)

Actions can declare `requires.actions = ["other-action@1.0.0"]` and Aileron resolves a dependency graph at install time, much like a package manager.

Rejected on a cost-benefit basis. The graph would require: version-range resolution, conflict detection between transitively-pulled versions, install ordering, lifecycle hooks for cleanup, partial-failure semantics across the graph, and a debug story when something goes wrong three levels deep. The benefit — reusable building blocks — is already addressed by either letting the agent compose actions in conversation or by writing a new action that exercises the underlying connectors directly. The cost-to-value ratio doesn't justify the complexity.

### Actions inherit the connector's full capability grant (rejected)

The action file declares which connectors it uses; the runtime lets the action invoke any operation the connector is *capable* of performing. The connector is the only enforcement boundary.

Rejected because it removes defense in depth. A bug or compromise in an action's execution path could exercise more of the connector's grant than intended without any second check. By requiring the action to also declare its capability subset, capability creep is *visible* (in the TOML diff) and *enforced* (at the action boundary). The cost — three lines of TOML per connector reference — is trivial.

### Built-in catalog of actions in Aileron core (rejected)

Aileron ships with first-party actions for common tasks ("send-email", "post-to-slack", "create-calendar-event") that developers don't need to install.

Rejected because it ossifies behavior at a layer that should remain flexible. A first-party `send-email` action implies one canonical authoring style, one default tone, one canonical set of trigger phrases. Real users want their actions to reflect their voice and their preferences. The ShadCN model — install a template, then own and edit the file — is the right shape for this layer. Sources (Hub, GitHub, GitLab, etc.) provide starting points; users customize.

## Consequences

### For users

- Actions live in `~/.aileron/actions/`, alongside the user's other Aileron state (vault, store, audit). They are personal capability extensions, not project-coordination state.
- The set of actions the user has installed is fully determined by reading the files in `~/.aileron/actions/`. No hidden registries, no runtime resolution surprises.
- Customizing an installed action is just editing a file. The source's template is a starting point; the user's copy is canonical from install onward.
- Compound operations are either composed in agent conversation or written as a new action. Either is a normal authoring activity, not a special framework feature.
- Users who want to back up or sync their actions across machines can put `~/.aileron/actions/` in a personal dotfiles repo. That's a personal workflow choice, not an Aileron concern.

### For action authors

- An action is a single file. Publishing is making that file available at an FQN — pushing to a GitHub release, a GitLab release, the Aileron Hub, or any future scheme the resolver knows.
- Publishers choose where to host. The Hub offers discoverability, search, and curation. Self-hosting on `github://` or `gitlab://` keeps the publisher in full control of release cadence and access policy.
- Authors do not control how their action is used after install. The user who runs `aileron action add` owns the resulting file outright.
- An action that wants new capability or a new connector dependency requires republishing a new template version. The user chooses whether and when to apply the diff.

### For Aileron runtime

- The runtime reads action files from `~/.aileron/actions/` on startup (or on action invocation; either is acceptable). It does not phone home, does not consult a registry, does not authenticate to an external service to load an action.
- Capability enforcement runs at *both* boundaries: the connector's manifest grant (per [ADR-0002](/adr/0002-connector-model)) and the action's declared subset. A violation at either boundary terminates the call.
- The runtime parses the TOML frontmatter and extracts the Markdown body separately; the body becomes the LLM-facing function description when actions are surfaced to the agent.

### For sources (Hub, GitHub releases, GitLab releases, etc.)

- A source is a curated catalog of action templates and connector binaries that publishers push to. It is *not* a runtime dependency.
- Action discovery, search, and browse happen on the source's surface (the Hub provides this natively; GitHub/GitLab provide it through their own browse and search). Installation is a copy operation; the source is not consulted again until the user asks for an update.
- The Hub validates action templates at publish time: TOML frontmatter parses; declared connectors exist at the named FQN+version+hash; declared capability subsets are valid against the connector's manifest; Markdown body parses. Other schemes do not enforce validation server-side; install tooling validates locally before writing to the user's actions directory.

### For composition and agent orchestration

- Compound flows are emergent from agent conversation. The agent calls action A, sees the result, decides whether to call action B. This is the natural mode.
- Authors who want a specific compound flow as a single tool write a new atomic action that exercises multiple connectors. There is no special "compose two actions" primitive.
- The atomicity rule keeps the dependency graph at depth 1 forever: actions → connectors → primitive capabilities. No transitive dependency resolution exists in the system.

### Open implementation questions (deferred)

- *How does `aileron action add` resolve missing connector dependencies and walk the user through bindings?* — deferred to [ADR-0004](/adr/0004-dependency-resolution) and [ADR-0007](/adr/0007-install-consent).
- *How does the runtime match agent intent to a specific installed action?* — deferred to [ADR-0008](/adr/0008-intent-matching).
- *How does an action behave when one of its connector calls fails partway through?* — deferred to [ADR-0010](/adr/0010-failure-handling).
- *Will Aileron support a project-level action surface (`<project>/actions/`) for teams that want to commit shared action sets?* — deliberately deferred. v1 is user-level only. The shape of any future project-level surface (precedence rules, merging behavior, conflict resolution) will be ratified when concrete teams ask for it.

## Examples

### A complete action file (`~/.aileron/actions/ship-update.md`)

````markdown
+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc123..."
capabilities = ["chat:write", "channels:read"]

[[requires.connectors]]
name = "github://aileron/git"
version = "2.1.0"
hash = "sha256:def456..."
capabilities = ["read"]

[match]
intent = "tell team I shipped"

[[inputs]]
name = "channel"
type = "string"
description = "Slack channel to post the announcement to (e.g. '#engineering')."

[[execute]]
id = "recent_merge"
connector = "github://aileron/git"
op = "read_recent_merge"

[[execute]]
id = "post"
connector = "github://aileron/slack"
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

`ship-update` declares `github://aileron/slack` with `capabilities = ["chat:write", "channels:read"]`. At runtime, an `[[execute]]` step attempts `slack.list_users`. The Slack connector's manifest *does* permit `users:read`, but the action did not declare it. The runtime denies the call at the action boundary before it reaches the connector:

```json
{
  "error": {
    "class": "capability_denied",
    "boundary": "action",
    "action": "ship-update@1.0.0",
    "connector": "github://aileron/slack@1.2.0",
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

**A new atomic action.** The user writes `~/.aileron/actions/ship-and-followup.md` declaring connectors for both Slack and Linear; the action's own `[[execute]]` steps perform the post and the ticket-creation. The new action does *not* declare a dependency on `ship-update` or `file-followup-ticket`. It exists as a sibling action, exercising the same connectors directly.

Either path keeps the action dependency graph flat.
