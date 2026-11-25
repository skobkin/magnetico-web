package stats

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// IngestState classifies the ingest schema as the web role sees it. The ingest
// schema is OPTIONAL for the web service: every state other than IngestOK
// degrades only the crawlers and quarantine pages.
type IngestState int

const (
	IngestOK IngestState = iota
	IngestMissing
	IngestIncompatible
	IngestPrivileges
)

func (s IngestState) String() string {
	return [...]string{"ok", "missing", "incompatible", "privileges missing"}[s]
}

// IngestStatus is the result of the capability probe.
type IngestStatus struct {
	State IngestState
	// Missing lists the missing structure or privileges. Names only: no SQL, no DSN.
	Missing []string
}

// The safe columns the web role may read (never token_hash, never the
// quarantine payload or info hash).
var (
	crawlerColumns    = []string{"crawler_id", "name", "enabled", "created_at", "token_rotated_at", "disabled_at"}
	quarantineColumns = []string{"id", "reason_code", "detail", "attempts", "payload_bytes", "received_at", "quarantined_at"}
)

// probeIngest looks at the ingest schema through catalogs and privilege
// functions only; it never reads a row.
func (r *Reader) probeIngest(ctx context.Context) (IngestStatus, error) {
	var exists, usage bool
	if err := r.db.QueryRow(ctx, `SELECT to_regnamespace('magnetico_ingest') IS NOT NULL,
COALESCE(has_schema_privilege(current_user, to_regnamespace('magnetico_ingest')::oid, 'USAGE'), false)`).Scan(&exists, &usage); err != nil {
		return IngestStatus{}, err
	}
	if !exists {
		return IngestStatus{State: IngestMissing, Missing: []string{"schema magnetico_ingest"}}, nil
	}
	if !usage {
		return IngestStatus{State: IngestPrivileges, Missing: []string{"magnetico_ingest: USAGE"}}, nil
	}
	var missing []string
	for _, spec := range []struct {
		table string
		cols  []string
	}{{"crawlers", crawlerColumns}, {"quarantine", quarantineColumns}} {
		var present bool
		if err := r.db.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "magnetico_ingest."+spec.table).Scan(&present); err != nil {
			return IngestStatus{}, err
		}
		if !present {
			missing = append(missing, "table "+spec.table)
			continue
		}
		for _, c := range spec.cols {
			var has bool
			if err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = $1::regclass AND attname = $2 AND NOT attisdropped AND attnum > 0)`,
				"magnetico_ingest."+spec.table, c).Scan(&has); err != nil {
				return IngestStatus{}, err
			}
			if !has {
				missing = append(missing, fmt.Sprintf("column %s.%s", spec.table, c))
			}
		}
	}
	if len(missing) > 0 {
		return IngestStatus{State: IngestIncompatible, Missing: missing}, nil
	}
	for _, spec := range []struct {
		table string
		cols  []string
	}{{"crawlers", crawlerColumns}, {"quarantine", quarantineColumns}} {
		for _, c := range spec.cols {
			var ok bool
			if err := r.db.QueryRow(ctx, `SELECT has_column_privilege(current_user, $1::regclass, $2, 'SELECT')`,
				"magnetico_ingest."+spec.table, c).Scan(&ok); err != nil {
				return IngestStatus{}, err
			}
			if !ok {
				missing = append(missing, fmt.Sprintf("SELECT (%s) on magnetico_ingest.%s", c, spec.table))
			}
		}
	}
	if len(missing) > 0 {
		return IngestStatus{State: IngestPrivileges, Missing: missing}, nil
	}
	return IngestStatus{State: IngestOK}, nil
}

// Cursor is the keyset position of the crawlers list: the lower-cased name of
// the last row of the previous page (names are unique case-insensitively, so
// it is a total order served by the unique index).
type Cursor struct{ Name string }

// Activity is what the statistics know about a crawler that has connected.
type Activity struct {
	FirstSeen, LastSeen time.Time
	LastIP              string
	LastSubmit          *time.Time
	Received, Enqueued  int64
	Duplicate           int64
	BytesBody           int64
	ExistsRequests      int64
	ExistsHashes        int64
	Rejected            int64
}

// CrawlerRow is one registered crawler. Activity is nil for a crawler that
// never connected (it is still listed).
type CrawlerRow struct {
	ID             string
	Name           string
	Enabled        bool
	CreatedAt      time.Time
	TokenRotatedAt *time.Time
	DisabledAt     *time.Time
	Activity       *Activity
}

// CrawlerPage is one page of the registry.
type CrawlerPage struct {
	Ingest IngestStatus
	Items  []CrawlerRow
	Next   *Cursor
	// StatsAvailable is false when the statistics schema cannot be read: the
	// registry is still listed, without activity.
	StatsAvailable bool
}

// crawlersSQL is the keyset page query: an index scan on the unique lower(name)
// index, so its cost depends on the page size, not on the registered count.
func crawlersSQL(statsUp bool) string {
	sel := `c.crawler_id::text, c.name, c.enabled, c.created_at, c.token_rotated_at, c.disabled_at`
	from := ` FROM magnetico_ingest.crawlers c`
	if statsUp {
		sel += `, a.first_seen_at, a.last_seen_at, host(a.last_ip), a.last_submit_at, a.records_received, a.records_enqueued,
 a.records_duplicate, a.bytes_body, a.exists_requests, a.exists_hashes, a.rejected_requests`
		from += ` LEFT JOIN magnetico_stats.crawler_activity a ON a.crawler_id = c.crawler_id`
	}
	return `SELECT ` + sel + from + ` WHERE lower(c.name) > $1 ORDER BY lower(c.name) LIMIT $2`
}

func (r *Reader) crawlers(ctx context.Context, after Cursor, statsUp bool) (CrawlerPage, error) {
	page := CrawlerPage{StatsAvailable: statsUp}
	rows, err := r.db.Query(ctx, crawlersSQL(statsUp), strings.ToLower(after.Name), CrawlerPageSize+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var c CrawlerRow
		dest := []any{&c.ID, &c.Name, &c.Enabled, &c.CreatedAt, &c.TokenRotatedAt, &c.DisabledAt}
		var first, last, submit *time.Time
		var ip *string
		var v [7]*int64
		if statsUp {
			dest = append(dest, &first, &last, &ip, &submit)
			for i := range v {
				dest = append(dest, &v[i])
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return page, err
		}
		if statsUp && first != nil && last != nil {
			a := &Activity{FirstSeen: first.UTC(), LastSeen: last.UTC(), LastSubmit: submit}
			if ip != nil {
				if addr, err := netip.ParseAddr(*ip); err == nil {
					a.LastIP = addr.String()
				}
			}
			get := func(i int) int64 {
				if v[i] == nil {
					return 0
				}
				return *v[i]
			}
			a.Received, a.Enqueued, a.Duplicate, a.BytesBody, a.ExistsRequests, a.ExistsHashes, a.Rejected =
				get(0), get(1), get(2), get(3), get(4), get(5), get(6)
			c.Activity = a
		}
		page.Items = append(page.Items, c)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > CrawlerPageSize {
		page.Items = page.Items[:CrawlerPageSize]
		page.Next = &Cursor{Name: strings.ToLower(page.Items[CrawlerPageSize-1].Name)}
	}
	return page, nil
}
