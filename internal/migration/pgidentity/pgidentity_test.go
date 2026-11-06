package pgidentity

import (
	"testing"
	"time"
)

func startAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestSameDatabase covers every transport combination the decision function
// can face. Address and port never feed the decision: only postmaster start
// time and database name do.
func TestSameDatabase(t *testing.T) {
	t.Parallel()

	serverA := startAt("2026-11-07T01:00:00Z")
	serverB := startAt("2026-11-07T02:00:00Z")
	sameMicrosecond := serverA // identical start time, second server: refused (fail-closed direction)

	tests := []struct {
		name     string
		a, b     Identity
		wantSame bool
		wantOK   bool
	}{
		{
			name:     "same database via TCP and via Unix socket",
			a:        Identity{PostmasterStart: serverA, Database: "magnetico", ServerAddress: "127.0.0.1", ServerPort: "5432", Known: true},
			b:        Identity{PostmasterStart: serverA, Database: "magnetico", ServerAddress: "", ServerPort: "", Known: true},
			wantSame: true,
			wantOK:   true,
		},
		{
			name:     "same database via 127.0.0.1 and localhost aliases",
			a:        Identity{PostmasterStart: serverA, Database: "magnetico", ServerAddress: "127.0.0.1", ServerPort: "5432", Known: true},
			b:        Identity{PostmasterStart: serverA, Database: "magnetico", ServerAddress: "::1", ServerPort: "5432", Known: true},
			wantSame: true,
			wantOK:   true,
		},
		{
			name:     "different databases on the same instance are distinct",
			a:        Identity{PostmasterStart: serverA, Database: "magnetico", Known: true},
			b:        Identity{PostmasterStart: serverA, Database: "magnetico_target", Known: true},
			wantSame: false,
			wantOK:   true,
		},
		{
			name:     "same database name on different instances is distinct",
			a:        Identity{PostmasterStart: serverA, Database: "magnetico", Known: true},
			b:        Identity{PostmasterStart: serverB, Database: "magnetico", Known: true},
			wantSame: false,
			wantOK:   true,
		},
		{
			name:     "different addresses with equal start time stay same (aliases)",
			a:        Identity{PostmasterStart: serverA, Database: "magnetico", ServerAddress: "10.0.0.1", Known: true},
			b:        Identity{PostmasterStart: serverA, Database: "magnetico", ServerAddress: "192.168.0.1", Known: true},
			wantSame: true,
			wantOK:   true,
		},
		{
			name:   "unknown identity on one side fails closed",
			a:      Identity{PostmasterStart: serverA, Database: "magnetico", Known: false},
			b:      Identity{PostmasterStart: serverB, Database: "other", Known: true},
			wantOK: false,
		},
		{
			name:   "unknown identity on both sides fails closed",
			a:      Identity{Known: false},
			b:      Identity{Known: false},
			wantOK: false,
		},
		{
			name:     "two servers started in the same microsecond with equal name are refused, not passed",
			a:        Identity{PostmasterStart: sameMicrosecond, Database: "magnetico", Known: true},
			b:        Identity{PostmasterStart: sameMicrosecond, Database: "magnetico", Known: true},
			wantSame: true,
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			same, ok := SameDatabase(tt.a, tt.b)
			if same != tt.wantSame || ok != tt.wantOK {
				t.Fatalf("SameDatabase() = (%v, %v), want (%v, %v)", same, ok, tt.wantSame, tt.wantOK)
			}
		})
	}
}
