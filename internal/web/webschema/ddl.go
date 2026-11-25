// Package webschema owns the magnetico_web PostgreSQL schema (docs/specs/
// web-v1.md §5): its DDL, the explicit non-destructive and idempotent
// initialization, the approved historical tracker seed, and the structural and
// privilege verification shared by the web service and the operator CLI.
//
// Nothing here is run implicitly: the schema is created only by
// `magnetico migrate web-schema-init`, and the web service only verifies it.
//
// Table names deliberately avoid torrents/files/torrent_file_blobs/
// schema_version: persistence treats such tables in ANY schema as markers of
// the torrent schema family, which is why the version table is web_version.
package webschema

import "github.com/skobkin/magnetico-web/internal/advisory"

// Version is the only supported magnetico_web.web_version row.
const Version = 1

// Advisory-lock keys (transaction-scoped) come from internal/advisory, the one
// registry of every key in the project.
const (
	initAdvisoryKey     = advisory.WebSchemaInit
	trackersAdvisoryKey = advisory.TrackersWrite
)

// ddl is the version-1 schema. It runs in one transaction. There are no
// triggers and no PL/pgSQL: lifecycle rules (permanent revocation, set-once
// invitation consumption, one-time link consumption) are enforced by
// conditional SQL under row locks in the stores.
var ddl = []string{
	`CREATE SCHEMA magnetico_web`,

	`CREATE TABLE magnetico_web.web_version (
    version    INTEGER PRIMARY KEY,
    applied_on TIMESTAMPTZ NOT NULL DEFAULT now()
)`,

	// Accounts: three independent attributes (active, nullable password_hash,
	// email with verification). No pending_setup state, no credential epoch.
	`CREATE TABLE magnetico_web.users (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY,
    username            TEXT NOT NULL,
    email               TEXT,
    email_verified_at   TIMESTAMPTZ,
    password_hash       TEXT,
    active              BOOLEAN NOT NULL DEFAULT true,
    role                TEXT NOT NULL DEFAULT 'user',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    password_changed_at TIMESTAMPTZ,
    last_login_at       TIMESTAMPTZ,
    CONSTRAINT users_pkey PRIMARY KEY (id),
    CONSTRAINT users_username_key UNIQUE (username),
    CONSTRAINT users_username_length CHECK (char_length(username) BETWEEN 1 AND 25),
    CONSTRAINT users_role_check CHECK (role IN ('user', 'admin')),
    CONSTRAINT users_email_length CHECK (email IS NULL OR char_length(email) BETWEEN 3 AND 254),
    CONSTRAINT users_verified_needs_email CHECK (email_verified_at IS NULL OR email IS NOT NULL),
    CONSTRAINT users_password_hash_nonempty CHECK (password_hash IS NULL OR password_hash <> '')
)`,
	`CREATE UNIQUE INDEX users_verified_email_key
    ON magnetico_web.users (lower(email)) WHERE email_verified_at IS NOT NULL`,

	// Server-side sessions keyed by the SHA-256 of the cookie token.
	`CREATE TABLE magnetico_web.sessions (
    token_hash   BYTEA NOT NULL,
    user_id      BIGINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    remember     BOOLEAN NOT NULL,
    CONSTRAINT sessions_pkey PRIMARY KEY (token_hash),
    CONSTRAINT sessions_token_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT sessions_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT sessions_user_fkey FOREIGN KEY (user_id) REFERENCES magnetico_web.users (id)
)`,
	// used by: disable / password reset / set-password (DELETE ... WHERE user_id)
	`CREATE INDEX sessions_user_id_idx ON magnetico_web.sessions (user_id)`,

	// JSON API credentials only; physically separate from rss_tokens.
	`CREATE TABLE magnetico_web.api_tokens (
    id           BIGINT GENERATED ALWAYS AS IDENTITY,
    token_hash   BYTEA NOT NULL,
    user_id      BIGINT NOT NULL,
    label        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    CONSTRAINT api_tokens_pkey PRIMARY KEY (id),
    CONSTRAINT api_tokens_token_hash_key UNIQUE (token_hash),
    CONSTRAINT api_tokens_token_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT api_tokens_label_length CHECK (label IS NULL OR char_length(label) BETWEEN 1 AND 100),
    CONSTRAINT api_tokens_user_fkey FOREIGN KEY (user_id) REFERENCES magnetico_web.users (id)
)`,
	// used by: revoke-tokens, password-reset revocation, account listing
	`CREATE INDEX api_tokens_user_id_idx ON magnetico_web.api_tokens (user_id)`,

	// RSS feed credentials only. Legacy PHP tokens are migrated here (W8) with
	// legacy = true.
	`CREATE TABLE magnetico_web.rss_tokens (
    id           BIGINT GENERATED ALWAYS AS IDENTITY,
    token_hash   BYTEA NOT NULL,
    user_id      BIGINT NOT NULL,
    label        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    legacy       BOOLEAN NOT NULL DEFAULT false,
    CONSTRAINT rss_tokens_pkey PRIMARY KEY (id),
    CONSTRAINT rss_tokens_token_hash_key UNIQUE (token_hash),
    CONSTRAINT rss_tokens_token_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT rss_tokens_label_length CHECK (label IS NULL OR char_length(label) BETWEEN 1 AND 100),
    CONSTRAINT rss_tokens_user_fkey FOREIGN KEY (user_id) REFERENCES magnetico_web.users (id)
)`,
	`CREATE INDEX rss_tokens_user_id_idx ON magnetico_web.rss_tokens (user_id)`,

	// Invitations; plaintext codes by owner decision. "Used" = used_by IS NOT
	// NULL. W8 sets a migrated invitation's created_at from its owner's
	// users.created_at (an intentional approximation: the legacy table has no
	// creation time).
	`CREATE TABLE magnetico_web.invites (
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    owner_id   BIGINT NOT NULL,
    code       TEXT NOT NULL,
    used_by    BIGINT,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    CONSTRAINT invites_pkey PRIMARY KEY (id),
    CONSTRAINT invites_code_key UNIQUE (code),
    CONSTRAINT invites_code_format CHECK (code ~ '^[A-Za-z0-9_-]{22,64}$'),
    CONSTRAINT invites_used_at_needs_used_by CHECK (used_at IS NULL OR used_by IS NOT NULL),
    CONSTRAINT invites_owner_fkey FOREIGN KEY (owner_id) REFERENCES magnetico_web.users (id),
    CONSTRAINT invites_used_by_fkey FOREIGN KEY (used_by) REFERENCES magnetico_web.users (id)
)`,
	// used by: invitation listing (CLI, account page)
	`CREATE INDEX invites_owner_id_idx ON magnetico_web.invites (owner_id)`,

	// Setup / reset / email-verification links. A row exists only while the
	// link is outstanding: consumption, supersession and revocation DELETE it.
	`CREATE TABLE magnetico_web.one_time_tokens (
    token_hash BYTEA NOT NULL,
    user_id    BIGINT NOT NULL,
    purpose    TEXT NOT NULL,
    email      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT one_time_tokens_pkey PRIMARY KEY (token_hash),
    CONSTRAINT one_time_tokens_token_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT one_time_tokens_purpose_check CHECK (purpose IN ('setup', 'reset', 'email_verify')),
    CONSTRAINT one_time_tokens_email_iff_verify CHECK ((purpose = 'email_verify') = (email IS NOT NULL)),
    CONSTRAINT one_time_tokens_email_length CHECK (email IS NULL OR char_length(email) BETWEEN 3 AND 254),
    CONSTRAINT one_time_tokens_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT one_time_tokens_user_fkey FOREIGN KEY (user_id) REFERENCES magnetico_web.users (id)
)`,
	// At most one outstanding password link (setup|reset) and one outstanding
	// email-verification link per user. Plain UNIQUE, no predicate: an expired
	// row still occupies the slot until a writer deletes it, so issuance
	// deletes the user's older link of the same kind first. It also serves the
	// per-user lookup.
	`CREATE UNIQUE INDEX one_time_tokens_one_per_kind
    ON magnetico_web.one_time_tokens (user_id, (purpose = 'email_verify'))`,

	// Public tracker list; order is position, id.
	`CREATE TABLE magnetico_web.trackers (
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    url        TEXT NOT NULL,
    position   INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT trackers_pkey PRIMARY KEY (id),
    CONSTRAINT trackers_url_key UNIQUE (url),
    CONSTRAINT trackers_url_format CHECK (char_length(url) <= 2048 AND url ~* '^(udp|https?|wss?)://[^[:space:]]+$'),
    CONSTRAINT trackers_position_positive CHECK (position > 0)
)`,

	// Web settings; holds the trackers.seed marker.
	`CREATE TABLE magnetico_web.settings (
    key        TEXT NOT NULL,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT settings_pkey PRIMARY KEY (key),
    CONSTRAINT settings_key_format CHECK (key ~ '^[a-z][a-z0-9_.-]{0,99}$')
)`,
}
