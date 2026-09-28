package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/iam"
)

// A CLOSED POSTURE OPENS THE SOCKET ON ITS QUERY TOKEN, through the whole
// app: the guard middleware and then the stream handler, in that order.
//
// The stream package's own suite dials the handler directly and so proved
// only that the HANDLER accepted a query token. The middleware in front of it
// read the header alone, and under a closed posture answered 401 before the
// handler ran, so the dashboard could not connect with a valid token on the
// one posture whose point is that the token is required. This test goes
// through api.App the way a browser does.
func TestTheSocketOpensOnItsQueryToken(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, QueueBackend: "jetstream"})
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"

	// Without a credential the handshake is refused.
	if conn, _, err := websocket.Dial(t.Context(), base, nil); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a socket opened with no credential at all")
	}
	// A wrong token in the query is refused too.
	if conn, _, err := websocket.Dial(t.Context(), base+"?token=wrong", nil); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a socket opened on a wrong token")
	}

	conn, _, err := websocket.Dial(t.Context(), base+"?token=secret", nil)
	if err != nil {
		t.Fatalf("the dashboard's own handshake was refused: %v", err)
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

// THE SHIPPED BUNDLE'S OWN CREDENTIAL PATH STILL WORKS, asserted at THIS
// commit rather than left to the next person to notice.
//
// # Why this is a gate rather than a note
//
// `static/dashboard` is a COMMITTED build output, and the bundle in the tree
// is what a `go install`ed binary serves. So an auth change lands in Go and
// the client that has to keep working is a file nobody recompiled — a pairing
// that can only break silently, because the engine's own suite passes and the
// dashboard's own suite passes and neither runs the other.
//
// Two things the shipped client depends on, and both are load-bearing:
//
//   - THE HANDSHAKE CREDENTIAL RIDES `?token=`. A browser cannot set a header
//     on a WebSocket constructor, so a pasted token has no other channel, and
//     the bundle the tree ships still opens its socket that way: its sign-in
//     does not yet hand the socket a session cookie. The guard ALREADY accepts
//     one on the handshake ([TestACookieAuthenticatesTheHandshake]), which is
//     the precondition for retiring the query branch — but not the whole of it:
//     the branch goes when the shipped bundle stops sending `?token=`, which
//     is what the needle below would then report.
//   - THE 401/426 PAIRING ON A PLAIN GET. A browser is told nothing about why
//     a handshake failed — no status, and no close code, because a connection
//     that never opened sends no close frame — so the client re-asks over
//     plain HTTP to tell "your token is wrong" from "the engine is down".
//     Collapse the two and a reader holding a stale token sees "retrying" for
//     ever.
func TestTheShippedBundleStillAuthenticates(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b})

	// THE PROBE, both arms. A GET with no Upgrade header.
	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"a refused credential", "wrong", http.StatusUnauthorized},
		{"no credential at all", "", http.StatusUnauthorized},
		{"an accepted credential", "secret", http.StatusUpgradeRequired},
	} {
		req := httptest.NewRequest(http.MethodGet, "/ws/stream", nil)
		if tc.token != "" {
			req.URL.RawQuery = "token=" + tc.token
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("the probe with %s answered %d, want %d: the dashboard "+
				"cannot tell a wrong token from a stopped engine, and a reader "+
				"holding a stale one retries for ever", tc.name, rec.Code, tc.want)
		}
	}

	// AND THE BUNDLE IN THE TREE IS STILL THE ONE THIS IS ABOUT. Without
	// this the case above would go on passing while the committed client
	// had moved to a credential path nothing here serves.
	bundle := readShippedBundle(t)
	for _, needle := range []string{"token=", "426"} {
		if !strings.Contains(bundle, needle) {
			t.Errorf("the committed dashboard bundle no longer contains %q: "+
				"it has moved off the credential path this case asserts, so "+
				"this gate is certifying a client nobody ships", needle)
		}
	}
}

// readShippedBundle is every committed dashboard script, concatenated.
//
// THE BUILT BUNDLE rather than dashboard/src, because the built one is what
// the binary embeds and serves: a source file that says the right thing and a
// bundle that was never rebuilt is exactly the drift this reads past.
func readShippedBundle(t *testing.T) string {
	t.Helper()
	scripts, err := filepath.Glob("../../static/dashboard/assets/*.js")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	scripts = append(scripts, "../../static/dashboard/protocol.js")
	var all strings.Builder
	for _, name := range scripts {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		all.Write(raw)
	}
	// The control: a glob that matched nothing reads as a bundle with no
	// credential path, which is the failure this is meant to REPORT rather
	// than the one it is meant to be.
	if all.Len() < 100_000 {
		t.Fatalf("the committed bundle reads as %d bytes over %d files; this "+
			"case is looking in the wrong place", all.Len(), len(scripts))
	}
	return all.String()
}

// A SESSION COOKIE AUTHENTICATES THE SOCKET'S HANDSHAKE.
//
// A browser attaches its cookie to a WebSocket handshake as it does to any
// request to its origin, so a person signed in with a session cookie needs no
// token in the URL — the one channel a pasted token has, and one that lands in
// proxy logs. Through the whole app, the way a browser arrives: the probe the
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
	// AND THE ORIGIN A BROWSER SENDS on every WebSocket upgrade — this
	// deployment's own, the external URL above. The handshake is judged
	// by the same cross-site rule as a write, which refuses a
	// cookie-authenticated upgrade carrying none; without the header this
	// case dialled as no browser does and was refused 403 for it.
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
