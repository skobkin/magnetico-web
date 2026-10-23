package opflags

import (
	"testing"
)

func TestCheckAddrs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		indexerAddrs []string
		expectError  bool
	}{
		{
			name:         "EmptyIndexerAddrs",
			indexerAddrs: []string{},
			expectError:  true,
		},
		{
			name:         "SingleEmptyIndexerAddr",
			indexerAddrs: []string{""},
			expectError:  true,
		},
		{
			name:         "ValidIndexerAddrs",
			indexerAddrs: []string{"127.0.0.1:6881", "192.168.1.1:6881"},
			expectError:  false,
		},
		{
			name:         "InvalidIndexerAddr",
			indexerAddrs: []string{"invalid-addr"},
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crawl := &CrawlOptions{
				IndexerAddrs: tt.indexerAddrs,
			}
			err := crawl.checkAddrs()
			if (err != nil) != tt.expectError {
				t.Errorf("checkAddrs() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		crawl       CrawlOptions
		expectError bool
	}{
		{
			name: "ValidAddrs",
			crawl: CrawlOptions{
				IndexerAddrs: []string{"127.0.0.1:6881"},
			},
			expectError: false,
		},
		{
			name: "InvalidAddrs",
			crawl: CrawlOptions{
				IndexerAddrs: []string{"invalid-addr"},
			},
			expectError: true,
		},
		{
			name: "InvalidCIDR",
			crawl: CrawlOptions{
				FilterNodesCIDRs: []string{"invalid-cidr"},
				IndexerAddrs:     []string{"0.0.0.0:0"},
			},
			expectError: true,
		},
		{
			name: "ValidCIDR",
			crawl: CrawlOptions{
				IndexerAddrs:     []string{"0.0.0.0:0"},
				FilterNodesCIDRs: []string{"192.168.1.0/24"},
			},
			expectError: false,
		},
		{
			name: "DefaultBootstrappingNodesInFilterMode",
			crawl: CrawlOptions{
				FilterNodesCIDRs:   []string{"192.168.1.0/24"},
				BootstrappingNodes: []string{"dht.tgragnato.it:80", "dht.tgragnato.it:443", "dht.tgragnato.it:1337", "dht.tgragnato.it:6969", "dht.tgragnato.it:6881", "dht.tgragnato.it:25401"},
				IndexerAddrs:       []string{"0.0.0.0:0"},
			},
			expectError: true,
		},
		{
			name: "CustomBootstrappingNodesInFilterMode",
			crawl: CrawlOptions{
				FilterNodesCIDRs:   []string{"", "192.168.1.0/24"},
				BootstrappingNodes: []string{"", "www.tgragnato.it:443"},
				IndexerAddrs:       []string{"0.0.0.0:0"},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.crawl.check()
			if (err != nil) != tt.expectError {
				t.Errorf("check() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}
