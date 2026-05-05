---
title: "Privacy Policy"
description: "Aileron is open source and runs locally on your computer. We don't see your data, because we're not in the loop."
---

*Effective: May 5, 2026*

## The short version

Aileron is an open-source project, maintained by a project lead — no company runs it. The purpose of Aileron is to give you security and confidence that no agent, no LLM, and not Aileron itself has direct access to your protected information beyond what you explicitly provide.

- Aileron runs entirely on your computer.
- Your credentials live in a local, encrypted vault. They never reach Aileron, your LLM, or anyone else.
- The data your agent works with — emails, files, calendar events — flows between you, the services you've connected, and the LLM provider you've configured. Aileron has no servers in that data path.
- We don't want or collect your personal information.
- You are not the product.
- The Aileron website and runtime use standard, anonymous web analytics. That's it.

## What this policy covers

This policy covers the Aileron desktop application, the `aileron` CLI, and the `aileron.sh` website. It does **not** cover services you connect *through* Aileron — Gmail, Google Drive, Google Calendar, Slack, GitHub, or any other third party. Those services have their own privacy policies; the data you exchange with them is governed by their terms.

## The desktop app and CLI

When you install and run Aileron on your computer:

- Your credential vault is stored locally and encrypted.
- OAuth tokens for connected services are held in that vault.
- Actions run on your machine. The data they read or write flows directly between you and the third-party service (Gmail, Slack, etc.) — Aileron has no servers in that data path.

Your agent itself runs through whichever LLM provider you've configured (Anthropic, OpenAI, etc.). The provider sees the prompts you write and the action results your agent processes — that's how agentic LLMs work, with or without Aileron. Their privacy policies govern what happens to your data once it reaches them.

## Anonymous analytics

The Aileron website and the local runtime use standard, anonymous analytics — page views or event names, plus aggregate usage metrics like which actions are run and how often. The website's analytics use [PostHog](https://posthog.com). No personally identifying information is captured beyond what's already present in any web request (IP address, user agent, the page or event name).

No content of your work is captured. No prompts, no action arguments, no action results, no Google data, no vault contents.

## How we handle Google user data

When you connect Gmail, Google Drive, or Google Calendar, Aileron uses Google's OAuth flow to obtain tokens that let it act on your behalf. Those tokens are stored in your local, encrypted vault — Aileron never holds them on any server.

Aileron's use of information received from Google APIs adheres to the [Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy), including the Limited Use requirements:

- **Limited use.** Data obtained through Google APIs is used only to provide or improve user-facing features that are visible and prominent in Aileron — for example, drafting an email you asked your agent to send, or summarizing a calendar event you asked it to read.
- **No transfer.** Google user data is not transferred to third parties except as necessary to provide those user-facing features, to comply with applicable law, or as part of a merger, acquisition, or sale of assets where the new owner inherits this commitment.
- **No advertising.** Google user data is never used to serve ads.
- **No human reading.** Humans do not read Google user data, except: with your explicit consent; for security purposes such as investigating abuse; to comply with applicable law; or where the data has been aggregated and anonymized for internal operations.
- **No sale.** Google user data is never sold.

Because Aileron runs on your computer, the practical effect of all of these commitments is that Google user data flows directly between Google and your machine. We are never in the path.

## Children's privacy

Aileron is not intended for children under 13, and we do not knowingly collect data from them.

## Changes to this policy

If this policy changes in a material way, we'll update the Effective date at the top and post a note on the homepage. Smaller corrections (typos, clarifications) we may make without notice.

## Contact

Questions about this policy or how Aileron handles your data: [alr@alrubinger.com](mailto:alr@alrubinger.com).

---

*Structure adapted from [Basecamp's open-source policies](https://github.com/basecamp/policies) under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/). The Limited Use language is taken from the [Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy).*
