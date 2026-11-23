package trackers

import (
	"net/http"

	"github.com/skobkin/magnetico-web/internal/web/server"
)

// Install registers GET /magnet/{infohash} (§8.3.1): public, no session, no
// cookie. It answers 307 with the magnet URI of the hash carrying the trackers
// of the in-memory snapshot, in their stored order and without dn. It never
// asks whether the torrent exists and does no database work at all (the
// snapshot is memory), so it cannot reveal which hashes are indexed and keeps
// working while the database is down.
func Install(srv *server.Server, c *Cache) {
	srv.Router().Handle("GET /magnet/{infohash}", server.KindRequest, server.Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash, ok := ValidInfoHash(r.PathValue("infohash"))
		if !ok {
			srv.ErrorPage(w, r, http.StatusNotFound)
			return
		}
		w.Header().Set("Location", MagnetURI(hash, nil, c.URLs()))
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
}
