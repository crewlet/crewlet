package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// A REFUSED COOKIE IS DELETED, AND NOTHING ELSE IS ASKED OF THE BROWSER.
//
// The guard refuses an ended, expired or malformed session cookie on every
// route the browser presents it to: the dashboard's document, each of its
// scripts, /health, the socket's handshake, /auth/session and every guarded
// route. Each answer deletes the cookie, and none carries Clear-Site-Data:
// Chrome holds a response that does until it has scanned this origin's whole
// HTTP cache, so a tab presenting an expired cookie never finished loading.
//
// Mutation: send Clear-Site-Data from session.End again and every row is red;
// drop the deletion from the guard's refusal and every row is red.
func TestARefusedCookieIsDeletedWithoutEmptyingTheBrowsersCache(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	authorize(&b, config.APIToken{ID: "fixture", Token: fixtureToken})
	b.API.ExternalURL = "http://127.0.0.1:8080"
	cookies := newCookieArm(t, &b)
	a := newApp(t, api.Options{Bootstrap: &b, Sessions: cookies.arm, Assets: tree()})

	refused := map[string]*http.Cookie{
		"an ended session": cookies.ended(t),
		// Past its absolute deadline, which mintAt sets eight hours on.
		"an expired session": cookies.mintAt(t, clock.Add(-9*time.Hour)).cookie,
		"a malformed cookie": {Name: cookies.name, Value: "not-a-bearer"},
	}
	handshake := func(r *http.Request) {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		r.Header.Set("Origin", b.API.ExternalURL)
	}
	routes := []struct {
		name, path string
		shape      func(*http.Request)
		// status is what the route answers a request holding no session,
		// zero where any answer will do.
		status int
	}{
		{"the root", "/", nil, http.StatusFound},
		{"the dashboard's document", auth.PathDashboard, nil, http.StatusOK},
		{"a dashboard script", hashedJS, nil, http.StatusOK},
		{"/health", "/health", nil, 0},
		{"/auth/session", auth.PathAuthSession, nil, http.StatusUnauthorized},
		{"the socket's handshake", auth.SocketPath, handshake, http.StatusUnauthorized},
		{"a guarded route", "/agents", nil, http.StatusUnauthorized},
	}
	for what, cookie := range refused {
		for _, route := range routes {
			r := httptest.NewRequest(http.MethodGet, route.path, nil)
			r.AddCookie(cookie)
			if route.shape != nil {
				route.shape(r)
			}
			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, r)
			if route.status != 0 && rec.Code != route.status {
				t.Errorf("%s on %s answered %d, want %d", what, route.name, rec.Code, route.status)
			}
			if v := rec.Header().Values("Clear-Site-Data"); len(v) != 0 {
				t.Errorf("%s on %s carried Clear-Site-Data %v: Chrome holds the response "+
					"until it has emptied the whole cache", what, route.name, v)
			}
			var deleted bool
			for _, c := range rec.Result().Cookies() {
				if c.Name == session.CookieBaseName && c.MaxAge < 0 {
					deleted = true
				}
			}
			if !deleted {
				t.Errorf("%s on %s did not delete the cookie (cookies %v), so the browser "+
					"presents it again on every request", what, route.name, rec.Result().Cookies())
			}
		}
	}
}
