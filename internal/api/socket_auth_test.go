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

// THE SOCKET IS OPEN AND EVERY QUESTION ON IT IS JUDGED AS ITS KEY, through the
// whole app: the guard middleware, then the stream handler, then the registry.
//
// The stream package's own suite dials the handler directly and so proves only
// what the HANDLER does with a key. The middleware in front of it once read the
// header alone and answered 401 before the handler ran, so the dashboard could
// not connect with a valid key on the posture whose point is that the key is
// required. These go through api.App the way a browser does.

// socketApp is a node closed to anybody without a key, with one admin key
// (`founder`, "secret") and one member key (`ada`, "member-secret").
func socketApp(t *testing.T) (*api.App, string) {
	t.Helper()
	b := closedPosture()
	b.API.Auth.Tokens = append(b.API.Auth.Tokens,
		config.APIToken{ID: "ada", Role: config.RoleMember, Token: "member-secret"})
	a := newApp(t, api.Options{Bootstrap: &b, QueueBackend: "jetstream"})
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return a, "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"
}

// socketConn is one open socket past its snapshot.
type socketConn struct {
	t    *testing.T
	conn *websocket.Conn
}

// dialSocket opens a socket at url and reads its snapshot, failing the case if
// either does not happen.
func dialSocket(t *testing.T, url string) socketConn {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("the handshake at %s was refused: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	s := socketConn{t: t, conn: conn}
	if first := s.read(); first["kind"] != string(stream.KindSnapshot) {
		t.Fatalf("first frame = %v, want the snapshot", first["kind"])
	}
	return s
}

func (s socketConn) read() map[string]any {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := s.conn.Read(ctx)
	if err != nil {
		s.t.Fatalf("read: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		s.t.Fatalf("decode %s: %v", raw, err)
	}
	return got
}

// ask sends one question, with a frame key when one is named, and returns its
// answer or its refusal.
func (s socketConn) ask(what, frameKey string) map[string]any {
	s.t.Helper()
	frame := map[string]any{"kind": "query", "id": 7, "what": what}
	if frameKey != "" {
		frame["token"] = frameKey
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := s.conn.Write(s.t.Context(), websocket.MessageText, raw); err != nil {
		s.t.Fatalf("write: %v", err)
	}
	for range 20 {
		if got := s.read(); got["kind"] == "result" || got["kind"] == "error" {
			return got
		}
	}
	s.t.Fatalf("%s was never answered", what)
	return nil
}

// A KEY THE NODE REJECTS IS REFUSED AT THE HANDSHAKE, and a valid one in the
// query — the only place a browser can put it — opens the socket as that key.
func TestTheSocketOpensOnItsQueryKeyAndRefusesAWrongOne(t *testing.T) {
	t.Parallel()
	_, base := socketApp(t)

	if conn, _, err := websocket.Dial(t.Context(), base+"?token=wrong", nil); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a socket opened on a key this node rejects")
	}
	admin := dialSocket(t, base+"?token=secret")
	if got := admin.ask("access", ""); got["kind"] != "result" {
		t.Errorf("an admin socket asking `access` = %v, want the answer", got)
	}
}

// THE SOCKET IS OPEN TO A CALLER WITH NO KEY, even under `anonymous: none`,
// because what a socket is sent is judged per question: the sign-in page asks
// `viewer` on it before anybody has signed in. What that caller may NOT ask is
// refused as any other keyless question is.
func TestAKeylessSocketOpensAndIsRefusedWhatItsReachDoesNotCover(t *testing.T) {
	t.Parallel()
	_, base := socketApp(t)
	nobody := dialSocket(t, base)
	if got := nobody.ask("viewer", ""); got["kind"] != "result" {
		t.Errorf("a keyless socket asking `viewer` = %v, want the answer", got)
	}
	if got := nobody.ask("access", ""); got["error"] != stream.CodeUnauthorized {
		t.Errorf("a keyless socket asking `access` = %v, want unauthorized", got)
	}
}

// A MEMBER'S SOCKET IS FORBIDDEN AN ADMIN'S QUESTION — not unauthorized, since
// the key was accepted and signing in again would change nothing.
func TestAMemberSocketIsForbiddenAnAdminsQuestion(t *testing.T) {
	t.Parallel()
	_, base := socketApp(t)
	member := dialSocket(t, base+"?token=member-secret")
	if got := member.ask("access", ""); got["error"] != stream.CodeForbidden {
		t.Errorf("a member socket asking `access` = %v, want forbidden", got)
	}
}

// A FRAME'S OWN KEY UPGRADES THAT ONE QUESTION, and only to what the key
// itself reaches: an admin key on a keyless socket is answered, a member key
// is forbidden, and a key the node rejects asks as the socket would.
func TestAFrameKeyAsksOneQuestionAsThatKey(t *testing.T) {
	t.Parallel()
	_, base := socketApp(t)
	nobody := dialSocket(t, base)
	if got := nobody.ask("access", "secret"); got["kind"] != "result" {
		t.Errorf("an admin frame key on a keyless socket = %v, want the answer", got)
	}
	if got := nobody.ask("access", "member-secret"); got["error"] != stream.CodeForbidden {
		t.Errorf("a member frame key = %v, want forbidden", got)
	}
	if got := nobody.ask("access", "wrong"); got["error"] != stream.CodeUnauthorized {
		t.Errorf("a rejected frame key = %v, want unauthorized as the socket", got)
	}
	// And the upgrade was the frame's alone: the socket is still nobody.
	if got := nobody.ask("access", ""); got["error"] != stream.CodeUnauthorized {
		t.Errorf("the socket after a keyed frame = %v, want unauthorized", got)
	}
}
