package api_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
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
func TestAClosedPostureOpensTheSocketOnItsQueryToken(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.API.Auth.AllowAnonymousRead = false
	b.API.Auth.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	a := newApp(t, api.Options{Bootstrap: &b, QueueBackend: "jetstream"})
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"

	// Without a credential the closed posture refuses the handshake.
	if conn, _, err := websocket.Dial(t.Context(), base, nil); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a closed posture opened an unauthenticated socket")
	}
	// A wrong token in the query is refused too.
	if conn, _, err := websocket.Dial(t.Context(), base+"?token=wrong", nil); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a closed posture opened a socket on a wrong token")
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
