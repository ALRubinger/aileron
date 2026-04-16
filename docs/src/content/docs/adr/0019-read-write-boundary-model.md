---
title: "ADR-0019: Read/Write Boundary Model — LLM Reads, Aileron Writes"
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-16</td></tr>
  <tr><th>Refines</th><td><a href="/adr/0009-deterministic-execution-plane">ADR-0009</a>, <a href="/adr/0018-context-store-architecture">ADR-0018</a></td></tr>
</table>
</div>

## Context

ADR-0009 established that "agents decide, Aileron acts" — agents propose intents, Aileron owns credentials and executes irreversible actions. ADR-0018 established that source system data stays in source systems and is fetched at query time through the user's OAuth credentials.

During implementation of the source connector layer and draft generation pipeline, a design question emerged: should the LLM call source connector tools directly (Model B), or should Aileron assemble context and pass it to the LLM as a pre-built prompt (Model A)?

Model B was chosen for the MVP because the LLM handles multi-step retrieval reasoning better than any pipeline logic we'd build — "search for JWT → find PR #247 → read the diff → check if claims changed" is natural for an LLM but hard to replicate deterministically.

However, this raised the question: does Model B violate Aileron's core premise that it controls access to real systems?

## Decision

Aileron adopts a **split boundary model** based on the reversibility of the operation:

### Reads: LLM calls tools, data stays in the customer's boundary

The LLM calls source connector tools during draft generation — `github_search_code`, `slack_channel_history`, `calendar_events`, etc. Aileron provides the tools and injects credentials (the LLM never sees OAuth tokens), but the LLM sees the data returned by these tools.

**This is acceptable because:**

1. **Credentials are protected.** Aileron holds all OAuth tokens in the vault (KEK-encrypted per ADR-0010). The LLM receives structured results, never raw tokens. It cannot make unauthorized API calls.

2. **Enterprise data privacy is solved by LLM tier, not by data sanitization.** For customers who need data privacy guarantees on reads, the answer is Tier 2/3 from issue #103 — run your own LLM in your own VPC. The LLM reads source data, but the LLM runs in your infrastructure, so the data never leaves your network. This is fundamentally cleaner than trying to sanitize or redact data before sending it to a public LLM while keeping the context useful.

3. **The alternative is worse.** Model A (Aileron assembles context, LLM only sees the assembled prompt) requires Aileron to replicate the LLM's reasoning about what's relevant. The LLM is better at multi-step retrieval. Building retrieval logic in Aileron would produce worse drafts and take longer to build.

4. **Read operations are reversible.** Reading a Slack message or a PR diff has no side effects. If the LLM retrieves irrelevant context, the cost is wasted tokens, not an irreversible action.

### Writes: Aileron owns execution, always

All irreversible actions flow through Aileron's execution plane regardless of LLM tier:

- **Sending messages** (Slack, email) — Aileron sends using the user's OAuth token after explicit user approval
- **Scheduling meetings** — Aileron creates calendar events
- **Making payments** — Aileron executes via payment rails
- **Any action with side effects** — Aileron owns it

The LLM proposes a draft. The user approves. Aileron executes. The LLM never has write access to any system. This is ADR-0009's execution plane model, unchanged.

### The enterprise privacy story

| Tier | Reads | Writes | Data boundary |
|------|-------|--------|---------------|
| **Tier 1** (Cloud LLM) | LLM reads via tools, data passes through Anthropic's API | Aileron sends, user approves | Anthropic API (no training on API data) |
| **Tier 2** (Customer VPC) | LLM reads via tools, data stays in customer's VPC | Aileron sends, user approves | Customer's infrastructure |
| **Tier 3** (Aileron-managed) | LLM reads via tools, data stays in customer's cloud account | Aileron sends, user approves | Customer's cloud account |

For Tier 2/3, the complete data flow stays within the customer's infrastructure. The LLM reads freely (within the user's OAuth permission scope), but it runs on the customer's hardware. Aileron Cloud only handles event routing (inside the secure enclave per #103) and execution of approved writes.

This eliminates the need for data sanitization, redaction, or filtering on reads. The privacy guarantee is structural — your LLM, your data, your infrastructure — not procedural.

## Consequences

### Source connector tools remain LLM-callable

The `POST /v1/tools/execute` endpoint and the draft pipeline's tool-use loop remain as built. The LLM calls tools during draft generation. This is the correct architecture for reads.

### Aileron's execution plane is for writes only

The intent/approval/execution flow (ADR-0009) applies to irreversible actions. Source connector reads bypass this flow — they are side-effect-free and authorized by the user's connected account OAuth token.

### Enterprise tier determines read privacy

Data privacy on reads is a function of where the LLM runs, not of what data Aileron filters. Sales conversations shift from "what data does Aileron see?" to "where does your LLM run?" Tier 2/3 answers this completely.

### No data sanitization layer needed

Aileron does not need to build data sanitization, PII redaction, or context filtering for read operations. The customer's LLM tier is the privacy control. This eliminates a complex, error-prone component that would degrade draft quality.

### The feedback loop confirms tool call quality

Draft feedback signals (approve/edit/discard) capture which tools the LLM called and whether the resulting draft was useful. Over time, this data informs the behavioral model about which sources produce good context for which types of questions — without Aileron needing to make the retrieval decisions itself.

## Relationship to other ADRs

- **ADR-0009 (Execution Plane):** Refined, not superseded. The execution plane applies to writes. Reads are handled by the source connector tool layer.
- **ADR-0010 (Zero-Knowledge Vault):** Unchanged. Credentials are always protected. The LLM receives data, never tokens.
- **ADR-0018 (Context Store):** Refined. The "proxy, don't replicate" model holds — source data stays in source systems and is fetched at query time. The LLM is the query-time consumer, not a pre-assembly pipeline.
- **Issue #103 (Cloud Messaging Gateway):** The LLM tier model (Tier 1/2/3) is the mechanism for enterprise read privacy.
