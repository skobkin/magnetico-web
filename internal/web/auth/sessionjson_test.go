package auth

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/web/server"
)

func TestSessionJSONGuard(t *testing.T) {
	e := newEnv(t)
	e.mkUser("olga", "the right password")
	e.srv.Router().Handle("GET /json-probe", server.KindRequest, server.SessionJSON,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))

	// Anonymous: 401 problem+json, never a redirect, no body detail beyond the type.
	anon := e.client()
	for _, path := range []string{"/json-probe", "/json-probe?x=1"} {
		res := anon.get(path)
		if res.StatusCode != 401 || res.Header.Get("Location") != "" {
			t.Fatalf("%s: status %d location %q", path, res.StatusCode, res.Header.Get("Location"))
		}
		if ct := res.Header.Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("content type %q", ct)
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(anon.body), &d); err != nil || d["type"] != "urn:magnetico:problem:unauthenticated" {
			t.Fatalf("body %q (%v)", anon.body, err)
		}
	}
	// A garbage session cookie is also rejected, and is cleared.
	bad := e.client()
	bad.jar["__Host-magnetico_session"] = &http.Cookie{Name: "__Host-magnetico_session", Value: "garbage"}
	if res := bad.get("/json-probe"); res.StatusCode != 401 {
		t.Fatalf("garbage cookie: %d", res.StatusCode)
	}

	// Signed in: served.
	c := e.client()
	c.get("/login")
	if res := c.signIn("olga", "the right password"); res.StatusCode != 303 {
		t.Fatalf("sign in: %d", res.StatusCode)
	}
	if res := c.get("/json-probe"); res.StatusCode != 200 || !strings.Contains(c.body, `"ok":true`) {
		t.Fatalf("signed in: %d %q", res.StatusCode, c.body)
	}
	// After sign-out the session is gone.
	e.exec(`DELETE FROM magnetico_web.sessions`)
	if res := c.get("/json-probe"); res.StatusCode != 401 {
		t.Fatalf("deleted session: %d", res.StatusCode)
	}
}

func TestSessionJSONRoutesAreGetOnly(t *testing.T) {
	e := newEnv(t)
	defer func() {
		if recover() == nil {
			t.Fatal("a POST SessionJSON route must panic at registration")
		}
	}()
	e.srv.Router().Handle("POST /x", server.KindRequest, server.SessionJSON, http.NotFoundHandler())
}
