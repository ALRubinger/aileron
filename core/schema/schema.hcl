// Aileron control plane database schema.
// Managed by Atlas (https://atlasgo.io) — declarative, schema-as-code.
//
// Apply with:
//   atlas schema apply --url "postgres://..." --to "file://core/schema/schema.hcl"

schema "public" {}

table "enterprises" {
  schema = schema.public

  column "id" {
    type = varchar(64)
    null = false
  }
  column "name" {
    type = varchar(255)
    null = false
  }
  column "slug" {
    type = varchar(128)
    null = false
  }
  column "plan" {
    type    = varchar(32)
    null    = false
    default = "free"
  }
  column "billing_email" {
    type = varchar(255)
    null = false
  }
  column "personal" {
    type    = boolean
    null    = false
    default = false
    comment = "True for single-user personal accounts (e.g. Gmail sign-in)"
  }
  column "sso_required" {
    type    = boolean
    null    = false
    default = false
  }
  column "allowed_auth_providers" {
    type    = text
    null    = false
    default = "[]"
    comment = "JSON array of allowed provider names"
  }
  column "allowed_email_domains" {
    type    = text
    null    = false
    default = "[]"
    comment = "JSON array of allowed email domains"
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }
  column "updated_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "idx_enterprises_slug" {
    columns = [column.slug]
    unique  = true
  }
}

table "users" {
  schema = schema.public

  column "id" {
    type    = varchar(64)
    null    = false
    comment = "Surrogate key (usr_ + UUID) — immutable, never changes"
  }
  column "enterprise_id" {
    type = varchar(64)
    null = false
  }
  column "email" {
    type    = varchar(255)
    null    = false
    comment = "Unique email — the stable logical identity across OAuth providers"
  }
  column "display_name" {
    type = varchar(255)
    null = false
  }
  column "avatar_url" {
    type    = text
    null    = false
    default = ""
  }
  column "role" {
    type    = varchar(32)
    null    = false
    default = "member"
  }
  column "status" {
    type    = varchar(32)
    null    = false
    default = "active"
  }
  column "password_hash" {
    type    = text
    null    = false
    default = ""
    comment = "bcrypt hash; empty for OAuth-only users"
  }
  column "last_login_at" {
    type = timestamptz
    null = true
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }
  column "updated_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  foreign_key "fk_users_enterprise" {
    columns     = [column.enterprise_id]
    ref_columns = [table.enterprises.column.id]
    on_delete   = CASCADE
  }

  index "idx_users_email" {
    columns = [column.email]
    unique  = true
  }

  index "idx_users_enterprise" {
    columns = [column.enterprise_id]
  }

}

table "user_auth_providers" {
  schema = schema.public

  column "id" {
    type = varchar(64)
    null = false
  }
  column "user_id" {
    type = varchar(64)
    null = false
  }
  column "provider" {
    type    = varchar(64)
    null    = false
    comment = "OAuth provider name (google, github, okta, saml, etc.)"
  }
  column "subject_id" {
    type    = varchar(255)
    null    = false
    comment = "External IdP subject identifier"
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  foreign_key "fk_user_auth_providers_user" {
    columns     = [column.user_id]
    ref_columns = [table.users.column.id]
    on_delete   = CASCADE
  }

  index "idx_user_auth_providers_user" {
    columns = [column.user_id]
  }

  index "idx_user_auth_providers_provider_subject" {
    columns = [column.provider, column.subject_id]
    unique  = true
  }
}

table "sessions" {
  schema = schema.public

  column "id" {
    type = varchar(64)
    null = false
  }
  column "user_id" {
    type = varchar(64)
    null = false
  }
  column "token_hash" {
    type = varchar(128)
    null = false
  }
  column "refresh_token_hash" {
    type = varchar(128)
    null = false
  }
  column "expires_at" {
    type = timestamptz
    null = false
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  foreign_key "fk_sessions_user" {
    columns     = [column.user_id]
    ref_columns = [table.users.column.id]
    on_delete   = CASCADE
  }

  index "idx_sessions_token_hash" {
    columns = [column.token_hash]
    unique  = true
  }

  index "idx_sessions_refresh_token_hash" {
    columns = [column.refresh_token_hash]
    unique  = true
  }

  index "idx_sessions_user" {
    columns = [column.user_id]
  }
}

table "verification_codes" {
  schema = schema.public

  column "id" {
    type = varchar(64)
    null = false
  }
  column "user_id" {
    type = varchar(64)
    null = false
  }
  column "code_hash" {
    type    = varchar(128)
    null    = false
    comment = "SHA-256 hash of the 6-digit verification code"
  }
  column "expires_at" {
    type = timestamptz
    null = false
  }
  column "used" {
    type    = boolean
    null    = false
    default = false
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  foreign_key "fk_verification_codes_user" {
    columns     = [column.user_id]
    ref_columns = [table.users.column.id]
    on_delete   = CASCADE
  }

  index "idx_verification_codes_user" {
    columns = [column.user_id]
  }
}

table "user_key_materials" {
  schema = schema.public

  column "user_id" {
    type    = varchar(64)
    null    = false
    comment = "Owning user (usr_ + UUID)"
  }
  column "salt" {
    type    = bytea
    null    = false
    comment = "Argon2id salt for KEK derivation"
  }
  column "kek_verification" {
    type    = bytea
    null    = false
    comment = "Known constant encrypted with KEK — used to verify passphrase without revealing KEK"
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }
  column "updated_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.user_id]
  }

  foreign_key "fk_user_key_materials_user" {
    columns     = [column.user_id]
    ref_columns = [table.users.column.id]
    on_delete   = CASCADE
  }
}

table "sso_configs" {
  schema = schema.public

  column "id" {
    type = varchar(64)
    null = false
  }
  column "enterprise_id" {
    type = varchar(64)
    null = false
  }
  column "provider" {
    type = varchar(64)
    null = false
  }
  column "client_id" {
    type = varchar(512)
    null = false
  }
  column "issuer_url" {
    type    = text
    null    = false
    default = ""
  }
  column "enabled" {
    type    = boolean
    null    = false
    default = true
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }
  column "updated_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  foreign_key "fk_sso_configs_enterprise" {
    columns     = [column.enterprise_id]
    ref_columns = [table.enterprises.column.id]
    on_delete   = CASCADE
  }

  index "idx_sso_configs_enterprise" {
    columns = [column.enterprise_id]
  }

  index "idx_sso_configs_enterprise_provider" {
    columns = [column.enterprise_id, column.provider]
    unique  = true
  }
}

table "connected_accounts" {
  schema = schema.public

  column "id" {
    type    = varchar(64)
    null    = false
    comment = "Surrogate key (conn_ + UUID)"
  }
  column "user_id" {
    type = varchar(64)
    null = false
  }
  column "provider" {
    type    = varchar(64)
    null    = false
    comment = "External service provider (gmail, slack, github_repos, etc.)"
  }
  column "scopes" {
    type    = text
    null    = false
    default = "[]"
    comment = "JSON array of granted OAuth scopes"
  }
  column "status" {
    type    = varchar(32)
    null    = false
    default = "active"
  }
  column "external_user_id" {
    type    = varchar(255)
    null    = false
    default = ""
    comment = "Provider-specific user ID (e.g. Slack U...)"
  }
  column "external_team_id" {
    type    = varchar(255)
    null    = false
    default = ""
    comment = "Provider-specific workspace/team ID (e.g. Slack T...)"
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }
  column "updated_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  foreign_key "fk_connected_accounts_user" {
    columns     = [column.user_id]
    ref_columns = [table.users.column.id]
    on_delete   = CASCADE
  }

  index "idx_connected_accounts_user" {
    columns = [column.user_id]
  }

  index "idx_connected_accounts_user_provider" {
    columns = [column.user_id, column.provider]
    unique  = true
  }
}

table "drafts" {
  schema = schema.public

  column "id" {
    type    = varchar(64)
    null    = false
    comment = "Surrogate key (dft_ + UUID)"
  }
  column "user_id" {
    type = varchar(64)
    null = false
  }
  column "status" {
    type    = varchar(32)
    null    = false
    default = "pending"
  }
  column "service" {
    type    = varchar(64)
    null    = false
    comment = "Source service (slack, discord, etc.)"
  }
  column "channel" {
    type = varchar(255)
    null = false
  }
  column "author" {
    type = varchar(255)
    null = false
  }
  column "message_body" {
    type = text
    null = false
  }
  column "message_ts" {
    type    = varchar(64)
    null    = false
    comment = "Original message timestamp for threading"
  }
  column "draft_body" {
    type = text
    null = false
  }
  column "sent_body" {
    type    = text
    null    = false
    default = ""
    comment = "What was actually sent; empty until approved or edited"
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }
  column "updated_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  foreign_key "fk_drafts_user" {
    columns     = [column.user_id]
    ref_columns = [table.users.column.id]
    on_delete   = CASCADE
  }

  index "idx_drafts_user" {
    columns = [column.user_id]
  }
}

table "user_instructions" {
  schema = schema.public

  column "id" {
    type    = varchar(64)
    null    = false
    comment = "Surrogate key (ins_ + UUID)"
  }
  column "user_id" {
    type = varchar(64)
    null = false
  }
  column "body" {
    type = text
    null = false
  }
  column "scope" {
    type    = text
    null    = false
    default = ""
    comment = "Optional free-text context: channel, person, topic, or empty for global"
  }
  column "active" {
    type    = boolean
    null    = false
    default = true
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }
  column "updated_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  foreign_key "fk_user_instructions_user" {
    columns     = [column.user_id]
    ref_columns = [table.users.column.id]
    on_delete   = CASCADE
  }

  index "idx_user_instructions_user" {
    columns = [column.user_id]
  }
}

table "draft_feedback" {
  schema = schema.public

  column "id" {
    type    = varchar(64)
    null    = false
    comment = "Surrogate key (fb_ + UUID)"
  }
  column "user_id" {
    type = varchar(64)
    null = false
  }
  column "draft_id" {
    type    = varchar(64)
    null    = false
    comment = "The draft this feedback is for"
  }
  column "signal" {
    type    = varchar(32)
    null    = false
    comment = "approved, edited, or discarded"
  }
  column "service" {
    type    = varchar(64)
    null    = false
  }
  column "channel" {
    type = varchar(255)
    null = false
  }
  column "draft_body" {
    type = text
    null = false
  }
  column "sent_body" {
    type    = text
    null    = false
    default = ""
    comment = "What was actually sent; empty if discarded"
  }
  column "tools_used" {
    type    = text
    null    = false
    default = "[]"
    comment = "JSON array of source connector tools called during generation"
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  foreign_key "fk_draft_feedback_user" {
    columns     = [column.user_id]
    ref_columns = [table.users.column.id]
    on_delete   = CASCADE
  }

  index "idx_draft_feedback_user" {
    columns = [column.user_id]
  }
}

table "vault_secrets" {
  schema = schema.public

  column "path" {
    type    = varchar(512)
    null    = false
    comment = "Vault path (e.g. connected-accounts/usr_xxx/slack)"
  }
  column "value" {
    type    = bytea
    null    = false
    comment = "AES-256-GCM encrypted secret value (nonce || ciphertext || tag)"
  }
  column "metadata" {
    type    = jsonb
    null    = false
    default = "{}"
    comment = "Non-secret attributes: type, labels, environment"
  }

  check "vault_secrets_encrypted" {
    expr    = "metadata->'labels'->>'encrypted' = 'true'"
    comment = "All vault secrets must be encrypted — plaintext storage is not allowed"
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }
  column "updated_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.path]
  }
}

table "llm_configs" {
  schema = schema.public

  column "id" {
    type = varchar(64)
    null = false
  }
  column "owner_type" {
    type    = varchar(32)
    null    = false
    comment = "user or enterprise"
  }
  column "owner_id" {
    type    = varchar(64)
    null    = false
    comment = "usr_xxx or enterprise ID"
  }
  column "provider" {
    type    = varchar(32)
    null    = false
    comment = "anthropic, openai, etc."
  }
  column "model_research" {
    type    = varchar(128)
    null    = false
    default = ""
  }
  column "model_synthesis" {
    type    = varchar(128)
    null    = false
    default = ""
  }
  column "created_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }
  column "updated_at" {
    type    = timestamptz
    null    = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "idx_llm_configs_owner" {
    columns = [column.owner_type, column.owner_id]
    unique  = true
  }
}
