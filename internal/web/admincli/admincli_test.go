package admincli

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/accounts"
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

type fakePasswords struct {
	pw  string
	err error
}

func (f fakePasswords) ReadNewPassword() (string, error) { return f.pw, f.err }

func newEnv(t *testing.T) (*Env, *bytes.Buffer, *pgxpool.Pool) {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	if _, err := webschema.Init(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	out := &bytes.Buffer{}
	base, _ := url.Parse("https://magnet.example.org")
	return &Env{
		Store:      accounts.New(pool),
		Trackers:   NewTrackerStore(pool),
		Out:        out,
		PublicBase: base,
		Hasher:     password.NewService(password.NewLimiter(2, time.Minute), password.Params{Memory: 64, Time: 1, Threads: 1}),
		Passwords:  fakePasswords{pw: "operator chosen password"},
	}, out, pool
}

// take returns and clears the captured output.
func take(out *bytes.Buffer) string { s := out.String(); out.Reset(); return s }

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func noSecrets(t *testing.T, s string, extra ...string) {
	t.Helper()
	for _, bad := range append([]string{"$argon2", "$2y$", "password_hash", "operator chosen password"}, extra...) {
		if strings.Contains(s, bad) {
			t.Fatalf("output leaks %q:\n%s", bad, s)
		}
	}
}

func TestUserAddAndList(t *testing.T) {
	e, out, _ := newEnv(t)
	ctx := tctx(t)
	if err := UserAdd(ctx, e, "alice", "alice@example.org", 2); err != nil {
		t.Fatal(err)
	}
	got := take(out)
	if !strings.Contains(got, "2 invitation(s)") || !strings.Contains(got, "https://magnet.example.org/password/setup/") {
		t.Fatalf("add output:\n%s", got)
	}
	noSecrets(t, got)
	if err := UserAdd(ctx, e, "alice", "", 0); !errors.Is(err, accounts.ErrUsernameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	e.InviteQuota = 4
	UserAdd(ctx, e, "bob", "", -1) // negative: the configured quota
	if !strings.Contains(take(out), "4 invitation(s)") {
		t.Fatal("configured quota not used")
	}

	if err := UserList(ctx, e); err != nil {
		t.Fatal(err)
	}
	list := take(out)
	noSecrets(t, list)
	for _, want := range []string{"alice", "bob", "user", "none", "unverified", "id", "last login"} {
		if !strings.Contains(list, want) {
			t.Fatalf("list lacks %q:\n%s", want, list)
		}
	}
}

func TestSetPasswordAndStates(t *testing.T) {
	e, out, pool := newEnv(t)
	ctx := tctx(t)
	UserAdd(ctx, e, "carol", "", 0)
	take(out)
	u, _ := e.Store.CreateUser(ctx, accounts.CreateUserParams{Username: "dan"})
	_ = u
	api, _ := e.Store.CreateAPIToken(ctx, 1, "", nil)
	_ = api
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_web.sessions (token_hash, user_id, expires_at, remember) VALUES ($1, 1, now() + interval '1 hour', false)`, bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}

	if err := UserSetPassword(ctx, e, "carol"); err != nil {
		t.Fatal(err)
	}
	got := take(out)
	if !strings.Contains(got, "1 session(s), 1 API token(s), 0 RSS token(s)") {
		t.Fatalf("set-password output:\n%s", got)
	}
	noSecrets(t, got)
	// The stored value is an argon2id hash, never the password.
	us, _ := e.Store.GetUser(ctx, "carol")
	if password.Classify(us.PasswordHash) == password.ClassNone || strings.Contains(us.PasswordHash, "operator chosen") {
		t.Fatalf("stored hash %q", us.PasswordHash)
	}
	if ok, err := e.hasher().Verify(ctx, "operator chosen password", us.PasswordHash); !ok || err != nil {
		t.Fatalf("password does not verify: %v %v", ok, err)
	}
	UserList(ctx, e)
	if !strings.Contains(take(out), "legacy") { // fast test parameters differ from the production ones
		t.Fatal("password state not shown")
	}

	// Validation and unknown user; nothing is hashed for a missing account.
	e.Passwords = fakePasswords{pw: "short"}
	if err := UserSetPassword(ctx, e, "carol"); !errors.Is(err, password.ErrTooShort) {
		t.Fatalf("short: %v", err)
	}
	if err := UserSetPassword(ctx, e, "nobody"); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	e.Passwords = fakePasswords{err: errors.New("the two entries do not match")}
	if err := UserSetPassword(ctx, e, "carol"); err == nil {
		t.Fatal("mismatch accepted")
	}
}

func TestPasswordLineReader(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want string
		err  bool
	}{
		"newline":       {"secret pass\n", "secret pass", false},
		"crlf":          {"secret pass\r\n", "secret pass", false},
		"no newline":    {"secret pass", "secret pass", false},
		"first line":    {"first\nsecond\n", "first", false},
		"keeps spaces":  {"  spaced  \n", "  spaced  ", false},
		"empty input":   {"", "", true},
		"too long":      {strings.Repeat("a", 1025) + "\n", "", true},
		"empty line ok": {"\n", "", false}, // rejected later by the length rule
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReadPasswordLine(strings.NewReader(tc.in))
			if (err != nil) != tc.err || got != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

func TestResetPasswordLinks(t *testing.T) {
	e, out, _ := newEnv(t)
	ctx := tctx(t)
	UserAdd(ctx, e, "erin", "", 0)
	take(out)
	if err := UserResetPassword(ctx, e, "erin"); err != nil {
		t.Fatal(err)
	}
	got := take(out)
	if !strings.Contains(got, "/password/setup/") || strings.Contains(got, "warning") {
		t.Fatalf("no password yet → setup link without the revocation warning:\n%s", got)
	}
	UserSetPassword(ctx, e, "erin")
	take(out)
	UserDisable(ctx, e, "erin")
	take(out)
	if err := UserResetPassword(ctx, e, "erin"); err != nil {
		t.Fatal(err)
	}
	got = take(out)
	for _, want := range []string{"/password/reset/", "ALL their API tokens", "RSS tokens", "disabled"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reset output lacks %q:\n%s", want, got)
		}
	}
	if err := UserResetPassword(ctx, e, "nobody"); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestDisableEnableRoleRevoke(t *testing.T) {
	e, out, pool := newEnv(t)
	ctx := tctx(t)
	UserAdd(ctx, e, "fred", "", 0)
	take(out)
	pool.Exec(ctx, `INSERT INTO magnetico_web.sessions (token_hash, user_id, expires_at, remember) VALUES ($1, 1, now() + interval '1 hour', false)`, bytes.Repeat([]byte{9}, 32))
	tok, _ := e.Store.CreateAPIToken(ctx, 1, "", nil)
	rss, _ := e.Store.CreateRSSToken(ctx, 1, "", nil)

	UserDisable(ctx, e, "fred")
	if got := take(out); !strings.Contains(got, "1 session(s) deleted") || !strings.Contains(got, "suspended, not revoked") {
		t.Fatalf("disable:\n%s", got)
	}
	UserEnable(ctx, e, "fred")
	if got := take(out); !strings.Contains(got, "work again") {
		t.Fatalf("enable:\n%s", got)
	}
	if _, err := e.Store.AuthenticateAPIToken(ctx, tok.Plaintext); err != nil {
		t.Fatalf("a token did not survive disable/enable: %v", err)
	}
	UserRevokeTokens(ctx, e, "fred")
	if got := take(out); got != "revoked: 1 API tokens, 1 RSS tokens\n" {
		t.Fatalf("revoke: %q", got)
	}
	noSecrets(t, tok.Plaintext+rss.Plaintext+"") // sanity: helper itself
	if _, err := e.Store.AuthenticateRSSToken(ctx, rss.Plaintext); err == nil {
		t.Fatal("revoked token works")
	}
	if err := UserSetRole(ctx, e, "fred", "admin"); err != nil {
		t.Fatal(err)
	}
	take(out)
	if err := UserSetRole(ctx, e, "fred", "root"); !errors.Is(err, accounts.ErrInvalidRole) {
		t.Fatalf("bad role: %v", err)
	}
	for _, f := range []func() error{
		func() error { return UserDisable(ctx, e, "nobody") },
		func() error { return UserEnable(ctx, e, "nobody") },
		func() error { return UserRevokeTokens(ctx, e, "nobody") },
	} {
		if err := f(); !errors.Is(err, accounts.ErrNotFound) {
			t.Fatalf("unknown user: %v", err)
		}
	}
}

func TestInvitationCommands(t *testing.T) {
	e, out, _ := newEnv(t)
	ctx := tctx(t)
	UserAdd(ctx, e, "gina", "", 0)
	take(out)
	if err := InviteAdd(ctx, e, "gina", 2, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	links := strings.Fields(take(out))
	if len(links) != 2 || !strings.HasPrefix(links[0], "https://magnet.example.org/register/") {
		t.Fatalf("links = %v", links)
	}
	InviteList(ctx, e, "gina")
	list := take(out)
	if strings.Count(list, "unused") != 2 || !strings.Contains(list, links[0]) {
		t.Fatalf("list:\n%s", list)
	}
	if err := InviteRevoke(ctx, e, "1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(take(out), "invitation 1 revoked") {
		t.Fatal("revoke message")
	}
	InviteRevoke(ctx, e, "1")
	if !strings.Contains(take(out), "already revoked") {
		t.Fatal("repeat revoke message")
	}
	InviteList(ctx, e, "gina")
	list = take(out)
	if !strings.Contains(list, "revoked") || strings.Contains(list, links[0]) && strings.Contains(strings.Split(list, "\n")[1], "register") {
		t.Fatalf("revoked invitation still shows a link:\n%s", list)
	}
	for _, bad := range []string{"abc", "0", "-3", ""} {
		if err := InviteRevoke(ctx, e, bad); err == nil {
			t.Errorf("id %q accepted", bad)
		}
	}
	if err := InviteRevoke(ctx, e, "4242"); err == nil || !strings.Contains(err.Error(), "no invitation") {
		t.Fatalf("unknown id: %v", err)
	}
	// Suspended owner: no link is offered.
	UserDisable(ctx, e, "gina")
	take(out)
	InviteList(ctx, e, "gina")
	if got := take(out); !strings.Contains(got, "suspended (owner disabled)") || strings.Contains(got, "/register/") {
		t.Fatalf("suspended list:\n%s", got)
	}
	if err := InviteList(ctx, e, "nobody"); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestTrackerCommands(t *testing.T) {
	e, out, pool := newEnv(t)
	ctx := tctx(t)
	TrackerList(ctx, e)
	if n := strings.Count(take(out), "\n"); n != 12 {
		t.Fatalf("seeded list has %d lines", n)
	}
	dense := func() []string {
		t.Helper()
		list, err := e.Trackers.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		urls := make([]string, len(list))
		for i, x := range list {
			if x.Position != i+1 {
				t.Fatalf("positions are not dense: %+v", list)
			}
			urls[i] = x.URL
		}
		return urls
	}
	first := dense()[0]

	if err := TrackerAdd(ctx, e, "udp://new.example.org:1337/announce", 0); err != nil {
		t.Fatal(err)
	}
	if got := take(out); !strings.Contains(got, "position 13") || !strings.Contains(got, "tracker-cache-ttl") {
		t.Fatalf("append:\n%s", got)
	}
	if err := TrackerAdd(ctx, e, "https://second.example.org/announce", 1); err != nil {
		t.Fatal(err)
	}
	take(out)
	urls := dense()
	if urls[0] != "https://second.example.org/announce" || urls[1] != first || len(urls) != 14 {
		t.Fatalf("insert at 1: %v", urls[:3])
	}
	// Move down, then up, by URL and by id.
	TrackerMove(ctx, e, "https://second.example.org/announce", 14)
	urls = dense()
	if urls[13] != "https://second.example.org/announce" || urls[0] != first {
		t.Fatalf("move down: %v", urls)
	}
	var id int64
	pool.QueryRow(ctx, `SELECT id FROM magnetico_web.trackers WHERE url = $1`, "udp://new.example.org:1337/announce").Scan(&id)
	take(out)
	TrackerMove(ctx, e, itoa(id), 1)
	urls = dense()
	if urls[0] != "udp://new.example.org:1337/announce" {
		t.Fatalf("move up by id: %v", urls[:2])
	}
	// Remove by id and by URL.
	if err := TrackerRemove(ctx, e, itoa(id)); err != nil {
		t.Fatal(err)
	}
	if err := TrackerRemove(ctx, e, "https://second.example.org/announce"); err != nil {
		t.Fatal(err)
	}
	take(out)
	if urls = dense(); len(urls) != 12 || urls[0] != first {
		t.Fatalf("after removals: %v", urls)
	}

	for name, f := range map[string]func() error{
		"invalid url":       func() error { return TrackerAdd(ctx, e, "ftp://x.example.org/a", 0) },
		"credentials":       func() error { return TrackerAdd(ctx, e, "http://u:p@x.example.org/a", 0) },
		"duplicate":         func() error { return TrackerAdd(ctx, e, first, 0) },
		"position high":     func() error { return TrackerAdd(ctx, e, "udp://z.example.org:1/a", 99) },
		"position low":      func() error { return TrackerAdd(ctx, e, "udp://z.example.org:1/a", -1) },
		"remove unknown":    func() error { return TrackerRemove(ctx, e, "udp://nope.example.org:1/a") },
		"remove unknown id": func() error { return TrackerRemove(ctx, e, "99999") },
		"move range":        func() error { return TrackerMove(ctx, e, first, 13) },
		"move zero":         func() error { return TrackerMove(ctx, e, first, 0) },
	} {
		if err := f(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(dense()) != 12 {
		t.Fatal("a refused command changed the list")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
