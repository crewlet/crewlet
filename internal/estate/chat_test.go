package estate

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE SERVING HALF IS THE DOMAIN'S OWN SHAPE: the engine hands a data node's
// chat reader and index to a [Backend] as they are, so an interface here that
// drifted from them would otherwise be found only by the wiring.
var (
	_ ChatReader   = (*chatlet.Reader)(nil)
	_ ChatSearcher = (*search.ChatIndexer)(nil)
)

// chatStream is the chat log's stream, as a floor names it.
var chatStream = floorStreams[chatDomain]

// chatDesk is one data node's chat as the router reaches it: every read names
// the viewer and the level it was asked at, and the search what it handed the
// index.
type chatDesk struct {
	mu sync.Mutex

	// calls is every operation asked, in order, as "<op> <viewer>".
	calls []string

	// readable is the rooms each viewer may read; searched is every room
	// set the index was handed.
	readable map[string][]string
	searched [][]string

	// building is an index whose walk has not caught up; flips turns it
	// over while a search runs, as a rebuild asked for or a walk catching
	// up would.
	building, flips bool

	// at is the copy's applied position.
	at statelog.Position
}

func (d *chatDesk) read(op, viewer string, fresh statelog.Freshness) chatlet.Served {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, op+" "+viewer)
	return chatlet.Served{Level: fresh.Level, Complete: true, Position: d.at}
}

func (d *chatDesk) asked() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *chatDesk) Channels(_ context.Context, viewer string, q chatlet.ChannelsQuery,
	fresh statelog.Freshness) (chatlet.ChannelListing, error) {
	served := d.read("channels", viewer, fresh)
	return chatlet.ChannelListing{Served: served, Truncated: q.IncludeArchived}, nil
}

func (d *chatDesk) Channel(_ context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	served := d.read("channel", viewer, fresh)
	return chatlet.ChannelDetail{Served: served, Channel: chatlet.Channel{ID: channelID}}, nil
}

func (d *chatDesk) ChannelNamed(_ context.Context, viewer, name string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	served := d.read("channel_named", viewer, fresh)
	return chatlet.ChannelDetail{Served: served, Channel: chatlet.Channel{Name: name}}, nil
}

func (d *chatDesk) Messages(_ context.Context, viewer string, q chatlet.TranscriptQuery,
	fresh statelog.Freshness) (chatlet.Transcript, error) {
	served := d.read("messages", viewer, fresh)
	return chatlet.Transcript{Served: served, ChannelID: q.ChannelID, NextCursor: q.Cursor}, nil
}

func (d *chatDesk) Thread(_ context.Context, viewer string, q chatlet.ThreadQuery,
	fresh statelog.Freshness) (chatlet.Thread, error) {
	served := d.read("thread", viewer, fresh)
	return chatlet.Thread{Served: served, ChannelID: q.ChannelID,
		Root: chatlet.MessageView{Message: chatlet.Message{ID: q.RootID}}}, nil
}

func (d *chatDesk) Mentions(_ context.Context, viewer string, q chatlet.MentionQuery,
	fresh statelog.Freshness) (chatlet.MentionFeed, error) {
	served := d.read("mentions", viewer, fresh)
	return chatlet.MentionFeed{Served: served, Handle: viewer, NextCursor: q.Cursor}, nil
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
	if d.flips {
		d.building = !d.building
	}
	if len(q.Channels) == 0 {
		return nil, search.ErrNoViewer
	}
	hits := make([]search.ChatHit, 0, len(q.Channels))
	for _, room := range q.Channels {
		hits = append(hits, search.ChatHit{ChannelID: room, Snippet: q.Text, Author: q.Author})
	}
	return hits, nil
}

func (d *chatDesk) Ready() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.building
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
	linear   = statelog.Freshness{Level: statelog.ReadLinearizable}
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
		read func() (chatlet.Served, bool, error)
	}{
		{"channels", func() (chatlet.Served, bool, error) {
			out, err := c.Channels(ctx, "jane", chatlet.ChannelsQuery{IncludeArchived: true}, linear)
			return out.Served, out.Truncated, err
		}},
		{"channel", func() (chatlet.Served, bool, error) {
			out, err := c.Channel(ctx, "jane", "room-1", linear)
			return out.Served, out.Channel.ID == "room-1", err
		}},
		{"channel_named", func() (chatlet.Served, bool, error) {
			out, err := c.ChannelNamed(ctx, "jane", "general", linear)
			return out.Served, out.Channel.Name == "general", err
		}},
		{"messages", func() (chatlet.Served, bool, error) {
			out, err := c.Messages(ctx, "jane", chatlet.TranscriptQuery{ChannelID: "room-1", Cursor: "c9"}, linear)
			return out.Served, out.ChannelID == "room-1" && out.NextCursor == "c9", err
		}},
		{"thread", func() (chatlet.Served, bool, error) {
			out, err := c.Thread(ctx, "jane", chatlet.ThreadQuery{ChannelID: "room-1", RootID: "m-1"}, linear)
			return out.Served, out.Root.Message.ID == "m-1", err
		}},
		{"mentions", func() (chatlet.Served, bool, error) {
			out, err := c.Mentions(ctx, "jane", chatlet.MentionQuery{Cursor: "c3"}, linear)
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

// A CHAT READ CARRIES THE CHAT LOG'S FLOOR AND NO OTHER: a position this node
// was told of on the chat log — its own seat's post, the record that woke a
// turn — holds the next chat read on whichever node answers it, the search
// included, and a tracker read waits for nothing chat wrote.
func TestAChatReadCarriesTheChatLogsFloorAndNoOther(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	withRooms(f)
	posted := statelog.Position{Stream: chatStream, Generation: 1, Seq: 77}
	f.client.Observe(posted)

	floorsOf := func() []statelog.Position {
		node.mu.Lock()
		defer node.mu.Unlock()
		floors := slices.Clone(node.floors)
		node.floors = nil
		return floors
	}
	if _, err := f.client.Chat().Messages(t.Context(), "jane",
		chatlet.TranscriptQuery{ChannelID: "room-1"}, linear); err != nil {
		t.Fatalf("messages: %v", err)
	}
	if floors := floorsOf(); len(floors) != 1 || floors[0] != posted {
		t.Fatalf("the read waited for %v, want the chat log's %v", floors, posted)
	}
	if _, err := f.client.ChatSearch().Search(t.Context(),
		ChatQuery{Viewer: "jane", Text: "deploy"}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if floors := floorsOf(); len(floors) != 1 || floors[0] != posted {
		t.Fatalf("the search waited for %v, want the chat log's %v: the rooms it may "+
			"search are rows a join may just have written", floors, posted)
	}
	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if floors := floorsOf(); len(floors) != 0 {
		t.Errorf("a tracker read waited on %v, the chat log's floor", floors)
	}
}

// THE SEARCH RESOLVES WHAT THE VIEWER MAY READ WHERE IT READS THE INDEX, and the
// index is handed that set and nothing a caller supplied: a named room is
// intersected with it, a room the viewer may not read answers nothing — exactly
// as a room nobody holds would — and a viewer who may read nothing is answered
// with no hits rather than handed to an index that refuses an empty set. And an
// index still catching up says so on the answer it gave, rather than leaving its
// short list to be read as everything anybody said.
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
	if all.Searched != 2 || len(all.Hits) != 2 || all.Position != chatSeen || all.Building {
		t.Errorf("jane's search answered %+v, want both her rooms searched at the copy's "+
			"position, over a caught-up index", all)
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
	desk.building = true
	desk.mu.Unlock()
	want := [][]string{{"room-1", "room-2"}, {"room-2"}}
	if !slices.EqualFunc(searched, want, slices.Equal[[]string]) {
		t.Errorf("the index was handed %v, want exactly the visible sets %v", searched, want)
	}

	partial, err := s.Search(t.Context(), ChatQuery{Viewer: "jane", Text: "deploy"})
	if err != nil {
		t.Fatalf("search a building index: %v", err)
	}
	if !partial.Building || partial.Searched != 2 {
		t.Errorf("a search over an index still catching up answered %+v, want its hits "+
			"marked as drawn from a building index", partial)
	}
}

// AN INDEX THAT WAS NOT READY THROUGHOUT THE SEARCH IS STILL BUILDING, whichever
// way its readiness moved while the search ran: a rebuild asked for meanwhile
// leaves hits drawn from the index of the copy an adoption replaced, and a walk
// that caught up meanwhile leaves hits drawn from the part it had reached — and
// readiness asked only before the search misses the first, only after it the
// second.
func TestAChatSearchIsBuildingUnlessItsIndexWasReadyThroughout(t *testing.T) {
	t.Parallel()
	for name, before := range map[string]bool{
		"a rebuild asked for while it ran":   false,
		"a walk that caught up while it ran": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, "data-a")
			desk := withRooms(f)["data-a"]
			desk.mu.Lock()
			desk.readable["jane"] = []string{"room-1"}
			desk.building, desk.flips = before, true
			desk.mu.Unlock()
			got, err := f.client.ChatSearch().Search(t.Context(),
				ChatQuery{Viewer: "jane", Text: "deploy"})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if !got.Building || len(got.Hits) != 1 {
				t.Errorf("a search whose index's readiness moved under it answered %+v, "+
					"want its hits marked as drawn from a building index", got)
			}
		})
	}
}

// A NODE WHOSE COMPANY'S CHAT IS NOT THE ENGINE'S OWN RUNS NO CHAT OPERATION,
// and says so rather than answering empty: a seat told "you are in no rooms"
// by a node that simply holds none would act on it.
func TestANodeWithoutChatServesNoChatOperation(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	_, err := f.client.Chat().Channels(t.Context(), "jane", chatlet.ChannelsQuery{}, linear)
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) {
		t.Fatalf("a chat read on a node running no chat = %v, want it unserved", err)
	}
	if _, err := f.client.ChatSearch().Search(t.Context(), ChatQuery{Viewer: "jane", Text: "x"}); !errors.As(err, &unserved) {
		t.Fatalf("a chat search on a node running no chat = %v, want it unserved", err)
	}
}

// ADMISSION NAMES THE CHAT HALF: a seat whose company runs Chatlet attaches
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
	asked := time.Now()
	other.client.now = func() time.Time { return asked }
	served, err = other.client.Serves(t.Context())
	if err != nil || !served.Chat {
		t.Fatalf("a remote copy running chat is served as %+v (%v), want chat", served, err)
	}

	// AND THE ANSWER IT KEEPS IS THE WHOLE ONE: admission asks again on
	// every sweep and is answered from the last ping for [admissionTrust],
	// so a kept answer that dropped a half would admit a chat seat once and
	// then never again.
	served, err = other.client.Serves(t.Context())
	if err != nil || !served.Chat {
		t.Fatalf("the remote copy's kept answer is %+v (%v), want chat", served, err)
	}
	peer := other.nodes["data-b"]
	peer.mu.Lock()
	pinged := 0
	for _, op := range peer.asked {
		if op == "admits" {
			pinged++
		}
	}
	peer.mu.Unlock()
	if pinged != 1 {
		t.Errorf("the remote copy was pinged %d times, want the second answer kept from the first", pinged)
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
