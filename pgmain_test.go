package main

import (
	"os"
	"testing"

	"github.com/skobkin/magnetico-web/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }
