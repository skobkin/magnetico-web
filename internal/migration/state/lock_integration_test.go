package state

// Suite ⑥ component: lock-connection death detection. The backend behind
// the lock connection is terminated out-of-band; an idle ping and the
// monitor must both report the loss (the runner stops fatally on either).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// terminateLockBackend kills the server backend currently holding the
// migration advisory lock in the given database.
func terminateLockBackend(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool parse: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool connect: %v", err)
	}
	defer pool.Close()
	var pid int32
	// A 64-bit advisory key is stored as (classid, objid), both 32-bit oids.
	// pg_locks is cluster-wide — filter to this test's own database so
	// parallel tests using the same key never terminate each other's backend.
	if err := pool.QueryRow(ctx,
		`SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND classid = $1 AND objid = $2
         AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		int32(uint32(migrationLockAdvisoryKey>>32)), int32(uint32(migrationLockAdvisoryKey&0xFFFFFFFF))).Scan(&pid); err != nil {
		t.Fatalf("find lock backend: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate lock backend: %v", err)
	}
}

func TestRunLockPingDetectsTerminatedBackend(t *testing.T) {
	t.Parallel()

	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	lock, err := AcquireRunLock(context.Background(), dsn)
	if err != nil {
		t.Fatalf("AcquireRunLock() error = %v", err)
	}
	defer lock.Release(context.Background())

	if err := lock.Ping(context.Background()); err != nil {
		t.Fatalf("initial Ping() error = %v", err)
	}
	if err := lock.Lost(); err != nil {
		t.Fatalf("Lost() before termination = %v, want nil", err)
	}

	terminateLockBackend(t, dsn)

	if err := lock.Ping(context.Background()); err == nil {
		t.Fatal("Ping() after termination must fail")
	}
	if err := lock.Lost(); err == nil {
		t.Fatal("Lost() after termination must report the loss")
	}
}

func TestMonitorLockLossInvokesCallback(t *testing.T) {
	t.Parallel()

	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	lock, err := AcquireRunLock(context.Background(), dsn)
	if err != nil {
		t.Fatalf("AcquireRunLock() error = %v", err)
	}
	defer lock.Release(context.Background())

	lost := make(chan error, 1)
	monitorCtx, stop := context.WithCancel(context.Background())
	defer stop()
	// MonitorLockLoss is fire-and-forget (spawns its own goroutine).
	lock.MonitorLockLoss(monitorCtx, func(err error) { lost <- err })

	terminateLockBackend(t, dsn)

	select {
	case err := <-lost:
		if err == nil {
			t.Fatal("onLost callback received nil error")
		}
		if err := lock.Lost(); err == nil {
			t.Fatal("Lost() must be marked when the monitor fires")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("monitor did not detect the terminated lock backend in time")
	}
}
