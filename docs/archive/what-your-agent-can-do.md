---
title: "What Your Agent Can Now Do"
description: "Hero use cases and the value Aileron unlocks for developers"
order: 1
---

Aileron sits between your agent and your LLM, intercepting requests and running deterministic actions when intent is clear. The result: your existing agent — Claude Code, Cursor, Continue, anything that speaks `chat/completions` — becomes capable of doing things it could not do reliably before. This document shows what those things look like in practice.

> **Companion documents:** for the pitch and architectural insight, see [Overview](/). For the load-bearing decisions, see [Architecture](/architecture/). For who this is for, see [The Customer](/customer).

Two heroes lead the document. Both are five-minute wins. Both deliver the moment where the agent does something it could not do before. They cover two developer contexts:

- **Hero 1** — for any developer, anywhere, with no third-party setup or admin involvement.
- **Hero 2** — for developers in workspaces where they (or their team) can install Slack apps.

After the heroes, five expansion patterns show how the same install scales to the systems developers actually touch.

---

## Hero 1: Your Agent Searches All Your Code — and Never Hallucinates a Reference

Every developer using an AI coding agent has had this moment: the agent confidently says *"we handle that in `auth.go` around line 42"* and you check and there's no such function, or the file doesn't exist, or it's in a completely different repo. Cursor's `@`-references help when you remember to use them. Claude Code's grep is fast but the agent picks the wrong search terms. Both fall over the moment your work spans multiple repos.

This is the daily, low-grade reliability tax of working with an agent. You can't trust references. You verify everything. You become the agent's fact-checker.

**Aileron makes hallucinated references structurally impossible.**

### What Aileron does that's actually new

Aileron Runtime intercepts code-search intent and routes it to a deterministic search action — symbol-aware, AST-grounded, indexed across every repo the user points it at. The agent gets back real file paths, real line numbers, real function signatures. The agent is not guessing; it is reporting ground truth.

Today's agents *attempt* this; none does it reliably across repos. Cursor's index is per-workspace. Claude Code's grep is per-invocation. Neither agent can answer *"find every place I used the deprecated OAuth1 flow across my last three years of work"* with verified results.

### The five-minute journey

```
$ brew install aileron
$ aileron start
✓ Aileron running on http://localhost:8721/v1

$ aileron action add codebase-search
✓ Installed: codebase-search@1.0.0 (no external connectors)
  Detected repos:
    ~/git    (23 repos, 142K files)
    ~/code   (4 repos, 18K files)
  Indexing… 38s
  Symbol index ready
```

Total: ~5 minutes including the index. Pure local. No browser opens. No admin involved. Works on a fresh laptop in airplane mode.

### The first moment

In Claude Code (or whatever agent the developer already uses), the developer types:

> "Find every place I authenticate against external APIs that still uses session tokens instead of OAuth."

What the agent sees is a normal LLM response. What actually happens:

1. Aileron Runtime intercepts. The `codebase-search` action matches.
2. Aileron runs a deterministic, symbol-aware search across every indexed repo. AST-grounded, not regex.
3. Returns a structured result to the agent: 7 locations across 4 repos, each with file path, line number, function signature, surrounding context.
4. Agent narrates: *"Found 7 places — three in `aileron-go/internal/auth/`, two in `client-app/services/`, two in legacy code in `old-prototype/`. Want me to walk through each?"*

Every reference is real. Every line number resolves. Every file exists. The developer can `cmd-click` any path and land exactly there.

### Why this is the moment

- **Universal daily pain.** Every developer has lost time to a hallucinated code reference. It happens multiple times a week, every week, with every agent.
- **Pure local, fully autonomous.** No OAuth, no admin, no third-party service.
- **Visible, immediate payoff.** The first query produces a result list with verifiable references. The developer can validate the value before the index is even fully warm.
- **Genuinely new capability.** No existing agent does cross-repo, symbol-aware, deterministic search. Cursor and Claude Code each have a piece; neither has all three.
- **Compounds.** Every subsequent *"where do we…"* question gets the same precision. The agent stops being a thing the developer fact-checks.

---

## Hero 2: Your Coding Agent Ships *and* Closes the Loop

Every developer ends every shipping moment the same way: context-switch out of code, write a Slack message, update a ticket, maybe email someone. The closeout is the most repeated, most context-disruptive part of the day. Agents can write the code. They can't do the closeout.

**Aileron closes that gap in five minutes.**

### The five-minute journey

```
$ brew install aileron
$ aileron start
✓ Aileron running on http://localhost:8721/v1

# Point your existing agent (Claude Code, Cursor, Continue) at this endpoint.
# No agent changes. It thinks Aileron is an LLM.

$ aileron connector install slack
→ Opening browser for Slack OAuth…
✓ Connected as alr@yourcompany (channels: #engineering, #shipping, #standups)

$ aileron action add ship-update
✓ Installed from Aileron Action Hub: ship-update@1.0.0
  Uses connectors: slack, git
```

Total elapsed: ~3 minutes. Most of it is the Slack OAuth window.

> **A note on Slack workspaces.** This five-minute flow works in personal and small-team workspaces where any member can install Slack apps. In admin-managed enterprise workspaces, the Aileron Slack app may need workspace-admin approval before the OAuth flow completes — Slack's standard third-party app gate, not specific to Aileron. For solo developers in restricted workspaces, Hero 1 is the better starting point.

### The first moment

In Claude Code (or any agent the developer already uses), the developer types:

> "I just merged the migration. Tell #engineering and link the PR."

What the agent sees is a normal LLM response. What actually happens:

1. Aileron Runtime intercepts the request at its endpoint — compatible with both OpenAI's chat completions API and Anthropic's Messages API.
2. The `ship-update` action matches.
3. Aileron deterministically reads the most recent merge commit from local git, extracts the PR URL, formats the message from the action's template, and posts to `#engineering` via the Slack connector — using credentials Aileron holds and the LLM never sees.
4. Returns the result to the agent: *"Posted to #engineering with PR link."*

In the developer's Slack, a message appears within ~2 seconds. Cost: $0 in tokens. Hallucination risk: zero — channel, message format, and PR link are all resolved deterministically.

**The agent just did something it could not do before, and it did it instantly, accurately, and for free.**

### Why this is the moment

- **Universal.** Every developer ships. Every developer closes the loop. The friction is daily.
- **Visible.** The result lands in front of teammates, in real time.
- **Genuinely new.** Native coding agents don't post to arbitrary Slack channels. MCP servers exist but require setup per integration.
- **Composable.** Once Slack is wired, GitHub, Linear, Calendar, and Email install the same way.
- **The architecture pays off without explanation.** The developer doesn't need to understand WASM sandboxing or capability binding to feel the difference. The message went to the right channel, with the right link, in two seconds, for free.

---

## What Else Opens Up

The same install pattern scales to every system the developer touches. Each expansion is one connector install away.

### 1. Coding agents that cannot delete production

**Pain.** A developer uses Claude Code, Cursor, or a similar coding agent. The agent decides — probabilistically — to run `rm -rf node_modules` to clean up. The model misreads the path and runs `rm -rf /` against the wrong directory. The hook system catches some of these; many slip through.

**Action.** An `ACTIONS.md` entry registers `clean_build_artifacts`. When the agent's intent is "clean the build," Aileron Runtime executes `task clean` (or the project's declared equivalent) directly. The LLM never composes the shell command. The action is reviewed, version-controlled, and behaves identically on every invocation.

**Wins.** Acceleration: developers actually let agents run autonomously. Speed: zero LLM latency or token cost. Safety: destructive shell commands cannot be hallucinated into existence.

### 2. Customer-facing communications without leaked PII

**Pain.** A support agent uses an LLM to compose customer emails. The LLM sees the full customer record in context, which lands in inference logs and provider-side caches. The LLM sometimes hallucinates the recipient. Customer communications get sent to the wrong customer.

**Action.** `ACTIONS.md` registers `send_customer_email`. Aileron looks up the customer in the CRM (deterministic), renders the message from a reviewed template, and sends via Gmail with credentials Aileron holds. The LLM never sees the customer record, never composes the body, never touches the credentials. Recipient hallucination becomes structurally impossible because the recipient is resolved from `customer_id`, not generated from text.

**Wins.** Acceleration: support agents that have been gated by privacy review can ship. Speed: sub-100ms execution. Safety: PII never enters LLM context.

### 3. Database queries that cannot be coerced

**Pain.** Agents that touch databases either get full credentials (terrifying) or call function-calling endpoints that run LLM-generated SQL (also terrifying). Prompt injection that produces `DROP TABLE`-class queries is a published attack class.

**Action.** `ACTIONS.md` registers query intents — `get_revenue_by_period`, `lookup_customer_by_id`. Each is a deterministic, parameterized query against the warehouse, with role-scoped read credentials Aileron holds. The agent expresses intent in natural language; Runtime parses intent and arguments, runs the bound query, returns results. The LLM never generates SQL.

**Wins.** Acceleration: analytical agents that compliance has blocked become deployable. Speed: parameterized queries run as fast as the database allows. Safety: prompt-injected SQL becomes a category-eliminated bug class.

### 4. Financial transactions with reproducible audit

**Pain.** Deploying agents that issue refunds, charge customers, or transfer funds requires a reproducible action stream. SOX, PCI, and internal financial controls require deterministic, idempotent, auditable transactions. LLM-mediated execution cannot meet that bar.

**Action.** `ACTIONS.md` registers `issue_refund`, `charge_customer`, `transfer_funds`. Each constructs the API call deterministically, with an idempotency key derived from action inputs, with required-approval flags routed through Aileron Control's inline approval UX, and with a complete pre-execution audit record.

**Wins.** Acceleration: finance-team agents that have been blocked from deployment for two years can ship. Speed: parallel approvable actions; deterministic execution times. Safety: duplicate charges, hallucinated amounts, and recipient confusion become structural impossibilities.

### 5. Personal automation on private data, locally

**Pain.** Individual developers want agents that can read calendar, parse email, search local files, control screen, manage contacts. Every cloud-based agent service requires sending that data off the machine.

**Action.** `ACTIONS.md` registers personal-data operations — `find_meeting`, `summarize_email_thread`, `pull_file_with_keywords`, `book_calendar_block`. Each runs locally, deterministically, against local data sources using OS-level entitlements Aileron holds. When LLM reasoning is genuinely needed, Runtime routes to the local model, on the user's hardware.

**Wins.** Acceleration: personal-automation surface becomes broadly usable for the first time. Speed: local actions and local inference. Privacy: personal data never leaves the device.

---

## The Pattern

Every hero, every expansion use case, every future capability built on Aileron uses the same architecture:

1. The developer points an existing agent at Aileron's endpoint — which speaks both OpenAI's chat completions API and Anthropic's Messages API, so any agent works.
2. The agent makes natural-language requests, unaware that Aileron is anything other than an LLM.
3. When the request matches a declared action, Aileron runs the action deterministically, with credentials it holds and never exposes, against the system in question.
4. When the request does not match, Aileron passes the LLM call through to the configured model unchanged.

The five-minute install pattern stays constant. Each new connector — Slack, Gmail, GitHub, Linear, Calendar, Postgres, Stripe, anything — installs the same way. Each new action snaps into the same `ACTIONS.md` file. The agent doesn't change. The developer's productivity scales with the actions and connectors they install.

That is the unlock: the agent the developer already uses, every day, becomes capable of doing things it could not reliably do before — and every new capability is one install away.

---

> **For the architectural and strategic picture:** see [Overview](/), [Aileron Runtime](/runtime), [Aileron Control](/control), and the [Architecture](/architecture/) section.
