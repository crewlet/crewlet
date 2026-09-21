package stream

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// mustJSON encodes a presence payload for the wire.
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

// mustDecode reads one back.
func mustDecode(t *testing.T, raw []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// THE CHAT ARM'S TESTS ARE INTERNAL, because what they protect is a decision
// taken between a committed record and a socket's own queue — the watcher
// registry, the visibility filter and the presence gather — none of which has
// an exported surface to drive it through. The alternative is a real
// WebSocket per case to assert that a private room did NOT arrive, which is a
// test that passes for the wrong reason whenever the socket is slow.

// permissiveRooms answers every room it holds, with this viewer's membership,
// AND DECIDES NOTHING.
//
// That is the whole point of the fixture. The real reader refuses a room the
// viewer may not read as `not found`, so a stub that did the same would make
// every case below pass with the filter deleted — the thing under test is
// what [ChatHub.maySee] does with a room it was handed.
type permissiveRooms struct {
	rooms   map[string]chat.Channel
	members map[string][]string
	err     error
}

func (p permissiveRooms) Channel(_ context.Context, viewer, channelID string,
	_ statelog.Freshness) (chat.ChannelDetail, error) {

	if p.err != nil {
		return chat.ChannelDetail{}, p.err
	}
	room, held := p.rooms[channelID]
	if !held {
		return chat.ChannelDetail{}, errors.New("no such room")
	}
	return chat.ChannelDetail{
		Channel: room,
		Member:  slices.Contains(p.members[channelID], viewer),
	}, nil
}

// twoRooms is a private room alice is in and bob is not, plus a public one.
func twoRooms() permissiveRooms {
	return permissiveRooms{
		rooms: map[string]chat.Channel{
			"private-1": {ID: "private-1", Kind: chat.KindPrivate},
			"public-1":  {ID: "public-1", Kind: chat.KindPublic},
		},
		members: map[string][]string{"private-1": {"alice"}},
	}
}

// hubFor builds a hub whose credentials resolve as seats says.
func hubFor(t *testing.T, rooms ChatRooms, seats map[string]string) *ChatHub {
	t.Helper()
	return NewChatHub(ChatHubOptions{
		Rooms:  func() ChatRooms { return rooms },
		Viewer: func(operatorID string) string { return seats[operatorID] },
		NodeID: "node-a",
		Now:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
}

// frames drains everything queued on a client without blocking.
func frames(c *Client) []Envelope {
	var out []Envelope
	for {
		select {
		case env := <-c.Out():
			out = append(out, env)
		default:
			return out
		}
	}
}

// workingIn is the room's working set once it has n entries, and it is a WAIT
// rather than a read because the raise is asynchronous by design:
// [notify.StatusDriver.Begin] states that "THE RAISE IS THE GOROUTINE'S FIRST
// ACT, not this function's last", so that a turn never blocks on a chat
// backend to start working. A test that read the room on the line after Begin
// would be asserting a goroutine scheduling order nothing promises — and it
// lost that race, reporting an empty room.
//
// BOUNDED AND LOUD, in [notify]'s own `shownAtLeast` idiom: two seconds is
// long enough that no scheduler misses it and short enough that a raise which
// genuinely never happens fails the suite rather than hanging it, and the
// failure prints what the room DID report so a real regression is
// distinguishable from a slow machine.
//
// Only the raise needs this. [notify.StatusSession.End] cancels the loop,
// waits on its done channel and clears inline, so a read straight after End is
// already ordered.
func workingIn(t *testing.T, h *ChatHub, room string, n int) []ChatWorking {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := h.presenceOf(nil, []string{room}, h.now())[room].Working
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s reports %v as working after two seconds, want %d",
				room, got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// kinds is what a client was sent, in order.
func kinds(c *Client) []string {
	var out []string
	for _, env := range frames(c) {
		out = append(out, env.Kind)
	}
	return out
}

func post(channelID string, seq int64) chat.Applied {
	return chat.Applied{
		Position:   statelog.Position{Stream: topics.ChatLogStream, Generation: 1, Seq: uint64(seq)},
		Subject:    chat.MessageSubject(channelID),
		Op:         chat.OpPost,
		OpID:       "op-1",
		ChannelID:  channelID,
		MessageID:  "m-1",
		ChannelSeq: seq,
		Actor:      "alice",
		ActorKind:  chat.AuthorHuman,
		At:         time.Unix(1_700_000_000, 0).UTC(),
	}
}

// A FRAME FROM A PRIVATE ROOM NEVER REACHES SOMEBODY WHO IS NOT IN IT.
//
// This is the one failure this whole arm exists to prevent. The hub's fan-out
// is over every open socket, so without the per-recipient filter a message in
// a private room is delivered to whoever happened to have a tab open — and
// the excerpt on the frame means the words themselves go with it.
func TestAChatFrameForAPrivateRoomNeverReachesANonMember(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice", "tok-b": "bob"})
	inside, outside := NewClient(), NewClient()
	if !h.join(inside, "tok-a") || !h.join(outside, "tok-b") {
		t.Fatal("a bound credential was not registered as a watcher")
	}

	h.fan(t.Context(), []chat.Applied{post("private-1", 7)})

	if got := kinds(inside); !slices.Equal(got, []string{KindChatPosted}) {
		t.Errorf("the member was sent %v, want one %s", got, KindChatPosted)
	}
	if got := kinds(outside); len(got) != 0 {
		t.Errorf("a private room's message reached a non-member as %v", got)
	}
}

// AND A PUBLIC ROOM REACHES EVERY SEAT, which is the other half of the same
// rule: the filter is [chat.Visible], and a company whose seats could not read
// its own open rooms would be one where the org chart hid the work.
//
// Without this case the filter could be "send nothing" and the case above
// would still pass.
func TestAChatFrameForAPublicRoomReachesEverySeat(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice", "tok-b": "bob"})
	joined, stranger := NewClient(), NewClient()
	h.join(joined, "tok-a")
	h.join(stranger, "tok-b")

	h.fan(t.Context(), []chat.Applied{post("public-1", 3)})

	for name, client := range map[string]*Client{"a member": joined, "a non-member": stranger} {
		got := frames(client)
		if len(got) != 1 {
			t.Fatalf("%s was sent %d frames for a public room", name, len(got))
		}
		change, ok := got[0].Data.(ChatChange)
		if !ok {
			t.Fatalf("%s was sent a %T rather than a change", name, got[0].Data)
		}
		if change.ChannelSeq != 3 || change.MessageID != "m-1" {
			t.Errorf("%s was sent seq %d of %q — the sequence is what lets a "+
				"client tell a frame it can apply from a hole it must refetch",
				name, change.ChannelSeq, change.MessageID)
		}
	}
}

// A CREDENTIAL BOUND TO NO SEAT WATCHES NOTHING, reads included.
//
// It is not registered and then filtered: an unbound caller has no membership
// anywhere, so a registry that held it would be one accidental change away
// from serving it every public room — a viewer the company never declared.
func TestASocketBoundToNoSeatWatchesNothing(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice"})
	unbound := NewClient()
	if h.join(unbound, "pipeline-token") {
		t.Fatal("a credential bound to no seat was registered as a watcher")
	}
	h.fan(t.Context(), []chat.Applied{post("public-1", 1)})
	if got := kinds(unbound); len(got) != 0 {
		t.Errorf("an unbound credential was sent %v", got)
	}
}

// A VISIBILITY DECISION THAT CANNOT BE MADE FAILS CLOSED.
//
// A store that will not answer is not permission to show a transcript. The
// screen repairs itself on its next fetch, which is what every dropped frame
// here relies on.
func TestAFrameWhoseVisibilityCannotBeDecidedIsWithheld(t *testing.T) {
	t.Parallel()
	broken := twoRooms()
	broken.err = errors.New("this node is behind the log")
	h := hubFor(t, broken, map[string]string{"tok-a": "alice"})
	client := NewClient()
	h.join(client, "tok-a")

	h.fan(t.Context(), []chat.Applied{post("public-1", 1)})

	if got := kinds(client); len(got) != 0 {
		t.Errorf("a frame went out on a read that failed: %v", got)
	}
}

// THE FRAMEWORK'S OWN RECORDS PUSH NOTHING.
//
// An eviction, a generation and a barrier change no room. A default arm that
// pushed them would put a frame with no case on every open socket, and the
// switch is the one place that can say so.
func TestTheFrameworksOwnRecordsPushNothing(t *testing.T) {
	t.Parallel()
	for _, op := range []chat.OpKind{chat.OpEviction, chat.OpGeneration, chat.OpBarrier} {
		if kind, push := chatPushKind(op); push {
			t.Errorf("%s would be pushed as %q", op, kind)
		}
	}
	// AND EVERY OP THAT IS ABOUT A CONVERSATION HAS ONE, so a gesture
	// added to the value layer is not silently invisible on every screen.
	for _, op := range []chat.OpKind{
		chat.OpCreate, chat.OpPatch, chat.OpMembers, chat.OpPost, chat.OpEdit,
		chat.OpDelete, chat.OpReact, chat.OpErase, chat.OpPrune,
	} {
		if _, push := chatPushKind(op); !push {
			t.Errorf("%s reaches no open screen", op)
		}
	}
}

// ---- presence ---------------------------------------------------------- //

// PRESENCE IS FILTERED BY THE SAME RULE AS A MESSAGE, IN BOTH DIRECTIONS.
//
// A tab may name any room it likes in a focus frame — it is saying where it is
// looking, not claiming a permission — so two things have to hold. It must
// learn nothing about a room it cannot open, and it must not APPEAR in that
// room to the people who can: a socket that could advertise itself into a
// private conversation by guessing its id would tell its members that a
// stranger was reading them.
func TestPresenceNeitherReachesNorNamesSomebodyOutsideTheRoom(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice", "tok-b": "bob"})
	inside, outside := NewClient(), NewClient()
	h.join(inside, "tok-a")
	h.join(outside, "tok-b")
	h.focus(inside, "private-1", true)
	h.focus(outside, "private-1", true)

	h.sweep(t.Context())

	if got := kinds(outside); len(got) != 0 {
		t.Errorf("a non-member was told about a private room's presence: %v", got)
	}
	got := frames(inside)
	if len(got) != 1 || got[0].Kind != KindChatPresence {
		t.Fatalf("the member was sent %v, want one %s", got, KindChatPresence)
	}
	frame, ok := got[0].Data.(ChatPresenceFrame)
	if !ok {
		t.Fatalf("the presence frame carries a %T", got[0].Data)
	}
	room := frame.Rooms["private-1"]
	if !slices.Equal(room.Viewing, []string{"alice"}) {
		t.Errorf("the room reports %v as viewing — somebody who cannot open a "+
			"room is not in it, whatever their tab says", room.Viewing)
	}
	if !slices.Equal(room.Typing, []string{"alice"}) {
		t.Errorf("the room reports %v as typing", room.Typing)
	}
}

// A PEER'S PROBE IS ANSWERED UNDER THE SAME RULE.
//
// The receiving node filters again at delivery, so this is not the only lock —
// but a node that answered with everybody focused on a room would put a
// stranger's handle on the wire, and the honest place to refuse is where the
// membership rows are.
func TestAPresenceProbeIsAnsweredWithOnlyThePeopleEntitledToBeInTheRoom(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice", "tok-b": "bob"})
	inside, outside := NewClient(), NewClient()
	h.join(inside, "tok-a")
	h.join(outside, "tok-b")
	h.focus(inside, "private-1", false)
	h.focus(outside, "private-1", false)

	raw, err := h.answer(t.Context(), mustJSON(t, chatPresenceRequest{
		Version: chatPresenceProtocol, Channels: []string{"private-1"},
	}))
	if err != nil {
		t.Fatalf("the probe was not answered: %v", err)
	}
	var reply chatPresenceReply
	mustDecode(t, raw, &reply)
	if got := reply.Rooms["private-1"].Viewing; !slices.Equal(got, []string{"alice"}) {
		t.Errorf("the answer names %v as viewing a private room", got)
	}
	if reply.Node != "node-a" {
		t.Errorf("the answer is unsigned (%q), so an asker cannot drop its own",
			reply.Node)
	}
}

// A PROBE THIS BUILD DOES NOT SPEAK IS ANSWERED WITH SILENCE, which the asker
// reads as a node with nobody in the room — the same fact as a node that is
// not running. Guessing at a shape nobody declared would render people into a
// room on the strength of it.
func TestAPresenceProbeFromAnotherProtocolIsNotAnswered(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice"})
	client := NewClient()
	h.join(client, "tok-a")
	h.focus(client, "public-1", false)

	if _, err := h.answer(t.Context(), mustJSON(t, chatPresenceRequest{
		Version: chatPresenceProtocol + 1, Channels: []string{"public-1"},
	})); err == nil {
		t.Fatal("a probe from a protocol this build does not speak was answered")
	}
}

// AN UNCHANGED VIEW COSTS NO FRAME.
//
// Presence is the one thing here evaluated on a timer rather than on an event,
// so without the fingerprint every open tab would receive a frame every
// interval for as long as it stayed open — on a socket whose queue drops its
// OLDEST envelope when it fills.
func TestAnUnchangedPresenceViewIsNotPushedAgain(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice"})
	client := NewClient()
	h.join(client, "tok-a")
	h.focus(client, "public-1", false)

	h.sweep(t.Context())
	if got := len(frames(client)); got != 1 {
		t.Fatalf("the first sweep sent %d frames", got)
	}
	h.sweep(t.Context())
	if got := frames(client); len(got) != 0 {
		t.Errorf("an unchanged view was pushed again: %v", got)
	}
	// AND A CHANGE IS. Without this the case above would pass for an
	// implementation that never pushed presence at all.
	h.SetStatus(t.Context(), "agent-swe", "public-1", "m-1", "reading the room")
	h.sweep(t.Context())
	if got := len(frames(client)); got != 1 {
		t.Errorf("a changed view sent %d frames", got)
	}
}

// A TYPING FLAG EXPIRES ON ITS OWN, for the tab that stops typing and says
// nothing. Closing a socket withdraws it at once; this is the other case.
func TestATypingFlagExpiresWithoutBeingReasserted(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0).UTC()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice"})
	h.now = func() time.Time { return at }
	client := NewClient()
	h.join(client, "tok-a")
	h.focus(client, "public-1", true)

	watcher := h.snapshot()
	if got := h.presenceOf(watcher, []string{"public-1"}, at); len(got) == 0 {
		t.Fatal("no presence at all")
	}
	if got := h.presenceOf(watcher, []string{"public-1"}, at)["public-1"].Typing; len(got) != 1 {
		t.Fatalf("a fresh typing flag reads as %v", got)
	}
	stale := at.Add(ChatTypingTTL)
	if got := h.presenceOf(watcher, []string{"public-1"}, stale)["public-1"].Typing; len(got) != 0 {
		t.Errorf("a typing flag survived its own window: %v", got)
	}
}

// ---- the working indicator --------------------------------------------- //

// `typing_status: addressed` RAISES THE INDICATOR ONLY WHERE THE RULE SAYS
// ADDRESSED.
//
// The mode is the company's own `chat.native.typing_status` and the rule is
// [chat.AddressRule] — the same value the prompt and the delivery gate read.
// A spinner over a turn that is allowed to end in silence, and a silence after
// a spinner, are one bug, so this asserts the two halves together: a mention
// raises it and an `@channel` does not.
func TestTheWorkingIndicatorFollowsTheCompanysTypingStatus(t *testing.T) {
	t.Parallel()
	addressed := chatTrigger(string(chat.ReasonMention))
	broadcast := chatTrigger(string(chat.ReasonCollective))

	for name, expect := range map[string]struct {
		mode             config.WorkingStatus
		addressed, quiet bool
	}{
		"addressed": {mode: config.StatusAddressed, addressed: true, quiet: false},
		"always":    {mode: config.StatusAlways, addressed: true, quiet: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := hubFor(t, twoRooms(), nil)
			driver := notify.NewStatusDriver(notify.StatusOptions{
				Poster: h, Mode: notify.StatusMode(expect.mode),
			})
			t.Cleanup(func() { driver.Stop(context.Background()) })

			if got := driver.Shows(addressed); got != expect.addressed {
				t.Errorf("a mention shows the indicator = %v, want %v",
					got, expect.addressed)
			}
			if got := driver.Shows(broadcast); got != expect.quiet {
				t.Errorf("an @channel shows the indicator = %v, want %v",
					got, expect.quiet)
			}
		})
	}
}

// AND RAISING IT PUTS THE SEAT IN THE ROOM IT IS ANSWERING.
//
// The whole point of the indicator is that a person who posted sees something
// while a turn runs for minutes, so the entry has to reach the room the reply
// will land in — which is the thread anchor the trigger carried, not some
// other place in the channel.
func TestRaisingTheIndicatorPutsTheSeatInTheRoomItIsAnswering(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), nil)
	driver := notify.NewStatusDriver(notify.StatusOptions{
		Poster: h, Mode: notify.StatusMode(config.StatusAlways),
	})
	t.Cleanup(func() { driver.Stop(context.Background()) })

	session := driver.Begin(t.Context(), "agent-swe", "turn-1", "execute",
		chatTrigger(string(chat.ReasonMention)))
	if session == nil {
		t.Fatal("no indicator was raised for a mention under `always`")
	}
	working := workingIn(t, h, "public-1", 1)
	if len(working) != 1 || working[0].Handle != "agent-swe" {
		t.Fatalf("the room reports %v as working", working)
	}
	if working[0].Thread != "m-1" {
		t.Errorf("the indicator sits under %q rather than the thread the reply "+
			"will land in", working[0].Thread)
	}
	if working[0].Status == "" {
		t.Error("the indicator carries no words, although this backend renders " +
			"text — a reader then cannot tell which phase is running")
	}

	// AND IT COMES DOWN WHEN THE TURN ENDS. An indicator that outlived
	// its turn would tell somebody an agent is working on an answer that
	// is never coming.
	session.End(t.Context(), false)
	if got := h.presenceOf(nil, []string{"public-1"}, h.now())["public-1"].Working; len(got) != 0 {
		t.Errorf("the indicator survived the turn: %v", got)
	}
}

// THE BACKEND NAME IS THE DOMAIN'S OWN.
//
// The driver matches it against the `transport` key the chat parser stamps on
// every trigger, so a second spelling raises no indicator at all — silently,
// on every native turn.
func TestTheIndicatorAnswersToTheNameTheChatParserStamps(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), nil)
	if h.StatusBackend() != chat.Source {
		t.Errorf("the poster answers to %q and the parser stamps %q",
			h.StatusBackend(), chat.Source)
	}
	if _, ok := notify.ConversationOf(chatTrigger(string(chat.ReasonMention)),
		h.StatusBackend()); !ok {
		t.Error("a native chat trigger does not resolve to a conversation on " +
			"this backend, so no indicator could ever be raised")
	}
}

// chatTrigger is a native chat wake's metadata, spelled with the keys the
// parser actually stamps.
func chatTrigger(reason string) map[string]string {
	return map[string]string{
		notify.TransportField:   chat.Source,
		notify.ChannelField:     "public-1",
		notify.ChannelTypeField: string(chat.KindPublic),
		notify.ChannelNameField: "general",
		notify.MessageIDField:   "m-1",
		// EMPTY ON A TOP-LEVEL MESSAGE and stamped anyway, exactly as
		// the parser stamps it: the whole follow model turns on that
		// emptiness meaning "this started nothing yet", and the anchor
		// beside it is where a reply will go.
		notify.ThreadField:       "",
		notify.ThreadAnchorField: "m-1",
		notify.FollowReasonField: reason,
		notify.UserField:         "founder",
		notify.ActorField:        "founder",
		chat.MetaAuthorKind:      string(chat.AuthorHuman),
	}
}

// ---- the cursor, and the applier ---------------------------------------- //

// A READ CURSOR REACHES ONLY ITS OWN PERSON'S SOCKETS.
//
// It is the one push here that is not about a room, because a cursor is not
// about a room: it is where one reader's eye has got to, it is not on the log,
// and the only thing in the world that wants it is that same person's other
// tabs.
func TestAReadCursorReachesOnlyItsOwnPersonsSockets(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice", "tok-b": "bob"})
	mine, theirs := NewClient(), NewClient()
	h.join(mine, "tok-a")
	h.join(theirs, "tok-b")

	h.PushTo("alice", KindChatCursorMoved, map[string]any{"cursors": 1})

	if got := kinds(mine); !slices.Equal(got, []string{KindChatCursorMoved}) {
		t.Errorf("the person's own tab was sent %v", got)
	}
	if got := kinds(theirs); len(got) != 0 {
		t.Errorf("somebody else's read state reached another person: %v", got)
	}
}

// THE APPLIER IS NEVER BLOCKED BY THE FAN-OUT.
//
// [ChatHub.Applied] runs on the applier's own goroutine, between one batch's
// commit and the next batch's transaction. An implementation that waited for
// a slow fan-out would be a node whose LOG applies slowly — and past the
// deferral grace, one that sheds its seats. A dropped frame costs one screen a
// refetch, which is why the drop is counted rather than prevented.
//
// This case HANGS rather than fails if the drop is ever turned into a wait,
// which is the honest shape for it: there is no bound to assert against a
// deadlock.
func TestTheApplierIsNeverBlockedByTheFanOut(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice"})
	h.join(NewClient(), "tok-a")

	// Nothing is draining: no loop is running, which is exactly the state
	// a node in a burst is in for a moment.
	for i := range chatAppliedQueue * 2 {
		h.Applied([]chat.Applied{post("public-1", int64(i))})
	}
	h.mu.Lock()
	dropped := h.dropped
	h.mu.Unlock()
	if dropped == 0 {
		t.Error("nothing was dropped, so the buffer either grew without bound " +
			"or the applier waited")
	}
}

// A CLOSED SOCKET STOPS BEING A WATCHER, and with it stops appearing in the
// rooms it was looking at.
func TestALeavingSocketIsNoLongerPresent(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice"})
	client := NewClient()
	h.join(client, "tok-a")
	h.focus(client, "public-1", true)
	h.leave(client)

	if got := h.presenceOf(h.snapshot(), []string{"public-1"}, h.now()); len(got) != 0 {
		t.Errorf("a socket that closed is still in the room: %v", got)
	}
}

// ---- the fleet half ----------------------------------------------------- //

// stubPeers is the ephemeral scatter, recording what was asked and answering
// with whatever a case put in it.
type stubPeers struct {
	mu       sync.Mutex
	subject  string
	asked    []string
	reply    *chatPresenceReply
	serveErr error

	// served closes on the first Serve, so a case can wait for the
	// subscription rather than sleeping for it. A Once because the hub
	// retries until one takes and a second close panics.
	served chan struct{}
	once   sync.Once
}

func (p *stubPeers) Ask(_ context.Context, subject string, request []byte, _ int) (
	[][]byte, error) {

	var req chatPresenceRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.subject, p.asked = subject, req.Channels
	reply := p.reply
	p.mu.Unlock()
	if reply == nil {
		return nil, nil
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return nil, err
	}
	return [][]byte{raw}, nil
}

func (p *stubPeers) Serve(context.Context, string, queue.AnswerFunc) (
	queue.Unsubscribe, error) {

	if p.serveErr != nil {
		return nil, p.serveErr
	}
	p.once.Do(func() {
		if p.served != nil {
			close(p.served)
		}
	})
	return func(context.Context) error { return nil }, nil
}

// A PEER'S ANSWER IS MERGED, and the probe asks only about the rooms somebody
// here has open.
//
// The second half is what keeps a fleet-wide scatter off an idle company's
// broker: the cost of presence is a function of how many tabs are open on a
// room, not of how many nodes exist.
func TestAPeersPresenceIsMergedIntoWhatALocalSocketIsShown(t *testing.T) {
	t.Parallel()
	peers := &stubPeers{reply: &chatPresenceReply{
		Version: chatPresenceProtocol, Node: "node-b",
		Rooms: map[string]ChatRoomPresence{"public-1": {
			Viewing: []string{"carol"}, Typing: []string{"carol"},
			Working: []ChatWorking{{Handle: "agent-swe", Thread: "m-1"}},
		}},
	}}
	h := NewChatHub(ChatHubOptions{
		Rooms:  func() ChatRooms { return twoRooms() },
		Viewer: func(id string) string { return map[string]string{"tok-a": "alice"}[id] },
		Peers:  func() ChatPeers { return peers },
		NodeID: "node-a",
	})
	client := NewClient()
	h.join(client, "tok-a")
	h.focus(client, "public-1", false)

	h.sweep(t.Context())

	peers.mu.Lock()
	subject, asked := peers.subject, peers.asked
	peers.mu.Unlock()
	if subject != topics.ChatPresence {
		t.Errorf("the probe went to %q", subject)
	}
	if !slices.Equal(asked, []string{"public-1"}) {
		t.Errorf("the probe asked about %v, want only the room somebody has open",
			asked)
	}
	got := frames(client)
	if len(got) != 1 {
		t.Fatalf("the socket was sent %d frames", len(got))
	}
	room := got[0].Data.(ChatPresenceFrame).Rooms["public-1"]
	if !slices.Equal(room.Viewing, []string{"alice", "carol"}) {
		t.Errorf("the merged view is %v — a peer's readers and this node's own "+
			"are different people, so the sets are a union", room.Viewing)
	}
	if len(room.Working) != 1 || room.Working[0].Handle != "agent-swe" {
		t.Errorf("a seat working on another node does not reach the room: %v",
			room.Working)
	}
}

// A REPLY CLAIMING TO BE THIS NODE IS DROPPED.
//
// Every server of a subject sees every request, so a scatter is answered by
// the asker too. This node's own presence is already in hand, first-hand and
// without the round trip — and taking the copy back off the wire would let a
// peer's build, or a stale one, restate this node's readers.
func TestAPresenceReplyFromThisNodeItselfIsDropped(t *testing.T) {
	t.Parallel()
	peers := &stubPeers{reply: &chatPresenceReply{
		Version: chatPresenceProtocol, Node: "node-a",
		Rooms: map[string]ChatRoomPresence{"public-1": {Viewing: []string{"mallory"}}},
	}}
	h := NewChatHub(ChatHubOptions{
		Rooms:  func() ChatRooms { return twoRooms() },
		Viewer: func(id string) string { return map[string]string{"tok-a": "alice"}[id] },
		Peers:  func() ChatPeers { return peers },
		NodeID: "node-a",
	})
	client := NewClient()
	h.join(client, "tok-a")
	h.focus(client, "public-1", false)

	h.sweep(t.Context())

	room := frames(client)[0].Data.(ChatPresenceFrame).Rooms["public-1"]
	if slices.Contains(room.Viewing, "mallory") {
		t.Errorf("a reply claiming to be this node was merged: %v", room.Viewing)
	}
}

// THE ANSWERING HALF IS RETRIED UNTIL IT TAKES.
//
// The hub is built before the broker client exists, so a subscription made
// once would be made against whatever happened to be wired at that instant —
// and a node that never answered a probe would be invisible to every
// colleague's typing indicator for the rest of its life, with nothing to say
// so.
func TestThePresenceSubscriptionIsRetriedUntilItTakes(t *testing.T) {
	t.Parallel()
	served := make(chan struct{})
	peers := &stubPeers{served: served}
	var live atomic.Pointer[stubPeers]
	h := NewChatHub(ChatHubOptions{
		Rooms:  func() ChatRooms { return twoRooms() },
		Viewer: func(string) string { return "alice" },
		Peers: func() ChatPeers {
			held := live.Load()
			if held == nil {
				return nil
			}
			return held
		},
		NodeID:           "node-a",
		PresenceInterval: 5 * time.Millisecond,
	})
	h.Start(t.Context())
	t.Cleanup(h.Stop)

	// The broker arrives late, which is the ordinary boot order.
	live.Store(peers)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the presence subject was never served, so this node answers " +
			"nobody's probe")
	}
}
