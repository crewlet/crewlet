package estate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE SERVING HALF IS THE DOMAIN'S OWN SHAPE: the engine hands a data node's
// chat reader, writer and index to a [Backend] as they are, so an interface
// here that drifted from them would otherwise be found only by the wiring.
var (
	_ ChatReader   = (*chatlet.Reader)(nil)
	_ ChatWriter   = (*chatlet.Store)(nil)
	_ ChatSearcher = (*search.ChatIndexer)(nil)
)

// chatStream is the chat log's stream, as a floor names it.
var chatStream = floorStreams[chatDomain]

// chatDesk is one data node's chat as the router reaches it: every read names
// the viewer and the level it was asked at, every write the key it was keyed
// under and the authority it crossed with, and the search what it handed the
// index.
type chatDesk struct {
	mu sync.Mutex

	// calls is every operation asked, in order, as "<op> <viewer or actor>".
	calls []string

	// seeds is every operation key a write arrived with, and opIDs every
	// whole operation id (the erase and the prune).
	seeds []string
	opIDs []string

	// moderated, archived and retained are what the authority-bearing
	// writes arrived with: each delete's authority, each patch's, and each
	// retention's days as a person would read them.
	moderated []chatlet.MessageAuthority
	archived  []chatlet.ChannelAuthority
	retained  []string

	// readable is the rooms each viewer may read; searched is every room
	// set the index was handed.
	readable map[string][]chatlet.ReadableRoom
	searched [][]string

	// building is an index whose walk has not caught up; flips turns it
	// over while a search runs, as a rebuild asked for or a walk catching
	// up would; readyErr is a readiness the node could not read.
	building, flips bool
	readyErr        error

	// at is the copy's applied position, written the position every write
	// reports, and unvouched answers every write `unknown`, unvouched by
	// this node's ledger.
	at        statelog.Position
	written   statelog.Position
	unvouched bool
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

func (d *chatDesk) keys() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.seeds)
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

func (d *chatDesk) RoomNames(_ context.Context, viewer string,
	fresh statelog.Freshness) (chatlet.RoomNames, error) {
	served := d.read("room_names", viewer, fresh)
	return chatlet.RoomNames{Served: served, Truncated: true,
		Rooms: []chatlet.RoomName{{ID: "room-1", Name: "general", Kind: chatlet.KindPublic}}}, nil
}

func (d *chatDesk) Readable(_ context.Context, viewer string,
	fresh statelog.Freshness) ([]chatlet.ReadableRoom, error) {
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

func (d *chatDesk) Ready(context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.building, d.readyErr
}

// wrote records one write and answers it.
func (d *chatDesk) wrote(op string, actor chatlet.Actor, opID string) (chatlet.Written, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, op+" "+actor.Handle)
	if actor.OpKey != "" {
		d.seeds = append(d.seeds, actor.OpKey)
	}
	if opID != "" {
		d.opIDs = append(d.opIDs, opID)
	}
	outcome := statelog.Result{Outcome: statelog.OutcomeApplied, Position: d.written, OpID: opID}
	if d.unvouched {
		outcome = statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: opID, Unvouched: true}
	}
	return chatlet.Written{ChangeID: op + ":" + actor.OpKey + opID, Revision: 7, Outcome: outcome}, nil
}

func (d *chatDesk) CreateChannel(_ context.Context, actor chatlet.Actor, _ chatlet.NewChannel) (chatlet.Written, error) {
	return d.wrote("create_channel", actor, "")
}

func (d *chatDesk) OpenDirect(_ context.Context, actor chatlet.Actor, participants []string) (
	chatlet.Written, bool, error) {
	w, err := d.wrote("open_direct", actor, "")
	return w, len(participants) > 1, err
}

func (d *chatDesk) PatchChannel(_ context.Context, actor chatlet.Actor, _ string,
	_ chatlet.ChannelPatch, authority chatlet.ChannelAuthority) (chatlet.Written, error) {
	d.mu.Lock()
	d.archived = append(d.archived, authority)
	d.mu.Unlock()
	return d.wrote("patch_channel", actor, "")
}

func (d *chatDesk) SetMembers(_ context.Context, actor chatlet.Actor, _ string,
	_ []chatlet.Member) (chatlet.Written, error) {
	return d.wrote("set_members", actor, "")
}

func (d *chatDesk) Join(_ context.Context, actor chatlet.Actor, _ string) (chatlet.Written, error) {
	return d.wrote("join", actor, "")
}

func (d *chatDesk) Leave(_ context.Context, actor chatlet.Actor, _ string) (chatlet.Written, error) {
	return d.wrote("leave", actor, "")
}

func (d *chatDesk) Post(_ context.Context, actor chatlet.Actor, _ string, _ chatlet.NewMessage) (chatlet.Written, error) {
	return d.wrote("post", actor, "")
}

func (d *chatDesk) Reply(_ context.Context, actor chatlet.Actor, _, replyTo string,
	_ chatlet.NewMessage) (chatlet.Written, error) {
	return d.wrote("reply:"+replyTo, actor, "")
}

func (d *chatDesk) Edit(_ context.Context, actor chatlet.Actor, _, _ string, _ chatlet.EditMessage) (chatlet.Written, error) {
	return d.wrote("edit", actor, "")
}

func (d *chatDesk) Delete(_ context.Context, actor chatlet.Actor, _, _ string,
	authority chatlet.MessageAuthority) (chatlet.Written, error) {
	d.mu.Lock()
	d.moderated = append(d.moderated, authority)
	d.mu.Unlock()
	return d.wrote("delete", actor, "")
}

func (d *chatDesk) React(_ context.Context, actor chatlet.Actor, _, _, emoji string) (chatlet.Written, error) {
	return d.wrote("react:"+emoji, actor, "")
}

func (d *chatDesk) Unreact(_ context.Context, actor chatlet.Actor, _, _, emoji string) (chatlet.Written, error) {
	return d.wrote("unreact:"+emoji, actor, "")
}

func (d *chatDesk) SetRetention(_ context.Context, actor chatlet.Actor, _ string,
	days *int) (chatlet.Written, error) {
	rendered := "the company's"
	if days != nil {
		rendered = fmt.Sprintf("%d days", *days)
	}
	d.mu.Lock()
	d.retained = append(d.retained, rendered)
	d.mu.Unlock()
	return d.wrote("set_retention", actor, "")
}

func (d *chatDesk) Erase(_ context.Context, actor chatlet.Actor, opID, _ string, _ []string,
	_ string) (chatlet.Written, error) {
	return d.wrote("erase", actor, opID)
}

func (d *chatDesk) Prune(_ context.Context, actor chatlet.Actor, opID, _ string, _ time.Time) (chatlet.Written, error) {
	return d.wrote("prune", actor, opID)
}

// withRooms gives every node of f a chat of its own, and returns them by node.
func withRooms(f *fleet) map[string]*chatDesk {
	desks := map[string]*chatDesk{}
	for name, node := range f.nodes {
		desk := &chatDesk{readable: map[string][]chatlet.ReadableRoom{}}
		desks[name] = desk
		node.set(func(n *fakeNode) { n.rooms = desk })
	}
	return desks
}

var (
	jane     = chatlet.Actor{Handle: "jane", Kind: chatlet.AuthorAgent}
	maya     = chatlet.Actor{Handle: "maya", Kind: chatlet.AuthorHuman}
	linear   = statelog.Freshness{Level: statelog.ReadLinearizable}
	chatSeen = statelog.Position{Stream: "CREWLET_CHAT_LOG", Generation: 1, Seq: 41}

	// chatKey is an operation key the engine minted, as [chatlet.Actor.OpKey]
	// holds one.
	chatKey = statelog.NewOpID(time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC), "chat")
)

// keyedAs is actor carrying the caller's operation key, as a seat's turn and a
// person's retried request both hand one.
func keyedAs(actor chatlet.Actor, key string) chatlet.Actor {
	actor.OpKey = key
	return actor
}

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
		{"room_names", func() (chatlet.Served, bool, error) {
			out, err := c.RoomNames(ctx, "jane", linear)
			return out.Served, out.Truncated && len(out.Rooms) == 1 &&
				out.Rooms[0].Name == "general", err
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

// A KEYED CHAT WRITE NOBODY ANSWERED IS ASKED AGAIN, UNDER ITS KEY — and an
// unkeyed one never is.
//
// The key is the actor's ([chatlet.Actor.OpKey]) for EVERY gesture: the chat
// store derives the operation id — and a message's or a new room's — from it
// and what the gesture asks, so the next holder's ledger collapses a write the
// silent one may have made. Without one the store mints a fresh id per call,
// and a second holder would say the message twice, open a second room or take
// a reaction off again. A predicate that read anything else either repeats an
// unkeyed write or strands a keyed one.
func TestAKeyedChatWriteNobodyAnsweredIsAskedAgainUnderItsKey(t *testing.T) {
	t.Parallel()
	forever := chatlet.RetentionForever
	for _, tc := range []struct {
		name  string
		asked string
		write func(ctx context.Context, c Chat, actor chatlet.Actor) error
	}{
		{"create channel", "create_channel", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.CreateChannel(ctx, a, chatlet.NewChannel{Name: "launch", Kind: chatlet.KindPublic})
			return err
		}},
		{"open direct", "open_direct", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, created, err := c.OpenDirect(ctx, a, []string{"jane", "sam"})
			if err == nil && !created {
				return errors.New("the conversation's creation did not cross")
			}
			return err
		}},
		{"patch channel", "patch_channel", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			topic := "launch week"
			_, err := c.PatchChannel(ctx, a, "room-1", chatlet.ChannelPatch{Topic: &topic},
				chatlet.ChannelAuthority{})
			return err
		}},
		{"set members", "set_members", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.SetMembers(ctx, a, "room-1", []chatlet.Member{{Handle: "sam"}})
			return err
		}},
		{"join", "join", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.Join(ctx, a, "room-1")
			return err
		}},
		{"leave", "leave", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.Leave(ctx, a, "room-1")
			return err
		}},
		{"post", "post", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.Post(ctx, a, "room-1", chatlet.NewMessage{Body: "hi"})
			return err
		}},
		{"reply", "reply:m-1", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.Reply(ctx, a, "room-1", "m-1", chatlet.NewMessage{Body: "on it"})
			return err
		}},
		{"edit", "edit", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.Edit(ctx, a, "room-1", "m-1", chatlet.EditMessage{Body: "on it now"})
			return err
		}},
		{"delete", "delete", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.Delete(ctx, a, "room-1", "m-1", chatlet.MessageAuthority{})
			return err
		}},
		{"react", "react:eyes", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.React(ctx, a, "room-1", "m-1", "eyes")
			return err
		}},
		{"unreact", "unreact:eyes", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.Unreact(ctx, a, "room-1", "m-1", "eyes")
			return err
		}},
		{"set retention", "set_retention", func(ctx context.Context, c Chat, a chatlet.Actor) error {
			_, err := c.SetRetention(ctx, a, "room-1", &forever)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A FLEET PER HALF: the holder that answered last is asked
			// first, so the second half on the first's fleet would never
			// meet the silent holder at all.
			silentFirst := func(t *testing.T) (*fleet, *chatDesk) {
				f := newFleet(t, "data-a", "data-b")
				desks := withRooms(f)
				_, first := f.first(t, f.client)
				first.set(func(n *fakeNode) { n.silent = true })
				return f, desks[f.other(first).name]
			}
			want := tc.asked + " " + jane.Handle

			f, second := silentFirst(t)
			if err := tc.write(t.Context(), f.client.Chat(), keyedAs(jane, chatKey)); err != nil {
				t.Fatalf("a keyed %s with its first holder silent = %v, want it written "+
					"by the next", tc.name, err)
			}
			if !slices.Contains(second.asked(), want) || !slices.Equal(second.keys(), []string{chatKey}) {
				t.Fatalf("the next holder ran %v under keys %v, want %q under %s",
					second.asked(), second.keys(), want, chatKey)
			}

			f, second = silentFirst(t)
			err := tc.write(t.Context(), f.client.Chat(), jane)
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("an unkeyed %s with its first holder silent = %v, want ErrOutcomeUnknown",
					tc.name, err)
			}
			if got := second.asked(); len(got) != 0 {
				t.Errorf("an unkeyed %s was asked of a second holder (%v): a repeat with no "+
					"key is a second write", tc.name, got)
			}
		})
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
	for name, gesture := range map[string]func(c Chat) error{
		"erase": func(c Chat) error {
			_, err := c.Erase(t.Context(), maya, "op-erase", "room-1",
				[]string{"m-1"}, "a leaked key")
			return err
		},
		"prune": func(c Chat) error {
			_, err := c.Prune(t.Context(), maya, "op-prune", "room-1",
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
		second.mu.Lock()
		got := slices.Clone(second.opIDs)
		second.mu.Unlock()
		if !slices.Equal(got, []string{"op-" + name}) {
			t.Errorf("%s: the next node ran operations %v, want the caller's [op-%s]",
				name, got, name)
		}
	}
}

// A REPEATABLE CHAT WRITE NO NODE ANSWERED IS UNKNOWN, NAMING THEM — never
// "nothing ran": a node that took the request and said nothing may have run
// it, and a caller told nothing ran writes again. A read no node answered is
// still unserved, which is the claim a read can make.
func TestAChatWriteNoNodeAnsweredIsUnknownNamingThem(t *testing.T) {
	t.Parallel()
	for name, write := range map[string]func(c Chat) error{
		"a keyed post": func(c Chat) error {
			_, err := c.Post(t.Context(), keyedAs(jane, chatKey), "room-1", chatlet.NewMessage{Body: "hi"})
			return err
		},
		"an erase": func(c Chat) error {
			_, err := c.Erase(t.Context(), maya, "op-erase", "room-1", []string{"m-1"}, "a leak")
			return err
		},
	} {
		f := newFleet(t, "data-a", "data-b")
		withRooms(f)
		for _, node := range f.nodes {
			node.set(func(n *fakeNode) { n.silent = true })
		}
		err := write(f.client.Chat())
		var unserved *ErrUnserved
		if !errors.Is(err, ErrOutcomeUnknown) || errors.As(err, &unserved) {
			t.Errorf("%s no node answered = %v, want ErrOutcomeUnknown and not ErrUnserved",
				name, err)
			continue
		}
		for _, node := range []string{"data-a", "data-b"} {
			if !strings.Contains(err.Error(), node) {
				t.Errorf("%s: the unknown write %q does not name %s, which it was sent to",
					name, err, node)
			}
		}
		if _, err := f.client.Chat().Messages(t.Context(), "jane",
			chatlet.TranscriptQuery{ChannelID: "room-1"}, linear); !errors.As(err, &unserved) {
			t.Errorf("%s: a read no node answered = %v, want ErrUnserved", name, err)
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

	written, err := f.client.Chat().Post(t.Context(), keyedAs(jane, chatKey), "room-1",
		chatlet.NewMessage{Body: "done"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if written.Outcome.Unvouched {
		t.Fatalf("the post answered the first node's unvouched %+v, want the next node's",
			written.Outcome)
	}
	if got := desks[f.other(first).name].keys(); !slices.Equal(got, []string{chatKey}) {
		t.Errorf("the next node ran the post under keys %v, want [%s]", got, chatKey)
	}
}

// WHAT A CALLER WAS ADMITTED TO DO ARRIVES AS IT WAS DECIDED. The asking node
// holds the grants and the chart, so it decides a moderator's take-down and an
// archive — and the unit the archive was decided on — and the serving node
// judges each value against its own snapshot; a value lost on the way is a
// moderator refused, or worse, an author's own delete read as a moderator's. A
// retention's three meanings must arrive apart too: the company's horizon, for
// ever, and a number of days — the second is a zero, and a wire that made it
// absent would hand the room the company's horizon and the duty a year of it.
func TestWhatAChatCallerWasAdmittedToDoArrivesAsItWasDecided(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	desk := withRooms(f)["data-a"]
	c := f.client.Chat()
	ctx := t.Context()

	for _, authority := range []chatlet.MessageAuthority{{Moderate: true}, {}} {
		if _, err := c.Delete(ctx, maya, "room-1", "m-1", authority); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	archive := true
	for _, authority := range []chatlet.ChannelAuthority{{Archive: true, ArchiveUnit: "eng"}, {}} {
		if _, err := c.PatchChannel(ctx, maya, "room-1", chatlet.ChannelPatch{Archived: &archive},
			authority); err != nil {
			t.Fatalf("patch: %v", err)
		}
	}
	forever, month := chatlet.RetentionForever, 30
	for _, days := range []*int{nil, &forever, &month} {
		if _, err := c.SetRetention(ctx, maya, "room-1", days); err != nil {
			t.Fatalf("retention: %v", err)
		}
	}

	desk.mu.Lock()
	defer desk.mu.Unlock()
	if want := []chatlet.MessageAuthority{{Moderate: true}, {}}; !slices.Equal(desk.moderated, want) {
		t.Errorf("the deletes arrived with %+v, want %+v", desk.moderated, want)
	}
	if want := []chatlet.ChannelAuthority{{Archive: true, ArchiveUnit: "eng"}, {}}; !slices.Equal(desk.archived, want) {
		t.Errorf("the patches arrived with %+v, want %+v", desk.archived, want)
	}
	if want := []string{"the company's", "0 days", "30 days"}; !slices.Equal(desk.retained, want) {
		t.Errorf("the retentions arrived as %q, want %q", desk.retained, want)
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

	if _, err := f.client.Chat().Post(t.Context(), keyedAs(jane, chatKey), "room-1",
		chatlet.NewMessage{Body: "hi"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	node.set(func(n *fakeNode) { n.floors = nil })
	if _, err := f.client.Chat().Messages(t.Context(), "jane",
		chatlet.TranscriptQuery{ChannelID: "room-1"}, linear); err != nil {
		t.Fatalf("messages: %v", err)
	}
	node.mu.Lock()
	floors := slices.Clone(node.floors)
	node.floors = nil
	node.mu.Unlock()
	if len(floors) != 1 || floors[0] != desk.written {
		t.Fatalf("the read waited for %v, want the post's %v", floors, desk.written)
	}
	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.floors) != 0 {
		t.Errorf("a tracker read waited on %v, the chat log's floor", node.floors)
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
// with no hits rather than handed to an index that refuses an empty set. Each
// hit is shown under its room's name and kind from that same set. And an index
// still catching up says so on the answer it gave, rather than leaving its
// short list to be read as everything anybody said.
func TestAChatSearchReadsTheViewersRoomsBesideTheIndex(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	desk := withRooms(f)["data-a"]
	desk.at = chatSeen
	desk.readable["jane"] = []chatlet.ReadableRoom{{ID: "room-1", Kind: chatlet.KindPublic,
		Name: "launch"}, {ID: "room-2", Kind: chatlet.KindDM}}
	s := f.client.ChatSearch()

	all, err := s.Search(t.Context(), ChatQuery{Viewer: "jane", Text: "deploy", Fresh: linear})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if all.Searched != 2 || len(all.Hits) != 2 || all.Position != chatSeen || all.Building {
		t.Errorf("jane's search answered %+v, want both her rooms searched at the copy's "+
			"position, over a caught-up index", all)
	}
	// EACH HIT IS SHOWN UNDER ITS ROOM as the visible set read it: a named
	// room by its name, a direct conversation, which has none, by its kind.
	shown := map[string]chatlet.ReadableRoom{
		"room-1": {ID: "room-1", Kind: chatlet.KindPublic, Name: "launch"},
		"room-2": {ID: "room-2", Kind: chatlet.KindDM},
	}
	if !maps.Equal(all.Rooms, shown) {
		t.Errorf("the hits are shown under the rooms %+v, want %+v", all.Rooms, shown)
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
			desk.readable["jane"] = []chatlet.ReadableRoom{{ID: "room-1", Kind: chatlet.KindPublic}}
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

// A READINESS THE NODE COULD NOT READ IS BUILDING, never caught up and never a
// failed search.
//
// The flag promises "these hits may not cover every message", and a node that
// could not tell cannot promise the opposite — while failing the search over it
// would answer a seat with a broken tool where there are hits it can show.
func TestAChatSearchWhoseReadinessIsUnreadIsBuilding(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	desk := withRooms(f)["data-a"]
	desk.mu.Lock()
	desk.readable["jane"] = []chatlet.ReadableRoom{{ID: "room-1", Kind: chatlet.KindPublic}}
	desk.readyErr = errors.New("the node estate is not open")
	desk.mu.Unlock()
	got, err := f.client.ChatSearch().Search(t.Context(), ChatQuery{Viewer: "jane", Text: "deploy"})
	if err != nil {
		t.Fatalf("a search whose index's readiness could not be read failed: %v", err)
	}
	if !got.Building || len(got.Hits) != 1 {
		t.Errorf("a search whose index's readiness could not be read answered %+v, "+
			"want its hits marked as drawn from a building index", got)
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
