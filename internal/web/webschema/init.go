package webschema

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/persistence"
)

// seedJSON is master:config/public_trackers.json, verbatim and in order.
//
//go:embed seed/public_trackers.json
var seedJSON []byte

// SeedSHA256 is the SHA-256 of seedJSON (spec §8.3.4).
const SeedSHA256 = "f599174962ae3d0839f997b9b0ee76418596328e022dc3ebe91577daaacfc633"

const seedMarkerKey = "trackers.seed"

// SeedOutcome tells what the tracker-seed step of an initialization did.
type SeedOutcome string

const (
	SeedApplied         SeedOutcome = "seeded"
	SeedAlreadyDone     SeedOutcome = "already seeded"
	SeedSkippedNonEmpty SeedOutcome = "skipped: trackers table is not empty and has no seed marker"
)

// Report describes one Init run.
type Report struct {
	Created     bool // the schema was created by this run
	Seed        SeedOutcome
	SeededCount int
}

// seedList parses and verifies the embedded seed.
func seedList() ([]string, error) {
	sum := sha256.Sum256(seedJSON)
	if hex.EncodeToString(sum[:]) != SeedSHA256 {
		return nil, errors.New("internal error: embedded tracker seed does not match its recorded SHA-256")
	}
	var urls []string
	if err := json.Unmarshal(seedJSON, &urls); err != nil || len(urls) == 0 {
		return nil, errors.New("internal error: embedded tracker seed is not a non-empty JSON string list")
	}
	return urls, nil
}

// Init creates the magnetico_web schema and applies the approved tracker seed.
// It is explicit, non-destructive and idempotent:
//
//  1. a verified torrent schema is required (read-only) before anything is
//     written;
//  2. everything runs in one transaction under pg_advisory_xact_lock;
//  3. an absent schema is created and immediately self-checked against the
//     structural expectation (a drift between DDL and expectation rolls the
//     whole transaction back);
//  4. an existing schema is accepted only if it is exactly the version-1
//     structure; anything else is refused with ErrWebSchemaUnsupported and
//     nothing is written or repaired;
//  5. the seed step runs only for an accepted schema.
func Init(ctx context.Context, dsn string) (Report, error) {
	urls, err := seedList()
	if err != nil {
		return Report{}, err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return Report{}, fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	if err := persistence.VerifySchema(ctx, pool); err != nil {
		return Report{}, err
	}

	var report Report
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", initAdvisoryKey); err != nil {
			return err
		}
		// Normalize deparsed definitions; every statement below is schema-
		// qualified, so this changes nothing else.
		if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
			return err
		}
		st, details, err := inspect(ctx, tx)
		if err != nil {
			return err
		}
		switch st {
		case stateAbsent:
			for _, stmt := range ddl {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO magnetico_web.web_version (version) VALUES ($1)`, Version); err != nil {
				return err
			}
			// Self-check: the DDL must equal the expectation.
			st, details, err = inspect(ctx, tx)
			if err != nil {
				return err
			}
			if st != stateOK {
				return &StateError{Err: ErrWebSchemaUnsupported, Details: append([]string{"internal DDL/expectation mismatch"}, details...)}
			}
			report.Created = true
		case stateNoVersion, stateBad:
			return &StateError{Err: ErrWebSchemaUnsupported, Details: details}
		}
		return seed(ctx, tx, urls, &report)
	})
	if err != nil {
		var se *StateError
		if errors.As(err, &se) {
			return Report{}, se
		}
		return Report{}, fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	return report, nil
}

// seed inserts the approved historical tracker list exactly once: only if the
// settings marker is absent and the table is empty. A later operator edit is
// never undone, and a non-empty unmarked table is left alone and reported.
func seed(ctx context.Context, tx pgx.Tx, urls []string, r *Report) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", trackersAdvisoryKey); err != nil {
		return err
	}
	var marked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM magnetico_web.settings WHERE key = $1)`, seedMarkerKey).Scan(&marked); err != nil {
		return err
	}
	if marked {
		r.Seed = SeedAlreadyDone
		return nil
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM magnetico_web.trackers`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		r.Seed = SeedSkippedNonEmpty
		return nil
	}
	for i, u := range urls {
		if _, err := tx.Exec(ctx, `INSERT INTO magnetico_web.trackers (url, position) VALUES ($1, $2)`, u, i+1); err != nil {
			return err
		}
	}
	marker, _ := json.Marshal(map[string]any{
		"version": 1, "source": "master:config/public_trackers.json", "sha256": SeedSHA256, "count": len(urls),
	})
	if _, err := tx.Exec(ctx, `INSERT INTO magnetico_web.settings (key, value) VALUES ($1, $2::jsonb)`, seedMarkerKey, string(marker)); err != nil {
		return err
	}
	r.Seed, r.SeededCount = SeedApplied, len(urls)
	return nil
}
