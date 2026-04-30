---
title: "Proof of Control"
description: "Auditability is not a log file — it is a verifiability property. Aileron records every consequential decision in a structured form designed to satisfy the Proof of Control spectrum: self-verifiable today, independently and cryptographically verifiable as the runtime moves into a TEE."
order: 5
---

When an agent posts a message, charges a card, or files a ticket on your behalf, the question that matters afterward isn't *what does the log say?* It's *can I trust the log?* The agent host wrote it. The connector contributed to it. If something went wrong, the same code path that produced the bad action also produced the record of the action.

The [Advanced AI Society's Proof of Control framework](https://www.advancedaisociety.org/proof-of-control) names this gap and the spectrum that closes it: **self-verifiable**, **independently verifiable**, and **cryptographically verifiable**. Aileron's audit story is designed deliberately against this spectrum, not as a free-form log.

## What gets recorded

Every load-bearing decision in the runtime emits a structured audit record. This is not incidental logging — it is the contract several ADRs agree on:

- **Install consent.** Every install, update, and `--yes` auto-approval records the artifact FQN, version, hash, signature status, declared capabilities, and the decision ([ADR-0007](/adr/0007-install-consent)).
- **Capability use.** Every action invocation records which connector it called, which capability it exercised, and which binding identity satisfied it ([ADR-0003](/adr/0003-action-model), [ADR-0011](/adr/0011-local-credential-vault)). The credential bytes are never recorded.
- **Failure.** Every failure surfaces with a stable `class`, `boundary`, retry count, and `audit_id` ([ADR-0010](/adr/0010-failure-handling)). Silent failure is forbidden by construction; if it isn't in the log, it didn't run.
- **Approval.** Every out-of-band prompt records the surface, time-to-decision, and outcome ([ADR-0009](/adr/0009-user-channel)).

A reader of `~/.aileron/audit/` can answer "what did the agent do, with what authority, against which service, and what did I approve?" without consulting any other source.

## Where Aileron sits on the spectrum

**Self-verifiable (v1).** The records are local, structured, and complete. The user and operator are the same person, so the log's integrity is bounded by the integrity of the machine — adequate when the trust domain is one user.

**Independently verifiable (Stage 2).** When [the vault](/concepts/the-vault/) moves into a TEE-backed Aileron Cloud, the runtime emitting the audit record runs in attested hardware the operator cannot inspect. The record gains a property the local runtime cannot provide: *the system that wrote it could not have rewritten it*.

**Cryptographically verifiable (Stage 3).** Audit records signed inside the enclave, with hash-chained continuity, become evidence a third party can check against the attestation transcript without trusting Aileron at all. This is the destination the v1 schema is forward-compatible with.

The point of starting at self-verifiable is honesty: cryptographic verifiability is the goal, but it has to be earned by infrastructure that doesn't exist yet on a single laptop. The schema and the discipline land first; the cryptographic teeth land with the TEE.
