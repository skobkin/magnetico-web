// Package advisory is the single registry of PostgreSQL advisory-lock keys used
// by Magnetico. All keys share one 64-bit key space per database, so two
// commands that pick the same number silently exclude each other; declaring
// them side by side is what makes a collision visible. Task 09A found one:
// `migrate web-schema-init` and the ingest consumer's session lock shared a
// key, so initialising the web schema blocked for as long as ingest ran.
//
// Rules, enforced by internal/repocheck:
//   - a key is declared here and nowhere else; no two constants share a value;
//   - code that takes an advisory lock imports this package;
//   - a key shared on purpose is one constant used from several places
//     (TrackersWrite), never two constants with equal values.
//
// Every key is a positive int64: the consumer proves lock ownership from
// pg_locks, which reports the key as two 32-bit halves.
//
// Keys are unreleased internal identifiers, so changing one needs no migration;
// a lock is held only by a running process.
package advisory

// Keys of the "magneti" family (a base plus a small offset). Transaction-scoped
// unless the comment says session.
const (
	// TorrentSchemaInit serialises persistence.InitSchema (transaction).
	TorrentSchemaInit int64 = 7881644658832750185
	// IngestSchemaInit serialises `migrate ingest-schema-init` (transaction).
	IngestSchemaInit int64 = 7881644658832750186
	// IngestConsumer is the ingest consumer's session lock, held for as long as
	// the consumer owns the queue; its holder is the elected statistics sampler.
	IngestConsumer int64 = 7881644658832750187
	// TrackersWrite serialises every write to magnetico_web.trackers: the web
	// schema seed, the operator CLI and the development seed (transaction).
	// Shared on purpose.
	TrackersWrite int64 = 7881644658832750188
	// UsersMigration serialises concurrent `migrate users` runs (transaction).
	UsersMigration int64 = 7881644658832750189
	// DevSeed serialises concurrent `dev seed-torrents` runs (session).
	DevSeed int64 = 7881644658832750190
	// StatsSchemaInit serialises `migrate stats-schema-init` (transaction).
	StatsSchemaInit int64 = 7881644658832750191
	// WebSchemaInit serialises `migrate web-schema-init` (transaction).
	WebSchemaInit int64 = 7881644658832750192
)

// MigrationRun is the torrent migrator's run lock, held on a dedicated
// connection for the whole run (session). It belongs to a different family
// ('migrat08') so it cannot meet the keys above by accident.
const MigrationRun int64 = 4048864026886105453
