---
title: "The Deterministic Layer for AI Agents"
description: "Strategy, architecture, and business model for the Aileron pivot"
order: 0
---

**Aileron is the deterministic execution layer for AI agents. Whether the agent invokes an action via standard tool-calling or Aileron matches unambiguous intent before the LLM runs, the execution is the same: capability-isolated, credentials sealed in a vault the LLM never touches, with tamper-resistant approval for consequential actions. When the LLM is genuinely required, Aileron routes to the cheapest model that meets quality on the user's hardware.**

It is two layers:

- **Aileron Runtime** — a transparent OpenAI-compatible endpoint that intercepts every agent request. Aileron augments the agent's tool catalog with installed actions; the LLM selects what to call; Aileron executes the deterministic ones in capability-isolated sandboxes. For unambiguous intents, Aileron can bypass the LLM entirely. When no action applies, Runtime orchestrates inference engines per machine and routes to the cheapest model that meets the developer's quality bar.
- **Aileron Control** — governance, vault, policy, approvals, and audit applied throughout, in the request path by construction.

These layers are unified by architecture, not marketing. The LLM endpoint is the only seam in the agent stack where you can guarantee, simultaneously: zero SDK integration, credentials never leaked to the model, deterministic action execution regardless of how the LLM coordinated the call, tamper-resistant user consent for consequential actions, and a complete audit trail. Every other interception point has a structural compromise.

> **Companion documents:** for concrete hero use cases and the developer experience, see [What Your Agent Can Now Do](/pivot/what-your-agent-can-do). For an at-a-glance contrast with adjacent approaches, see [Aileron vs Tool Calling vs MCP](/pivot/tool-calling-mcp-comparison) and the deeper [Competitive Landscape](/pivot/competitive-landscape). This document covers the strategy, architecture, and business model.

---

## The Architectural Insight

Every agent in production speaks `chat/completions` (or its successor). That endpoint is the most concentrated decision point in the stack: it is where intent is expressed, where a model is asked to act, where credentials would be exposed if anyone were careless enough to put them there, and where probabilistic execution begins.

Compare the available interception points:

| Seam | What it can do | Compromise |
|---|---|---|
| Pre-tool-use hooks (Claude Code, Cursor) | Rewrite, block, or approve tool calls | LLM has already emitted the call; cost paid, intent shaped |
| MCP server | Expose tools to the LLM | LLM still orchestrates; LLM in the loop |
| Agent SDK middleware | Wrap framework operations | Requires SDK cooperation; framework-specific |
| Sandboxed agent process | Intercept system calls | Cannot read LLM-shaped intent |
| Post-LLM gateway | Filter, route, observe | LLM has run; cost and latency already paid |
| **LLM endpoint substitution** | **Run a deterministic action in place of the LLM call, or execute the LLM's tool calls under capability isolation** | **None — invisible to the agent at the boundary that matters** |

This is the seam where Aileron makes deterministic execution possible. The LLM may still select what to call (via standard function calling); Aileron ensures the *execution* is deterministic, sandboxed, audited, and tamper-resistant. For unambiguous intents, Aileron can also bypass the LLM entirely. Either way, the agent's tool calls land in a structurally safer place than what any current architecture provides.

---

## The Problem

### Agents are probabilistic where they should be deterministic

When an agent says "send the invoice to acme@example.com for $4,200," there is one correct outcome. The current architecture runs that intent through a stochastic model that may hallucinate the recipient, the amount, or the action itself. The model is asked to make decisions a deterministic function should make. Production agents accumulate this debt every turn.

### LLMs touch credentials they should never see

To execute real-world actions, agents pass API keys, tokens, and secrets through the LLM context — sometimes inadvertently, often unavoidably. The model could leak them in a response, log them in a trace, or return them as part of a tool argument it composed. "LLM never sees the key" is now table stakes (1Password, HashiCorp Vault, NVIDIA OpenShell all ship it), but every solution requires the agent author to opt in.

### The agent itself is in the trust path for consent

Approvals, denials, and configuration changes all flow through the agent UI today. A buggy or compromised agent can rewrite a denial as an approval, substitute one action's ID for another, or fabricate user consent that never happened. Existing tool-execution layers — gateways, MCP servers, agent frameworks — accept this risk because they have no separate channel for user intent. Aileron does.

### Cost and latency are paid even when the work is deterministic

A 50-step agent run sends every step through inference, including the steps where the answer is computable, the format is fixed, and the procedure is settled. The trivial turns and the hard turns pay the same token cost and incur the same network latency. Typical tool-call-heavy agents see 3–5x cost overhead from over-provisioning; substitution-dominant workloads can see 10x or more. The categorical wins are bigger still — substituted actions skip inference entirely, cutting end-to-end latency from seconds to milliseconds and producing identical results across runs.

### Audit and compliance can't keep up with probabilism

Regulated industries — finance, healthcare, legal — cannot deploy agents whose actions are non-reproducible. The same input produces different outputs across runs. The same prompt produces different tool invocations. There is no audit trail because there is no determinism to audit.

### Governance products require integration the agent author doesn't perform

Cerbos, Permit.io, Lakera, Pangea, Galileo all require SDK integration. The agent author is asked to be the safety engineer. Most aren't, and most won't be. The governance layer needs to be invisible to the agent — sitting in the path the agent already uses — or it doesn't get installed at all.

---

## What Aileron Is

### Aileron Runtime — interception, substitution, and routing in one process

Aileron Runtime is one process running locally. The agent points at `http://localhost:8721/v1` thinking it is talking to an LLM. Behind that endpoint, Runtime does three things in sequence on every request:

**1. It augments the agent's tool catalog with installed actions and matches against `ACTIONS.md`.**

Authors declare deterministic actions in a manifest:

```yaml
# actions/send_invoice.yaml
match:
  intent: "send invoice"
  required_args: [customer_id, amount]
execute:
  steps:
    - lookup: { source: crm, key: customer_id }
    - render: { template: invoice_email.tmpl }
    - send: { service: gmail, credential: aileron://vault/gmail }
returns:
  format: chat.completion
```

Aileron exposes installed actions to the LLM as functions the LLM can call. When the LLM calls one, Aileron executes the action deterministically — same inputs, same outputs, every time. For high-confidence intents, Aileron can also bypass the LLM entirely and execute the action directly.

`ACTIONS.md` is the primitive. It composes like Anthropic's Skills — declarative, file-based, version-controlled, shareable — but at the seam where the LLM lives, with deterministic execution semantics. A community ecosystem of actions becomes possible: Stripe, Salesforce, Gmail, Google Calendar, GitHub, Postgres, and the long tail of services agents touch every day.

**2. It orchestrates inference engines when no action matches.**

Runtime profiles the hardware, benchmarks available engines (Ollama, llama.cpp, MLX, vLLM, others as they emerge), and learns which engine + model + quantization performs best on this specific machine. Over time it builds a per-machine performance profile that improves with use. The user does not pick the engine. Does not pick the quantization. Does not tune.

This is structurally different from Ollama. Ollama is in the inference-engine business — they wrap llama.cpp and optimize that one experience. Routing across competing engines, including their own, is a conflict of interest they cannot resolve. Aileron is the layer above. We do not run inference; we decide who does.

**3. It routes per request.**

Runtime decides per request whether to run locally, route to a small cloud model, or escalate to a frontier model. Trivial turns run free on local hardware. Moderate turns route to small cloud models. Frontier models are reserved for the slice of work that genuinely requires them.

The honest claim: Aileron does not promise frontier quality on a laptop. Aileron promises **the cheapest model that meets the developer's quality bar**.

### Aileron Control — governance that comes for free

Because Aileron Runtime is in the request path by construction, governance attaches without integration:

- **Identity and credential vault.** Keys, tokens, secrets — held by Aileron, never reach the LLM, scoped per action.
- **Policy enforcement.** Deterministic rules about what agents can do, with whom, under what conditions, applied before any action or LLM call executes.
- **Tamper-resistant approvals.** When an action requires human confirmation, Aileron prompts via system notification, OS biometric prompt, dedicated TUI panel, or web UI — never through the agent UI. The user's consent travels on a channel the agent cannot reach. (No competitor offers approval flows that are structurally protected from agent mediation.)
- **Audit and execution.** Every action execution is deterministic and reproducible. Every LLM call is logged with routing decisions. Every approval is recorded with the surface it came from.

---

## How Aileron Works

The architectural decisions below will be ratified as ADRs. They're sketched here so the strategic choices are visible up front: Aileron core ships small, knows nothing about specific systems, and treats every connector as untrusted code with declared, enforced capabilities.

### The connector model

Aileron core ships only the *primitive capability types* — "network host:port," "vault credential of kind X," "host function Y" — not specific connectors. Gmail, Slack, GitHub, Stripe — these are connectors that arrive as separately distributed code, signed by their publishers, declaring what they need. Aileron core never has built-in knowledge of "Gmail" or "Slack."

Each connector ships a manifest declaring its needs:

```toml
[connector]
name = "gmail"
version = "1.2.3"
publisher = "acme.dev"
provenance_hash = "sha256:abc123..."

[capabilities.network]
hosts = ["gmail.googleapis.com:443", "oauth2.googleapis.com:443"]

[capabilities.credential]
kind = "oauth2"
scope = "https://www.googleapis.com/auth/gmail.send"

[capabilities.runtime]
imports = ["wasi:http/outgoing-handler", "wasi:cli/stdout"]

[provides]
intents = ["send_email", "draft_email"]
```

The manifest is a *request*. The runtime grants nothing not declared in it.

### The capability model: types, not paths

The connector cannot name a vault path. It declares an abstract requirement ("an OAuth2 credential with this scope"), and the user binds the requirement to a concrete vault entry at install time:

```
Connector "gmail" requests an OAuth2 credential with scope:
  https://www.googleapis.com/auth/gmail.send

You have:
  ▸ gmail/work        (alr@workplace.com)
  ▸ gmail/personal    (alr@home.com)
  ▸ Add new account…

Bind to: [gmail/work]
```

This is structurally important. A malicious connector cannot even *name* a key it shouldn't reach. It declares an abstract need; the user binds the specific resource. The Stripe connector requesting an OAuth2 scope of `gmail.send` would either fail (no matching credential exists) or be visibly wrong to the user.

This pattern matches Android's `ContentProvider` model, iOS privacy entitlements, and macOS TCC. Well-trodden ground.

### The action model

Actions are the composable units developers work with. Each action is a declarative manifest that describes what intent it matches, which connectors it uses, and what those connectors do during execution.

Actions are **atomic and do not depend on each other.** If a developer wants a compound operation — "ship-update + create-followup-ticket + block-calendar" — they either let the agent orchestrate the actions in sequence (the natural mode) or write a *new* action that performs all three using connectors. Action-to-action dependencies are deliberately not modeled. They open a dependency-graph problem unnecessary for the value we're after.

Actions **do depend on connectors**, with explicit version ranges and capability subsets:

An action file lives at `actions/ship-update.md` and is yours to evolve once installed:

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

The TOML frontmatter is the contract Aileron executes; the Markdown body is human-facing documentation that doubles as the function description when this action is surfaced to the LLM as a tool.

Three things make this declaration the source of truth:

**1. Declared capabilities are enforced at runtime.** If `ship-update` declares `slack: chat:write` but at runtime tries to call `chat:read`, Runtime refuses. The connector might be capable of `chat:read`, but the action didn't declare it. Capability creep is blocked at the action boundary, not just the connector boundary. Defense in depth.

**2. Audit becomes precise.** A reviewer reading the action manifest knows exactly what capabilities the action uses, without reading the execution body. Reviews stay tractable.

**3. Install resolution becomes deterministic.** When a developer runs `aileron action add ship-update`, Aileron resolves the dependency graph in a single bundled consent moment, not a series of surprise prompts at first invocation:

```
$ aileron action add ship-update
→ Fetching action 'ship-update' from Aileron Hub...
✓ Action file written to actions/ship-update.md
  Declares connectors: slack@1.2.0, git@2.1.0

This action requires:
  ✓ slack 1.2.0        (installed)
    Capabilities:
      ✓ chat:write     (already granted)
      ✗ channels:read  (NOT granted to slack/work)
  ✗ git 2.1.0          (not installed)

Resolving:
  → Re-bind slack connector to grant 'channels:read'?
  → Install missing connector 'git' (hash verified)?

[Continue]  [Customize…]  [Cancel]
```

There is no separate lock file. The action file itself is the contract — version pins, content hashes, declared capabilities, and execution steps, all in one place, owned by the developer in their git repo. Runtime verifies these constraints on every action invocation before execution begins. If any check fails, the action fails fast with a precise error rather than crashing mid-execution.

**Action files are owned, not installed.** When `aileron action add ship-update` runs, the action file is *copied* into the developer's project and tracked by git. From that moment forward, the developer owns the file: customize it to fit project conventions, modify the connector versions, refine the templates, evolve it as the project evolves. The Aileron Hub is a curated catalog of starting-point templates; installation is a copy operation, not a runtime dependency. This follows the ShadCN distribution model — the right pattern for declarative source code that wants to live alongside the developer's other source files.

### Connectors, not capabilities

Actions bind to specific connectors (`slack@1.2.0`), not to abstract capabilities (`messaging:post_to_channel`). This is a deliberate simplification, not a deferred decision. Capability abstraction would add standardization governance (who defines `messaging:post_to_channel`?), parser complexity, a second trust layer (trust the spec *and* trust any implementation), and UX ambiguity ("I want messaging" vs "I want Slack") in exchange for marginal substitutability benefit.

Aileron does not include capability abstraction. Action authors name the connector they want; if they need to swap implementations, they edit the action file. The ShadCN distribution model makes this trivial — the action file is theirs to evolve.

### How intent matches to actions

Aileron uses two mechanisms in tandem to translate user intent into action execution.

**Primary: tool augmentation via function calling.** Modern agents (Claude Code, Cursor, Copilot, ChatGPT, etc.) speak function calling — they construct chat completion requests with a `tools` array describing available functions, and the LLM decides which (if any) to call. Aileron leverages this directly: on every incoming chat completion request, Aileron augments the agent's `tools` array with installed actions, translated into function definitions:

```yaml
# An action's match clause becomes a function description the LLM sees:
- name: ship-update
  description: "Post a 'shipped' update to a Slack channel with the merged PR link"
  parameters:
    type: object
    properties:
      channel: { type: string }
```

The augmented request goes to whichever LLM Runtime's per-request router selected. The LLM does the categorization — picking when to invoke an action and what arguments to pass. Aileron then executes the deterministic ones with capability isolation; agent-defined tools (like `bash` or `file_read`) pass through unchanged.

This division leverages the strongest available intent-matcher — the LLM that's already processing the request — without adding inference cost or probabilism to the matching step. Selection is what LLMs are good at; execution is where determinism matters. Aileron stops competing with the LLM at categorization and instead focuses on what makes its execution layer different: capability isolation, vault-held credentials, deterministic outcomes, audit trails, and tamper-resistant approval.

**Secondary: pre-LLM bypass for clear intents.** For high-confidence patterns, Aileron can short-circuit the LLM call entirely. The action manifest's `match` clause can declare deterministic patterns:

```yaml
match:
  type: pattern
  patterns:
    - "(?i)post (?:an? )?ship update to #(\\w+) for (.+)"
```

When Aileron detects a clear pattern match in the user's last turn, it executes the action directly with extracted arguments — no LLM round-trip. Saves cost and latency for clear intents. Disabled by default; opt-in per action or globally.

**Tool name collisions.** Agent-defined tools take precedence; Aileron actions with conflicting names are renamed with a namespace prefix (`aileron.ship_update`) and the developer is notified. The agent never sees two tools with the same name.

**The agent visibility property.** Because Aileron exposes installed actions as tools to the LLM, the agent is naturally aware of what's available — they appear in the tool catalog. The architectural property the doc claims is not "the agent doesn't know actions exist" but rather: **the agent's tool calls have superpowers it can't perceive.** The agent sees a function called `send_invoice` and treats it like any other tool. What it doesn't see: `send_invoice` is executed by sandboxed code, with credentials the LLM never touched, against a real Stripe API call, with an immutable audit record, and with consent the agent itself cannot tamper with. The visible surface looks like a normal tool call. The execution semantics are categorically different.

### Install consent: one path

There is no tiered installation flow. Every connector and every action installs through the same path. The user sees the publisher identity, the signature status, and the full capability declaration; the user either clicks Install or Cancel. There is no option to selectively deny capabilities — the contract installs whole or not at all.

This is a deliberate simplification. Partial-install state — where a connector is installed but missing some of the capabilities it declared — is a source of ambiguity: the connector might or might not work, the action might or might not run, errors become confusing. By removing the option to selectively deny, Aileron preserves a clear invariant: an installed connector has all of its declared capabilities; an installed action has all of its declared connector dependencies; if not, it isn't installed.

Publisher identity and signature verification still matter — they appear in the install consent UI so the user can decide whether to install at all. A connector signed by a known publisher whose key Aileron has verified shows that signature; an unsigned connector from an unknown source shows "UNSIGNED" prominently. Verification is information shown to the user, not a different UX flow.

The Hub may use signing and verification status to organize and rank entries (verified publishers may appear higher; unsigned entries may carry a warning badge in browse views). But once the user chooses to install, the consent flow is the same regardless: show what you're getting, get explicit user approval, install in full or not at all.

### The install moment

At install, Aileron presents a single readable summary:

```
Installing: gmail v1.2.3
Publisher: acme.dev  (signature verified)

This connector requests:

  Network
    • gmail.googleapis.com:443
    • oauth2.googleapis.com:443

  Credentials (bound at first-use)
    • An OAuth2 token (scope: gmail.send)

  Capabilities
    • Outbound HTTP
    • No filesystem access
    • No environment access

  Provides actions: send_email, draft_email

[Install]  [Cancel]
```

The user installs the contract as declared, or cancels. There is no partial install; there is no per-capability customization. This preserves the invariant that an installed artifact carries all of its declared capabilities — no ambiguity, no half-state.

### Capability binding UX: one auth path, triggered on demand

Five architectural commitments shape how users authenticate and bind credentials. The detailed prompt sequences, multi-account UX, and conflict-resolution flows defer to the binding-UX ADR.

**1. Bindings are always explicit user actions.** Aileron never silently associates a credential with a capability. The user confirms at the moment a binding is created or modified.

**2. Capability *types* surface at install (transparency only).** When a connector or action is installed, Aileron shows what capabilities will be requested ("this connector will want OAuth2 access with scope `chat:write`"). No browser opens, no binding happens — this is just transparency about what the user is committing to allow later.

**3. Capability *bindings* surface at first-use.** The actual OAuth dance and credential binding run when the action first needs the credential. This is the same code path that handles credential refresh after expiration or revocation. One auth flow, triggered whenever credentials are needed and missing or expired, regardless of whether it's the first time or the hundredth. Consistency is the win.

**4. Pre-binding is opt-in for users who want it.** `aileron binding setup` (or `aileron sync --bind-now`) runs the auth flow eagerly for everything the project needs. Power users can pre-authenticate everything. Headless and autonomous workflows require this — non-interactive runs cannot complete OAuth flows mid-execution, so credentials must be pre-bound or federated through Control.

**5. Bindings are managed visibly.** `aileron binding list`, `aileron binding inspect`, and `aileron binding rebind` give the user direct control. Bindings are observable, replaceable, and removable on demand.

The detailed UX — exact prompt sequences, what happens when a binding name collides with an existing one, how multi-account workflows look, the precise behavior of `aileron sync --bind-now` in mixed-state projects — defers to the binding-UX ADR. These five commitments set the architectural posture; the ADR fills in the operational details with concrete implementation experience.

### Two distribution mechanics: binaries and files

Aileron uses two distinct distribution models for two different kinds of artifact, each suited to its purpose:

- **Connectors are binaries.** Sandboxed WASM modules with capability declarations and signed publisher hashes. They live in a content-addressed store outside any specific project (similar to Docker's image cache or Cargo's crate cache). Action files reference them by name + exact version + hash. When an action loads, Aileron verifies the connector binary in the store matches the declared hash before executing.

- **Actions are declarative source files.** They get copied into the developer's project on install, live in `actions/` (or wherever the project chooses), are tracked by git, and become the developer's own to evolve. The Hub serves them as starting-point templates; once installed they're indistinguishable from action files the developer wrote themselves.

This is the ShadCN model for actions plus a content-addressed store for connectors. There is no project-level lock file — each action file declares its own dependencies (connector name + exact version + hash) and is therefore self-describing. Reproducibility comes from git.

The CLI surface follows from this:

- **`aileron connector install slack@1.2.0`** — fetches the binary into the local connector store, verifies the hash and signature, runs the install-time consent flow, registers capability bindings.
- **`aileron action add ship-update`** — fetches the action template from the Hub, copies it into `actions/ship-update.md`, resolves any missing connector dependencies, prompts for capability bindings. From that moment on, the developer owns the file.
- **`aileron sync`** — reads the action files in the project, installs missing connectors (verifying hashes), prompts for unbound credentials. Standard `npm ci`-shape, but driven by the action files themselves.

Update tooling assists where useful, always editing action files visibly:

- **`aileron action update <name>`** — fetches the latest version of an action from the Hub and generates a diff against the local file. The developer accepts or rejects via git review.
- **`aileron connector check`** — scans all action files, lists available updates for any connector referenced anywhere.
- **`aileron connector update <connector>`** — bumps a connector reference (name + version + hash) across all action files that use it, after explicit confirmation.
- **`aileron action audit`** — lists every action and connector the project uses, every declared capability, every binding identity. Single command, full picture.

Updates always show up as git diffs. Nothing happens silently.

### Sandboxing and runtime enforcement

Connectors run in a WASM sandbox by default — capability-based isolation, language-agnostic, fast startup, deterministic execution. Aileron's vault sits in the host process; credentials are issued to connectors as short-lived scoped tokens per call, never as long-term keys.

The sandbox guarantees:

- Connector A cannot read Connector B's memory.
- Connector A cannot dial network hosts not in its grant.
- Connector A cannot access vault entries outside its bound credentials.
- Capability requests at runtime not in the install grant are denied at the WASM boundary.
- Action requests at runtime not in the action's `requires` declaration are denied at the action boundary.

For ultra-sensitive credentials (banking, healthcare), connectors can be escalated to OS-process isolation as a higher tier — slower, stronger boundary. Default is WASM; escalation is opt-in.

### The user channel

The agent UI is one path between the user and Aileron, but it's not the only one — and for security-critical interactions, it cannot be the only one.

When a user types into an agent's prompt, their message reaches Aileron only after the agent transforms it into a chat completion request. That transformation can paraphrase, wrap, or rewrite the user's text. For most read-only interactions (status checks, audit queries, action explanations), this is fine — if the agent garbles the request, the user just retries. But for **consent-bearing interactions like approvals**, an agent in the trust path is a security risk: a buggy or compromised agent could rewrite a denial as an approval, or substitute a different action's ID. The user's consent must travel through a channel the agent cannot reach.

Aileron solves this with two complementary mechanisms.

**Output: streaming chat completions.** Aileron holds the agent's chat completion request open as a stream and writes progressive updates into it: *"Reviewing action…"*, *"Waiting for approval…"*, *"Approval received"*, *"Executing…"*, *"Done."* The agent UI displays this progression as the response unfolds. The full lifecycle of an action — including the pause for approval and the result after — surfaces inside the same chat turn that triggered it. The agent UI is a *passive viewer* of the flow; it cannot influence anything inside the stream beyond reading and rendering.

**Consent: out-of-band surfaces.** The user's actual approval click does not travel through the agent UI or the chat completion path. It travels through a surface that connects directly to Aileron's local process, structurally bypassing the agent. Five surfaces, ordered from most-to-least visually integrated:

| Tier | Surface | Visual integration | Reliability |
|---|---|---|---|
| 1 | OS-level biometric / confirmation dialog (macOS Touch ID, Windows UAC) | Overlays focused window; minimal context shift | Native, when supported |
| 2 | System notification with action buttons | Banner over current app | Cross-platform default |
| 3 | Aileron TUI as a docked side panel | Persistent always-open view; auto-updates with pending approvals | Power-user opt-in |
| 4 | Web UI in browser tab (`localhost:8721/approve/<id>`) | Tab opens via clickable URL in stream | Universal fallback |
| 5 | CLI (`aileron approve <id>`) | Manual terminal context-switch | Always works |

Each surface shows the user the action description from Aileron's source of truth — not from the agent's rendering — so substitution attacks (where the agent silently approves the wrong action) are visible to the user. Each surface routes the click directly to Aileron via OS IPC or local-only HTTP, never through the agent.

For v1, Tier 1 (where available) and Tier 2 (cross-platform default) are the recommended approval surfaces. Tier 5 is the always-available backstop. The remaining tiers fill in for cases where the primary surfaces aren't a fit.

**The honest property.** Approval cannot be both fully in-band in the agent UI *and* immune to agent tampering — these are mutually exclusive given the topology of chat-completion-mediated agents. Aileron treats this as a security feature, not a UX bug: the user's consent for consequential actions is structurally protected from agent mediation. The cost is a small visual context shift to whichever OOB surface the user has configured. The cost is real but minimizable, and the security property — *the agent cannot fabricate, modify, or replay user consent* — is something no chat-completion-mediated tool layer can match without this design.

**Read-only commands remain fluid in-band.** For status, audit, and explanation commands (where the trust requirement is low because the user can simply retry if garbled), an in-band convention works:

```
/aileron status
/aileron actions
/aileron explain "send invoice to acme"
/aileron audit
```

These reach Aileron via the chat completion path, are best-effort but reliable across major agents, and avoid the OOB context shift for low-stakes interactions. The CLI (`aileron status`, etc.) and TUI offer the deterministic alternative when needed.

### Failure handling: visible by default, not silent

Four architectural commitments shape how Aileron behaves when an action fails. Specific retry budgets, manifest syntax for failure overrides, and detailed failure-class policies are deferred to the failure-handling ADR — they benefit from real implementation experience.

**1. Visible failure is the default; never silent LLM fallback.** When an action fails, the agent receives a structured error. Aileron does not fall back to the LLM and let it produce a probabilistic response that masquerades as the action's output. Silent fallback is the worst possible failure mode because the user thinks the action succeeded when it didn't — the LLM hallucinates a confirmation while real-world state diverges from belief. This is a security commitment, not a UX preference.

**2. LLM fallback is opt-in, gated to informational-class actions only.** Some actions are read-only or synthesis-style ("look up customer info," "summarize this thread") where degraded LLM answers beat no answers. For these, the action manifest can opt into LLM fallback explicitly, with the response clearly flagged as estimated. For any action with side effects — sends email, charges a card, modifies a database — Aileron refuses the fallback flag at install time. Side-effecting actions cannot fall back to probabilistic execution; the runtime structurally prevents it.

**3. Errors are structured so agents can reason about them.** When an action fails inside a function-calling flow, the tool result includes a structured error payload (failure class, retriability, user-facing message, audit ID). When an action fails in pre-LLM bypass mode, the streaming response includes a recognizable marker block. Agents that want to retry, inform the user, or try a different approach have the information to do so.

**4. Actions are designed for idempotency by default.** Idempotency keys derived from action inputs make retries safe. Authors writing genuinely non-idempotent actions opt out explicitly in the manifest; Aileron then disables auto-retry for those actions. This commitment shapes how the action manifest schema looks and how partial-failure recovery works — both of which become tractable when retries don't risk double-execution.

The detailed policy — failure-class taxonomy, default retry budgets, manifest syntax for overrides, recovery semantics for partial failures, OAuth refresh as a first-class flow rather than a failure mode — is deferred to the failure-handling ADR. These four commitments set the architectural posture; the ADR fills in the operational details with concrete implementation experience.

### Project portability: action files travel; credentials don't

Four architectural commitments shape how project state survives across machines and team members. There is no separate lock file — the action files themselves are the contract. The detailed `aileron sync` UX, prompt sequences, conflict resolution, and Control-federation interactions defer to the project-portability ADR.

**1. Action files are committable; credentials are not.** Action files (`actions/*.md`) live in the project repo, version-controlled by git. They contain everything needed to reproduce the project's action behavior: connector names, exact versions, hashes, declared capabilities, execution steps. They do not contain credentials or anything secret. Anything secret stays in the developer's local vault.

**2. Bindings are personal; intent is shared.** Action files reference bindings by name (`slack/work`); each developer's local vault holds *their own* credential under that name. The team aligns on intent (which workspace conceptually, which environment); each developer's identity remains private. Two developers running the same project from the same action files each post to their own Slack via their own credentials — same name, different bound resource.

**3. `aileron sync` is the guided-setup primitive.** A teammate cloning the repo runs one command. Aileron reads the action files, identifies missing connectors and unbound credentials, walks the developer through installing missing connectors (verifying hashes match the declarations in the action files), and completes the OAuth flows they need to bind their own credentials. Standard `npm ci`-shape — but driven by the action files themselves, not a separate lock file.

**4. Shared credentials federate through Control.** For service accounts and organization-shared credentials (CI bots, automation identities, shared workspace tokens), Aileron Control hosts the binding centrally. The action files reference the same binding name; resolution differs depending on whether the developer's machine is signed into Control. The same project files work whether the developer is solo, on a team using personal bindings, or on a team using federated org-shared bindings — without changing how the action files are written.

The detailed policy — exact `sync` UX, conflict resolution when actions disagree on connector versions, prompt sequences for OAuth in restrictive workspaces, how Control federation handles permission changes, the precise behavior when an action file's hash doesn't match what the Hub serves — defers to the project-portability ADR. These four commitments set the architectural posture; the ADR fills in the operational details with concrete implementation experience.

### Manifest format: Markdown plus TOML frontmatter

Aileron commits to specific format choices for each kind of artifact. The choices reflect a security-and-clarity preference for declarative source code that drives execution.

| Artifact | Format |
|---|---|
| Action files | Markdown body + TOML frontmatter (`+++` delimited) |
| Connector manifests | Pure TOML |
| Project config | Pure TOML |
| Runtime IPC and internal state | JSON |

**Why TOML over YAML for the structured parts.** YAML's whitespace sensitivity, type coercion (the "Norway problem"), and parser-implementation differences are real footguns for content that drives execution. A misparsed action file isn't a broken build — it's the wrong action running with the wrong arguments, against real systems, with real credentials. TOML's strict, unambiguous syntax eliminates these footguns. Parser attack surface is also smaller; the deserialization-to-arbitrary-objects vulnerability class that has produced repeated YAML CVEs simply doesn't exist in TOML. For security-critical declarative content, TOML's strictness is a structural advantage, not just a stylistic preference.

**Why Markdown for action files.** Action files have three audiences simultaneously: the runtime needs the structured contract, developers reading the project need documentation, and the LLM that surfaces the action as a tool needs a function description. Markdown with structured frontmatter is the established pattern for handling this — used by Anthropic Skills, AGENTS.md, Hugo, Jekyll, GitHub issues, and the broader "structured frontmatter + prose" convention. The Hub renders the Markdown for browsing; developers read the prose; the runtime parses the frontmatter; the LLM consumes the description from the body. One file, four readers, no impedance mismatch.

**The body doubles as the LLM-facing description.** When Aileron augments the agent's tool catalog with installed actions, each function's `description` is drawn from the action file's Markdown body — typically the first paragraph or a designated section. The documentation the author writes for humans IS the description the LLM reads. One source of truth, no separate "LLM hint" field to keep in sync with the prose.

**On YAML preference among action authors.** Some authors will prefer YAML for the frontmatter — particularly those coming from CI config or Anthropic Skills tooling. We're shipping TOML and watching for adoption friction. Format choice is reversible (clean conversion in either direction) and we can add YAML frontmatter support later if community feedback shows TOML is a real barrier. Starting with the safer format is the right default for security-critical content.

### Why these decisions become ADRs

The connector model, the action model, capability binding UX, dependency resolution, intent matching mechanisms, the user channel and out-of-band approval surfaces, failure-handling policy, project portability and the action-file-as-contract model, manifest format conventions, install consent flow, and sandbox choice are all foundational. Each will get an ADR with the trade-offs, alternatives considered, and decision criteria recorded explicitly. That documentation exists to keep these decisions reviewable and to make changes deliberate, not accidental.

---

## Why This Is Structurally Defensible

Every other product in the surrounding space solves a fragment of the pattern but cannot occupy this seam.

| Player | What they solve | Why they cannot do this |
|---|---|---|
| Aurelio Semantic Router | Deterministic intent → action dispatch | Python library; agent must integrate it |
| Docker Cagent | Proxy returning chat-completion responses without LLM | CI replay tool; cassettes are recordings, not authored actions |
| Anthropic Skills | Declarative capability manifest | LLM still executes; not deterministic |
| MCP | Expose tools to the LLM | LLM in the loop; no tamper-resistant approval channel |
| NVIDIA OpenShell | Sandbox-level interception of agent process | Cannot read LLM-shaped intent; not at the endpoint seam |
| LiteLLM / Portkey / OpenRouter | OpenAI-compatible gateway | Always routes to *another* LLM |
| Ollama Cloud | Local + cloud overflow | Local-only routing, no action layer, no governance |
| Salesforce Agent Fabric | "Guided determinism" workflow handoffs | No-code orchestration DAG; not a transparent proxy |
| 1Password Agent Access | Scoped credentials for agents | Vault layer, not execution substitution |

The space is thoroughly *surrounded*. No vendor has assembled the load-bearing pieces — declarative manifest, transparent OpenAI-compatible endpoint, deterministic tool execution, capability-bound connectors, action-level capability subsetting, credential isolation, and tamper-resistant out-of-band approval — at the seam where they belong. Shipping that combination first, with an open manifest format, is a category-defining move.

For the full competitive scan — including the deterministic-substitution-pattern research, threat ranking, and named players in each adjacent category — see [Competitive Landscape](/pivot/competitive-landscape).

---

## The Customer

### Individual developers — productivity that secures itself

- AI coding tool users (Claude Code, Cursor, Continue.dev, Copilot) who want their agents to actually finish the work — write the code, ship it, tell the team — instead of stopping at the boundary of "code only."
- Local-first developers running Ollama, llama.cpp, or MLX, frustrated with the configuration tax and leaving 30–50% of hardware capability unused.
- Privacy-first power users automating personal workflows where data must not leave the device.
- Indie agent builders shipping products where per-request token cost is a line item.

The wedge is the individual developer. The expansion is structurally aligned: the same install pattern scales to every system the developer touches, and the same architecture that gives an individual developer their first hero moment also gives an enterprise its first compliance-grade deployment.

### Enterprise — addressed in detail later

Compliance, vault federation, audit retention, signed connectors, attested execution — these matter to enterprises and the architecture supports them. They get a dedicated treatment in a separate document. This document is for the developer who needs to feel productive in five minutes.

---

## The Business: Revenue Atop the Open Source

The core is open source. That is the distribution layer. Free is the right price because distribution is the asset.

- **Aileron Runtime** — MIT, self-hosted, `brew install aileron`. Includes the action engine, the inference orchestration, the routing intelligence, the connector sandbox, and the capability enforcement layer.
- **`ACTIONS.md` and the connector manifest format** — open primitives; community-authored actions and connectors available freely.
- **Aileron Control basics** — local vault, basic policy, single-user audit.

Revenue compounds across these surfaces:

### 1. Aileron Cloud — overflow as a service

When local execution is insufficient and a frontier model is required, requests route through Aileron Cloud. We select across providers, optimize cost and reliability, present unified billing, and apply the routing decisions as a margin-bearing service.

- **Indie tier:** $20/mo for individuals, capped overflow.
- **Pro tier:** $100/mo per developer, higher quotas, priority routing.
- **Team tier:** usage-based, volume discounts.

Comparable shape to OpenRouter; structural advantage is being the routing decision-maker, not a thin proxy.

### 2. Aileron Control — governance for production agents

Sold to teams shipping production agents that take real-world actions. The full Control surface: multi-user vault with scoped credentials, policy authoring and enforcement, inline approval workflows, full audit retention, RBAC, SSO.

- **Team tier:** $50/seat/mo.
- **Enterprise tier:** custom, with on-prem option, SLA, and dedicated support.

The expansion vector is architectural: every Runtime install is a Control candidate the moment the agent takes its first real-world action.

### 3. Aileron Hub — the unified marketplace surface

The Hub is one developer-facing browse experience covering both connectors and actions. Internally there are two distinct distribution mechanics — connectors are sandboxed binaries with signing, version pinning, and capability declaration; actions are template manifests copied into the developer's project on install. Externally these appear as one unified surface where developers browse for capabilities ("I want Slack things") and find both connectors and actions related to that domain.

Three tiers across both content types:

- **Free tier:** community-published connectors and actions, unsigned, best-effort.
- **Verified tier:** signed by Aileron, security-reviewed, SLA-backed.
- **Enterprise tier:** built and supported by Aileron for SAP, Salesforce, Workday, ServiceNow, and other high-value vertical integrations.

Aileron sits in the middle with a take rate. Pattern shape: Docker Hub, npm, GitHub Marketplace.

### 4. Aileron Connector Certification — vendor-funded trust

SaaS vendors pay Aileron to certify their official connector. Their logo gets the "Aileron Verified" badge. Customers get an audited supply chain. Vendor-funded, not customer-funded.

### 5. Aileron Connector Studio — for organizations building their own

Tooling for organizations building connectors against their own internal systems — sandbox primitives, capability declaration helpers, signing flow, CI integration, testing harness. Per-seat pricing.

### 6. Aileron Insights — observability and cost analytics

A first-party observability surface populated by every Runtime install:

- **Determinism rate** per agent — what fraction of requests are substituted vs. LLM-mediated.
- **Cost analytics** — savings from action substitution, savings from per-request routing.
- **Quality metrics** — model selection performance against the developer's quality bar.
- **Connector observability** — which connectors run hot, which fail, which capabilities are exercised.
- **Action observability** — which actions are most-used, which fail, which capability surfaces are growing.

Sold per seat or as part of higher Control tiers.

### 7. Aileron Enterprise — the high-touch top

The traditional enterprise tier: on-premises deployment, SSO, RBAC, audit retention beyond cloud limits, dedicated support, SLAs, security reviews, custom action and connector development. Six-figure annual contracts. Detailed treatment in the enterprise document.

### The compounding logic

Every Aileron Runtime install is a candidate for all surfaces. Cloud monetizes overflow inference. Control monetizes production governance. Hub monetizes the platform itself. Certification monetizes vendor reputation. Studio monetizes custom integration. Insights monetizes ongoing operations. Enterprise monetizes the long tail of high-value organizations. Each makes the others more valuable. All ride on a free OSS distribution layer.

---

## What Aileron Is Not

- Not another model.
- Not another agent framework.
- Not a wrapper around Ollama — Ollama is one engine Aileron orchestrates.
- Not a thin API gateway — the routing and action decisions are the value, not the proxy.
- Not "near-frontier results on a laptop" — Aileron promises the cheapest model that meets the developer's quality bar.
- Not a tool-calling framework — Aileron *executes* tool calls deterministically with capability isolation, rather than delegating to whatever code the LLM coordinates.
- Not a content firewall — Aileron does not filter LLM output; it executes deterministic actions with structural safety guarantees.
- Not a workflow orchestrator — agents do not author DAGs; they speak chat completions, and Aileron decides what happens behind that endpoint.

---

## The One-Sentence Pitch

**Aileron is the deterministic execution layer for AI agents: it executes the tool calls agents make with capability isolation, sealed credentials, tamper-resistant approval, and a complete audit trail — and routes to the cheapest model that meets quality only when the LLM is genuinely required. The agent's tool calls have properties it can't perceive: deterministic execution and trustworthy consent.**
