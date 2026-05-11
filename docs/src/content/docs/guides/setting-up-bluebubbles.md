---
title: "Setting up BlueBubbles for Aileron"
description: "Prerequisite for the iMessage connector: install BlueBubbles Server on your Mac, grant the OS permissions it needs, and verify Aileron can reach it."
---

The Aileron iMessage connector doesn't touch your Mac's Messages app directly. Instead it talks to **BlueBubbles**, a free open-source server you run locally. BlueBubbles handles the platform-specific work (reading the iMessage history, sending new messages, listening for incoming ones) and exposes a simple HTTP API on your machine. Aileron talks to that API.

This separation matters in two ways. First, **you grant macOS permissions to BlueBubbles, not to Aileron**. BlueBubbles is purpose-built for this; Aileron stays out of the operating system's privileged surface. Second, **if Apple ever changes how iMessage data is stored**, BlueBubbles' maintainers handle that — Aileron's connector keeps working without changes.

You only need to do this setup once per Mac.

## Before you start

- A Mac that's already signed in to iMessage and receiving messages normally.
- About 5 minutes.
- You'll create one password during this setup. Pick something memorable; you'll give it to Aileron later as a credential.

## 1. Install BlueBubbles Server

Download the Mac app from [bluebubbles.app](https://bluebubbles.app/downloads/) and drag it to your Applications folder. Launch it from there (not from the downloads folder, so macOS treats it as a normal app).

On first launch, BlueBubbles walks you through a setup wizard. You only need the first two steps for Aileron's purposes:

1. **Set a server password.** Choose something strong; this is the only thing standing between anyone on your local network and your iMessage history. Save it in your password manager.
2. **Skip the Firebase / Google setup** unless you separately want BlueBubbles' phone-app features. Aileron doesn't use them.

Leave BlueBubbles Server running. It lives in your menu bar.

## 2. Grant the macOS permissions BlueBubbles needs

BlueBubbles needs two macOS permissions to do its work. macOS prompts for both the first time BlueBubbles needs them; if you missed the prompt, you can grant them by hand:

- **Full Disk Access**. Open *System Settings → Privacy & Security → Full Disk Access*. Toggle BlueBubbles Server **on**. This lets BlueBubbles read your iMessage database.
- **Automation**. Open *System Settings → Privacy & Security → Automation*. Find BlueBubbles Server in the list and check the box next to **Messages**. This lets BlueBubbles send new messages on your behalf.

Quit and relaunch BlueBubbles Server after granting either of these. macOS is picky about when permission changes take effect.

## 3. Confirm BlueBubbles is reachable

BlueBubbles Server runs an HTTP API on `localhost:1234` by default. Open a terminal and ask it for its status:

```sh
curl http://localhost:1234/api/v1/server/info?password=YOUR_BLUEBUBBLES_PASSWORD
```

If the response is a JSON object describing your server (version, OS, iMessage status), you're done. If you get a connection error, BlueBubbles isn't running; relaunch it from Applications.

If you changed the default port during setup, substitute that port number in the URL above and remember it for the next step.

## 4. Install Aileron's iMessage connector

```sh
aileron connector install github://ALRubinger/aileron-connector-bluebubbles@latest
```

Aileron prompts you for the BlueBubbles server password you set in step 1. Paste it in. The credential lands in your Aileron vault encrypted; Aileron's connector reads it only at call time, and the bytes never reach the agent.

If your BlueBubbles is on a non-default port, you'll see a follow-up prompt for the URL. Otherwise the default `http://localhost:1234` is used.

## 5. Try it

Install one of the iMessage actions, then ask your agent something simple:

```sh
aileron action add github://ALRubinger/aileron-connector-bluebubbles/actions/list-recent-chats@latest
aileron launch claude
```

> *"List the iMessage conversations I've had in the last week."*

The agent should return a list of recent chats. If you see a `bridge_unreachable` error, jump to the troubleshooting section below.

## Troubleshooting

**`bridge_unreachable`**: BlueBubbles Server isn't running. Open Applications and relaunch it. Confirm the menu bar icon is present and that `curl http://localhost:1234/api/v1/server/info?password=...` returns JSON.

**`permission_denied` or similar OS-level error**: You probably granted BlueBubbles Full Disk Access but not Automation (or vice versa). Re-check both *System Settings → Privacy & Security → Full Disk Access* and *Automation* per step 2, then relaunch BlueBubbles.

**`unauthorized` from BlueBubbles**: The password Aileron is sending doesn't match the one BlueBubbles expects. Run `aileron binding setup github://ALRubinger/aileron-connector-bluebubbles` to re-bind with the correct password.

**Wrong port**: If you changed BlueBubbles' port during setup, Aileron needs to know. Update the URL by re-running `aileron binding setup` for the connector and providing the new URL when prompted.

## What this gives the agent

With this setup, your agent can:

- **Read** recent iMessage conversations and individual messages.
- **Send** new iMessages to existing chats.
- **List** the chats you're part of.

Every send goes through Aileron's standard approval gate; you confirm the recipient and message body before BlueBubbles actually delivers it. Reads happen without prompting because they're scoped to your own message history.

The full set of operations the connector exposes is listed in its [hub entry](https://hub.aileron.dev/imsg).

## What this does *not* do

- **Not a phone bridge.** You can use BlueBubbles' own iOS/Android app features alongside this if you want, but Aileron doesn't need them. Skip Firebase / Google during BlueBubbles' setup wizard.
- **Not for received-message webhooks.** v1 of Aileron's iMessage connector is pull-only. Reacting to new incoming messages in real time is a v3 conversation.
- **Not a Mac-less option.** BlueBubbles needs the Mac to be running and signed in to iMessage. There is no way around this; iMessage is Apple-only by design.

## See also

- [BlueBubbles documentation](https://bluebubbles.app/docs/)
- [Installing a Connector](/guides/installing-a-connector/)
- [Wrapping a CLI](/guides/wrapping-a-cli/) — for the spawn-primitive pattern used by other connectors (not this one; iMessage uses HTTP instead).
