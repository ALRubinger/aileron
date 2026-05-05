---
title: "Privacy Policy"
description: "Aileron is open source and runs locally on your computer. We don't capture or store your data — and you can read the code to verify it."
---

## The short version

Aileron is an open-source project, stewarded by its creator. No company or organization runs it. The purpose of Aileron is to give you security and confidence that no agent, no LLM, and not Aileron itself has direct access to your protected information beyond what you explicitly provide.

- Aileron runs entirely on your computer.
- Your credentials live in a local, encrypted vault. They never reach Aileron, your LLM, or anyone else.
- The data your agent works with (emails, files, calendar events) passes through Aileron on the way to the services you've connected and the LLM provider you've configured. Aileron does not capture or store any of it.
- Aileron is open source. You can read the code and verify these claims for yourself.
- We don't want or collect your personal information.
- You are not the product.
- The Aileron website and runtime use standard, anonymous web analytics. That's it.

## What this policy covers

This policy covers the Aileron runtime (the server daemon, the `aileron` CLI, and the local webapp, all running on your machine) and the `withaileron.ai` website. It does **not** cover services you connect *through* Aileron, like Gmail, Google Drive, Google Calendar, Slack, or GitHub. Those services have their own privacy policies; the data you exchange with them is governed by their terms.

## The Aileron runtime

When you run Aileron on your computer (the server daemon started by `aileron launch`, the `aileron` CLI, and the local webapp):

- Your credential vault is stored locally and encrypted.
- OAuth tokens for connected services are held in that vault.
- Actions run on your machine. They call third-party services (Gmail, Slack, and so on) directly from your computer, using credentials from your local vault.

Your agent talks to your chosen LLM provider (Anthropic, OpenAI, etc.) through Aileron's LLM Gateway, which runs locally as part of `aileron launch`. The gateway proxies the agent's requests to the provider. The provider sees the prompts you write and the action results your agent processes; that's how agentic LLMs work, with or without Aileron. Their privacy policies govern what happens to your data once it reaches them.

Aileron does not capture or store any of the traffic flowing through these local components. Aileron is open source, so you can verify that for yourself.

## Anonymous analytics

The Aileron website and the local runtime use standard, anonymous analytics: page views or event names, plus aggregate usage metrics like which actions are run and how often. No personally identifying information is captured beyond what's already present in any web request (IP address, user agent, the page or event name).

No content of your work is captured. No prompts, no action arguments, no action results, no contents from connected services, no vault contents.

## Data from connected services

When you connect a service (Gmail, Google Drive, Slack, GitHub, etc.), Aileron uses OAuth to obtain tokens that let it act on your behalf. Those tokens are stored in your local, encrypted vault. The data your agent fetches from these services is not captured or stored by Aileron.

### Google services

Google requires apps that access Gmail, Drive, or Calendar to publish how they use Google user data. Aileron's use of information received from Google APIs adheres to the [Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy), including the Limited Use requirements:

- **Limited use.** Data obtained through Google APIs is used only to provide or improve user-facing features that are visible and prominent in Aileron, like drafting an email you asked your agent to send, or summarizing a calendar event you asked it to read.
- **No transfer.** Google user data is not transferred to third parties except as necessary to provide those user-facing features, to comply with applicable law, or as part of a merger, acquisition, or sale of assets where the new owner inherits this commitment.
- **No advertising.** Google user data is never used to serve ads.
- **No human reading.** Humans do not read Google user data, except: with your explicit consent; for security purposes such as investigating abuse; to comply with applicable law; or where the data has been aggregated and anonymized for internal operations.
- **No sale.** Google user data is never sold.

Because Aileron runs on your computer, Google user data goes directly between Google and your machine. None of it is captured or stored by Aileron.

## Children's privacy

Aileron is not intended for children under 13, and we do not knowingly collect data from them.

## Contact

Questions about this policy or how Aileron handles your data: [fly@withaileron.ai](mailto:fly@withaileron.ai).

---

*Structure adapted from [Basecamp's open-source policies](https://github.com/basecamp/policies) under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/). The Limited Use language is taken from the [Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy).*
