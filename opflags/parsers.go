package opflags

import (
	"errors"
	"os"

	"github.com/jessevdk/go-flags"
)

// Parse parses the global options and the subcommand, applies the
// configuration layers (config file and environment) with the precedence
// CLI > ENV > config file > defaults, validates the crawl options, and
// returns the name of the selected subcommand.
func (o *OpFlags) Parse() (string, error) {
	parser := flags.NewParser(o, flags.Default)
	_, err := parser.Parse()
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

	if command == "crawl" {
		if err := o.applyConfig(explicitOptions(parser.Command)); err != nil {
			return "", err
		}
		if err := o.Crawl.check(); err != nil {
			return "", err
		}
	}

	return command, nil
}
