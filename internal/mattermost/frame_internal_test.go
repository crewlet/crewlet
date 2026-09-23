package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// frameServer is a Mattermost websocket endpoint that sends one text message
// and then waits for the client to go away.
func frameServer(t *testing.T, message []byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if err := conn.Write(r.Context(), websocket.MessageText, message); err != nil {
			return
		}
		// Held open until the client closes, so what the client reads is
		// decided by the frame and not by the server hanging up.
		_, _, _ = conn.Read(r.Context())
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientOptions{URL: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// postedFrame is a `posted` event whose post message is n bytes, shaped the
// way Mattermost sends it: the post a JSON STRING inside the envelope.
func postedFrame(t *testing.T, n int) []byte {
	t.Helper()
	post, err := json.Marshal(map[string]any{
		"id": "post-1", "channel_id": "ch-1", "message": strings.Repeat("m", n),
	})
	if err != nil {
		t.Fatalf("encode post: %v", err)
	}
	frame, err := json.Marshal(map[string]any{
		"event": "posted", "data": map[string]any{"post": string(post)},
	})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	return frame
}

// A POST PAST THE LIBRARY'S DEFAULT IS READ, because a seat's socket sets its
// own ceiling. Left at coder/websocket's 32 KiB, every long post would close
// the socket and arrive only through a reconnect's replay.
func TestALongPostIsReadOffTheSocket(t *testing.T) {
	t.Parallel()
	const size = 1 << 20 // far past 32 KiB, inside maxFrameBytes
	frame := postedFrame(t, size)
	if len(frame) >= maxFrameBytes {
		t.Fatalf("the fixture is %d bytes, not inside the ceiling this case is about", len(frame))
	}
	c := frameServer(t, frame)

	socket, err := dialWebsocket(t.Context(), Seat{Handle: "swe"}, c)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })

	body, err := socket.Read(t.Context())
	if err != nil {
		t.Fatalf("a %d-byte frame was refused: %v", len(frame), err)
	}
	post, _ := body["post"].(map[string]any)
	if msg, _ := post["message"].(string); len(msg) != size {
		t.Errorf("the post's message arrived %d bytes, want the whole %d", len(msg), size)
	}
}

// A FRAME PAST THE CEILING IS REFUSED, NEVER CUT: the read fails, naming why,
// rather than handing back a prefix — which is what lets the reconnect's
// replay be the route that recovers the post. See [maxFrameBytes].
func TestAFramePastTheCeilingIsRefusedRatherThanCut(t *testing.T) {
	t.Parallel()
	// A real post, so a socket that read it instead of refusing it answers
	// at once rather than waiting on the next frame.
	frame := postedFrame(t, maxFrameBytes)
	if len(frame) <= maxFrameBytes {
		t.Fatalf("the fixture is %d bytes, not past the ceiling this case is about", len(frame))
	}
	c := frameServer(t, frame)

	socket, err := dialWebsocket(t.Context(), Seat{Handle: "swe"}, c)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	body, err := socket.Read(ctx)
	if err == nil {
		t.Fatalf("a frame past the ceiling was read as %v", body)
	}
	if !errors.Is(err, websocket.ErrMessageTooBig) {
		t.Errorf("the read failed with %v, not the refusal that names the ceiling", err)
	}
}
