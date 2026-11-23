package problem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrite(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	Write(rec, Problem{Slug: FullTreeUnavailable, Title: "Full tree unavailable", Status: http.StatusUnprocessableEntity,
		Detail: "use the paginated endpoint", Alternate: "/torrents/1/files", RetryAfterSeconds: 5})
	if rec.Code != 422 || rec.Header().Get("Content-Type") != "application/problem+json" || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d["type"] != "urn:magnetico:problem:full-tree-unavailable" || d["status"] != float64(422) || d["alternate"] != "/torrents/1/files" {
		t.Fatalf("%v", d)
	}
}
