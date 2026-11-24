package users

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
)

func isContextDone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// SrcUser, SrcInvite and SrcToken are the legacy rows as stored. Timestamps
// are PHP wall-clock values without a zone: pgx returns them labelled UTC and
// the labels are discarded when the operator-stated zone is applied.
type SrcUser struct {
	ID        int64
	Username  string
	Password  string
	Email     string
	Roles     string
	CreatedAt time.Time
}

type SrcInvite struct {
	ID       int64
	UserID   int64
	UsedByID *int64
	Code     string
}

type SrcToken struct {
	Key       string
	UserID    int64
	CreatedAt time.Time
}

// Evidence is what the migrator can observe about legacy write isolation. It
// is informational: it can never prove that no writer remains.
type Evidence struct {
	OtherClientBackends int
	DatabaseReadOnly    bool
	WritePrivileges     bool
	Identity            pgidentity.Identity
}

// Snapshot is one consistent read of the legacy tables.
type Snapshot struct {
	Users   []SrcUser
	Invites []SrcInvite
	Tokens  []SrcToken
	Digest  string
	// Notes are fingerprint warnings (a missing constraint, a write grant).
	Notes []string
}

// source is the read-only connection to the legacy database.
type source struct{ conn *pgx.Conn }

func openSource(ctx context.Context, dsn string) (*source, *Error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, newErr(ClassConfig, "the source database URL cannot be parsed")
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["application_name"] = "magnetico_migrate_users_source"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, storageErr("connect source", err)
	}
	return &source{conn: conn}, nil
}

func (s *source) close() { _ = s.conn.Close(context.Background()) }

var expectedColumns = map[string]map[string]string{
	"users": { //nolint:gosec // G101: legacy column definitions (a column named "password"), not a credential
		"id": "integer NOT NULL", "username": "character varying(25) NOT NULL",
		"password": "text NOT NULL", "email": "character varying(254) NOT NULL",
		"roles": "json NOT NULL", "created_at": "timestamp(0) without time zone NOT NULL",
	},
	"invites": {
		"id": "integer NOT NULL", "user_id": "integer NOT NULL",
		"used_by_id": "integer NULL", "code": "character varying(32) NOT NULL",
	},
	"api_tokens": {
		"key": "character varying(32) NOT NULL", "user_id": "integer NOT NULL",
		"created_at": "timestamp(0) without time zone NOT NULL",
	},
}

var expectedTables = []string{"api_tokens", "invites", "password_reset_tokens", "users"}

// knownDoctrineVersions are the five migrations of master:src/Migrations.
var knownDoctrineVersions = []string{
	"Version20180626223216", "Version20200118004840", "Version20250701154230",
	"Version20250701155112", "Version20250701161732",
}

// expectedConstraints are the PK/unique/FK counts the legacy schema carries;
// a shortfall is a warning only (the data checks re-establish the invariants).
var expectedConstraints = map[string]map[string]int{
	"users":      {"p": 1},
	"invites":    {"p": 1, "f": 2},
	"api_tokens": {"p": 1, "f": 1},
}

// expectedUniqueIndexes are the single-column uniqueness guarantees of the
// legacy schema. Doctrine creates them with CREATE UNIQUE INDEX, which leaves
// no pg_constraint row, so they are looked up in pg_index.
var expectedUniqueIndexes = []struct{ table, column string }{
	{"users", "username"},
	{"users", "email"},
	{"invites", "code"},
}

// fingerprint refuses any source that is not exactly the known legacy schema.
// It runs inside the snapshot transaction. Returns warnings.
func fingerprint(ctx context.Context, tx pgx.Tx) (notes []string, err *Error) {
	var has bool
	if e := tx.QueryRow(ctx, `SELECT to_regnamespace('users') IS NOT NULL`).Scan(&has); e != nil {
		return nil, storageErr("source fingerprint", e)
	}
	if !has {
		return nil, newErr(ClassSourceSchema, `the source database has no "users" schema`)
	}
	rows, e := tx.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'users' AND c.relkind IN ('r','p','v','m','f') ORDER BY 1`)
	if e != nil {
		return nil, storageErr("source fingerprint", e)
	}
	var tables []string
	for rows.Next() {
		var n string
		if e := rows.Scan(&n); e != nil {
			rows.Close()
			return nil, storageErr("source fingerprint", e)
		}
		tables = append(tables, n)
	}
	rows.Close()
	if e := rows.Err(); e != nil {
		return nil, storageErr("source fingerprint", e)
	}
	if strings.Join(tables, ",") != strings.Join(expectedTables, ",") {
		return nil, newErr(ClassSourceSchema, "the source schema does not contain exactly the legacy tables",
			"found: "+strings.Join(tables, ","), "expected: "+strings.Join(expectedTables, ","))
	}
	for _, t := range []string{"users", "invites", "api_tokens"} {
		crows, e := tx.Query(ctx, `SELECT a.attname,
                format_type(a.atttypid, a.atttypmod) || CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE ' NULL' END
            FROM pg_attribute a WHERE a.attrelid = to_regclass('users.' || quote_ident($1)) AND a.attnum > 0 AND NOT a.attisdropped`, t)
		if e != nil {
			return nil, storageErr("source fingerprint", e)
		}
		got := map[string]string{}
		for crows.Next() {
			var name, typ string
			if e := crows.Scan(&name, &typ); e != nil {
				crows.Close()
				return nil, storageErr("source fingerprint", e)
			}
			got[name] = typ
		}
		crows.Close()
		if e := crows.Err(); e != nil {
			return nil, storageErr("source fingerprint", e)
		}
		var diffs []string
		for name, typ := range expectedColumns[t] {
			if got[name] != typ {
				diffs = append(diffs, fmt.Sprintf("users.%s.%s", t, name))
			}
		}
		for name := range got {
			if _, ok := expectedColumns[t][name]; !ok {
				diffs = append(diffs, fmt.Sprintf("users.%s.%s (unexpected)", t, name))
			}
		}
		if len(diffs) > 0 {
			sort.Strings(diffs)
			return nil, newErr(ClassSourceSchema, "a migrated source table differs from the legacy definition", diffs...)
		}
	}
	// Doctrine version table: public first, then users.
	versionTable := ""
	for _, cand := range []string{"public.migration_versions", "users.migration_versions"} {
		var ok bool
		if e := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, cand).Scan(&ok); e != nil {
			return nil, storageErr("source fingerprint", e)
		}
		if ok {
			versionTable = cand
			break
		}
	}
	if versionTable == "" {
		return nil, newErr(ClassSourceSchema, "the Doctrine migration_versions table was not found (public or users schema)")
	}
	vrows, e := tx.Query(ctx, `SELECT version FROM `+versionTable)
	if e != nil {
		return nil, storageErr("source fingerprint", e)
	}
	seen := map[string]bool{}
	var unknown int
	for vrows.Next() {
		var v string
		if e := vrows.Scan(&v); e != nil {
			vrows.Close()
			return nil, storageErr("source fingerprint", e)
		}
		if i := strings.LastIndex(v, `\`); i >= 0 {
			v = v[i+1:]
		}
		known := false
		for _, k := range knownDoctrineVersions {
			if v == k {
				known, seen[k] = true, true
			}
		}
		if !known {
			unknown++
		}
	}
	vrows.Close()
	if e := vrows.Err(); e != nil {
		return nil, storageErr("source fingerprint", e)
	}
	var missing []string
	for _, k := range knownDoctrineVersions {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 || unknown > 0 {
		d := missing
		if unknown > 0 {
			d = append(d, fmt.Sprintf("%d unknown newer migration version(s)", unknown))
		}
		return nil, newErr(ClassSourceSchema, "the source Doctrine migration history is not the known one", d...)
	}
	for t, want := range expectedConstraints {
		for typ, n := range want {
			var got int
			if e := tx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
                WHERE conrelid = to_regclass('users.' || quote_ident($1)) AND contype = $2::"char"`, t, typ).Scan(&got); e != nil {
				return nil, storageErr("source fingerprint", e)
			}
			if got < n {
				notes = append(notes, fmt.Sprintf("source-constraint-missing: users.%s has %d of %d %s constraints", t, got, n, typ))
			}
		}
	}
	for _, u := range expectedUniqueIndexes {
		var ok bool
		// A valid, non-partial, non-expression unique index over exactly
		// this one column (a unique constraint creates one too).
		if e := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_index i
            JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
            WHERE i.indrelid = to_regclass('users.' || quote_ident($1))
              AND i.indisunique AND i.indisvalid AND i.indislive
              AND i.indnkeyatts = 1 AND i.indpred IS NULL AND i.indexprs IS NULL
              AND a.attname = $2)`, u.table, u.column).Scan(&ok); e != nil {
			return nil, storageErr("source fingerprint", e)
		}
		if !ok {
			notes = append(notes, fmt.Sprintf("source-constraint-missing: users.%s has no valid unique index on one column (%s)", u.table, u.column))
		}
	}
	var writeGrant bool
	if e := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM unnest(ARRAY['users.users','users.invites','users.api_tokens','users.password_reset_tokens']) t,
            unnest(ARRAY['INSERT','UPDATE','DELETE']) p WHERE has_table_privilege(current_user, t, p))`).Scan(&writeGrant); e != nil {
		return nil, storageErr("source fingerprint", e)
	}
	if writeGrant {
		notes = append(notes, "source-role-has-write-privileges: the source role holds INSERT/UPDATE/DELETE on a legacy table (all source transactions are READ ONLY regardless)")
	}
	sort.Strings(notes)
	return notes, nil
}

// snapshot reads the fingerprint and every migrated row inside one
// REPEATABLE READ READ ONLY transaction, in deterministic order.
func (s *source) snapshot(ctx context.Context) (*Snapshot, *Error) {
	tx, err := s.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, storageErr("begin source", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return nil, storageErr("source search_path", err)
	}
	notes, ferr := fingerprint(ctx, tx)
	if ferr != nil {
		return nil, ferr
	}
	snap := &Snapshot{Notes: notes}

	rows, err := tx.Query(ctx, `SELECT id, username, password, email, roles::text, created_at FROM users.users ORDER BY id`)
	if err != nil {
		return nil, storageErr("read source users", err)
	}
	for rows.Next() {
		var u SrcUser
		if err := rows.Scan(&u.ID, &u.Username, &u.Password, &u.Email, &u.Roles, &u.CreatedAt); err != nil {
			rows.Close()
			return nil, storageErr("read source users", err)
		}
		snap.Users = append(snap.Users, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, storageErr("read source users", err)
	}

	rows, err = tx.Query(ctx, `SELECT id, user_id, used_by_id, code FROM users.invites ORDER BY id`)
	if err != nil {
		return nil, storageErr("read source invites", err)
	}
	for rows.Next() {
		var v SrcInvite
		if err := rows.Scan(&v.ID, &v.UserID, &v.UsedByID, &v.Code); err != nil {
			rows.Close()
			return nil, storageErr("read source invites", err)
		}
		snap.Invites = append(snap.Invites, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, storageErr("read source invites", err)
	}

	rows, err = tx.Query(ctx, `SELECT key, user_id, created_at FROM users.api_tokens`)
	if err != nil {
		return nil, storageErr("read source tokens", err)
	}
	for rows.Next() {
		var t SrcToken
		if err := rows.Scan(&t.Key, &t.UserID, &t.CreatedAt); err != nil {
			rows.Close()
			return nil, storageErr("read source tokens", err)
		}
		snap.Tokens = append(snap.Tokens, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, storageErr("read source tokens", err)
	}
	// The order of tokens is fixed in Go (created_at, owner, key) so it does
	// not depend on the server collation.
	sort.Slice(snap.Tokens, func(i, j int) bool {
		a, b := snap.Tokens[i], snap.Tokens[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		return a.Key < b.Key
	})
	snap.Digest = snap.digest()
	return snap, nil
}

func (s *Snapshot) digest() string {
	h := newHasher("magnetico-users-source-v1")
	h.u64(uint64(len(s.Users)))
	for _, u := range s.Users {
		h.i64(u.ID)
		h.str(u.Username)
		h.str(u.Password)
		h.str(u.Email)
		h.str(u.Roles)
		h.time(u.CreatedAt)
	}
	h.u64(uint64(len(s.Invites)))
	for _, v := range s.Invites {
		h.i64(v.ID)
		h.i64(v.UserID)
		h.opt(v.UsedByID)
		h.str(v.Code)
	}
	h.u64(uint64(len(s.Tokens)))
	for _, t := range s.Tokens {
		h.str(t.Key)
		h.i64(t.UserID)
		h.time(t.CreatedAt)
	}
	return h.sum()
}

// evidence reports what is observable about legacy write isolation.
func (s *source) evidence(ctx context.Context) (Evidence, *Error) {
	var ev Evidence
	id, err := pgidentity.Read(ctx, s.conn)
	if err != nil {
		return ev, storageErr("source identity", err)
	}
	ev.Identity = id
	if err := s.conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
        WHERE datname = current_database() AND pid <> pg_backend_pid() AND backend_type = 'client backend'`).Scan(&ev.OtherClientBackends); err != nil {
		return ev, storageErr("source activity", err)
	}
	if err := s.conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_db_role_setting s JOIN pg_database d ON d.oid = s.setdatabase
        WHERE d.datname = current_database() AND s.setrole = 0
          AND EXISTS (SELECT 1 FROM unnest(s.setconfig) c WHERE c = 'default_transaction_read_only=on'))`).Scan(&ev.DatabaseReadOnly); err != nil {
		return ev, storageErr("source settings", err)
	}
	return ev, nil
}
