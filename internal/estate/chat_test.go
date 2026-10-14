package estate

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// chatStream is the chat log's stream, as a floor names it.
var chatStream = floorStreams[chatDomain]

// chatDesk is one data node's chat as the router reaches it: every read names
// the viewer and the level it was asked at, every write the operation it was
// keyed under, and the search what it handed the index.
type chatDesk struct {
	mu sync.Mutex

	// calls is every operation asked, in order, as "<op> <viewer or actor>".
	calls []string

	// fresh is every read level a read arrived with.
	fresh []statelog.ReadLevel

	// seeds is every call key's seed a write arrived with, and opIDs every
	// whole operation id (the erase and the prune).
	seeds []string
	opIDs []string

	// readable is the rooms each viewer may read; searched is every room
	// set the index was handed.
	readable map[string][]string
	searched [][]string

	// at is the copy's applied position, written the position every write
	// reports, and unvouched answers every write `unknown`, unvouched by
	// this node's ledger.
	at        statelog.Position
	written   statelog.Position
	unvouched bool

	// refuse answers every write with itself.
	refuse error
}

func (d *chatDesk) read(op, viewer string, fresh statelog.Freshness) chat.Served {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, op+" "+viewer)
	d.fresh = append(d.fresh, fresh.Level)
	return chat.Served{Level: fresh.Level, Complete: true, Position: d.at}
}

func (d *chatDesk) asked() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *chatDesk) Channels(_ context.Context, viewer string, q chat.ChannelsQuery,
	fresh statelog.Freshness) (chat.ChannelListing, error) {
	served := d.read("channels", viewer, fresh)
	return chat.ChannelListing{Served: served, Truncated: q.IncludeArchived}, nil
}

func (d *chatDesk) Channel(_ context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chat.ChannelDetail, error) {
	served := d.read("channel", viewer, fresh)
	return chat.ChannelDetail{Served: served, Channel: chat.Channel{ID: channelID}}, nil
}

func (d *chatDesk) ChannelNamed(_ context.Context, viewer, name string,
	fresh statelog.Freshness) (chat.ChannelDetail, error) {
	served := d.read("channel_named", viewer, fresh)
	return chat.ChannelDetail{Served: served, Channel: chat.Channel{Name: name}}, nil
}

func (d *chatDesk) Messages(_ context.Context, viewer string, q chat.TranscriptQuery,
	fresh statelog.Freshness) (chat.Transcript, error) {
	served := d.read("messages", viewer, fresh)
	return chat.Transcript{Served: served, ChannelID: q.ChannelID, NextCursor: q.Cursor}, nil
}

func (d *chatDesk) Thread(_ context.Context, viewer string, q chat.ThreadQuery,
	fresh statelog.Freshness) (chat.Thread, error) {
	served := d.read("thread", viewer, fresh)
	return chat.Thread{Served: served, ChannelID: q.ChannelID,
		Root: chat.MessageView{Message: chat.Message{ID: q.RootID}}}, nil
}

func (d *chatDesk) Mentions(_ context.Context, viewer string, q chat.MentionQuery,
	fresh statelog.Freshness) (chat.MentionFeed, error) {
	served := d.read("mentions", viewer, fresh)
	return chat.MentionFeed{Served: served, Handle: viewer, NextCursor: q.Cursor}, nil
}

func (d *chatDesk) Readable(_ context.Context, viewer string, fresh statelog.Freshness) ([]string, error) {
	d.read("readable", viewer, fresh)
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.readable[viewer]), nil
}

func (d *chatDesk) At() statelog.Position {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.at
}

func (d *chatDesk) SearchMessages(_ context.Context, q search.ChatQuery) ([]search.ChatHit, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "index")
	d.searched = append(d.searched, slices.Clone(q.Channels))
	if len(q.Channels) == 0 {
		return nil, search.ErrNoViewer
	}
	hits := make([]search.ChatHit, 0, len(q.Channels))
	for _, room := range q.Channels {
		hits = append(hits, search.ChatHit{ChannelID: room, Excerpt: q.Text, Author: q.Author})
	}
	return hits, nil
}

// wrote records one write and answers it.
func (d *chatDesk) wrote(op string, actor chat.Actor, key statelog.CallKey, opID string) (chat.Written, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, op+" "+actor.Handle)
	if key.String() != "" {
		d.seeds = append(d.seeds, key.Seed)
	}
	if opID != "" {
		d.opIDs = append(d.opIDs, opID)
	}
	if d.refuse != nil {
		return chat.Written{}, d.refuse
	}
	outcome := statelog.Result{Outcome: statelog.OutcomeApplied, Position: d.written, OpID: opID}
	if d.unvouched {
		outcome = statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: opID, Unvouched: true}
	}
	return chat.Written{ChangeID: op + ":" + key.Seed + opID, Revision: 7, Outcome: outcome}, nil
}

func (d *chatDesk) CreateChannel(_ context.Context, actor chat.Actor, in chat.NewChannel) (chat.Written, error) {
	return d.wrote("create_channel", actor, in.CallKey, "")
}

func (d *chatDesk) OpenDirect(_ context.Context, actor chat.Actor, participants []string,
	key statelog.CallKey) (chat.Written, bool, error) {
	w, err := d.wrote("open_direct", actor, key, "")
	return w, len(participants) > 1, err
}

func (d *chatDesk) PatchChannel(_ context.Context, actor chat.Actor, _ string,
	_ chat.ChannelPatch, key statelog.CallKey) (chat.Written, error) {
	return d.wrote("patch_channel", actor, key, "")
}

func (d *chatDesk) SetMembers(_ context.Context, actor chat.Actor, _ string,
	_ []chat.Member, key statelog.CallKey) (chat.Written, error) {
	return d.wrote("set_members", actor, key, "")
}

func (d *chatDesk) Join(_ context.Context, actor chat.Actor, _ string, key statelog.CallKey) (chat.Written, error) {
	return d.wrote("join", actor, key, "")
}

func (d *chatDesk) Leave(_ context.Context, actor chat.Actor, _ string, key statelog.CallKey) (chat.Written, error) {
	return d.wrote("leave", actor, key, "")
}

func (d *chatDesk) Post(_ context.Context, actor chat.Actor, _ string, in chat.NewMessage) (chat.Written, error) {
	return d.wrote("post", actor, in.CallKey, "")
}

func (d *chatDesk) Reply(_ context.Context, actor chat.Actor, _, replyTo string,
	in chat.NewMessage) (chat.Written, error) {
	return d.wrote("reply:"+replyTo, actor, in.CallKey, "")
}

func (d *chatDesk) Edit(_ context.Context, actor chat.Actor, _, _ string, in chat.EditMessage) (chat.Written, error) {
	return d.wrote("edit", actor, in.CallKey, "")
}

func (d *chatDesk) Delete(_ context.Context, actor chat.Actor, _, _ string, key statelog.CallKey) (chat.Written, error) {
	return d.wrote("delete", actor, key, "")
}

func (d *chatDesk) React(_ context.Context, actor chat.Actor, _, _, emoji string,
	key statelog.CallKey) (chat.Written, error) {
	return d.wrote("react:"+emoji, actor, key, "")
}

func (d *chatDesk) Unreact(_ context.Context, actor chat.Actor, _, _, emoji string,
	key statelog.CallKey) (chat.Written, error) {
	return d.wrote("unreact:"+emoji, actor, key, "")
}

func (d *chatDesk) Erase(_ context.Context, actor chat.Actor, opID, _ string, _ []string,
	_ string) (chat.Written, error) {
	return d.wrote("erase", actor, statelog.CallKey{}, opID)
}

func (d *chatDesk) Prune(_ context.Context, actor chat.Actor, opID, _ string, _ time.Time) (chat.Written, error) {
	return d.wrote("prune", actor, statelog.CallKey{}, opID)
}

// withRooms gives every node of f a chat of its own, and returns them by node.
func withRooms(f *fleet) map[string]*chatDesk {
	desks := map[string]*chatDesk{}
	for name, node := range f.nodes {
		desk := &chatDesk{readable: map[string][]string{}}
		desks[name] = desk
		node.set(func(n *fakeNode) { n.rooms = desk })
	}
	return desks
}

var (
	jane     = chat.Actor{Handle: "jane", Kind: chat.AuthorAgent}
	linear   = statelog.Freshness{Level: statelog.ReadLinearizable}
	turnKey  = statelog.CallKey{Seed: "turn-1", Since: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC), Tool: "post_message"}
	chatSeen = statelog.Position{Stream: "CREWLET_CHAT_LOG", Generation: 1, Seq: 41}
)

// EVERY CHAT READ IS ANSWERED FOR ITS VIEWER, at the level it asked for, by a
// data node that serves chat — so a seat on a node holding no data reads its
// rooms exactly as one on a data node does, and the room rules (what a viewer
// may see) are the serving reader's own, applied to the viewer that crossed.
func TestEveryChatReadIsAnsweredForItsViewerAtItsLevel(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	desk := withRooms(f)["data-a"]
	desk.at = chatSeen
	c := f.client.Chat()
	ctx := t.Context()
	for _, tc := range []struct {
		op   string
		read func() (chat.Served, bool, error)
	}{
		{"channels", func() (chat.Served, bool, error) {
			out, err := c.Channels(ctx, "jane", chat.ChannelsQuery{IncludeArchived: true}, linear)
			return out.Served, out.Truncated, err
		}},
		{"channel", func() (chat.Served, bool, error) {
			out, err := c.Channel(ctx, "jane", "room-1", linear)
			return out.Served, out.Channel.ID == "room-1", err
		}},
		{"channel_named", func() (chat.Served, bool, error) {
			out, err := c.ChannelNamed(ctx, "jane", "general", linear)
			return out.Served, out.Channel.Name == "general", err
		}},
		{"messages", func() (chat.Served, bool, error) {
			out, err := c.Messages(ctx, "jane", chat.TranscriptQuery{ChannelID: "room-1", Cursor: "c9"}, linear)
			return out.Served, out.ChannelID == "room-1" && out.NextCursor == "c9", err
		}},
		{"thread", func() (chat.Served, bool, error) {
			out, err := c.Thread(ctx, "jane", chat.ThreadQuery{ChannelID: "room-1", RootID: "m-1"}, linear)
			return out.Served, out.Root.Message.ID == "m-1", err
		}},
		{"mentions", func() (chat.Served, bool, error) {
			out, err := c.Mentions(ctx, "jane", chat.MentionQuery{Cursor: "c3"}, linear)
			return out.Served, out.Handle == "jane" && out.NextCursor == "c3", err
		}},
	} {
		served, carried, err := tc.read()
		if err != nil {
			t.Errorf("%s: %v", tc.op, err)
			continue
		}
		if !carried {
			t.Errorf("%s: the query did not reach the serving node whole", tc.op)
		}
		if served.Level != statelog.ReadLinearizable || served.Position != chatSeen {
			t.Errorf("%s: served %+v, want the serving copy's position at the level asked",
				tc.op, served)
		}
		if got := desk.asked(); got[len(got)-1] != tc.op+" jane" {
			t.Errorf("%s: the serving node was asked %v, want the read for jane", tc.op, got)
		}
	}
}

// A CHAT WRITE NOBODY ANSWERED IS REPEATED ONLY UNDER THE CALLER'S KEY. A post a
// seat makes in a turn carries its key, so the store derives the same message
// from it on whichever node runs it and a repeat is the same message; one with
// no key is minted afresh by the store that runs it, so a repeat would say it
// twice — and is reported as possibly landed instead.
func TestAnUnansweredChatWriteIsRepeatedOnlyUnderItsKey(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	desks := withRooms(f)
	name, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.silent = true })
	second := desks[f.other(first).name]

	_, err := f.client.Chat().Post(t.Context(), jane, "room-1", chat.NewMessage{Body: "hi"})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("an unkeyed post %s never answered = %v, want ErrOutcomeUnknown", name, err)
	}
	if len(second.asked()) != 0 {
		t.Fatalf("an unkeyed post was asked again of the next node (%v): a repeat with no "+
			"key is a second message", second.asked())
	}

	written, err := f.client.Chat().Post(t.Context(), jane, "room-1",
		chat.NewMessage{Body: "hi", CallKey: turnKey})
	if err != nil {
		t.Fatalf("a keyed post %s never answered = %v, want it asked of the next node", name, err)
	}
	if written.Outcome.Outcome != statelog.OutcomeApplied {
		t.Errorf("the keyed post answered %+v", written.Outcome)
	}
	if got := second.seeds; !slices.Equal(got, []string{"turn-1"}) {
		t.Errorf("the next node ran the post under keys %v, want the caller's [turn-1]", got)
	}

	// AND EVERY OTHER GESTURE ON THE SAME RULE: keyed, it moves on.
	for _, gesture := range []struct {
		name string
		do   func() error
	}{
		{"reply", func() error {
			_, err := f.client.Chat().Reply(t.Context(), jane, "room-1", "m-1",
				chat.NewMessage{Body: "on it", CallKey: turnKey})
			return err
		}},
		{"react", func() error {
			_, err := f.client.Chat().React(t.Context(), jane, "room-1", "m-1", "eyes", turnKey)
			return err
		}},
		{"unreact", func() error {
			_, err := f.client.Chat().Unreact(t.Context(), jane, "room-1", "m-1", "eyes", turnKey)
			return err
		}},
		{"join", func() error {
			_, err := f.client.Chat().Join(t.Context(), jane, "room-1", turnKey)
			return err
		}},
		{"open_direct", func() error {
			_, created, err := f.client.Chat().OpenDirect(t.Context(), jane, []string{"jane", "sam"}, turnKey)
			if err == nil && !created {
				return errors.New("the conversation's creation did not cross")
			}
			return err
		}},
	} {
		if err := gesture.do(); err != nil {
			t.Errorf("a keyed %s whose first node never answered = %v", gesture.name, err)
		}
	}
	calls := second.asked()
	for _, want := range []string{"reply:m-1 jane", "react:eyes jane", "unreact:eyes jane",
		"join jane", "open_direct jane"} {
		if !slices.Contains(calls, want) {
			t.Errorf("the next node was never asked %q; it was asked %v", want, calls)
		}
	}
}

// AN ERASE AND A PRUNE CARRY THE CALLER'S OWN OPERATION ID, as a purge does, so
// one nobody answered is asked again under it — and the ledger of the node that
// runs it answers a repeat with the first outcome.
//
// A FLEET PER GESTURE: a node that went silent is asked LAST for a while, so a
// second gesture through the same router would never reach it and would prove
// nothing about its own class.
func TestAnUnansweredEraseOrPruneRepeatsUnderTheSameOperation(t *testing.T) {
	t.Parallel()
	operator := chat.Actor{Handle: "maya", Kind: chat.AuthorOperator, OperatorID: "tok-1"}
	for name, gesture := range map[string]func(c Chat) error{
		"erase": func(c Chat) error {
			_, err := c.Erase(t.Context(), operator, "op-erase", "room-1",
				[]string{"m-1"}, "a leaked key")
			return err
		},
		"prune": func(c Chat) error {
			_, err := c.Prune(t.Context(), operator, "op-prune", "room-1",
				time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
			return err
		},
	} {
		f := newFleet(t, "data-a", "data-b")
		desks := withRooms(f)
		_, first := f.first(t, f.client)
		first.set(func(n *fakeNode) { n.silent = true })
		second := desks[f.other(first).name]
		if err := gesture(f.client.Chat()); err != nil {
			t.Errorf("%s whose first node never answered = %v, want it asked of the next", name, err)
			continue
		}
		if got := second.opIDs; !slices.Equal(got, []string{"op-" + name}) {
			t.Errorf("%s: the next node ran operations %v, want the caller's [op-%s]",
				name, got, name)
		}
	}
}

// A WRITE ONE NODE'S LEDGER CANNOT VOUCH FOR IS ASKED OF THE NEXT, under the
// same key: the outcome a chat write carries is found where a tracker write's
// is, and another node's ledger may hold the row this one lost.
func TestAnUnvouchedChatWriteIsAskedOfTheNextNode(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	desks := withRooms(f)
	_, first := f.first(t, f.client)
	desks[first.name].unvouched = true

	written, err := f.client.Chat().Post(t.Context(), jane, "room-1",
		chat.NewMessage{Body: "done", CallKey: turnKey})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if written.Outcome.Unvouched {
		t.Fatalf("the post answered the first node's unvouched %+v, want the next node's",
			written.Outcome)
	}
	if got := desks[f.other(first).name].seeds; !slices.Equal(got, []string{"turn-1"}) {
		t.Errorf("the next node ran the post under keys %v, want [turn-1]", got)
	}
}

// A CHAT WRITE RAISES THE CHAT LOG'S FLOOR AND NO OTHER, so the seat's next
// chat read — on whichever node answers it — includes the write, and its next
// tracker read waits for nothing chat wrote.
func TestAChatWritesPositionIsTheNextChatReadsFloor(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	desk := withRooms(f)["data-a"]
	desk.written = statelog.Position{Stream: chatStream, Generation: 1, Seq: 77}

	if _, err := f.client.Chat().Post(t.Context(), jane, "room-1",
		chat.NewMessage{Body: "hi", CallKey: turnKey}); err != nil {
		t.Fatalf("post: %v", err)
	}
	node.set(func(n *fakeNode) { n.floors = nil })
	if _, err := f.client.Chat().Messages(t.Context(), "jane",
		chat.TranscriptQuery{ChannelID: "room-1"}, linear); err != nil {
		t.Fatalf("messages: %v", err)
	}
	node.mu.Lock()
	floors := slices.Clone(node.floors)
	node.floors = nil
	node.mu.Unlock()
	if len(floors) != 1 || floors[0] != desk.written {
		t.Fatalf("the read waited for %v, want the post's %v", floors, desk.written)
	}
	if _, err := f.client.Work().Tasks(t.Context(), tracker.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.floors) != 0 {
		t.Errorf("a tracker read waited on %v, the chat log's floor", node.floors)
	}
}

// THE SEARCH RESOLVES WHAT THE VIEWER MAY READ WHERE IT READS THE INDEX, and the
// index is handed that set and nothing a caller supplied: a named room is
// intersected with it, a room the viewer may not read answers nothing — exactly
// as a room nobody holds would — and a viewer who may read nothing is answered
// with no hits rather than handed to an index that refuses an empty set.
func TestAChatSearchReadsTheViewersRoomsBesideTheIndex(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	desk := withRooms(f)["data-a"]
	desk.at = chatSeen
	desk.readable["jane"] = []string{"room-1", "room-2"}
	s := f.client.ChatSearch()

	all, err := s.Search(t.Context(), ChatQuery{Viewer: "jane", Text: "deploy", Fresh: linear})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if all.Searched != 2 || len(all.Hits) != 2 || all.Position != chatSeen {
		t.Errorf("jane's search answered %+v, want both her rooms searched at the copy's position",
			all)
	}

	one, err := s.Search(t.Context(), ChatQuery{Viewer: "jane", Text: "deploy", Channel: "room-2"})
	if err != nil {
		t.Fatalf("search one room: %v", err)
	}
	if one.Searched != 1 || len(one.Hits) != 1 || one.Hits[0].ChannelID != "room-2" {
		t.Errorf("a search narrowed to room-2 answered %+v", one)
	}

	hidden, err := s.Search(t.Context(), ChatQuery{Viewer: "jane", Text: "deploy", Channel: "secret"})
	if err != nil {
		t.Fatalf("search a hidden room: %v", err)
	}
	nobody, err := s.Search(t.Context(), ChatQuery{Viewer: "sam", Text: "deploy"})
	if err != nil {
		t.Fatalf("search for a viewer who may read nothing = %v, want no hits", err)
	}
	for name, got := range map[string]ChatSearchAnswer{"a hidden room": hidden, "nobody's rooms": nobody} {
		if got.Searched != 0 || got.Hits == nil || len(got.Hits) != 0 {
			t.Errorf("%s answered %+v, want no rooms searched and an empty, non-nil list",
				name, got)
		}
	}

	desk.mu.Lock()
	searched := slices.Clone(desk.searched)
	desk.mu.Unlock()
	want := [][]string{{"room-1", "room-2"}, {"room-2"}}
	if !slices.EqualFunc(searched, want, slices.Equal[[]string]) {
		t.Errorf("the index was handed %v, want exactly the visible sets %v", searched, want)
	}
}

// A NODE WHOSE COMPANY'S CHAT IS NOT THE ENGINE'S OWN RUNS NO CHAT OPERATION,
// and says so rather than answering empty: a seat told "you are in no rooms"
// by a node that simply holds none would act on it.
func TestANodeWithoutChatServesNoChatOperation(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	_, err := f.client.Chat().Channels(t.Context(), "jane", chat.ChannelsQuery{}, linear)
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) {
		t.Fatalf("a chat read on a node running no chat = %v, want it unserved", err)
	}
	if _, err := f.client.ChatSearch().Search(t.Context(), ChatQuery{Viewer: "jane", Text: "x"}); !errors.As(err, &unserved) {
		t.Fatalf("a chat search on a node running no chat = %v, want it unserved", err)
	}
}

// ADMISSION NAMES THE CHAT HALF: a seat whose company runs native chat attaches
// only to a copy that serves it, whichever node that copy is on.
func TestAdmissionSaysWhetherTheCopyServesChat(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	served, err := f.client.Serves(t.Context())
	if err != nil || served.Chat || !served.Tracker {
		t.Fatalf("a copy running no chat is served as %+v (%v), want the tracker and no chat",
			served, err)
	}

	local := &fakeNode{name: "data-self", units: chartOf("self"), rooms: &chatDesk{}}
	served, err = newFleet(t).router(t, "data-self", local).Serves(t.Context())
	if err != nil || !served.Chat {
		t.Fatalf("this node's own copy running chat is served as %+v (%v), want chat", served, err)
	}

	other := newFleet(t, "data-b")
	withRooms(other)
	served, err = other.client.Serves(t.Context())
	if err != nil || !served.Chat {
		t.Fatalf("a remote copy running chat is served as %+v (%v), want chat", served, err)
	}
}

// A ROOM ASKED "HERE" IS ANSWERED BY THIS NODE'S OWN COPY, EVEN LAGGING.
//
// The live channel decides who may see a record this node's applier has just
// committed: that copy holds it by construction, and a peer may not yet. The
// ordinary walk passes a lagging copy over for a peer's — and a burst is what
// makes a copy lag — so every decision during one became a round trip to a
// node that might not have applied the record at all. ChannelHere asks this
// copy first and the peer not at all; Channel, beside it, still asks the peer
// whose copy does not lag first, which is what every other read wants. And a
// copy that is out of service is passed over either way: lagging is distance,
// out of service is wrong.
func TestARoomAskedHereIsAnsweredByThisNodesOwnCopyEvenLagging(t *testing.T) {
	t.Parallel()
	peered := newFleet(t, "data-a")
	desks := withRooms(peered)
	own := &chatDesk{}
	local := &fakeNode{name: "data-self", units: chartOf("self"), notReady: true, rooms: own}
	r := peered.router(t, "data-self", local)
	fresh := statelog.Freshness{Level: statelog.ReadStale, MinPosition: chatSeen}

	if _, err := r.Chat().ChannelHere(t.Context(), "jane", "room-1", fresh); err != nil {
		t.Fatalf("channel here: %v", err)
	}
	if got := own.asked(); !slices.Equal(got, []string{"channel jane"}) {
		t.Errorf("this node's lagging copy answered %v, want the room asked here", got)
	}
	if got := desks["data-a"].asked(); len(got) != 0 {
		t.Errorf("a room asked here went to a peer as %v", got)
	}

	if _, err := r.Chat().Channel(t.Context(), "jane", "room-1", fresh); err != nil {
		t.Fatalf("channel: %v", err)
	}
	if got := desks["data-a"].asked(); !slices.Equal(got, []string{"channel jane"}) {
		t.Errorf("the ordinary read asked the peer %v, want it asked first", got)
	}

	local.set(func(n *fakeNode) { n.outOfService = true })
	if _, err := r.Chat().ChannelHere(t.Context(), "jane", "room-2", fresh); err != nil {
		t.Fatalf("channel here, out of service: %v", err)
	}
	if got := own.asked(); len(got) != 1 {
		t.Errorf("a copy out of service answered a room asked here: %v", got)
	}
	if got := desks["data-a"].asked(); len(got) != 2 {
		t.Errorf("with this copy out of service the peer was asked %v, want it to answer", got)
	}
}
