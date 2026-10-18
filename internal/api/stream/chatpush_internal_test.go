package stream

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CHAT ARM'S TESTS ARE INTERNAL, because what they protect is a decision
// taken between a committed record and a socket's own queue — the watcher
// registry, the visibility filter and the presence gather — none of which has
// an exported surface to drive it through. The alternative is a real
// WebSocket per case to assert that a private room did NOT arrive, which is a
// test that passes for the wrong reason whenever the socket is slow. The
// socket's own joining of the hub is held at the end of this file, through
// the handler's serveSocket.

// permissiveRooms answers every room it holds, with this viewer's membership,
// AND DECIDES NOTHING.
//
// That is the whole point of the fixture. The real reader refuses a room the
// viewer may not read as `not found`, so a stub that did the same would make
// every case below pass with the filter deleted — the thing under test is
// what [ChatHub.maySee] does with a room it was handed.
type permissiveRooms struct {
	rooms   map[string]chatlet.Channel
	members map[string][]string
	err     error
}

func (p permissiveRooms) Channel(_ context.Context, viewer, channelID string,
	_ statelog.Freshness) (chatlet.ChannelDetail, error) {

	if p.err != nil {
		return chatlet.ChannelDetail{}, p.err
	}
	room, held := p.rooms[channelID]
	if !held {
		return chatlet.ChannelDetail{}, errors.New("no such room")
	}
	return chatlet.ChannelDetail{
		Channel: room,
		Member:  slices.Contains(p.members[channelID], viewer),
	}, nil
}

// ChannelHere answers as Channel does: which of the two reads a pass makes is
// [recordingRooms]' concern, not the filter's.
func (p permissiveRooms) ChannelHere(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	return p.Channel(ctx, viewer, channelID, fresh)
}

// twoRooms is a private room alice is in and bob is not, plus a public one.
func twoRooms() permissiveRooms {
	return permissiveRooms{
		rooms: map[string]chatlet.Channel{
			"private-1": {ID: "private-1", Kind: chatlet.KindPrivate},
			"public-1":  {ID: "public-1", Kind: chatlet.KindPublic},
		},
		members: map[string][]string{"private-1": {"alice"}},
	}
}

// chatClock is the instant every hub here is pinned to.
var chatClock = time.Unix(1_700_000_000, 0).UTC()

// hubOver builds a hub over rooms, on this node, at [chatClock].
func hubOver(rooms ChatRooms) *ChatHub {
	return NewChatHub(ChatHubOptions{
		Rooms:  func() ChatRooms { return rooms },
		NodeID: "node-a",
		Now:    func() time.Time { return chatClock },
	})
}

// chatPerson is a person acting as their own human seat and holding Chatlet
// — the one principal a socket is shown chat as. Their login is not their
// handle, as a person's never has to be.
func chatPerson(seat string) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: seat + ".login", Kind: iam.KindPerson,
		Seat: seat, Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantChatlet}}
}

// watcherFor is one socket of p's, joined to h as the handler joins it: built
// with p's audience and registered under the seat [chatViewerOf] names.
func watcherFor(t *testing.T, h *ChatHub, p iam.Principal) *Client {
	t.Helper()
	c := NewClient(NewAudience(p, nil))
	if !h.join(c, chatViewerOf(p)) {
		t.Fatalf("%s acting as %q was not joined", p.Login, p.Seat)
	}
	return c
}

// chatKinds is what a client was sent, in order.
func chatKinds(c *Client) []Kind {
	var out []Kind
	for _, f := range drainClient(c) {
		out = append(out, f.Kind())
	}
	return out
}

// dataAs decodes one frame's `data` as a browser receives it.
func dataAs[T any](t *testing.T, f *Frame) T {
	t.Helper()
	var outer struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(f.Raw(), &outer); err != nil {
		t.Fatalf("decode %s: %v", f.Raw(), err)
	}
	var out T
	if err := json.Unmarshal(outer.Data, &out); err != nil {
		t.Fatalf("decode the %s frame's data %s: %v", f.Kind(), outer.Data, err)
	}
	return out
}

func post(channelID string, seq int64) chatlet.Applied {
	return chatlet.Applied{
		Position:   statelog.Position{Stream: topics.ChatLogStream, Generation: 1, Seq: uint64(seq)},
		Subject:    chatlet.MessageSubject(channelID),
		Op:         chatlet.OpPost,
		OpID:       "op-1",
		ChannelID:  channelID,
		MessageID:  "m-1",
		ChannelSeq: seq,
		Actor:      "alice",
		ActorKind:  chatlet.AuthorHuman,
		At:         chatClock,
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
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	r.record("channel", fresh)
	return r.permissiveRooms.Channel(ctx, viewer, channelID, fresh)
}

func (r *recordingRooms) ChannelHere(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	r.record("here", fresh)
	return r.permissiveRooms.Channel(ctx, viewer, channelID, fresh)
}

// ---- the committed half ------------------------------------------------ //

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
	h := hubOver(rooms)
	alice := watcherFor(t, h, chatPerson("alice"))
	watcherFor(t, h, chatPerson("bob"))

	h.fan(t.Context(), []chatlet.Applied{post("private-1", 9), post("public-1", 7)})
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
	if len(rooms.reads) == 0 {
		t.Fatal("the sweep decided nothing, so the presence half asserts nothing")
	}
	for i, read := range rooms.reads {
		if read != "channel" || !rooms.fresh[i].MinPosition.IsZero() {
			t.Errorf("a presence decision was made by %q at %+v, want the walk's "+
				"own read at no floor", read, rooms.fresh[i])
		}
	}
}

// A FRAME FROM A PRIVATE ROOM NEVER REACHES SOMEBODY WHO IS NOT IN IT.
//
// This is the one failure this whole arm exists to prevent. Without the
// per-recipient filter a message in a private room is delivered to whoever
// happened to have a tab open — and the excerpt on the frame means the words
// themselves go with it.
func TestAChatFrameForAPrivateRoomNeverReachesANonMember(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	inside := watcherFor(t, h, chatPerson("alice"))
	outside := watcherFor(t, h, chatPerson("bob"))

	h.fan(t.Context(), []chatlet.Applied{post("private-1", 7)})

	if got := chatKinds(inside); !slices.Equal(got, []Kind{KindChatPosted}) {
		t.Errorf("the member was sent %v, want one %s", got, KindChatPosted)
	}
	if got := chatKinds(outside); len(got) != 0 {
		t.Errorf("a private room's message reached a non-member as %v", got)
	}
}

// A PERSON TAKEN OUT OF A PRIVATE ROOM IS TOLD IT CHANGED; NOBODY ELSE OUTSIDE IS.
//
// A frame is decided at its batch's own position, and that position is exactly
// where the removed stopped being able to read the room — so the visibility
// rule alone sent them nothing, and the room stayed in their rail, openable and
// badged, until something else made it read again. The record says who it took
// out ([chatlet.Applied.Removed]); they are sent the change, a frame naming no
// member, and somebody who was never in the room still is not.
func TestAPersonTakenOutOfAPrivateRoomIsToldItChanged(t *testing.T) {
	t.Parallel()
	rooms := twoRooms()
	// AT THE BATCH'S POSITION alice is already gone: the room's only member
	// is carol, who the same record kept.
	rooms.members["private-1"] = []string{"carol"}
	h := hubOver(rooms)
	removed := watcherFor(t, h, chatPerson("alice"))
	kept := watcherFor(t, h, chatPerson("carol"))
	stranger := watcherFor(t, h, chatPerson("bob"))

	members := chatlet.Applied{
		Position:  statelog.Position{Stream: topics.ChatLogStream, Generation: 1, Seq: 12},
		Subject:   chatlet.ChannelSubject("private-1"),
		Op:        chatlet.OpMembers,
		OpID:      "op-members",
		ChannelID: "private-1",
		Actor:     "carol",
		ActorKind: chatlet.AuthorHuman,
		At:        chatClock,
		Removed:   []string{"alice"},
	}
	h.fan(t.Context(), []chatlet.Applied{members})

	frames := drainClient(removed)
	if len(frames) != 1 || frames[0].Kind() != KindChatMembershipChanged {
		t.Fatalf("the person taken out of the room was sent %v, want one %s — "+
			"their rail keeps the room until something else reads it again",
			frames, KindChatMembershipChanged)
	}
	if got := dataAs[ChatChange](t, frames[0]); got.ChannelID != "private-1" || got.OpID != "op-members" {
		t.Errorf("the removal frame is %+v, want the room's own membership record", got)
	}
	if got := chatKinds(kept); !slices.Equal(got, []Kind{KindChatMembershipChanged}) {
		t.Errorf("a member the record kept was sent %v", got)
	}
	if got := chatKinds(stranger); len(got) != 0 {
		t.Errorf("somebody never in the private room was sent %v about it", got)
	}
}

// A FRAME FROM A PUBLIC ROOM REACHES EVERY SEAT, joined or not, with the
// sequence that lets a client tell a frame it can apply from a hole it must
// refetch — and in ONE encode for all of them, since the frame is the same.
// Without the reach the filter could be "send nothing" and the case above
// would still pass.
func TestAChatFrameForAPublicRoomReachesEverySeatInOneEncode(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	joined := watcherFor(t, h, chatPerson("alice"))
	stranger := watcherFor(t, h, chatPerson("bob"))

	h.fan(t.Context(), []chatlet.Applied{post("public-1", 3)})

	var sent []*Frame
	for name, client := range map[string]*Client{"a member": joined, "a non-member": stranger} {
		got := drainClient(client)
		if len(got) != 1 {
			t.Fatalf("%s was sent %d frames for a public room", name, len(got))
		}
		change := dataAs[ChatChange](t, got[0])
		if change.ChannelSeq != 3 || change.MessageID != "m-1" ||
			change.ChannelID != "public-1" || change.Op != chatlet.OpPost {
			t.Errorf("%s was sent %+v", name, change)
		}
		sent = append(sent, got[0])
	}
	if sent[0] != sent[1] {
		t.Error("one committed frame to two viewers in one posture was encoded twice")
	}
}

// ONLY A PERSON ACTING AS THEIR SEAT, HOLDING CHAT, IS SHOWN CHAT — and the
// seat is the one they act as, never their login.
//
// A credential acting as itself (a Tier A token, a service account bound to
// none) is no seat: [chatlet.Visible] reads any non-empty viewer as somebody who
// may read every public room, so a login handed in as the viewer would be
// shown the company's conversation as nobody. An agent's own principal is a
// seat but no person. A person without the app is not its reader. None of
// them is registered at all; a machine the directory binds to a person is
// composed as that person and is.
func TestOnlyAPersonActingAsTheirSeatWithChatIsShownChat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		p    iam.Principal
		want string
	}{
		{"a person with chat", chatPerson("alice"), "alice"},
		{"a token the directory binds to a person", iam.Principal{Login: "ci-bot",
			Kind: iam.KindPerson, Seat: "alice", Via: "pat:7", Stage: iam.StageActive,
			Grants: []iam.Grant{iam.GrantChatlet}}, "alice"},
		{"a person without chat", iam.Principal{Login: "bob.login", Kind: iam.KindPerson,
			Seat: "bob", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}}, ""},
		{"a credential acting as itself", iam.Principal{Login: "token:pipeline",
			Kind: iam.KindMachine, Stage: iam.StageActive, Grants: iam.AllGrants}, ""},
		{"an agent's own principal", iam.Principal{Login: "seat:agent-swe",
			Kind: iam.KindSeat, Seat: "agent-swe", Stage: iam.StageActive,
			Grants: []iam.Grant{iam.GrantChatlet}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chatViewerOf(tc.p); got != tc.want {
				t.Fatalf("the viewer is %q, want %q", got, tc.want)
			}
			h := hubOver(twoRooms())
			c := NewClient(NewAudience(tc.p, nil))
			if joined := h.join(c, chatViewerOf(tc.p)); joined != (tc.want != "") {
				t.Fatalf("joined = %v", joined)
			}
			h.fan(t.Context(), []chatlet.Applied{post("public-1", 1)})
			if got := chatKinds(c); (len(got) == 1) != (tc.want != "") {
				t.Errorf("was sent %v", got)
			}
		})
	}
}

// AND A FRAME NEEDS THE READER'S `chatlet` WHATEVER THE HUB DECIDED: the
// audience's need is checked on the one fan-out every push leaves by, so a
// socket somehow joined without the app is still sent nothing. Without this
// the join above would be the only lock.
func TestAChatFrameNeedsItsReadersChatApp(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	withoutChat := iam.Principal{Login: "bob.login", Kind: iam.KindPerson, Seat: "bob",
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}}
	c := NewClient(NewAudience(withoutChat, nil))
	h.join(c, "bob")
	h.fan(t.Context(), []chatlet.Applied{post("public-1", 1)})
	h.Reread()
	h.reread()
	if got := chatKinds(c); len(got) != 0 {
		t.Errorf("a reader without Chatlet was sent %v", got)
	}
	for _, kind := range ChatPushKinds() {
		if NewAudience(withoutChat, nil).Receives(kind) {
			t.Errorf("%s reaches a reader without chat", kind)
		}
		if !NewAudience(chatPerson("alice"), nil).Receives(kind) {
			t.Errorf("%s does not reach a reader with chat", kind)
		}
	}
}

// A VISIBILITY DECISION THAT CANNOT BE MADE FAILS CLOSED.
//
// A store that will not answer is not permission to show a transcript. The
// screen repairs itself on its next read.
func TestAFrameWhoseVisibilityCannotBeDecidedIsWithheld(t *testing.T) {
	t.Parallel()
	broken := twoRooms()
	broken.err = errors.New("this node is behind the log")
	h := hubOver(broken)
	client := watcherFor(t, h, chatPerson("alice"))

	h.fan(t.Context(), []chatlet.Applied{post("public-1", 1)})

	if got := chatKinds(client); len(got) != 0 {
		t.Errorf("a frame went out on a read that failed: %v", got)
	}
}

// AND NO ROOM READER IS NO FRAME AT ALL: a node whose Chatlet half is not
// up cannot decide, and a filter that cannot be run is not permission to skip
// it.
func TestAHubWithNoRoomReaderSendsNothing(t *testing.T) {
	t.Parallel()
	h := NewChatHub(ChatHubOptions{Rooms: func() ChatRooms { return nil }})
	client := watcherFor(t, h, chatPerson("alice"))
	h.fan(t.Context(), []chatlet.Applied{post("public-1", 1)})
	if got := chatKinds(client); len(got) != 0 {
		t.Errorf("a frame went out with no room reader: %v", got)
	}
}

// EVERY OP IS EITHER A FRAME OR NAMED AS THE FRAMEWORK'S OWN.
//
// An eviction, a generation and a barrier change no room; a default arm that
// pushed them would put a frame with no case on every open socket. And every
// op that is about a conversation has a kind — over the domain's own list, so
// an op added there is a decision made here rather than a gesture silently
// invisible on every screen.
func TestEveryChatOpIsAFrameOrTheFrameworksOwn(t *testing.T) {
	t.Parallel()
	machinery := []chatlet.OpKind{chatlet.OpEviction, chatlet.OpGeneration, chatlet.OpBarrier}
	for _, op := range chatlet.OpKinds {
		kind, push := chatPushKind(op)
		switch {
		case slices.Contains(machinery, op) && push:
			t.Errorf("the framework's %s would be pushed as %q", op, kind)
		case !slices.Contains(machinery, op) && !push:
			t.Errorf("%s reaches no open screen", op)
		case push && RouteOf(kind) != RouteViewer:
			t.Errorf("%s is pushed as %q, which is not routed per viewer", op, kind)
		}
	}
}

// EVERY VIEWER-ROUTED KIND IS THE CHAT HUB'S, AND EACH HAS A PRODUCER.
//
// A kind routed per viewer that nothing here produces is a route nobody
// takes; one this hub produces that the route table sends another way would
// be fanned out by route — a private room on every tab. The committed records'
// kinds come from [chatPushKind]; presence and a person's read state have a
// path each, held by their own cases below.
func TestEveryViewerRoutedKindIsTheChatHubsAndProduced(t *testing.T) {
	t.Parallel()
	produced := map[Kind]bool{KindChatPresence: true, KindChatCursorMoved: true}
	for _, op := range chatlet.OpKinds {
		if kind, push := chatPushKind(op); push {
			produced[kind] = true
		}
	}
	routed := map[Kind]bool{}
	for kind, route := range routes {
		if route == RouteViewer {
			routed[kind] = true
		}
	}
	listed := map[Kind]bool{}
	for _, kind := range ChatPushKinds() {
		listed[kind] = true
	}
	for name, set := range map[string]map[Kind]bool{
		"produced": produced, "routed per viewer": routed,
	} {
		if !maps.Equal(set, listed) {
			t.Errorf("the kinds %s are %v, want exactly ChatPushKinds %v",
				name, slices.Sorted(maps.Keys(set)), slices.Sorted(maps.Keys(listed)))
		}
	}
	for kind := range listed {
		if need, _ := NeedOf(kind); need != NeedChatlet {
			t.Errorf("%s is read by %q, want Chatlet", kind, need)
		}
	}
}

// A CHAT FRAME IS NEVER BROADCAST. The shared fan-out is over every open
// socket, so a chat kind handed to it is a private room on every tab — and it
// is refused there, whoever calls it.
func TestAChatFrameIsNeverBroadcast(t *testing.T) {
	t.Parallel()
	hub := NewHub()
	defer hub.Close()
	reader := NewClient(NewAudience(chatPerson("alice"), nil))
	hub.Register(reader)
	for _, kind := range ChatPushKinds() {
		hub.Broadcast(Push(kind, ChatChange{ChannelID: "private-1"}, chatClock))
	}
	if got := chatKinds(reader); len(got) != 0 {
		t.Errorf("a broadcast delivered chat frames %v", got)
	}
}

// A DEGRADED NODE SENDS NO CHAT FRAME: its copy is the one nobody vouches for,
// and a degraded client keeps only what answers it and the health frame.
//
// AND A COMMITTED FRAME IT WILL NEVER BE SENT COSTS NO VISIBILITY READ: the
// posture is asked before the room is, so a degraded node — every socket on it
// degraded — does not read a room through the router for every batch only to
// withhold each frame. Its tab still CONTRIBUTES to presence, which is why the
// sweep below may read.
func TestADegradedClientIsSentNoChatFrame(t *testing.T) {
	t.Parallel()
	rooms := &recordingRooms{permissiveRooms: twoRooms()}
	h := hubOver(rooms)
	client := watcherFor(t, h, chatPerson("alice"))
	if !client.SetPosture(FrameDegraded) {
		t.Fatal("the posture did not take")
	}
	h.fan(t.Context(), []chatlet.Applied{post("public-1", 1)})
	rooms.mu.Lock()
	read := slices.Clone(rooms.reads)
	rooms.mu.Unlock()
	if len(read) != 0 {
		t.Errorf("a committed frame no socket would be sent read its room: %v", read)
	}
	h.focus(client, "public-1", false)
	h.sweep(t.Context())
	if got := chatKinds(client); len(got) != 0 {
		t.Errorf("a degraded client was sent %v", got)
	}
}

// ---- a gap -------------------------------------------------------------- //

// A GAP IS A RE-READ OF EVERY ROOM, FOR EVERY WATCHER: one `chat_room_changed`
// naming no room, since nothing between is replayed and a frame per room
// would need to know what every screen holds. A socket not joined is told
// nothing, as it is told nothing else.
func TestAGapTellsEveryWatcherToReadItsRoomsAgain(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	h.Start(t.Context())
	t.Cleanup(h.Stop)
	alice := watcherFor(t, h, chatPerson("alice"))
	bob := watcherFor(t, h, chatPerson("bob"))
	stranger := NewClient(NewAudience(chatPerson("carol"), nil))

	h.Reread()
	for name, c := range map[string]*Client{"alice": alice, "bob": bob} {
		f := nextFrame(t, c, KindChatRoomChanged)
		if change := dataAs[ChatChange](t, f); change.ChannelID != "" {
			t.Errorf("%s's gap names room %q, want none", name, change.ChannelID)
		}
	}
	if got := chatKinds(stranger); len(got) != 0 {
		t.Errorf("a socket not joined was told %v", got)
	}
}

// nextFrame waits for a client's next frame of kind, failing after a bound.
func nextFrame(t *testing.T, c *Client, kind Kind) *Frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, open := <-c.Out():
			if !open {
				t.Fatalf("the client was closed waiting for %s", kind)
			}
			if f.Kind() == kind {
				return f
			}
		case <-deadline:
			t.Fatalf("no %s frame within five seconds", kind)
		}
	}
}

// THE APPLIER IS NEVER BLOCKED BY THE FAN-OUT, AND WHAT IT DROPS IS REPAIRED.
//
// [ChatHub.Applied] runs on the applier's own goroutine, between one batch's
// commit and the next batch's transaction. An implementation that waited for
// a slow fan-out would be a node whose LOG applies slowly — and past the
// deferral grace, one that sheds its seats. So a full buffer drops, says so as
// it happens, and owes every screen the re-read that covers the hole: the last
// messages said before a quiet spell would otherwise be missing until
// somebody reloaded, since only a later message reveals a gap in a room's
// sequence.
//
// This case HANGS rather than fails if the drop is ever turned into a wait,
// which is the honest shape for it: there is no bound to assert against a
// deadlock.
func TestTheApplierIsNeverBlockedAndADropIsRepaired(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	client := watcherFor(t, h, chatPerson("alice"))

	// Nothing is draining: no loop is running, which is exactly the state
	// a node in a burst is in for a moment.
	for i := range chatAppliedQueue * 2 {
		h.Applied([]chatlet.Applied{post("public-1", int64(i))})
	}
	h.mu.Lock()
	dropped := h.dropped
	h.mu.Unlock()
	if dropped.total == 0 {
		t.Fatal("nothing was dropped, so the buffer either grew without bound " +
			"or the applier waited")
	}
	if dropped.said.IsZero() {
		t.Error("the drops were counted and said nowhere while the hub ran")
	}
	if !h.rereadOwed.Load() {
		t.Fatal("a dropped batch owes no screen a re-read")
	}
	h.Start(t.Context())
	t.Cleanup(h.Stop)
	if change := dataAs[ChatChange](t, nextFrame(t, client, KindChatRoomChanged)); change.ChannelID != "" {
		t.Errorf("the repair names room %q, want every room", change.ChannelID)
	}
}

// A DROP IS SAID WHILE IT IS HAPPENING, AND NOT ONCE PER FRAME.
//
// The first drop writes a line at once; drops inside [chatDropReportEvery] of
// it are counted for the next; the first drop past the window says everything
// since; and what is left when the hub stops is said then.
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

// ---- a person's own read state ------------------------------------------ //

// A PERSON'S READ STATE REACHES THEIR OWN SOCKETS AND NOBODY ELSE'S, pushed
// from the coordination record itself — the twin the engine's suite certifies
// — on a node serving sockets: a flush through any surface on any node is a
// record every node's watch hears.
func TestAReadStateReachesOnlyItsOwnPersonsSockets(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	h := NewChatHub(ChatHubOptions{
		Rooms: func() ChatRooms { return twoRooms() }, Reads: fleet,
		Now: func() time.Time { return chatClock },
	})
	h.Start(t.Context())
	t.Cleanup(h.Stop)
	h.AttachSockets()
	mine := watcherFor(t, h, chatPerson("alice"))
	other := watcherFor(t, h, chatPerson("bob"))

	written, err := fleet.AdvanceChatRead(t.Context(), "alice", coord.ChatReadDelta{
		Cursors: map[string]int64{"public-1": 7}, At: chatClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	pushed := dataAs[coord.PersonChatRead](t, nextFrame(t, mine, KindChatCursorMoved))
	if pushed.Handle != "alice" || pushed.Cursors["public-1"] != 7 ||
		pushed.Revision != written.Revision {
		t.Errorf("the push is %+v, want alice's state at revision %d", pushed,
			written.Revision)
	}
	// AND NOT BOB'S: a write of his own, after hers, is what he hears first.
	if _, err := fleet.AdvanceChatRead(t.Context(), "bob", coord.ChatReadDelta{
		Mute: []string{"public-1"}, At: chatClock,
	}); err != nil {
		t.Fatal(err)
	}
	if got := dataAs[coord.PersonChatRead](t, nextFrame(t, other, KindChatCursorMoved)); got.Handle != "bob" {
		t.Errorf("bob's socket was handed %s's read state", got.Handle)
	}
}

// AND WHAT IS PUSHED IS THE PERSON'S OWN STATE, NEWER THAN WHAT WAS: never the
// reconcile's absence stamp, which is the sweep's machinery; nothing for a
// forgotten record, whose seat left the company and whose sockets closed with
// it; and nothing again for a record a watch begun again re-delivers at a
// revision already pushed.
func TestAPushedReadStateIsThePersonsOwnAndOnlyWhenItMoved(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	client := watcherFor(t, h, chatPerson("alice"))
	stamped := coord.ChatReadState{Cursors: map[string]int64{"public-1": 3},
		Revision: 5, AbsentSince: chatClock}

	h.pushRead(coord.ChatReadUpdate{Handle: "alice", State: &stamped})
	frames := drainClient(client)
	if len(frames) != 1 {
		t.Fatalf("the update pushed %d frames", len(frames))
	}
	var raw map[string]any
	if err := json.Unmarshal(frames[0].Raw(), &raw); err != nil {
		t.Fatal(err)
	}
	if data, _ := raw["data"].(map[string]any); data == nil || data["absent_since"] != nil {
		t.Errorf("the push carries the reconcile's stamp: %v", raw["data"])
	}

	h.pushRead(coord.ChatReadUpdate{Handle: "alice", State: &stamped})
	older := stamped
	older.Revision = 4
	h.pushRead(coord.ChatReadUpdate{Handle: "alice", State: &older})
	if got := chatKinds(client); len(got) != 0 {
		t.Errorf("a revision already pushed was pushed again: %v", got)
	}
	h.pushRead(coord.ChatReadUpdate{Handle: "alice", State: nil})
	if got := chatKinds(client); len(got) != 0 {
		t.Errorf("a forgotten record pushed %v", got)
	}
	// AND IT IS FORGOTTEN HERE TOO: the revisions are kept per seat, and a
	// seat the company lost would otherwise be a row this hub holds for the
	// life of the process.
	h.mu.Lock()
	_, held := h.pushed["alice"]
	h.mu.Unlock()
	if held {
		t.Error("a forgotten record's revision is still held")
	}
	// AND A RECORD WRITTEN AGAIN AFTER IT WAS FORGOTTEN is pushed: its
	// revision is past every one the old record ever had.
	again := coord.ChatReadState{Revision: 9}
	h.pushRead(coord.ChatReadUpdate{Handle: "alice", State: &again})
	if got := chatKinds(client); !slices.Equal(got, []Kind{KindChatCursorMoved}) {
		t.Errorf("a record written again pushed %v", got)
	}
}

// countingReads is a read-state watch that counts each watch it was asked for
// and hands out a channel the case closes to end it.
type countingReads struct {
	mu      sync.Mutex
	watches int
	open    chan coord.ChatReadUpdate
	asked   chan struct{}
}

func (c *countingReads) WatchChatReads(ctx context.Context) (<-chan coord.ChatReadUpdate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watches++
	open := make(chan coord.ChatReadUpdate)
	c.open = open
	select {
	case c.asked <- struct{}{}:
	default:
	}
	return open, nil
}

// end closes the watch the hub holds, as a store does when its watch fails.
func (c *countingReads) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	close(c.open)
}

// THE READ-STATE WATCH RUNS ONLY WHERE SOCKETS ARE SERVED, AND IS BEGUN AGAIN
// WHEN IT ENDS.
//
// A node that serves no socket has nobody to push to, and a watch there would
// hear every flush in the company for nothing — and a watch that ended and
// was not begun again would leave every tab on this node deaf to its person's
// other tabs for the life of the process.
func TestTheReadWatchRunsWhereSocketsAreServedAndIsBegunAgain(t *testing.T) {
	t.Parallel()
	reads := &countingReads{asked: make(chan struct{}, 4)}
	h := NewChatHub(ChatHubOptions{Reads: reads})
	// UNDER A CONTEXT NOTHING ENDS, as the process starts it: only Stop
	// can end the hub here, which is what the last half of this asserts.
	h.Start(context.WithoutCancel(t.Context()))
	select {
	case <-reads.asked:
		t.Fatal("a hub serving no socket watched the read state")
	case <-time.After(100 * time.Millisecond):
	}
	if h.ServesSockets() {
		t.Fatal("a hub no socket surface was built over says it serves sockets")
	}

	h.AttachSockets()
	if !h.ServesSockets() {
		t.Fatal("a hub the socket surface was built over says it serves none")
	}
	waitFor(t, reads.asked, "the watch once sockets were served")
	reads.end()
	waitFor(t, reads.asked, "the watch begun again after it ended")
	// AND THE HUB STOPS while a watch it holds is open and the store has
	// not closed it: its stop never waits on the store, and ends the
	// watch it began.
	stopped := make(chan struct{})
	go func() { h.Stop(); close(stopped) }()
	waitFor(t, stopped, "the hub stopping while its read watch was open")
}

// waitFor fails the case unless ch yields within a bound.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never happened", what)
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
	h := hubOver(twoRooms())
	inside := watcherFor(t, h, chatPerson("alice"))
	outside := watcherFor(t, h, chatPerson("bob"))
	h.focus(inside, "private-1", true)
	h.focus(outside, "private-1", true)

	h.sweep(t.Context())

	if got := chatKinds(outside); len(got) != 0 {
		t.Errorf("a non-member was told about a private room's presence: %v", got)
	}
	got := drainClient(inside)
	if len(got) != 1 || got[0].Kind() != KindChatPresence {
		t.Fatalf("the member was sent %d frames, want one %s", len(got), KindChatPresence)
	}
	room := dataAs[ChatPresenceFrame](t, got[0]).Rooms["private-1"]
	if !slices.Equal(room.Viewing, []string{"alice"}) {
		t.Errorf("the room reports %v as viewing — somebody who cannot open a "+
			"room is not in it, whatever their tab says", room.Viewing)
	}
	if !slices.Equal(room.Typing, []string{"alice"}) {
		t.Errorf("the room reports %v as typing", room.Typing)
	}
}

// A PEER'S PROBE IS ANSWERED UNDER THE SAME RULE, by THIS node's watchers'
// own visibility — the asker is nobody it can check — and names the node so
// the asker can drop its own reply.
func TestAPresenceProbeIsAnsweredWithOnlyThePeopleEntitledToBeInTheRoom(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	inside := watcherFor(t, h, chatPerson("alice"))
	outside := watcherFor(t, h, chatPerson("bob"))
	h.focus(inside, "private-1", false)
	h.focus(outside, "private-1", false)

	raw, err := h.answer(t.Context(), mustJSON(t, chatPresenceRequest{
		Version: chatPresenceProtocol, Channels: []string{"private-1"},
	}))
	if err != nil {
		t.Fatalf("the probe was not answered: %v", err)
	}
	var reply chatPresenceReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	if got := reply.Rooms["private-1"].Viewing; !slices.Equal(got, []string{"alice"}) {
		t.Errorf("the answer names %v as viewing a private room", got)
	}
	if reply.Node != "node-a" {
		t.Errorf("the answer names node %q, so an asker cannot drop its own", reply.Node)
	}
}

// mustJSON encodes a presence payload for the wire.
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

// A PROBE THIS BUILD DOES NOT SPEAK IS ANSWERED WITH SILENCE, which the asker
// reads as a node with nobody in the room.
func TestAPresenceProbeFromAnotherProtocolIsNotAnswered(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	client := watcherFor(t, h, chatPerson("alice"))
	h.focus(client, "public-1", false)

	if _, err := h.answer(t.Context(), mustJSON(t, chatPresenceRequest{
		Version: chatPresenceProtocol + 1, Channels: []string{"public-1"},
	})); err == nil {
		t.Fatal("a probe from a protocol this build does not speak was answered")
	}
}

// AN UNCHANGED VIEW COSTS NO FRAME, and a change does.
//
// Presence is the one thing here evaluated on a timer rather than on an event,
// so without the fingerprint every open tab would receive a frame every
// interval for as long as it stayed open.
func TestAnUnchangedPresenceViewIsNotPushedAgain(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	client := watcherFor(t, h, chatPerson("alice"))
	h.focus(client, "public-1", false)

	h.sweep(t.Context())
	if got := len(drainClient(client)); got != 1 {
		t.Fatalf("the first sweep sent %d frames", got)
	}
	h.sweep(t.Context())
	if got := chatKinds(client); len(got) != 0 {
		t.Errorf("an unchanged view was pushed again: %v", got)
	}
	h.SetStatus(t.Context(), "agent-swe", "public-1", "m-1", "reading the room")
	h.sweep(t.Context())
	if got := len(drainClient(client)); got != 1 {
		t.Errorf("a changed view sent %d frames", got)
	}
}

// A TYPING FLAG EXPIRES ON ITS OWN, for the tab that stops typing and says
// nothing. Closing a socket withdraws it at once; this is the other case.
func TestATypingFlagExpiresWithoutBeingReasserted(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	client := watcherFor(t, h, chatPerson("alice"))
	h.focus(client, "public-1", true)

	watchers := h.current()
	if got := h.presenceOf(watchers, []string{"public-1"}, chatClock)["public-1"].Typing; len(got) != 1 {
		t.Fatalf("a fresh typing flag reads as %v", got)
	}
	stale := chatClock.Add(ChatTypingTTL)
	if got := h.presenceOf(watchers, []string{"public-1"}, stale)["public-1"].Typing; len(got) != 0 {
		t.Errorf("a typing flag survived its own window: %v", got)
	}
}

// A CLOSED SOCKET STOPS BEING A WATCHER, and with it stops appearing in the
// rooms it was looking at.
func TestALeavingSocketIsNoLongerPresent(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	client := watcherFor(t, h, chatPerson("alice"))
	h.focus(client, "public-1", true)
	h.leave(client)

	if got := h.presenceOf(h.current(), []string{"public-1"}, chatClock); len(got) != 0 {
		t.Errorf("a socket that closed is still in the room: %v", got)
	}
}

// A CHANGE INSIDE THE GAP IS PROBED AT THE GAP'S END, NOT AT THE NEXT TICK.
//
// The floor between two probes coalesces a burst — typing is a focus change
// per keystroke — and its own justification is that half a second is under
// what a person perceives. That holds only if what it skips is owed at the
// gap's end. Here the tick is ten seconds away, so only the owed probe can
// deliver in time.
func TestAFocusChangeInsideTheGapIsProbedAtItsEnd(t *testing.T) {
	t.Parallel()
	h := NewChatHub(ChatHubOptions{
		Rooms:            func() ChatRooms { return twoRooms() },
		NodeID:           "node-a",
		PresenceInterval: 10 * time.Second,
	})
	h.Start(t.Context())
	t.Cleanup(h.Stop)

	client := watcherFor(t, h, chatPerson("alice"))
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
		case f, open := <-c.Out():
			if !open {
				t.Fatalf("the socket was closed waiting for %s's presence", room)
			}
			if f.Kind() != KindChatPresence {
				continue
			}
			if _, named := dataAs[ChatPresenceFrame](t, f).Rooms[room]; named {
				return
			}
		case <-deadline:
			t.Fatalf("no presence frame for %s within %s", room, within)
		}
	}
}

// stubPeers is the ephemeral scatter, recording what was asked and answering
// with whatever a case put in it.
type stubPeers struct {
	mu      sync.Mutex
	subject string
	asked   []string
	reply   *chatPresenceReply

	// refuse is how many Serve attempts fail before one takes, and serves
	// counts every attempt.
	refuse int
	serves atomic.Int64
	served chan struct{}
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

func (p *stubPeers) Serve(_ context.Context, subject string, _ queue.AnswerFunc) (
	queue.Unsubscribe, error) {

	n := p.serves.Add(1)
	if subject != topics.ChatPresence {
		return nil, errors.New("served on the wrong subject")
	}
	if int(n) <= p.refuse {
		return nil, errors.New("the broker is not answering yet")
	}
	if p.served != nil {
		select {
		case p.served <- struct{}{}:
		default:
		}
	}
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
		Rooms: func() ChatRooms { return twoRooms() }, Peers: peers, NodeID: "node-a",
	})
	client := watcherFor(t, h, chatPerson("alice"))
	h.focus(client, "public-1", false)

	h.sweep(t.Context())

	peers.mu.Lock()
	subject, asked := peers.subject, peers.asked
	peers.mu.Unlock()
	if subject != topics.ChatPresence {
		t.Errorf("the probe went to %q", subject)
	}
	if !slices.Equal(asked, []string{"public-1"}) {
		t.Errorf("the probe asked about %v, want only the room somebody has open", asked)
	}
	got := drainClient(client)
	if len(got) != 1 {
		t.Fatalf("the socket was sent %d frames", len(got))
	}
	room := dataAs[ChatPresenceFrame](t, got[0]).Rooms["public-1"]
	if !slices.Equal(room.Viewing, []string{"alice", "carol"}) {
		t.Errorf("the merged view is %v — a peer's readers and this node's own "+
			"are different people, so the sets are a union", room.Viewing)
	}
	if len(room.Working) != 1 || room.Working[0].Handle != "agent-swe" {
		t.Errorf("a seat working on another node does not reach the room: %v",
			room.Working)
	}
}

// A REPLY CLAIMING TO BE THIS NODE IS DROPPED: this node's own presence is
// already in hand, first-hand and without the round trip.
func TestAPresenceReplyFromThisNodeItselfIsDropped(t *testing.T) {
	t.Parallel()
	peers := &stubPeers{reply: &chatPresenceReply{
		Version: chatPresenceProtocol, Node: "node-a",
		Rooms: map[string]ChatRoomPresence{"public-1": {Viewing: []string{"mallory"}}},
	}}
	h := NewChatHub(ChatHubOptions{
		Rooms: func() ChatRooms { return twoRooms() }, Peers: peers, NodeID: "node-a",
	})
	client := watcherFor(t, h, chatPerson("alice"))
	h.focus(client, "public-1", false)

	h.sweep(t.Context())

	got := drainClient(client)
	if len(got) != 1 {
		t.Fatalf("the socket was sent %d frames", len(got))
	}
	if room := dataAs[ChatPresenceFrame](t, got[0]).Rooms["public-1"]; slices.Contains(room.Viewing, "mallory") {
		t.Errorf("a reply claiming to be this node was merged: %v", room.Viewing)
	}
}

// A STARTED HUB ALREADY ANSWERS ITS PEERS, and one the broker refused is
// retried until it takes.
//
// The attempt is made by [ChatHub.Start] itself, so the node a boot hands back
// is in the fleet's presence before its first tick; and a node that never
// answered a probe would be invisible to every colleague's typing indicator
// for the rest of its life, with nothing to say so.
func TestAHubAnswersItsPeersFromItsStartAndRetriesARefusal(t *testing.T) {
	t.Parallel()
	ready := &stubPeers{}
	h := NewChatHub(ChatHubOptions{
		Rooms: func() ChatRooms { return twoRooms() }, Peers: ready, NodeID: "node-a",
		PresenceInterval: time.Hour,
	})
	h.Start(t.Context())
	t.Cleanup(h.Stop)
	if ready.serves.Load() != 1 {
		t.Errorf("Start returned with %d attempts to answer, want the one it makes",
			ready.serves.Load())
	}

	refused := &stubPeers{refuse: 2, served: make(chan struct{}, 1)}
	retried := NewChatHub(ChatHubOptions{
		Rooms: func() ChatRooms { return twoRooms() }, Peers: refused, NodeID: "node-b",
		PresenceInterval: 5 * time.Millisecond,
	})
	retried.Start(t.Context())
	t.Cleanup(retried.Stop)
	waitFor(t, refused.served, "the presence subject served after two refusals")
}

// ---- the working indicator --------------------------------------------- //

// `typing_status: addressed` RAISES THE INDICATOR ONLY WHERE THE RULE SAYS
// ADDRESSED.
//
// The mode is the company's own `chat.chatlet.typing_status` and the rule is
// [chatlet.AddressRule] — the same value the prompt and the delivery gate read.
// A spinner over a turn that is allowed to end in silence, and a silence after
// a spinner, are one bug, so this asserts the two halves together: a mention
// raises it and an `@channel` does not.
func TestTheWorkingIndicatorFollowsTheCompanysTypingStatus(t *testing.T) {
	t.Parallel()
	addressed := chatTrigger(string(chatlet.ReasonMention))
	broadcast := chatTrigger(string(chatlet.ReasonCollective))

	for name, expect := range map[string]struct {
		mode             config.WorkingStatus
		addressed, quiet bool
	}{
		"addressed": {mode: config.StatusAddressed, addressed: true, quiet: false},
		"always":    {mode: config.StatusAlways, addressed: true, quiet: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := hubOver(twoRooms())
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

// AND RAISING IT PUTS THE SEAT IN THE ROOM IT IS ANSWERING, under the thread
// the reply will land in, with words — and it comes down when the turn ends.
func TestRaisingTheIndicatorPutsTheSeatInTheRoomItIsAnswering(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	driver := notify.NewStatusDriver(notify.StatusOptions{
		Poster: h, Mode: notify.StatusMode(config.StatusAlways),
	})
	t.Cleanup(func() { driver.Stop(context.Background()) })

	session := driver.Begin(t.Context(), "agent-swe", "turn-1", "execute",
		chatTrigger(string(chatlet.ReasonMention)))
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

	session.End(t.Context(), false)
	if got := h.presenceOf(nil, []string{"public-1"}, chatClock)["public-1"].Working; len(got) != 0 {
		t.Errorf("the indicator survived the turn: %v", got)
	}
}

// workingIn is the room's working set once it has n entries: a WAIT rather
// than a read, because the raise is the driver's goroutine's first act rather
// than [notify.StatusDriver.Begin]'s last, so a turn never blocks on a chat
// backend to start working. Bounded and loud.
func workingIn(t *testing.T, h *ChatHub, room string, n int) []ChatWorking {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := h.presenceOf(nil, []string{room}, chatClock)[room].Working
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

// THE BACKEND NAME IS THE DOMAIN'S OWN: the driver matches it against the
// `transport` key the chat parser stamps on every trigger, so a second
// spelling raises no indicator at all — silently, on every Chatlet turn.
func TestTheIndicatorAnswersToTheNameTheChatParserStamps(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	if h.StatusBackend() != chatlet.Source {
		t.Errorf("the poster answers to %q and the parser stamps %q",
			h.StatusBackend(), chatlet.Source)
	}
	if _, ok := notify.ConversationOf(chatTrigger(string(chatlet.ReasonMention)),
		h.StatusBackend()); !ok {
		t.Error("a Chatlet trigger does not resolve to a conversation on " +
			"this backend, so no indicator could ever be raised")
	}
}

// chatTrigger is a Chatlet wake's metadata, spelled with the keys the
// parser actually stamps.
func chatTrigger(reason string) map[string]string {
	return map[string]string{
		notify.TransportField:   chatlet.Source,
		notify.ChannelField:     "public-1",
		notify.ChannelTypeField: string(chatlet.KindPublic),
		notify.ChannelNameField: "general",
		notify.MessageIDField:   "m-1",
		// EMPTY ON A TOP-LEVEL MESSAGE and stamped anyway, exactly as
		// the parser stamps it: the anchor beside it is where a reply
		// will go.
		notify.ThreadField:       "",
		notify.ThreadAnchorField: "m-1",
		notify.FollowReasonField: reason,
		notify.UserField:         "founder",
		notify.ActorField:        "founder",
		chatlet.MetaAuthorKind:   string(chatlet.AuthorHuman),
	}
}

// ---- the socket -------------------------------------------------------- //

// A SOCKET JOINS AS THE SEAT ITS PRINCIPAL ACTS AS, SAYS WHERE IT IS LOOKING,
// AND LEAVES WHEN WHAT ENDS ITS CREDENTIAL CLOSES IT.
//
// Through serveSocket, as the handler serves one: the join uses the principal
// the audience was built from, a `chat_focus` frame moves the watcher's room,
// and a move naming nobody — which closes every socket for its handshake to
// decide again — takes the watcher out of the registry, so a principal that
// moved is never shown chat as the seat it used to be.
func TestASocketJoinsAsItsSeatAndLeavesWhenItIsClosed(t *testing.T) {
	t.Parallel()
	h := hubOver(twoRooms())
	svc := newDecidingService(t, authz.NoChart{})
	alice := chatPerson("alice")
	s := serve(t, svc, socketCase{principal: alice, opened: sessionOf(alice),
		decide: resolvedAs(alice), chat: h})
	s.settled(t, 1)

	waitUntil(t, func() bool {
		watchers := h.current()
		return len(watchers) == 1 && watchers[0].viewer == "alice"
	}, "the socket never joined chat as alice")
	s.write(t, map[string]any{"kind": ChatFocusKind, "channel_id": "public-1", "typing": true})
	waitUntil(t, func() bool {
		watchers := h.current()
		return len(watchers) == 1 && watchers[0].channel == "public-1" &&
			!watchers[0].typingAt.IsZero()
	}, "the focus frame never reached the watcher")

	svc.CredentialsMoved(Moved{Everyone: true})
	if got := s.closedWith(t); got != CloseUndecided {
		t.Fatalf("the socket closed %d, want %d", got, CloseUndecided)
	}
	waitUntil(t, func() bool { return len(h.current()) == 0 },
		"a closed socket is still a chat watcher")
}
