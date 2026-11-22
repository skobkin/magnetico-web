package webschema

// colSpec is the expected shape of one column.
type colSpec struct {
	name      string
	typ       string // format_type
	notNull   bool
	identity  string // attidentity: "a" (GENERATED ALWAYS) or ""
	generated string // attgenerated: always ""
	def       string // normalized pg_get_expr of the default, "" if none
}

type tableSpec struct {
	name string
	cols []colSpec
}

const (
	tBigint = "bigint"
	tInt    = "integer"
	tText   = "text"
	tBytea  = "bytea"
	tTstz   = "timestamp with time zone"
	tBool   = "boolean"
	tJSONB  = "jsonb"
)

func id() colSpec { return colSpec{name: "id", typ: tBigint, notNull: true, identity: "a"} }
func col(name, typ string) colSpec {
	return colSpec{name: name, typ: typ}
}
func nn(name, typ string) colSpec { return colSpec{name: name, typ: typ, notNull: true} }
func nnDef(name, typ, def string) colSpec {
	return colSpec{name: name, typ: typ, notNull: true, def: def}
}

var expectedTables = []tableSpec{
	{"web_version", []colSpec{nn("version", tInt), nnDef("applied_on", tTstz, "now()")}},
	{"users", []colSpec{
		id(), nn("username", tText), col("email", tText), col("email_verified_at", tTstz), col("password_hash", tText),
		nnDef("active", tBool, "true"), nnDef("role", tText, "'user'::text"), nnDef("created_at", tTstz, "now()"),
		col("password_changed_at", tTstz), col("last_login_at", tTstz),
	}},
	{"sessions", []colSpec{
		nn("token_hash", tBytea), nn("user_id", tBigint), nnDef("created_at", tTstz, "now()"),
		nnDef("last_seen_at", tTstz, "now()"), nn("expires_at", tTstz), nn("remember", tBool),
	}},
	{"api_tokens", []colSpec{
		id(), nn("token_hash", tBytea), nn("user_id", tBigint), col("label", tText), nnDef("created_at", tTstz, "now()"),
		col("last_used_at", tTstz), col("expires_at", tTstz), col("revoked_at", tTstz),
	}},
	{"rss_tokens", []colSpec{
		id(), nn("token_hash", tBytea), nn("user_id", tBigint), col("label", tText), nnDef("created_at", tTstz, "now()"),
		col("last_used_at", tTstz), col("expires_at", tTstz), col("revoked_at", tTstz), nnDef("legacy", tBool, "false"),
	}},
	{"invites", []colSpec{
		id(), nn("owner_id", tBigint), nn("code", tText), col("used_by", tBigint), col("used_at", tTstz),
		nnDef("created_at", tTstz, "now()"), col("expires_at", tTstz), col("revoked_at", tTstz),
	}},
	{"one_time_tokens", []colSpec{
		nn("token_hash", tBytea), nn("user_id", tBigint), nn("purpose", tText), col("email", tText),
		nnDef("created_at", tTstz, "now()"), nn("expires_at", tTstz),
	}},
	{"trackers", []colSpec{
		id(), nn("url", tText), nn("position", tInt), nnDef("created_at", tTstz, "now()"), nnDef("updated_at", tTstz, "now()"),
	}},
	{"settings", []colSpec{nn("key", tText), nn("value", tJSONB), nnDef("updated_at", tTstz, "now()")}},
}

func tableNames() []string {
	out := make([]string, len(expectedTables))
	for i, t := range expectedTables {
		out[i] = t.name
	}
	return out
}

func isExpectedTable(name string) bool {
	for _, t := range expectedTables {
		if t.name == name {
			return true
		}
	}
	return false
}

// expectedConstraints and expectedIndexes are keyed "table.name" and hold the
// whitespace-normalized PostgreSQL deparse of each definition, baselined on
// PostgreSQL 17 (the supported minimum) with search_path = pg_catalog.
var expectedConstraints = map[string]string{
	"api_tokens.api_tokens_label_length":                    "CHECK (((label IS NULL) OR ((char_length(label) >= 1) AND (char_length(label) <= 100))))",
	"api_tokens.api_tokens_pkey":                            "PRIMARY KEY (id)",
	"api_tokens.api_tokens_token_hash_key":                  "UNIQUE (token_hash)",
	"api_tokens.api_tokens_token_hash_len":                  "CHECK ((octet_length(token_hash) = 32))",
	"api_tokens.api_tokens_user_fkey":                       "FOREIGN KEY (user_id) REFERENCES magnetico_web.users(id)",
	"invites.invites_code_format":                           "CHECK ((code ~ '^[A-Za-z0-9_-]{22,64}$'::text))",
	"invites.invites_code_key":                              "UNIQUE (code)",
	"invites.invites_owner_fkey":                            "FOREIGN KEY (owner_id) REFERENCES magnetico_web.users(id)",
	"invites.invites_pkey":                                  "PRIMARY KEY (id)",
	"invites.invites_used_at_needs_used_by":                 "CHECK (((used_at IS NULL) OR (used_by IS NOT NULL)))",
	"invites.invites_used_by_fkey":                          "FOREIGN KEY (used_by) REFERENCES magnetico_web.users(id)",
	"one_time_tokens.one_time_tokens_email_iff_verify":      "CHECK (((purpose = 'email_verify'::text) = (email IS NOT NULL)))",
	"one_time_tokens.one_time_tokens_email_length":          "CHECK (((email IS NULL) OR ((char_length(email) >= 3) AND (char_length(email) <= 254))))",
	"one_time_tokens.one_time_tokens_expiry_after_creation": "CHECK ((expires_at > created_at))",
	"one_time_tokens.one_time_tokens_pkey":                  "PRIMARY KEY (token_hash)",
	"one_time_tokens.one_time_tokens_purpose_check":         "CHECK ((purpose = ANY (ARRAY['setup'::text, 'reset'::text, 'email_verify'::text])))",
	"one_time_tokens.one_time_tokens_token_hash_len":        "CHECK ((octet_length(token_hash) = 32))",
	"one_time_tokens.one_time_tokens_user_fkey":             "FOREIGN KEY (user_id) REFERENCES magnetico_web.users(id)",
	"rss_tokens.rss_tokens_label_length":                    "CHECK (((label IS NULL) OR ((char_length(label) >= 1) AND (char_length(label) <= 100))))",
	"rss_tokens.rss_tokens_pkey":                            "PRIMARY KEY (id)",
	"rss_tokens.rss_tokens_token_hash_key":                  "UNIQUE (token_hash)",
	"rss_tokens.rss_tokens_token_hash_len":                  "CHECK ((octet_length(token_hash) = 32))",
	"rss_tokens.rss_tokens_user_fkey":                       "FOREIGN KEY (user_id) REFERENCES magnetico_web.users(id)",
	"sessions.sessions_expiry_after_creation":               "CHECK ((expires_at > created_at))",
	"sessions.sessions_pkey":                                "PRIMARY KEY (token_hash)",
	"sessions.sessions_token_hash_len":                      "CHECK ((octet_length(token_hash) = 32))",
	"sessions.sessions_user_fkey":                           "FOREIGN KEY (user_id) REFERENCES magnetico_web.users(id)",
	"settings.settings_key_format":                          "CHECK ((key ~ '^[a-z][a-z0-9_.-]{0,99}$'::text))",
	"settings.settings_pkey":                                "PRIMARY KEY (key)",
	"trackers.trackers_pkey":                                "PRIMARY KEY (id)",
	"trackers.trackers_position_positive":                   "CHECK ((\"position\" > 0))",
	"trackers.trackers_url_format":                          "CHECK (((char_length(url) <= 2048) AND (url ~* '^(udp|https?|wss?)://[^[:space:]]+$'::text)))",
	"trackers.trackers_url_key":                             "UNIQUE (url)",
	"users.users_email_length":                              "CHECK (((email IS NULL) OR ((char_length(email) >= 3) AND (char_length(email) <= 254))))",
	"users.users_password_hash_nonempty":                    "CHECK (((password_hash IS NULL) OR (password_hash <> ''::text)))",
	"users.users_pkey":                                      "PRIMARY KEY (id)",
	"users.users_role_check":                                "CHECK ((role = ANY (ARRAY['user'::text, 'admin'::text])))",
	"users.users_username_key":                              "UNIQUE (username)",
	"users.users_username_length":                           "CHECK (((char_length(username) >= 1) AND (char_length(username) <= 25)))",
	"users.users_verified_needs_email":                      "CHECK (((email_verified_at IS NULL) OR (email IS NOT NULL)))",
	"web_version.web_version_pkey":                          "PRIMARY KEY (version)",
}

var expectedIndexes = map[string]string{
	"api_tokens.api_tokens_pkey":                   "CREATE UNIQUE INDEX api_tokens_pkey ON magnetico_web.api_tokens USING btree (id)",
	"api_tokens.api_tokens_token_hash_key":         "CREATE UNIQUE INDEX api_tokens_token_hash_key ON magnetico_web.api_tokens USING btree (token_hash)",
	"api_tokens.api_tokens_user_id_idx":            "CREATE INDEX api_tokens_user_id_idx ON magnetico_web.api_tokens USING btree (user_id)",
	"invites.invites_code_key":                     "CREATE UNIQUE INDEX invites_code_key ON magnetico_web.invites USING btree (code)",
	"invites.invites_owner_id_idx":                 "CREATE INDEX invites_owner_id_idx ON magnetico_web.invites USING btree (owner_id)",
	"invites.invites_pkey":                         "CREATE UNIQUE INDEX invites_pkey ON magnetico_web.invites USING btree (id)",
	"one_time_tokens.one_time_tokens_one_per_kind": "CREATE UNIQUE INDEX one_time_tokens_one_per_kind ON magnetico_web.one_time_tokens USING btree (user_id, ((purpose = 'email_verify'::text)))",
	"one_time_tokens.one_time_tokens_pkey":         "CREATE UNIQUE INDEX one_time_tokens_pkey ON magnetico_web.one_time_tokens USING btree (token_hash)",
	"rss_tokens.rss_tokens_pkey":                   "CREATE UNIQUE INDEX rss_tokens_pkey ON magnetico_web.rss_tokens USING btree (id)",
	"rss_tokens.rss_tokens_token_hash_key":         "CREATE UNIQUE INDEX rss_tokens_token_hash_key ON magnetico_web.rss_tokens USING btree (token_hash)",
	"rss_tokens.rss_tokens_user_id_idx":            "CREATE INDEX rss_tokens_user_id_idx ON magnetico_web.rss_tokens USING btree (user_id)",
	"sessions.sessions_pkey":                       "CREATE UNIQUE INDEX sessions_pkey ON magnetico_web.sessions USING btree (token_hash)",
	"sessions.sessions_user_id_idx":                "CREATE INDEX sessions_user_id_idx ON magnetico_web.sessions USING btree (user_id)",
	"settings.settings_pkey":                       "CREATE UNIQUE INDEX settings_pkey ON magnetico_web.settings USING btree (key)",
	"trackers.trackers_pkey":                       "CREATE UNIQUE INDEX trackers_pkey ON magnetico_web.trackers USING btree (id)",
	"trackers.trackers_url_key":                    "CREATE UNIQUE INDEX trackers_url_key ON magnetico_web.trackers USING btree (url)",
	"users.users_pkey":                             "CREATE UNIQUE INDEX users_pkey ON magnetico_web.users USING btree (id)",
	"users.users_username_key":                     "CREATE UNIQUE INDEX users_username_key ON magnetico_web.users USING btree (username)",
	"users.users_verified_email_key":               "CREATE UNIQUE INDEX users_verified_email_key ON magnetico_web.users USING btree (lower(email)) WHERE (email_verified_at IS NOT NULL)",
	"web_version.web_version_pkey":                 "CREATE UNIQUE INDEX web_version_pkey ON magnetico_web.web_version USING btree (version)",
}
