---
title: "Aileron Runtime"
description: "Interception, substitution, and routing in one local process"
order: 2
---

> **Section:** part of the Pivot strategy. See [Overview](/pivot/overview) for the two-layer summary, [Aileron Control](/pivot/control) for the governance side, and [Architecture](/pivot/architecture/) for the load-bearing decisions.

Aileron Runtime is one process running locally. The agent points at `http://localhost:8721/v1` thinking it is talking to an LLM. Behind that endpoint, Runtime does three things in sequence on every request:

## 1. It augments the agent's tool catalog with installed actions and matches against `ACTIONS.md`.

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

## 2. It orchestrates inference engines when no action matches.

Runtime profiles the hardware, benchmarks available engines (Ollama, llama.cpp, MLX, vLLM, others as they emerge), and learns which engine + model + quantization performs best on this specific machine. Over time it builds a per-machine performance profile that improves with use. The user does not pick the engine. Does not pick the quantization. Does not tune.

This is structurally different from Ollama. Ollama is in the inference-engine business — they wrap llama.cpp and optimize that one experience. Routing across competing engines, including their own, is a conflict of interest they cannot resolve. Aileron is the layer above. We do not run inference; we decide who does.

## 3. It routes per request.

Runtime decides per request whether to run locally, route to a small cloud model, or escalate to a frontier model. Trivial turns run free on local hardware. Moderate turns route to small cloud models. Frontier models are reserved for the slice of work that genuinely requires them.

The honest claim: Aileron does not promise frontier quality on a laptop. Aileron promises **the cheapest model that meets the developer's quality bar**.
