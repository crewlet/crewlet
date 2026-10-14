package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chat"
)

// THE LIVE CHANNEL ACROSS A FLEET: what one node's hub tells a socket attached
// to another. The hub's own suite proves each half against an in-process
// scatter; what only a running pair of nodes can show is that every node runs
// one — `crewlet run` builds it through [api.NewEngineWithChat] whatever the
// node's roles — and that the broker carries its ephemeral subjects between
// them.

// focusOn tells a socket's hub which room its tab is looking at.
func focusOn(t *testing.T, conn *websocket.Conn, channelID string) {
	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"kind": stream.ChatFocusKind, "channel_id": channelID, "typing": false,
	})
	if err != nil {
		t.Fatalf("encode the focus frame: %v", err)
	}
	if err := conn.Write(t.Context(), websocket.MessageText, frame); err != nil {
		t.Fatalf("send the focus frame: %v", err)
	}
}

// awaitFrame reads past every other frame until one of kind satisfies match,
// failing after [waitBudget] with what it was waiting for.
func awaitFrame(t *testing.T, conn *websocket.Conn, kind stream.Kind, what string,
	match func(data json.RawMessage) bool) {

	t.Helper()
	deadline, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()
	for {
		_, raw, err := conn.Read(deadline)
		if err != nil {
			t.Fatalf("waiting for %s: %v", what, err)
		}
		var env struct {
			Kind stream.Kind     `json:"kind"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Kind != kind {
			continue
		}
		if match(env.Data) {
			return
		}
	}
}

// A WORKING INDICATOR RAISED ON A NODE THAT SERVES NO API SHOWS IN A ROOM
// WATCHED ON ANOTHER.
//
// The node running a seat's turn is rarely the node a browser is attached to:
// here it is the pair's stateless member, which holds no data and binds no
// API, and the seat pinned to it raises its indicator in that node's hub. The
// room is open on the data member. What reaches the tab is that member's
// presence probe, answered by the stateless member's hub over the broker —
// the hub that used to be started only by the API, and so never ran on a node
// like this one.
//
// THE SCATTER ALONE: the indicator is raised on the hub by hand, so this
// certifies the hub-to-hub half and nothing of the turn. That a turn raises it
// — the engine's [notify.StatusDriver] over the stateless member's hub, under
// `chat.native.typing_status` — is
// [TestASeatOnAStatelessNodeAnswersInARoomTheFleetHolds]'s, which holds a real
// turn until the same tab has been shown it. This case stays because it fails
// on the scatter alone, where that one fails on either half.
func TestAnIndicatorRaisedOnANodeWithNoAPIShowsInARoomOnAnother(t *testing.T) {
	t.Parallel()
	p := startStatelessPairSpec(t, pairSpec{company: peopleDoc, boot: peopleTokens})
	waitFor(t, "the data member's native backends to hydrate", hydrated(t, p.data.engine))

	made := openRoomAs(t, p.data, "alice-token", map[string]any{
		"name": "standup", "kind": string(chat.KindPublic),
	})
	if !p.agent.chat.Hub().SetStatus(t.Context(), "ceo", made.Channel, "", "reading the room") {
		t.Fatal("the stateless member's hub refused the indicator")
	}

	conn := dialAs(t, p.data, "alice-token")
	focusOn(t, conn, made.Channel)
	awaitFrame(t, conn, stream.KindChatPresence, "ceo working in #"+made.Name,
		func(data json.RawMessage) bool {
			var frame stream.ChatPresenceFrame
			if json.Unmarshal(data, &frame) != nil {
				return false
			}
			return slices.ContainsFunc(frame.Rooms[made.Channel].Working,
				func(w stream.ChatWorking) bool { return w.Handle == "ceo" })
		})
}

// A PERSON'S READ STATE MOVED THROUGH ONE NODE REACHES THEIR SOCKET ON ANOTHER.
//
// A cursor is on no log, so no applier anywhere will ever announce it: the
// node that took the write tells its peers on the person subject, and each
// delivers to that person's sockets of its own. Here the write is
// `mark_chat_read` through the operator surface built over the pair's
// stateless member — [api.NewEngineOperator] over that node's engine and its
// own hub, exactly as an ingress node builds its own, driven through the act
// transport's handler — and the person's other tab is attached to the data
// member, which only that fan-out can tell.
//
// THE HANDLER RATHER THAN A LISTENER, because a node without data may not
// take the ingress role (Tier A refuses it), so in production two tabs on two
// nodes are two data members. What crosses here is what crosses there: one
// node's hub scattering to another's over the broker. The data member holds
// the socket, and the write is made anywhere else.
func TestAPersonsReadStateMovedThroughOneNodeReachesTheirSocketOnAnother(t *testing.T) {
	t.Parallel()
	p := startStatelessPairSpec(t, pairSpec{company: peopleDoc, boot: peopleTokens})
	waitFor(t, "the data member's native backends to hydrate", hydrated(t, p.data.engine))

	made := openRoomAs(t, p.data, "alice-token", map[string]any{
		"name": "deploys", "kind": string(chat.KindPublic),
	})
	conn := dialAs(t, p.data, "alice-token")
	// REGISTERED BEFORE IT IS PUSHED TO: a person's push is news for the
	// sockets attached when it lands and nobody else, and the socket joins
	// its hub before the snapshot is sent — so the snapshot is the proof.
	awaitFrame(t, conn, stream.KindSnapshot, "the socket's snapshot",
		func(json.RawMessage) bool { return true })

	surface, err := api.NewEngineOperator(p.agent.engine, p.agent.chat.Hub())
	if err != nil {
		t.Fatalf("the stateless member's operator surface: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /operator/act/{tool}", surface.ActHandler())
	body, err := json.Marshal(map[string]any{
		"request_id": uuid.Must(uuid.NewV7()).String(),
		"args":       map[string]any{"mute": []any{made.Channel}},
	})
	if err != nil {
		t.Fatalf("encode the act request: %v", err)
	}
	// AS THE GUARD ADMITS ALICE'S TOKEN: the operator on the request.
	req := httptest.NewRequestWithContext(auth.WithOperator(t.Context(), "alice"),
		http.MethodPost, "/operator/act/"+chat.MarkChatReadTool, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mark_chat_read through the stateless member answered %d: %s",
			rec.Code, rec.Body)
	}

	awaitFrame(t, conn, stream.KindChatCursorMoved, "alice's mute of #"+made.Name,
		func(data json.RawMessage) bool {
			var state struct {
				Muted []string `json:"muted"`
			}
			return json.Unmarshal(data, &state) == nil && slices.Contains(state.Muted, made.Channel)
		})
}
