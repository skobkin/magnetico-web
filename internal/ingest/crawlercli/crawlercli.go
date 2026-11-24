// Package crawlercli implements the operator commands `magnetico ingest
// crawler add|list|disable|enable|rotate-token`. A token is printed exactly
// once, by add and rotate-token, and never again: only its digest is stored.
// Listings never contain a token or a digest.
package crawlercli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/skobkin/magnetico-web/internal/ingest/crawlers"
)

// propagationNote is printed by every command that changes what a running
// ingest instance may accept (the authentication cache lives 60 s).
const propagationNote = "Running ingest instances apply this within 60 seconds (their authentication cache lives that long)."

// Env is what the commands need.
type Env struct {
	Store *crawlers.Store
	Out   io.Writer
}

func (e *Env) printf(format string, args ...any) { fmt.Fprintf(e.Out, format, args...) }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func opt(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return ts(*t)
}

func (e *Env) printCreated(verb string, c crawlers.Created) {
	e.printf("%s crawler %s\n", verb, c.Name)
	e.printf("id: %s\n", c.ID)
	e.printf("token: %s\n", c.Token)
	e.printf("The token is shown only now and cannot be recovered. Give it to that crawler as MAGNETICO_CRAWL_INGEST_TOKEN.\n")
}

// Add registers a crawler.
func Add(ctx context.Context, e *Env, name string) error {
	c, err := e.Store.Add(ctx, name)
	if err != nil {
		return err
	}
	e.printCreated("registered", c)
	e.printf("A new token is accepted immediately.\n")
	return nil
}

// List prints one line per crawler (no token, no digest).
func List(ctx context.Context, e *Env) error {
	list, err := e.Store.List(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(e.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "id\tname\tenabled\tcreated\ttoken rotated\tdisabled")
	for _, c := range list {
		fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\t%s\n", c.ID, c.Name, c.Enabled, ts(c.CreatedAt), opt(c.TokenRotatedAt), opt(c.DisabledAt))
	}
	return w.Flush()
}

// SetEnabled disables or enables a crawler.
func SetEnabled(ctx context.Context, e *Env, name string, enabled bool) error {
	if err := e.Store.SetEnabled(ctx, name, enabled); err != nil {
		return err
	}
	if enabled {
		e.printf("enabled crawler %s\n", name)
	} else {
		e.printf("disabled crawler %s\n", name)
	}
	e.printf("%s\n", propagationNote)
	return nil
}

// RotateToken replaces a crawler's token.
func RotateToken(ctx context.Context, e *Env, name string) error {
	c, err := e.Store.RotateToken(ctx, name)
	if err != nil {
		return err
	}
	e.printCreated("rotated the token of", c)
	e.printf("The old token stops working on running ingest instances within 60 seconds; the new one is accepted immediately.\n")
	return nil
}
