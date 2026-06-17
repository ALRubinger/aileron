---
title: "Proof of Control"
description: "Auditability is a verifiability property, not just a log file. Aileron records every consequential decision in a structured form designed against the Proof of Control spectrum. The log is self-verifiable today. It becomes trustworthy on a stronger basis under BYOC, where the customer operates the runtime that writes it."
order: 5
---

When an agent posts a message, charges a card, or files a ticket on your behalf, the question that matters afterward isn't *what does the log say?* It's *can I trust the log?* The agent host wrote it. The connector contributed to it. If something went wrong, the same code path that produced the bad action also produced the record of the action.

The [Advanced AI Society's Proof of Control framework](https://www.advancedaisociety.org/proof-of-control) names this gap and the spectrum of verifiability that closes it, from self-verifiable through stronger independent guarantees. Aileron's audit story is designed deliberately against this spectrum, not as a free-form log. Aileron is self-verifiable today and reaches operator-trustworthy auditability under BYOC, where the customer runs the runtime that writes the log.

## What gets recorded

Every load-bearing decision in the runtime emits a structured audit record. This is not incidental logging — it is the contract several ADRs agree on:

- **Install consent.** Every install, update, and `--yes` auto-approval records the artifact FQN, version, hash, signature status, declared capabilities, and the decision ([ADR-0007](/adr/0007-install-consent)).
- **Capability use.** Every action invocation records which connector it called, which capability it exercised, and which binding identity satisfied it ([ADR-0003](/adr/0003-action-model), [ADR-0011](/adr/0011-local-credential-vault)). The credential bytes are never recorded.
- **Failure.** Every failure surfaces with a stable `class`, `boundary`, retry count, and `audit_id` ([ADR-0010](/adr/0010-failure-handling)). Silent failure is forbidden by construction; if it isn't in the log, it didn't run.
- **Approval.** Every out-of-band prompt records the surface, time-to-decision, and outcome ([ADR-0009](/adr/0009-user-channel)).

A reader of `~/.aileron/audit/` can answer "what did the agent do, with what authority, against which service, and what did I approve?" without consulting any other source.

## Where Aileron sits on the spectrum

**Self-verifiable (v1).** The records are local, structured, and complete. The user and operator are the same person, so the log's integrity is bounded by the integrity of the machine. This is adequate when the trust domain is one user.

**Operator-trustworthy (BYOC).** When the customer operates the runtime in their own environment (BYOC), the system that wrote the audit record is operated by the customer, not by Aileron. Its integrity rests on infrastructure the customer controls rather than on trusting Aileron-as-operator. The audit log gains a property the local single-user runtime does not need to claim: the party that could rewrite it is the customer themselves.

The point of starting at self-verifiable is honesty. The schema and the discipline land first on a single laptop. The stronger operator-trust property lands when the customer runs the runtime themselves under BYOC.
