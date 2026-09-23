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
//     on a WebSocket constructor, so this is the only channel it has. Nothing
//     mints a cookie yet, so removing it would take the dashboard off the air
//     entirely — which is why the per-frame credential could go with this work
//     and the handshake one could not.
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
