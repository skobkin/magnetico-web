package webschema

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Sentinels. Their messages are fixed, actionable and free of data.
var (
	// ErrWebSchemaMissing: the magnetico_web schema or its web_version table
	// is absent.
	ErrWebSchemaMissing = errors.New(
		"the magnetico_web schema is missing or incomplete: initialize it with `magnetico migrate web-schema-init` before starting the web service")

	// ErrWebSchemaUnsupported: the schema exists but is not exactly the
	// supported version-1 structure. It is never repaired automatically.
	ErrWebSchemaUnsupported = errors.New(
		"the magnetico_web schema is not the supported version; refusing to use or modify it (no automatic repair exists)")

	// ErrWebPrivileges: the database role lacks a required privilege.
	ErrWebPrivileges = errors.New(
		"the database role lacks privileges required by the web service (see docs/web-deployment.md for the minimum GRANT statements)")
)

// StateError carries a refused state with the catalog objects that caused it
// (schema-qualified names and privilege names only, never data). It unwraps
// to ErrWebSchemaUnsupported or ErrWebPrivileges.
type StateError struct {
	Err     error
	Details []string
}

func (e *StateError) Error() string { return e.Err.Error() + ": " + strings.Join(e.Details, "; ") }
func (e *StateError) Unwrap() error { return e.Err }

// querier is the subset of pgx.Tx / pgx.Conn used here.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type state int

const (
	stateAbsent    state = iota // no magnetico_web schema
	stateNoVersion              // schema exists, web_version table does not
	stateBad                    // structure differs from version 1
	stateOK
)

// inspect is the targeted, read-only catalog check of the version-1 structure
// (not a generic schema-diff): relation inventory, columns, constraints,
// indexes, row-level security, unexpected triggers and the exact version row.
// The caller must have set search_path = pg_catalog in the transaction so the
// deparsed definitions are fully qualified and independent of the session.
func inspect(ctx context.Context, q querier) (state, []string, error) {
	var hasSchema, hasVersion bool
	if err := q.QueryRow(ctx, `SELECT to_regnamespace('magnetico_web') IS NOT NULL,
        to_regclass('magnetico_web.web_version') IS NOT NULL`).Scan(&hasSchema, &hasVersion); err != nil {
		return 0, nil, err
	}
	if !hasSchema {
		return stateAbsent, nil, nil
	}
	if !hasVersion {
		return stateNoVersion, []string{"magnetico_web.web_version absent"}, nil
	}

	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	// 1. Relation inventory.
	rows, err := q.Query(ctx, `SELECT c.relname, c.relkind::text, c.relrowsecurity, c.relpersistence::text
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magnetico_web' AND c.relkind NOT IN ('i','I','S') ORDER BY 1`)
	if err != nil {
		return 0, nil, err
	}
	found := map[string]bool{}
	for rows.Next() {
		var name, kind, persistence string
		var rls bool
		if err := rows.Scan(&name, &kind, &rls, &persistence); err != nil {
			rows.Close()
			return 0, nil, err
		}
		found[name] = true
		switch {
		case !isExpectedTable(name):
			add("unexpected relation magnetico_web.%s (kind %s)", name, kind)
		case kind != "r":
			add("magnetico_web.%s is not an ordinary table (kind %s)", name, kind)
		case rls:
			add("magnetico_web.%s has row-level security enabled", name)
		case persistence != "p":
			add("magnetico_web.%s is not a permanent table", name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	for _, t := range tableNames() {
		if !found[t] {
			add("missing table magnetico_web.%s", t)
		}
	}

	// 2. Columns: exact set, type, nullability, identity, generated, default.
	rows, err = q.Query(ctx, `SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod),
            a.attnotnull, a.attidentity::text, a.attgenerated::text, COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
        FROM pg_attribute a
        JOIN pg_class c ON c.oid = a.attrelid
        JOIN pg_namespace n ON n.oid = c.relnamespace
        LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
        WHERE n.nspname = 'magnetico_web' AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped
        ORDER BY 1, a.attnum`)
	if err != nil {
		return 0, nil, err
	}
	gotCols := map[string]colSpec{}
	for rows.Next() {
		var table string
		var c colSpec
		if err := rows.Scan(&table, &c.name, &c.typ, &c.notNull, &c.identity, &c.generated, &c.def); err != nil {
			rows.Close()
			return 0, nil, err
		}
		c.def = normalize(c.def)
		gotCols[table+"."+c.name] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	wantCols := map[string]colSpec{}
	for _, t := range expectedTables {
		for _, c := range t.cols {
			wantCols[t.name+"."+c.name] = c
		}
	}
	for key, want := range wantCols {
		got, ok := gotCols[key]
		switch {
		case !found[strings.SplitN(key, ".", 2)[0]]:
			// the missing table is already reported
		case !ok:
			add("missing column magnetico_web.%s", key)
		case got != want:
			add("column magnetico_web.%s differs (have %s)", key, got.describe())
		}
	}
	for key := range gotCols {
		if _, ok := wantCols[key]; !ok && isExpectedTable(strings.SplitN(key, ".", 2)[0]) {
			add("unexpected column magnetico_web.%s", key)
		}
	}

	// 3. Constraints (NOT NULL constraints of newer servers are covered by
	// the attnotnull check above and excluded here).
	rows, err = q.Query(ctx, `SELECT c.relname, k.conname, k.convalidated, k.condeferrable, pg_get_constraintdef(k.oid)
        FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magnetico_web' AND k.contype IN ('p','u','f','c','x','t') ORDER BY 1, 2`)
	if err != nil {
		return 0, nil, err
	}
	gotCons := map[string]string{}
	for rows.Next() {
		var table, name, def string
		var validated, deferrable bool
		if err := rows.Scan(&table, &name, &validated, &deferrable, &def); err != nil {
			rows.Close()
			return 0, nil, err
		}
		key := table + "." + name
		switch {
		case !validated:
			add("constraint magnetico_web.%s is NOT VALID", key)
		case deferrable:
			add("constraint magnetico_web.%s is deferrable", key)
		}
		gotCons[key] = normalize(def)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	for key, want := range expectedConstraints {
		got, ok := gotCons[key]
		switch {
		case !found[strings.SplitN(key, ".", 2)[0]]:
		case !ok:
			add("missing constraint magnetico_web.%s", key)
		case got != want:
			add("constraint magnetico_web.%s differs (have %s)", key, got)
		}
	}
	for key := range gotCons {
		if _, ok := expectedConstraints[key]; !ok {
			add("unexpected constraint magnetico_web.%s", key)
		}
	}

	// 4. Indexes (including those backing constraints), by name.
	rows, err = q.Query(ctx, `SELECT t.relname, i.relname, x.indisvalid, x.indisready, pg_get_indexdef(i.oid)
        FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class t ON t.oid = x.indrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = 'magnetico_web' ORDER BY 1, 2`)
	if err != nil {
		return 0, nil, err
	}
	gotIdx := map[string]string{}
	for rows.Next() {
		var table, name, def string
		var valid, ready bool
		if err := rows.Scan(&table, &name, &valid, &ready, &def); err != nil {
			rows.Close()
			return 0, nil, err
		}
		key := table + "." + name
		if !valid || !ready {
			add("index magnetico_web.%s is not valid", key)
		}
		gotIdx[key] = normalize(def)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	for key, want := range expectedIndexes {
		got, ok := gotIdx[key]
		switch {
		case !found[strings.SplitN(key, ".", 2)[0]]:
		case !ok:
			add("missing index magnetico_web.%s", key)
		case got != want:
			add("index magnetico_web.%s differs (have %s)", key, got)
		}
	}
	for key := range gotIdx {
		if _, ok := expectedIndexes[key]; !ok {
			add("unexpected index magnetico_web.%s", key)
		}
	}

	// 5. Triggers: the design has none; a foreign one could change writes.
	rows, err = q.Query(ctx, `SELECT c.relname, t.tgname
        FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magnetico_web' AND NOT t.tgisinternal ORDER BY 1, 2`)
	if err != nil {
		return 0, nil, err
	}
	for rows.Next() {
		var table, name string
		if err := rows.Scan(&table, &name); err != nil {
			rows.Close()
			return 0, nil, err
		}
		add("unexpected trigger %s on magnetico_web.%s", name, table)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	// 6. Version rows, only if the version table has its expected shape.
	if !versionTableDamaged(problems) {
		rows, err = q.Query(ctx, `SELECT version FROM magnetico_web.web_version ORDER BY 1`)
		if err != nil {
			return 0, nil, err
		}
		var versions []int32
		for rows.Next() {
			var v int32
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return 0, nil, err
			}
			versions = append(versions, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, nil, err
		}
		if len(versions) != 1 || versions[0] != Version {
			add("magnetico_web.web_version rows %v, want exactly [%d]", versions, Version)
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return stateBad, problems, nil
	}
	return stateOK, nil, nil
}

func versionTableDamaged(problems []string) bool {
	for _, p := range problems {
		if strings.Contains(p, "magnetico_web.web_version.") || strings.Contains(p, "table magnetico_web.web_version") ||
			strings.Contains(p, "column magnetico_web.web_version") {
			return true
		}
	}
	return false
}

func normalize(s string) string { return strings.Join(strings.Fields(s), " ") }
