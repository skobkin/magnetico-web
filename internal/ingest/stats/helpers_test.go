package stats

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/opstats"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

var bg = context.Background()

type ownerFlag struct{ v atomic.Bool }

func (o *ownerFlag) Owns() bool { return o.v.Load() }

type wenv struct {
	t     *testing.T
	dsn   string
	pool  *pgxpool.Pool
	clock *fakeClock
	rec   *Recorder
	w     *Writer
	owner *ownerFlag
}

// newWEnv builds a database with the 0.8.0, ingest and statistics schemas and a
// writer over it (not running: tests call flushOnce and the samplers directly).
func newWEnv(t *testing.T, mutate func(*WriterOptions, *wenv)) *wenv {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(bg, dsn); err != nil {
		t.Fatal(err)
	}
	if err := ingestSchemaInit(dsn); err != nil {
		t.Fatal(err)
	}
	if err := opstats.InitSchema(bg, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(bg, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clock := newClock(time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC))
	e := &wenv{t: t, dsn: dsn, pool: pool, clock: clock, rec: New(clock.Now), owner: &ownerFlag{}}
	opt := WriterOptions{DSN: dsn, Clock: clock.Now, Logger: discardLogger(), QuotaMaxBytes: 1 << 30}
	if mutate != nil {
		mutate(&opt, e)
	}
	w, err := NewWriter(e.rec, e.owner, opt)
	if err != nil {
		t.Fatal(err)
	}
	e.w = w
	t.Cleanup(func() { w.flushPool.Close(); w.hbPool.Close(); w.samplePool.Close() })
	return e
}

func (e *wenv) flush() error { return e.w.flushOnce(bg, true) }

func (e *wenv) mustFlush() {
	e.t.Helper()
	if err := e.flush(); err != nil {
		e.t.Fatalf("flush: %v", err)
	}
}

func (e *wenv) n(sql string, args ...any) int64 {
	e.t.Helper()
	var v *int64
	if err := e.pool.QueryRow(bg, sql, args...).Scan(&v); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	if v == nil {
		return -1
	}
	return *v
}

func (e *wenv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(bg, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// total is Σ of a counter over agg_hour.
func (e *wenv) total(col string) int64 {
	return e.n(`SELECT COALESCE(sum(` + col + `), 0) FROM magnetico_stats.agg_hour`)
}

func (e *wenv) seq() int64 {
	return e.n(`SELECT last_flush_seq FROM magnetico_stats.ingest_instances WHERE instance_id = $1`, e.w.instanceID)
}

// ---- a TCP proxy that can cut a connection around COMMIT ------------------------

type faultProxy struct {
	ln     net.Listener
	target string

	mu     sync.Mutex
	conns  []net.Conn
	mode   string // "", "down", "cut-before-commit", "cut-after-commit"
	commit atomic.Int64
}

func newProxy(t *testing.T, dsn string) *faultProxy {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{ln: ln, target: u.Host}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close(); p.closeAll() })
	return p
}

func (p *faultProxy) dsn(dsn string) string {
	u, _ := url.Parse(dsn)
	u.Host = p.ln.Addr().String()
	return u.String()
}

func (p *faultProxy) setMode(m string) {
	p.mu.Lock()
	p.mode = m
	p.mu.Unlock()
	if m == "down" {
		p.closeAll()
	}
}

func (p *faultProxy) getMode() string { p.mu.Lock(); defer p.mu.Unlock(); return p.mode }

func (p *faultProxy) closeAll() {
	p.mu.Lock()
	cs := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}

func (p *faultProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.getMode() == "down" {
			_ = c.Close()
			continue
		}
		s, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c, s)
		p.mu.Unlock()
		go p.pump(c, s)
	}
}

func (p *faultProxy) pump(client, server net.Conn) {
	cutServerToClient := atomic.Bool{}
	go func() { // server → client
		buf := make([]byte, 32<<10)
		for {
			n, err := server.Read(buf)
			if n > 0 && !cutServerToClient.Load() {
				if _, werr := client.Write(buf[:n]); werr != nil {
					return
				}
			} else if n > 0 && cutServerToClient.Load() {
				// The commit reply arrived: the transaction is durably committed.
				_ = client.Close()
				_ = server.Close()
				return
			}
			if err != nil {
				_ = client.Close()
				return
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := client.Read(buf)
		if n > 0 {
			isCommit := bytes.Contains(bytes.ToLower(buf[:n]), []byte("commit"))
			switch {
			case isCommit && p.getMode() == "cut-before-commit":
				p.commit.Add(1)
				_ = client.Close()
				_ = server.Close()
				return
			case isCommit && p.getMode() == "cut-after-commit":
				p.commit.Add(1)
				cutServerToClient.Store(true)
				if _, werr := server.Write(buf[:n]); werr != nil {
					return
				}
				continue
			}
			if _, werr := server.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			_ = server.Close()
			return
		}
	}
}

var _ = io.EOF
var _ = errors.New
var _ = strings.Contains
