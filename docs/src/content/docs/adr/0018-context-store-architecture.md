---
title: "ADR-0018: Context Store Architecture"
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-14</td></tr>
</table>
</div>

## Context

Issues #103 and #104 describe Aileron's vision for AI-drafted communications: messages arrive from Slack, Gmail, Teams, etc., Aileron assembles context from multiple sources, an LLM drafts a reply, and the user approves before sending. Issue #104 calls the context pipeline "the product" — the LLM is commodity, the messaging gateway is plumbing, and the context is what makes drafts good.

The central architectural question is: what does Aileron's context store contain, where does data live, and how does context reach the LLM at draft time?

### Options Considered

1. **Unified envelope store (replicated data)** — Aileron ingests and stores copies of all context (Slack messages, GitHub PRs, calendar events, etc.) in a single denormalized store with a `ContextItem` envelope type. Typed query views provide structured access. Full data residency in Aileron's infrastructure.

2. **Store per context type (replicated data)** — Each context source has its own store with a native schema (MessageStore, CalendarStore, CodeStore, etc.). A fan-out retrieval layer queries all stores at draft time and merges results.

3. **Unified envelope for Aileron-owned data + proxy access to source systems** — Aileron stores only what it uniquely owns (user instructions, draft feedback, behavioral model, integration metadata). Source system data (Slack messages, GitHub PRs, calendar events) stays in the source systems and is fetched at query time through the user's own credentials.

### Evaluation

**Option 1** centralizes access control enforcement (one store, one enforcement point) and makes cross-source ranking natural (all items in one result set). But it replicates data that already exists in source systems, creating staleness, sync complexity, storage cost, and data residency concerns. Every calendar event moved, every Slack message edited, every PR merged requires the replica to catch up or serve stale data.

**Option 2** gives each source a native schema optimized for its access patterns (time-range queries for calendar, repo+status for PRs). But it duplicates access control enforcement across N stores — N implementations of the same invariant that #106 calls "a precondition for the product existing." It also duplicates encryption, retention logic, and dashboard aggregation. Adding a new context type is full-stack work across every concern.

**Option 3** avoids data replication entirely. Source system data is always fresh because it's read from the source at query time. Access control is simplified — if the source system API returns data for the user's token, they have access; if it doesn't, they don't. The access check and the data retrieval are the same operation. Aileron's store is small and focused: only data that exists nowhere else. Compliance is cleaner ("we don't store your Slack messages"). The cost is query-time latency to source systems (50-300ms per source, parallelized), mitigated by short-TTL ephemeral caching.

## Decision

Aileron adopts **Option 3: unified envelope for Aileron-owned data, proxy access to source systems.**

### What Aileron stores (the envelope)

The context store holds only data that Aileron uniquely owns — data that does not exist in any source system:

**User instructions.** Explicit rules and preferences the user defines for how Aileron should communicate on their behalf. Analogous to CLAUDE.md but for communication. Examples: "Never commit to deadlines without checking with my tech lead." "Always mention PR numbers when discussing code changes." "Be brief in #incidents, detailed in #architecture."

**Draft feedback signals.** Every draft interaction produces a signal that feeds back into the system:

- *Approved as-is* — the context assembly, tone, and content were correct. Aileron records which source systems were queried, which tool calls the LLM made, which results informed the draft, and the channel/audience context.
- *Edited before sending* — the diff between the draft and the sent message is a correction. Content corrections signal context assembly quality; style corrections feed the behavioral model.
- *Discarded* — the draft was wrong enough that editing wasn't worth it. Strong negative signal with the same metadata as approval.
- *Revised via feedback* — the user provides explicit direction ("make it shorter", "mention the deadline"). Stored as contextual instruction — may be one-time or, if repeated, promoted to a permanent user instruction.

**Behavioral model.** A compressed representation of the user's communication patterns, derived from accumulated feedback signals. Not raw examples — derived preferences: tone per channel/audience, typical response length and formality, phrases the user favors, topics they defer vs. answer directly. This is a materialized view over feedback signals, periodically regenerated as new signals arrive. Included in the LLM system prompt at draft time.

**Integration metadata.** Which sources are connected, OAuth token references (pointing to the vault, not stored here), per-source user preferences ("don't use #random for context"), and learned topic-to-source associations ("auth questions" -> GitHub/aileron repo + #backend channel + BACKEND Linear project). This is the map of where knowledge lives — built up organically through real interactions, not configured manually.

Each item in the envelope store follows a unified schema:

```
ContextItem {
  id:           string
  user_id:      string
  content_type: enum (user_instruction, draft_feedback, behavioral_model,
                      integration_metadata, topic_association)
  created_at:   timestamp
  updated_at:   timestamp
  payload:      bytes  // encrypted with user's KEK (ADR-0010)
}
```

All items are encrypted with the user's Key Encryption Key per ADR-0010. The envelope store is private — every item belongs to exactly one user. There is no organizational context tier in the envelope itself; organizational context comes from source systems where it already has appropriate access controls.

Typed query views provide structured access over the envelope without requiring payload deserialization for common operations:

- **InstructionView** — all user instructions, loaded eagerly (small set, always included in prompts)
- **FeedbackView** — feedback signals, queryable by signal type, channel, audience, date range (secondary indexes on these fields)
- **BehavioralView** — the current behavioral model, loaded per-user (one active artifact at a time)
- **IntegrationView** — connected sources, topic-to-source associations, per-source preferences

### What Aileron does not store

Aileron does not replicate source system data. No copies of Slack messages, GitHub PRs, calendar events, issue tickets, email threads, or any other data that lives in an external system. This data is fetched at query time through the user's own credentials.

### How source system data reaches the LLM

At draft time, the LLM is given access-scoped tools — one set per connected source system. Each tool calls the source system API using the user's OAuth credentials from the vault:

- **GitHub connector:** `search_code`, `get_pr`, `get_diff`, `list_reviews`
- **Slack connector:** `channel_history`, `thread_replies`, `search_messages`
- **Calendar connector:** `events_in_range`, `free_busy`
- **Linear connector:** `search_issues`, `get_issue`

The LLM decides which tools to call based on the inbound message and the user's owned context (instructions, behavioral model). This is standard agent-style tool use — the LLM reasons about what context it needs, calls the appropriate tools, receives results, and generates the draft in a single inference pass.

Aileron's role is to:

1. **Provide owned context** — user instructions and behavioral model in the system prompt
2. **Provide access-scoped tools** — each tool uses the user's credentials, so the user's permissions in the source system are the access boundary
3. **Enforce the trust boundary** — the tool set is the access control surface; if the user hasn't connected GitHub, there is no GitHub tool; if they're not in a private channel, the Slack API won't return messages from it
4. **Capture feedback** — after the draft lifecycle completes, store the signal in the envelope

This aligns with ADR-0009's execution plane model: Aileron owns identity and credentials, the LLM proposes intent, Aileron executes within policy. The same architecture applies bidirectionally — reading context and executing actions both flow through Aileron's connector layer with the user's credentials.

### Access control

Per issue #106, Aileron never makes something more visible than it already was in the source system. In the proxy model, this is enforced structurally:

- Source system APIs return only what the user's token authorizes. A Slack user token returns messages from channels the user is a member of — not private channels they've been removed from, not DMs between other people.
- No cached permissions. Every query is a live API call. If a user is removed from a channel between two drafts, the second draft cannot reference messages from that channel.
- On access check failure or timeout, the context is excluded. False negatives (missing relevant context) are acceptable. False positives (leaking private context) are not.
- Ephemeral response caching (1-5 minute TTL) is permitted for performance. The cache is keyed by user+query, scoped to the user's session, and evicted aggressively. This is HTTP-level caching, not data replication.

### The context store as a learned map

The context store is not a database of facts. It is a map of where knowledge lives, built up organically as the user works.

When a user connects Slack, Aileron learns which workspace and channels they're in. When a message arrives about JWT and the LLM calls the GitHub tool and finds PR #247, Aileron now knows this user has a repo called `aileron`, they authored PR #247, it touches `auth/jwt.go`. When the user approves a draft that referenced the PR, Aileron learns that code references improve draft quality for this user. The integration metadata and topic-to-source associations compound over time.

A competitor can plug into the same APIs. They cannot replicate the learned map of which sources matter for which topics, refined through months of real interactions and draft feedback.

### The user instruction contract

Users can explicitly instruct Aileron to improve its context store. This is a first-class interaction — not a hidden setting, but an active collaboration between user and system.

**Direct instructions.** Users create, update, and delete instructions that govern Aileron's behavior:

- "Always reference PR numbers when discussing code changes in #backend"
- "Never commit to deadlines without checking with my tech lead"
- "When Sarah asks about auth, check the aileron repo first"
- "Be brief in #incidents — facts only, no pleasantries"
- "I don't take meetings before 10am"

These are the highest-priority context. They override learned patterns, behavioral model inferences, and any context retrieved from source systems. They are always included in the system prompt.

**Source guidance.** Users can direct Aileron to specific sources for specific topics:

- "For questions about the migration, always check Linear project BACKEND"
- "My architecture decisions are documented in the aileron repo under docs/adr/"
- "Ignore messages from #random — it's never relevant"

These instructions refine the integration metadata directly. They shortcut the learning process — instead of waiting for the feedback loop to discover that Linear is relevant for migration questions, the user states it explicitly.

**Feedback as instruction.** When a user edits a draft or provides revision feedback, Aileron can detect repeated patterns and propose promoting them to explicit instructions:

- "You've edited 8 drafts in #backend to add PR numbers. Save as instruction: 'Always reference PR numbers in #backend'?"
- "You've shortened 5 drafts to Sarah. Save as instruction: 'Keep replies to Sarah concise'?"

The user confirms or dismisses. This closes the loop between implicit correction and explicit rule. Aileron never auto-promotes — the user is always in control.

**Context store inspection.** Users can ask what Aileron knows and where it would look:

- "What sources would you check if someone asked about the auth service?"
- "What have you learned about how I communicate in #backend?"
- "Show me my active instructions"

Responses are grounded in the actual envelope contents — instructions, topic-to-source associations, behavioral model. This transparency builds trust and gives the user agency over their context store.

**Context store correction.** Users can correct the learned map:

- "Stop associating auth questions with the old-infra repo — we migrated to aileron"
- "Forget what you learned about my tone in #general — I was having a bad week"
- "Reset my behavioral model for DMs with Alex"

Corrections take effect immediately. Aileron deletes or updates the relevant envelope items. There is no hidden state the user cannot reach.

### Feedback loop mechanics

The feedback loop is how the context store improves over time. It operates at three levels:

**Level 1: Context assembly quality.** Each draft records which tools the LLM called and which results it used. Approved drafts reinforce those tool-call patterns. Edited/discarded drafts weaken them. Over time, topic-to-source associations in the integration metadata reflect which sources actually produce useful context for which types of questions.

**Level 2: Behavioral model refinement.** Style corrections (edits that change tone, length, or formality without changing facts) accumulate into the behavioral model. The model is periodically regenerated from the full history of style-relevant feedback. It is a compact style guide — "terse in #incidents, detailed in #architecture, casual in DMs with the team, formal with external stakeholders" — not a collection of examples.

**Level 3: Instruction promotion.** Repeated corrections on the same dimension suggest an unstated preference. Aileron detects these patterns and offers to promote them to explicit instructions. The threshold for suggestion is configurable (default: 5 similar corrections). The user always decides.

Feedback signals never leave the user's boundary. They are envelope items encrypted with the user's KEK. In Tier 2/3 deployments (customer VPC or Aileron-managed), feedback never leaves the customer's infrastructure.

Feedback signals do not fine-tune the LLM. The LLM is stateless between calls and pluggable (cloud API, customer VPC, Aileron-managed per #103). Feedback improves what context Aileron provides to the LLM and how it instructs the LLM to behave — the system prompt gets better, the tool selection gets better, the style guidance gets better. The model itself is unchanged.

## Consequences

### Aileron's core IP is the learned map, not the data

The context store holds instructions, feedback, behavioral model, and integration metadata — data that exists nowhere else. Source system data stays in source systems. This means Aileron's value is the orchestration: knowing where to look, what to ask for, how the user communicates, and which context makes drafts good. This compounds with use and cannot be replicated by a competitor who connects to the same APIs.

### No data replication means no sync complexity

There are no webhook-driven ingestion pipelines to maintain, no staleness to manage, no "the calendar event was moved but our store still shows the old time" bugs. The source system is always authoritative, queried in real time.

### Access control is structural, not enforced

Because source system APIs are called with the user's own credentials, access boundaries are inherited from the source system. Aileron does not maintain parallel ACLs. This eliminates the class of bugs where Aileron's permission model diverges from the source system's.

### Query-time latency to source systems

Draft generation requires live API calls to source systems (50-300ms per source, parallelized). This is within the latency budget — the LLM call dominates at 1-5 seconds. Short-TTL ephemeral caching mitigates repeated queries. If a source system is unavailable, the draft is generated with less context — degraded but functional.

### The LLM is pluggable

Because Aileron provides context through a system prompt and tools, any LLM that supports tool use can generate drafts. The context store and connector layer are LLM-agnostic. This preserves the Tier 1/2/3 model from #103.

### The user is always in control

Users can inspect, instruct, correct, and reset any aspect of the context store. There is no hidden state. Instructions override learned behavior. Feedback signals are private. The system is transparent by design.

### The envelope store is small

Without replicated source data, the envelope store contains only Aileron-specific items — instructions (tens), feedback signals (thousands over time), behavioral model (one active artifact per user), and integration metadata (per-source configuration and learned associations). This is kilobytes to low megabytes per user, not gigabytes. Storage, encryption, and backup are straightforward.

### Connector layer is the integration surface

Each source system integration is a set of tools exposed to the LLM. Adding a new source (e.g., Notion, Confluence) means building a new connector with tool definitions, OAuth flow, and API calls. The envelope store, access model, feedback loop, and draft lifecycle are unchanged. This is the same pattern as ADR-0009's Protected Actions catalog — connectors for both reading context and executing actions.

## Relationship to other ADRs and issues

- **ADR-0009 (Execution Plane):** The context store extends the execution plane model bidirectionally. Aileron owns identity and credentials for both reading context and executing actions. The LLM proposes intent (including which context to retrieve); Aileron executes.
- **ADR-0010 (Zero-Knowledge Vault):** Envelope items are encrypted with the user's KEK. OAuth credentials for source system access are stored in the vault. The key hierarchy from ADR-0010 applies unchanged.
- **Issue #103 (Cloud Messaging Gateway):** The gateway routes inbound messages to the draft pipeline. The context store provides owned context; the connector layer provides proxied source system access. The enclave ensures message content is not visible to operators.
- **Issue #104 (Personal Memory Layer):** This ADR implements #104's vision with a key architectural refinement: the memory layer is a learned map of where knowledge lives, not a warehouse of copied data.
- **Issue #105 (Product Strategy):** The individual-first principle is enforced: the user installs, connects, instructs, and controls. The context store is personal. Organizational benefit is emergent.
- **Issue #106 (Context Boundaries):** Access control is structural — source system APIs enforce boundaries via the user's own credentials. No parallel ACLs, no permission caching, no revocation sweeps.
