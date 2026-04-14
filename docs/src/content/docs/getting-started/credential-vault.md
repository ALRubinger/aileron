---
title: "Credential Vault"
description: "Store and broker secrets securely"
---

Aileron's credential vault uses a zero-knowledge architecture. Your secrets are encrypted with a key derived from a passphrase that only you know. Aileron stores the encrypted ciphertext and the Argon2id salt, never the key itself.

## How it works

1. You set a vault passphrase. Aileron derives a 256-bit Key Encryption Key (KEK) using Argon2id, stores only the salt and a verification blob, then discards the KEK from memory.
2. When you store a secret, it's encrypted with AES-256-GCM using your KEK before storage. The database holds only ciphertext.
3. To use credentials, you verify your passphrase to unlock a time-limited session (default: 24 hours). The KEK is held in memory only for the session duration.
4. When an agent triggers an action that needs a credential, Aileron decrypts it, makes the call, and discards the plaintext.

## Managing secrets

```sh
aileron secret set slack_bot_token    # prompts for passphrase + token
aileron secret set my_api_key
aileron secret list                   # shows stored names (not values)
```

## Referencing secrets in config

Secrets are referenced in `aileron.yaml` using `vault:` prefixes:

```yaml
notifications:
  slack:
    bot_token: vault:slack_bot_token
```

Aileron rejects plaintext tokens in config files. This prevents secrets from being committed to version control.

## Credential brokering

The agent never sees raw credentials. When the agent needs to call an external API, it provides a URL and headers. Aileron matches the URL to a configured secret, injects the credential, makes the call, and returns the response. The agent sees the result, never the secret.

This means prompt injection can't leak what the agent doesn't have.

## What a breach yields

- A database breach yields only ciphertext, useless without your passphrase.
- Aileron operators cannot read your credentials, even with full database access.
- Hosting providers see only encrypted data.

For the full trust model, see [How Aileron Works](/how-aileron-works) and [ADR-0010: Zero-Knowledge Vault](/adr/0010-zero-knowledge-vault-trust-model).
