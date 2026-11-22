package webschema

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/persistence"
)

var specTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://tracker.open-internet.nl:6969/announce",
	"udp://tracker.piratepublic.com:1337/announce",
	"udp://9.rarbg.to:2710/announce",
	"udp://tracker.zer0day.to:1337/announce",
	"http://nyaa.tracker.wf:7777/announce",
	"http://t.nyaatracker.com:80/announce",
	"udp://open.stealth.si:80/announce",
	"udp://bt.xxx-tracker.com:2710/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://open.demonii.si:1337/announce",
	"udp://tracker.torrent.eu.org:451/announce",
}

func trackerURLs(t *testing.T, dsn string) []string {
	t.Helper()
	conn, err := pgx.Connect(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(tctx(t))
	rows, err := conn.Query(tctx(t), `SELECT url FROM magnetico_web.trackers ORDER BY position, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		_ = rows.Scan(&u)
		out = append(out, u)
	}
	return out
}

func TestInitFreshCreatesVerifiedSchemaAndSeed(t *testing.T) {
	dsn, _ := torrentDB(t)
	rep, err := Init(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Created || rep.Seed != SeedApplied || rep.SeededCount != 12 {
		t.Fatalf("report = %+v", rep)
	}
	if err := Verify(tctx(t), openPool(t, dsn), Operator); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got := trackerURLs(t, dsn)
	if strings.Join(got, "\n") != strings.Join(specTrackers, "\n") {
		t.Fatalf("seed = %v", got)
	}
	// The snapshot has no trigger and no function in the schema.
	snap := snapshot(t, dsn)
	if strings.Contains(snap, "trg|") || strings.Contains(snap, "fn|") {
		t.Fatalf("unexpected triggers or functions:\n%s", snap)
	}
}

func TestEmbeddedSeedMatchesApprovedList(t *testing.T) {
	urls, err := seedList()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(urls, "\n") != strings.Join(specTrackers, "\n") {
		t.Fatalf("embedded seed differs from spec §8.3.4: %v", urls)
	}
	var again []string
	if err := json.Unmarshal(seedJSON, &again); err != nil || len(again) != 12 {
		t.Fatalf("seed JSON: %v", err)
	}
}

func TestInitIsIdempotentAndChangesNothing(t *testing.T) {
	dsn := webDB(t)
	before := snapshot(t, dsn)
	rep, err := Init(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created || rep.Seed != SeedAlreadyDone {
		t.Fatalf("report = %+v", rep)
	}
	if after := snapshot(t, dsn); after != before {
		t.Fatalf("a repeated init changed the database:\n--- before\n%s--- after\n%s", before, after)
	}
}

func TestConcurrentInitsCreateOnce(t *testing.T) {
	dsn, _ := torrentDB(t)
	const n = 8
	var wg sync.WaitGroup
	reports := make([]Report, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reports[i], errs[i] = Init(tctx(t), dsn)
		}()
	}
	wg.Wait()
	created, seeded := 0, 0
	for i := range reports {
		if errs[i] != nil {
			t.Fatalf("init %d: %v", i, errs[i])
		}
		if reports[i].Created {
			created++
		}
		if reports[i].Seed == SeedApplied {
			seeded++
		}
	}
	if created != 1 || seeded != 1 {
		t.Fatalf("created %d, seeded %d; want exactly one each", created, seeded)
	}
	if got := trackerURLs(t, dsn); len(got) != 12 {
		t.Fatalf("trackers = %d", len(got))
	}
}

func TestInitRequiresVerifiedTorrentSchema(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		dsn := mustFreshDB(t)
		if _, err := Init(tctx(t), dsn); err == nil {
			t.Fatal("init must refuse a database without the torrent schema")
		}
		if snap := snapshot(t, dsn); strings.Contains(snap, "rel|") {
			t.Fatalf("magnetico_web was created:\n%s", snap)
		}
	})
	t.Run("wrong version", func(t *testing.T) {
		dsn, _ := torrentDB(t)
		execSQL(t, dsn, `UPDATE magneticod.schema_version SET version = version + 100`)
		_, err := Init(tctx(t), dsn)
		if !errors.Is(err, persistence.ErrUnsupportedSchema) {
			t.Fatalf("err = %v", err)
		}
		if snapshot(t, dsn) != "" {
			t.Fatal("magnetico_web was created")
		}
	})
}

func TestCoexistsWithTorrentAndIngestSchemas(t *testing.T) {
	dsn := webDB(t)
	// The web version table is named web_version precisely so that the
	// torrent-schema inventory is not disturbed.
	if err := persistence.InitSchema(tctx(t), dsn); err != nil {
		t.Fatalf("persistence.InitSchema after web init: %v", err)
	}
	if err := persistence.VerifySchema(tctx(t), openPool(t, dsn)); err != nil {
		t.Fatalf("VerifySchema: %v", err)
	}
	if err := pgqueue.InitSchema(tctx(t), dsn); err != nil {
		t.Fatalf("ingest schema init after web init: %v", err)
	}
	if _, err := Init(tctx(t), dsn); err != nil {
		t.Fatalf("web init after ingest init: %v", err)
	}
}

func TestSeedNeverResurrectsOrOverrides(t *testing.T) {
	t.Run("removed and reordered stay", func(t *testing.T) {
		dsn := webDB(t)
		execSQL(t, dsn,
			`DELETE FROM magnetico_web.trackers WHERE position = 4`,
			`UPDATE magnetico_web.trackers SET position = position + 100 WHERE position = 1`)
		before := snapshot(t, dsn)
		rep, err := Init(tctx(t), dsn)
		if err != nil || rep.Seed != SeedAlreadyDone {
			t.Fatalf("rep=%+v err=%v", rep, err)
		}
		if snapshot(t, dsn) != before {
			t.Fatal("init modified the operator's tracker edits")
		}
		if got := trackerURLs(t, dsn); len(got) != 11 || got[len(got)-1] != specTrackers[0] {
			t.Fatalf("trackers = %v", got)
		}
	})
	t.Run("even with every tracker removed", func(t *testing.T) {
		dsn := webDB(t)
		execSQL(t, dsn, `DELETE FROM magnetico_web.trackers`)
		if rep, err := Init(tctx(t), dsn); err != nil || rep.Seed != SeedAlreadyDone {
			t.Fatalf("rep=%+v err=%v", rep, err)
		}
		if got := trackerURLs(t, dsn); len(got) != 0 {
			t.Fatalf("trackers resurrected: %v", got)
		}
	})
	t.Run("non-empty table without marker is left alone", func(t *testing.T) {
		dsn := webDB(t)
		execSQL(t, dsn, `DELETE FROM magnetico_web.settings WHERE key = 'trackers.seed'`)
		before := snapshot(t, dsn)
		rep, err := Init(tctx(t), dsn)
		if err != nil || rep.Seed != SeedSkippedNonEmpty {
			t.Fatalf("rep=%+v err=%v", rep, err)
		}
		if snapshot(t, dsn) != before {
			t.Fatal("init modified a non-empty unmarked tracker table")
		}
	})
	t.Run("empty table without marker is seeded", func(t *testing.T) {
		dsn := webDB(t)
		execSQL(t, dsn, `DELETE FROM magnetico_web.trackers; DELETE FROM magnetico_web.settings`)
		if rep, err := Init(tctx(t), dsn); err != nil || rep.Seed != SeedApplied {
			t.Fatalf("rep=%+v err=%v", rep, err)
		}
		if got := trackerURLs(t, dsn); len(got) != 12 {
			t.Fatalf("trackers = %v", got)
		}
	})
}

// A damaged or foreign magnetico_web schema must be refused by BOTH the
// initialization and the startup verification, with nothing changed.
func TestIncompatibleSchemaIsRefusedWithoutChanges(t *testing.T) {
	cases := []struct {
		name string
		sql  []string
		want string // fragment of the reported detail
	}{
		{"dropped role check", []string{`ALTER TABLE magnetico_web.users DROP CONSTRAINT users_role_check`}, "users_role_check"},
		{"dropped purpose check", []string{`ALTER TABLE magnetico_web.one_time_tokens DROP CONSTRAINT one_time_tokens_purpose_check`}, "one_time_tokens_purpose_check"},
		{"dropped email_iff_verify", []string{`ALTER TABLE magnetico_web.one_time_tokens DROP CONSTRAINT one_time_tokens_email_iff_verify`}, "email_iff_verify"},
		{"weakened check", []string{`ALTER TABLE magnetico_web.users DROP CONSTRAINT users_role_check`,
			`ALTER TABLE magnetico_web.users ADD CONSTRAINT users_role_check CHECK (role IN ('user','admin','root'))`}, "users_role_check"},
		{"dropped foreign key", []string{`ALTER TABLE magnetico_web.api_tokens DROP CONSTRAINT api_tokens_user_fkey`}, "api_tokens_user_fkey"},
		{"cascading foreign key", []string{`ALTER TABLE magnetico_web.rss_tokens DROP CONSTRAINT rss_tokens_user_fkey`,
			`ALTER TABLE magnetico_web.rss_tokens ADD CONSTRAINT rss_tokens_user_fkey FOREIGN KEY (user_id) REFERENCES magnetico_web.users(id) ON DELETE CASCADE`}, "rss_tokens_user_fkey"},
		{"deferrable foreign key", []string{`ALTER TABLE magnetico_web.sessions DROP CONSTRAINT sessions_user_fkey`,
			`ALTER TABLE magnetico_web.sessions ADD CONSTRAINT sessions_user_fkey FOREIGN KEY (user_id) REFERENCES magnetico_web.users(id) DEFERRABLE`}, "sessions_user_fkey"},
		{"not valid constraint", []string{`ALTER TABLE magnetico_web.users DROP CONSTRAINT users_role_check`,
			`ALTER TABLE magnetico_web.users ADD CONSTRAINT users_role_check CHECK (role IN ('user','admin')) NOT VALID`}, "NOT VALID"},
		{"dropped primary key", []string{`ALTER TABLE magnetico_web.invites DROP CONSTRAINT invites_pkey CASCADE`}, "invites_pkey"},
		{"dropped username unique", []string{`ALTER TABLE magnetico_web.users DROP CONSTRAINT users_username_key`}, "users_username_key"},
		{"dropped invite code unique", []string{`ALTER TABLE magnetico_web.invites DROP CONSTRAINT invites_code_key`}, "invites_code_key"},
		{"dropped api token unique", []string{`ALTER TABLE magnetico_web.api_tokens DROP CONSTRAINT api_tokens_token_hash_key`}, "api_tokens_token_hash_key"},
		{"dropped verified email index", []string{`DROP INDEX magnetico_web.users_verified_email_key`}, "users_verified_email_key"},
		{"dropped one-per-kind index", []string{`DROP INDEX magnetico_web.one_time_tokens_one_per_kind`}, "one_time_tokens_one_per_kind"},
		{"dropped secondary index", []string{`DROP INDEX magnetico_web.sessions_user_id_idx`}, "sessions_user_id_idx"},
		{"index with changed predicate", []string{`DROP INDEX magnetico_web.users_verified_email_key`,
			`CREATE UNIQUE INDEX users_verified_email_key ON magnetico_web.users (lower(email))`}, "users_verified_email_key"},
		{"index with changed expression", []string{`DROP INDEX magnetico_web.one_time_tokens_one_per_kind`,
			`CREATE UNIQUE INDEX one_time_tokens_one_per_kind ON magnetico_web.one_time_tokens (user_id, purpose)`}, "one_time_tokens_one_per_kind"},
		{"extra index", []string{`CREATE INDEX extra_idx ON magnetico_web.users (created_at)`}, "extra_idx"},
		{"dropped column", []string{`ALTER TABLE magnetico_web.rss_tokens DROP COLUMN legacy`}, "rss_tokens.legacy"},
		{"extra column", []string{`ALTER TABLE magnetico_web.users ADD COLUMN note text`}, "users.note"},
		{"column type changed", []string{`ALTER TABLE magnetico_web.sessions DROP CONSTRAINT sessions_token_hash_len`, `ALTER TABLE magnetico_web.sessions DROP CONSTRAINT sessions_pkey`,
			`ALTER TABLE magnetico_web.sessions ALTER COLUMN token_hash TYPE text USING encode(token_hash,'hex')`}, "sessions.token_hash"},
		{"id narrowed to integer", []string{`ALTER TABLE magnetico_web.users DROP CONSTRAINT users_pkey CASCADE`,
			`ALTER TABLE magnetico_web.users ALTER COLUMN id TYPE integer`}, "users.id"},
		{"column made nullable", []string{`ALTER TABLE magnetico_web.sessions ALTER COLUMN expires_at DROP NOT NULL`}, "sessions.expires_at"},
		{"identity removed", []string{`ALTER TABLE magnetico_web.users ALTER COLUMN id DROP IDENTITY`}, "users.id"},
		{"default changed", []string{`ALTER TABLE magnetico_web.users ALTER COLUMN active SET DEFAULT false`}, "users.active"},
		{"default dropped", []string{`ALTER TABLE magnetico_web.users ALTER COLUMN role DROP DEFAULT`}, "users.role"},
		{"unexpected trigger", []string{
			`CREATE FUNCTION public.noop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`,
			`CREATE TRIGGER sneaky BEFORE UPDATE ON magnetico_web.users FOR EACH ROW EXECUTE FUNCTION public.noop()`}, "sneaky"},
		{"extra table", []string{`CREATE TABLE magnetico_web.extra (x int)`}, "extra"},
		{"view in schema", []string{`CREATE VIEW magnetico_web.v AS SELECT 1 AS x`}, "magnetico_web.v"},
		{"row level security", []string{`ALTER TABLE magnetico_web.users ENABLE ROW LEVEL SECURITY`}, "row-level security"},
		{"missing table", []string{`DROP TABLE magnetico_web.settings`}, "settings"},
		{"version 2", []string{`UPDATE magnetico_web.web_version SET version = 2`}, "web_version"},
		{"version row missing", []string{`DELETE FROM magnetico_web.web_version`}, "web_version"},
		{"two version rows", []string{`INSERT INTO magnetico_web.web_version (version) VALUES (2)`}, "web_version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dsn := webDB(t)
			execSQL(t, dsn, c.sql...)
			before := snapshot(t, dsn)

			_, err := Init(tctx(t), dsn)
			if !isUnsupported(err) {
				t.Fatalf("Init err = %v, want ErrWebSchemaUnsupported", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Init error does not name %q: %v", c.want, err)
			}
			if err := Verify(tctx(t), openPool(t, dsn), Runtime); !isUnsupported(err) {
				t.Fatalf("Verify err = %v, want ErrWebSchemaUnsupported", err)
			}
			if after := snapshot(t, dsn); after != before {
				t.Fatalf("a refused call changed the database:\n--- before\n%s--- after\n%s", before, after)
			}
		})
	}
}

func TestSchemaWithoutVersionTable(t *testing.T) {
	dsn, _ := torrentDB(t)
	execSQL(t, dsn, `CREATE SCHEMA magnetico_web`)
	before := snapshot(t, dsn)
	if _, err := Init(tctx(t), dsn); !isUnsupported(err) {
		t.Fatalf("Init: %v", err)
	}
	if err := Verify(tctx(t), openPool(t, dsn), Runtime); !errors.Is(err, ErrWebSchemaMissing) {
		t.Fatalf("Verify: %v", err)
	}
	if snapshot(t, dsn) != before {
		t.Fatal("changed")
	}
}

func TestVerifyReportsMissingSchema(t *testing.T) {
	dsn, _ := torrentDB(t)
	if err := Verify(tctx(t), openPool(t, dsn), Runtime); !errors.Is(err, ErrWebSchemaMissing) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyIgnoresAdditionalGrantsAndAnOwnerChange(t *testing.T) {
	dsn := webDB(t)
	execSQL(t, dsn, `GRANT SELECT ON magnetico_web.users TO PUBLIC`)
	if err := Verify(tctx(t), openPool(t, dsn), Operator); err != nil {
		t.Fatalf("extra grants must not matter: %v", err)
	}
}

func TestSchemaSemantics(t *testing.T) {
	dsn := webDB(t)
	conn, err := pgx.Connect(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(tctx(t))
	exec := func(sql string, args ...any) error {
		_, err := conn.Exec(tctx(t), sql, args...)
		return err
	}
	mustFail := func(what, sql string, args ...any) {
		t.Helper()
		if err := exec(sql, args...); err == nil {
			t.Errorf("%s: expected a constraint violation", what)
		}
	}
	mustOK := func(what, sql string, args ...any) {
		t.Helper()
		if err := exec(sql, args...); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}

	// Case-sensitive usernames.
	mustOK("Alice", `INSERT INTO magnetico_web.users (username) VALUES ('Alice')`)
	mustOK("alice distinct from Alice", `INSERT INTO magnetico_web.users (username) VALUES ('alice')`)
	mustFail("exact duplicate", `INSERT INTO magnetico_web.users (username) VALUES ('Alice')`)
	mustFail("empty username", `INSERT INTO magnetico_web.users (username) VALUES ('')`)
	mustFail("26-char username", `INSERT INTO magnetico_web.users (username) VALUES (repeat('x', 26))`)
	mustOK("25-char username", `INSERT INTO magnetico_web.users (username) VALUES (repeat('x', 25))`)
	mustOK("25 multibyte code points", `INSERT INTO magnetico_web.users (username) VALUES (repeat('я', 25))`)

	mustFail("bad role", `INSERT INTO magnetico_web.users (username, role) VALUES ('r1', 'root')`)
	mustOK("admin role", `INSERT INTO magnetico_web.users (username, role) VALUES ('r2', 'admin')`)
	mustFail("empty password hash", `INSERT INTO magnetico_web.users (username, password_hash) VALUES ('p1', '')`)
	mustOK("NULL password hash", `INSERT INTO magnetico_web.users (username, password_hash) VALUES ('p2', NULL)`)
	mustFail("verified without email", `INSERT INTO magnetico_web.users (username, email_verified_at) VALUES ('v1', now())`)

	// Verified-address uniqueness is by lower(email) and only for verified rows.
	mustOK("unverified e1", `INSERT INTO magnetico_web.users (username, email) VALUES ('e1', 'A@Example.org')`)
	mustOK("unverified duplicate allowed", `INSERT INTO magnetico_web.users (username, email) VALUES ('e2', 'a@example.org')`)
	mustOK("verified e3", `INSERT INTO magnetico_web.users (username, email, email_verified_at) VALUES ('e3', 'A@Example.org', now())`)
	mustFail("verified duplicate differing by case", `INSERT INTO magnetico_web.users (username, email, email_verified_at) VALUES ('e4', 'a@EXAMPLE.org', now())`)

	var uid int64
	if err := conn.QueryRow(tctx(t), `SELECT id FROM magnetico_web.users WHERE username = 'Alice'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	hash := func(b byte, n int) []byte { return []byte(strings.Repeat(string([]byte{b}), n)) }

	// Sessions: absolute expiry is mandatory.
	mustFail("session without expiry", `INSERT INTO magnetico_web.sessions (token_hash, user_id, remember) VALUES ($1, $2, false)`, hash('a', 32), uid)
	mustFail("session short hash", `INSERT INTO magnetico_web.sessions (token_hash, user_id, expires_at, remember) VALUES ($1, $2, now() + interval '1 hour', false)`, hash('a', 31), uid)
	mustFail("session expiring in the past", `INSERT INTO magnetico_web.sessions (token_hash, user_id, expires_at, remember) VALUES ($1, $2, now() - interval '1 hour', false)`, hash('a', 32), uid)
	mustOK("session", `INSERT INTO magnetico_web.sessions (token_hash, user_id, expires_at, remember) VALUES ($1, $2, now() + interval '1 hour', false)`, hash('a', 32), uid)
	mustFail("session unknown user", `INSERT INTO magnetico_web.sessions (token_hash, user_id, expires_at, remember) VALUES ($1, 999999, now() + interval '1 hour', false)`, hash('b', 32))

	// The two credential stores are independent: the same digest may exist in
	// each (separation is by table), but not twice in one.
	mustOK("api token", `INSERT INTO magnetico_web.api_tokens (token_hash, user_id) VALUES ($1, $2)`, hash('c', 32), uid)
	mustFail("api token duplicate", `INSERT INTO magnetico_web.api_tokens (token_hash, user_id) VALUES ($1, $2)`, hash('c', 32), uid)
	mustOK("rss token", `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy) VALUES ($1, $2, true)`, hash('c', 32), uid)
	mustFail("rss token duplicate", `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id) VALUES ($1, $2)`, hash('c', 32), uid)
	mustFail("token label empty", `INSERT INTO magnetico_web.api_tokens (token_hash, user_id, label) VALUES ($1, $2, '')`, hash('d', 32), uid)
	mustFail("token short hash", `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id) VALUES ($1, $2)`, hash('e', 16), uid)

	// Invitations: legacy 32-hex and new 22-char codes are accepted; others not.
	mustOK("legacy code", `INSERT INTO magnetico_web.invites (owner_id, code) VALUES ($1, 'd41d8cd98f00b204e9800998ecf8427e')`, uid)
	mustOK("new code", `INSERT INTO magnetico_web.invites (owner_id, code) VALUES ($1, 'AAAAAAAAAAAAAAAAAAAAAA')`, uid)
	mustFail("duplicate code", `INSERT INTO magnetico_web.invites (owner_id, code) VALUES ($1, 'AAAAAAAAAAAAAAAAAAAAAA')`, uid)
	mustFail("short code", `INSERT INTO magnetico_web.invites (owner_id, code) VALUES ($1, 'short')`, uid)
	mustFail("code with a slash", `INSERT INTO magnetico_web.invites (owner_id, code) VALUES ($1, 'AAAAAAAAAAAAAAAAAAAAA/')`, uid)
	mustFail("used_at without used_by", `INSERT INTO magnetico_web.invites (owner_id, code, used_at) VALUES ($1, 'BBBBBBBBBBBBBBBBBBBBBB', now())`, uid)

	// One-time tokens: at most one outstanding password link and one
	// email-verification link per user (no used_at, deletion is consumption).
	mustOK("setup link", `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1, $2, 'setup', now() + interval '72 hours')`, hash('1', 32), uid)
	mustFail("second password link (reset)", `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1, $2, 'reset', now() + interval '1 hour')`, hash('2', 32), uid)
	mustOK("email link alongside", `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, email, expires_at) VALUES ($1, $2, 'email_verify', 'n@example.org', now() + interval '24 hours')`, hash('3', 32), uid)
	mustFail("second email link", `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, email, expires_at) VALUES ($1, $2, 'email_verify', 'm@example.org', now() + interval '24 hours')`, hash('4', 32), uid)
	mustFail("email on a password link", `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, email, expires_at) VALUES ($1, 2, 'setup', 'x@example.org', now() + interval '1 hour')`, hash('5', 32))
	mustFail("email link without address", `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1, 2, 'email_verify', now() + interval '1 hour')`, hash('6', 32))
	mustFail("unknown purpose", `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1, 2, 'login', now() + interval '1 hour')`, hash('7', 32))
	mustFail("expiry before creation", `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1, 2, 'setup', now() - interval '1 hour')`, hash('8', 32))
	// An EXPIRED outstanding row still blocks (writers must delete it first) …
	mustOK("expire the setup link", `UPDATE magnetico_web.one_time_tokens SET created_at = now() - interval '2 days', expires_at = now() - interval '1 day' WHERE token_hash = $1`, hash('1', 32))
	mustFail("replacement before deleting the expired link", `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1, $2, 'reset', now() + interval '1 hour')`, hash('9', 32), uid)
	// … and deleting it (consumption/supersession) frees the slot.
	mustOK("delete expired link", `DELETE FROM magnetico_web.one_time_tokens WHERE token_hash = $1`, hash('1', 32))
	mustOK("replacement after delete", `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1, $2, 'reset', now() + interval '1 hour')`, hash('9', 32), uid)

	// Trackers and settings.
	mustFail("tracker bad scheme", `INSERT INTO magnetico_web.trackers (url, position) VALUES ('ftp://x/announce', 99)`)
	mustFail("tracker whitespace", `INSERT INTO magnetico_web.trackers (url, position) VALUES ('udp://x y/announce', 99)`)
	mustFail("tracker duplicate", `INSERT INTO magnetico_web.trackers (url, position) VALUES ($1, 99)`, specTrackers[0])
	mustFail("tracker position 0", `INSERT INTO magnetico_web.trackers (url, position) VALUES ('udp://new.example:1/announce', 0)`)
	mustOK("tracker wss", `INSERT INTO magnetico_web.trackers (url, position) VALUES ('WSS://new.example/announce', 99)`)
	mustFail("setting key format", `INSERT INTO magnetico_web.settings (key, value) VALUES ('Bad Key', '{}')`)

	// Foreign keys do not cascade: a user with dependents cannot be deleted.
	mustFail("delete user with dependents", `DELETE FROM magnetico_web.users WHERE id = $1`, uid)
}

// Migration safety of preserved identifiers (spec §5.2): legacy IDs are
// inserted with OVERRIDING SYSTEM VALUE and the sequences advanced.
func TestIdentityOverrideAndSequenceAdvance(t *testing.T) {
	dsn := webDB(t)
	conn, err := pgx.Connect(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(tctx(t))
	if _, err := conn.Exec(tctx(t), `INSERT INTO magnetico_web.users (username) VALUES ('plain')`); err != nil {
		t.Fatal(err)
	}
	// GENERATED ALWAYS refuses an explicit ID without the override.
	if _, err := conn.Exec(tctx(t), `INSERT INTO magnetico_web.users (id, username) VALUES (500, 'x')`); err == nil {
		t.Fatal("an explicit id must be refused without OVERRIDING SYSTEM VALUE")
	}
	for _, q := range []string{
		`INSERT INTO magnetico_web.users (id, username) OVERRIDING SYSTEM VALUE VALUES (500, 'legacy1'), (42, 'legacy2')`,
		`INSERT INTO magnetico_web.invites (id, owner_id, code) OVERRIDING SYSTEM VALUE VALUES (77, 500, 'd41d8cd98f00b204e9800998ecf8427e')`,
		`SELECT setval(pg_get_serial_sequence('magnetico_web.users', 'id'), (SELECT max(id) FROM magnetico_web.users))`,
		`SELECT setval(pg_get_serial_sequence('magnetico_web.invites', 'id'), (SELECT max(id) FROM magnetico_web.invites))`,
	} {
		if _, err := conn.Exec(tctx(t), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var next int64
	if err := conn.QueryRow(tctx(t), `INSERT INTO magnetico_web.users (username) VALUES ('after') RETURNING id`).Scan(&next); err != nil || next != 501 {
		t.Fatalf("next user id = %d, %v", next, err)
	}
	if err := conn.QueryRow(tctx(t), `INSERT INTO magnetico_web.invites (owner_id, code) VALUES (500, 'AAAAAAAAAAAAAAAAAAAAAA') RETURNING id`).Scan(&next); err != nil || next != 78 {
		t.Fatalf("next invite id = %d, %v", next, err)
	}
}

func mustFreshDB(t *testing.T) string {
	t.Helper()
	cl, _ := torrentDBCluster(t)
	return cl.TestDatabase(t)
}
