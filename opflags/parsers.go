package opflags

import (
	"errors"
	"os"

	"github.com/goccy/go-yaml"
	"github.com/jessevdk/go-flags"
)

// Parse parses the global options and the subcommand, applies the YAML
// configuration to the crawl options when a config file is given, validates
// them, and returns the name of the selected subcommand.
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
		// Note: upstream behavior preserved at this step - the config file is
		// applied after flag parsing and its values override CLI values. A
		// later commit replaces this with the CLI > ENV > file > defaults
		// hierarchy.
		if err := o.parseYaml(); err != nil {
			return "", err
		}
		if err := o.Crawl.check(); err != nil {
			return "", err
		}
	}

	return command, nil
}

func (o *OpFlags) parseYaml() error {
	if o.ConfigFilePath == "" {
		return nil
	}

	data, err := os.ReadFile(o.ConfigFilePath)
	if err != nil {
		return err
	}

	return yaml.Unmarshal(data, &o.Crawl)
}
