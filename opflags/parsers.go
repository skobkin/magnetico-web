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

	active := parser.Command.Active
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
			return "", errors.New("migrate: expected a subcommand; expected: schema-init (see --help)")
		}
		if err := o.applyConfig(explicitOptions(parser.Command), command); err != nil {
			return "", err
		}
	}

	return command, nil
}
