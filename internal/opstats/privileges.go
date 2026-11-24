package opstats

import (
	"context"
	"fmt"
)

// Role selects the privilege set a caller needs on magnetico_stats.
type Role int

const (
	// RoleIngest writes statistics: DML on every table.
	RoleIngest Role = iota
	// RoleWeb reads statistics: SELECT on every table.
	RoleWeb
)

func (r Role) privileges() []string {
	if r == RoleIngest {
		return []string{"SELECT", "INSERT", "UPDATE", "DELETE"}
	}
	return []string{"SELECT"}
}

// MissingPrivileges returns "schema.table: PRIV" for every privilege the role
// lacks on an existing magnetico_stats relation (and "magnetico_stats: USAGE").
// An absent schema yields an empty list: Verify reports that. The current
// database user is checked.
func MissingPrivileges(ctx context.Context, db DB, role Role) ([]string, error) {
	var exists, usage bool
	if err := db.QueryRow(ctx, `SELECT to_regnamespace($1) IS NOT NULL,
COALESCE(has_schema_privilege(current_user, to_regnamespace($1)::oid, 'USAGE'), false)`, SchemaName).Scan(&exists, &usage); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	if !usage {
		return []string{SchemaName + ": USAGE"}, nil
	}
	var missing []string
	for _, t := range tables {
		qualified := SchemaName + "." + t.name
		var present bool
		if err := db.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, qualified).Scan(&present); err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		for _, p := range role.privileges() {
			var ok bool
			if err := db.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, qualified, p).Scan(&ok); err != nil {
				return nil, err
			}
			if !ok {
				missing = append(missing, fmt.Sprintf("%s: %s", qualified, p))
			}
		}
	}
	return missing, nil
}
