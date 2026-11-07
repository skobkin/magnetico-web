package target

// Phase-aware target schema verification.
//
// The strict production state (everything `migrate schema-init` creates,
// including the three secondary indexes) delegates to persistence's own
// verification — the migrator must never accept less than the application
// would. Only an authorized deferred-index run is verified by the custom
// inventory here, which tolerates exactly the three dropped indexes:
//   - importing / data_loaded: all three absent (the drop is atomic with its
//     state marker, so partial absence is impossible and corrupt)
//   - building_indexes: per index absent (crash recovery) or present and
//     definition-identical
// Any other deviation is refused with the offending catalog objects named.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/persistence"
)

// verifyInput describes the tolerance the run state authorizes.
type verifyInput struct {
	deferRequested  bool
	indexesDeferred bool
	phase           state.Phase
}

// secondaryIndexDefinitions maps the three droppable indexes to the
// pg_get_indexdef fragment that identifies them by definition.
var secondaryIndexDefinitions = map[string]string{
	"idx_torrents_total_size":    "USING btree (total_size)",
	"idx_torrents_discovered_on": "USING btree (discovered_on)",
	"idx_torrents_name_gin_trgm": "USING gin (name gin_trgm_ops)",
}

// VerifySchema verifies the target schema for the run's phase. Strict cases
// delegate to persistence.MakeDatabase (production verification).
func VerifySchema(ctx context.Context, dsn string, in verifyInput) error {
	if !in.deferRequested || in.phase.Complete() {
		// No deferral, or a completed run: the application's strict state.
		store, err := persistence.MakeDatabase(ctx, dsn)
		if err != nil {
			return err
		}
		_ = store.Close()
		return nil
	}

	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return verifyDeferredState(ctx, pool, in)
}

// verifyDeferredState is the authorized index-less inventory: everything the
// strict verification demands, minus the three secondary indexes.
func verifyDeferredState(ctx context.Context, pool *pgxpool.Pool, in verifyInput) error {
	var details []string

	// Schema version: exactly one row carrying the production version.
	var versionRows []int64
	rows, err := pool.Query(ctx, `SELECT version FROM magneticod.schema_version ORDER BY 1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		versionRows = append(versionRows, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(versionRows) != 1 || versionRows[0] != persistence.SchemaVersion {
		return &persistence.SchemaStateError{Err: persistence.ErrUnsupportedSchema,
			Details: []string{fmt.Sprintf("magneticod.schema_version rows %v, want [%d]",
				versionRows, persistence.SchemaVersion)}}
	}

	// Exact table inventory: no unexpected relations in magneticod.
	rows, err = pool.Query(ctx, `SELECT c.relname, c.relkind
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magneticod' AND c.relkind IN ('r','p','v','m','f')
        ORDER BY 1`)
	if err != nil {
		return err
	}
	relkind := map[string]string{}
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			rows.Close()
			return err
		}
		relkind[name] = kind
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for name := range map[string]bool{"torrents": true, "torrent_file_blobs": true, "schema_version": true} {
		if relkind[name] != "r" && relkind[name] != "p" {
			details = append(details, "missing table magneticod."+name)
		}
	}
	for name, kind := range relkind {
		if (kind == "r" || kind == "p") &&
			name != "torrents" && name != "torrent_file_blobs" && name != "schema_version" {
			details = append(details, "unexpected table magneticod."+name)
		}
	}

	// Integrity constraints and the blob table must be intact throughout.
	intact, err := constraintsIntact(ctx, pool)
	if err != nil {
		return err
	}
	details = append(details, intact...)

	// pg_trgm stays installed (its GIN index may legitimately be absent
	// only while deferred).
	var trgm bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm')`).Scan(&trgm); err != nil {
		return err
	}
	if !trgm {
		details = append(details, "pg_trgm extension missing")
	}

	// Secondary index state per phase.
	idxDetails, err := verifySecondaryIndexes(ctx, pool, in.phase)
	if err != nil {
		return err
	}
	details = append(details, idxDetails...)

	if len(details) > 0 {
		return &persistence.SchemaStateError{Err: persistence.ErrUnsupportedSchema, Details: details}
	}
	return nil
}

func constraintsIntact(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	var details []string
	var pkTorrents, pkBlobs, fkBlobs, uniqueInfoHash bool
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT c.relname, con.contype
        FROM pg_constraint con
        JOIN pg_class c ON c.oid = con.conrelid
        JOIN pg_namespace n ON n.oid = con.connamespace
        WHERE n.nspname = 'magneticod'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var table, contype string
			if err := rows.Scan(&table, &contype); err != nil {
				return err
			}
			switch {
			case table == "torrents" && contype == "p":
				pkTorrents = true
			case table == "torrent_file_blobs" && contype == "p":
				pkBlobs = true
			case table == "torrent_file_blobs" && contype == "f":
				fkBlobs = true
			case table == "torrents" && contype == "u":
				uniqueInfoHash = true
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if !pkTorrents {
		details = append(details, "magneticod.torrents primary key missing")
	}
	if !pkBlobs {
		details = append(details, "magneticod.torrent_file_blobs primary key missing")
	}
	if !fkBlobs {
		details = append(details, "magneticod.torrent_file_blobs foreign key missing")
	}
	if !uniqueInfoHash {
		details = append(details, "magneticod.torrents unique constraint on info_hash missing")
	}
	return details, nil
}

func verifySecondaryIndexes(ctx context.Context, pool *pgxpool.Pool, phase state.Phase) ([]string, error) {
	type index struct {
		name  string
		valid bool
		def   string
	}
	rows, err := pool.Query(ctx, `SELECT i.relname, ix.indisvalid, pg_get_indexdef(ix.indexrelid)
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
	var found []index
	for rows.Next() {
		var idx index
		if err := rows.Scan(&idx.name, &idx.valid, &idx.def); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, idx)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	byName := map[string]index{}
	var unexpected []string
	for _, idx := range found {
		if _, ok := secondaryIndexDefinitions[idx.name]; !ok {
			unexpected = append(unexpected, "unexpected index magneticod."+idx.name)
			continue
		}
		byName[idx.name] = idx
	}

	var details []string
	details = append(details, unexpected...)

	switch phase {
	case state.PhaseImporting, state.PhaseDataLoaded:
		// The drop is atomic with its marker: all three must be absent.
		for name := range secondaryIndexDefinitions {
			if _, present := byName[name]; present {
				details = append(details, fmt.Sprintf(
					"index magneticod.%s exists though the deferred run has not reached index building", name))
			}
		}
	case state.PhaseBuildingIndexes:
		// Partial progress is valid; present indexes must be valid and
		// definition-identical.
		for name, wantDef := range secondaryIndexDefinitions {
			idx, present := byName[name]
			if !present {
				continue
			}
			if !idx.valid {
				details = append(details, fmt.Sprintf("index magneticod.%s is not valid", name))
			}
			if !strings.Contains(idx.def, wantDef) {
				details = append(details, fmt.Sprintf(
					"index magneticod.%s does not match the expected definition (%s)", name, wantDef))
			}
		}
	default:
		return nil, errors.New("deferred-run verification for phase " + string(phase) + " is undefined")
	}
	return details, nil
}
