package data

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/admin/access"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

var bg = context.Background()
var admin = access.Grant(1, "root")

type env struct {
	t    *testing.T
	dsn  string
	pool *pgxpool.Pool // the owner
	rest *Store        // the restricted web role: column grants only
}

// newEnv builds the schemas and a role that can read exactly the columns the
// documented grants allow: any query that touched a credential column, the
// quarantine payload or the info hash fails with a permission error.
func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(bg, dsn); err != nil {
		t.Fatal(err)
	}
	if err := pgqueue.InitSchema(bg, dsn); err != nil {
		t.Fatal(err)
	}
	if _, err := webschema.Init(bg, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(bg, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	role := "adm_" + pgtest.RandomSuffix()
	q := pgtest.QuoteIdent(role)
	for _, s := range []string{
		`CREATE ROLE ` + q + ` LOGIN`,
		`GRANT USAGE ON SCHEMA magnetico_web, magnetico_ingest TO ` + q,
		`GRANT SELECT (id, username, email, email_verified_at, active, role, created_at, last_login_at) ON magnetico_web.users TO ` + q,
		`GRANT SELECT (id, owner_id, used_by, used_at, created_at, expires_at, revoked_at) ON magnetico_web.invites TO ` + q,
		`GRANT SELECT ON magnetico_web.trackers TO ` + q,
		`GRANT SELECT (id, reason_code, detail, attempts, payload_bytes, received_at, quarantined_at) ON magnetico_ingest.quarantine TO ` + q,
	} {
		if _, err := pool.Exec(bg, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	cl := pgtest.IntegrationCluster(t)
	t.Cleanup(func() {
		if c, err := pgx.Connect(bg, dsn); err == nil {
			_, _ = c.Exec(bg, `DROP OWNED BY `+q)
			c.Close(bg)
		}
		if c, err := pgx.Connect(bg, cl.AdminDSN); err == nil {
			_, _ = c.Exec(bg, `DROP ROLE IF EXISTS `+q)
			c.Close(bg)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = role
	rp, err := pgxpool.NewWithConfig(bg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rp.Close)
	return &env{t: t, dsn: dsn, pool: pool, rest: New(rp)}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(bg, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) users(n int) {
	e.exec(`INSERT INTO magnetico_web.users (username, email, password_hash, role, created_at)
SELECT 'user' || lpad(g::text, 4, '0'), 'user' || g || '@example.org', '$argon2id$v=19$m=65536,t=3,p=4$SECRETSALT$SECRETHASH' || g, CASE WHEN g = 1 THEN 'admin' ELSE 'user' END,
       now() - make_interval(secs => g) FROM generate_series(1, $1) g`, n)
}

func TestCredentialColumnsAreNeverSelected(t *testing.T) {
	e := newEnv(t)
	e.users(3)
	e.exec(`INSERT INTO magnetico_web.invites (owner_id, code, created_at) SELECT id, 'SECRETINVITECODE' || id || repeat('x', 12), now() FROM magnetico_web.users`)
	e.exec(`INSERT INTO magnetico_ingest.quarantine (info_hash, format, payload, payload_bytes, reason_code, detail, attempts, received_at)
VALUES ('\xdeadbeef', 1, '\x0102', 2, 'invalid_record', 'detail text', 1, now())`)
	// Every listing works through a role that has no grant on any credential column.
	if pg, err := e.rest.Users(bg, admin, "", 0); err != nil || len(pg.Items) != 3 {
		t.Fatalf("Users: %v %v", pg, err)
	}
	if pg, err := e.rest.Invitations(bg, admin, 0); err != nil || len(pg.Items) != 3 {
		t.Fatalf("Invitations: %v %v", pg, err)
	}
	if pg, err := e.rest.Quarantine(bg, admin, 0); err != nil || len(pg.Items) != 1 {
		t.Fatalf("Quarantine: %v %v", pg, err)
	}
	if rows, err := e.rest.Trackers(bg, admin); err != nil || len(rows) == 0 {
		t.Fatalf("Trackers: %v %v", rows, err)
	}
	// And the role really cannot read the forbidden columns: the guarantee is structural, not accidental.
	for _, q := range []string{
		`SELECT password_hash FROM magnetico_web.users`, `SELECT code FROM magnetico_web.invites`,
		`SELECT payload FROM magnetico_ingest.quarantine`, `SELECT info_hash FROM magnetico_ingest.quarantine`, `SELECT * FROM magnetico_web.users`,
	} {
		rp := e.rest.db.(*pgxpool.Pool)
		if _, err := rp.Exec(bg, q); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %v, want permission denied", q, err)
		}
	}
	// The row types cannot carry a credential, a code, a hash or a payload.
	for _, v := range []any{UserRow{}, InvitationRow{}, QuarantineRow{}, TrackerRow{}} {
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumField(); i++ {
			n := strings.ToLower(typ.Field(i).Name)
			for _, bad := range []string{"password", "hash", "code", "token", "secret", "payload", "infohash"} {
				if strings.Contains(n, bad) && n != "payloadbytes" {
					t.Errorf("%s has a field named %s", typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}

func TestInvitationStatusIsDerivedAndCodesAreAbsent(t *testing.T) {
	e := newEnv(t)
	e.users(3)
	e.exec(`INSERT INTO magnetico_web.invites (owner_id, code, used_by, used_at, created_at) VALUES (1, 'USEDCODE' || repeat('a', 20), 2, now(), now() - interval '3 days')`)
	e.exec(`INSERT INTO magnetico_web.invites (owner_id, code, revoked_at, created_at) VALUES (1, 'REVOKEDCODE' || repeat('b', 20), now(), now() - interval '2 days')`)
	e.exec(`INSERT INTO magnetico_web.invites (owner_id, code, expires_at, created_at) VALUES (1, 'EXPIREDCODE' || repeat('c', 20), now() - interval '1 hour', now() - interval '1 day')`)
	e.exec(`INSERT INTO magnetico_web.invites (owner_id, code, created_at) VALUES (1, 'UNUSEDCODE' || repeat('d', 20), now())`)
	pg, err := e.rest.Invitations(bg, admin, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, i := range pg.Items {
		got[i.Status] = i.UsedBy + "/" + i.Owner
	}
	for _, want := range []string{"used", "revoked", "expired", "unused"} {
		if _, ok := got[want]; !ok {
			t.Errorf("status %q missing: %v", want, got)
		}
	}
	if got["used"] != "user0002/user0001" {
		t.Errorf("used invitation: %q", got["used"])
	}
	if s := fmt.Sprintf("%+v", pg); strings.Contains(s, "CODE") {
		t.Errorf("an invitation code reached the result: %s", s)
	}
	// "Invited by" on the users listing.
	up, err := e.rest.Users(bg, admin, "user0002", 0)
	if err != nil || len(up.Items) != 1 || up.Items[0].InvitedBy != "user0001" {
		t.Fatalf("invited by: %+v %v", up.Items, err)
	}
}

func TestPaginationIsKeysetBoundedAndStable(t *testing.T) {
	e := newEnv(t)
	e.users(120)
	seen := map[int64]bool{}
	var after int64
	var sizes []int
	for {
		pg, err := e.rest.Users(bg, admin, "", after)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(pg.Items))
		for _, u := range pg.Items {
			if seen[u.ID] || u.ID <= after {
				t.Fatalf("user %d repeated or out of order after %d", u.ID, after)
			}
			seen[u.ID] = true
		}
		if pg.Next == 0 {
			break
		}
		after = pg.Next
	}
	if len(seen) != 120 || fmt.Sprint(sizes) != "[50 50 20]" {
		t.Fatalf("%d users in pages %v", len(seen), sizes)
	}
	// A row inserted between two page requests changes nothing already shown or still to come.
	first, _ := e.rest.Users(bg, admin, "", 0)
	e.exec(`INSERT INTO magnetico_web.users (username) VALUES ('latecomer')`)
	second, _ := e.rest.Users(bg, admin, "", first.Next)
	if len(second.Items) == 0 || second.Items[0].ID != first.Next+1 {
		t.Fatalf("the second page does not continue exactly after the first: %+v", second.Items[:1])
	}
	// Quarantine and invitations: newest first, bounded, stable.
	e.exec(`INSERT INTO magnetico_ingest.quarantine (info_hash, format, payload, payload_bytes, reason_code, attempts, received_at)
SELECT decode(lpad(to_hex(g), 40, '0'), 'hex'), 1, '\x00', 1, 'invalid_record', 1, now() FROM generate_series(1, 120) g`)
	var qseen int
	var prev int64 = 1 << 62
	var qafter int64
	for {
		pg, err := e.rest.Quarantine(bg, admin, qafter)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range pg.Items {
			if r.ID >= prev {
				t.Fatalf("quarantine not newest-first: %d after %d", r.ID, prev)
			}
			prev = r.ID
			qseen++
		}
		if pg.Next == 0 {
			break
		}
		qafter = pg.Next
	}
	if qseen != 120 {
		t.Fatalf("%d quarantine rows listed", qseen)
	}
}

func TestUserSearchIsBoundedAndEscapesWildcards(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO magnetico_web.users (username, email) VALUES ('plain', 'a@example.org'), ('100%real', NULL), ('under_score', NULL), ('backslash\x', NULL), ('Mixed', 'Person@Example.ORG')`)
	for q, want := range map[string]int{
		"plain": 1, "PLAIN": 1, "%": 1, "_": 1, "100%": 1, "under_": 1, "\\x": 1, "example.org": 2, "person@example": 1, "nomatch": 0,
	} {
		pg, err := e.rest.Users(bg, admin, q, 0)
		if err != nil || len(pg.Items) != want {
			t.Errorf("search %q: %d rows (%v), want %d: a wildcard in the text must be literal", q, len(pg.Items), err, want)
		}
	}
	if all, _ := e.rest.Users(bg, admin, "", 0); len(all.Items) != 5 {
		t.Fatalf("no search: %d rows", len(all.Items))
	}
	for _, bad := range []string{strings.Repeat("a", MaxSearch+1), "bad\xff", "nul\x00byte"} {
		if _, err := e.rest.Users(bg, admin, bad, 0); err != ErrBadInput {
			t.Errorf("%q: %v, want ErrBadInput", bad, err)
		}
	}
	// A search cannot inject SQL.
	if _, err := e.rest.Users(bg, admin, "x'; DROP TABLE magnetico_web.users;--", 0); err != nil {
		t.Fatal(err)
	}
	if n := e.n(`SELECT count(*) FROM magnetico_web.users`); n != 5 {
		t.Fatalf("users table damaged: %d", n)
	}
}

func (e *env) n(q string) int64 {
	var v int64
	if err := e.pool.QueryRow(bg, q).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

func TestEveryFunctionRefusesAnInvalidPrincipal(t *testing.T) {
	e := newEnv(t)
	var zero access.Principal
	if _, err := e.rest.Users(bg, zero, "", 0); err != access.ErrForbidden {
		t.Errorf("Users: %v", err)
	}
	if _, err := e.rest.Invitations(bg, zero, 0); err != access.ErrForbidden {
		t.Errorf("Invitations: %v", err)
	}
	if _, err := e.rest.Quarantine(bg, zero, 0); err != access.ErrForbidden {
		t.Errorf("Quarantine: %v", err)
	}
	if _, err := e.rest.Trackers(bg, zero); err != access.ErrForbidden {
		t.Errorf("Trackers: %v", err)
	}
}

func TestQuarantineDetailAndTrackerURLsAreClipped(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO magnetico_ingest.quarantine (info_hash, format, payload, payload_bytes, reason_code, detail, attempts, received_at)
VALUES ('\x01', 1, '\x00', 1, 'x', repeat('é', 5000), 1, now() - interval '1 minute')`)
	pg, err := e.rest.Quarantine(bg, admin, 0)
	if err != nil || len([]rune(pg.Items[0].Detail)) > 201 {
		t.Fatalf("detail not clipped: %d runes (%v)", len([]rune(pg.Items[0].Detail)), err)
	}
	if pg.Items[0].QuarantinedAt.Before(time.Now().Add(-time.Hour)) {
		t.Fatal("timestamps")
	}
}
