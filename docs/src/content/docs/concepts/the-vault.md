---
title: "The Vault"
description: "How Aileron protects your credentials. A local encrypted store, unlocked by a passphrase you control, with credentials that connectors are designed to never see."
order: 4
---

The **vault** is where Aileron stores your credentials — OAuth tokens for Slack, API keys for Linear, refresh tokens for Gmail, and so on. It lives in your home directory, encrypted at rest with a passphrase you control. The runtime can only reach the credentials inside it while a process you started is running; nothing on disk is recoverable without your passphrase.

If [actions](/concepts/actions/) are *what* the agent can do and [connectors](/concepts/connectors/) are *how* it gets done, the vault is *what makes that safe*. Without it, every credential the agent might use would have to live somewhere — in environment variables, in a config file, in the agent host's process memory. The vault is the answer to "where do these credentials actually go."

## The shape of it

The vault is a single encrypted file at `~/.aileron/secrets.json`. Each entry is a credential bound to a name you chose:

```
oauth2/slack/work          ← OAuth2 token for your work Slack
oauth2/slack/personal      ← OAuth2 token for your personal Slack
api_key/linear/team        ← API key for your team's Linear
oauth2/gmail/work          ← OAuth2 refresh token for work Gmail
```

These names — *binding identities* — are how actions and connectors reach into the vault. An action that posts to Slack doesn't say "use this specific token"; it says "I need an OAuth2 credential with the `chat:write` scope," and the vault resolves the request to a concrete binding the user picked.

You see the binding identities in `aileron binding list`. You set them up with `aileron binding setup` (proactively) or at first use (when an action invokes a connector that needs a credential you haven't bound yet). The full lifecycle is covered in [ADR-0006: Capability Binding UX](/adr/0006-capability-binding-ux).

## The passphrase

The vault is encrypted with a key derived from a passphrase you chose. The first time you run Aileron, it asks you to set one:

```
$ aileron launch
No vault detected. Create one now? [Y/n] Y
Choose a passphrase to protect your credentials:
> ********
Confirm passphrase:
> ********

Deriving encryption key (Argon2id, 64 MiB memory, ~500ms)...  ✓
✓ Vault created at ~/.aileron/secrets.json
```

Every time you start Aileron after that, you re-enter the passphrase to unlock the vault:

```
$ aileron launch
Vault is encrypted. Enter passphrase to unlock:
> ********

Verifying passphrase...  ✓
✓ Vault unlocked
✓ Listening on http://localhost:8721/v1
```

The passphrase **never goes to disk**. The runtime derives an encryption key from it (using [Argon2id](https://datatracker.ietf.org/doc/html/rfc9106), the modern password-hashing standard), holds the key in memory, and uses it to decrypt credentials when the agent needs them. When the runtime exits, the key goes with it. Next time you start, you re-derive.

## What the vault protects you from

The threat model the vault is designed for:

- **Disk theft.** Your laptop is stolen. The attacker has your `~/.aileron/secrets.json` file but not your passphrase. Without the passphrase, the file is opaque. Brute-forcing the passphrase against Argon2id is intentionally slow.
- **Backup leaks.** Your `~/.aileron/` directory is in a cloud backup. The same property holds: the file is encrypted; the backup provider sees ciphertext.
- **Process inspection by other processes.** A second process running as your user can read your home directory, but cannot reach the in-memory key inside the running Aileron process.
- **Connector compromise.** Even a malicious connector binary cannot reach the vault. The runtime mediates every credential use; connectors receive opaque capability handles, not credential bytes (see [Connectors](/concepts/connectors/) and [ADR-0005](/adr/0005-sandbox-choice)).

## What the vault is honest about

Some things the vault does *not* protect you from:

- **A compromised running process.** While Aileron is running with the vault unlocked, the encryption key is in process memory. A privileged attacker who can read Aileron's memory can extract it. (The post-MVP TEE-backed variant addresses this; v1 doesn't.)
- **A weak passphrase.** Argon2id makes brute-forcing slow but not impossible. A short or guessable passphrase undermines the protection.
- **Phishing or social engineering.** If you're tricked into entering your passphrase into something that isn't Aileron, the vault doesn't help.

The right framing is: the vault is strong protection at rest and a meaningful boundary while running, but it's not magic. Choose a strong passphrase.

## How a credential travels

This is the path a credential takes when an action invokes a connector that needs it:

1. **Action invocation.** The agent calls an action, say `ship-update`. The action declares it needs `oauth2(scope=chat:write)`.
2. **Binding resolution.** The runtime looks up the user's binding for that capability — say, `oauth2/slack/work`.
3. **Credential decryption.** The runtime decrypts the credential value from the vault using the in-memory key.
4. **Capability handle.** The runtime issues an opaque handle to the connector (not the credential itself).
5. **Connector emits a request.** The connector formats an HTTP request with placeholder authorization, including the handle.
6. **Runtime intercepts and signs.** The runtime sees the outgoing request, resolves the handle to the actual credential, attaches the auth header, and forwards the request.
7. **Response returns.** The connector sees the API response; never sees the credential bytes.
8. **Connector terminates.** The instance exits; any handles it held become invalid immediately.

The connector's only reach into the credential world is through this mediated path. There is no `vault.read("...")` API, no host function that exposes credential material, no way for the connector to ask for raw bytes. By design.

## What's encrypted, what's not

Inside the vault file, only the credential *values* are encrypted. The metadata that helps you reason about your bindings — the kind, the scope, the identity — is plaintext:

```json
{
  "oauth2/slack/work": {
    "metadata": {
      "kind": "oauth2",
      "scope": "chat:write,channels:read",
      "encrypted": "true"
    },
    "ciphertext": "<encrypted credential value>"
  }
}
```

This is intentional. `aileron binding list` should work without prompting for the passphrase — listing the names of your bindings doesn't require decryption. Only when an action actually needs to *use* a credential does the runtime decrypt.

## When you forget the passphrase

There is no recovery in v1. A forgotten passphrase means the vault contents are unrecoverable. You'd need to:

1. Rotate every credential at the upstream service (revoke OAuth tokens at Slack admin, reissue API keys at Linear, etc.).
2. Delete `~/.aileron/secrets.json`.
3. Re-create the vault with a new passphrase.
4. Re-bind every credential through `aileron binding setup` or first-use binding.

This is the honest trade-off for zero-knowledge encryption. Adding a recovery mechanism — escrow, recovery codes, anything — would mean *something* in the system holds enough information to recover your credentials, which is exactly what we're trying to prevent. Recovery codes might land post-MVP; v1 ships without.

## Beyond v1: where the vault is going

The v1 design (a local encrypted file with a passphrase-derived key) is what ships today. The same on-disk format and cryptographic primitives run unchanged when the customer operates the runtime in their own infrastructure (BYOC).

Under BYOC the runtime that decrypts credentials runs in the customer's own environment. Aileron never holds the KEK or the plaintext, because the process that performs decryption is operated by the customer rather than by Aileron. The vault file does not change. The party that controls the decrypting process is the customer.
