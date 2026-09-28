package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE SOCKET OPENS ON A BEARER IN ITS HEADER AND ON NOTHING IN ITS URL,
// through the whole app: the guard middleware and then the stream handler, in
// that order.
//
// The socket used to take `?token=`, because a browser cannot set a header on
// a WebSocket constructor. The dashboard's handshake carries its session
// cookie now ([TestACookieAuthenticatesTheHandshake]) and a script sets the
// header, so the query is read nowhere: a URL is written into every proxy's
// access log, and the one route that paid that price no longer has a reason
// to. A valid token there is refused exactly like a missing one — on the
// handshake and on the plain GET the dashboard re-asks with.
//
// Control: the same token in the header opens the socket and is sent the
// snapshot, so the refusals are about where the token was presented.
func TestTheSocketOpensOnAHeaderAndNeverOnItsURL(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, QueueBackend: "jetstream"})
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"

	for _, target := range []string{base, base + "?token=secret"} {
		if conn, _, err := websocket.Dial(t.Context(), target, nil); err == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
			t.Fatalf("a socket opened at %s with no bearer in its header", target)
		}
	}

	conn, _, err := websocket.Dial(t.Context(), base,
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer secret"}}})
	if err != nil {
		t.Fatalf("a handshake carrying the token in its header was refused: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var first map[string]any
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if first["kind"] != stream.KindSnapshot {
		t.Fatalf("first frame = %v, want the snapshot", first["kind"])
	}
}

// THE 401/426 PAIRING ON A PLAIN GET, which the dashboard's socket depends on.
//
// A browser is told nothing about why a handshake failed — no status, and no
// close code, because a connection that never opened sends no close frame —
// so the client re-asks the same path over plain HTTP to tell "nobody is
// signed in" (401, and it sends the reader to sign in) from "the engine is
// down" (a throw, and it keeps reconnecting). An accepted credential stops
// one line short of the upgrade with 426. Collapse the two and a reader whose
// session ended sees "retrying" for ever.
func TestTheSocketProbeTellsARefusalFromAnOutage(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b})

	for _, tc := range []struct {
		name   string
		header string
		query  string
		want   int
	}{
		{"no credential at all", "", "", http.StatusUnauthorized},
		{"a refused bearer", "Bearer wrong", "", http.StatusUnauthorized},
		{"a valid token in the URL, read by nobody", "", "token=secret", http.StatusUnauthorized},
		{"an accepted bearer", "Bearer secret", "", http.StatusUpgradeRequired},
	} {
		req := httptest.NewRequest(http.MethodGet, "/ws/stream", nil)
		req.URL.RawQuery = tc.query
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("the probe with %s answered %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

// A SESSION COOKIE AUTHENTICATES THE SOCKET'S HANDSHAKE.
//
// A browser attaches its cookie to a WebSocket handshake as it does to any
// request to its origin, and that is the dashboard's whole credential there:
// it cannot set a header on a WebSocket constructor, and a token in the URL
// is read by nobody. Through the whole app, the way a browser arrives: the probe the
// dashboard re-asks over plain HTTP answers 426 for a live session and 401 for
// one this node has applied the end of, and a real handshake carrying only the
// cookie opens and is sent the snapshot.
//
// Control: the same cookie against an app with no session arm is 401, so the
// 426 above is the cookie's doing and nothing else's.
func TestACookieAuthenticatesTheHandshake(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	b.API.ExternalURL = "http://127.0.0.1:8080"
	cookies := newCookieArm(t, &b)
	live := cookies.person(t, []iam.Grant{iam.GrantStateRead}, clock)
	over := cookies.ended(t)
	a := newApp(t, api.Options{Bootstrap: &b, Sessions: cookies.arm})
	without := newApp(t, api.Options{Bootstrap: &b})

	probe := func(app *api.App, cookie *http.Cookie) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/ws/stream", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := probe(a, live); got != http.StatusUpgradeRequired {
		t.Errorf("the probe with a live session's cookie answered %d, want 426", got)
	}
	if got := probe(a, over); got != http.StatusUnauthorized {
		t.Errorf("the probe with an ended session's cookie answered %d, want 401", got)
	}
	if got := probe(without, live); got != http.StatusUnauthorized {
		t.Fatalf("the control — the same cookie on an app with no session arm — "+
			"answered %d, want 401; the 426 above is not the cookie's", got)
	}

	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	// WITH THE ORIGIN A BROWSER SENDS on every handshake: the socket is
	// judged by the writes' cross-site rule, which refuses a cookie that
	// arrives with no Origin at all.
	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/stream",
		&websocket.DialOptions{HTTPHeader: http.Header{
			"Cookie": {live.String()},
			"Origin": {b.API.ExternalURL},
		}})
	if err != nil {
		t.Fatalf("a handshake carrying a live session's cookie was refused: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var first map[string]any
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if first["kind"] != stream.KindSnapshot {
		t.Errorf("first frame = %v, want the snapshot", first["kind"])
	}
}
