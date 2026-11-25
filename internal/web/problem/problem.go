// Package problem writes RFC 9457 application/problem+json responses. It is
// shared by the session-authenticated UI JSON endpoints and the future
// bearer-authenticated API v2 so both report errors identically.
//
// Problem documents never carry names, paths, tokens or internal detail.
package problem

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Type URIs are URNs: stable identifiers, not dereferenceable pages.
const typePrefix = "urn:magnetico:problem:"

// Stable problem type slugs.
const (
	Unauthenticated       = "unauthenticated"
	Forbidden             = "forbidden"
	NotFound              = "not-found"
	InvalidParameter      = "invalid-parameter"
	FullTreeUnavailable   = "full-tree-unavailable"
	IndexTooComplex       = "index-too-complex"
	TemporarilyBusy       = "temporarily-unavailable"
	InternalError         = "internal-error"
	MethodNotAllowed      = "method-not-allowed"
	ServiceNotImplemented = "not-implemented"
	RateLimited           = "rate-limited"
)

// Problem is one problem document.
type Problem struct {
	Slug   string // stable machine-readable type (without the URN prefix)
	Title  string
	Status int
	Detail string
	// Alternate, if set, is the URL of the endpoint the client should use
	// instead (the paginated file view for full-tree-unavailable).
	Alternate string
	// RetryAfterSeconds, if positive, is sent as Retry-After.
	RetryAfterSeconds int
}

type document struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Alternate string `json:"alternate,omitempty"`
}

// Write sends p.
func Write(w http.ResponseWriter, p Problem) {
	body, _ := json.Marshal(document{
		Type: typePrefix + p.Slug, Title: p.Title, Status: p.Status, Detail: p.Detail, Alternate: p.Alternate,
	})
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Cache-Control", "private, no-store")
	if p.RetryAfterSeconds > 0 {
		h.Set("Retry-After", strconv.Itoa(p.RetryAfterSeconds))
	}
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}
