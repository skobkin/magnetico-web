package users

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

func tctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return c
}

const testZone = "America/New_York"

func admin(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func execSQL(t *testing.T, dsn string, stmts ...string) {
	t.Helper()
	c := admin(t, dsn)
	for _, s := range stmts {
		if _, err := c.Exec(tctx(t), s); err != nil {
			t.Fatalf("%.80s: %v", s, err)
		}
	}
}

func scalar[T any](t *testing.T, dsn, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := admin(t, dsn).QueryRow(tctx(t), sql, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// legacyDB is a disposable database holding the legacy "users" schema.
func legacyDB(t *testing.T) string {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	sql, err := os.ReadFile("testdata/legacy_users.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin(t, dsn).Exec(tctx(t), string(sql)); err != nil {
		t.Fatal(err)
	}
	return dsn
}

// targetDB is a disposable database with the 0.8.0 torrent schema and an
// initialized magnetico_web schema.
func targetDB(t *testing.T) string {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	if _, err := webschema.Init(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	return dsn
}

// ---- synthetic legacy data ----

type sUser struct {
	ID                                int64
	Name, Hash, Email, Roles, Created string
}

type sInvite struct {
	ID, Owner int64
	Used      *int64
	Code      string
}

type sToken struct {
	Key     string
	User    int64
	Created string
}

type dataset struct {
	Users   []sUser
	Invites []sInvite
	Tokens  []sToken
}

func i64(v int64) *int64 { return &v }

type fixtureCase struct {
	ID, Family, Hash, PasswordB64 string
}

func fixtureCases(t *testing.T) []fixtureCase {
	t.Helper()
	raw, err := os.ReadFile("../../web/password/testdata/php/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			ID          string `json:"id"`
			Family      string `json:"family"`
			Hash        string `json:"hash"`
			PasswordB64 string `json:"password_b64"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var out []fixtureCase
	for _, c := range doc.Cases {
		out = append(out, fixtureCase{c.ID, c.Family, c.Hash, c.PasswordB64})
	}
	return out
}

// hashFor returns a PHP-generated fixture hash of the given family key
// (HashFamily form).
func hashFor(t *testing.T, family string) string {
	t.Helper()
	for _, c := range fixtureCases(t) {
		if f, ok := HashFamily(c.Hash); ok && f == family {
			return c.Hash
		}
	}
	t.Fatalf("no fixture of family %s", family)
	return ""
}

const (
	famSodium    = "argon2id/v=19/m=65536,t=4,p=1"
	famArgon2i   = "argon2i/v=19/m=16384,t=2,p=4"
	famBcrypt10  = "2y/cost=10"
	famBcrypt13  = "2y/cost=13"
	famArgonT2   = "argon2id/v=19/m=65536,t=2,p=1"
	famArgonT3P2 = "argon2id/v=19/m=65536,t=3,p=2"
)

// stdData is a six-user dataset covering every verified hash family, a
// case-distinct username pair, an email that differs only in case, gaps in
// the invitation ids, a used invitation and tokens for three users.
func stdData(t *testing.T) dataset {
	return dataset{
		Users: []sUser{
			{1, "alice", hashFor(t, famSodium), "alice@example.org", `["ROLE_USER"]`, "2021-01-15 10:00:00"},
			{2, "Bob", hashFor(t, famBcrypt10), "bob@example.org", `["ROLE_USER"]`, "2021-07-15 10:00:00"},
			{3, "bob", hashFor(t, famArgon2i), "Bob@Example.org", `["ROLE_USER","ROLE_ADMIN"]`, "2021-07-16 09:30:00"},
			{4, "carol", hashFor(t, famArgonT2), "carol@example.org", `["ROLE_USER"]`, "2022-03-13 02:30:00"},
			{6, "dave", hashFor(t, famBcrypt13), "dave@example.org", `["ROLE_USER"]`, "2022-11-06 01:30:00"},
			{9, "erin", hashFor(t, famArgonT3P2), "erin@example.org", `["ROLE_USER"]`, "2023-05-01 00:00:01"},
		},
		Invites: []sInvite{
			{3, 1, i64(2), "0123456789abcdef0123456789abcdef"},
			{7, 1, nil, "1123456789abcdef0123456789abcdef"},
			{10, 2, nil, "2123456789abcdef0123456789abcdef"},
			{11, 3, i64(4), "3123456789abcdef0123456789abcdef"},
			{12, 9, nil, "4123456789abcdef0123456789abcdef"},
		},
		Tokens: []sToken{
			{"a0000000000000000000000000000001", 1, "2021-01-16 08:00:00"},
			{"a0000000000000000000000000000002", 1, "2021-08-01 08:00:00"},
			{"b0000000000000000000000000000001", 2, "2021-07-20 12:00:00"},
			{"c0000000000000000000000000000001", 6, "2022-12-01 12:00:00"},
		},
	}
}

func (d dataset) load(t *testing.T, dsn string) {
	t.Helper()
	c := admin(t, dsn)
	ctx := tctx(t)
	for _, u := range d.Users {
		if _, err := c.Exec(ctx, `INSERT INTO users.users (id, username, password, email, roles, created_at) VALUES ($1,$2,$3,$4,$5::json,$6::timestamp)`,
			u.ID, u.Name, u.Hash, u.Email, u.Roles, u.Created); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range d.Invites {
		if _, err := c.Exec(ctx, `INSERT INTO users.invites (id, user_id, used_by_id, code) VALUES ($1,$2,$3,$4)`, v.ID, v.Owner, v.Used, v.Code); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range d.Tokens {
		if _, err := c.Exec(ctx, `INSERT INTO users.api_tokens (key, user_id, created_at) VALUES ($1,$2,$3::timestamp)`, k.Key, k.User, k.Created); err != nil {
			t.Fatal(err)
		}
	}
}

// secrets lists every value that must never appear in a report, a log line
// or an error message, nor (except invitation codes, plaintext by approved
// design) in the target.
func (d dataset) secrets() []string {
	var out []string
	for _, u := range d.Users {
		out = append(out, u.Name, u.Hash, u.Email)
	}
	for _, v := range d.Invites {
		out = append(out, v.Code)
	}
	for _, k := range d.Tokens {
		sum := sha256.Sum256([]byte(k.Key))
		out = append(out, k.Key, hex.EncodeToString(sum[:]))
	}
	return out
}

// seeded returns a legacy database holding stdData and its dataset.
func seeded(t *testing.T) (string, dataset) {
	t.Helper()
	dsn := legacyDB(t)
	d := stdData(t)
	d.load(t, dsn)
	return dsn, d
}

func cfgFor(src, tgt string) Config {
	return Config{SourceDSN: src, TargetDSN: tgt, SourceTimezone: testZone, SourceWritesStopped: true}
}

func mustRun(t *testing.T, cfg Config) *Report {
	t.Helper()
	rep, err := Run(tctx(t), cfg)
	if err != nil {
		t.Fatalf("Run failed: %v (report %+v)", err, rep)
	}
	return rep
}

func wantClass(t *testing.T, err error, class string) *Error {
	t.Helper()
	e, ok := err.(*Error)
	if !ok || e == nil {
		t.Fatalf("want error class %q, got %v", class, err)
	}
	if e.Class != class {
		t.Fatalf("want error class %q, got %q (%v)", class, e.Class, e)
	}
	return e
}

// ---- target state captures ----

var userTableNames = []string{"sessions", "one_time_tokens", "api_tokens", "rss_tokens", "invites", "users"}

// userState is every row of the user-owned tables plus the exact state of
// the three identity sequences. Equal strings mean "nothing changed".
func userState(t *testing.T, dsn string) string {
	t.Helper()
	var b strings.Builder
	c := admin(t, dsn)
	for _, tbl := range userTableNames {
		rows, err := c.Query(tctx(t), `SELECT t::text FROM magnetico_web.`+tbl+` t ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(tbl + ":\n")
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			b.WriteString(s + "\n")
		}
		rows.Close()
	}
	b.WriteString(seqState(t, dsn))
	return b.String()
}

func seqState(t *testing.T, dsn string) string {
	t.Helper()
	var b strings.Builder
	for _, tbl := range []string{"users", "invites", "rss_tokens", "api_tokens"} {
		var last int64
		var called bool
		seq := scalar[string](t, dsn, `SELECT pg_get_serial_sequence($1, 'id')`, "magnetico_web."+tbl)
		if err := admin(t, dsn).QueryRow(tctx(t), `SELECT last_value, is_called FROM `+seq).Scan(&last, &called); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "seq %s last=%d called=%v\n", tbl, last, called)
	}
	return b.String()
}

// unrelatedState covers what the migrator must never touch.
func unrelatedState(t *testing.T, dsn string) string {
	t.Helper()
	var b strings.Builder
	c := admin(t, dsn)
	for _, q := range []string{
		`SELECT t::text FROM magnetico_web.trackers t ORDER BY id`,
		`SELECT t::text FROM magnetico_web.settings t ORDER BY key`,
		`SELECT t::text FROM magnetico_web.web_version t`,
		`SELECT t::text FROM magneticod.schema_version t`,
	} {
		rows, err := c.Query(tctx(t), q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			b.WriteString(s + "\n")
		}
		rows.Close()
	}
	fmt.Fprintf(&b, "torrents=%d\n", scalar[int](t, dsn, `SELECT count(*) FROM magneticod.torrents`))
	return b.String()
}

// sourceState checksums the legacy tables (the source must stay untouched).
func sourceState(t *testing.T, dsn string) string {
	t.Helper()
	var b strings.Builder
	c := admin(t, dsn)
	for _, tbl := range []string{"users.users", "users.invites", "users.api_tokens", "users.password_reset_tokens", "migration_versions"} {
		rows, err := c.Query(tctx(t), `SELECT t::text FROM `+tbl+` t ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			b.WriteString(s + "\n")
		}
		rows.Close()
	}
	return b.String()
}

func reportJSON(t *testing.T, rep *Report) string {
	t.Helper()
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newRole creates a login role on the disposable cluster. Call it BEFORE the
// databases of the test so that its cleanup (LIFO) runs after they are dropped.
func newRole(t *testing.T) string {
	t.Helper()
	cl := pgtest.IntegrationCluster(t)
	name := "mig_" + pgtest.RandomSuffix()
	c, err := pgx.Connect(tctx(t), cl.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	if _, err := c.Exec(tctx(t), `CREATE ROLE `+pgtest.QuoteIdent(name)+` LOGIN`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), cl.AdminDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), `DROP ROLE IF EXISTS `+pgtest.QuoteIdent(name))
	})
	return name
}

// asRole derives a DSN for a role on the same database (trust authentication
// on the disposable cluster).
func asRole(t *testing.T, dsn, role string) string {
	t.Helper()
	i := strings.Index(dsn, "://")
	j := strings.Index(dsn[i+3:], "@")
	if i < 0 || j < 0 {
		t.Fatalf("unexpected DSN shape")
	}
	return dsn[:i+3] + role + dsn[i+3+j:]
}

// grantRead gives role SELECT on everything the dry-run reads.
func grantReadTarget(t *testing.T, tgt, role string) {
	t.Helper()
	q := pgtest.QuoteIdent(role)
	execSQL(t, tgt,
		`GRANT USAGE ON SCHEMA magnetico_web TO `+q,
		`GRANT SELECT ON ALL TABLES IN SCHEMA magnetico_web TO `+q,
		`GRANT SELECT ON ALL SEQUENCES IN SCHEMA magnetico_web TO `+q)
}

func grantReadSource(t *testing.T, src, role string) {
	t.Helper()
	q := pgtest.QuoteIdent(role)
	execSQL(t, src,
		`GRANT USAGE ON SCHEMA users TO `+q,
		`GRANT SELECT ON ALL TABLES IN SCHEMA users TO `+q,
		`GRANT SELECT ON migration_versions TO `+q)
}

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(tctx(t))
	t.Cleanup(cancel)
	return ctx, cancel
}

func quoteRole(r string) string { return pgtest.QuoteIdent(r) }

// execLegacy adds the legacy schema to an existing database (used to put
// source and target into one database).
func execLegacy(t *testing.T, dsn string) {
	t.Helper()
	sql, err := os.ReadFile("testdata/legacy_users.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin(t, dsn).Exec(tctx(t), string(sql)); err != nil {
		t.Fatal(err)
	}
}
