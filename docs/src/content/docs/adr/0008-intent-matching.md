---
title: "ADR-0008: Intent Matching Mechanisms"
description: "Tool augmentation via function calling is the primary intent-matching mechanism; pre-LLM bypass for high-confidence patterns is an opt-in secondary path; agent-defined tools take precedence in name collisions"
order: 8
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

[ADR-0003](/adr/0003-action-model) establishes that actions are atomic, declaratively-defined units that the agent invokes. It defers a load-bearing question: *how does the runtime decide which action to invoke for a given user request?*

Aileron sits at the LLM endpoint. Every chat completion request from the agent passes through it. The runtime sees:

- The conversation messages so far.
- The agent's declared tools (the array of function definitions the agent's host code passes in).
- The agent's hyperparameters (model, temperature, etc.).

What the runtime *doesn't* see — at least not directly — is the user's intent. The agent's host application may have already run the user's input through routing, extraction, or pre-processing. The runtime gets the request as it would arrive at the upstream provider — either OpenAI's Chat Completions shape (`/v1/chat/completions`) or Anthropic's Messages shape (`/v1/messages`): a list of messages and tools, with provider-specific field naming.

This ADR ratifies how the runtime matches that request to an installed action.

The decision rests on two observations:

1. **Function calling is the universal seam.** Every modern agent framework — Claude Code, Cursor, Continue, Copilot, Aider, custom code calling chat completions directly — already speaks function calling at the LLM endpoint. The agent's host code declares tools; the LLM picks one; the host executes it. This is the only place in the agent stack where Aileron can intercept *every* agent without requiring SDK changes, host plugins, or framework-specific integrations.

2. **The LLM is good at intent recognition.** Modern instruction-following models excel at choosing among well-described tools. Asking the LLM to disambiguate "tell the team I shipped" → `ship_update` from "draft a PR description" → `pr_draft` is exactly the work the LLM is built for. Bypassing the LLM for intent recognition is an *optimization*, not a default.

These two observations together pin the design. The primary mechanism rides function calling; the LLM does the matching. A secondary mechanism for opt-in fast-path matches exists, but the primary path is what makes Aileron compatible with every agent that already exists.

## Decision

### Primary: tool augmentation via function calling

When a chat completion request arrives, Aileron does the following before forwarding upstream:

1. Read the user's installed actions from `~/.aileron/actions/`.
2. For each action, derive a function definition: name, description (from the action's Markdown body, per [ADR-0001](/adr/0001-manifest-format) and [ADR-0003](/adr/0003-action-model)), and parameter schema (from the action's declared inputs).
3. Append those function definitions to the request's `tools` array.
4. Forward the augmented request to the upstream LLM.
5. Stream the LLM's response back to the agent.
6. When the LLM emits a tool call:
   - If the tool name matches an Aileron-added action, intercept it. Execute the action deterministically (per [ADR-0003](/adr/0003-action-model)), construct the tool result, return it to the agent in the stream as if the agent's host had executed.
   - If the tool name matches an agent-declared tool, pass through unchanged. The agent's host code executes it normally.

The agent never sees the difference. From its perspective, it declared `[search_codebase, read_file]` and the LLM chose to call `ship_update` with arguments `{ channel: "#engineering" }`. The agent's host code sees the tool call, but `ship_update` isn't in its handlers — the host returns a normal tool result Aileron synthesized. The execution layer is invisible.

This is the structural property: **the agent's tool calls have superpowers it can't perceive.** The agent posts to Slack with chat:write scope, and never holds the OAuth token. The agent files a Linear ticket, and never sees the API key. The capability boundary is enforced where the agent has no visibility, no SDK to bypass, no flag to flip.

### Secondary: pre-LLM bypass for high-confidence matches (opt-in per action)

For some intents, calling the LLM at all is wasteful. "What time is it?" → return the time. "Open the documentation" → open the browser. The user's intent is unambiguous; the LLM round-trip adds latency and cost without adding value.

For these, Aileron supports a pre-LLM bypass path:

1. Examine the most recent user message in the chat completion request.
2. Match it against the `[[match.patterns]]` blocks of installed actions whose `[match].bypass` is enabled.
3. If exactly one action matches with high confidence, execute it directly. Return a synthesized chat completion response — never call the upstream LLM.
4. Otherwise (no match, multiple matches, or low confidence), proceed to the primary tool-augmentation path.

The bypass is **strict**:

- **Opt-in per action.** Action authors enable it explicitly in the manifest. Default is off.
- **Conservative matching.** A pattern must match the entire user message (with optional argument extraction); ambiguous matches fall through.
- **Single-match required.** If two actions both match the same input, neither bypasses; both go to the LLM, which can disambiguate.
- **No bypass for side-effecting actions.** An action whose execution has external side effects — sends email, posts to Slack, charges a card — must never bypass the LLM. The LLM's interpretation of context (recipient names, message content, intent confidence) is part of the safety story for consequential actions. Action authors who set `bypass = true` on a side-effecting action will see install-time validation reject it.

In practice, bypass is most useful for **read-only or informational actions**: time, weather, status checks, file lookups, "open the X" commands.

### Pattern schema

The `[match]` block in an action manifest:

```toml
[match]
intent = "natural-language description used by the LLM in tool augmentation"

# (Optional) Patterns for pre-LLM bypass. Only meaningful if [match.bypass]
# is enabled. Patterns are also helpful as additional context for the LLM
# in tool augmentation, but bypass requires the explicit opt-in.

[[match.patterns]]
phrase = "what time is it"

[[match.patterns]]
phrase = "what's the time"

[[match.patterns]]
regex = "^(show|open|view) (the )?docs?$"

[match.bypass]
enabled = true
# Implicit assertion: this action is read-only and safe to execute
# without LLM-mediated context interpretation.
```

The `intent` field is always required and is what the LLM sees in the function description for tool augmentation. Patterns are optional and only consumed when `bypass.enabled = true`.

Pattern matching:

- `phrase` — case-insensitive literal phrase match against the user's message after trimming whitespace. Supports inline argument capture: `phrase = "open {file}"` matches "open docs/api.md" and binds `file = "docs/api.md"`.
- `regex` — full regex match. Captures by name bind to action arguments.
- A pattern is high-confidence if it matches the message in full (modulo whitespace and case). Partial matches don't bypass.

### Tool name collisions: agent-defined tools take precedence

If the agent's tools array already contains a function whose name collides with an Aileron action — e.g., the agent declares `search` and the project also has a `search` action — the agent's tool takes precedence. Aileron renames its action with an `aileron.` prefix in the augmented tools array (`aileron.search`).

This rule is in service of the agent compatibility property: an existing agent that uses `search` must continue to work after Aileron is in place. Aileron is additive; it never overrides the agent's own declarations. The renamed `aileron.search` is still callable — the LLM may choose it explicitly if the description is more relevant — but the agent's `search` retains its name.

The `aileron.` prefix is reserved. An agent declaring a tool with that prefix produces a name collision in the other direction: the runtime emits a warning and renames the agent's tool to `agent.<original-name>` to preserve unambiguous identification.

### What the LLM sees

For each Aileron-added action, the function definition is:

```json
{
  "type": "function",
  "function": {
    "name": "ship_update",
    "description": "Posts a 'shipped' announcement to a Slack channel with the merged PR link.\n\nWhen it fires:\n  - 'tell team I shipped the migration'\n  - 'post a ship update to #engineering'\n  - 'let the team know I merged the PR'",
    "parameters": {
      "type": "object",
      "properties": {
        "channel": { "type": "string", "description": "Slack channel name" }
      },
      "required": ["channel"]
    }
  }
}
```

The `description` is the action's Markdown body (or its first paragraph plus a designated "When it fires" section). The author's prose IS the LLM's prompt — there is no separate "LLM hint" field. One source of truth for what the action does and how to invoke it.

Action names use snake_case in the LLM-facing form (consistent with the OpenAI and Anthropic function-calling conventions); the action's manifest `name` field, which uses kebab-case (`ship-update`), is mapped automatically.

### Multi-turn conversations and tool-call sequences

Function calling is iterative by design. The LLM may call several tools across turns, observe results, and call more. Aileron handles this naturally because each Aileron-added tool call is the same kind of intercept-and-execute as the first.

A multi-turn example:

1. User: "What's the latest commit?"
2. LLM calls `read_recent_merge` (Aileron action). Aileron executes locally, returns the result.
3. LLM calls `ship_update` with the merge info. Aileron executes (with bound Slack credential), returns success.
4. LLM emits final assistant message: "Posted to #engineering. The PR was #4218."

Each tool invocation is its own action invocation per [ADR-0003](/adr/0003-action-model)'s atomicity rule. Aileron does not maintain cross-call state for the agent's session beyond what the audit log records.

### Streaming is preserved

Chat completion responses stream from the upstream LLM through Aileron to the agent. Aileron's interception happens at well-defined boundary points:

- Tool calls: when the upstream LLM emits a tool call delta for an Aileron-added tool, Aileron pauses the stream, executes the action, injects the result back into the conversation, and resumes. The agent sees a normal stream with a synthetic tool-result message.
- Pre-LLM bypass: when bypass fires, no upstream LLM is called. Aileron streams a synthesized response that follows the same chunk format the upstream would have used.

The agent's streaming code path doesn't need to know whether a given response came from the upstream LLM, an Aileron-executed action, or a bypass-path direct response. The transport contract is the same.

## Alternatives Considered

### Replace the agent's tools entirely (rejected)

Aileron strips the agent's declared tools and replaces them with its own. The LLM only sees Aileron's actions.

Rejected because it breaks every agent that already works. An agent host has tools for a reason — codebase search, file reading, terminal execution, project-specific integrations — and stripping those would render the agent useless. Aileron's value is *additive*: it adds capability the agent didn't have. Replacing what the agent already provides is destructive in a way the use case doesn't justify.

### Side-channel intent matching only (no LLM tool augmentation) (rejected)

Aileron does not augment the LLM's tools. Instead, it inspects every chat completion request, matches the user's intent against installed actions via patterns, and either executes a matched action or passes the request through unchanged.

Rejected because it severely limits the surface where Aileron can act. The vast majority of useful agent intents are not expressible as exact-phrase patterns. "Tell the team I shipped the auth migration to staging by EOD" doesn't match a regex; it matches the LLM's understanding of "tell-the-team-something" semantics. Without tool augmentation, Aileron only catches the simplest cases and misses the conversational ones — exactly the cases where deterministic execution is most valuable.

### Pre-LLM bypass enabled by default (rejected)

Pattern-matched bypass is the default path. The LLM is consulted only when no pattern matches.

Rejected because it makes "did the LLM see this?" a question the user has to answer per request, which destroys the trust story. Side-effecting actions that match a pattern would execute without LLM-mediated interpretation of context — "send the email" matched against a careless typo would post a message the user didn't intend. The LLM's job in modern agent flows includes reading the *full* context (prior messages, user style, conversational nuance) and refusing or modifying invocations that look wrong. Skipping that by default trades safety for a latency win that's not worth it.

The opt-in posture (`bypass.enabled = true`) lets authors enable bypass for genuinely safe actions while keeping the safe default for everything else.

### Match agent intent through a dedicated `aileron.match` tool (rejected)

Aileron adds a single tool named `aileron.match` that the LLM calls with `{ intent: "..." }`. Aileron then dispatches to the right action server-side.

Rejected because it inverts the LLM's role. The LLM is good at picking among many well-described functions; it is not as good at synthesizing a freeform intent string and trusting an opaque server-side dispatcher to do the right thing. The function-augmentation approach lets the LLM see exactly which action is being chosen and which arguments are being passed; the `aileron.match` approach hides those details inside Aileron's dispatcher and obscures the trust decision the LLM is making.

### Separate Aileron actions into a different namespace the LLM treats specially (rejected)

The LLM is prompted (via system message or fine-tune) to recognize Aileron actions as special — perhaps to prefer them, or to require additional confirmation before calling them.

Rejected because it requires LLM-side awareness of Aileron, which violates the "works with every modern agent" property. The whole point of the function-calling integration is that the LLM treats Aileron tools identically to host tools. Adding LLM-side discrimination would couple Aileron to specific models or specific system-prompt structures, neither of which is the right shape for a layer that should be invisible.

## Consequences

### For agent hosts

- Existing agents that point at `https://api.openai.com/v1` or `https://api.anthropic.com/v1` work unchanged when the URL is changed to Aileron's local endpoint. Aileron is both OpenAI- and Anthropic-compatible by construction; it serves Chat Completions on `/v1/chat/completions` and Messages on `/v1/messages`. Tool augmentation, interception, and capability enforcement work identically across both protocols.
- Tools the host declares are preserved. Aileron is additive only.
- The agent receives tool results for Aileron-executed actions in the same shape it receives results for its own tools. There is no special handling required.

### For the LLM

- Aileron-added tools appear identically alongside host-declared tools. The LLM picks among them on its merits.
- Function descriptions are the action's Markdown body. Authors who write good action documentation see the LLM make better choices; documentation IS prompt engineering at this layer.
- Tool name collisions are surfaced in the augmented array via the `aileron.` prefix; the LLM sees both the host's `search` and Aileron's `aileron.search` and picks based on description fit.

### For action authors

- The Markdown body of the action file is the LLM's prompt for whether to invoke the action. Write descriptions that match how users actually phrase intents.
- Pre-LLM bypass is available for read-only, idempotent actions. Side-effecting actions cannot enable it. The Hub validates this at publish time.
- Patterns serve double duty: they help the LLM understand intent (when included in the description) and they enable bypass (when explicitly opted in).

### For the runtime

- The chat-completion handler is the heart of the runtime: parse incoming request → augment tools → match for bypass → forward upstream OR execute locally → stream response.
- Streaming preservation requires careful handling of partial tool-call deltas; the runtime buffers tool-call fragments until complete, then dispatches.
- The audit log records every chat completion (request and response shape, not contents) plus every action execution (full detail). The two audit streams cross-reference.

### For users

- Adding Aileron is a one-line change to the agent's API base URL. No SDK changes, no agent restart in workflow-disruptive ways.
- Actions installed via `aileron action add` are immediately available to the agent on the next chat completion. There is no "register this with the agent" step.
- Read-only actions enabled for bypass run faster than LLM-mediated ones. Agent's perceived latency for `what time is it` drops from seconds to milliseconds.

### Open implementation questions (deferred)

- *What surfaces does the agent see when an action requires user approval mid-execution (e.g., "send email" with a recipient list that needs confirmation)?* — [ADR-0009](/adr/0009-user-channel).
- *How does action invocation handle partial failure — connector call succeeds but commit fails, action declares retriability, etc.?* — [ADR-0010](/adr/0010-failure-handling).
- *What happens to in-flight chat completions when the connector store is updated mid-conversation?* — implementation-level concern; runtime maintains version coherence per request.

## Examples

### Tool-augmentation flow (typical agent invocation)

Agent posts to Aileron's gateway. The example below uses the OpenAI Chat Completions shape; an Anthropic Messages-shaped request to `/v1/messages` flows through the same augmentation and interception logic with provider-specific field names.

```json
POST /v1/chat/completions
{
  "model": "claude-sonnet-4-6",
  "messages": [
    { "role": "user", "content": "tell the team I shipped the auth migration" }
  ],
  "tools": [
    { "type": "function", "function": { "name": "search", "description": "..." } },
    { "type": "function", "function": { "name": "read_file", "description": "..." } }
  ],
  "stream": true
}
```

Aileron augments the tools array:

```json
"tools": [
  { "type": "function", "function": { "name": "search", "description": "..." } },
  { "type": "function", "function": { "name": "read_file", "description": "..." } },
  { "type": "function", "function": {
    "name": "ship_update",
    "description": "Posts a 'shipped' announcement to a Slack channel...",
    "parameters": { ... }
  }},
  { "type": "function", "function": {
    "name": "read_recent_merge",
    "description": "Reads the most recent merge commit from local git...",
    "parameters": { ... }
  }}
]
```

Forwarded upstream. LLM responds with a tool call:

```
{ "tool_call": { "name": "ship_update", "arguments": { "channel": "#engineering" } } }
```

Aileron intercepts, executes the action (per [ADR-0003](/adr/0003-action-model)), injects the result back into the stream:

```
{ "tool_result": { "name": "ship_update", "content": "Posted: 'auth migration shipped' to #engineering" } }
```

LLM continues the conversation, emits final assistant message: "Posted to #engineering. Anything else?"

Agent sees a normal streaming response. It never knew `ship_update` wasn't one of its own tools.

### Pre-LLM bypass

Project has a `current_time` action with bypass enabled:

```toml
[match]
intent = "answer 'what time is it' with the current local time"

[[match.patterns]]
phrase = "what time is it"

[[match.patterns]]
phrase = "what's the time"

[[match.patterns]]
phrase = "what time is it now"

[match.bypass]
enabled = true
```

User: "What time is it?"

Aileron matches the pattern, executes `current_time` directly, returns synthesized response:

```
It's 4:23 PM (Pacific).
```

No upstream LLM call. Latency: ~5ms. Cost: $0.00.

### Tool name collision

Agent declares `search`. Project has a `search` action. Augmented tools array:

```json
"tools": [
  { "type": "function", "function": { "name": "search", "description": "Agent's codebase search..." } },
  { "type": "function", "function": { "name": "aileron.search", "description": "Project's custom search action..." } }
]
```

The LLM sees both, picks based on which description fits the user's intent. The agent's `search` continues to work; the user's project-specific `search` is also available under `aileron.search`.

### Bypass refused for side-effecting action

Action manifest:

```toml
[match]
intent = "send an email"

[[match.patterns]]
phrase = "send email to {recipient}"

[match.bypass]
enabled = true
```

The connector this action uses declares `oauth2(scope=gmail.send)` — a side-effecting credential. Hub validation at publish time:

```
ERROR: action 'send-email' enables match.bypass but uses a side-effecting capability:
  oauth2 scope: gmail.send

Side-effecting actions cannot bypass the LLM. The bypass mechanism is reserved
for read-only, idempotent actions whose context interpretation does not need
LLM mediation.

Either remove `[match.bypass] enabled = true` or scope the action to a
read-only operation.
```

The action is rejected from publish. The author either removes the bypass flag (action stays in the catalog, runs through tool augmentation as normal) or splits it into a read-only "draft email" action with bypass and a side-effecting "send email" action without.
