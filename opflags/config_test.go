package opflags

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// defaults returns CrawlOptions equivalent to the built-in flag defaults.
func defaults() CrawlOptions {
	return CrawlOptions{
		DatabaseURL:         "postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable",
		PyroscopeURL:        "",
		IndexerAddrs:        []string{"0.0.0.0:0"},
		IndexerMaxNeighbors: 5000,
		LeechDeadline:       600,
		LeechMaxN:           1000,
		MaxRPS:              500,
		BootstrappingNodes: []string{
			"dht.tgragnato.it:80", "dht.tgragnato.it:443", "dht.tgragnato.it:1337",
			"dht.tgragnato.it:6969", "dht.tgragnato.it:6881", "dht.tgragnato.it:25401",
		},
		FilterNodesCIDRs: []string{""},
	}
}

func TestApplyYamlConfig_SparseFileKeepsDefaults(t *testing.T) {
	t.Parallel()

	data := []byte("databaseURL: postgres://file-only@localhost/db\n")

	o := &OpFlags{Crawl: defaults()}
	if err := o.applyYamlConfig(data, map[string]bool{}); err != nil {
		t.Fatalf("applyYamlConfig() error = %v", err)
	}

	expected := defaults()
	expected.DatabaseURL = "postgres://file-only@localhost/db"

	if !reflect.DeepEqual(o.Crawl, expected) {
		t.Errorf("sparse file changed fields other than databaseURL:\n got %+v\nwant %+v", o.Crawl, expected)
	}
}

func TestApplyYamlConfig_ExplicitValuesApplied(t *testing.T) {
	t.Parallel()

	data := []byte("maxRPS: 123\nindexerAddrs:\n  - 127.0.0.1:6881\n  - 127.0.0.2:6881\n")

	o := &OpFlags{Crawl: defaults()}
	if err := o.applyYamlConfig(data, map[string]bool{}); err != nil {
		t.Fatalf("applyYamlConfig() error = %v", err)
	}

	if o.Crawl.MaxRPS != 123 {
		t.Errorf("MaxRPS = %d, want 123", o.Crawl.MaxRPS)
	}
	if len(o.Crawl.IndexerAddrs) != 2 || o.Crawl.IndexerAddrs[0] != "127.0.0.1:6881" || o.Crawl.IndexerAddrs[1] != "127.0.0.2:6881" {
		t.Errorf("IndexerAddrs = %v, want two explicit addresses", o.Crawl.IndexerAddrs)
	}
}

func TestApplyYamlConfig_ZeroishValuesApply(t *testing.T) {
	t.Parallel()

	// A file that explicitly sets a zero-ish value must apply it; it must not
	// be mistaken for an absent key.
	data := []byte("leechMaxN: 0\nfilterNodesCIDRs: []\n")

	o := &OpFlags{Crawl: defaults()}
	if err := o.applyYamlConfig(data, map[string]bool{}); err != nil {
		t.Fatalf("applyYamlConfig() error = %v", err)
	}

	if o.Crawl.LeechMaxN != 0 {
		t.Errorf("LeechMaxN = %d, want 0", o.Crawl.LeechMaxN)
	}
	if o.Crawl.FilterNodesCIDRs == nil || len(o.Crawl.FilterNodesCIDRs) != 0 {
		t.Errorf("FilterNodesCIDRs = %#v, want empty non-nil slice", o.Crawl.FilterNodesCIDRs)
	}
}

func TestApplyYamlConfig_CLIBeatsFile(t *testing.T) {
	t.Parallel()

	data := []byte("databaseURL: postgres://file@localhost/db\nmaxRPS: 123\n")

	o := &OpFlags{Crawl: defaults()}
	explicit := map[string]bool{"database": true}
	if err := o.applyYamlConfig(data, explicit); err != nil {
		t.Fatalf("applyYamlConfig() error = %v", err)
	}

	if o.Crawl.DatabaseURL != defaults().DatabaseURL {
		t.Errorf("explicit CLI flag was overwritten by the file: %s", o.Crawl.DatabaseURL)
	}
	if o.Crawl.MaxRPS != 123 {
		t.Errorf("MaxRPS = %d, want 123 (file value for a non-explicit flag)", o.Crawl.MaxRPS)
	}
}

func TestApplyYamlConfig_UnknownKeyRejected(t *testing.T) {
	t.Parallel()

	data := []byte("databaseURL: postgres://x@localhost/db\nrunWeb: true\n")

	o := &OpFlags{Crawl: defaults()}
	err := o.applyYamlConfig(data, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "runWeb") {
		t.Errorf("expected unknown-key error mentioning runWeb, got: %v", err)
	}
}

func TestApplyYamlConfig_DerivedFieldRejected(t *testing.T) {
	t.Parallel()

	// The payloads are the shapes that strict decoding silently accepted
	// before the field was excluded: an empty list and null decode into the
	// []net.IPNet field without error and were then ignored by the merge
	// layer. (Scalar, non-empty-list and mapping payloads already failed with
	// type errors, and camelCase was rejected only by accident of verbatim
	// key matching.) yaml:"-" makes any shape a loud unknown-field error.
	tests := []struct {
		name string
		data string
		key  string
	}{
		{
			name: "lowercase render name with empty list",
			data: "databaseURL: postgres://x@localhost/db\nfilternodesipnets: []\n",
			key:  "filternodesipnets",
		},
		{
			name: "lowercase render name with null",
			data: "databaseURL: postgres://x@localhost/db\nfilternodesipnets: null\n",
			key:  "filternodesipnets",
		},
		{
			name: "camelCase",
			data: "databaseURL: postgres://x@localhost/db\nfilterNodesIpNets:\n  - ip: [10, 0, 0, 0]\n    mask: [255, 255, 0, 0]\n",
			key:  "filterNodesIpNets",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			o := &OpFlags{Crawl: defaults()}
			err := o.applyYamlConfig([]byte(tt.data), map[string]bool{})
			if err == nil || !strings.Contains(err.Error(), `unknown field "`+tt.key+`"`) {
				t.Errorf("expected unknown-field error naming %s, got: %v", tt.key, err)
			}
		})
	}
}

func TestApplyYamlConfig_CidrsApplyAndIpNetsStayDerived(t *testing.T) {
	t.Parallel()

	data := []byte("filterNodesCIDRs:\n  - 192.168.1.0/24\n")

	o := &OpFlags{Crawl: defaults()}
	if err := o.applyYamlConfig(data, map[string]bool{}); err != nil {
		t.Fatalf("applyYamlConfig() error = %v", err)
	}

	if len(o.Crawl.FilterNodesCIDRs) != 1 || o.Crawl.FilterNodesCIDRs[0] != "192.168.1.0/24" {
		t.Errorf("FilterNodesCIDRs = %v, want [192.168.1.0/24]", o.Crawl.FilterNodesCIDRs)
	}
	if o.Crawl.FilterNodesIpNets != nil {
		t.Errorf("FilterNodesIpNets = %v, want nil before check() derives it", o.Crawl.FilterNodesIpNets)
	}

	// check() derives it. The bootstrapping nodes are replaced because the
	// upstream defaults are rejected in filter mode.
	crawl := o.Crawl
	crawl.BootstrappingNodes = []string{"internal:6881"}
	if err := crawl.check(); err != nil {
		t.Fatalf("check() error = %v", err)
	}
	if len(crawl.FilterNodesIpNets) != 1 || crawl.FilterNodesIpNets[0].String() != "192.168.1.0/24" {
		t.Errorf("FilterNodesIpNets = %v, want the derived 192.168.1.0/24", crawl.FilterNodesIpNets)
	}
}

func TestApplyEnvConfig_Precedence(t *testing.T) {
	// t.Setenv forbids parallel execution.
	data := []byte("databaseURL: postgres://file@localhost/db\nmaxRPS: 123\n")

	t.Run("EnvBeatsFile", func(t *testing.T) {
		t.Setenv("MAGNETICO_CRAWL_DATABASE", "postgres://env@localhost/db")

		o := &OpFlags{Crawl: defaults()}
		if err := o.applyYamlConfig(data, map[string]bool{}); err != nil {
			t.Fatalf("applyYamlConfig() error = %v", err)
		}
		if err := o.applyEnvConfig(map[string]bool{}); err != nil {
			t.Fatalf("applyEnvConfig() error = %v", err)
		}

		if o.Crawl.DatabaseURL != "postgres://env@localhost/db" {
			t.Errorf("DatabaseURL = %s, want the env value", o.Crawl.DatabaseURL)
		}
		if o.Crawl.MaxRPS != 123 {
			t.Errorf("MaxRPS = %d, want the file value", o.Crawl.MaxRPS)
		}
	})

	t.Run("EnvIgnoredWhenFlagExplicit", func(t *testing.T) {
		t.Setenv("MAGNETICO_CRAWL_DATABASE", "postgres://env@localhost/db")

		o := &OpFlags{Crawl: defaults()}
		explicit := map[string]bool{"database": true}
		if err := o.applyEnvConfig(explicit); err != nil {
			t.Fatalf("applyEnvConfig() error = %v", err)
		}

		if o.Crawl.DatabaseURL != defaults().DatabaseURL {
			t.Errorf("DatabaseURL = %s, want the CLI/default value", o.Crawl.DatabaseURL)
		}
	})

	t.Run("EnvListSplitsOnCommas", func(t *testing.T) {
		t.Setenv("MAGNETICO_CRAWL_BOOTSTRAP_NODE", "host1:6881,host2:6881")

		o := &OpFlags{Crawl: defaults()}
		if err := o.applyEnvConfig(map[string]bool{}); err != nil {
			t.Fatalf("applyEnvConfig() error = %v", err)
		}

		if len(o.Crawl.BootstrappingNodes) != 2 || o.Crawl.BootstrappingNodes[0] != "host1:6881" {
			t.Errorf("BootstrappingNodes = %v, want two comma-separated hosts", o.Crawl.BootstrappingNodes)
		}
	})

	t.Run("EmptyEnvTreatedAsUnset", func(t *testing.T) {
		t.Setenv("MAGNETICO_CRAWL_DATABASE", "")

		o := &OpFlags{Crawl: defaults()}
		if err := o.applyEnvConfig(map[string]bool{}); err != nil {
			t.Fatalf("applyEnvConfig() error = %v", err)
		}

		if o.Crawl.DatabaseURL != defaults().DatabaseURL {
			t.Errorf("DatabaseURL = %s, want the default (empty env is unset)", o.Crawl.DatabaseURL)
		}
	})

	t.Run("UnknownEnvRejected", func(t *testing.T) {
		t.Setenv("MAGNETICO_CRAWL_BOGUS", "1")

		o := &OpFlags{Crawl: defaults()}
		err := o.applyEnvConfig(map[string]bool{})
		if err == nil || !strings.Contains(err.Error(), "MAGNETICO_CRAWL_BOGUS") {
			t.Errorf("expected unknown-variable error, got: %v", err)
		}
	})

	t.Run("DerivedFieldEnvRejected", func(t *testing.T) {
		t.Setenv("MAGNETICO_CRAWL_FILTER_NODES_IP_NETS", "10.0.0.0/8")

		o := &OpFlags{Crawl: defaults()}
		err := o.applyEnvConfig(map[string]bool{})
		if err == nil || !strings.Contains(err.Error(), "MAGNETICO_CRAWL_FILTER_NODES_IP_NETS") {
			t.Errorf("expected unknown-variable error, got: %v", err)
		}
	})

	t.Run("ConversionErrorReported", func(t *testing.T) {
		t.Setenv("MAGNETICO_CRAWL_MAX_RPS", "not-a-number")

		o := &OpFlags{Crawl: defaults()}
		err := o.applyEnvConfig(map[string]bool{})
		if err == nil || !strings.Contains(err.Error(), "MAGNETICO_CRAWL_MAX_RPS") {
			t.Errorf("expected conversion error naming the variable, got: %v", err)
		}
	})
}

func TestApplyConfig_ConfigFilePathFromEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "databaseURL: postgres://envfile@localhost/db\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	t.Setenv("MAGNETICO_CONFIG_FILE_PATH", path)

	o := &OpFlags{Crawl: defaults()}
	if err := o.applyConfig(map[string]bool{}, "crawl"); err != nil {
		t.Fatalf("applyConfig() error = %v", err)
	}

	if o.Crawl.DatabaseURL != "postgres://envfile@localhost/db" {
		t.Errorf("DatabaseURL = %s, want the file value via MAGNETICO_CONFIG_FILE_PATH", o.Crawl.DatabaseURL)
	}
}

func TestApplyConfig_MissingFileError(t *testing.T) {
	o := &OpFlags{
		ConfigFilePath: filepath.Join(t.TempDir(), "does-not-exist.yml"),
		Crawl:          defaults(),
	}
	if err := o.applyConfig(map[string]bool{}, "crawl"); err == nil {
		t.Error("expected an error for a missing config file, got nil")
	}
}
