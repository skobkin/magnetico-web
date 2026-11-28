package migration

// Plan regression for the shared verification page query (0.8.0-rc1
// production incident, docs/wip/verify-page-query-performance-0.8.0-rc2.md):
// under the production plan shape — a Merge Left Join over the two primary
// key indexes — the blob-side scan must start at the cursor instead of
// traversing the already processed prefix, for custom AND generic
// parameterized plans. Assertions count rows, never time.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// unrestrictedDecodePageQuery is the 0.8.0-rc1 page query, kept only as the
// control that proves the forced plan shape reproduces the prefix scan.
const unrestrictedDecodePageQuery = `SELECT t.id, t.info_hash, t.name, t.total_size, t.discovered_on,
            t.file_count, b.torrent_id, octet_length(b.data)
        FROM magneticod.torrents t
        LEFT JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id
        WHERE t.id > $1
        ORDER BY t.id
        LIMIT $2`

// planNode is the subset of EXPLAIN (FORMAT JSON) a scan-volume check needs.
type planNode struct {
	NodeType          string     `json:"Node Type"`
	RelationName      string     `json:"Relation Name"`
	IndexCond         string     `json:"Index Cond"`
	ActualRows        float64    `json:"Actual Rows"`
	ActualLoops       float64    `json:"Actual Loops"`
	RowsRemovedFilter float64    `json:"Rows Removed by Filter"`
	Plans             []planNode `json:"Plans"`
}

// pageQueryPlan summarises one EXPLAIN ANALYZE of a page query.
type pageQueryPlan struct {
	mergeJoin    bool
	blobRows     int64    // rows the blob-side scans produced or filtered
	blobIndexCnd []string // Index Cond of every blob-side scan
}

func summarisePlan(n planNode, out *pageQueryPlan) {
	if strings.HasPrefix(n.NodeType, "Merge") {
		out.mergeJoin = true
	}
	if n.RelationName == "torrent_file_blobs" {
		loops := n.ActualLoops
		if loops < 1 {
			loops = 1
		}
		out.blobRows += int64((n.ActualRows + n.RowsRemovedFilter) * loops)
		out.blobIndexCnd = append(out.blobIndexCnd, n.IndexCond)
	}
	for _, c := range n.Plans {
		summarisePlan(c, out)
	}
}

// explainPageQuery prepares query on conn under planCacheMode, runs it once
// for its rows and once under EXPLAIN ANALYZE, and returns both.
func explainPageQuery(t *testing.T, ctx context.Context, conn *pgx.Conn, planCacheMode, query string, cursor int64, limit int) (pageQueryPlan, []decodeRow) {
	t.Helper()
	if _, err := conn.Exec(ctx, "SET plan_cache_mode = "+planCacheMode); err != nil {
		t.Fatalf("set plan_cache_mode: %v", err)
	}
	if _, err := conn.Exec(ctx, "PREPARE page_q AS "+query); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "DEALLOCATE page_q") }()

	rows, err := conn.Query(ctx, fmt.Sprintf("EXECUTE page_q(%d, %d)", cursor, limit))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var got []decodeRow
	for rows.Next() {
		var r decodeRow
		var blobID *int64
		if err := rows.Scan(&r.id, &r.infoHash, &r.name, &r.totalSize, &r.discoveredOn,
			&r.fileCount, &blobID, &r.blobLen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		r.hasBlobRow = blobID != nil
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	var raw []byte
	if err := conn.QueryRow(ctx, fmt.Sprintf(
		"EXPLAIN (ANALYZE, TIMING OFF, FORMAT JSON) EXECUTE page_q(%d, %d)", cursor, limit)).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var doc []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("explain output %s: %v", raw, err)
	}
	var plan pageQueryPlan
	summarisePlan(doc[0].Plan, &plan)
	return plan, got
}

func TestDecodePageQueryBlobScanStartsAtCursor(t *testing.T) {
	t.Parallel()

	tgt := createTargetForRunner(t)
	const total = 4000
	const cursor = 3000
	const limit = 64
	// Even ids carry a blob, odd ids none: the page must keep both shapes.
	execTargetSQL(t, tgt, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on, file_count)
        SELECT g, decode(lpad(to_hex(g), 40, '0'), 'hex'), 't' || g, 10, 1700000000, 2
        FROM generate_series(1, $1::int) g`, total)
	execTargetSQL(t, tgt, `INSERT INTO magneticod.torrent_file_blobs (torrent_id, data)
        SELECT g, '\x0102'::bytea FROM generate_series(2, $1::int, 2) g`, total)
	execTargetSQL(t, tgt, `ANALYZE magneticod.torrents, magneticod.torrent_file_blobs`)

	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, tgt)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	// Force the production plan shape: a Merge Left Join over the primary
	// key index scans. A small fixture would otherwise get a nested loop or
	// a hash join, which hides the prefix scan either way.
	for _, s := range []string{"enable_seqscan", "enable_bitmapscan", "enable_hashjoin", "enable_nestloop", "enable_sort"} {
		if _, err := conn.Exec(ctx, "SET "+s+" = off"); err != nil {
			t.Fatalf("set %s: %v", s, err)
		}
	}

	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		// Control: the rc1 query under the same shape walks the processed
		// prefix (every blob up to the cursor) — the incident reproduced.
		ctrl, _ := explainPageQuery(t, ctx, conn, mode, unrestrictedDecodePageQuery, cursor, limit)
		if !ctrl.mergeJoin || ctrl.blobRows < cursor/2 {
			t.Fatalf("%s control: merge join %v, blob rows %d — the forced shape no longer reproduces the prefix scan (want a merge join and >= %d blob rows)",
				mode, ctrl.mergeJoin, ctrl.blobRows, cursor/2)
		}

		plan, rows := explainPageQuery(t, ctx, conn, mode, decodePageQuery, cursor, limit)
		if !plan.mergeJoin {
			t.Fatalf("%s: plan is not a merge join under the forced shape", mode)
		}
		// One page of blobs (every other row) plus the merge's look-ahead
		// row; anything near cursor/2 means the prefix is scanned again.
		if plan.blobRows > limit {
			t.Fatalf("%s: blob-side scan produced %d rows for a %d-row page at cursor %d, want <= %d (index conds %q)",
				mode, plan.blobRows, limit, cursor, limit, plan.blobIndexCnd)
		}
		wantCond := fmt.Sprint(cursor)
		if mode == "force_generic_plan" {
			wantCond = "$1"
		}
		if len(plan.blobIndexCnd) == 0 || !strings.Contains(plan.blobIndexCnd[0], wantCond) {
			t.Fatalf("%s: blob index conds %q, want the cursor bound (%s) in the index condition",
				mode, plan.blobIndexCnd, wantCond)
		}

		// Semantics: the next limit torrents in id order, rows without a
		// blob kept with NULL blob columns.
		if len(rows) != limit {
			t.Fatalf("%s: page rows = %d, want %d", mode, len(rows), limit)
		}
		for i, r := range rows {
			wantID := int64(cursor + 1 + i)
			wantBlob := wantID%2 == 0
			if r.id != wantID || r.hasBlobRow != wantBlob || (r.blobLen != nil) != wantBlob ||
				(wantBlob && *r.blobLen != 2) {
				t.Fatalf("%s: row %d = id %d blob %v len %v, want id %d blob %v",
					mode, i, r.id, r.hasBlobRow, r.blobLen, wantID, wantBlob)
			}
		}
	}
}
