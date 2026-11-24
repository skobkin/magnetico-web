package crawlercli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/ingest/crawlers"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

func newEnv(t *testing.T) (*Env, *bytes.Buffer, *pgxpool.Pool) {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	ctx := context.Background()
	if err := persistence.InitSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	if err := pgqueue.InitSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var out bytes.Buffer
	return &Env{Store: crawlers.New(pool), Out: &out}, &out, pool
}

func field(out, name string) string {
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, name+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func TestCrawlerAddPrintsTokenOnceAndListNeverDoes(t *testing.T) {
	e, out, _ := newEnv(t)
	ctx := context.Background()
	if err := Add(ctx, e, "alpha"); err != nil {
		t.Fatal(err)
	}
	token, id := field(out.String(), "token"), field(out.String(), "id")
	if !strings.HasPrefix(token, crawlers.TokenPrefix) || id == "" {
		t.Fatalf("add output lacks the id/token:\n%s", out)
	}
	if strings.Count(out.String(), token) != 1 {
		t.Fatalf("the token must appear exactly once:\n%s", out)
	}
	out.Reset()
	if err := List(ctx, e); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), token) || strings.Contains(out.String(), crawlers.TokenPrefix) {
		t.Fatalf("list leaks a token:\n%s", out)
	}
	if !strings.Contains(out.String(), "alpha") || !strings.Contains(out.String(), id) {
		t.Fatalf("list lacks the crawler:\n%s", out)
	}
	if err := Add(ctx, e, "ALPHA"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate name: %v", err)
	}
}

func TestRotateKeepsIdentityAndPrintsTheNewTokenOnce(t *testing.T) {
	e, out, _ := newEnv(t)
	ctx := context.Background()
	if err := Add(ctx, e, "alpha"); err != nil {
		t.Fatal(err)
	}
	id1, tok1 := field(out.String(), "id"), field(out.String(), "token")
	out.Reset()
	if err := RotateToken(ctx, e, "alpha"); err != nil {
		t.Fatal(err)
	}
	id2, tok2 := field(out.String(), "id"), field(out.String(), "token")
	if id1 != id2 || tok1 == tok2 || strings.Contains(out.String(), tok1) {
		t.Fatalf("rotate: id %s→%s, tokens %q→%q\n%s", id1, id2, tok1, tok2, out)
	}
	if !strings.Contains(out.String(), "60 seconds") {
		t.Fatalf("the propagation delay is not stated:\n%s", out)
	}
	if err := RotateToken(ctx, e, "nobody"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown name: %v", err)
	}
}

func TestDisableOneCrawlerLeavesOthersUntouched(t *testing.T) {
	e, out, _ := newEnv(t)
	ctx := context.Background()
	_ = Add(ctx, e, "a")
	tokA := field(out.String(), "token")
	out.Reset()
	_ = Add(ctx, e, "b")
	tokB := field(out.String(), "token")
	out.Reset()
	if err := SetEnabled(ctx, e, "a", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "60 seconds") {
		t.Fatalf("the propagation delay is not stated:\n%s", out)
	}
	ra, err := e.Store.LookupDigest(ctx, crawlers.Digest(tokA))
	if err != nil || ra.Enabled {
		t.Fatalf("a: %+v %v", ra, err)
	}
	rb, err := e.Store.LookupDigest(ctx, crawlers.Digest(tokB))
	if err != nil || !rb.Enabled {
		t.Fatalf("b: %+v %v", rb, err)
	}
	if err := SetEnabled(ctx, e, "a", true); err != nil {
		t.Fatal(err)
	}
	if ra, _ = e.Store.LookupDigest(ctx, crawlers.Digest(tokA)); !ra.Enabled {
		t.Fatal("enable did not take effect")
	}
}

func TestCommandsNeverPrintDSNOrHash(t *testing.T) {
	e, out, pool := newEnv(t)
	ctx := context.Background()
	_ = Add(ctx, e, "alpha")
	_ = List(ctx, e)
	_ = RotateToken(ctx, e, "alpha")
	_ = SetEnabled(ctx, e, "alpha", false)
	var hash string
	if err := pool.QueryRow(ctx, `SELECT encode(token_hash,'hex') FROM magnetico_ingest.crawlers`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{hash, "postgres://", "password"} {
		if strings.Contains(out.String(), bad) {
			t.Fatalf("output contains %q:\n%s", bad, out)
		}
	}
}
