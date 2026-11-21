package existence

import "github.com/skobkin/magnetico-web/persistence"

// persistence.Store is the production Finder.
var _ Finder = persistence.Store(nil)
