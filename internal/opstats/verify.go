package opstats

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/persistence"
)

// ErrSchemaMissing: magnetico_stats is absent or has no supported version row.
var ErrSchemaMissing = errors.New(
	"the magnetico_stats schema is missing or has an unsupported version; run `magnetico migrate stats-schema-init`")

// ErrSchemaInvalid is matched by every *SchemaInvalidError: the schema has the
// supported version but is not structurally what this release expects.
var ErrSchemaInvalid = errors.New("the magnetico_stats schema is structurally incompatible")

// SchemaInvalidError lists what is wrong with an existing magnetico_stats. The
// schema is never repaired automatically.
type SchemaInvalidError struct{ Details []string }

func (e *SchemaInvalidError) Error() string {
	return "the magnetico_stats schema is structurally incompatible (" + strings.Join(e.Details, "; ") +
		"); it is never repaired automatically: statistics are disposable, drop the schema (`DROP SCHEMA magnetico_stats CASCADE;`) " +
		"and run `magnetico migrate stats-schema-init`"
}

// Is makes errors.Is(err, ErrSchemaInvalid) true.
func (e *SchemaInvalidError) Is(target error) bool { return target == ErrSchemaInvalid }

// DB is the read subset of *pgxpool.Pool and pgx.Tx that verification and the
// privilege check need.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Verify checks that magnetico_stats exists with version row exactly
// {SchemaVersion} and exactly the expected tables, columns, types,
// nullability, primary keys, no triggers and no foreign keys. It only reads.
// Errors match ErrSchemaMissing, ErrSchemaInvalid or persistence.ErrStorageIO.
func Verify(ctx context.Context, db DB) error {
	err := verify(ctx, db)
	if err == nil || errors.Is(err, ErrSchemaMissing) || errors.Is(err, ErrSchemaInvalid) {
		return err
	}
	return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
}

func queryAll(ctx context.Context, db DB, sql string, args []any, scan func(pgx.Rows) error) error {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func verify(ctx context.Context, db DB) error {
	var reg *string
	if err := db.QueryRow(ctx, `SELECT to_regclass('`+SchemaName+`.stats_version')::text`).Scan(&reg); err != nil {
		return err
	}
	if reg == nil {
		return ErrSchemaMissing
	}
	var versions []int32
	if err := queryAll(ctx, db, `SELECT version FROM `+SchemaName+`.stats_version`, nil, func(r pgx.Rows) error {
		var v int32
		if err := r.Scan(&v); err != nil {
			return err
		}
		versions = append(versions, v)
		return nil
	}); err != nil {
		return err
	}
	if len(versions) != 1 || versions[0] != SchemaVersion {
		return fmt.Errorf("%w (found versions %v)", ErrSchemaMissing, versions)
	}

	var details []string
	want := map[string]tableSpec{}
	for _, t := range tables {
		want[t.name] = t
	}

	// Relations of the schema: only the expected tables.
	have := map[string]bool{}
	if err := queryAll(ctx, db, `SELECT c.relname, c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','f','S')`, []any{SchemaName}, func(r pgx.Rows) error {
		var n, k string
		if err := r.Scan(&n, &k); err != nil {
			return err
		}
		if _, ok := want[n]; !ok || k != "r" {
			details = append(details, fmt.Sprintf("unexpected relation %s (kind %s)", n, k))
		}
		have[n] = true
		return nil
	}); err != nil {
		return err
	}
	for n := range want {
		if !have[n] {
			details = append(details, "missing table "+n)
		}
	}

	// Columns.
	type col struct {
		typ     string
		notNull bool
	}
	cols := map[string]map[string]col{}
	if err := queryAll(ctx, db, `SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull
FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped`, []any{SchemaName}, func(r pgx.Rows) error {
		var t, n, ty string
		var nn bool
		if err := r.Scan(&t, &n, &ty, &nn); err != nil {
			return err
		}
		if cols[t] == nil {
			cols[t] = map[string]col{}
		}
		cols[t][n] = col{ty, nn}
		return nil
	}); err != nil {
		return err
	}
	for _, t := range tables {
		got := cols[t.name]
		if got == nil {
			continue // already reported as missing
		}
		for _, c := range t.cols {
			g, ok := got[c.name]
			switch {
			case !ok:
				details = append(details, fmt.Sprintf("%s lacks column %s", t.name, c.name))
			case g.typ != c.typ:
				details = append(details, fmt.Sprintf("%s.%s has type %s, want %s", t.name, c.name, g.typ, c.typ))
			case g.notNull != c.notNull:
				details = append(details, fmt.Sprintf("%s.%s has the wrong nullability", t.name, c.name))
			}
		}
		for n := range got {
			if _, ok := t.col(n); !ok {
				details = append(details, fmt.Sprintf("%s has unexpected column %s", t.name, n))
			}
		}
	}

	// Primary keys, foreign keys and triggers.
	pks := map[string]string{}
	if err := queryAll(ctx, db, `SELECT c.relname, string_agg(a.attname, ',' ORDER BY u.ord)
FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL unnest(k.conkey) WITH ORDINALITY AS u(attnum, ord)
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = u.attnum
WHERE n.nspname = $1 AND k.contype = 'p' GROUP BY c.relname`, []any{SchemaName}, func(r pgx.Rows) error {
		var t, cols string
		if err := r.Scan(&t, &cols); err != nil {
			return err
		}
		pks[t] = cols
		return nil
	}); err != nil {
		return err
	}
	for _, t := range tables {
		if have[t.name] && pks[t.name] != strings.Join(t.pk, ",") {
			details = append(details, fmt.Sprintf("%s has primary key (%s), want (%s)", t.name, pks[t.name], strings.Join(t.pk, ",")))
		}
	}
	var fks, trigs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint k JOIN pg_namespace n ON n.oid = k.connamespace
WHERE n.nspname = $1 AND k.contype = 'f'`, SchemaName).Scan(&fks); err != nil {
		return err
	}
	if fks > 0 {
		details = append(details, "foreign keys are not allowed (the schema is independent of the others)")
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND NOT t.tgisinternal`, SchemaName).Scan(&trigs); err != nil {
		return err
	}
	if trigs > 0 {
		details = append(details, "triggers are not allowed")
	}

	if len(details) > 0 {
		sort.Strings(details)
		return &SchemaInvalidError{Details: details}
	}
	return nil
}
