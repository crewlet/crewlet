package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	qmemory "github.com/crewlet/crewlet/internal/queue/memory"
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

// ChannelHere answers as Channel does: which of the two reads a pass makes is
// [recordingRooms]'s concern, not the filter's.
func (p permissiveRooms) ChannelHere(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chat.ChannelDetail, error) {
	return p.Channel(ctx, viewer, channelID, fresh)
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

// frames drains everything queued on a client without blocking — and stops at
// a client the hub closed, whose channel would otherwise answer zero values for
// ever.
func frames(c *Client) []Envelope {
	var out []Envelope
	for {
		select {
		case env, open := <-c.Out():
			if !open {
				return out
			}
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
func kinds(c *Client) []Kind {
	var out []Kind
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
		ActorKind:  chat.AuthorOperator,
		At:         time.Unix(1_700_000_000, 0).UTC(),
	}
}

// recordingRooms is [permissiveRooms] that remembers which read each decision
// made, and at what freshness.
type recordingRooms struct {
	permissiveRooms
	mu    sync.Mutex
	reads []string
	fresh []statelog.Freshness
}

func (r *recordingRooms) record(read string, fresh statelog.Freshness) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, read)
	r.fresh = append(r.fresh, fresh)
}

func (r *recordingRooms) Channel(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chat.ChannelDetail, error) {
	r.record("channel", fresh)
	return r.permissiveRooms.Channel(ctx, viewer, channelID, fresh)
}

func (r *recordingRooms) ChannelHere(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chat.ChannelDetail, error) {
	r.record("here", fresh)
	return r.permissiveRooms.Channel(ctx, viewer, channelID, fresh)
}

// A COMMITTED FRAME IS DECIDED HERE, AT ITS BATCH'S OWN POSITION; PRESENCE IS
// NOT.
//
// The batch is this node's applier's, so its copy holds it — while the
// estate's ordinary walk asks a peer first whenever that copy lags, which a
// burst is what causes, making every decision a round trip to a node that may
// not have applied the record. And the floor holds whichever copy answers to
// the batch: one that had not applied a membership change would decide against
// the rows from before it. A presence decision is about no record, so it reads
// whichever copy the walk prefers, at no floor.
func TestACommittedFrameIsDecidedHereAtItsBatchsPosition(t *testing.T) {
	t.Parallel()
	rooms := &recordingRooms{permissiveRooms: twoRooms()}
	h := hubFor(t, rooms, map[string]string{"tok-a": "alice", "tok-b": "bob"})
	alice, bob := NewClient(), NewClient()
	h.join(alice, "tok-a")
	h.join(bob, "tok-b")

	h.fan(t.Context(), []chat.Applied{post("private-1", 9), post("public-1", 7)})
	want := statelog.Position{Stream: topics.ChatLogStream, Generation: 1, Seq: 9}
	rooms.mu.Lock()
	reads, fresh := slices.Clone(rooms.reads), slices.Clone(rooms.fresh)
	rooms.reads, rooms.fresh = nil, nil
	rooms.mu.Unlock()
	if len(reads) == 0 {
		t.Fatal("the batch was decided without a read")
	}
	for i, read := range reads {
		if read != "here" || fresh[i].Level != statelog.ReadStale || fresh[i].MinPosition != want {
			t.Errorf("a committed frame was decided by %q at %+v, want this node's "+
				"copy first at stale from %v", read, fresh[i], want)
		}
	}

	h.focus(alice, "public-1", false)
	h.sweep(t.Context())
	rooms.mu.Lock()
	defer rooms.mu.Unlock()
	for i, read := range rooms.reads {
		if read != "channel" || !rooms.fresh[i].MinPosition.IsZero() {
			t.Errorf("a presence decision was made by %q at %+v, want the walk's "+
				"own read at no floor", read, rooms.fresh[i])
		}
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

	if got := kinds(inside); !slices.Equal(got, []Kind{KindChatPosted}) {
		t.Errorf("the member was sent %v, want one %s", got, KindChatPosted)
	}
	if got := kinds(outside); len(got) != 0 {
		t.Errorf("a private room's message reached a non-member as %v", got)
	}
}

// bindings is a credential-to-seat table a case can change while the hub runs,
// as an apply changes `contact.crewlet_operator_id`.
type bindings struct {
	mu    sync.Mutex
	seats map[string]string
}

func (b *bindings) viewer(operatorID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seats[operatorID]
}

func (b *bindings) bind(operatorID, seat string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if seat == "" {
		delete(b.seats, operatorID)
		return
	}
	b.seats[operatorID] = seat
}

// isClosed reports whether the hub let go of a socket, which is how it makes a
// tab reconnect.
func isClosed(c *Client) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// A SOCKET WHOSE BINDING MOVES IS CLOSED, NEVER SHOWN THE OLD SEAT'S ROOMS.
//
// `contact.crewlet_operator_id` is live configuration. A watcher resolved once
// at accept kept receiving the OLD seat's view — a private room's frames with
// their excerpts, its presence, its read state — after an apply unbound the
// token, rebound it to somebody else or removed the seat, for as long as the
// tab stayed open. Every pass resolves again, and a socket whose seat moved is
// closed rather than switched: the dashboard reconnects under the binding the
// company holds now. A socket whose binding did NOT move is left alone, or this
// would pass for a hub that closed everything.
func TestASocketWhoseBindingMovesIsClosedRatherThanShownTheOldSeat(t *testing.T) {
	t.Parallel()
	for name, move := range map[string]func(b *bindings){
		"unbound":                  func(b *bindings) { b.bind("tok-a", "") },
		"rebound to somebody else": func(b *bindings) { b.bind("tok-a", "bob") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := &bindings{seats: map[string]string{"tok-a": "alice", "tok-c": "alice"}}
			h := NewChatHub(ChatHubOptions{
				Rooms:  func() ChatRooms { return twoRooms() },
				Viewer: b.viewer,
				NodeID: "node-a",
			})
			moving, staying := NewClient(), NewClient()
			h.join(moving, "tok-a")
			h.join(staying, "tok-c")
			h.focus(moving, "private-1", false)

			move(b)
			h.fan(t.Context(), []chat.Applied{post("private-1", 7)})
			h.PushTo("alice", KindChatCursorMoved, map[string]any{"revision": 1})
			h.sweep(t.Context())

			if got := kinds(moving); len(got) != 0 {
				t.Errorf("a socket whose token moved off alice was sent %v", got)
			}
			if !isClosed(moving) {
				t.Error("a socket whose binding moved was kept open, so it never " +
					"reconnects under the binding the company holds now")
			}
			if got := h.presenceOf(h.current(), []string{"private-1"},
				h.now())["private-1"].Viewing; len(got) != 0 {
				t.Errorf("the moved socket is still in the room it was looking at: %v", got)
			}
			if isClosed(staying) {
				t.Error("a socket whose binding did not move was closed")
			}
			if got := kinds(staying); !slices.Contains(got, KindChatPosted) ||
				!slices.Contains(got, KindChatCursorMoved) {
				t.Errorf("a socket still bound to alice was sent %v", got)
			}
		})
	}
}

// A SOCKET ACCEPTED UNDER A TOKEN NOBODY HAD BOUND STARTS WATCHING WHEN AN
// APPLY BINDS IT — by being closed, so it reconnects as that person, rather
// than watching nothing until somebody reloads the tab.
func TestASocketWhoseTokenIsBoundLaterReconnectsAsThatPerson(t *testing.T) {
	t.Parallel()
	b := &bindings{seats: map[string]string{}}
	h := NewChatHub(ChatHubOptions{
		Rooms:  func() ChatRooms { return twoRooms() },
		Viewer: b.viewer,
		NodeID: "node-a",
	})
	client, anonymous := NewClient(), NewClient()
	if h.join(client, "tok-new") {
		t.Fatal("a token bound to nobody was registered as a watcher")
	}
	h.join(anonymous, "")
	h.fan(t.Context(), []chat.Applied{post("public-1", 7)})
	if isClosed(client) {
		t.Fatal("an unbound socket was closed before anything bound it")
	}

	b.bind("tok-new", "carol")
	h.fan(t.Context(), []chat.Applied{post("public-1", 8)})
	if !isClosed(client) {
		t.Error("a socket whose token was bound later was left watching nothing")
	}
	if isClosed(anonymous) {
		t.Error("a socket with no credential at all was closed")
	}
}

// rebindLog holds the process's log lines while a case that reads them runs.
type rebindLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *rebindLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// rebound counts the `chat_socket_rebound` lines said about one credential.
func (l *rebindLog) rebound(t *testing.T, operatorID string) int {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %q: %v", line, err)
		}
		if record["msg"] == "chat_socket_rebound" && record["operator"] == operatorID {
			n++
		}
	}
	return n
}

// A REBINDING IS SAID ONCE, BY THE PASS THAT WITHDREW THE SOCKET.
//
// Every pass copies the registry and resolves it with the lock let go, and
// several run at once — the fan loop, the presence sweep, a peer's probe, a
// person's own push. Two that copied a socket before either withdrew it both
// found its binding moved, and both said so; and one whose copy held a socket
// its tab then closed said its binding had moved when it had simply gone. The
// line is the one an operator reads to learn why a tab dropped, so said twice
// it reads as two applies, and said for a closed tab it is not true at all.
//
// The case holds the first pass inside its resolution — after the copy, with
// the binding already moved — while the other thing happens, then lets it
// finish. Not parallel: it swaps the process-wide logger, which this package
// otherwise leaves at its boot default.
func TestARebindingIsSaidOnceByThePassThatWithdrewTheSocket(t *testing.T) {
	logs := &rebindLog{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	for name, c := range map[string]struct {
		operatorID string
		meanwhile  func(h *ChatHub, client *Client)
		want       int
	}{
		"another pass withdrew it first": {
			operatorID: "tok-raced",
			meanwhile:  func(h *ChatHub, _ *Client) { h.current() },
			want:       1,
		},
		"its tab closed it": {
			operatorID: "tok-gone",
			meanwhile: func(h *ChatHub, client *Client) {
				h.leave(client)
				client.Close()
			},
			want: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := &bindings{seats: map[string]string{c.operatorID: "alice"}}
			var hold atomic.Bool
			entered, release := make(chan struct{}), make(chan struct{})
			h := NewChatHub(ChatHubOptions{
				Rooms: func() ChatRooms { return twoRooms() },
				Viewer: func(operatorID string) string {
					viewer := b.viewer(operatorID)
					if hold.CompareAndSwap(true, false) {
						close(entered)
						<-release
					}
					return viewer
				},
				NodeID: "node-a",
			})
			client := NewClient()
			h.join(client, c.operatorID)
			b.bind(c.operatorID, "bob")

			hold.Store(true)
			first := make(chan struct{})
			go func() {
				defer close(first)
				h.current()
			}()
			<-entered
			c.meanwhile(h, client)
			close(release)
			<-first

			if got := logs.rebound(t, c.operatorID); got != c.want {
				t.Errorf("the socket's rebinding was said %d times, want %d", got, c.want)
			}
			if !isClosed(client) {
				t.Error("the socket was left open, so it never reconnects")
			}
		})
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

	watcher := h.current()
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
		chat.MetaAuthorKind:      string(chat.AuthorOperator),
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

	if got := kinds(mine); !slices.Equal(got, []Kind{KindChatCursorMoved}) {
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
	if dropped.total == 0 {
		t.Error("nothing was dropped, so the buffer either grew without bound " +
			"or the applier waited")
	}
	// AND IT WAS SAID AS IT HAPPENED, not saved for a stop this node may
	// never reach: the first drop wrote its line at once.
	if dropped.said.IsZero() {
		t.Error("the drops were counted and said nowhere while the hub ran")
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

	if got := h.presenceOf(h.current(), []string{"public-1"}, h.now()); len(got) != 0 {
		t.Errorf("a socket that closed is still in the room: %v", got)
	}
}

// A CHANGE INSIDE THE GAP IS PROBED AT THE GAP'S END, NOT AT THE NEXT TICK.
//
// The floor between two probes coalesces a burst — typing is a focus change
// per keystroke — and its own justification is that half a second is under
// what a person perceives. That holds only if what it skips is owed at the
// gap's end: a wake that merely `continue`d waited for the idle tick, so a tab
// moving to another room just after a sweep was shown that room's presence up
// to three seconds late. Here the tick is ten seconds away, so only the owed
// probe can deliver in time.
func TestAFocusChangeInsideTheGapIsProbedAtItsEnd(t *testing.T) {
	t.Parallel()
	seats := map[string]string{"tok-a": "alice"}
	h := NewChatHub(ChatHubOptions{
		Rooms:            func() ChatRooms { return twoRooms() },
		Viewer:           func(id string) string { return seats[id] },
		NodeID:           "node-a",
		PresenceInterval: 10 * time.Second,
	})
	h.Start(t.Context())
	t.Cleanup(h.Stop)

	client := NewClient()
	h.join(client, "tok-a")
	h.focus(client, "public-1", false)
	presenceFor(t, client, "public-1", ChatPresenceGap+time.Second)

	// INSIDE THE GAP the first sweep just opened.
	h.focus(client, "private-1", false)
	presenceFor(t, client, "private-1", ChatPresenceGap+time.Second)
}

// presenceFor waits for a presence frame naming room, failing after within.
func presenceFor(t *testing.T, c *Client, room string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case env, open := <-c.Out():
			if !open {
				t.Fatalf("the socket was closed waiting for %s's presence", room)
			}
			if env.Kind != KindChatPresence {
				continue
			}
			frame, ok := env.Data.(ChatPresenceFrame)
			if !ok {
				t.Fatalf("a presence frame carried %T", env.Data)
			}
			if _, named := frame.Rooms[room]; named {
				return
			}
		case <-deadline:
			t.Fatalf("no presence frame for %s within %s", room, within)
		}
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

	if subject != topics.ChatPresence {
		// A PERSON PUSH, which nothing here answers.
		return nil, nil
	}
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

func (p *stubPeers) Serve(_ context.Context, subject string, _ queue.AnswerFunc) (
	queue.Unsubscribe, error) {

	if p.serveErr != nil {
		return nil, p.serveErr
	}
	if subject != topics.ChatPresence {
		// THE PERSON SUBJECT, which this stub serves without a word: a
		// case waiting on `served` is waiting for PRESENCE, and the person
		// half registering first must not answer for it.
		return func(context.Context) error { return nil }, nil
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

// A NODE ANSWERS ITS PEERS THE MOMENT ITS BROKER EXISTS, NOT A TICK LATER.
//
// The hub starts before the engine does, so its start-up attempt finds no
// broker, and a person push is sent once: a node that waited for its next
// presence tick to subscribe lost every push its colleagues sent in between,
// for good. [ChatHub.Wired] is synchronous, so both subjects are served when
// it returns — with an interval an hour long, nothing else could have served
// them — and it returns at once from a hub that is not running rather than
// waiting on a loop that will never read it.
func TestAHubServesItsPeersAsSoonAsItIsWired(t *testing.T) {
	t.Parallel()
	broker := qmemory.NewBroker()
	peers := newFleetPeers(t, broker)
	var live atomic.Pointer[fleetPeers]
	h := NewChatHub(ChatHubOptions{
		Rooms:  func() ChatRooms { return twoRooms() },
		Viewer: func(string) string { return "alice" },
		Peers: func() ChatPeers {
			if held := live.Load(); held != nil {
				return held
			}
			return nil
		},
		NodeID:           "node-a",
		PresenceInterval: time.Hour,
	})
	// NOT RUNNING: nothing reads the request, so a wait here is a hang.
	unstarted := make(chan struct{})
	go func() { h.Wired(t.Context()); close(unstarted) }()
	waitFor(t, unstarted, "Wired returning from a hub that is not running")

	h.Start(t.Context())
	t.Cleanup(h.Stop)
	// THE START-UP ATTEMPT IS OVER before the broker arrives, which is the
	// order a boot has: a Wired the loop has answered is one it reached after
	// its first attempt, made here against nothing.
	h.Wired(t.Context())
	live.Store(peers)
	h.Wired(t.Context())
	for _, subject := range []string{topics.ChatPresence, topics.ChatPerson} {
		select {
		case <-peers.served[subject]:
		default:
			t.Errorf("%s was not served when Wired returned: a colleague's "+
				"scatter in the next %s passes this node over", subject,
				h.opts.PresenceInterval)
		}
	}
}

// ---- a person's own state, across the fleet ----------------------------- //

// fleetPeers is a real scatter — the memory broker's — that says when each
// subject is served and when each ask is over, so a case can wait for both
// rather than sleeping for them.
type fleetPeers struct {
	*qmemory.Queue
	served map[string]chan struct{}
	asked  chan struct{}
	once   map[string]*sync.Once
}

func newFleetPeers(t *testing.T, broker *qmemory.Broker) *fleetPeers {
	t.Helper()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start a queue client: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	p := &fleetPeers{Queue: q, asked: make(chan struct{}, 64),
		served: map[string]chan struct{}{}, once: map[string]*sync.Once{}}
	for _, subject := range []string{topics.ChatPresence, topics.ChatPerson} {
		p.served[subject] = make(chan struct{})
		p.once[subject] = &sync.Once{}
	}
	return p
}

func (p *fleetPeers) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (
	queue.Unsubscribe, error) {

	unsubscribe, err := p.Queue.Serve(ctx, subject, h)
	if err == nil {
		p.once[subject].Do(func() { close(p.served[subject]) })
	}
	return unsubscribe, err
}

func (p *fleetPeers) Ask(ctx context.Context, subject string, request []byte,
	want int) ([][]byte, error) {

	replies, err := p.Queue.Ask(ctx, subject, request, want)
	if subject == topics.ChatPerson {
		p.asked <- struct{}{}
	}
	return replies, err
}

// waitFor fails the case unless ch closes, or yields, within a bound.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// A PERSON'S READ STATE REACHES THEIR SOCKETS ON EVERY NODE, ONCE, AND NOBODY
// ELSE'S.
//
// A person's second tab is attached to whichever ingress node its load
// balancer chose, and their assistant writes through whichever node its MCP
// client reached — so a push that stayed on the node that took the flush
// cleared a badge in one window and left it in the other until that window
// read the rail again. Two hubs on one broker: a push on A reaches the same
// person's socket on B, carrying what A was handed, and never the other
// person's there; and A's own socket, which A delivered to directly, is not
// handed it a second time when A's push comes back off the scatter — every
// server of a subject sees every request, its asker's included.
func TestAPersonsPushReachesTheirSocketsOnEveryNode(t *testing.T) {
	t.Parallel()
	broker := qmemory.NewBroker()
	seats := map[string]string{"tok-a": "alice", "tok-b": "bob"}
	// EACH NODE'S OWN CLOCK, an hour apart, so a frame stamped by the node
	// that delivered it rather than the node that took the write shows.
	hubOn := func(node string, clock time.Time) (*ChatHub, *fleetPeers) {
		peers := newFleetPeers(t, broker)
		h := NewChatHub(ChatHubOptions{
			Rooms:            func() ChatRooms { return twoRooms() },
			Viewer:           func(id string) string { return seats[id] },
			Peers:            func() ChatPeers { return peers },
			NodeID:           node,
			Now:              func() time.Time { return clock },
			PresenceInterval: 5 * time.Millisecond,
			AskBudget:        time.Second,
		})
		h.Start(t.Context())
		t.Cleanup(h.Stop)
		waitFor(t, peers.served[topics.ChatPerson], node+" serving the person subject")
		return h, peers
	}
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	a, peersA := hubOn("node-a", at)
	b, _ := hubOn("node-b", at.Add(time.Hour))
	aliceOnA, aliceOnB, bobOnB := NewClient(), NewClient(), NewClient()
	a.join(aliceOnA, "tok-a")
	b.join(aliceOnB, "tok-a")
	b.join(bobOnB, "tok-b")

	state := map[string]any{"cursors": map[string]any{"public-1": 42}, "revision": 7}
	a.PushTo("alice", KindChatCursorMoved, state)
	waitFor(t, peersA.asked, "node-a's fan-out")

	// THE OTHER NODE'S SOCKET FOR THE SAME PERSON, carrying what A said.
	got := frames(aliceOnB)
	if len(got) != 1 || got[0].Kind != KindChatCursorMoved {
		t.Fatalf("alice's socket on node-b was sent %v, want one %s",
			kindsOf(got), KindChatCursorMoved)
	}
	raw, err := json.Marshal(got[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustJSON(t, state); string(raw) != string(want) {
		t.Errorf("node-b delivered %s, want what node-a was handed: %s", raw, want)
	}
	if want := Push(KindChatCursorMoved, nil, at).TS; got[0].TS != want {
		t.Errorf("node-b stamped the frame %s, want node-a's instant %s", got[0].TS, want)
	}
	// NOBODY ELSE'S, on either node.
	if got := kinds(bobOnB); len(got) != 0 {
		t.Errorf("another person's socket on node-b was sent %v", got)
	}
	// AND ONCE ON THE NODE THAT TOOK IT, the scatter's echo dropped.
	if got := kinds(aliceOnA); !slices.Equal(got, []Kind{KindChatCursorMoved}) {
		t.Errorf("alice's socket on node-a was sent %v, want exactly one %s — "+
			"the push came back off the scatter and was delivered again",
			got, KindChatCursorMoved)
	}
}

// kindsOf is what a run of frames says it is.
func kindsOf(frames []Envelope) []Kind {
	out := make([]Kind, 0, len(frames))
	for _, env := range frames {
		out = append(out, env.Kind)
	}
	return out
}

// A FAN-OUT THAT HAS FALLEN BEHIND NEVER HOLDS THE WRITE THAT PUSHED.
//
// [ChatHub.PushTo] runs on the request whose flush has already landed, so a
// fleet that is slow to take the push must cost a dropped frame on another
// node — repaired by that screen's next read of the rail — and never the
// request. This node's own sockets have their frame regardless. Like
// [TestTheApplierIsNeverBlockedByTheFanOut], it hangs rather than fails if the
// drop ever becomes a wait.
func TestAPersonPushNeverWaitsForTheFleet(t *testing.T) {
	t.Parallel()
	h := NewChatHub(ChatHubOptions{
		Rooms:  func() ChatRooms { return twoRooms() },
		Viewer: func(string) string { return "alice" },
		Peers:  func() ChatPeers { return &stubPeers{} },
		NodeID: "node-a",
	})
	mine := NewClient()
	h.join(mine, "tok-a")
	// Nothing is draining: the loop is not running.
	for range chatPersonQueue * 2 {
		h.PushTo("alice", KindChatCursorMoved, map[string]any{"revision": 1})
	}
	h.mu.Lock()
	dropped := h.personDropped
	h.mu.Unlock()
	if dropped.total != chatPersonQueue {
		t.Errorf("%d pushes were dropped, want the %d past the queue", dropped.total,
			chatPersonQueue)
	}
	// SAID AS IT HAPPENED: the first drop at once, the rest held for the
	// next line inside [chatDropReportEvery].
	if dropped.said.IsZero() || dropped.unsaid != chatPersonQueue-1 {
		t.Errorf("the drops were said at %v with %d unsaid, want a line at the "+
			"first and the other %d held for the next", dropped.said, dropped.unsaid,
			chatPersonQueue-1)
	}
	if n := len(frames(mine)); n == 0 {
		t.Error("this node's own socket was sent nothing while the fan-out was behind")
	}
}

// A PEER'S PUSH THIS BUILD CANNOT READ, OR THIS HUB'S OWN, IS NOT DELIVERED.
func TestAPersonPushIsDeliveredOnlyFromAPeerThisBuildSpeaks(t *testing.T) {
	t.Parallel()
	h := hubFor(t, twoRooms(), map[string]string{"tok-a": "alice"})
	mine := NewClient()
	h.join(mine, "tok-a")
	push := []chatPersonPush{{Handle: "alice", Kind: KindChatCursorMoved,
		Data: json.RawMessage(`{"revision":3}`), At: h.now()}}

	for name, req := range map[string]chatPersonRequest{
		"this hub's own":   {Version: chatPersonProtocol, Origin: h.origin, Pushes: push},
		"a newer protocol": {Version: chatPersonProtocol + 1, Origin: "peer", Pushes: push},
		"a push naming nobody": {Version: chatPersonProtocol, Origin: "peer",
			Pushes: []chatPersonPush{{Kind: KindChatCursorMoved, Data: json.RawMessage(`{}`)}}},
	} {
		if err := h.answerPerson(mustJSON(t, req)); err == nil {
			t.Errorf("%s answered something; a push answers nothing", name)
		}
		if got := kinds(mine); len(got) != 0 {
			t.Errorf("%s was delivered: %v", name, got)
		}
	}
	if err := h.answerPerson(mustJSON(t, chatPersonRequest{
		Version: chatPersonProtocol, Origin: "peer", Pushes: push,
	})); !errors.Is(err, errChatPersonDelivered) {
		t.Errorf("a peer's push answered %v, want nothing, delivered", err)
	}
	if got := kinds(mine); !slices.Equal(got, []Kind{KindChatCursorMoved}) {
		t.Errorf("a peer's push was delivered as %v", got)
	}
}

// A DROP IS SAID WHILE IT IS HAPPENING, AND NOT ONCE PER FRAME.
//
// The first drop writes a line at once; drops inside [chatDropReportEvery] of
// it are counted for the next; the first drop past the window says everything
// since; and what is left when the hub stops is said then. A tally that said
// nothing until the stop is the shape this replaced: a node that runs for
// months never stops, so it never said it.
func TestADropIsSaidAsItHappensAtMostOncePerWindow(t *testing.T) {
	t.Parallel()
	var d dropTally
	at := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	if n := d.drop(at); n != 1 {
		t.Fatalf("the first drop is said as %d, want 1 at once", n)
	}
	for i := range 5 {
		if n := d.drop(at.Add(time.Duration(i+1) * time.Second)); n != 0 {
			t.Fatalf("a drop inside the window wrote a line of %d", n)
		}
	}
	if n := d.drop(at.Add(chatDropReportEvery)); n != 6 {
		t.Errorf("the first drop past the window says %d, want the 5 held and itself", n)
	}
	d.drop(at.Add(chatDropReportEvery + time.Second))
	if n := d.rest(); n != 1 || d.total != 8 {
		t.Errorf("the hub's last word says %d of %d, want the 1 no line said of 8", n, d.total)
	}
	if n := d.rest(); n != 0 {
		t.Errorf("a second last word repeats %d", n)
	}
}

// THE PERSON FAN-OUT IS PACED BY ITS OWN SEND, NEVER BY THE PRESENCE PROBE'S WAIT.
//
// It waits for no reply, so its scatter's deadline is the loop's whole pace: a
// batch per window. Borrowing the probe's three quarters of a second for it
// capped a node at sixty-four pushes per probe-wait and dropped the rest. A
// presence wait set long here must not reach the person subject at all.
func TestAPersonFanOutIsPacedByItsOwnSend(t *testing.T) {
	t.Parallel()
	peers := &deadlinePeers{asked: make(chan time.Duration, 1)}
	h := NewChatHub(ChatHubOptions{
		Rooms:     func() ChatRooms { return twoRooms() },
		Viewer:    func(string) string { return "alice" },
		Peers:     func() ChatPeers { return peers },
		NodeID:    "node-a",
		AskBudget: time.Minute,
	})
	h.Start(t.Context())
	t.Cleanup(h.Stop)
	h.PushTo("alice", KindChatCursorMoved, map[string]any{"revision": 1})
	select {
	case window := <-peers.asked:
		if window > chatPersonSend {
			t.Errorf("the person fan-out waited up to %v for replies that never "+
				"come, want at most its own %v", window, chatPersonSend)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the push never left for the fleet")
	}
}

// deadlinePeers reports how long each person fan-out was given to wait.
type deadlinePeers struct{ asked chan time.Duration }

func (p *deadlinePeers) Ask(ctx context.Context, subject string, _ []byte, _ int) (
	[][]byte, error) {

	if subject == topics.ChatPerson {
		deadline, _ := ctx.Deadline()
		select {
		case p.asked <- time.Until(deadline):
		default:
		}
	}
	return nil, nil
}

func (p *deadlinePeers) Serve(context.Context, string, queue.AnswerFunc) (queue.Unsubscribe, error) {
	return func(context.Context) error { return nil }, nil
}
