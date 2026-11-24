package cli

import (
	"os"
	"testing"

	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/testkit"
)

func TestMain(m *testing.M) {
	code := pgtest.RunTests(m)
	testkit.Cleanup()
	os.Exit(code)
}
