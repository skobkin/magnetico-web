package state

// RunLock: a session-level advisory lock on a dedicated connection,
// excluding concurrent migrator instances against one target for the whole
// run. Every use of the lock-owning connection — acquisition, liveness
// pings, index DDL, release — is serialized behind one mutex, because
// pgx.Conn permits no concurrent use: the liveness monitor pings only when
// the connection is idle (the mutex is free), so it can never race or
// interrupt a long-running index build.
//
// A silently-dead lock connection would void exclusivity; loss is detected
// by any lock-connection operation error or the first idle ping after a gap
// and reported through Dead, which the runner treats as fatal (never
// continue unprotected). PostgreSQL auto-releases the lock when the
// connection dies — the backstop that keeps a dead holder from blocking
// others.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrLockBusy reports another migration holds the run lock on the target.
var ErrLockBusy = errors.New("another migration run holds the lock on this target")

// pingInterval is the idle-time liveness probe cadence.
const pingInterval = 5 * time.Second

// RunLock owns the advisory-lock connection.
type RunLock struct {
	mu   sync.Mutex
	conn *pgx.Conn

	deadErr error // guarded by mu; nil while the lock is held
	deadCh  chan struct{}
}

// AcquireRunLock connects and takes the session advisory lock. The returned
// lock must be Released (or dies with its connection).
func AcquireRunLock(ctx context.Context, dsn string) (*RunLock, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("lock connection: %w", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1)`, migrationLockAdvisoryKey).Scan(&acquired); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	if !acquired {
		conn.Close(ctx)
		return nil, ErrLockBusy
	}
	return &RunLock{conn: conn, deadCh: make(chan struct{})}, nil
}

// With runs fn on the lock-owning connection under the serialization mutex.
// It fails immediately if the lock has been lost.
func (l *RunLock) With(ctx context.Context, fn func(conn *pgx.Conn) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.withLocked(ctx, fn)
}

func (l *RunLock) withLocked(ctx context.Context, fn func(conn *pgx.Conn) error) error {
	if l.deadErr != nil {
		return fmt.Errorf("migration lock lost: %w", l.deadErr)
	}
	if err := fn(l.conn); err != nil {
		if isConnClosed(err) {
			l.markDeadLocked(err)
		}
		return err
	}
	return nil
}

// markDeadLocked records lock loss; the caller must hold l.mu.
func (l *RunLock) markDeadLocked(err error) {
	if l.deadErr == nil {
		l.deadErr = err
		close(l.deadCh)
	}
}

// markDead records lock loss from outside the serialization mutex.
func (l *RunLock) markDead(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.markDeadLocked(err)
}

// Lost reports a previously detected lock loss; nil means the lock is (still)
// held.
func (l *RunLock) Lost() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deadErr != nil {
		return fmt.Errorf("migration lock lost: %w", l.deadErr)
	}
	return nil
}

// Dead returns a channel closed when the lock connection is lost.
func (l *RunLock) Dead() <-chan struct{} { return l.deadCh }

// Ping verifies the lock connection is still alive. Called only while idle
// (With serializes against index DDL and other lock-connection work).
func (l *RunLock) Ping(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.withLocked(ctx, func(conn *pgx.Conn) error {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return conn.Ping(pingCtx)
	})
}

// Release unlocks and closes the connection. Releasing a dead lock is a
// no-op on the unlock statement.
func (l *RunLock) Release(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		if l.deadErr == nil {
			// Best-effort explicit unlock; connection close would release
			// anyway.
			unlockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, _ = l.conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockAdvisoryKey)
			cancel()
		}
		l.conn.Close(ctx)
		l.conn = nil
	}
}

// isConnClosed reports errors that mean the lock connection itself is gone.
func isConnClosed(err error) bool {
	return errors.Is(err, pgconn.ErrConnClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// MonitorLockLoss runs idle liveness pings until the lock dies or ctx is
// cancelled. On detection it invokes onLost once (from the monitor
// goroutine) and closes Dead.
func (l *RunLock) MonitorLockLoss(ctx context.Context, onLost func(error)) {
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-l.deadCh:
				return
			case <-ticker.C:
				// With() blocks while a build holds the connection, so this
				// ping waits for an idle window instead of interrupting DDL.
				if err := l.Ping(ctx); err != nil {
					l.markDead(err)
					if onLost != nil {
						onLost(err)
					}
					return
				}
			}
		}
	}()
}
