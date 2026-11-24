package devseed

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/persistence"
)

// Refusals. All are fixed messages: they never carry a database URL, a driver
// error or any value read from the database.
var (
	// ErrForeignTorrents: the database holds torrents that are not part of
	// the fixture dataset. Nothing is written.
	ErrForeignTorrents = errors.New("the database already contains torrents that are not part of the development fixture; " +
		"refusing to seed (this command only ever adds fixture torrents to an empty or fixture-only database)")
	// ErrFixtureConflict: a fixture infohash is present but its stored
	// metadata differs from the fixture (a different dataset version, or a
	// real torrent that collides with a fixture hash). Nothing is written.
	ErrFixtureConflict = errors.New("a fixture infohash is already stored with different metadata; refusing to seed")
	// ErrBusy: another seeder holds the advisory lock.
	ErrBusy = errors.New("another seed-torrents run is in progress on this database")
)

// advisoryKey serializes concurrent seed runs; a neighbour of the other
// project-specific advisory keys.
const advisoryKey int64 = 7881644658832750190

// State describes what the database looked like before the run.
type State string

const (
	StateEmpty   State = "empty"   // no torrents at all
	StatePartial State = "partial" // only fixture torrents, some missing (an interrupted run)
	StateSeeded  State = "seeded"  // exactly the complete fixture dataset
)

// Result is the outcome of a run. It carries counts and a state name only.
type Result struct {
	DryRun   bool
	Before   State
	Torrents int   // dataset size
	Files    int   // total files in the dataset
	Created  int   // torrents inserted by this run (0 for a dry run)
	Existing int   // fixture torrents that were already stored
	Bytes    int64 // sum of the dataset's total sizes
}

// Seed adds the development dataset to the PostgreSQL database at rawURL
// through the production persistence API (persistence.Store.AddTorrent: the
// real validation, codec and transaction logic).
//
// Safety contract:
//
//   - The schema is verified read-only first (persistence.MakeDatabase): a
//     missing, legacy or unsupported schema is refused and never created or
//     altered here, and nothing is ever updated, deleted or truncated.
//   - The target must be empty or contain only fixture torrents. Any other
//     torrent (anything beyond the fixture infohashes) refuses the run with
//     ErrForeignTorrents before a single row is written.
//   - Fixture torrents that already exist must match the dataset exactly
//     (name, size, discovery time, file count and the complete (path, size)
//     multiset of the file list), otherwise ErrFixtureConflict; an unreadable
//     or corrupt file list fails the run.
//   - Re-running is therefore safe and defined: on a completed dataset it is
//     a no-op reporting StateSeeded; after an interruption it adds only the
//     missing torrents (AddTorrent is idempotent per infohash).
//   - dryRun performs every check and writes nothing.
//
// The torrent-count check and the inserts are not one transaction. The
// advisory lock stops two seeders racing each other, but a concurrent writer
// (a running ingest) can still add torrents; use a database nothing else
// writes to. No DHT, BitTorrent, tracker or other unrelated network activity is performed;
// the only connections are to the PostgreSQL server named by rawURL.
func Seed(ctx context.Context, rawURL string, dryRun bool) (Result, error) {
	return seed(ctx, rawURL, Dataset(), dryRun)
}

func seed(ctx context.Context, rawURL string, data []Fixture, dryRun bool) (Result, error) {
	store, err := persistence.MakeDatabase(ctx, rawURL)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = store.Close() }()

	unlock, err := lock(ctx, rawURL)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	res := Result{DryRun: dryRun, Torrents: len(data)}
	hashes := make([][]byte, len(data))
	for i, f := range data {
		hashes[i] = f.Torrent.InfoHash
		res.Files += len(f.Files)
		res.Bytes += f.Torrent.TotalSize
	}

	present, err := store.ExistingInfoHashes(ctx, hashes)
	if err != nil {
		return Result{}, err
	}
	stats, err := store.GetStatistics(ctx)
	if err != nil {
		return Result{}, err
	}
	if stats.Torrents != uint64(len(present)) {
		return Result{}, ErrForeignTorrents
	}

	for _, f := range data {
		var key [20]byte
		copy(key[:], f.Torrent.InfoHash)
		if _, ok := present[key]; !ok {
			continue
		}
		got, err := store.GetTorrent(ctx, f.Torrent.InfoHash)
		if err != nil {
			return Result{}, err
		}
		if got.Name != f.Torrent.Name || got.TotalSize != f.Torrent.TotalSize ||
			got.DiscoveredOn != f.Torrent.DiscoveredOn || got.FileCount != int32(len(f.Files)) {
			return Result{}, ErrFixtureConflict
		}
		// The summary matches; the stored file list must match too: the
		// complete (path, size) multiset, duplicates included. A corrupt or
		// missing payload is an error, never a successful seed.
		files, err := store.GetFiles(ctx, f.Torrent.InfoHash)
		if err != nil {
			return Result{}, fmt.Errorf("reading the stored file list of a fixture torrent (%s): %w", f.Key, err)
		}
		if !sameFiles(files, f.Files) {
			return Result{}, ErrFixtureConflict
		}
	}

	res.Existing = len(present)
	switch {
	case len(present) == 0:
		res.Before = StateEmpty
	case len(present) == len(data):
		res.Before = StateSeeded
	default:
		res.Before = StatePartial
	}
	if dryRun {
		return res, nil
	}

	for _, f := range data {
		created, _, err := store.AddTorrent(ctx, f.Torrent, f.Files)
		if err != nil {
			return res, fmt.Errorf("storing a fixture torrent (%s): %w", f.Key, err)
		}
		if created {
			res.Created++
		}
	}
	return res, nil
}

// sameFiles reports whether a and b hold the same (path, size) multiset.
// Order is irrelevant (the codec may reorder), multiplicity is not.
func sameFiles(a, b []persistence.File) bool {
	if len(a) != len(b) {
		return false
	}
	type pair struct {
		path string
		size int64
	}
	key := func(fs []persistence.File) []pair {
		out := make([]pair, len(fs))
		for i, f := range fs {
			out[i] = pair{f.Path, f.Size}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].path != out[j].path {
				return out[i].path < out[j].path
			}
			return out[i].size < out[j].size
		})
		return out
	}
	ka, kb := key(a), key(b)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

// lock takes a session-level advisory lock on a dedicated connection and
// returns its release function.
func lock(ctx context.Context, rawURL string) (func(), error) {
	conn, err := pgx.Connect(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("connecting for the seed lock: %w", persistence.ErrStorageIO)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, advisoryKey).Scan(&got); err != nil {
		_ = conn.Close(ctx)
		return nil, fmt.Errorf("taking the seed lock: %w", persistence.ErrStorageIO)
	}
	if !got {
		_ = conn.Close(ctx)
		return nil, ErrBusy
	}
	return func() {
		// Closing the session releases the lock; use a fresh context so a
		// cancelled run still releases it.
		_ = conn.Close(context.Background())
	}, nil
}
