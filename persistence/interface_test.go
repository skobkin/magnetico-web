package persistence

import (
	"errors"
	"testing"
)

// TestMakeDatabase_FailClosed is the permanent guard of the fail-closed
// storage contract: until phase 3 implements the new PostgreSQL storage,
// MakeDatabase must refuse every scheme and never return a usable Database,
// so that no code path can create or migrate a legacy schema.
func TestMakeDatabase_FailClosed(t *testing.T) {
	t.Parallel()

	rawURLs := []string{
		"sqlite3:///tmp/makedatabase_failclosed.db?cache=shared&mode=memory",
		"sqlite:///tmp/makedatabase_failclosed.db?cache=shared&mode=memory",
		"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable",
		"cockroach://magnetico:magnetico@localhost:26257/magnetico",
		"zmq://127.0.0.1:1234",
		"zeromq://127.0.0.1:1234",
		"amqp://guest:guest@localhost:5672/",
		"amqps://guest:guest@localhost:5671/",
		"bitmagnet://localhost:3333/import?debug=true&source=testsource",
		"bitmagnets://localhost:3333/import?debug=true&source=testsource",
		"",
		"nonsense",
		"invalidscheme://localhost",
	}

	for _, rawURL := range rawURLs {
		db, err := MakeDatabase(rawURL)
		if !errors.Is(err, ErrStorageNotImplemented) {
			t.Errorf("Expected ErrStorageNotImplemented for URL %q, got: %v", rawURL, err)
		}
		if db != nil {
			t.Errorf("Expected nil Database for URL %q, got %T", rawURL, db)
		}
	}
}
