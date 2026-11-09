package opflags

import (
	"errors"
	"os"

	"github.com/jessevdk/go-flags"
)

// Parse parses the global options and the subcommand, applies the
// configuration layers (config file and environment) with the precedence
// CLI > ENV > config file > defaults, validates the active command's
// options, and returns the name of the selected subcommand.
func (o *OpFlags) Parse() (string, error) {
	return o.parseArgs(os.Args[1:])
}

// parseArgs is Parse over an explicit argument vector (testable).
func (o *OpFlags) parseArgs(args []string) (string, error) {
	parser := flags.NewParser(o, flags.Default)
	_, err := parser.ParseArgs(args)
	if err != nil {
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		}
		return "", err
	}

	active := parser.Active
	if active == nil {
		return "", errors.New("no command given; expected one of: crawl, web, migrate (see --help)")
	}
	command := active.Name

	switch command {
	case "crawl":
		if err := o.applyConfig(explicitOptions(parser.Command), command); err != nil {
			return "", err
		}
		if err := o.Crawl.check(); err != nil {
			return "", err
		}
	case "migrate":
		if active.Active == nil {
			return "", errors.New("migrate: expected a subcommand; expected: schema-init, torrents, verify-decode, verify-content, errors, cleanup (see --help)")
		}
		sub := active.Active
		switch {
		case sub.Name == "torrents" && sub.Active != nil:
			// `migrate torrents retry-errors` — the nested repair pass is
			// the effective subcommand (go-flags only ever activates the
			// known nested command, so the name check is belt and braces).
			o.Migrate.Subcommand = sub.Active.Name
			o.transferSharedFlagsToRetry(sub, sub.Active)
		case sub.Name == "errors":
			// `migrate errors export` — the nested forensic export is the
			// effective subcommand; bare `migrate errors` is a usage error.
			if sub.Active == nil {
				return "", errors.New("migrate errors: expected a subcommand; expected: export (see --help)")
			}
			o.Migrate.Subcommand = sub.Active.Name
			o.transferSharedFlagsToExport(sub, sub.Active)
		default:
			o.Migrate.Subcommand = sub.Name
		}
		if err := o.applyConfig(explicitOptions(parser.Command), command); err != nil {
			return "", err
		}
	}

	return command, nil
}

// transferSharedFlagsToRetry moves the shared repair-pass options the
// operator positioned before the nested command onto RetryErrors:
// `migrate torrents --database X retry-errors` parses `--database X` into
// the torrents command and leaves RetryErrors' own field at its tag
// default — the repair pass would silently run against a different target
// (or with different tunables) than the command line says. Each shared
// option transfers only when the parent position set it explicitly and the
// nested position did not: the nested position always wins. This runs
// before the config layers, so the transferred value takes the CLI
// precedence in the whole-tree explicit check (yaml/env skip when any
// --database was given on the command line).
func (o *OpFlags) transferSharedFlagsToRetry(parent, nested *flags.Command) {
	pset, nset := explicitInCommand(parent), explicitInCommand(nested)
	t := &o.Migrate.Torrents
	r := &t.RetryErrors
	if pset["database"] && !nset["database"] {
		r.DatabaseURL = t.DatabaseURL
	}
	if pset["workers"] && !nset["workers"] {
		r.Workers = t.Workers
	}
	if pset["batch-size"] && !nset["batch-size"] {
		r.BatchSize = t.BatchSize
	}
	if pset["max-inflight-bytes"] && !nset["max-inflight-bytes"] {
		r.MaxInflightBytes = t.MaxInflightBytes
	}
}

// transferSharedFlagsToExport moves the shared --database the operator
// positioned before the nested export command onto ErrorsExport:
// `migrate errors --database X export` parses `--database X` into the errors
// command and leaves the export's own field at its tag default — the same
// parent/nested precedence rule as transferSharedFlagsToRetry (the nested
// position always wins; this runs before the config layers).
func (o *OpFlags) transferSharedFlagsToExport(parent, nested *flags.Command) {
	pset, nset := explicitInCommand(parent), explicitInCommand(nested)
	e := &o.Migrate.Errors
	x := &e.Export
	if pset["database"] && !nset["database"] {
		x.DatabaseURL = e.DatabaseURL
	}
}
