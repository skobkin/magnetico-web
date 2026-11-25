package admin

import (
	"fmt"

	"github.com/skobkin/magnetico-web/internal/web/server"
)

// installPages registers the listing pages (added by later steps).
func (h *Handlers) installPages(rt *server.Router) {}

func typeName(v any) string { return fmt.Sprintf("%T", v) }
