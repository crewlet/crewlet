package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// The room every case here addresses. An ID that is a uuid, because that is
// what the tools take without a lookup, and a NAME beside it so the
// name-resolution path is exercised by the tests that want it.
const (
	chatRoomID   = "6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f"
	chatRoomName = "eng"
)

// posted is one message the fake took, with what decided its id.
type postedMessage struct {
	channelID string
	replyTo   string
	actor     chat.Actor
	in        chat.NewMessage
}

// id is the message id the domain would derive for this call, which is what
// makes "two messages" and "the same message twice" assertable without a
// database.
func (p postedMessage) id(t *testing.T) string {
	t.Helper()
	got, err := chat.TurnMessageID(p.actor.TurnID, p.channelID, p.in.Ordinal)
	if err != nil {
		t.Fatalf("derive the message id: %v", err)
	}
	return got.String()
}

type fakeChat struct {
	posts     []postedMessage
	reactions []string
	joined    []string
	left      []string

	// awaited is every position [builtin.ChatDeps.Await] was handed, in
	// order, so a write that waited for the wrong position — or a read
	// that waited at all — is visible rather than merely plausible.
	awaited []uint64

	// levels is every read level these tools asked for, so a tool that
	// stopped naming one is visible.
	levels []statelog.ReadLevel

	// reads counts every call that reached the read side, which is what
	// "no read awaits" is asserted against.
	reads int

	writeErr error
	readErr  error

	searched []builtin.ChatSearch
	viewers  []string
}

func newFakeChat() *fakeChat { return &fakeChat{} }

func (f *fakeChat) Channels(_ context.Context, viewer string, q chat.ChannelsQuery,
	fresh statelog.Freshness) (chat.ChannelListing, error) {

	f.levels = append(f.levels, fresh.Level)
	f.viewers = append(f.viewers, viewer)
	f.reads++
	if f.readErr != nil {
		return chat.ChannelListing{}, f.readErr
	}
	return chat.ChannelListing{
		Channels: []chat.ChannelSummary{{
			Channel: chat.Channel{ID: chatRoomID, Name: chatRoomName, Kind: chat.KindPublic},
			Unread:  7,
		}},
	}, nil
}

func (f *fakeChat) Messages(_ context.Context, viewer string, q chat.TranscriptQuery,
	fresh statelog.Freshness) (chat.Transcript, error) {

	f.levels = append(f.levels, fresh.Level)
	f.viewers = append(f.viewers, viewer)
	f.reads++
	if f.readErr != nil {
		return chat.Transcript{}, f.readErr
	}
	return chat.Transcript{ChannelID: q.ChannelID}, nil
}

func (f *fakeChat) Thread(_ context.Context, viewer string, q chat.ThreadQuery,
	fresh statelog.Freshness) (chat.Thread, error) {

	f.levels = append(f.levels, fresh.Level)
	f.viewers = append(f.viewers, viewer)
	f.reads++
	if f.readErr != nil {
		return chat.Thread{}, f.readErr
	}
	return chat.Thread{ChannelID: q.ChannelID}, nil
}

func (f *fakeChat) Post(_ context.Context, actor chat.Actor, channelID string,
	in chat.NewMessage) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.posts = append(f.posts, postedMessage{channelID: channelID, actor: actor, in: in})
	return f.wrote(20 + uint64(len(f.posts))), nil
}

func (f *fakeChat) Reply(_ context.Context, actor chat.Actor, channelID, replyTo string,
	in chat.NewMessage) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.posts = append(f.posts, postedMessage{
		channelID: channelID, replyTo: replyTo, actor: actor, in: in,
	})
	return f.wrote(20 + uint64(len(f.posts))), nil
}

func (f *fakeChat) OpenDirect(_ context.Context, _ chat.Actor, participants []string) (
	chat.Written, bool, error) {

	if f.writeErr != nil {
		return chat.Written{}, false, f.writeErr
	}
	room := chat.Written{
		Channel: chat.Channel{
			ID: "dm-" + strings.Join(participants, "-"), Kind: chat.KindDM,
		},
		Revision: 3,
		Outcome: statelog.Result{
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "CREWLET_CHAT_LOG", Seq: 11},
		},
	}
	return room, true, nil
}

func (f *fakeChat) React(_ context.Context, _ chat.Actor, _, messageID, emoji string) (
	chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.reactions = append(f.reactions, messageID+emoji)
	return f.wrote(31), nil
}

func (f *fakeChat) Unreact(_ context.Context, _ chat.Actor, _, messageID, emoji string) (
	chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.reactions = append(f.reactions, "-"+messageID+emoji)
	return f.wrote(32), nil
}

func (f *fakeChat) Join(_ context.Context, _ chat.Actor, channelID string) (chat.Written, error) {
	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.joined = append(f.joined, channelID)
	return f.wrote(33), nil
}

func (f *fakeChat) Leave(_ context.Context, _ chat.Actor, channelID string) (chat.Written, error) {
	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.left = append(f.left, channelID)
	return f.wrote(34), nil
}

func (f *fakeChat) SearchMessages(_ context.Context, viewer string, q builtin.ChatSearch) (
	[]search.ChatHit, error) {

	f.viewers = append(f.viewers, viewer)
	f.reads++
	f.searched = append(f.searched, q)
	if f.readErr != nil {
		return nil, f.readErr
	}
	return []search.ChatHit{{MessageID: "m1", ChannelID: chatRoomID, Excerpt: "the backoff"}}, nil
}

func (f *fakeChat) wrote(seq uint64) chat.Written {
	return chat.Written{
		Channel:  chat.Channel{ID: chatRoomID, Name: chatRoomName},
		Revision: seq,
		Outcome: statelog.Result{
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "CREWLET_CHAT_LOG", Seq: seq},
		},
	}
}

func (f *fakeChat) await(_ context.Context, at statelog.Position) error {
	f.awaited = append(f.awaited, at.Seq)
	return nil
}

// chatDeps is the wiring every case here starts from: both halves, the index,
// and the settle seam armed so the waiting can be asserted.
func chatDeps(f *fakeChat) builtin.ChatDeps {
	return builtin.ChatDeps{Reader: f, Writer: f, Search: f, Await: f.await}
}

func chatRegistry(t *testing.T, deps builtin.ChatDeps) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{Chat: deps}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// callChat runs one chat tool for a turn the caller pins, because every
// interesting case here is about WHICH turn made the call.
func callChat(t *testing.T, reg *tools.Registry, name string, turn *turnctx.Turn,
	args map[string]any) tools.Result {

	t.Helper()
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	seated, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("%s cannot know who called it", name)
	}
	got, err := seated.CallForTurn(t.Context(), turn, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

// ---- the cases -------------------------------------------------------- //

// SAYING SOMETHING IS THE ANSWER, AND ONLY SAYING SOMETHING IS. A turn woken
// because somebody spoke to it answers in the thread, in the room or in a DM —
// and the gate has to know that, or the turn is corrected for having done what
// it was woken to do. The other half is what this protects: if a REACTION
// counted, every addressed turn could discharge its obligation with a thumb,
// and joining a room would close out a question nobody had answered.
func TestOnlyTheThreePostingToolsCountAsChatDeliveries(t *testing.T) {
	t.Parallel()
	reg := chatRegistry(t, chatDeps(newFakeChat()))
	deliveries := reg.Deliveries()

	for _, name := range builtin.ChatWrites() {
		// AND ON CHAT'S OWN SOURCE, not just "somewhere". The gate
		// compares a delivery's surface against the source the inbound
		// wake carried, so a second spelling here makes every native
		// chat obligation unreachable and the check silently falls back
		// to "any delivery counts".
		if deliveries[name] != chat.Source {
			t.Errorf("%s delivers to %q, want %q", name, deliveries[name], chat.Source)
		}
	}
	for _, name := range []string{
		builtin.ReactToMessageTool, builtin.JoinChannelTool, builtin.LeaveChannelTool,
		builtin.ReadChannelTool, builtin.ListChannelsTool, builtin.SearchMessagesTool,
	} {
		if surface, ok := deliveries[name]; ok {
			t.Errorf("%s counts as a delivery to %q, so a turn that never "+
				"answered would pass", name, surface)
		}
	}
}

// A TURN MAY LEGITIMATELY SAY TWO THINGS, and a re-run of that turn must say
// each of them once.
//
// The whole of native chat's idempotency rests on this pair: the id is
// (turn, channel, ordinal), so the ordinal is what keeps a turn's second
// remark from overwriting its first, and the SEED is the work key rather than
// the run id, so a redelivered trigger — which is ordinary — derives the same
// two ids instead of a second copy of the conversation.
func TestATurnThatPostsTwiceWritesTwoMessagesAndARerunWritesOne(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))

	// The first run: two posting calls, with the loop's record growing
	// between them exactly as the surface hands it over.
	first := turnFor(t, "agent-cto")
	callChat(t, reg, builtin.PostMessageTool, first, map[string]any{
		"channel": chatRoomID, "body": "on it",
	})
	callChat(t, reg, builtin.ReplyInThreadTool,
		first.WithCalls([]turnctx.Call{{Name: builtin.PostMessageTool}}),
		map[string]any{"channel": chatRoomID, "reply_to": "m1", "body": "and here is why"})

	if len(f.posts) != 2 {
		t.Fatalf("a turn that said two things wrote %d messages", len(f.posts))
	}
	if a, b := f.posts[0].id(t), f.posts[1].id(t); a == b {
		t.Fatalf("a turn's second remark took the first one's id (%s), so the "+
			"applier would decline it and the turn would believe it said two "+
			"things", a)
	}
	// AND THE ORDINAL NUMBERS THE GESTURE RATHER THAN THE TOOL: a room
	// post and a thread reply share one id space, so the reply must be
	// call one and not a second call zero.
	if f.posts[0].in.Ordinal != 0 || f.posts[1].in.Ordinal != 1 {
		t.Fatalf("ordinals %d and %d, want 0 and 1",
			f.posts[0].in.Ordinal, f.posts[1].in.Ordinal)
	}

	// The re-run: the same unit of work, a NEW run id, the same two calls.
	rerun := turnFor(t, "agent-cto")
	rerun.RunID = "run-2"
	before := len(f.posts)
	callChat(t, reg, builtin.PostMessageTool, rerun, map[string]any{
		"channel": chatRoomID, "body": "on it",
	})
	callChat(t, reg, builtin.ReplyInThreadTool,
		rerun.WithCalls([]turnctx.Call{{Name: builtin.PostMessageTool}}),
		map[string]any{"channel": chatRoomID, "reply_to": "m1", "body": "and here is why"})

	for i := before; i < len(f.posts); i++ {
		if got, want := f.posts[i].id(t), f.posts[i-before].id(t); got != want {
			t.Errorf("the re-run's message %d derived %s, want the first run's "+
				"%s — a redelivered trigger would post the whole conversation "+
				"again", i-before, got, want)
		}
	}
}

// A FAILED POST DOES NOT CONSUME AN ORDINAL, because it wrote nothing.
//
// A model whose message was refused — a body over the cap, a room it named
// wrongly — fixes it and calls again, and that retry is still the turn's FIRST
// message. Counting the refusal would leave a hole in the numbering and make
// the ids depend on how many times the model got it wrong, which a re-run
// reproduces only by failing in exactly the same way.
func TestAFailedPostDoesNotConsumeAnOrdinal(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	f.writeErr = fmt.Errorf("%w: body: a message is 40960 bytes and the maximum "+
		"is %d", chat.ErrInvalid, chat.MaxBody)
	reg := chatRegistry(t, chatDeps(f))

	turn := turnFor(t, "agent-cto")
	refused := callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": chatRoomID, "body": strings.Repeat("x", 12),
	})
	if !refused.Failed {
		t.Fatal("an oversized body was accepted")
	}

	f.writeErr = nil
	callChat(t, reg, builtin.PostMessageTool,
		turn.WithCalls([]turnctx.Call{{Name: builtin.PostMessageTool, Failed: true}}),
		map[string]any{"channel": chatRoomID, "body": "shorter"})

	if len(f.posts) != 1 {
		t.Fatalf("%d messages reached the store after one refusal and one post",
			len(f.posts))
	}
	if f.posts[0].in.Ordinal != 0 {
		t.Errorf("the retry took ordinal %d: a call that wrote nothing consumed "+
			"a number, so a re-run derives a different id unless it fails the "+
			"same way", f.posts[0].in.Ordinal)
	}
}

// A VALUE REFUSAL NAMES THE FIELD AND DOES NOT READ AS A TRANSIENT FAILURE.
//
// The two arms say opposite things to a model: "refused that" is something it
// can fix and call again, while the fall-through says the write may or may not
// have landed and must not be reported as done. Reporting a body over the cap
// as the second sends the model to report a message it never sent.
func TestABodyOverTheCapIsRefusedNamingTheField(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	f.writeErr = fmt.Errorf("%w: body: a message is 40960 bytes and the maximum "+
		"is %d — prose longer than that is a document", chat.ErrInvalid, chat.MaxBody)
	reg := chatRegistry(t, chatDeps(f))

	got := callChat(t, reg, builtin.PostMessageTool, turnFor(t, "agent-cto"),
		map[string]any{"channel": chatRoomID, "body": "too long"})
	if !got.Failed {
		t.Fatal("an oversized body was accepted")
	}
	if !strings.Contains(got.Output, "refused that") {
		t.Errorf("a refusal about the value was reported as a write that may "+
			"have landed: %s", got.Output)
	}
	if !strings.Contains(got.Output, "body") {
		t.Errorf("the refusal does not name the field the model must change: %s",
			got.Output)
	}
}

// THE AUTHOR IS THE TURN'S SEAT AND NEVER AN ARGUMENT. A model that could name
// its own author could speak as anybody, and a room where that is possible is
// not a conversation — the name beside the words is the whole of what a reader
// has.
func TestAPostIsAttributedToTheTurnsSeatAndNotToAnArgument(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))

	callChat(t, reg, builtin.PostMessageTool, turnFor(t, "agent-cto"), map[string]any{
		"channel": chatRoomID, "body": "shipping tomorrow",
		"author": "agent-ceo", "handle": "agent-ceo", "actor": "agent-ceo",
		"author_kind": "human",
	})
	if len(f.posts) != 1 {
		t.Fatalf("%d messages reached the store", len(f.posts))
	}
	actor := f.posts[0].actor
	if actor.Handle != "agent-cto" {
		t.Errorf("the message was written as %q: an argument moved the author",
			actor.Handle)
	}
	if actor.Kind != chat.AuthorAgent {
		t.Errorf("a seat's message was written as %q, want %q",
			actor.Kind, chat.AuthorAgent)
	}
	// AND THE SEED IS THE WORK KEY, not the run: the run id is minted
	// fresh on every attempt, so an id derived from it makes every
	// redelivery a second copy of what the turn said.
	if actor.TurnID != "wk-1" {
		t.Errorf("the message id is seeded from %q, want the turn's work key — "+
			"a re-run would post everything twice", actor.TurnID)
	}
}

// EVERY WRITE WAITS FOR ITS OWN POSITION, AND NO READ WAITS AT ALL.
//
// A write lands on the fleet's log and every read here is served from this
// node's own rows, so a seat that posts and then reads the room in the same
// turn must be made to see its own message — a model that cannot find what it
// just said says it again. Waiting before a READ would buy nothing: there is
// no position to wait for that the caller did not already produce.
func TestEveryChatWriteAwaitsItsOwnPositionAndNoReadDoes(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")

	for _, c := range []struct {
		name string
		args map[string]any
		want uint64
	}{
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "body": "a"}, 21},
		{builtin.ReplyInThreadTool,
			map[string]any{"channel": chatRoomID, "reply_to": "m1", "body": "b"}, 22},
		{builtin.ReactToMessageTool,
			map[string]any{"channel": chatRoomID, "message": "m1", "emoji": ":eyes:"}, 31},
		{builtin.JoinChannelTool, map[string]any{"channel": chatRoomID}, 33},
		{builtin.LeaveChannelTool, map[string]any{"channel": chatRoomID}, 34},
	} {
		before := len(f.awaited)
		if got := callChat(t, reg, c.name, turn, c.args); got.Failed {
			t.Fatalf("%s failed: %s", c.name, got.Output)
		}
		switch {
		case len(f.awaited) != before+1:
			t.Errorf("%s waited %d times, want exactly its own write",
				c.name, len(f.awaited)-before)
		case f.awaited[before] != c.want:
			t.Errorf("%s waited for position %d, want %d — a tool that waits "+
				"for the wrong write lets the next read miss it",
				c.name, f.awaited[before], c.want)
		}
	}

	// A DIRECT MESSAGE WAITS TWICE, and the first wait is load-bearing:
	// the post decides from this node's own rows, so one issued before the
	// conversation's own create has been applied here finds no room at all.
	before := len(f.awaited)
	if got := callChat(t, reg, builtin.SendDMTool, turn, map[string]any{
		"to": []any{"agent-ceo"}, "body": "between us",
	}); got.Failed {
		t.Fatalf("send_dm failed: %s", got.Output)
	}
	if waits := f.awaited[before:]; len(waits) != 2 || waits[0] != 11 {
		t.Errorf("send_dm waited %v, want the conversation's create (11) and "+
			"then its own message", waits)
	}

	before = len(f.awaited)
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{builtin.ReadChannelTool, map[string]any{"channel": chatRoomID}},
		{builtin.ReadChannelTool, map[string]any{"channel": chatRoomID, "thread": "m1"}},
		{builtin.ListChannelsTool, nil},
		{builtin.SearchMessagesTool, map[string]any{"text": "backoff"}},
	} {
		if got := callChat(t, reg, c.name, turn, c.args); got.Failed {
			t.Fatalf("%s failed: %s", c.name, got.Output)
		}
	}
	if extra := f.awaited[before:]; len(extra) != 0 {
		t.Errorf("a read waited for %v — a read has no write of its own to "+
			"wait for, and waiting spends the turn's budget on nothing", extra)
	}
}

// EVERY READ ASKS FOR THE SEAT'S OWN LEVEL. A seat has no screen on which to
// notice a stale answer and its reads DECIDE things: one that reads a room
// without its own last message posts it again.
func TestEveryChatReadAsksForTheSeatLevel(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")

	callChat(t, reg, builtin.ReadChannelTool, turn, map[string]any{"channel": chatRoomID})
	callChat(t, reg, builtin.ReadChannelTool, turn,
		map[string]any{"channel": chatRoomID, "thread": "m1"})
	callChat(t, reg, builtin.ListChannelsTool, turn, nil)

	want := statelog.DefaultReadLevel(statelog.SurfaceSeat)
	if len(f.levels) == 0 {
		t.Fatal("no read named a level")
	}
	for i, level := range f.levels {
		if level != want {
			t.Errorf("read %d asked for %q, want the seat's own %q", i, level, want)
		}
	}
}

// A WORKER IS DENIED EVERY CHAT WRITE THAT REACHES SOMEBODY, and holds the two
// that only move its own subscription.
//
// The sub-agent guard asks [mcp.WritesToSharedSurface], and the question that
// flag asks is whether a second party is written FOR — not whether a row
// changes. A worker acting under its parent's name that could post would be a
// colleague answering in somebody else's voice, with nothing in the room to
// say so; one that joins a room changes only where its parent listens.
func TestAWorkerIsDeniedEveryChatWriteItsParentHolds(t *testing.T) {
	t.Parallel()
	reg := chatRegistry(t, chatDeps(newFakeChat()))

	denied := append(slices.Clone(builtin.ChatWrites()), builtin.ReactToMessageTool)
	for _, name := range denied {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if !mcp.WritesToSharedSurface(entry.Annotations) {
			t.Errorf("%s is classified %+v, which a worker may call under its "+
				"parent's identity", name, entry.Annotations)
		}
	}
	for _, name := range []string{
		builtin.JoinChannelTool, builtin.LeaveChannelTool,
		builtin.ReadChannelTool, builtin.ListChannelsTool, builtin.SearchMessagesTool,
	} {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if mcp.WritesToSharedSurface(entry.Annotations) {
			t.Errorf("%s is classified as a write to a shared surface (%+v), so "+
				"a worker is refused a tool its parent granted it", name,
				entry.Annotations)
		}
	}
}

// EVERY CHAT TOOL IS CLASSIFIED BY NAME, never by the default arm.
//
// That arm is fail-closed — an unclassified builtin is treated as a public
// write — so a tool that fell through it would still be SAFE and would be
// wrong in two invisible ways: a read would be counted as a possible delivery
// by the gate, and a private-state write would be withheld from a worker its
// parent granted it. The table is the assertion; a dropped arm changes these
// values.
func TestEveryChatToolCarriesItsOwnAnnotations(t *testing.T) {
	t.Parallel()
	read := tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}
	shared := tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	own := tools.Annotations{
		ReadOnly: mcp.No, Destructive: mcp.No, Idempotent: mcp.Yes, OpenWorld: mcp.No,
	}
	want := map[string]tools.Annotations{
		builtin.ReadChannelTool:    read,
		builtin.ListChannelsTool:   read,
		builtin.SearchMessagesTool: read,
		builtin.PostMessageTool:    shared,
		builtin.ReplyInThreadTool:  shared,
		builtin.SendDMTool:         shared,
		builtin.ReactToMessageTool: tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		},
		builtin.JoinChannelTool:  own,
		builtin.LeaveChannelTool: own,
	}
	for _, name := range chat.Tools() {
		expected, named := want[name]
		if !named {
			t.Fatalf("%s is in the catalogue and this table does not say what "+
				"it is", name)
		}
		if got := builtin.AnnotationsFor(name); got != expected {
			t.Errorf("%s is annotated %+v, want %+v", name, got, expected)
		}
	}
}

// A NAME RESOLVES AGAINST THE ROOMS THIS SEAT IS IN, because a model types a
// room's name and never its uuid — and a tool that took only the id would
// refuse the first call of every turn that had not just listed the rooms.
func TestAChannelNameResolvesAgainstTheRoomsYouAreIn(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")

	for _, ref := range []string{chatRoomName, "#" + chatRoomName, "ENG"} {
		before := len(f.posts)
		if got := callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
			"channel": ref, "body": "hello",
		}); got.Failed {
			t.Fatalf("a post to %q was refused: %s", ref, got.Output)
		}
		if f.posts[before].channelID != chatRoomID {
			t.Errorf("%q resolved to %q, want %q", ref, f.posts[before].channelID, chatRoomID)
		}
	}

	// AND AN ID COSTS NO LOOKUP. The common case is a room id carried in
	// from the wake that woke this seat, and paying a listing for it would
	// put a read on the hot path of every answer.
	f.reads = 0
	callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": chatRoomID, "body": "hello",
	})
	if f.reads != 0 {
		t.Errorf("a post addressed by id read the rail %d times", f.reads)
	}

	// AND A NAME NOBODY HAS SAYS WHERE TO LOOK, rather than reporting that
	// the room does not exist — a public room this seat has not joined is
	// real and is addressed by its id.
	got := callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": "marketing", "body": "hello",
	})
	if !got.Failed {
		t.Fatal("a post to a room nobody is in was accepted")
	}
	if !strings.Contains(got.Output, builtin.ListChannelsTool) {
		t.Errorf("the refusal does not say how to find the room: %s", got.Output)
	}
}

// ADDRESSING THE WHOLE ROOM IS DERIVED FROM THE WORDS, and it has to be: the
// prompt a woken seat reads tells it that `@channel` is how a room is
// addressed, so that is what a model writes — and a message saying `@channel`
// that woke nobody reads to the room as though everybody had seen it.
func TestAtChannelInTheBodyAddressesTheWholeRoom(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")

	for _, c := range []struct {
		body string
		want bool
	}{
		{"@channel the deploy is blocked", true},
		{"heads up @channel", true},
		{"we never use @channels here", false},
		{"mail ops@channel-list.example.com", false},
		{"nothing to see", false},
	} {
		before := len(f.posts)
		callChat(t, reg, builtin.PostMessageTool, turn,
			map[string]any{"channel": chatRoomID, "body": c.body})
		if got := f.posts[before].in.Collective; got != c.want {
			t.Errorf("%q addressed the room = %v, want %v", c.body, got, c.want)
		}
	}
}

// AN UNREAD COUNT IS A PERSON'S, and a seat has none. Reported anyway it is
// the room's whole tail, and a model told it has seven unread messages in a
// room it was never reading goes off to catch up on somebody else's backlog.
func TestAListingCarriesNoUnreadCountWithoutReadState(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))

	got := callChat(t, reg, builtin.ListChannelsTool, turnFor(t, "agent-cto"), nil)
	if got.Failed {
		t.Fatalf("list_channels failed: %s", got.Output)
	}
	if strings.Contains(got.Output, "unread") {
		t.Errorf("a seat's listing carries an unread count: %s", got.Output)
	}

	deps := chatDeps(f)
	deps.ReadState = func(context.Context, string) map[string]int64 {
		return map[string]int64{chatRoomID: 4}
	}
	got = callChat(t, chatRegistry(t, deps), builtin.ListChannelsTool,
		turnFor(t, "agent-cto"), nil)
	if !strings.Contains(got.Output, `"unread": 7`) {
		t.Errorf("a caller with read state was not told what it has not read: %s",
			got.Output)
	}
}

// A TOOL WHOSE HALF IS ABSENT IS OMITTED, never registered and broken: a model
// shown a tool that always fails learns to distrust the whole catalogue.
func TestChatToolsAreOmittedWithoutTheHalfTheyNeed(t *testing.T) {
	t.Parallel()
	f := newFakeChat()

	reads := chatRegistry(t, builtin.ChatDeps{Reader: f})
	for _, name := range builtin.ChatWrites() {
		if _, ok := reads.Lookup(name); ok {
			t.Errorf("%s was registered against a company with no chat writer", name)
		}
	}
	if _, ok := reads.Lookup(builtin.SearchMessagesTool); ok {
		t.Error("search_messages was registered against a node with no chat index")
	}

	writes := chatRegistry(t, builtin.ChatDeps{Writer: f})
	for _, name := range []string{builtin.ReadChannelTool, builtin.ListChannelsTool} {
		if _, ok := writes.Lookup(name); ok {
			t.Errorf("%s was registered against a company with no chat reader", name)
		}
	}

	none := chatRegistry(t, builtin.ChatDeps{})
	for _, name := range chat.Tools() {
		if _, ok := none.Lookup(name); ok {
			t.Errorf("%s was registered for a company that runs no native chat", name)
		}
	}
}

// A CALLER THE ENGINE CANNOT RESOLVE TO A SEAT GETS NOTHING, READS INCLUDED —
// and the refusal carries the remedy the seam named.
//
// A private room's contents are decided by its membership, so a caller with no
// seat has no membership: serving it "everything public" would invent a viewer
// the company never declared. The refusal is passed through rather than
// flattened into this package's own sentence, because on the operator surface
// it names the config field that fixes it, and "only during a turn" would send
// a person looking for a turn they do not have.
func TestACallerWithNoSeatIsRefusedEveryChatToolWithTheRemedy(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	deps := chatDeps(f)
	deps.Actor = func(context.Context, *turnctx.Turn) (chat.Actor, error) {
		return chat.Actor{}, errors.New("the token \"ci\" is bound to no seat — " +
			"set `contact.crewlet_operator_id` on the `kind: human` seat")
	}
	reg := chatRegistry(t, deps)

	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{builtin.ListChannelsTool, nil},
		{builtin.ReadChannelTool, map[string]any{"channel": chatRoomID}},
		{builtin.SearchMessagesTool, map[string]any{"text": "anything"}},
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "body": "hi"}},
		{builtin.ReplyInThreadTool,
			map[string]any{"channel": chatRoomID, "reply_to": "m1", "body": "hi"}},
		{builtin.SendDMTool, map[string]any{"to": []any{"agent-ceo"}, "body": "hi"}},
		{builtin.ReactToMessageTool,
			map[string]any{"channel": chatRoomID, "message": "m1", "emoji": ":+1:"}},
		{builtin.JoinChannelTool, map[string]any{"channel": chatRoomID}},
		{builtin.LeaveChannelTool, map[string]any{"channel": chatRoomID}},
	} {
		got := callChat(t, reg, c.name, turnFor(t, "agent-cto"), c.args)
		if !got.Failed {
			t.Errorf("%s answered a caller with no seat: %s", c.name, got.Output)
			continue
		}
		if !strings.Contains(got.Output, "contact.crewlet_operator_id") {
			t.Errorf("%s refused without the remedy: %s", c.name, got.Output)
		}
	}
	if f.reads != 0 || len(f.posts) != 0 {
		t.Errorf("a caller with no seat reached the backend: %d reads, %d posts",
			f.reads, len(f.posts))
	}
}

// A READ THAT COULD NOT BE SERVED IS NEVER AN EMPTY ROOM. A node whose rows
// are behind must not be able to tell a seat that nobody answered, because a
// seat that believes nobody answered asks again.
func TestAFailedChatReadIsNotReportedAsSilence(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	f.readErr = errors.New("the store would not answer")
	reg := chatRegistry(t, chatDeps(f))

	got := callChat(t, reg, builtin.ReadChannelTool, turnFor(t, "agent-cto"),
		map[string]any{"channel": chatRoomID})
	if !got.Failed {
		t.Fatal("a read that failed was reported as an answer")
	}
	if !strings.Contains(got.Output, "NOT an empty room") {
		t.Errorf("the failure could be read as silence: %s", got.Output)
	}
}
