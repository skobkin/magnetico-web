package stats

import (
	"io"
	"log"
	"net/netip"

	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
)

func ingestSchemaInit(dsn string) error { return pgqueue.InitSchema(bg, dsn) }

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func netipAddr(s string) netip.Addr { return netip.MustParseAddr(s) }
