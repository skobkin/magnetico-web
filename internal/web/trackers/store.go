// Package trackers serves the public tracker list used in magnet links: a
// read-only reader of magnetico_web.trackers and an in-memory snapshot cache
// (docs/specs/web-v1.md §8.3).
package trackers

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Source returns the ordered tracker URLs.
type Source interface {
	URLs(ctx context.Context) ([]string, error)
}

// Reader reads the tracker table. It needs only SELECT on it.
type Reader struct{ pool *pgxpool.Pool }

// NewReader returns a Reader over pool.
func NewReader(pool *pgxpool.Pool) *Reader { return &Reader{pool: pool} }

// URLs returns the tracker URLs ordered by position, then id, as stored: no
// filtering and no repair.
func (r *Reader) URLs(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT url FROM magnetico_web.trackers ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
