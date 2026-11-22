package admincli

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
)

// TrackerStore edits magnetico_web.trackers. Every write runs in one
// transaction under webschema.TrackersAdvisoryKey (the same key the seed
// uses) and leaves positions dense, 1..n.
type TrackerStore struct{ pool *pgxpool.Pool }

func NewTrackerStore(pool *pgxpool.Pool) *TrackerStore { return &TrackerStore{pool: pool} }

type Tracker struct {
	Position int
	ID       int64
	URL      string
}

func (t *TrackerStore) list(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]Tracker, error) {
	rows, err := q.Query(ctx, `SELECT position, id, url FROM magnetico_web.trackers ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tracker
	for rows.Next() {
		var x Tracker
		if err := rows.Scan(&x.Position, &x.ID, &x.URL); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (t *TrackerStore) List(ctx context.Context) ([]Tracker, error) { return t.list(ctx, t.pool) }

func (t *TrackerStore) write(ctx context.Context, fn func(tx pgx.Tx, cur []Tracker) error) error {
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, webschema.TrackersAdvisoryKey); err != nil {
		return err
	}
	cur, err := t.list(ctx, tx)
	if err != nil {
		return err
	}
	if err := fn(tx, cur); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE magnetico_web.trackers t SET position = r.rn, updated_at = now()
        FROM (SELECT id, row_number() OVER (ORDER BY position, id)::int AS rn FROM magnetico_web.trackers) r
        WHERE t.id = r.id AND t.position <> r.rn`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// find resolves an all-digits argument as an id and anything else as an exact URL.
func find(cur []Tracker, ref string) (Tracker, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil && isDigits(ref) {
		for _, x := range cur {
			if x.ID == id {
				return x, nil
			}
		}
		return Tracker{}, cmdErr("no tracker with id %d", id)
	}
	for _, x := range cur {
		if x.URL == ref {
			return x, nil
		}
	}
	return Tracker{}, cmdErr("no tracker with that URL")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Add inserts at position (0: append).
func (t *TrackerStore) Add(ctx context.Context, rawURL string, position int) (Tracker, error) {
	var added Tracker
	if err := accounts.ValidateTrackerURL(rawURL); err != nil {
		return added, err
	}
	err := t.write(ctx, func(tx pgx.Tx, cur []Tracker) error {
		for _, x := range cur {
			if x.URL == rawURL {
				return cmdErr("that tracker is already in the list")
			}
		}
		pos := position
		if pos == 0 {
			pos = len(cur) + 1
		}
		if pos < 1 || pos > len(cur)+1 {
			return cmdErr("position must be between 1 and %d", len(cur)+1)
		}
		if _, err := tx.Exec(ctx, `UPDATE magnetico_web.trackers SET position = position + 1, updated_at = now() WHERE position >= $1`, pos); err != nil {
			return err
		}
		added = Tracker{Position: pos, URL: rawURL}
		return tx.QueryRow(ctx, `INSERT INTO magnetico_web.trackers (url, position) VALUES ($1, $2) RETURNING id`, rawURL, pos).Scan(&added.ID)
	})
	return added, err
}

func (t *TrackerStore) Remove(ctx context.Context, ref string) (Tracker, error) {
	var gone Tracker
	err := t.write(ctx, func(tx pgx.Tx, cur []Tracker) error {
		x, err := find(cur, ref)
		if err != nil {
			return err
		}
		gone = x
		_, err = tx.Exec(ctx, `DELETE FROM magnetico_web.trackers WHERE id = $1`, x.ID)
		return err
	})
	return gone, err
}

func (t *TrackerStore) Move(ctx context.Context, ref string, position int) (Tracker, error) {
	var moved Tracker
	err := t.write(ctx, func(tx pgx.Tx, cur []Tracker) error {
		x, err := find(cur, ref)
		if err != nil {
			return err
		}
		if position < 1 || position > len(cur) {
			return cmdErr("position must be between 1 and %d", len(cur))
		}
		// cur is dense and ordered, so x.Position is its index+1.
		switch {
		case position > x.Position:
			_, err = tx.Exec(ctx, `UPDATE magnetico_web.trackers SET position = position - 1, updated_at = now() WHERE position > $1 AND position <= $2`, x.Position, position)
		case position < x.Position:
			_, err = tx.Exec(ctx, `UPDATE magnetico_web.trackers SET position = position + 1, updated_at = now() WHERE position >= $2 AND position < $1`, x.Position, position)
		}
		if err != nil {
			return err
		}
		x.Position = position
		moved = x
		_, err = tx.Exec(ctx, `UPDATE magnetico_web.trackers SET position = $2, updated_at = now() WHERE id = $1`, x.ID, position)
		return err
	})
	return moved, err
}

const trackerNote = "running web services pick this up within the configured tracker-cache-ttl\n"

func TrackerList(ctx context.Context, e *Env) error {
	list, err := e.Trackers.List(ctx)
	if err != nil {
		return err
	}
	for _, x := range list {
		e.printf("%d\t%d\t%s\n", x.Position, x.ID, x.URL)
	}
	return nil
}

func TrackerAdd(ctx context.Context, e *Env, rawURL string, position int) error {
	x, err := e.Trackers.Add(ctx, rawURL, position)
	if err != nil {
		return err
	}
	e.printf("added tracker %d at position %d\n%s", x.ID, x.Position, trackerNote)
	return nil
}

func TrackerRemove(ctx context.Context, e *Env, ref string) error {
	x, err := e.Trackers.Remove(ctx, ref)
	if err != nil {
		return err
	}
	e.printf("removed tracker %d (%s)\n%s", x.ID, x.URL, trackerNote)
	return nil
}

func TrackerMove(ctx context.Context, e *Env, ref string, position int) error {
	x, err := e.Trackers.Move(ctx, ref, position)
	if err != nil {
		return err
	}
	e.printf("moved tracker %d to position %d\n%s", x.ID, x.Position, trackerNote)
	return nil
}
