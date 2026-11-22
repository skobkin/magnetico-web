package webschema

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/persistence"
)

// Requirement selects the privilege set a caller needs.
type Requirement int

const (
	// Runtime is the web service (spec §2.7): read the torrent schema, DML on
	// the web schema, no DDL.
	Runtime Requirement = iota
	// Operator is the administrator CLI: Runtime plus tracker writes.
	Operator
)

type tablePriv struct {
	table string
	privs []string
}

// torrentPrivileges are read by persistence.VerifySchema and the later search
// and details code; schema_version is read at every startup.
var torrentPrivileges = []tablePriv{
	{"magneticod.torrents", []string{"SELECT"}},
	{"magneticod.torrent_file_blobs", []string{"SELECT"}},
	{"magneticod.schema_version", []string{"SELECT"}},
}

var webPrivileges = []tablePriv{
	{"magnetico_web.web_version", []string{"SELECT"}},
	{"magnetico_web.settings", []string{"SELECT"}},
	{"magnetico_web.trackers", []string{"SELECT"}},
	{"magnetico_web.users", []string{"SELECT", "INSERT", "UPDATE"}},
	{"magnetico_web.api_tokens", []string{"SELECT", "INSERT", "UPDATE"}},
	{"magnetico_web.rss_tokens", []string{"SELECT", "INSERT", "UPDATE"}},
	{"magnetico_web.invites", []string{"SELECT", "INSERT", "UPDATE"}},
	{"magnetico_web.sessions", []string{"SELECT", "INSERT", "UPDATE", "DELETE"}},
	{"magnetico_web.one_time_tokens", []string{"SELECT", "INSERT", "UPDATE", "DELETE"}},
}

var operatorExtra = []tablePriv{
	{"magnetico_web.trackers", []string{"INSERT", "UPDATE", "DELETE"}},
}

// missingPrivileges returns "schema.table: PRIV" for every missing privilege
// of an existing relation (an absent relation is skipped, never an error).
func missingPrivileges(ctx context.Context, q querier, schema string, schemaUsage bool, list []tablePriv) ([]string, error) {
	var missing []string
	if schemaUsage {
		var exists, ok bool
		if err := q.QueryRow(ctx, `SELECT to_regnamespace($1) IS NOT NULL,
            COALESCE(has_schema_privilege(current_user, to_regnamespace($1)::oid, 'USAGE'), false)`, schema).Scan(&exists, &ok); err != nil {
			return nil, err
		}
		if exists && !ok {
			// Without USAGE even to_regclass on the schema's tables raises, so
			// stop here: the schema privilege is the actionable finding.
			return []string{schema + ": USAGE"}, nil
		}
	}
	for _, tp := range list {
		var exists bool
		if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, tp.table).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		for _, p := range tp.privs {
			var ok bool
			if err := q.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, tp.table, p).Scan(&ok); err != nil {
				return nil, err
			}
			if !ok {
				missing = append(missing, tp.table+": "+p)
			}
		}
	}
	return missing, nil
}

func readOnly(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	return fn(tx)
}

// VerifyTorrentPrivileges reports, as ErrWebPrivileges with the missing
// privilege names, a role that cannot read what persistence.VerifySchema and
// the web service read. It runs BEFORE persistence.VerifySchema so that a
// missing grant produces a controlled message instead of a raw driver error.
// An absent torrent schema is not this function's concern (nil).
func VerifyTorrentPrivileges(ctx context.Context, pool *pgxpool.Pool) error {
	return readOnly(ctx, pool, func(tx pgx.Tx) error {
		missing, err := missingPrivileges(ctx, tx, "magneticod", true, torrentPrivileges)
		if err != nil {
			return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
		}
		if len(missing) > 0 {
			return &StateError{Err: ErrWebPrivileges, Details: missing}
		}
		return nil
	})
}

// Verify is the read-only startup verification of the web schema shared by the
// web service and the operator CLI: the exact version-1 structure (the same
// check Init applies) and then the privileges of req. It never modifies the
// database.
//
//   - ErrWebSchemaMissing: the schema or web_version table is absent;
//   - ErrWebSchemaUnsupported (a *StateError): the structure differs;
//   - ErrWebPrivileges (a *StateError): a required privilege is missing.
func Verify(ctx context.Context, pool *pgxpool.Pool, req Requirement) error {
	return readOnly(ctx, pool, func(tx pgx.Tx) error {
		// Privileges first: inspect reads web_version, and an absent object
		// is skipped here rather than raising.
		list := webPrivileges
		if req == Operator {
			list = append(append([]tablePriv{}, webPrivileges...), operatorExtra...)
		}
		missing, err := missingPrivileges(ctx, tx, "magnetico_web", true, list)
		if err != nil {
			return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
		}
		if len(missing) > 0 {
			return &StateError{Err: ErrWebPrivileges, Details: missing}
		}
		st, details, err := inspect(ctx, tx)
		if err != nil {
			return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
		}
		switch st {
		case stateAbsent, stateNoVersion:
			return ErrWebSchemaMissing
		case stateBad:
			return &StateError{Err: ErrWebSchemaUnsupported, Details: details}
		}
		return nil
	})
}
