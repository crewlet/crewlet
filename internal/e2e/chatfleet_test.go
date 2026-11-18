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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/presence"
)

// THE LIVE CHANNEL ACROSS A FLEET: what one node's hub tells a socket attached
// to another. The chat hub's own suite proves each half against an in-process
// presence hub; what only a running pair of nodes can show is that every node
// runs one — `crewlet run` builds it through [api.NewEngineChatLive] whatever
// the node's roles, and so does this harness ([newEngine]) — that the broker
// carries the presence hubs' SEALED frames between them (internal/presence,
// ADR-0038), and that a person's read state, which no log carries, reaches
// their tab from the coordination record whichever node wrote it.

// focusOn tells a socket's hub which room its tab is looking at: the chat
// room's `presence` frame, viewing, which joins the socket to that room's
// presence as the room's screen does when it opens.
func focusOn(t *testing.T, conn *websocket.Conn, channelID string) {
	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"kind": "presence", "room": string(presence.RoomChat), "channel_id": channelID,
		"activity": string(presence.Viewing),
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
// The seat's turn runs on the stateless member (no data, no API) and raises its
// indicator in that node's chat hub. The data member's presence hub asks its
// peers as its first socket joins the room; the stateless member's hub answers
// with a frame SEALED under the fleet keyring.
//
// THE PRESENCE HUBS ALONE: the indicator is raised by hand. That a turn raises
// it ([notify.StatusDriver]) is
// [TestASeatOnAStatelessNodeAnswersInARoomTheFleetHolds]'s, which fails on
// either half; this fails on the hubs alone.
func TestAnIndicatorRaisedOnANodeWithNoAPIShowsInARoomOnAnother(t *testing.T) {
	t.Parallel()
	p := startStatelessPairSpec(t, pairSpec{company: peopleDoc, dataBoot: withSignIn})
	waitFor(t, "the data member's native backends to hydrate", hydrated(t, p.data.engine))
	alices := signedIn(t, p.data, alice)

	made := openRoomAs(t, p.data, alices, map[string]any{
		"channel": "standup", "kind": string(chatlet.KindPublic),
	})
	if !p.agent.chat.Hub().SetStatus(t.Context(), notify.StatusPost{
		Handle: "ceo", Channel: made.Channel, Turn: "run-standup", Text: "reading the room",
	}) {
		t.Fatal("the stateless member's hub refused the indicator")
	}

	conn := dialAs(t, p.data, alices)
	focusOn(t, conn, made.Channel)
	awaitFrame(t, conn, stream.KindChatPresence, "ceo working in #"+made.Name,
		func(data json.RawMessage) bool {
			return showsWorking(data, made.Channel, "ceo", "reading the room")
		})
}

// showsWorking reports whether one `chat_presence` push shows a seat at work
// in a room: its agent member, editing, with the line the room shows beside
// it — or any line, where status is empty.
func showsWorking(data json.RawMessage, room, seat, status string) bool {
	var frame stream.ChatPresence
	if json.Unmarshal(data, &frame) != nil || frame.ChannelID != room {
		return false
	}
	return slices.ContainsFunc(frame.Upsert, func(m presence.Member) bool {
		return m.Seat == seat && m.Kind == iam.ActorAgent &&
			m.Activity == presence.Editing && (status == "" || m.Status == status)
	})
}

// A PERSON'S READ STATE MOVED THROUGH ONE NODE REACHES THEIR SOCKET ON ANOTHER.
//
// A cursor is on no log, so no applier announces it. It is a coordination
// record every node serving sockets WATCHES, pushing a changed one to that
// seat's own sockets there, never told by the node that took the write. Here
// the write goes through the stateless member, the tab is on the data member.
//
// THE COORDINATION WRITE ITSELF, the one `mark_chat_read` makes
// ([coord.ChatReads.AdvanceChatRead]), not a route: a node without `data`
// serves no API (Tier A refuses it `ingress`), so in production two tabs on two
// nodes are two data members. The builtin's suite covers the tool's record.
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
