package auth

import (
	"context"
	"math/rand/v2"
	"time"
)

// Background is the purger: it deletes expired sessions and expired one-time
// links (W2's residual). It runs once shortly after start and then every
// PurgeEvery with ±20 % jitter, and returns when ctx is cancelled. Each run's
// context derives from ctx, so shutdown interrupts a running purge; Server.Run
// waits for this function to return before it closes the database.
func (a *Auth) Background(ctx context.Context) {
	wait := a.cfg.PurgeFirst
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		a.purgeOnce(ctx)
		wait = jitter(a.cfg.PurgeEvery)
	}
}

func jitter(d time.Duration) time.Duration {
	return d + time.Duration((rand.Float64()*0.4-0.2)*float64(d))
}

func (a *Auth) purgeOnce(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, purgeRunTimeout)
	defer cancel()
	sessions, err := a.cfg.Store.PurgeSessions(rctx, a.cfg.Lifetimes.Idle)
	if err != nil && ctx.Err() == nil {
		a.log.Warn("session purge failed", "error_type", typeName(err))
	}
	links, err2 := a.cfg.Store.PurgeExpiredLinks(rctx)
	if err2 != nil && ctx.Err() == nil {
		a.log.Warn("one-time link purge failed", "error_type", typeName(err2))
	}
	if sessions > 0 || links > 0 {
		a.log.Debug("purged", "sessions", sessions, "links", links)
	}
}
