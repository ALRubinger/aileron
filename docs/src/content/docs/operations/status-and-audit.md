---
title: "Status & Audit"
description: "Inspect configuration and review the audit trail"
---

## Status

See your full configuration at a glance:

```sh
aileron status              # show everything
aileron status policy       # merged policy: defaults + project + user
aileron status env          # scrubbed and passthrough vars
aileron status notifications # Slack/Discord channels and token status
aileron status vault        # stored secrets (names only)
```

## Audit trail

Every command, policy decision, approval, message sent, and credential used is recorded in an append-only local audit log. The log is the ground truth for what happened, not a narrative that can be cleaned up after the fact.

```sh
aileron log
```

The audit trail records:

- Every command the agent ran and the policy decision (allowed, denied, or prompted)
- Every approval or denial you gave
- Every message sent through Aileron
- Every credential usage (which secret was used, for what request, when)

This provides accountability and proof of control: who did what, when, and why.
