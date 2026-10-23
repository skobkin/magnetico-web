package opflags

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/jessevdk/go-flags"
)

const envPrefix = "MAGNETICO_CRAWL_"

// crawlSetting describes one configurable crawl option: its CLI long name,
// its YAML key and its environment variable, plus the setters used to apply
// values from the configuration file and the environment. The value
// precedence is CLI > ENV > config file > built-in defaults.
type crawlSetting struct {
	longName string
	yamlKey  string
	envName  string
	// copyFrom applies a value decoded from the configuration file.
	copyFrom func(dst *CrawlOptions, src *CrawlOptions)
	// setFromString applies an environment-variable value.
	setFromString func(o *CrawlOptions, value string) error
}

func parseUint(value string, bitSize int) (uint, error) {
	n, err := strconv.ParseUint(value, 10, bitSize)
	if err != nil {
		return 0, err
	}
	return uint(n), nil
}

func setStringList(value string) ([]string, error) {
	return strings.Split(value, ","), nil
}

var crawlSettings = []crawlSetting{
	{
		longName: "database", yamlKey: "databaseURL", envName: "MAGNETICO_CRAWL_DATABASE",
		copyFrom:      func(dst, src *CrawlOptions) { dst.DatabaseURL = src.DatabaseURL },
		setFromString: func(o *CrawlOptions, v string) error { o.DatabaseURL = v; return nil },
	},
	{
		longName: "pyroscope", yamlKey: "pyroscopeURL", envName: "MAGNETICO_CRAWL_PYROSCOPE",
		copyFrom:      func(dst, src *CrawlOptions) { dst.PyroscopeURL = src.PyroscopeURL },
		setFromString: func(o *CrawlOptions, v string) error { o.PyroscopeURL = v; return nil },
	},
	{
		longName: "indexer-addr", yamlKey: "indexerAddrs", envName: "MAGNETICO_CRAWL_INDEXER_ADDR",
		copyFrom:      func(dst, src *CrawlOptions) { dst.IndexerAddrs = src.IndexerAddrs },
		setFromString: func(o *CrawlOptions, v string) error { l, err := setStringList(v); o.IndexerAddrs = l; return err },
	},
	{
		longName: "indexer-max-neighbors", yamlKey: "indexerMaxNeighbors", envName: "MAGNETICO_CRAWL_INDEXER_MAX_NEIGHBORS",
		copyFrom: func(dst, src *CrawlOptions) { dst.IndexerMaxNeighbors = src.IndexerMaxNeighbors },
		setFromString: func(o *CrawlOptions, v string) error {
			n, err := parseUint(v, strconv.IntSize)
			o.IndexerMaxNeighbors = n
			return err
		},
	},
	{
		longName: "leech-deadline", yamlKey: "leechDeadline", envName: "MAGNETICO_CRAWL_LEECH_DEADLINE",
		copyFrom: func(dst, src *CrawlOptions) { dst.LeechDeadline = src.LeechDeadline },
		setFromString: func(o *CrawlOptions, v string) error {
			n, err := parseUint(v, strconv.IntSize)
			o.LeechDeadline = n
			return err
		},
	},
	{
		longName: "leech-max-n", yamlKey: "leechMaxN", envName: "MAGNETICO_CRAWL_LEECH_MAX_N",
		copyFrom: func(dst, src *CrawlOptions) { dst.LeechMaxN = src.LeechMaxN },
		setFromString: func(o *CrawlOptions, v string) error {
			n, err := parseUint(v, strconv.IntSize)
			o.LeechMaxN = n
			return err
		},
	},
	{
		longName: "max-rps", yamlKey: "maxRPS", envName: "MAGNETICO_CRAWL_MAX_RPS",
		copyFrom: func(dst, src *CrawlOptions) { dst.MaxRPS = src.MaxRPS },
		setFromString: func(o *CrawlOptions, v string) error {
			n, err := parseUint(v, strconv.IntSize)
			o.MaxRPS = n
			return err
		},
	},
	{
		longName: "bootstrap-node", yamlKey: "bootstrappingNodes", envName: "MAGNETICO_CRAWL_BOOTSTRAP_NODE",
		copyFrom: func(dst, src *CrawlOptions) { dst.BootstrappingNodes = src.BootstrappingNodes },
		setFromString: func(o *CrawlOptions, v string) error {
			l, err := setStringList(v)
			o.BootstrappingNodes = l
			return err
		},
	},
	{
		longName: "filter-nodes-cidrs", yamlKey: "filterNodesCIDRs", envName: "MAGNETICO_CRAWL_FILTER_NODES_CIDRS",
		copyFrom:      func(dst, src *CrawlOptions) { dst.FilterNodesCIDRs = src.FilterNodesCIDRs },
		setFromString: func(o *CrawlOptions, v string) error { l, err := setStringList(v); o.FilterNodesCIDRs = l; return err },
	},
}

// applyConfig overlays the configuration file and the environment onto the
// parsed crawl options, enforcing CLI > ENV > config file > defaults.
// explicit maps the long names of options set on the command line.
func (o *OpFlags) applyConfig(explicit map[string]bool) error {
	// The config file location itself follows CLI > ENV.
	configPath := o.ConfigFilePath
	if !explicit["config-file-path"] {
		if value, ok := os.LookupEnv("MAGNETICO_CONFIG_FILE_PATH"); ok && value != "" {
			configPath = value
		}
	}

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return err
		}
		if err := o.applyYamlConfig(data, explicit); err != nil {
			return err
		}
	}

	return o.applyEnvConfig(explicit)
}

// applyYamlConfig merges the parsed file into the live options. The file is
// decoded twice: once into a generic map to learn which keys are present, and
// once strictly into a fresh CrawlOptions (unknown keys are an error). Only
// present keys are merged, so a sparse file never replaces defaults with zero
// values; explicitly configured zero-ish values are real values and apply.
func (o *OpFlags) applyYamlConfig(data []byte, explicit map[string]bool) error {
	var present map[string]any
	if err := yaml.Unmarshal(data, &present); err != nil {
		return fmt.Errorf("config file: %w", err)
	}

	var file CrawlOptions
	if err := yaml.UnmarshalWithOptions(data, &file, yaml.Strict()); err != nil {
		return fmt.Errorf("config file: %w", err)
	}

	for _, s := range crawlSettings {
		if _, found := present[s.yamlKey]; !found {
			continue
		}
		if explicit[s.longName] {
			continue
		}
		s.copyFrom(&o.Crawl, &file)
	}

	return nil
}

// applyEnvConfig applies MAGNETICO_CRAWL_* variables and rejects unknown ones.
// Empty values are treated as unset so that template-based deployments can
// safely expand unset placeholders.
func (o *OpFlags) applyEnvConfig(explicit map[string]bool) error {
	for _, s := range crawlSettings {
		if explicit[s.longName] {
			continue
		}
		if value, ok := os.LookupEnv(s.envName); ok && value != "" {
			if err := s.setFromString(&o.Crawl, value); err != nil {
				return fmt.Errorf("%s: %w", s.envName, err)
			}
		}
	}

	known := make(map[string]bool, len(crawlSettings)+1)
	known[envPrefix] = true
	for _, s := range crawlSettings {
		known[s.envName] = true
	}

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, envPrefix) || known[name] {
			continue
		}
		names := make([]string, 0, len(crawlSettings))
		for _, s := range crawlSettings {
			names = append(names, s.envName)
		}
		return fmt.Errorf(
			"unknown environment variable %s; known variables: MAGNETICO_CONFIG_FILE_PATH, %s",
			name, strings.Join(names, ", "),
		)
	}

	return nil
}

// explicitOptions collects the long names of all options that were set on the
// command line (as opposed to defaulted) across the whole command tree.
//
// go-flags applies tag defaults through Option.Set as well, so Option.IsSet()
// alone is true for every option after parsing. An explicitly provided option
// skips the subsequent default application, which leaves Option.IsSetDefault()
// false for it; both checks together identify CLI-provided options.
func explicitOptions(root *flags.Command) map[string]bool {
	set := map[string]bool{}
	var walkGroup func(g *flags.Group)
	walkGroup = func(g *flags.Group) {
		for _, opt := range g.Options() {
			if opt.IsSet() && !opt.IsSetDefault() && opt.LongName != "" {
				set[opt.LongName] = true
			}
		}
		for _, child := range g.Groups() {
			walkGroup(child)
		}
	}
	var walkCommand func(c *flags.Command)
	walkCommand = func(c *flags.Command) {
		walkGroup(c.Group)
		for _, sub := range c.Commands() {
			walkCommand(sub)
		}
	}
	walkCommand(root)
	return set
}
