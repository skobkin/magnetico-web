package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssetsAreContentHashed(t *testing.T) {
	a, err := NewAssets()
	if err != nil {
		t.Fatal(err)
	}
	u, err := a.URL("app.css")
	if err != nil || !strings.HasPrefix(u, "/static/app.") || !strings.HasSuffix(u, ".css") || u == "/static/app.css" {
		t.Fatalf("url = %q err=%v", u, err)
	}
	if _, err := a.URL("missing.css"); err == nil {
		t.Fatal("unknown asset must be an error")
	}
	if len(a.Names()) == 0 {
		t.Fatal("no embedded assets")
	}

	file := strings.TrimPrefix(u, StaticPrefix)
	rec := httptest.NewRecorder()
	a.ServeFile(rec, httptest.NewRequest("GET", u, nil), file)
	if rec.Code != 200 || rec.Header().Get("ETag") == "" ||
		rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") || rec.Body.Len() == 0 {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	for _, bad := range []string{"app.css", "app.000000000000.css", "", "../app.css"} {
		rec = httptest.NewRecorder()
		a.ServeFile(rec, httptest.NewRequest("GET", "/static/x", nil), bad)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%q served with %d", bad, rec.Code)
		}
	}
}

func TestRendererErrorPageEscapesAndBuffers(t *testing.T) {
	a, _ := NewAssets()
	r, err := NewRenderer(a, "")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	err = r.Render(rec, 500, "error", ErrorPage{Status: 500, Heading: "Oops", Message: "<script>alert(1)</script>", RequestID: "abc123"})
	if err != nil || rec.Code != 500 {
		t.Fatalf("%v %d", err, rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("message not escaped: %s", body)
	}
	for _, want := range []string{"abc123", `name="robots" content="noindex, nofollow"`, "/static/app.", "<!doctype html>"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}

	// A failing render writes nothing.
	rec = httptest.NewRecorder()
	if err := r.Render(rec, 200, "nope", nil); err == nil || rec.Body.Len() != 0 {
		t.Fatalf("unknown page: err=%v body=%q", err, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	if err := r.Render(rec, 200, "error", "wrong data type"); err == nil || rec.Body.Len() != 0 {
		t.Fatalf("template execution failure must not emit a partial page: err=%v body=%q", err, rec.Body.String())
	}
}
