package e2e

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
)

// THE LIVE CHANNEL ACROSS A FLEET: what one node's hub tells a socket attached
// to another. The hub's own suite proves each half against an in-process
// scatter; what only a running pair of nodes can show is that every node runs
// one — `crewlet run` builds it through [api.NewEngineChatLive] whatever the
// node's roles, and so does this harness ([newEngine]) — that the broker
// carries the presence scatter between them, and that a person's read state,
// which no log carries, reaches their tab from the coordination record
// whichever node wrote it.

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
// `chat.chatlet.typing_status` — is
// [TestASeatOnAStatelessNodeAnswersInARoomTheFleetHolds]'s, which holds a real
// turn until the same tab has been shown it. This case stays because it fails
// on the scatter alone, where that one fails on either half.
func TestAnIndicatorRaisedOnANodeWithNoAPIShowsInARoomOnAnother(t *testing.T) {
	t.Parallel()
	p := startStatelessPairSpec(t, pairSpec{company: peopleDoc, dataBoot: withSignIn})
	waitFor(t, "the data member's native backends to hydrate", hydrated(t, p.data.engine))
	alices := signedIn(t, p.data, alice)

	made := openRoomAs(t, p.data, alices, map[string]any{
		"channel": "standup", "kind": string(chatlet.KindPublic),
	})
	if !p.agent.chat.Hub().SetStatus(t.Context(), "ceo", made.Channel, "", "reading the room") {
		t.Fatal("the stateless member's hub refused the indicator")
	}

	conn := dialAs(t, p.data, alices)
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
// A cursor is on no log, so no applier anywhere will ever announce it. It is a
// coordination record, and every node serving sockets WATCHES those records
// and pushes a changed one to that seat's own sockets there — the push derived
// from the durable record, never told to a node by the node that took the
// write. So a flush through ANY node reaches every tab its person has open,
// on whichever node each is attached to. Here the write is made through the
// pair's stateless member and the tab is on the data member, which only the
// record crossing between them and the data member's own watch can tell.
//
// THE COORDINATION WRITE ITSELF, the one `mark_chat_read` makes
// ([coord.ChatReads.AdvanceChatRead]), on the stateless member's own
// connection to the fleet's store — not a route, because a node without
// `data` serves no API at all (Tier A refuses it `ingress`), so in production
// two tabs on two nodes are two data members. What crosses here is what
// crosses there: one node's write to the coordination store, and another
// node's watch of it. That the tool writes exactly this record is the
// builtin's own suite's subject.
func TestAPersonsReadStateMovedThroughOneNodeReachesTheirSocketOnAnother(t *testing.T) {
	t.Parallel()
	p := startStatelessPairSpec(t, pairSpec{company: peopleDoc, dataBoot: withSignIn})
	waitFor(t, "the data member's native backends to hydrate", hydrated(t, p.data.engine))
	alices := signedIn(t, p.data, alice)

	made := openRoomAs(t, p.data, alices, map[string]any{
		"channel": "deploys", "kind": string(chatlet.KindPublic),
	})
	conn := dialAs(t, p.data, alices)
	// REGISTERED BEFORE IT IS PUSHED TO: a person's push is news for the
	// sockets attached when it lands and nobody else, and the socket joins
	// its hub before the snapshot is sent — so the snapshot is the proof.
	awaitFrame(t, conn, stream.KindSnapshot, "the socket's snapshot",
		func(json.RawMessage) bool { return true })

	// ALICE'S SEAT'S RECORD, as `mark_chat_read` would advance it from a tab
	// on another node: the room muted.
	if _, err := p.agent.engine.Backends().Fleet.AdvanceChatRead(t.Context(), alice.seat,
		coord.ChatReadDelta{Mute: []string{made.Channel}, At: time.Now().UTC()}); err != nil {
		t.Fatalf("muting #%s through the stateless member: %v", made.Name, err)
	}

	awaitFrame(t, conn, stream.KindChatCursorMoved, "alice's mute of #"+made.Name,
		func(data json.RawMessage) bool {
			var state coord.PersonChatRead
			return json.Unmarshal(data, &state) == nil && state.Handle == alice.seat &&
				slices.Contains(state.Muted, made.Channel)
		})
}
