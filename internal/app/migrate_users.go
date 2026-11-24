package app

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	usermig "github.com/skobkin/magnetico-web/internal/migration/users"
	"github.com/skobkin/magnetico-web/opflags"
)

// runMigrateUsers implements `magnetico migrate users` (W8): the identity-
// preserving import of the legacy PHP accounts in one target transaction, or
// its read-only --dry-run. Exit status 0 only for a completed (or already
// completed) import or a dry-run a final run would accept; 1 for everything
// else. Output carries counts, ids, classes and digests only.
func runMigrateUsers(opFlags *opflags.UsersOptions) {
	ids, err := usermig.ParseIDList(opFlags.DeleteUnmatchedUserIDs)
	if err != nil {
		log.Fatalln("migrate users [config]: --delete-unmatched-user-ids must be a comma-separated list of distinct positive integers")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, runErr := usermig.Run(ctx, usermig.Config{
		SourceDSN:           opFlags.SourceDatabaseURL,
		TargetDSN:           opFlags.DatabaseURL,
		SourceTimezone:      opFlags.SourceTimezone,
		DryRun:              opFlags.DryRun,
		AcceptNeedsReset:    opFlags.AcceptNeedsReset,
		ReplaceUsers:        opFlags.ReplaceUsers,
		ConfirmPlan:         opFlags.ConfirmPlan,
		DeleteUnmatchedIDs:  ids,
		SourceWritesStopped: opFlags.SourceWritesStopped,
	})
	finishUsersCommand("migrate users", opFlags.ReportJSON, rep, runErr)
}

// runMigrateUsersVerify implements `magnetico migrate users verify`.
func runMigrateUsersVerify(opFlags *opflags.UsersVerifyOptions) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, runErr := usermig.Verify(ctx, usermig.Config{
		SourceDSN:      opFlags.SourceDatabaseURL,
		TargetDSN:      opFlags.DatabaseURL,
		SourceTimezone: opFlags.SourceTimezone,
	})
	finishUsersCommand("migrate users verify", opFlags.ReportJSON, rep, runErr)
}

// finishUsersCommand writes the optional report file, prints the secret-free
// summary and exits non-zero for an error. It never prints err.Error() of a
// foreign error: usermig errors are classified and secret-safe by construction.
func finishUsersCommand(name, reportPath string, rep *usermig.Report, runErr error) {
	if rep != nil {
		for _, line := range rep.Summary() {
			log.Printf("%s: %s", name, line)
		}
		if reportPath != "" {
			if err := usermig.WriteReport(reportPath, rep); err != nil {
				log.Printf("%s: could not write the report file: %v", name, err)
				if runErr == nil {
					os.Exit(1)
				}
			}
		}
	}
	if runErr != nil {
		var e *usermig.Error
		if errors.As(runErr, &e) {
			log.Fatalf("%s failed [%s]: %s", name, e.Class, strings.TrimPrefix(e.Error(), e.Class+": "))
		}
		log.Fatalf("%s failed: unexpected %T", name, runErr)
	}
}
