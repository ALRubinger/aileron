---
title: "The User Channel"
description: "Streaming output in-band; consent out-of-band via five tiers; the agent is structurally never in the trust path for approvals"
order: 9
---

> **Architecture:** part of the [Architecture](/pivot/architecture/) section of the Pivot strategy. See also [Aileron Control](/pivot/control), [Failure Handling](/pivot/architecture/failure-handling), and [The Problem](/pivot/the-problem) for why agent-mediated consent is unsafe.

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
