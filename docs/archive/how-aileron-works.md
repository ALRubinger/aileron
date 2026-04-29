---
title: "How Aileron Works"
description: "The building blocks that make Aileron trustworthy with real life"
order: 3
---

Aileron handles things that matter. Your messages, your money, your schedule, your credentials. The way it earns the right to do that is by drawing a hard line between two things current AI architectures usually conflate: **deciding what to do** and **actually doing it.** Language models are great at the first; they shouldn't be trusted with the second. Aileron is the layer that takes the model's proposed action — at the LLM endpoint, before anything irreversible happens — and executes it deterministically, with sealed credentials and your explicit approval where it counts.

Trust isn't "we promise to be careful." It's "the system is designed so that even we can't violate it."

Here's how.

## You are always in control

Nothing happens without your approval. Aileron prepares, assembles, and recommends. You decide. Every message, every payment, every action waits for you to say yes. If you say no, nothing moves. If you edit, Aileron learns. If you walk away, everything stops.

This isn't a safety net bolted on at the end. It's the foundation everything else is built on. The approval flow itself runs on a channel the agent can't tamper with — see [Aileron Control](/control) for the architecture.

## LLMs express intent. Aileron owns execution.

Language models are good at understanding what needs to happen. They are not good at holding your credentials, sending your money, or acting as you. So we separated the two.

The model figures out what you'd probably want to do. Aileron is the one that actually does it. Your credentials, your OAuth tokens, your payment instruments never touch the model. It sees results, not secrets. It can suggest "reply to Sarah's message about the migration," but it cannot access your Slack token, your bank account, or anything else without Aileron brokering the interaction on your behalf, with your approval.

This means a prompt injection, a model hallucination, or a bad suggestion can't do damage. The worst it can do is recommend something you'd decline. The architectural commitments behind this — the connector model, the action model, capability binding, sandboxing — live in the [Pivot Architecture](/architecture/) section.

## The zero-knowledge vault

Your secrets are encrypted before they're stored. The key lives with you, not with us.

When you add a credential to Aileron, it's encrypted using a key derived from your passphrase. We never see the passphrase. We never store the key. Our database holds ciphertext that is useless without you. If someone compromised every server we run, they'd get encrypted blobs and nothing else.

When you need a credential for an action, you unlock your vault once. The decrypted key lives in memory for a session, then it's gone. Your secrets exist in plaintext only for as long as they're needed, and nowhere else.

This isn't new or experimental. Zero-knowledge encryption is the same proven technique behind password managers like 1Password and Bitwarden, secure email like Proton Mail, and end-to-end encrypted messaging like Signal. It's the standard the security community trusts for protecting data that matters.

For users who need a stronger boundary still, sensitive operations can additionally run inside a hardware-isolated enclave whose code is verifiable via remote attestation; see the [TEE Enclave deployment](/deployment/tee-enclave) guide. For everyday use, the vault is the guarantee.

## The personal context store

A language model predicts the next word. That's all it does. Context is the only thing that makes the prediction useful.

Without context, you get generic filler. With the right context, you get something you'd trust with your name on it. The difference isn't the model. It's what the model knows when it runs.

Aileron builds that context around how you actually work and communicate. Your conversations, your projects, your decisions, your preferences. It learns from what you approve, what you edit, and what you throw away. Nobody fills out a profile. Nobody writes a prompt. The context grows because you're living your life, and it gets better every day because it's learning from you.

This is the thing that makes everything else work. The vault protects your credentials. Policy controls what gets to act on your behalf. The context store is what makes the output worth protecting.

## The audit trail

Every action Aileron takes is recorded. Who did what, when, and why. Not just that a message was sent, but which context informed it, which model generated the draft, and that you approved it.

This is accountability. If something goes wrong, there's a complete record. If a regulator asks, there's proof of human control. If you just want to understand what happened last Tuesday, the trail is there.

For organizations, this is how you demonstrate that your people are in control of the tools acting on their behalf. For you, it's the confidence that nothing is happening in the dark.

## Policy as code

Every action Aileron takes on your behalf runs through a policy layer. These are rules you can read, review, and change. They live in a file, not a dashboard. They can be checked into a repository and reviewed in a pull request.

Safe actions are approved automatically. Dangerous actions are blocked. Everything in between asks you once. The rules are transparent, auditable, and yours.

## What this unlocks

These aren't features on a checklist. They're building blocks that solve a single problem: how do you trust a service with your real life?

The zero-knowledge vault means your credentials are safe even if everything else fails. The separation of intent and execution means a bad model output can't cause real harm. The personal context store means every interaction gets better. The audit trail means there's proof. The human in control means nothing consequential happens without you. (For users who need it, optional hardware-isolated enclaves add a stronger boundary still.)

Together, they let Aileron do something no other service can: work with your actual life. Your real messages, your real money, your real relationships, your real schedule. Not a sandbox. Not a demo. The things that matter, handled responsibly, so you can spend your time on what matters more.

A partner to crush the overhead of everyday life and work, and give time back to you where it counts. For the architectural picture behind this, start at [Aileron Runtime](/runtime).
