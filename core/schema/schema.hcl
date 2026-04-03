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
  column "auth_provider" {
    type = varchar(64)
    null = false
  }
  column "auth_provider_subject_id" {
    type    = varchar(255)
    null    = false
    default = ""
    comment = "External IdP subject identifier; empty for email auth"
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

  index "idx_users_provider_subject" {
    columns = [column.auth_provider, column.auth_provider_subject_id]
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
