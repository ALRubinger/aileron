//go:build integration

package pgtest

// FullSchema is the SQL DDL for the complete Aileron database schema.
// It is translated from core/schema/schema.hcl and must be kept in sync.
const FullSchema = `
CREATE TABLE enterprises (
	id varchar(64) NOT NULL PRIMARY KEY,
	name varchar(255) NOT NULL,
	slug varchar(128) NOT NULL,
	plan varchar(32) NOT NULL DEFAULT 'free',
	billing_email varchar(255) NOT NULL,
	personal boolean NOT NULL DEFAULT false,
	sso_required boolean NOT NULL DEFAULT false,
	allowed_auth_providers text NOT NULL DEFAULT '[]',
	allowed_email_domains text NOT NULL DEFAULT '[]',
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_enterprises_slug ON enterprises(slug);

CREATE TABLE users (
	id varchar(64) NOT NULL PRIMARY KEY,
	enterprise_id varchar(64) NOT NULL REFERENCES enterprises(id) ON DELETE CASCADE,
	email varchar(255) NOT NULL,
	display_name varchar(255) NOT NULL,
	avatar_url text NOT NULL DEFAULT '',
	role varchar(32) NOT NULL DEFAULT 'member',
	status varchar(32) NOT NULL DEFAULT 'active',
	password_hash text NOT NULL DEFAULT '',
	last_login_at timestamptz,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_users_email ON users(email);
CREATE INDEX idx_users_enterprise ON users(enterprise_id);

CREATE TABLE user_auth_providers (
	id varchar(64) NOT NULL PRIMARY KEY,
	user_id varchar(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	provider varchar(64) NOT NULL,
	subject_id varchar(255) NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_user_auth_providers_user ON user_auth_providers(user_id);
CREATE UNIQUE INDEX idx_user_auth_providers_provider_subject ON user_auth_providers(provider, subject_id);

CREATE TABLE sessions (
	id varchar(64) NOT NULL PRIMARY KEY,
	user_id varchar(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash varchar(128) NOT NULL,
	refresh_token_hash varchar(128) NOT NULL,
	expires_at timestamptz NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_sessions_token_hash ON sessions(token_hash);
CREATE UNIQUE INDEX idx_sessions_refresh_token_hash ON sessions(refresh_token_hash);
CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE TABLE verification_codes (
	id varchar(64) NOT NULL PRIMARY KEY,
	user_id varchar(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	code_hash varchar(128) NOT NULL,
	expires_at timestamptz NOT NULL,
	used boolean NOT NULL DEFAULT false,
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_verification_codes_user ON verification_codes(user_id);

CREATE TABLE user_key_materials (
	user_id varchar(64) NOT NULL PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	salt bytea NOT NULL,
	kek_verification bytea NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sso_configs (
	id varchar(64) NOT NULL PRIMARY KEY,
	enterprise_id varchar(64) NOT NULL REFERENCES enterprises(id) ON DELETE CASCADE,
	provider varchar(64) NOT NULL,
	client_id varchar(512) NOT NULL,
	issuer_url text NOT NULL DEFAULT '',
	enabled boolean NOT NULL DEFAULT true,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_sso_configs_enterprise ON sso_configs(enterprise_id);
CREATE UNIQUE INDEX idx_sso_configs_enterprise_provider ON sso_configs(enterprise_id, provider);

CREATE TABLE connected_accounts (
	id varchar(64) NOT NULL PRIMARY KEY,
	user_id varchar(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	provider varchar(64) NOT NULL,
	scopes text NOT NULL DEFAULT '[]',
	status varchar(32) NOT NULL DEFAULT 'active',
	external_user_id varchar(255) NOT NULL DEFAULT '',
	external_team_id varchar(255) NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_connected_accounts_user ON connected_accounts(user_id);
CREATE UNIQUE INDEX idx_connected_accounts_user_provider ON connected_accounts(user_id, provider);

CREATE TABLE drafts (
	id varchar(64) NOT NULL PRIMARY KEY,
	user_id varchar(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	status varchar(32) NOT NULL DEFAULT 'pending',
	service varchar(64) NOT NULL,
	channel varchar(255) NOT NULL,
	author varchar(255) NOT NULL,
	message_body text NOT NULL,
	message_ts varchar(64) NOT NULL,
	draft_body text NOT NULL,
	sent_body text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_drafts_user ON drafts(user_id);

CREATE TABLE user_instructions (
	id varchar(64) NOT NULL PRIMARY KEY,
	user_id varchar(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	body text NOT NULL,
	scope text NOT NULL DEFAULT '',
	active boolean NOT NULL DEFAULT true,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_user_instructions_user ON user_instructions(user_id);

CREATE TABLE draft_feedback (
	id varchar(64) NOT NULL PRIMARY KEY,
	user_id varchar(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	draft_id varchar(64) NOT NULL,
	signal varchar(32) NOT NULL,
	service varchar(64) NOT NULL,
	channel varchar(255) NOT NULL,
	draft_body text NOT NULL,
	sent_body text NOT NULL DEFAULT '',
	tools_used text NOT NULL DEFAULT '[]',
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_draft_feedback_user ON draft_feedback(user_id);

CREATE TABLE escrow_index (
	vault_path varchar(512) NOT NULL PRIMARY KEY,
	escrow_id varchar(128) NOT NULL,
	user_id varchar(64) NOT NULL,
	expires_at timestamptz NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE llm_configs (
	id varchar(64) NOT NULL PRIMARY KEY,
	owner_type varchar(32) NOT NULL,
	owner_id varchar(64) NOT NULL,
	provider varchar(32) NOT NULL,
	model_research varchar(128) NOT NULL DEFAULT '',
	model_synthesis varchar(128) NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_llm_configs_owner ON llm_configs(owner_type, owner_id);
`
