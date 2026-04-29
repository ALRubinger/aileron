---
title: "Project Portability"
description: "Action files travel; credentials don't. Personal bindings, `aileron sync`, and Control federation for shared credentials"
order: 11
---

> **Architecture:** part of the [Architecture](/architecture/) section of the Pivot strategy. See also [The Action Model](/architecture/action-model), [Two Distribution Mechanics](/architecture/distribution-mechanics), and [Capability Binding UX](/architecture/capability-binding).

Four architectural commitments shape how project state survives across machines and team members. There is no separate lock file — the action files themselves are the contract. The detailed `aileron sync` UX, prompt sequences, conflict resolution, and Control-federation interactions defer to the project-portability ADR.

**1. Action files are committable; credentials are not.** Action files (`actions/*.md`) live in the project repo, version-controlled by git. They contain everything needed to reproduce the project's action behavior: connector names, exact versions, hashes, declared capabilities, execution steps. They do not contain credentials or anything secret. Anything secret stays in the developer's local vault.

**2. Bindings are personal; intent is shared.** Action files reference bindings by name (`slack/work`); each developer's local vault holds *their own* credential under that name. The team aligns on intent (which workspace conceptually, which environment); each developer's identity remains private. Two developers running the same project from the same action files each post to their own Slack via their own credentials — same name, different bound resource.

**3. `aileron sync` is the guided-setup primitive.** A teammate cloning the repo runs one command. Aileron reads the action files, identifies missing connectors and unbound credentials, walks the developer through installing missing connectors (verifying hashes match the declarations in the action files), and completes the OAuth flows they need to bind their own credentials. Standard `npm ci`-shape — but driven by the action files themselves, not a separate lock file.

**4. Shared credentials federate through Control.** For service accounts and organization-shared credentials (CI bots, automation identities, shared workspace tokens), Aileron Control hosts the binding centrally. The action files reference the same binding name; resolution differs depending on whether the developer's machine is signed into Control. The same project files work whether the developer is solo, on a team using personal bindings, or on a team using federated org-shared bindings — without changing how the action files are written.

The detailed policy — exact `sync` UX, conflict resolution when actions disagree on connector versions, prompt sequences for OAuth in restrictive workspaces, how Control federation handles permission changes, the precise behavior when an action file's hash doesn't match what the Hub serves — defers to the project-portability ADR. These four commitments set the architectural posture; the ADR fills in the operational details with concrete implementation experience.
