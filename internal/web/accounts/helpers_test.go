package accounts

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/password"
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

// newStore returns a Store over a disposable database with the real schema.
func newStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	if _, err := webschema.Init(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 24
	pool, err := pgxpool.NewWithConfig(tctx(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return New(pool), pool
}

var (
	hashOnce sync.Once
	hashVal  string
)

// testHash is a valid (fast, legacy-class) argon2id hash.
func testHash(t *testing.T) string {
	t.Helper()
	hashOnce.Do(func() {
		svc := password.NewService(password.NewLimiter(1, time.Minute), password.Params{Memory: 64, Time: 1, Threads: 1})
		h, err := svc.Hash(context.Background(), "test password 1")
		if err != nil {
			panic(err)
		}
		hashVal = h
	})
	return hashVal
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(tctx(t), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(tctx(t), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// mkUser creates an active user that already has a password.
func mkUser(t *testing.T, s *Store, pool *pgxpool.Pool, name string) User {
	t.Helper()
	res, err := s.CreateUser(tctx(t), CreateUserParams{Username: name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompletePasswordLink(tctx(t), res.Setup.Token, PurposeSetup, testHash(t)); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUser(tctx(t), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func addSession(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	b := make([]byte, 32)
	rand.Read(b)
	exec(t, pool, `INSERT INTO magnetico_web.sessions (token_hash, user_id, expires_at, remember) VALUES ($1,$2, now() + interval '1 hour', false)`, b, userID)
}

func sessions(t *testing.T, pool *pgxpool.Pool, userID int64) int {
	return count(t, pool, `SELECT count(*) FROM magnetico_web.sessions WHERE user_id = $1`, userID)
}

// tableDump is a deterministic dump of the rows that disable/enable must not touch.
func tableDump(t *testing.T, pool *pgxpool.Pool, userID int64) string {
	t.Helper()
	var out string
	for _, q := range []string{
		`SELECT t::text FROM magnetico_web.api_tokens t WHERE user_id = $1 ORDER BY id`,
		`SELECT t::text FROM magnetico_web.rss_tokens t WHERE user_id = $1 ORDER BY id`,
		`SELECT t::text FROM magnetico_web.invites t WHERE owner_id = $1 ORDER BY id`,
		`SELECT t::text FROM magnetico_web.one_time_tokens t WHERE user_id = $1 ORDER BY created_at`,
		`SELECT (password_hash, password_changed_at, email, email_verified_at, last_login_at, role)::text FROM magnetico_web.users WHERE id = $1`,
	} {
		rows, err := pool.Query(tctx(t), q, userID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out += s + "\n"
		}
		rows.Close()
	}
	return out
}

func mustErr(t *testing.T, err, want error, what string) {
	t.Helper()
	if err == nil || !errorsIs(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

func errorsIs(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

var _ = fmt.Sprint
