package persistence

// Unit-level bounds on QueryTorrents argument validation. The checks run
// before any pool access, so they are pinned without a database.

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/skobkin/magnetico-web/codec"
)

// TestQueryTorrentsRejectsMaxTuplesOverInt32 pins the int32 bind-parameter
// bound: a MaxTuples above math.MaxInt32 is rejected as invalid input, not
// silently truncated. The store's pool is nil — validation must precede any
// database access.
func TestQueryTorrentsRejectsMaxTuplesOverInt32(t *testing.T) {
	t.Parallel()

	store := newPostgresStore(nil, codec.DefaultProfile(), codec.DefaultLimits())
	_, err := store.QueryTorrents(context.Background(), TorrentQuery{MaxTuples: math.MaxInt32 + 1})
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("MaxTuples > MaxInt32 error = %v, want ErrInvalidMetadata", err)
	}
}
