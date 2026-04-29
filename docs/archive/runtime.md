---
title: "Aileron Runtime"
description: "The action-execution layer behind a transparent endpoint compatible with both OpenAI's chat completions API and Anthropic's Messages API"
order: 2
---

> **Section:** part of the Pivot strategy. See [Overview](/) for the two-layer summary, [Aileron Control](/control) for the governance side, and [Architecture](/architecture/) for the load-bearing decisions.

Aileron Runtime is one process running locally. The agent points at `http://localhost:8721/v1` thinking it is talking to an LLM. Behind that endpoint, Runtime serves both OpenAI's chat completions API and Anthropic's Messages API — whichever shape the agent's client speaks. Runtime intercepts every request and augments the agent's tool catalog with installed actions; when the LLM calls one, Runtime executes it deterministically — sealed credentials, capability isolation, your approval where it matters. When no action applies, the request passes through to the configured LLM unchanged.

## Actions: declarative, deterministic, shareable

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
