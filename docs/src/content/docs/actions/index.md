---
title: "Actions"
description: "The actions Aileron ships ready-to-install, grouped into suites by the service they integrate with."
order: 0
---

An action is a single capability your agent can call — read recent emails, send a message, list calendar events. Aileron ships actions in **suites**, grouped by the service they integrate with. Each suite has its own page here covering install commands, prerequisites, and a description of every action the suite provides.

If you're new to Aileron, [the Actions concept page](/concepts/actions/) explains what an action is and how the runtime enforces capability bounds when one runs. This section is the catalog: what's available, how to install it, what each action does, and which inputs the agent will fill in.

## Suites

### [Gmail and Google Calendar](/actions/gmail-and-google-calendar/)

Read and search Gmail, draft and send email, list and create calendar events. Six actions; send and create are approval-gated.

### [iMessage via BlueBubbles](/actions/imessage-via-bluebubbles/)

Read recent iMessage conversations and send new ones through a local [BlueBubbles](https://bluebubbles.app/) bridge. Three actions; send is approval-gated. Requires a one-time BlueBubbles install on your Mac per the [setup guide](/guides/setting-up-bluebubbles/).

## How install commands work

Every suite page leads with two install paths:

- **Install the whole suite.** One `aileron action add-suite` command. The CLI pulls the suite manifest at the named release tag, prompts to trust the publisher's signing key the first time you install from that publisher, installs every action in declaration order with a single install-consent prompt, and at the end prompts you to bind the credential the connector needs (an OAuth account for Gmail, a server password for BlueBubbles).
- **Install individual actions.** One `aileron action add` command per action, useful when you only want a subset. The first `add` for a given publisher walks through the trust and binding flows; subsequent ones reuse those grants.

Both paths produce the same result for the actions they install. The suite path is shorter; the individual path lets you cherry-pick. Trust grants and credential bindings carry across both. Once installed, the actions are available to the agent from the next `aileron launch` onward.

## Authoring your own

If you want to package a different service or wrap a local CLI:

- [Authoring a Connector](/guides/authoring-a-connector/) — write a connector from scratch in Go, targeting `wasip1`.
- [Authoring an Action](/guides/authoring-an-action/) — build an action.md against an existing connector.
- [Authoring an Action Suite](/guides/authoring-an-action-suite/) — bundle related actions into an installable suite.
- [Wrapping a CLI](/guides/wrapping-a-cli/) — the shortcut for the long tail of POSIX CLIs you want to give your agent access to.
- [Publishing a Connector](/guides/publishing-a-connector/) — release a connector under your own FQN so others can install it.
