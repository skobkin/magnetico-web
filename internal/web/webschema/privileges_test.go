package webschema

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestDocumentedMinimumGrantsSuffice(t *testing.T) {
	cl := pgtest.IntegrationCluster(t)
	dsn := webDB(t)
	rt, op := roleSetup(t, cl, dsn)

	rtPool := openPool(t, roleDSN(t, dsn, rt))
	if err := VerifyTorrentPrivileges(tctx(t), rtPool); err != nil {
		t.Fatalf("torrent privileges: %v", err)
	}
	if err := persistence.VerifySchema(tctx(t), rtPool); err != nil {
		t.Fatalf("persistence.VerifySchema as the runtime role: %v", err)
	}
	if err := Verify(tctx(t), rtPool, Runtime); err != nil {
		t.Fatalf("runtime: %v", err)
	}
	// The runtime role cannot write trackers, so it is not an operator.
	err := Verify(tctx(t), rtPool, Operator)
	if !errors.Is(err, ErrWebPrivileges) || !strings.Contains(err.Error(), "magnetico_web.trackers: INSERT") {
		t.Fatalf("runtime as operator: %v", err)
	}
	if err := Verify(tctx(t), openPool(t, roleDSN(t, dsn, op)), Operator); err != nil {
		t.Fatalf("operator: %v", err)
	}

	// The runtime DML works, including INSERT into identity columns (no
	// separate sequence grant is needed) and the session/link DELETEs.
	conn, err := pgx.Connect(tctx(t), roleDSN(t, dsn, rt))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(tctx(t))
	for _, q := range []string{
		`INSERT INTO magnetico_web.users (username) VALUES ('u1')`,
		`UPDATE magnetico_web.users SET active = false WHERE username = 'u1'`,
		`INSERT INTO magnetico_web.api_tokens (token_hash, user_id) SELECT '\x0000000000000000000000000000000000000000000000000000000000000001'::bytea, id FROM magnetico_web.users`,
		`INSERT INTO magnetico_web.rss_tokens (token_hash, user_id) SELECT '\x0000000000000000000000000000000000000000000000000000000000000002'::bytea, id FROM magnetico_web.users`,
		`INSERT INTO magnetico_web.invites (owner_id, code) SELECT id, 'AAAAAAAAAAAAAAAAAAAAAA' FROM magnetico_web.users`,
		`INSERT INTO magnetico_web.sessions (token_hash, user_id, expires_at, remember) SELECT '\x0000000000000000000000000000000000000000000000000000000000000003'::bytea, id, now() + interval '1 hour', false FROM magnetico_web.users`,
		`DELETE FROM magnetico_web.sessions`,
		`INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, expires_at) SELECT '\x0000000000000000000000000000000000000000000000000000000000000004'::bytea, id, 'setup', now() + interval '1 hour' FROM magnetico_web.users`,
		`DELETE FROM magnetico_web.one_time_tokens`,
		`SELECT count(*) FROM magnetico_web.trackers`,
	} {
		if _, err := conn.Exec(tctx(t), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// No DDL for the runtime role.
	if _, err := conn.Exec(tctx(t), `CREATE TABLE magnetico_web.sneaky (x int)`); err == nil {
		t.Fatal("the runtime role must not be able to create objects")
	}
}

// Revoking any single documented privilege is detected, named, and reported
// as ErrWebPrivileges (never a raw driver error).
func TestEveryMissingPrivilegeIsNamed(t *testing.T) {
	cl := pgtest.IntegrationCluster(t)
	dsn := webDB(t)
	rt, _ := roleSetup(t, cl, dsn)
	rtPool := openPool(t, roleDSN(t, dsn, rt))

	revokeTable := func(tp tablePriv) {
		for _, p := range tp.privs {
			t.Run(tp.table+" "+p, func(t *testing.T) {
				execSQL(t, dsn, "REVOKE "+p+" ON "+tp.table+" FROM "+pgtest.QuoteIdent(rt))
				defer execSQL(t, dsn, "GRANT "+p+" ON "+tp.table+" TO "+pgtest.QuoteIdent(rt))
				var err error
				want := tp.table + ": " + p
				if strings.HasPrefix(tp.table, "magneticod.") {
					err = VerifyTorrentPrivileges(tctx(t), rtPool)
				} else {
					err = Verify(tctx(t), rtPool, Runtime)
				}
				var se *StateError
				if !errors.As(err, &se) || !errors.Is(err, ErrWebPrivileges) || !strings.Contains(strings.Join(se.Details, ";"), want) {
					t.Fatalf("err = %v, want ErrWebPrivileges naming %q", err, want)
				}
			})
		}
	}
	for _, tp := range torrentPrivileges {
		revokeTable(tp)
	}
	for _, tp := range webPrivileges {
		revokeTable(tp)
	}
	t.Run("schema usage", func(t *testing.T) {
		execSQL(t, dsn, "REVOKE USAGE ON SCHEMA magnetico_web FROM "+pgtest.QuoteIdent(rt))
		defer execSQL(t, dsn, "GRANT USAGE ON SCHEMA magnetico_web TO "+pgtest.QuoteIdent(rt))
		if err := Verify(tctx(t), rtPool, Runtime); !errors.Is(err, ErrWebPrivileges) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("magneticod usage", func(t *testing.T) {
		execSQL(t, dsn, "REVOKE USAGE ON SCHEMA magneticod FROM "+pgtest.QuoteIdent(rt))
		defer execSQL(t, dsn, "GRANT USAGE ON SCHEMA magneticod TO "+pgtest.QuoteIdent(rt))
		err := VerifyTorrentPrivileges(tctx(t), rtPool)
		if !errors.Is(err, ErrWebPrivileges) || !strings.Contains(err.Error(), "magneticod: USAGE") {
			t.Fatalf("err = %v", err)
		}
	})
}
