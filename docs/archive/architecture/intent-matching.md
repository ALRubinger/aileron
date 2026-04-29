---
title: "How Intent Matches to Actions"
description: "Primary tool augmentation via function calling, secondary pre-LLM bypass for clear intents"
order: 4
---

> **Architecture:** part of the [Architecture](/architecture/) section of the Pivot strategy. See also [The Action Model](/architecture/action-model) and [Aileron Runtime](/runtime).

Aileron uses two mechanisms in tandem to translate user intent into action execution.

**Primary: tool augmentation via function calling.** Modern agents (Claude Code, Cursor, Copilot, ChatGPT, etc.) speak function calling — they construct LLM requests with a `tools` array describing available functions, and the LLM decides which (if any) to call. Both OpenAI's chat completions API and Anthropic's Messages API surface this in compatible shapes; Aileron Runtime serves both. On every incoming request, Aileron augments the agent's `tools` array with installed actions, translated into function definitions:

```yaml
# An action's match clause becomes a function description the LLM sees:
- name: ship-update
  description: "Post a 'shipped' update to a Slack channel with the merged PR link"
  parameters:
    type: object
    properties:
      channel: { type: string }
```

The augmented request passes through to the configured LLM. The LLM does the categorization — picking when to invoke an action and what arguments to pass. Aileron then executes the deterministic ones with capability isolation; agent-defined tools (like `bash` or `file_read`) pass through unchanged.

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
