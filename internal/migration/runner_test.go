package migration

// Unit tests for the runner's input validation: refusals that must happen
// before any database contact — a missing source DSN (it has no CLI flag and
// empty env/file resolution must never fall back to the target) and
// out-of-range import tunables — plus ConfigError's static, secret-free
// text contract.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/migration/target"
)

func TestImportTorrentsRefusesWithoutDatabaseContact(t *testing.T) {
	t.Parallel()

	t.Run("MissingSourceDSN", func(t *testing.T) {
		err := ImportTorrents(context.Background(),
			TorrentsInput{TargetDSN: "postgres://localhost/target"})
		if !errors.Is(err, target.ErrSourceRequired) {
			t.Fatalf("ImportTorrents() error = %v, want ErrSourceRequired", err)
		}
	})

	t.Run("WorkersAboveMax", func(t *testing.T) {
		err := ImportTorrents(context.Background(), TorrentsInput{
			SourceDSN:        "postgres://localhost/source",
			TargetDSN:        "postgres://localhost/target",
			Workers:          MaxWorkers + 1,
			BatchSize:        1,
			MaxInflightBytes: 1 << 20,
		})
		var cfgErr *ConfigError
		if !errors.As(err, &cfgErr) || !strings.Contains(err.Error(), "--workers") {
			t.Fatalf("ImportTorrents() error = %v, want a ConfigError about --workers", err)
		}
	})

	t.Run("BatchAboveMax", func(t *testing.T) {
		err := ImportTorrents(context.Background(), TorrentsInput{
			SourceDSN:        "postgres://localhost/source",
			TargetDSN:        "postgres://localhost/target",
			Workers:          1,
			BatchSize:        MaxBatchSize + 1,
			MaxInflightBytes: 1 << 20,
		})
		var cfgErr *ConfigError
		if !errors.As(err, &cfgErr) || !strings.Contains(err.Error(), "--batch-size") {
			t.Fatalf("ImportTorrents() error = %v, want a ConfigError about --batch-size", err)
		}
	})

	t.Run("InflightBelowMinimum", func(t *testing.T) {
		err := ImportTorrents(context.Background(), TorrentsInput{
			SourceDSN:        "postgres://localhost/source",
			TargetDSN:        "postgres://localhost/target",
			Workers:          1,
			BatchSize:        1,
			MaxInflightBytes: (1 << 20) - 1,
		})
		var cfgErr *ConfigError
		if !errors.As(err, &cfgErr) || !strings.Contains(err.Error(), "--max-inflight-bytes") {
			t.Fatalf("ImportTorrents() error = %v, want a ConfigError about --max-inflight-bytes", err)
		}
	})
}
