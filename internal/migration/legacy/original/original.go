package original

// Source adapter for the original boramalper/magnetico v0.12.0 PostgreSQL
// layout (frozen upstream DDL): schema `magneticod`, ordinary unpartitioned
// `torrents`/`files`/`migrations` tables, `migrations.schema_version` = 0.
//
// The probe inspects the actual catalog (never trusts table sketches or
// index names alone), refuses everything that is not exactly that layout,
// and reports the detected state without secrets.

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
)

// expectedRelations is the exact relation inventory of the v0.12.0 layout:
// three ordinary (relkind 'r') tables and nothing else. Views, materialized
// views, foreign tables and partitioned tables are refused.
var expectedRelations = map[string]string{
	"torrents":   "r",
	"files":      "r",
	"migrations": "r",
}

// expectedColumns is the exact column inventory per table: ordinal order,
// data type and nullability must match the frozen DDL.
var expectedColumns = map[string][]struct {
	name     string
	dataType string
	nullable bool
}{
	"torrents": {
		{"id", "integer", false},
		{"info_hash", "bytea", false},
		{"name", "text", false},
		{"total_size", "bigint", false},
		{"discovered_on", "integer", false},
	},
	"files": {
		{"id", "integer", false},
		{"torrent_id", "integer", true}, // nullable in the original DDL
		{"size", "bigint", false},
		{"path", "text", false},
	},
	"migrations": {
		{"schema_version", "smallint", false},
	},
}

// expectedIndexes are the non-constraint indexes of the frozen DDL. The
// files lookup index additionally gets a by-definition verification (first
// key column torrent_id), so a name-only accident on another column is
// refused.
var expectedIndexes = []string{
	"idx_torrents_total_size",
	"idx_torrents_discovered_on",
	"idx_torrents_name_gin_trgm",
	"idx_files_torrent_id",
}

const filesLookupIndex = "idx_files_torrent_id"

// Source is an opened, read-only connection to a verified original v0.12.0
// database. All adapter connections run with default_transaction_read_only=on
// so even an adapter bug cannot write to the source; the probe never issues
// anything but SELECTs.
type Source struct {
	pool *pgxpool.Pool
}

// Open connects to the source, verifies the exact v0.12.0 layout and returns
// a read-only source handle. Schema mismatches return typed errors
// (ErrSourceNotMagnetico, ErrSourceV1Fork, ErrSourceUnsupported) wrapped in
// *SourceStateError where details exist; connectivity errors are returned
// unwrapped.
func Open(ctx context.Context, dsn string) (*Source, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("source DSN: %w", err)
	}
	cfg.MaxConns = 2
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// Session-local read-only enforcement: the adapter's own
		// connections cannot write, regardless of the account's grants.
		_, err := conn.Exec(ctx, "SET default_transaction_read_only = on")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("source connect: %w", err)
	}
	if err := probe(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Source{pool: pool}, nil
}

// Close releases the source connections. The source itself is never altered.
func (s *Source) Close() { s.pool.Close() }

// Identity exposes the source's server identity for source-vs-target
// comparison (transport-independent decision in pgidentity).
func (s *Source) Identity(ctx context.Context) (pgidentity.Identity, error) {
	return pgidentity.Read(ctx, s.pool)
}

// probe verifies the exact v0.12.0 catalog state. It is strictly SELECT-only
// so a refusal leaves the source untouched.
// queryer is the subset of pgxpool.Pool / pgx.Conn the probe uses.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// probe verifies the exact v0.12.0 catalog state. It is strictly SELECT-only
// so a refusal leaves the source untouched.
func probe(ctx context.Context, q queryer) error {
	// 1. Schema and exact relation inventory.
	var schemaExists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'magneticod')`).Scan(&schemaExists); err != nil {
		return err
	}
	if !schemaExists {
		return ErrSourceNotMagnetico
	}

	type relation struct{ name, kind string }
	var relations []relation
	rows, err := q.Query(ctx, `SELECT c.relname, c.relkind
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magneticod' AND c.relkind IN ('r','p','v','m','f')
        ORDER BY 1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r relation
		if err := rows.Scan(&r.name, &r.kind); err != nil {
			rows.Close()
			return err
		}
		relations = append(relations, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	filesPartitioned := false
	var details []string
	seen := map[string]string{}
	for _, r := range relations {
		seen[r.name] = r.kind
		if want, ok := expectedRelations[r.name]; !ok || want != r.kind {
			details = append(details, describeRelation(r.name, r.kind))
		}
		if r.name == "files" && r.kind == "p" {
			filesPartitioned = true
		}
	}
	for name := range expectedRelations {
		if _, ok := seen[name]; !ok {
			details = append(details, "missing table magneticod."+name)
		}
	}
	if !seenAny(seen) {
		return ErrSourceNotMagnetico
	}
	// The v1 fork is identified by its partitioned files parent; report it
	// as its own refusal category before any other mismatch.
	if filesPartitioned {
		return &SourceStateError{Err: ErrSourceV1Fork,
			Details: []string{"magneticod.files is a partitioned table"}}
	}

	// 2. Exact column inventory.
	if colDetails := probeColumns(ctx, q); colDetails != nil {
		if colErr := colDetails.err; colErr != nil {
			return colErr
		}
		details = append(details, colDetails.items...)
	}

	// 3. migrations rows: exactly {0}. A 1 anywhere means the fork's
	// upgraded database.
	version, versionErr := migrationsVersions(ctx, q)
	if versionErr != nil {
		return versionErr
	}
	if containsInt16(version, 1) {
		return &SourceStateError{Err: ErrSourceV1Fork,
			Details: []string{"magneticod.migrations rows: " + formatVersions(version)}}
	}
	if len(version) != 1 || version[0] != 0 {
		details = append(details, "magneticod.migrations rows: "+formatVersions(version)+
			" (want exactly 0)")
	}

	// 4. Mandatory constraints (by definition, not by name).
	conDetails, conErr := probeConstraints(ctx, q)
	if conErr != nil {
		return conErr
	}
	details = append(details, conDetails...)

	// 5. Non-constraint indexes: exact expected set, all valid, plus the
	// by-definition files lookup index check.
	idxDetails, idxErr := probeIndexes(ctx, q)
	if idxErr != nil {
		return idxErr
	}
	details = append(details, idxDetails...)

	if len(details) > 0 {
		return &SourceStateError{Err: ErrSourceUnsupported, Details: details}
	}
	return nil
}

// columnProblems carries refusal details or a query error.
type columnProblems struct {
	items []string
	err   error
}

func probeColumns(ctx context.Context, q queryer) *columnProblems {
	type column struct {
		table, name, dataType, nullable string
	}
	var cols []column
	rows, err := q.Query(ctx, `SELECT table_name, column_name, data_type, is_nullable
        FROM information_schema.columns
        WHERE table_schema = 'magneticod'
        ORDER BY table_name, ordinal_position`)
	if err != nil {
		return &columnProblems{err: err}
	}
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.name, &c.dataType, &c.nullable); err != nil {
			rows.Close()
			return &columnProblems{err: err}
		}
		cols = append(cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return &columnProblems{err: err}
	}

	byTable := map[string][]column{}
	for _, c := range cols {
		byTable[c.table] = append(byTable[c.table], c)
	}
	var items []string
	for table, want := range expectedColumns {
		got, ok := byTable[table]
		if !ok {
			continue // reported as a missing table already
		}
		if len(got) != len(want) {
			items = append(items, fmt.Sprintf("magneticod.%s has %d columns, want %d",
				table, len(got), len(want)))
			continue
		}
		for i, w := range want {
			if got[i].name != w.name || got[i].dataType != w.dataType ||
				(got[i].nullable == "YES") != w.nullable {
				items = append(items, fmt.Sprintf(
					"magneticod.%s column %d is %s %s nullable=%s, want %s %s nullable=%t",
					table, i+1, got[i].name, got[i].dataType, got[i].nullable,
					w.name, w.dataType, w.nullable))
			}
		}
	}
	for _, c := range cols {
		if _, ok := expectedRelations[c.table]; !ok {
			items = append(items, fmt.Sprintf("unexpected column magneticod.%s.%s",
				c.table, c.name))
		}
	}
	return &columnProblems{items: items}
}

func probeConstraints(ctx context.Context, q queryer) ([]string, error) {
	// Primary keys on the two tables, exactly one each.
	var details []string
	rows, err := q.Query(ctx, `SELECT c.relname FROM pg_constraint con
        JOIN pg_class c ON c.oid = con.conrelid
        JOIN pg_namespace n ON n.oid = con.connamespace
        WHERE n.nspname = 'magneticod' AND con.contype = 'p'
        ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	var pks []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		pks = append(pks, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pkSeen := map[string]bool{}
	for _, t := range pks {
		pkSeen[t] = true
	}
	if !pkSeen["torrents"] {
		details = append(details, "magneticod.torrents primary key missing")
	}
	if !pkSeen["files"] {
		details = append(details, "magneticod.files primary key missing")
	}

	// UNIQUE constraint covering info_hash on torrents.
	var uniqueInfoHash bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (
        SELECT 1 FROM pg_constraint con
        WHERE con.conrelid = 'magneticod.torrents'::regclass AND con.contype = 'u'
          AND con.conkey @> ARRAY[(
              SELECT attnum FROM pg_attribute
              WHERE attrelid = 'magneticod.torrents'::regclass AND attname = 'info_hash')])`,
	).Scan(&uniqueInfoHash); err != nil {
		return nil, err
	}
	if !uniqueInfoHash {
		details = append(details, "magneticod.torrents unique constraint on info_hash missing")
	}

	// FK files.torrent_id -> magneticod.torrents(id). Presence only: a NOT
	// VALID constraint is accepted (data quality is the orphan policy's
	// job), a missing one is refused.
	var fk bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (
        SELECT 1 FROM pg_constraint con
        WHERE con.conrelid = 'magneticod.files'::regclass AND con.contype = 'f'
          AND con.confrelid = 'magneticod.torrents'::regclass
          AND con.conkey @> ARRAY[(
              SELECT attnum FROM pg_attribute
              WHERE attrelid = 'magneticod.files'::regclass AND attname = 'torrent_id')]
          AND con.confkey @> ARRAY[(
              SELECT attnum FROM pg_attribute
              WHERE attrelid = 'magneticod.torrents'::regclass AND attname = 'id')])`,
	).Scan(&fk); err != nil {
		return nil, err
	}
	if !fk {
		details = append(details, "magneticod.files foreign key to torrents(id) missing")
	}
	return details, nil
}

func probeIndexes(ctx context.Context, q queryer) ([]string, error) {
	type index struct {
		name, table string
		valid       bool
		firstKey    string
	}
	var indexes []index
	rows, err := q.Query(ctx, `SELECT i.relname, t.relname, ix.indisvalid,
            pg_get_indexdef(ix.indexrelid, 1, true)
        FROM pg_index ix
        JOIN pg_class i ON i.oid = ix.indexrelid
        JOIN pg_class t ON t.oid = ix.indrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = 'magneticod'
          AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.oid)
        ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var idx index
		if err := rows.Scan(&idx.name, &idx.table, &idx.valid, &idx.firstKey); err != nil {
			rows.Close()
			return nil, err
		}
		indexes = append(indexes, idx)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var details []string
	seen := map[string]index{}
	for _, idx := range indexes {
		seen[idx.name] = idx
		expected := false
		for _, want := range expectedIndexes {
			if want == idx.name {
				expected = true
				break
			}
		}
		if !expected {
			details = append(details, fmt.Sprintf("unexpected index magneticod.%s on %s",
				idx.name, idx.table))
			continue
		}
		if !idx.valid {
			details = append(details, fmt.Sprintf("index magneticod.%s is not valid", idx.name))
		}
	}
	for _, want := range expectedIndexes {
		if _, ok := seen[want]; !ok {
			details = append(details, "missing index magneticod."+want)
		}
	}
	// The files lookup index must actually key on torrent_id — the reader
	// depends on it for bounded set-oriented reads.
	if lookup, ok := seen[filesLookupIndex]; ok && (lookup.table != "files" || lookup.firstKey != "torrent_id") {
		details = append(details, fmt.Sprintf(
			"index magneticod.%s does not key files(torrent_id) (on %s, first key %q)",
			filesLookupIndex, lookup.table, lookup.firstKey))
	}
	return details, nil
}

func migrationsVersions(ctx context.Context, q queryer) ([]int16, error) {
	rows, err := q.Query(ctx, `SELECT schema_version FROM magneticod.migrations ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var versions []int16
	for rows.Next() {
		var v int16
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func describeRelation(name, kind string) string {
	switch kind {
	case "p":
		return fmt.Sprintf("magneticod.%s is a partitioned table", name)
	case "v":
		return fmt.Sprintf("magneticod.%s is a view", name)
	case "m":
		return fmt.Sprintf("magneticod.%s is a materialized view", name)
	case "f":
		return fmt.Sprintf("magneticod.%s is a foreign table", name)
	default:
		return fmt.Sprintf("unexpected relation magneticod.%s", name)
	}
}

func seenAny(seen map[string]string) bool {
	for name := range seen {
		if _, ok := expectedRelations[name]; ok {
			return true
		}
	}
	return false
}

func containsInt16(values []int16, want int16) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func formatVersions(values []int16) string {
	if len(values) == 0 {
		return "(empty)"
	}
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return strings.Join(parts, ", ")
}
