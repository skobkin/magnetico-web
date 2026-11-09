package migration

// The 04C wrappers map context cancellation to the shared ErrInterrupted
// contract (exit 2) before any database contact: a cancelled context fails
// the connection attempt, never the gate logic. This pins the runMigrate*
// exit-code table's interrupted paths without a live PostgreSQL.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/skobkin/magnetico-web/internal/migration/forensics"
)

func Test04CWrappersMapCancellationToInterrupted(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A bogus DSN is fine: the wrappers must refuse through ErrInterrupted
	// at the (cancelled) connection attempt, before any network I/O.
	t.Run("ExportErrors", func(t *testing.T) {
		_, err := ExportErrors(ctx, forensics.ExportInput{
			TargetDSN: "postgres://127.0.0.1:1/none",
			Options: forensics.ExportOptions{
				OutputPath: forensics.StdoutPath,
				Limits:     forensics.DefaultLimits(),
			},
		})
		if err != ErrInterrupted {
			t.Fatalf("ExportErrors() error = %v, want ErrInterrupted", err)
		}
	})

	t.Run("Cleanup", func(t *testing.T) {
		_, err := Cleanup(ctx, forensics.CleanupInput{
			TargetDSN: "postgres://127.0.0.1:1/none",
			Options:   forensics.CleanupOptions{DryRun: true},
		})
		if err != ErrInterrupted {
			t.Fatalf("Cleanup() error = %v, want ErrInterrupted", err)
		}
	})

	// The forensics layer validates flags before the connection: flag
	// contradictions surface as typed OptionsErrors even under cancellation.
	t.Run("CleanupFlagContradictionStillTyped", func(t *testing.T) {
		_, err := Cleanup(ctx, forensics.CleanupInput{
			TargetDSN: "postgres://127.0.0.1:1/none",
			Options:   forensics.CleanupOptions{DryRun: true, Execute: true},
		})
		var optsErr *forensics.OptionsError
		if !errors.As(err, &optsErr) {
			t.Fatalf("Cleanup() error = %v, want an OptionsError (validated before any connection)", err)
		}
	})
}

// Test04CWrappersMapLockLossCauseToSafetyFailure (review round 1, finding
// 5): a lock loss cancels the run with a typed cause — the wrappers must
// surface that cause as the SAFETY failure it is, never the operator
// interruption contract (which maps to exit 2).
func Test04CWrappersMapLockLossCauseToSafetyFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(fmt.Errorf("%w: ping failed after the connection died", forensics.ErrLockLost))

	t.Run("ExportErrors", func(t *testing.T) {
		_, err := ExportErrors(ctx, forensics.ExportInput{
			TargetDSN: "postgres://127.0.0.1:1/none",
			Options: forensics.ExportOptions{
				OutputPath: forensics.StdoutPath,
				Limits:     forensics.DefaultLimits(),
			},
		})
		if errors.Is(err, ErrInterrupted) {
			t.Fatalf("ExportErrors() = ErrInterrupted — a lock loss must never surface as an operator interrupt")
		}
		if !errors.Is(err, forensics.ErrLockLost) {
			t.Fatalf("ExportErrors() error = %v, want the forensics.ErrLockLost cause", err)
		}
	})

	t.Run("Cleanup", func(t *testing.T) {
		_, err := Cleanup(ctx, forensics.CleanupInput{
			TargetDSN: "postgres://127.0.0.1:1/none",
			Options:   forensics.CleanupOptions{DryRun: true},
		})
		if errors.Is(err, ErrInterrupted) {
			t.Fatalf("Cleanup() = ErrInterrupted — a lock loss must never surface as an operator interrupt")
		}
		if !errors.Is(err, forensics.ErrLockLost) {
			t.Fatalf("Cleanup() error = %v, want the forensics.ErrLockLost cause", err)
		}
	})
}
