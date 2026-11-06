// Package pgidentity establishes whether two PostgreSQL connections point at
// the same actual database, using only transport-independent facts.
//
// inet_server_addr()/inet_server_port() are NULL over Unix sockets and
// differ across TCP/socket aliases of one server, so they can never prove
// equality or distinctness; they are recorded as informational fields only.
// The decision uses pg_postmaster_start_time() (identical for every
// connection to one postmaster, distinct across independent servers —
// barring a same-microsecond start, which fails closed toward "same") and
// current_database().
//
// Distinctness is concluded only from differing start time or database name;
// unreadable identity fails closed with ok=false. Never trust URL text.
package pgidentity

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Identity is one connection's server-side view of where it is attached.
type Identity struct {
	// PostmasterStart is the server process start time; identical for every
	// connection to one running PostgreSQL instance.
	PostmasterStart time.Time
	// Database is current_database().
	Database string
	// ServerAddress and ServerPort are informational only (empty over Unix
	// sockets); they never feed the decision.
	ServerAddress string
	ServerPort    string
	// Known reports that the identity could be established. Unknown identity
	// must fail closed.
	Known bool
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Read establishes the identity of the database behind one connection.
func Read(ctx context.Context, q queryRower) (Identity, error) {
	var (
		identity Identity
		addr     *string
		port     *int
	)
	if err := q.QueryRow(ctx, `SELECT pg_postmaster_start_time(), current_database(),
	        host(inet_server_addr()), inet_server_port()`).Scan(
		&identity.PostmasterStart, &identity.Database, &addr, &port,
	); err != nil {
		return Identity{}, fmt.Errorf("database identity query failed: %w", err)
	}
	if addr != nil {
		identity.ServerAddress = *addr
	}
	if port != nil {
		identity.ServerPort = fmt.Sprintf("%d", *port)
	}
	// A zero start time or empty database name means the server answered
	// with something unusable — refuse to conclude anything.
	identity.Known = !identity.PostmasterStart.IsZero() && identity.Database != ""
	return identity, nil
}

// SameDatabase reports whether two identities denote the same actual
// database. ok=false means identity could not be established on either side
// and the caller must fail closed rather than assume distinctness.
func SameDatabase(a, b Identity) (same, ok bool) {
	if !a.Known || !b.Known {
		return false, false
	}
	return a.PostmasterStart.Equal(b.PostmasterStart) && a.Database == b.Database, true
}
