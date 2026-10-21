package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/knowledge"
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

	// chatRoomMessages is how many messages the fake's one room holds.
	chatRoomMessages = 7
)

// postedMessage is one message the fake took, with the actor whose key
// decides its id.
type postedMessage struct {
	channelID string
	replyTo   string
	actor     chatlet.Actor
	in        chatlet.NewMessage
}

type fakeChat struct {
	posts     []postedMessage
	reactions []string
	joined    []string
	left      []string

	// keys is the operation key every gesture other than a post arrived
	// with ([chatlet.Actor.OpKey]), in order — a post's is on its actor.
	keys []string

	// awaited is every position [builtin.ChatDeps.Await] was handed, in
	// order, so a write that waited for the wrong position — or a read
	// that waited at all — is visible rather than merely plausible.
	awaited []uint64

	// levels is every read level these tools asked for, so a tool that
	// stopped naming one is visible.
	levels []statelog.ReadLevel

	// reads counts every call that reached the read side, which is what
	// "no read awaits" and "an id costs no lookup" are asserted against.
	reads int

	writeErr error
	readErr  error

	// postErr, where set, refuses a post or a reply alone — the half of a
	// direct message that comes after its conversation opened.
	postErr error

	// opened, where set, is the outcome a direct conversation's open
	// answers — and an unvouched one answers no room at all, as the store's
	// does.
	opened *statelog.Result

	// outcome, where set, is the outcome every other write answers, with
	// the same rule for an unvouched one.
	outcome *statelog.Result

	searched []estate.ChatQuery
	viewers  []string
	building bool
	hits     []search.ChatHit

	// listed is every listing query, so a case can see which cursors the
	// unread counts were taken against.
	listed []chatlet.ChannelsQuery

	// room is the stored row [fakeChat.Channel] answers — who made it and
	// the unit that holds it, which a room's archive is decided on — and
	// author whose words the message a Thread read is asked of are.
	room   chatlet.Channel
	author string

	// threads is every thread read, so the read an edit or a delete makes
	// to learn whose words they are is visible.
	threads []chatlet.ThreadQuery

	// What a read answers beyond the bare room: the transcript a room read
	// is handed, the root's words and the replies a thread read is, and the
	// last thing said in the room a listing shows.
	transcript []chatlet.MessageView
	rootBody   string
	rootKind   chatlet.AuthorKind
	replies    []chatlet.MessageView
	last       *chatlet.MessagePreview

	// What the person's gestures handed the store, in order.
	created     []chatlet.NewChannel
	patches     []chatlet.ChannelPatch
	authorities []chatlet.ChannelAuthority
	memberSets  [][]chatlet.Member
	edits       []chatlet.EditMessage
	deleted     []string
	moderations []chatlet.MessageAuthority
	actors      []chatlet.Actor
}

func newFakeChat() *fakeChat {
	return &fakeChat{
		room: chatlet.Channel{ID: chatRoomID, Name: chatRoomName, Kind: chatlet.KindPublic,
			CreatedBy: "jane"},
		author: "jane",
		hits:   []search.ChatHit{{MessageID: "m1", ChannelID: chatRoomID, Snippet: "the backoff"}},
	}
}

func (f *fakeChat) read(viewer string, fresh statelog.Freshness) error {
	f.levels = append(f.levels, fresh.Level)
	f.viewers = append(f.viewers, viewer)
	f.reads++
	return f.readErr
}

func (f *fakeChat) Channels(_ context.Context, viewer string, q chatlet.ChannelsQuery,
	fresh statelog.Freshness) (chatlet.ChannelListing, error) {

	f.listed = append(f.listed, q)
	if err := f.read(viewer, fresh); err != nil {
		return chatlet.ChannelListing{}, err
	}
	// COUNTED ABOVE THE CURSOR IT WAS HANDED, as the store counts: the room
	// holds seven messages at positions one to seven, so a reader who has
	// read through four has three new and one who has opened nothing has
	// seven. A count that ignored the cursor would hold no case here to it.
	return chatlet.ChannelListing{
		Served: chatlet.Served{Complete: true},
		Channels: []chatlet.ChannelSummary{{
			Channel: f.room,
			Unread:  int(max(chatRoomMessages-q.Cursors[chatRoomID], 0)),
			Last:    f.last,
		}},
	}, nil
}

func (f *fakeChat) Channel(_ context.Context, viewer, _ string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {

	if err := f.read(viewer, fresh); err != nil {
		return chatlet.ChannelDetail{}, err
	}
	return chatlet.ChannelDetail{Channel: f.room}, nil
}

// ChannelNamed resolves the one room this fake holds by its name, for any
// viewer, as the store resolves a public room's address.
func (f *fakeChat) ChannelNamed(_ context.Context, viewer, name string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {

	if err := f.read(viewer, fresh); err != nil {
		return chatlet.ChannelDetail{}, err
	}
	if chatlet.NormalizeName(name) != chatRoomName {
		return chatlet.ChannelDetail{}, fmt.Errorf("%w: no room is called %q",
			chatlet.ErrNotFound, name)
	}
	return chatlet.ChannelDetail{Channel: f.room}, nil
}

func (f *fakeChat) Messages(_ context.Context, viewer string, q chatlet.TranscriptQuery,
	fresh statelog.Freshness) (chatlet.Transcript, error) {

	if err := f.read(viewer, fresh); err != nil {
		return chatlet.Transcript{}, err
	}
	return chatlet.Transcript{ChannelID: q.ChannelID, Messages: f.transcript}, nil
}

// Thread answers any message as the root of its own thread, as the store
// does, written by [fakeChat.author].
func (f *fakeChat) Thread(_ context.Context, viewer string, q chatlet.ThreadQuery,
	fresh statelog.Freshness) (chatlet.Thread, error) {

	f.threads = append(f.threads, q)
	if err := f.read(viewer, fresh); err != nil {
		return chatlet.Thread{}, err
	}
	return chatlet.Thread{ChannelID: q.ChannelID, Root: chatlet.MessageView{
		Message: chatlet.Message{ID: q.RootID, ChannelID: q.ChannelID, Author: f.author,
			AuthorKind: f.rootKind, Body: f.rootBody},
	}, Replies: f.replies}, nil
}

func (f *fakeChat) Post(_ context.Context, actor chatlet.Actor, channelID string,
	in chatlet.NewMessage) (chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	if f.postErr != nil {
		return chatlet.Written{}, f.postErr
	}
	f.posts = append(f.posts, postedMessage{channelID: channelID, actor: actor, in: in})
	return f.wrote(20 + uint64(len(f.posts))), nil
}

func (f *fakeChat) Reply(_ context.Context, actor chatlet.Actor, channelID, replyTo string,
	in chatlet.NewMessage) (chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	if f.postErr != nil {
		return chatlet.Written{}, f.postErr
	}
	f.posts = append(f.posts, postedMessage{
		channelID: channelID, replyTo: replyTo, actor: actor, in: in,
	})
	return f.wrote(20 + uint64(len(f.posts))), nil
}

func (f *fakeChat) OpenDirect(_ context.Context, actor chatlet.Actor, participants []string) (
	chatlet.Written, bool, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, false, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.actors = append(f.actors, actor)
	room := chatlet.Written{
		Channel: chatlet.Channel{
			ID: "dm-" + strings.Join(participants, "-"), Kind: chatlet.KindDM,
		},
		Revision: 3,
		Outcome: statelog.Result{
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "CREWLET_CHAT_LOG", Seq: 11},
		},
	}
	if f.opened != nil {
		room.Outcome = *f.opened
		if f.opened.Unvouched {
			return chatlet.Written{ChangeID: f.opened.OpID, Outcome: *f.opened}, false, nil
		}
	}
	return room, true, nil
}

func (f *fakeChat) React(_ context.Context, actor chatlet.Actor, _, messageID,
	emoji string) (chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.reactions = append(f.reactions, messageID+emoji)
	return f.wrote(31), nil
}

func (f *fakeChat) Unreact(_ context.Context, actor chatlet.Actor, _, messageID,
	emoji string) (chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.reactions = append(f.reactions, "-"+messageID+emoji)
	return f.wrote(32), nil
}

func (f *fakeChat) Join(_ context.Context, actor chatlet.Actor, channelID string) (
	chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.joined = append(f.joined, channelID)
	return f.wrote(33), nil
}

func (f *fakeChat) Leave(_ context.Context, actor chatlet.Actor, channelID string) (
	chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.left = append(f.left, channelID)
	return f.wrote(34), nil
}

func (f *fakeChat) CreateChannel(_ context.Context, actor chatlet.Actor,
	in chatlet.NewChannel) (chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.created = append(f.created, in)
	f.actors = append(f.actors, actor)
	out := f.wrote(41)
	out.Channel = chatlet.Channel{ID: chatRoomID, Name: in.Name, Kind: chatlet.KindPublic}
	if f.outcome != nil && f.outcome.Unvouched {
		out.Channel = chatlet.Channel{}
	}
	return out, nil
}

func (f *fakeChat) PatchChannel(_ context.Context, actor chatlet.Actor, _ string,
	patch chatlet.ChannelPatch, authority chatlet.ChannelAuthority) (chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.patches = append(f.patches, patch)
	f.authorities = append(f.authorities, authority)
	f.actors = append(f.actors, actor)
	return f.wrote(42), nil
}

func (f *fakeChat) SetMembers(_ context.Context, actor chatlet.Actor, _ string,
	members []chatlet.Member) (chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.memberSets = append(f.memberSets, members)
	f.actors = append(f.actors, actor)
	return f.wrote(43), nil
}

func (f *fakeChat) Edit(_ context.Context, actor chatlet.Actor, _, _ string,
	in chatlet.EditMessage) (chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.edits = append(f.edits, in)
	f.actors = append(f.actors, actor)
	return f.wrote(44), nil
}

func (f *fakeChat) Delete(_ context.Context, actor chatlet.Actor, _, messageID string,
	authority chatlet.MessageAuthority) (chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.deleted = append(f.deleted, messageID)
	f.moderations = append(f.moderations, authority)
	f.actors = append(f.actors, actor)
	return f.wrote(45), nil
}

func (f *fakeChat) Search(_ context.Context, q estate.ChatQuery) (estate.ChatSearchAnswer, error) {
	f.viewers = append(f.viewers, q.Viewer)
	f.levels = append(f.levels, q.Fresh.Level)
	f.reads++
	f.searched = append(f.searched, q)
	if f.readErr != nil {
		return estate.ChatSearchAnswer{}, f.readErr
	}
	return estate.ChatSearchAnswer{Hits: f.hits, Searched: 1, Building: f.building}, nil
}

func (f *fakeChat) wrote(seq uint64) chatlet.Written {
	if f.outcome != nil {
		if f.outcome.Unvouched {
			return chatlet.Written{ChangeID: f.outcome.OpID, Outcome: *f.outcome}
		}
		return chatlet.Written{
			Channel:  chatlet.Channel{ID: chatRoomID, Name: chatRoomName},
			ChangeID: f.outcome.OpID, Outcome: *f.outcome,
		}
	}
	return chatlet.Written{
		Channel:  chatlet.Channel{ID: chatRoomID, Name: chatRoomName},
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
	if _, err := builtin.Register(reg, gated(builtin.Deps{Chat: deps})); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// callChat runs one chat tool for a turn the caller pins, because every
// interesting case here is about WHICH turn made the call — as a seat holding
// every app, which is what the turn's own principal is.
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
	got, err := seated.CallForTurn(turnctx.WithPrincipal(t.Context(), turn), turn, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

// chatSurface is a tool surface over the chat tools, bound to a turn with a
// call log of its own — the frame a real turn's calls go through.
func chatSurface(t *testing.T, f *fakeChat, runID string) *tools.Surface {
	t.Helper()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")
	turn.RunID = runID
	turn.Calls = turnctx.NewCallLog()
	return tools.NewSurface("execute", reg.Snapshot(), chatlet.Tools()).ForTurn(turn)
}

// ---- the cases -------------------------------------------------------- //

// SAYING SOMETHING IS THE ANSWER, AND ONLY SAYING SOMETHING IS. A turn woken
// because somebody spoke to it answers in the thread, in the room or in a DM —
// and the gate has to know that, or the turn is corrected for having done what
// it was woken to do. The other half is what this protects: if a REACTION
// counted, every addressed turn could discharge its obligation with a thumb,
// and joining a room would close out a question nobody had answered.
//
// Mutation: drop the chat arm of Register's delivery switch and the three go
// red; put the reaction in it and the reaction does.
func TestOnlyTheThreePostingToolsCountAsChatDeliveries(t *testing.T) {
	t.Parallel()
	reg := chatRegistry(t, chatDeps(newFakeChat()))
	deliveries := reg.Deliveries()

	for _, name := range builtin.ChatWrites() {
		// AND ON CHAT'S OWN SOURCE, not just "somewhere". The gate
		// compares a delivery's surface against the source the inbound
		// wake carried, so a second spelling here makes every Chatlet
		// obligation unreachable and the check silently falls back
		// to "any delivery counts".
		if deliveries[name] != chatlet.Source {
			t.Errorf("%s delivers to %q, want %q", name, deliveries[name], chatlet.Source)
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
// The whole of Chatlet's idempotency rests on the key every post hands
// the store: it is derived from the turn's unit of work rather than its run,
// so a redelivered trigger — which is ordinary — derives the same messages
// instead of a second copy of the conversation; and from the run's own call
// log's repeat count, kept by the surface every call goes through, so a turn's
// second remark is never its first one's retry. These calls go through a real
// surface for that reason.
//
// Mutation: drop the repeat count from callOpKey's identity and the third
// post's key equals the first's.
func TestATurnThatPostsTwiceWritesTwoMessagesAndARerunWritesOne(t *testing.T) {
	t.Parallel()
	calls := []struct {
		tool string
		args map[string]any
	}{
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "body": "on it"}},
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "body": "opened ENG-14"}},
		{builtin.ReplyInThreadTool, map[string]any{
			"channel": chatRoomID, "reply_to": "m1", "body": "and here is why",
		}},
		// THE SAME WORDS AGAIN, AFTER A DIFFERENT POST: a third message.
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "body": "on it"}},
		// AND AGAIN WITH NOTHING DIFFERENT BETWEEN: the same call, asked
		// twice — one message.
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "body": "on it"}},
	}
	run := func(runID string) []string {
		f := newFakeChat()
		surface := chatSurface(t, f, runID)
		for _, c := range calls {
			executeWork(t, surface, c.tool, c.args)
		}
		keys := make([]string, 0, len(f.posts))
		for _, p := range f.posts {
			keys = append(keys, p.actor.OpKey)
		}
		return keys
	}
	first := run("run-1")
	if len(first) != len(calls) {
		t.Fatalf("%d calls reached the store with %d keys", len(calls), len(first))
	}
	for i, key := range first {
		if statelog.CheckCallerOpID(key) != nil {
			t.Fatalf("call %d carried %q, which is no operation key the store "+
				"takes — every key was %q", i, key, first)
		}
	}
	if first[0] == first[1] || first[0] == first[2] {
		t.Errorf("two different remarks carried one key: %q", first)
	}
	if first[0] == first[3] {
		t.Errorf("\"on it\" said again after a different post carried the first "+
			"one's key %q — it would be answered as that post's retry and never "+
			"said", first[0])
	}
	if first[3] != first[4] {
		t.Errorf("the same post asked twice in a row carried %q and %q — an "+
			"executor that asks twice would say it twice", first[3], first[4])
	}

	// THE RE-RUN: the same unit of work, a NEW run id, a fresh call log.
	if rerun := run("run-2"); !slices.Equal(rerun, first) {
		t.Errorf("the re-run carried %q, the first run %q — a redelivered "+
			"trigger would post the whole conversation again", rerun, first)
	}
}

// JOINING AGAIN AFTER LEAVING IS A NEW OPERATION, not the first join's retry.
//
// join_channel and leave_channel write one membership, so a turn counts them
// as one family (chatlet.CallFamily): the leave between two joins raises the
// second one's count, where a count kept per tool saw nothing different and
// handed the second join the first one's key — and with it the first one's
// success, while the seat stayed outside the room. A call outside the family
// counts for nothing, and the same join asked again with nothing between is
// the same operation.
//
// Mutation: pass a nil family from ChatDeps.actor and the join after the leave
// carries the first join's key.
func TestAJoinAfterALeaveIsANewOperation(t *testing.T) {
	t.Parallel()
	room := map[string]any{"channel": chatRoomID}
	calls := []struct {
		tool string
		args map[string]any
	}{
		{builtin.JoinChannelTool, room},
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "body": "hi"}},
		{builtin.JoinChannelTool, room},
		{builtin.LeaveChannelTool, room},
		{builtin.JoinChannelTool, room},
		{builtin.LeaveChannelTool, room},
	}
	f := newFakeChat()
	surface := chatSurface(t, f, "run-1")
	for _, c := range calls {
		executeWork(t, surface, c.tool, c.args)
	}
	// The membership writes, in order: join, the same join again, leave,
	// join, leave.
	if len(f.keys) != 5 {
		t.Fatalf("%d membership writes reached the store, want 5", len(f.keys))
	}
	if f.keys[0] != f.keys[1] {
		t.Errorf("the same join asked twice with only a post between carried %q "+
			"and %q — a call outside the family raised the count", f.keys[0], f.keys[1])
	}
	if f.keys[0] == f.keys[3] {
		t.Errorf("the join after a leave carried the first join's key %q — it "+
			"would be answered with that join's success and change nothing", f.keys[0])
	}
	if f.keys[2] == f.keys[4] {
		t.Errorf("the second leave carried the first one's key %q", f.keys[2])
	}
}

// A CHAT WRITE CARRIES THE INSTANT ITS TURN'S WORK BEGAN, and a rebased turn's
// instead — never the call's.
//
// The key's instant is what every id the store derives from it carries, and
// the state log reads it to decide whether this node's ledger can vouch for a
// retry: a re-run comes after any loss it is judged against, so a key stamped
// with the call's clock is one a re-run after the ledger's sweep publishes a
// second time. And a turn whose work began longer ago than the ledger
// remembers is rebased (turnctx.Turn.RebasedTo), which has to reach these ids
// too, or every message such a turn says is answered `unknown` and lost.
func TestAChatWriteCarriesTheInstantItsWorkBegan(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	rebased := began.Add(40 * 24 * time.Hour)
	for name, want := range map[string]time.Time{"began": began, "rebased": rebased} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeChat()
			reg := chatRegistry(t, chatDeps(f))
			turn := turnFor(t, "agent-cto")
			turn.WorkSince = began
			if name == "rebased" {
				turn.RebasedTo = rebased
			}
			for _, c := range seatChatWrites() {
				if got := callChat(t, reg, c.tool, turn, c.args); got.Failed {
					t.Fatalf("%s failed: %s", c.tool, got.Output)
				}
			}
			keys := slices.Clone(f.keys)
			for _, p := range f.posts {
				keys = append(keys, p.actor.OpKey)
			}
			// A post, a reply, a direct message's open and its post, a
			// reaction, a join and a leave.
			if len(keys) != 7 {
				t.Fatalf("the writes reached the store with %d keys, want 7", len(keys))
			}
			for i, key := range keys {
				if at, ok := statelog.OpMintedAt(key); !ok || !at.Equal(want) {
					t.Errorf("chat write %d carries key %q minted at %s, want %s",
						i, key, at, want)
				}
			}
		})
	}
}

// A PERSON'S RETRIED REQUEST IS ONE MESSAGE: outside a turn the key is the one
// the caller's transport carried on the actor, as it was sent — the act
// transport's request key — so the retry is the message the first attempt
// posted. And a caller that names none — an operator's assistant over MCP —
// writes under a fresh operation every time, because nothing redelivers that
// call and a stable key would collapse every later post into the first.
//
// Mutation: have callOpKey answer "" outside a turn and the retry is a second
// message.
func TestAPersonsChatRequestIsKeyedOnTheKeyItCarried(t *testing.T) {
	t.Parallel()
	key := statelog.NewOpID(time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC), "")
	args := map[string]any{"channel": chatRoomID, "body": "standup moved to ten"}
	for _, carried := range []string{key, ""} {
		f := newFakeChat()
		person := operatorChat(t, f, nil, func(context.Context, *turnctx.Turn) (chatlet.Actor, error) {
			return chatlet.Actor{Handle: "jane", Kind: chatlet.AuthorHuman, OperatorID: "pat:1",
				OpKey: carried}, nil
		})
		for range 2 {
			if got := callOperator(t, person[builtin.PostMessageTool], args); got.Failed {
				t.Fatalf("post: %s", got.Output)
			}
		}
		if len(f.posts) != 2 {
			t.Fatalf("%d posts reached the store", len(f.posts))
		}
		for _, p := range f.posts {
			if p.actor.OpKey != carried {
				t.Errorf("a request carrying key %q posted under %q", carried, p.actor.OpKey)
			}
		}
	}
}

// A DIRECT MESSAGE WHOSE CONVERSATION'S OPEN CAME BACK UNKNOWN SAYS NOTHING,
// and says so.
//
// An open can answer `unknown` — a lost acknowledgement, or a node whose
// ledger cannot vouch for an operation, whose answer names no room at all.
// Posting on regardless sent the message into the room id "" — refused as
// "not found" for a conversation that may well exist — or into a room this
// node has not applied, refused the same way. Either is a different and
// misleading answer, so the tool stops: nothing reaches the writer's post, the
// answer is the unknown one, says the message was not sent, and tells a seat
// to make the same call again.
func TestADirectMessageWhoseOpenIsUnknownSaysNothing(t *testing.T) {
	t.Parallel()
	const op = "01a11a61-ff0c-77c1-8cb1-7b9e5497550e"
	for name, outcome := range map[string]statelog.Result{
		"unvouched":            {Outcome: statelog.OutcomeUnknown, OpID: op, Unvouched: true},
		"lost acknowledgement": {Outcome: statelog.OutcomeUnknown, OpID: op},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeChat()
			f.opened = &outcome
			reg := chatRegistry(t, chatDeps(f))
			got := callChat(t, reg, builtin.SendDMTool, turnFor(t, "agent-cto"),
				map[string]any{"to": []any{"agent-ceo"}, "body": "a word"})
			if len(f.posts) != 0 {
				t.Fatalf("an unknown open was followed by a post into %q — it is "+
					"refused as not found, or lands in a room nobody knows opened",
					f.posts[0].channelID)
			}
			if !tools.UnknownOf(got) || tools.RefusalOf(got) != tools.RefusalUnavailable {
				t.Fatalf("an unknown open answered %+v, want the unknown answer", got)
			}
			for _, want := range []string{"was NOT sent", op,
				"send_dm again with exactly the same arguments"} {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the answer does not say %q: %s", want, got.Output)
				}
			}
			if outcome.Unvouched && !strings.Contains(got.Output, "ledger") {
				t.Errorf("an unvouched open does not say this node cannot "+
					"tell: %s", got.Output)
			}
		})
	}
}

// A CHAT WRITE WHOSE OUTCOME IS UNKNOWN IS NEITHER DONE NOR FAILED, and says
// how to finish it.
//
// A receipt built whatever the write answered would come back with a
// `message_id` for an `unknown` post, and a seat would report a message it may
// never have said — while an operation this node's ledger cannot vouch for is
// answered `unknown` WITHOUT being published, and its answer names no room, so
// a join would report `joined` into a room called nothing. Each write answers
// the unknown result: a post with how to repeat it as the same message and
// what a reworded one costs, a statement — a reaction, a join, a leave — with
// its repeat offered, because stating the same thing again changes nothing.
func TestAChatWriteWhoseOutcomeIsUnknownIsNeitherDoneNorFailed(t *testing.T) {
	t.Parallel()
	const op = "01a11a61-ff0c-77c1-8cb1-7b9e5497550e"
	for name, outcome := range map[string]statelog.Result{
		"unvouched":            {Outcome: statelog.OutcomeUnknown, OpID: op, Unvouched: true},
		"lost acknowledgement": {Outcome: statelog.OutcomeUnknown, OpID: op},
	} {
		for _, w := range seatChatWrites() {
			t.Run(name+"/"+w.tool, func(t *testing.T) {
				t.Parallel()
				f := newFakeChat()
				f.outcome = &outcome
				reg := chatRegistry(t, chatDeps(f))
				got := callChat(t, reg, w.tool, turnFor(t, "agent-cto"), w.args)
				if !tools.UnknownOf(got) || tools.RefusalOf(got) != tools.RefusalUnavailable {
					t.Fatalf("an unknown %s answered %+v, want the unknown answer",
						w.tool, got)
				}
				for _, want := range []string{"is unknown", op, "do not report it as done",
					w.next} {
					if !strings.Contains(got.Output, want) {
						t.Errorf("the answer does not say %q: %s", want, got.Output)
					}
				}
				if outcome.Unvouched && !strings.Contains(got.Output, "ledger") {
					t.Errorf("an unvouched %s does not say this node cannot tell: %s",
						w.tool, got.Output)
				}
			})
		}
	}

	// AND A WRITE THAT LANDED SAYS SO, with the outcome and the position a
	// caller waits for.
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	got := callChat(t, reg, builtin.PostMessageTool, turnFor(t, "agent-cto"),
		map[string]any{"channel": chatRoomID, "body": "on it"})
	if got.Failed || !strings.Contains(got.Output, `"outcome": "applied"`) ||
		!strings.Contains(got.Output, `"position": "CREWLET_CHAT_LOG@`) {
		t.Errorf("a post that landed answered %+v, want its outcome and position", got)
	}
}

// seatChatWrites are a seat's chat writes, each with the sentence its unknown
// answer offers a turn as the repeat.
//
// A POST'S REPEAT is the same call, made before its tool is called with
// anything else — and names no other tool, because no posting tool is counted
// among a family (TestNoPostingToolIsCountedAmongAFamily). A STATEMENT'S is
// offered as harmless, because stating the same thing again changes nothing.
func seatChatWrites() []struct {
	tool string
	args map[string]any
	next string
} {
	sameCall := func(tool string) string {
		return tool + " again with exactly the same arguments, " +
			"before calling it with any others"
	}
	return []struct {
		tool string
		args map[string]any
		next string
	}{
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "body": "on it"},
			sameCall(builtin.PostMessageTool)},
		{builtin.ReplyInThreadTool,
			map[string]any{"channel": chatRoomID, "reply_to": "m1", "body": "ok"},
			sameCall(builtin.ReplyInThreadTool)},
		{builtin.SendDMTool, map[string]any{"to": []any{"agent-ceo"}, "body": "a word"},
			sameCall(builtin.SendDMTool)},
		{builtin.ReactToMessageTool,
			map[string]any{"channel": chatRoomID, "message_id": "m1", "emoji": ":+1:"},
			"harmless"},
		{builtin.JoinChannelTool, map[string]any{"channel": chatRoomID}, "harmless"},
		{builtin.LeaveChannelTool, map[string]any{"channel": chatRoomID}, "harmless"},
	}
}

// A WRITE NOBODY ANSWERED SAYS HOW THIS CALLER REPEATS IT.
//
// The router answers [estate.ErrOutcomeUnknown] for a write some data node
// never answered. A KEYED one — a turn's call, a person's request — is asked
// of every other data node first and reported only when none of them answered,
// and the same call IS its safe retry; one carrying no key of its caller's —
// an operator's assistant over MCP — is reported at the first data node that
// stayed silent, and its repeat is a second write. On a node without data every
// chat write is remote, so this is the common path, not an edge.
func TestAWriteNobodyAnsweredSaysHowThisCallerRepeatsIt(t *testing.T) {
	t.Parallel()
	silent := fmt.Errorf("%w (chat.post, asked of data-1, data-2)", estate.ErrOutcomeUnknown)
	for _, w := range seatChatWrites() {
		t.Run(w.tool, func(t *testing.T) {
			t.Parallel()
			f := newFakeChat()
			f.writeErr = silent
			got := callChat(t, chatRegistry(t, chatDeps(f)), w.tool,
				turnFor(t, "agent-cto"), w.args)
			if !tools.UnknownOf(got) {
				t.Fatalf("a %s nobody answered answered %+v, want unknown", w.tool, got)
			}
			if !strings.Contains(got.Output, w.next) {
				t.Errorf("a turn's %s nobody answered does not offer %q: %s",
					w.tool, w.next, got.Output)
			}
			if strings.Contains(got.Output, "a second call is a new operation") {
				t.Errorf("a turn's %s nobody answered warns that its repeat "+
					"duplicates, which for a turn it does not: %s", w.tool, got.Output)
			}
		})
	}

	// AN UNKEYED CALLER IS WARNED: the operator's assistant names no
	// operation for these tools, so its repeat is a second message.
	f := newFakeChat()
	f.writeErr = silent
	person := operatorChat(t, f, nil, nil)
	got := callOperator(t, person[builtin.PostMessageTool],
		map[string]any{"channel": chatRoomID, "body": "on it"})
	if !tools.UnknownOf(got) {
		t.Fatalf("an operator's post nobody answered answered %+v, want unknown", got)
	}
	if !strings.Contains(got.Output, "a second call is a new operation") ||
		strings.Contains(got.Output, "exactly the same arguments") {
		t.Errorf("an operator's post nobody answered does not warn that its "+
			"repeat is a second message: %s", got.Output)
	}
	// AND A STATEMENT IS STILL OFFERED AS HARMLESS, whoever made it.
	got = callOperator(t, person[builtin.ReactToMessageTool],
		map[string]any{"channel": chatRoomID, "message_id": "m1", "emoji": ":+1:"})
	if !tools.UnknownOf(got) || !strings.Contains(got.Output, "harmless") {
		t.Errorf("an operator's reaction nobody answered is not offered as "+
			"harmless: %s", got.Output)
	}
}

// NO POSTING TOOL IS COUNTED AMONG A FAMILY, which is what lets a post's
// "make the same call again" name its own tool and no other.
//
// A turn's repeat count for a tool in a family (chatlet.CallFamily) is raised by
// EVERY call to another tool of that family, whatever its arguments, so the
// sentence a post's unknown answer gives — "before calling it with any
// others" — would be wrong for one: it would let the other tool be called
// between, and that call makes the repeat a new operation.
func TestNoPostingToolIsCountedAmongAFamily(t *testing.T) {
	t.Parallel()
	// AND A ROOM'S CREATE, which answers an unknown with the same sentence.
	for _, tool := range append(chatlet.WriteTools(), chatlet.CreateChannelTool) {
		if family := chatlet.CallFamily(tool); len(family) != 0 {
			t.Errorf("%s is counted among %v, and its unknown answer tells a "+
				"seat to repeat it before calling it with other arguments — a "+
				"call to %v between would make that repeat a new message",
				tool, family, family)
		}
	}
}

// A VALUE REFUSAL NAMES THE FIELD AND DOES NOT READ AS A TRANSIENT FAILURE.
//
// The two arms say opposite things to a model: "refused that" is something it
// can fix and call again, while the fall-through says the write may or may not
// have landed and must not be reported as done. The refusal is the store's
// own, so its sentence is the store's without the Go package's prefix.
func TestABodyOverTheCapIsRefusedNamingTheField(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	f.writeErr = chatlet.NewMessage{Body: strings.Repeat("x", chatlet.MaxBody+1)}.Validate()
	if f.writeErr == nil {
		t.Fatal("a body over the cap validates, so this case holds nothing")
	}
	got := callChat(t, chatRegistry(t, chatDeps(f)), builtin.PostMessageTool,
		turnFor(t, "agent-cto"), map[string]any{"channel": chatRoomID, "body": "too long"})
	if tools.RefusalOf(got) != tools.RefusalInvalid {
		t.Fatalf("an oversized body answered %+v, want the argument's class", got)
	}
	if !strings.Contains(got.Output, "refused that") || !strings.Contains(got.Output, "`body`") {
		t.Errorf("the refusal does not say which field to change: %s", got.Output)
	}
	if strings.Contains(got.Output, "chatlet: invalid") {
		t.Errorf("the refusal carries the Go package's prefix: %s", got.Output)
	}
}

// EVERY POSTING TOOL TELLS A MODEL HOW MANY LINKS A MESSAGE TAKES. The store
// refuses a message carrying more than [chatlet.MaxLinks], and the parameter's
// description is the one place a model learns that before it writes: a posting
// tool whose `links` said nothing left a seat to meet the cap as a refusal.
//
// Mutation: drop send_dm's `links` description, and its row goes red.
func TestEveryPostingToolTellsAModelItsLinkCap(t *testing.T) {
	t.Parallel()
	reg := chatRegistry(t, chatDeps(newFakeChat()))
	limit := fmt.Sprintf("Up to %d URLs", chatlet.MaxLinks)
	for _, name := range builtin.ChatWrites() {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		properties, _ := entry.Tool.Parameters()["properties"].(map[string]any)
		links, _ := properties["links"].(map[string]any)
		if said, _ := links["description"].(string); !strings.Contains(said, limit) {
			t.Errorf("%s's `links` is described %q, which does not tell a model it "+
				"takes at most %d", name, said, chatlet.MaxLinks)
		}
	}
}

// EVERY CHAT TOOL THAT TAKES LINKS TELLS A MODEL WHAT A LINK IS. The store
// refuses a link that is not an absolute address in one of
// [chatlet.LinkSchemes], and the parameter's description is the one place a
// model learns that before it writes. Every chat tool a person's surface
// serves is walked, the seat's three posting tools and edit_message among
// them, so a tool that takes links later is held to it as well.
//
// Mutation: drop the rule from edit_message's `links`, or mailto from
// linkShape, and the tools it is missing from go red.
func TestEveryChatToolTellsAModelWhatALinkIs(t *testing.T) {
	t.Parallel()
	described := map[string]bool{}
	for name, tool := range operatorChat(t, newFakeChat(), nil, nil) {
		properties, _ := tool.Parameters()["properties"].(map[string]any)
		links, takes := properties["links"].(map[string]any)
		if !takes {
			continue
		}
		described[name] = true
		said, _ := links["description"].(string)
		words := strings.FieldsFunc(said, func(r rune) bool { return r < 'a' || r > 'z' })
		if !slices.Contains(words, "absolute") {
			t.Errorf("%s's `links` is described %q, which does not say a link is "+
				"an absolute address", name, said)
		}
		for _, scheme := range chatlet.LinkSchemes() {
			if !slices.Contains(words, scheme) {
				t.Errorf("%s's `links` is described %q, which does not say a link "+
					"may be %s", name, said, scheme)
			}
		}
	}
	for _, name := range []string{chatlet.PostMessageTool, chatlet.ReplyInThreadTool,
		chatlet.SendDMTool, chatlet.EditMessageTool} {
		if !described[name] {
			t.Errorf("%s takes no `links` on the person's surface, so this walk "+
				"held nothing to the rule", name)
		}
	}
}

// THE AUTHOR IS THE TURN'S SEAT AND NEVER AN ARGUMENT. A model that could name
// its own author could speak as anybody, and a room where that is possible is
// not a conversation — the name beside the words is the whole of what a reader
// has. An argument that tries is refused before anything is said, because no
// chat tool declares one.
func TestAPostIsAttributedToTheTurnsSeatAndNotToAnArgument(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")
	for _, arg := range []string{"author", "handle", "actor", "author_kind"} {
		got := callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
			"channel": chatRoomID, "body": "shipping tomorrow", arg: "agent-ceo",
		})
		if !got.Failed || len(f.posts) != 0 {
			t.Fatalf("a post naming its own %s answered %+v and reached the "+
				"store %d times", arg, got, len(f.posts))
		}
	}
	callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": chatRoomID, "body": "shipping tomorrow",
	})
	if len(f.posts) != 1 {
		t.Fatalf("%d messages reached the store", len(f.posts))
	}
	actor := f.posts[0].actor
	if actor.Handle != "agent-cto" || actor.Kind != chatlet.AuthorAgent {
		t.Errorf("the message was written as %q (%s): an argument moved the author",
			actor.Handle, actor.Kind)
	}
	// AND THE TURN ID IS THE RUN, which is provenance — while the KEY is
	// the work's: the run id is minted fresh on every attempt, so an id
	// derived from it makes every redelivery a second copy.
	if actor.TurnID != "run-1" {
		t.Errorf("the message names run %q, want the run that said it", actor.TurnID)
	}
	if actor.OpKey == "" {
		t.Error("a seat's message carries no key, so a re-run would post it twice")
	}
}

// EVERY WRITE WAITS FOR ITS OWN POSITION, AND NO READ WAITS AT ALL.
//
// A write lands on the fleet's log and a read is answered from whichever
// copy's rows serve it, so a seat that posts and then reads the room in the
// same turn must be made to see its own message — a model that cannot find
// what it just said says it again. The position is handed to the seam
// ([builtin.ChatDeps.Await]) and it must be THIS write's, or the next read can
// miss it. Waiting before a READ would buy nothing: there is no position to
// wait for that the caller did not already produce — and that holds for the
// reads a gesture makes to decide itself, too.
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
			map[string]any{"channel": chatRoomID, "message_id": "m1", "emoji": ":eyes:"}, 31},
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
	// the post decides from the rows of whichever copy takes it, so one
	// issued before the conversation's own create has been applied there
	// finds no room at all.
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

	// AND A PERSON'S GESTURES, through the catalogue that serves them —
	// the archive and the remark decided after a read included.
	person := operatorChat(t, f, nil, nil)
	for name, want := range map[string]uint64{
		chatlet.CreateChannelTool: 41, chatlet.OpenDMTool: 11, chatlet.UpdateChannelTool: 42,
		chatlet.SetChannelMembersTool: 43, chatlet.EditMessageTool: 44,
		chatlet.DeleteMessageTool: 45,
	} {
		args := operatorChatWrites[name]
		if name == chatlet.UpdateChannelTool {
			args = map[string]any{"channel": chatRoomID, "archived": true}
		}
		before := len(f.awaited)
		if got := callOperator(t, person[name], args); got.Failed {
			t.Fatalf("%s failed: %s", name, got.Output)
		}
		if waits := f.awaited[before:]; len(waits) != 1 || waits[0] != want {
			t.Errorf("%s waited %v, want exactly its own write at %d", name, waits, want)
		}
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

	callChat(t, reg, builtin.ReadChannelTool, turn, map[string]any{"channel": chatRoomName})
	callChat(t, reg, builtin.ReadChannelTool, turn,
		map[string]any{"channel": chatRoomID, "thread": "m1"})
	callChat(t, reg, builtin.ListChannelsTool, turn, nil)
	callChat(t, reg, builtin.SearchMessagesTool, turn, map[string]any{"text": "backoff"})

	want := statelog.DefaultReadLevel(statelog.SurfaceSeat)
	if len(f.levels) < 5 {
		t.Fatalf("%d reads named a level, want every one of the five", len(f.levels))
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
	stated := tools.Annotations{
		ReadOnly: mcp.No, Destructive: mcp.No, Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
	}
	own := tools.Annotations{
		ReadOnly: mcp.No, Destructive: mcp.No, Idempotent: mcp.Yes, OpenWorld: mcp.No,
	}
	restated := tools.Annotations{
		ReadOnly: mcp.No, Destructive: mcp.Yes, Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
	}
	want := map[string]tools.Annotations{
		builtin.ReadChannelTool:    read,
		builtin.ListChannelsTool:   read,
		builtin.SearchMessagesTool: read,
		builtin.PostMessageTool:    shared,
		builtin.ReplyInThreadTool:  shared,
		builtin.SendDMTool:         shared,
		builtin.ReactToMessageTool: stated,
		builtin.JoinChannelTool:    own,
		builtin.LeaveChannelTool:   own,

		// THE PERSON'S OWN GESTURES, classified by the same switch: the
		// dashboard's act transport refuses a proven read, and an assistant
		// is told which of these destroys what somebody else wrote.
		chatlet.CreateChannelTool:     shared,
		chatlet.OpenDMTool:            stated,
		chatlet.UpdateChannelTool:     restated,
		chatlet.SetChannelMembersTool: restated,
		chatlet.EditMessageTool:       restated,
		chatlet.DeleteMessageTool:     restated,
		chatlet.MarkChatReadTool:      own,
	}
	for _, name := range append(chatlet.Tools(), chatlet.OperatorOnlyTools()...) {
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

// A ROOM'S NAME IS ITS ADDRESS, and it resolves for any room this seat may
// read, joined or not. A model types a room's name and never its uuid, so a
// tool that took only the id would refuse the first call of every turn that
// had not just listed the rooms. This fake resolves its one public room for
// every viewer, as the store does.
func TestAChannelNameResolvesAsTheRoomsAddress(t *testing.T) {
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
	// AS THE SEAT, because a private room's address resolves only for its
	// members: a lookup made as anybody else names rooms the seat cannot
	// read.
	for _, viewer := range f.viewers {
		if viewer != "agent-cto" {
			t.Errorf("a name was resolved for %q, want the seat it is said as", viewer)
		}
	}

	// AND AN ID COSTS NO LOOKUP. The common case is a room id carried in
	// from the wake that woke this seat, and resolving it through the
	// reader would put a read on the hot path of every answer.
	f.reads = 0
	callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": chatRoomID, "body": "hello",
	})
	if f.reads != 0 {
		t.Errorf("a post addressed by id reached the reader %d times", f.reads)
	}

	// AND A ROOM THE STORE DOES NOT HOLD NAMES BOTH ADDRESSES, so the seat
	// reading it is never sent after an id it did not need: a name reaches
	// every room it may read as surely as an id does.
	f.writeErr = fmt.Errorf("%w: room %s", chatlet.ErrNotFound, chatRoomID)
	got := callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": chatRoomID, "body": "hello",
	})
	if tools.RefusalOf(got) != tools.RefusalNotFound ||
		!strings.Contains(got.Output, builtin.ListChannelsTool) ||
		!strings.Contains(got.Output, "its id or to its name") {

		t.Errorf("a post to a room the store does not hold answered %+v, want "+
			"not-found naming where a room is looked up by either address", got)
	}
}

// A ROOM NAMED BY A WORD NOTHING MATCHES IS THE ARGUMENT'S TO FIX, and a
// lookup this node could not serve is the node's.
//
// The first is fixed by naming a room the caller may read — a name resolves
// for every one of those, joined or not, and a private room the caller is not
// in answers exactly as a name nobody holds, so "not found" would claim more
// than the lookup knows. The second says nothing about the name at all, and
// told "invalid" a person would retype a name that was right.
func TestARoomNameThatResolvesToNothingIsTheArgumentsToFix(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	got := callChat(t, chatRegistry(t, chatDeps(f)), builtin.PostMessageTool,
		turnFor(t, "agent-cto"), map[string]any{"channel": "marketing", "body": "hi"})
	if tools.RefusalOf(got) != tools.RefusalInvalid ||
		!strings.Contains(got.Output, builtin.ListChannelsTool) {
		t.Errorf("a name nothing matches answered %+v, want the argument class "+
			"saying how to find the room", got)
	}

	f = newFakeChat()
	f.readErr = &statelog.Unavailable{Reason: statelog.ReasonDeferred}
	got = callChat(t, chatRegistry(t, chatDeps(f)), builtin.PostMessageTool,
		turnFor(t, "agent-cto"), map[string]any{"channel": "eng", "body": "hi"})
	if tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Errorf("a name lookup that failed answered %+v, want the node's class", got)
	}
	if len(f.posts) != 0 {
		t.Errorf("a post whose room could not be resolved reached the store: %v", f.posts)
	}

	// AND A SURFACE WITH NO READER CANNOT RESOLVE A NAME AT ALL: the id is
	// the argument that works there, which is the argument's class again.
	f = newFakeChat()
	got = callChat(t, chatRegistry(t, builtin.ChatDeps{Writer: f}), builtin.PostMessageTool,
		turnFor(t, "agent-cto"), map[string]any{"channel": "eng", "body": "hi"})
	if tools.RefusalOf(got) != tools.RefusalInvalid ||
		!strings.Contains(got.Output, "by its id") {
		t.Errorf("a name on a surface with no reader answered %+v", got)
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
	got := callChat(t, chatRegistry(t, chatDeps(newFakeChat())), builtin.ListChannelsTool,
		turnFor(t, "agent-cto"), nil)
	if got.Failed {
		t.Fatalf("list_channels failed: %s", got.Output)
	}
	if strings.Contains(got.Output, "unread") {
		t.Errorf("a seat's listing carries an unread count: %s", got.Output)
	}
}

// readStateChat is the operator catalogue's list_channels over f, handed the
// read state ReadState answers, recording whose read state it was asked for.
func readStateChat(t *testing.T, f *fakeChat, state coord.ChatReadState,
	err error) (tools.Callable, *[]string) {

	t.Helper()
	var asked []string
	deps := chatDeps(f)
	deps.Actor = operatorChatActor
	deps.Now = func() time.Time { return time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC) }
	deps.ReadState = func(_ context.Context, viewer string) (coord.ChatReadState, error) {
		asked = append(asked, viewer)
		return state, err
	}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Chat: deps, Authorize: builtin.Decide(chartLeads),
	}) {
		if tool.Name() == builtin.ListChannelsTool {
			return tool, &asked
		}
	}
	t.Fatal("list_channels is not served with a read state")
	return nil, nil
}

// listing decodes list_channels' answer.
func listing(t *testing.T, got tools.Result) (top map[string]any, rows []map[string]any) {
	t.Helper()
	if got.Failed {
		t.Fatalf("list_channels failed: %s", got.Output)
	}
	if err := json.Unmarshal([]byte(got.Output), &top); err != nil {
		t.Fatalf("the answer is not JSON: %v: %s", err, got.Output)
	}
	raw, _ := top["channels"].([]any)
	for _, row := range raw {
		decoded, _ := row.(map[string]any)
		rows = append(rows, decoded)
	}
	if len(rows) == 0 {
		t.Fatalf("the rooms were not listed: %s", got.Output)
	}
	return top, rows
}

// A PERSON'S COUNTS ARE TAKEN AGAINST THEIR OWN CURSORS. The listing counts
// above where the person read, so the cursor has to reach the read — a count
// taken from zero calls every room wholly unread to somebody who has read most
// of it — and it has to be THE PERSON's: the handle the read state is asked
// for is the seat the call acts as, which is the one `mark_chat_read` wrote it
// under.
func TestAPersonsUnreadCountsAreTakenAgainstTheirOwnCursors(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	tool, asked := readStateChat(t, f, coord.ChatReadState{
		Cursors: map[string]int64{chatRoomID: 4},
	}, nil)
	_, rows := listing(t, callOperator(t, tool, nil))
	if unread, _ := rows[0]["unread"].(float64); unread != 3 {
		t.Errorf("a person who read through 4 of %d messages is told %v are new, "+
			"want 3: %v", chatRoomMessages, rows[0]["unread"], rows[0])
	}
	if got := f.listed[len(f.listed)-1].Cursors; got[chatRoomID] != 4 {
		t.Errorf("the listing was counted against cursors %v, want the person's", got)
	}
	if !slices.Equal(*asked, []string{"jane"}) {
		t.Errorf("the read state was asked for %v, want the seat the call acts "+
			"as (jane)", *asked)
	}

	// NONE AT ALL IS SOMEBODY WHO HAS OPENED NOTHING, and to them every
	// room's tail is new.
	tool, _ = readStateChat(t, newFakeChat(), coord.ChatReadState{}, nil)
	_, rows = listing(t, callOperator(t, tool, nil))
	if unread, _ := rows[0]["unread"].(float64); unread != chatRoomMessages {
		t.Errorf("a person who has opened nothing is told %v are new, want %d",
			rows[0]["unread"], chatRoomMessages)
	}
}

// A ROOM THE PERSON MUTED, AND A DO-NOT-DISTURB THEY ARE IN, ARE SAID AS THE
// RAIL SAYS THEM. A person's assistant reads their rooms through this tool, and
// a muted room handed over as ordinary unread is a backlog it offers to work
// through that the person deliberately silenced.
func TestAPersonsMutesAndDoNotDisturbAreListedAsTheRailShowsThem(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)
	tool, _ := readStateChat(t, newFakeChat(), coord.ChatReadState{
		Muted: []string{chatRoomID}, DNDUntil: until,
	}, nil)
	top, rows := listing(t, callOperator(t, tool, nil))
	if rows[0]["muted"] != true {
		t.Errorf("a room the person muted is listed as %v", rows[0])
	}
	if unread, _ := rows[0]["unread"].(float64); unread != chatRoomMessages {
		t.Errorf("a mute changed the count to %v: it suppresses notice, never "+
			"what is there", rows[0]["unread"])
	}
	if top["dnd_until"] != until.Format(time.RFC3339) {
		t.Errorf("a person in do-not-disturb until %s is listed with %v",
			until.Format(time.RFC3339), top["dnd_until"])
	}

	// AND A ROOM NOT MUTED, BY SOMEBODY WHOSE DO-NOT-DISTURB IS OVER, SAYS
	// NEITHER.
	tool, _ = readStateChat(t, newFakeChat(), coord.ChatReadState{
		DNDUntil: until.Add(-24 * time.Hour),
	}, nil)
	top, rows = listing(t, callOperator(t, tool, nil))
	if _, said := rows[0]["muted"]; said {
		t.Errorf("a room nobody muted is marked: %v", rows[0])
	}
	if _, said := top["dnd_until"]; said {
		t.Errorf("a do-not-disturb that ended yesterday is listed as current: %v", top)
	}
}

// A READ STATE THAT COULD NOT BE READ IS NOT A CURSOR AT ZERO. Counted from
// zero, every room would read as wholly unread to a person who has read all of
// it — so the listing is still served, with no count on ANY row, and says why
// under the rail's own name for the fact rather than under `unread`, which
// every row uses for a number — in fixed words, never the store's error, which
// names its transport.
func TestAnUnreadableReadStateCostsTheCountsAndNotTheListing(t *testing.T) {
	t.Parallel()
	tool, _ := readStateChat(t, newFakeChat(), coord.ChatReadState{},
		fmt.Errorf("%w: nats: no responders at 10.0.0.7:4222", coord.ErrUnavailable))
	top, rows := listing(t, callOperator(t, tool, nil))
	if rows[0]["name"] != chatRoomName {
		t.Errorf("the rooms were not listed: %v", rows)
	}
	for _, row := range rows {
		for _, key := range []string{"unread", "unread_capped", "muted"} {
			if _, said := row[key]; said {
				t.Errorf("a read state nobody read put %q on a row: %v", key, row)
			}
		}
	}
	said, _ := top["read_state"].(string)
	if !strings.HasPrefix(said, "unknown:") ||
		!strings.Contains(said, "the coordination store did not answer") {
		t.Errorf("the listing says %q of its read state, want unknown, and why", said)
	}
	if strings.Contains(said, "10.0.0.7") {
		t.Errorf("the listing hands its caller the store's transport: %q", said)
	}
	if _, said := top["unread"]; said {
		t.Errorf("the answer puts a sentence under `unread`, which every row uses "+
			"for a number: %v", top)
	}
}

// A TOOL WHOSE HALF IS ABSENT IS OMITTED, never registered and broken: a model
// shown a tool that always fails learns to distrust the whole catalogue. The
// gestures decided after a read — whose words a message is, who made a room —
// need the reader too.
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
	for _, name := range chatlet.Tools() {
		if _, ok := none.Lookup(name); ok {
			t.Errorf("%s was registered for a company that runs no Chatlet", name)
		}
	}

	served := map[string]bool{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Chat: builtin.ChatDeps{Writer: f}, Authorize: builtin.Decide(chartLeads),
	}) {
		served[tool.Name()] = true
	}
	for _, name := range []string{chatlet.EditMessageTool, chatlet.DeleteMessageTool,
		chatlet.UpdateChannelTool} {
		if served[name] {
			t.Errorf("%s was served with no chat reader to decide it on", name)
		}
	}
	if served[chatlet.MarkChatReadTool] {
		t.Error("mark_chat_read was served with no read-state store")
	}
}

// A CALLER THE ENGINE CANNOT RESOLVE TO A SEAT GETS NOTHING, READS INCLUDED —
// and the refusal carries the remedy.
//
// A private room's contents are decided by its membership, so a caller with no
// seat has no membership: serving it "everything public" would invent a viewer
// the company never declared. Two shapes reach here behind a table that
// admitted them: the actor seam answering nobody, and a credential acting as
// itself — which the table refuses first (`unseated`), so this is the arm a
// surface wired without the table would otherwise reach. FORBIDDEN, the class
// whose sentence a tool tells as written, which is what carries the remedy.
func TestACallerWithNoSeatIsRefusedEveryChatToolWithTheRemedy(t *testing.T) {
	t.Parallel()
	args := map[string]map[string]any{
		builtin.ListChannelsTool:   nil,
		builtin.ReadChannelTool:    {"channel": chatRoomID},
		builtin.SearchMessagesTool: {"text": "anything"},
		chatlet.MarkChatReadTool:   {"mute": []any{chatRoomID}},
	}
	for _, w := range seatChatWrites() {
		args[w.tool] = w.args
	}
	for name, a := range operatorChatWrites {
		args[name] = a
	}
	for shape, actor := range map[string]func(context.Context, *turnctx.Turn) (chatlet.Actor, error){
		"nobody": func(context.Context, *turnctx.Turn) (chatlet.Actor, error) {
			return chatlet.Actor{}, errors.New("builtin: this request carries nobody " +
				"to attribute a write to (anonymous)")
		},
		"a credential acting as itself": func(context.Context, *turnctx.Turn) (chatlet.Actor, error) {
			return chatlet.Actor{Handle: "token:ci", Kind: chatlet.AuthorOperator,
				OperatorID: "token:ci"}, nil
		},
	} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			f := newFakeChat()
			var flushed []flush
			catalogue := operatorChat(t, f, fakeCursors{flushed: &flushed}, actor)
			for _, name := range append(chatlet.Tools(), chatlet.OperatorOnlyTools()...) {
				tool, served := catalogue[name]
				if !served {
					t.Errorf("the operator catalogue does not serve %s, so this "+
						"case cannot hold it", name)
					continue
				}
				got := callOperator(t, tool, args[name])
				if tools.RefusalOf(got) != tools.RefusalForbidden ||
					!strings.Contains(got.Output, "crewlet iam bind") {
					t.Errorf("%s answered a caller with no seat %+v, want forbidden "+
						"carrying the remedy", name, got)
				}
			}
			if f.reads != 0 || len(f.posts) != 0 || len(f.keys) != 0 || len(flushed) != 0 {
				t.Errorf("a caller with no seat reached the backend: %d reads, %d "+
					"posts, %d other writes, %d read-state flushes", f.reads,
					len(f.posts), len(f.keys), len(flushed))
			}
		})
	}
}

// A READ THAT COULD NOT BE SERVED IS NEVER AN EMPTY ROOM. A node whose rows
// are behind — or one that failed — must not be able to tell a seat that
// nobody answered, because a seat that believes nobody answered asks again.
func TestAFailedChatReadIsNotReportedAsSilence(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"condition": &statelog.Unavailable{Reason: statelog.ReasonDeferred},
		"fault":     errors.New("open /var/lib/crewlet/replicated.db: disk I/O error"),
	} {
		f := newFakeChat()
		f.readErr = err
		got := callChat(t, chatRegistry(t, chatDeps(f)), builtin.ReadChannelTool,
			turnFor(t, "agent-cto"), map[string]any{"channel": chatRoomID})
		if !got.Failed || !strings.Contains(got.Output, "NOT an empty room") {
			t.Errorf("%s: the failure could be read as silence: %s", name, got.Output)
		}
		if strings.Contains(got.Output, "/var/lib") {
			t.Errorf("%s: a fault's own words reached the caller: %s", name, got.Output)
		}
	}
}

// EVERY CHAT TOOL ANSWERS A REFUSAL WITH ITS CLASS, never with the argument
// class a bare failure carries.
//
// The class is what a person's surface acts on — which status the act
// transport answers, which code's sentence it is told in, whether a retry could
// ever help — so a node that could not read its rows, a lost race and a name
// somebody else holds must each tell a person something different. Each
// refusal here is shaped the way the store builds it and reaches the tool
// through its writer or its reader, so a call site that bypasses the
// classifier is caught at the call site.
//
// Mutation: drop any arm of chatClass and its rows go red.
func TestEveryChatToolAnswersARefusalWithItsClass(t *testing.T) {
	t.Parallel()
	for _, w := range seatChatWrites() {
		for _, c := range chatWriteRefusals {
			t.Run(w.tool+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				f := newFakeChat()
				f.writeErr = c.err
				got := callChat(t, chatRegistry(t, chatDeps(f)), w.tool,
					turnFor(t, "agent-cto"), w.args)
				if tools.RefusalOf(got) != c.want {
					t.Errorf("%s refused by %v answered class %q, want %q: %s",
						w.tool, c.err, tools.RefusalOf(got), c.want, got.Output)
				}
			})
		}
	}

	// AND THE GESTURES ONLY A PERSON MAKES, through the operator catalogue
	// that serves them to the dashboard: the same store, the same sentinels,
	// and a person's screen acting on the class exactly as a model does.
	for name, args := range operatorChatWrites {
		for _, c := range chatWriteRefusals {
			t.Run("operator/"+name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				f := newFakeChat()
				f.writeErr = c.err
				got := callOperator(t, operatorChat(t, f, nil, nil)[name], args)
				if tools.RefusalOf(got) != c.want {
					t.Errorf("%s refused by %v answered class %q, want %q: %s",
						name, c.err, tools.RefusalOf(got), c.want, got.Output)
				}
			})
		}
	}

	reads := map[string]map[string]any{
		builtin.ReadChannelTool:    {"channel": chatRoomID},
		builtin.ListChannelsTool:   nil,
		builtin.SearchMessagesTool: {"text": "the backoff"},
	}
	for name, args := range reads {
		for want, err := range map[tools.Refusal]error{
			// A READ THIS NODE COULD NOT SERVE IS NEVER NOT-FOUND: a
			// surface told so would render the room as gone.
			tools.RefusalUnavailable: &statelog.Unavailable{Reason: statelog.ReasonDeferred},
			tools.RefusalInternalError: errors.New("open /var/lib/crewlet/replicated.db: " +
				"disk I/O error"),
			tools.RefusalNotFound: fmt.Errorf("%w: room room-1", chatlet.ErrNotFound),
			// AN ARGUMENT THE READER REFUSED — a cursor that is no
			// position — is the caller's to fix: "try again" is a retry
			// refused the same way every time.
			tools.RefusalInvalid: chatlet.ChannelPatch{}.Validate(),
			// A READER WITH NO SEAT BEHIND IT is refused by the store
			// naming the binding it lacks, which is no retry either.
			tools.RefusalForbidden: fmt.Errorf("%w: viewer: this chat read "+
				"resolved to no seat", chatlet.ErrForbidden),
		} {
			t.Run(name+"/"+string(want), func(t *testing.T) {
				t.Parallel()
				f := newFakeChat()
				f.readErr = err
				got := callChat(t, chatRegistry(t, chatDeps(f)), name,
					turnFor(t, "agent-cto"), args)
				if tools.RefusalOf(got) != want {
					t.Errorf("%s refused by %v answered class %q, want %q: %s",
						name, err, tools.RefusalOf(got), want, got.Output)
				}
			})
		}
	}
}

// operatorChatWrites are the gestures only a person makes that append to the
// log, each with arguments that reach its writer — every one the operator
// catalogue serves but mark_chat_read, which writes the coordination store.
var operatorChatWrites = map[string]map[string]any{
	chatlet.CreateChannelTool: {"channel": "launch"},
	chatlet.OpenDMTool:        {"to": []any{"bob"}},
	chatlet.UpdateChannelTool: {"channel": chatRoomID, "topic": "the launch"},
	chatlet.SetChannelMembersTool: {
		"channel": chatRoomID, "members": []any{"jane"},
	},
	chatlet.EditMessageTool:   {"channel": chatRoomID, "message_id": "m1", "body": "fixed"},
	chatlet.DeleteMessageTool: {"channel": chatRoomID, "message_id": "m1"},
}

// chatWriteRefusals are the chat store's write refusals, each with the class
// every chat write tool answers it with.
var chatWriteRefusals = []struct {
	name string
	want tools.Refusal
	err  error
}{
	{"invalid", tools.RefusalInvalid,
		fmt.Errorf("%w: body: a message is 40960 bytes", chatlet.ErrInvalid)},
	{"archived", tools.RefusalConflict,
		fmt.Errorf("%w: room-1 was archived", chatlet.ErrArchived)},
	{"reactions full", tools.RefusalConflict,
		fmt.Errorf("%w: the message reached its 32 distinct emoji", chatlet.ErrReactionsFull)},
	{"forbidden", tools.RefusalForbidden,
		fmt.Errorf("%w: room-1 is a private room", chatlet.ErrForbidden)},
	{"name taken", tools.RefusalExists, fmt.Errorf("%w: launch", chatlet.ErrNameTaken)},
	{"room exists", tools.RefusalExists, fmt.Errorf("%w: dm-1", chatlet.ErrRoomExists)},
	{"conflict", tools.RefusalConflict, fmt.Errorf("%w: room-1", chatlet.ErrConflict)},
	{"not found", tools.RefusalNotFound, fmt.Errorf("%w: room room-1", chatlet.ErrNotFound)},
	// A ROOM A NEWER PEER MADE IS THIS NODE'S, NOT A PERMISSION.
	{"unknown kind", tools.RefusalUnavailable,
		fmt.Errorf("%w: room-1 is a \"guild\" room", chatlet.ErrUnknownKind)},
	{"log refused", tools.RefusalUnavailable,
		&statelog.Unavailable{Reason: statelog.ReasonDeferred}},
	// AND A FAULT IS THE NODE'S, whose words are the log's.
	{"fault", tools.RefusalInternalError,
		errors.New("statelog: read the operation ledger: disk I/O error")},
}

// A DIRECT MESSAGE IS TWO RECORDS AND ANSWERS FOR BOTH.
//
// Its conversation's open and its post are appended one after the other, so
// the call is as certain as the LESS certain of the two and a caller reading
// after it waits for the LATER position — the rule every write that appends
// twice answers by. And where the post is refused after the open landed, the
// answer says the conversation is there: "nothing was written" would be false
// about the open, and the class is the post's, since the post is what did not
// happen.
func TestADirectMessageAnswersForBothItsRecords(t *testing.T) {
	t.Parallel()
	dm := func(f *fakeChat, body string) tools.Result {
		return callChat(t, chatRegistry(t, chatDeps(f)), builtin.SendDMTool,
			turnFor(t, "agent-cto"), map[string]any{"to": []any{"agent-ceo"}, "body": body})
	}
	f := newFakeChat()
	f.opened = &statelog.Result{Outcome: statelog.OutcomePending,
		Position: statelog.Position{Stream: "CREWLET_CHAT_LOG", Seq: 11}}
	got := dm(f, "a word")
	if got.Failed {
		t.Fatalf("send_dm failed: %s", got.Output)
	}
	if !strings.Contains(got.Output, `"outcome": "pending"`) {
		t.Errorf("a direct message whose open is pending answered as more certain "+
			"than its open: %s", got.Output)
	}
	if !strings.Contains(got.Output, `"position": "CREWLET_CHAT_LOG@0:21"`) {
		t.Errorf("a direct message answered a position other than its post's, "+
			"the later of its two: %s", got.Output)
	}

	// THE STORE'S OWN FIELD REFUSAL, so the sentence the caller is told is
	// the store's without its Go prefix.
	f = newFakeChat()
	f.postErr = chatlet.ChannelPatch{}.Validate()
	got = dm(f, "a word")
	if tools.RefusalOf(got) != tools.RefusalInvalid {
		t.Fatalf("a refused post after an open answered %+v, want the post's class", got)
	}
	for _, want := range []string{"is open", "dm-agent-ceo", "was NOT sent", "`patch`"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the answer does not say %q: %s", want, got.Output)
		}
	}

	// A POST THE NODE COULD NOT MAKE AFTER THE OPEN is the node's class —
	// and the answer still says the conversation is open, never that
	// nothing was written: the open is a record, and that sentence is the
	// one this answer exists to correct.
	for name, c := range map[string]struct {
		err  error
		want tools.Refusal
		says string
	}{
		"condition": {&statelog.Unavailable{Reason: statelog.ReasonDeferred},
			tools.RefusalUnavailable, "did not land"},
		"fault": {errors.New("statelog: read the operation ledger: disk I/O"),
			tools.RefusalInternalError, "a fault in this node"},
	} {
		f = newFakeChat()
		f.postErr = c.err
		got = dm(f, "a word")
		if tools.RefusalOf(got) != c.want {
			t.Fatalf("%s: a post the node could not make answered %+v, want %q",
				name, got, c.want)
		}
		for _, want := range []string{"is open", "dm-agent-ceo", "was NOT sent", c.says} {
			if !strings.Contains(got.Output, want) {
				t.Errorf("%s: the answer does not say %q: %s", name, want, got.Output)
			}
		}
		if strings.Contains(got.Output, "nothing was written") ||
			strings.Contains(got.Output, "disk I/O") {
			t.Errorf("%s: the answer says nothing was written, or hands over the "+
				"fault's own words: %s", name, got.Output)
		}
	}

	// A POST NOBODY ANSWERED AFTER THE OPEN is unknown, never "NOT sent":
	// the router does not repeat a post carrying no key of its caller's, so
	// one a data node never answered may have been said.
	f = newFakeChat()
	f.postErr = fmt.Errorf("%w (chat.post, asked of data-1)", estate.ErrOutcomeUnknown)
	got = dm(f, "a word")
	if !tools.UnknownOf(got) || tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Fatalf("a post nobody answered after an open answered %+v, want an "+
			"unavailable refusal marked unknown", got)
	}
	if !strings.Contains(got.Output, "is open") || strings.Contains(got.Output, "NOT sent") {
		t.Errorf("a post nobody answered does not say the conversation is open, "+
			"or says the message was not sent: %s", got.Output)
	}

	// A POST WHOSE OUTCOME IS UNKNOWN AFTER THE OPEN says the open landed,
	// as a refused one does: two records, and only the second is in doubt.
	f = newFakeChat()
	f.outcome = &statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: "op-post"}
	got = dm(f, "a word")
	if !tools.UnknownOf(got) {
		t.Fatalf("a post of unknown outcome answered %+v, want unknown", got)
	}
	for _, want := range []string{"is open", "dm-agent-ceo", "unknown", "op-post"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the unknown answer does not say %q: %s", want, got.Output)
		}
	}

	// AND WHAT THE MESSAGE SAYS IS REFUSED BEFORE ANYTHING IS OPENED: a body
	// over the cap is refused by the post however the room is found, so
	// opening the conversation first only left a room in every
	// participant's list, opened by a refusal.
	f = newFakeChat()
	got = dm(f, strings.Repeat("x", chatlet.MaxBody+1))
	if tools.RefusalOf(got) != tools.RefusalInvalid {
		t.Fatalf("a body over the cap answered %+v, want invalid", got)
	}
	if len(f.keys) != 0 || len(f.posts) != 0 {
		t.Errorf("a message refused for what it says opened %d conversations "+
			"and posted %d messages", len(f.keys), len(f.posts))
	}
	if strings.Contains(got.Output, "is open") {
		t.Errorf("a refusal before the open says the conversation is open: %s", got.Output)
	}
}

// A SEARCH TEXT PAST THE BOUND EVERY RANKED SEARCH SHARES IS REFUSED NAMING IT,
// and never reaches the index — where it would come back as a failure the model
// reads as chat being unreadable, rather than as what to send instead. At the
// bound it is a search.
func TestALongChatSearchIsRefusedNotCut(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")

	got := callChat(t, reg, builtin.SearchMessagesTool, turn, map[string]any{
		"text": strings.Repeat("w", knowledge.MaxQueryBytes+1),
	})
	if !got.Failed || !strings.Contains(got.Output, fmt.Sprintf("takes at most %d",
		knowledge.MaxQueryBytes)) {
		t.Fatalf("a long text was not refused naming the limit: %q", got.Output)
	}
	if len(f.searched) != 0 {
		t.Fatal("a refused text still reached the index")
	}
	if got := callChat(t, reg, builtin.SearchMessagesTool, turn, map[string]any{
		"text": strings.Repeat("w", knowledge.MaxQueryBytes),
	}); got.Failed {
		t.Fatalf("a text at the bound was refused: %q", got.Output)
	}
}

// A SEARCH IS ASKED AS THE SEAT, NARROWED AS NAMED, AND A ROOM'S NAME THAT
// NAMES NOTHING IS REFUSED rather than dropped.
//
// The viewer is what decides which rooms are searched — a private room's
// contents are its membership's — so it is the seat the call is made as and
// never an argument. A `channel` is resolved like every other tool's; a name
// nothing readable holds, searched in its absence, would hand back hits from
// rooms the caller did not mean, read as the one it did.
func TestAChatSearchIsAskedAsTheSeatAndNarrowedAsNamed(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")

	if got := callChat(t, reg, builtin.SearchMessagesTool, turn, map[string]any{
		"text": "the backoff", "channel": "#eng", "author": " agent-ceo ", "limit": 7,
	}); got.Failed {
		t.Fatalf("search_messages failed: %s", got.Output)
	}
	want := estate.ChatQuery{Viewer: "agent-cto", Text: "the backoff",
		Channel: chatRoomID, Author: "agent-ceo", Limit: 7,
		Fresh: statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)}}
	if got := f.searched[len(f.searched)-1]; got != want {
		t.Errorf("the search asked %+v, want %+v", got, want)
	}

	searched := len(f.searched)
	got := callChat(t, reg, builtin.SearchMessagesTool, turn, map[string]any{
		"text": "the backoff", "channel": "marketing",
	})
	if tools.RefusalOf(got) != tools.RefusalInvalid || len(f.searched) != searched {
		t.Errorf("a search narrowed to a name nothing readable holds answered %+v "+
			"and searched %d times more", got, len(f.searched)-searched)
	}
}

// A CHAT SEARCH'S HITS ARE KEYED AS EVERY OTHER CHAT ANSWER IS. The index's own
// value has no JSON names, and handed over whole it keyed a hit `MessageID`
// beside the `message_id` the transcript, the receipts and reply_in_thread's
// own description use, so the id a seat replies under was spelled two ways.
//
// Mutation: answer `answer.Hits` as it was and every key here goes missing.
func TestAChatSearchHitIsKeyedAsEveryChatAnswerIs(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	at := time.Unix(1_700_000_000, 0).UTC()
	f.hits = []search.ChatHit{{MessageID: "m1", ChannelID: chatRoomID, ThreadRoot: "m0",
		Author: "agent-ceo", Snippet: "the backoff", CreatedAt: at, Score: 2.5}}
	got := callChat(t, chatRegistry(t, chatDeps(f)), builtin.SearchMessagesTool,
		turnFor(t, "agent-cto"), map[string]any{"text": "the backoff"})
	if got.Failed {
		t.Fatalf("search_messages failed: %s", got.Output)
	}
	var answer struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil || len(answer.Messages) != 1 {
		t.Fatalf("the answer is %s (%v)", got.Output, err)
	}
	want := map[string]any{"message_id": "m1", "channel_id": chatRoomID, "thread_root": "m0",
		"author": "agent-ceo", "at": at.Format(time.RFC3339), "score": 2.5}
	for key, value := range want {
		if answer.Messages[0][key] != value {
			t.Errorf("the hit's %s is %v, want %v: %v", key, answer.Messages[0][key], value,
				answer.Messages[0])
		}
	}
	// THE SNIPPET IS THE MESSAGE ITSELF, and arrives fenced as one.
	if said := unfenced(t, answer.Messages[0]["snippet"]); said != "the backoff" {
		t.Errorf("the hit's snippet says %q, want the message", said)
	}
	if len(answer.Messages[0]) != len(want)+1 {
		t.Errorf("the hit carries %v, want exactly %v and its snippet", answer.Messages[0], want)
	}
}

// A SEARCH OF AN INDEX STILL BEING BUILT IS NEVER "NOTHING MATCHED". The index
// a node answers from may not have reached a message yet, and a seat told
// nobody said it acts on that — so an empty answer says the index is catching
// up, and hits beside it say they may not be all there is.
//
// Mutation: drop the Building arm from search_messages and the empty case
// reads as nothing matched.
func TestASearchOfAnIndexStillBeingBuiltSaysSo(t *testing.T) {
	t.Parallel()
	turn := turnFor(t, "agent-cto")
	args := map[string]any{"text": "the backoff"}

	f := newFakeChat()
	f.building, f.hits = true, nil
	got := callChat(t, chatRegistry(t, chatDeps(f)), builtin.SearchMessagesTool, turn, args)
	if got.Failed || !strings.Contains(got.Output, "still being built") {
		t.Errorf("an empty search of an index being built answered %+v", got)
	}

	f = newFakeChat()
	f.building = true
	got = callChat(t, chatRegistry(t, chatDeps(f)), builtin.SearchMessagesTool, turn, args)
	if got.Failed || !strings.Contains(got.Output, `"building": true`) {
		t.Errorf("hits from an index being built do not say so: %+v", got)
	}

	f = newFakeChat()
	f.hits = nil
	got = callChat(t, chatRegistry(t, chatDeps(f)), builtin.SearchMessagesTool, turn, args)
	if got.Failed || !strings.Contains(got.Output, "Nothing in the rooms you can read") ||
		strings.Contains(got.Output, "being built") {
		t.Errorf("an empty search of a built index answered %+v", got)
	}
}

// A FULL PAGE OF CHAT HITS IS ONE TOOL ANSWER — OF PROSE.
//
// The index answers at most [search.MaxChatSearchLimit] hits of at most
// [search.ChatSnippetBytes] each, and a tool answer is at most
// [builtin.ToolAnswerBytes]: a page past it is REFUSED rather than delivered,
// so a ceiling the answer could not carry would be a search that answers its
// own largest page of ordinary words with "ask for fewer". Held at a heavy one
// — every snippet cut at its bound, both marks on it, fenced, a different
// author of the longest handle on every hit (so the fence's label and the
// envelope each name the longest handle once a hit), and quotes throughout,
// each of which the answer's JSON writes as two bytes.
//
// A PAGE OF MARKUP IS ONE ANSWER TOO, AND READS AS WRITTEN: a tool answer
// carries `<`, `>` and `&` as the bytes they are, not as the six-byte escapes
// an HTML-safe encoder writes, which made fifty snippets of them four times
// the page above and refused a room of pasted code as "ask for fewer".
//
// AND A PAGE THE ANSWER'S JSON STILL WIDENS IS REFUSED NAMING `limit`, never
// cut: a control character — a terminal's escape, pasted with its colours —
// is written as six bytes, so fifty snippets of them are the case the ceiling
// is for. Held so the limit's doc stays true in both directions — a limit
// small enough to carry that page turns this half red and the doc is re-read.
//
// Mutation: raise MaxChatSearchLimit to 45 and the first half goes red; put
// the HTML escaping back and the markup half does; drop the narrowing advice
// from the search's answer and the last one does.
func TestAFullChatSearchPageIsOneToolAnswer(t *testing.T) {
	t.Parallel()
	page := func(body string) tools.Result {
		f := newFakeChat()
		f.hits = nil
		for i := range search.MaxChatSearchLimit {
			f.hits = append(f.hits, search.ChatHit{
				MessageID: uuid.NewString(), ChannelID: uuid.NewString(),
				ThreadRoot: uuid.NewString(),
				Author:     strings.Repeat("h", iam.MaxLogin-2) + fmt.Sprintf("%02d", i),
				AuthorKind: string(chatlet.AuthorAgent),
				Snippet:    "…" + body + "…", CreatedAt: time.Now().UTC(), Score: 12.3456789,
			})
		}
		return callChat(t, chatRegistry(t, chatDeps(f)), builtin.SearchMessagesTool,
			turnFor(t, "agent-cto"), map[string]any{"text": "the backoff"})
	}
	quoted := strings.Repeat(`a "b" `, search.ChatSnippetBytes/len(`a "b" `))
	if got := page(quoted); got.Failed {
		t.Fatalf("a full page of hits was refused as one answer: %s", got.Output)
	}
	markup := page(strings.Repeat("<", search.ChatSnippetBytes))
	if markup.Failed {
		t.Fatalf("a full page of markup was refused as one answer: %.200q", markup.Output)
	}
	if !strings.Contains(markup.Output, strings.Repeat("<", 64)) ||
		strings.Contains(markup.Output, `\u003c`) {
		t.Errorf("a page of markup reads %.200q, want the snippets' own `<`", markup.Output)
	}
	got := page(strings.Repeat("\x1b", search.ChatSnippetBytes))
	if !got.Failed || !strings.Contains(got.Output, "`limit`") {
		t.Errorf("a full page of control characters answered %.200q, want refused "+
			"naming `limit`", got.Output)
	}
}

// AN EDIT AND A DELETE ARE DECIDED ON WHOSE WORDS THE MESSAGE IS.
//
// The tools name a message by its id, so the table cannot form the object
// from the arguments: an admin is admitted on the UNREAD message — moderation,
// which reaches a message in a room the moderator cannot read without showing
// it to them — and anybody else is told to read it and is decided again on
// its author. What came back is carried to the store as the decision
// ([chatlet.MessageAuthority.Moderate]), never re-derived there.
//
// Mutation: ask the second decision with no Author and the author is refused
// their own message; pass Moderate true from both arms and a colleague's
// delete reaches the store as moderation.
func TestARemarkIsDecidedOnWhoseWordsItIs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		grants   []iam.Grant
		author   string
		admitted bool
		read     bool
		moderate bool
	}{
		{"the author", []iam.Grant{iam.GrantChatlet}, "jane", true, true, false},
		{"a colleague", []iam.Grant{iam.GrantChatlet}, "sam", false, true, false},
		{"an admin, unread", []iam.Grant{iam.GrantAdmin}, "sam", true, false, true},
	} {
		for _, tool := range []string{chatlet.EditMessageTool, chatlet.DeleteMessageTool} {
			t.Run(c.name+"/"+tool, func(t *testing.T) {
				t.Parallel()
				f := newFakeChat()
				f.author = c.author
				catalogue := operatorChat(t, f, nil, nil)
				got := callOperatorAs(personBoundTo("jane", c.grants...), t,
					catalogue[tool], operatorChatWrites[tool])
				if got.Failed == c.admitted {
					t.Fatalf("%s's %s answered %+v, want admitted %v", c.name, tool,
						got, c.admitted)
				}
				if read := len(f.threads) > 0; read != c.read {
					t.Errorf("%s's %s read the message = %v, want %v", c.name, tool,
						read, c.read)
				}
				if !c.admitted {
					var refused *builtin.DecisionRefused
					if !errors.As(got.Cause, &refused) ||
						refused.Decision.Reason != authz.ReasonNotAuthor {
						t.Errorf("a colleague's %s was refused %+v, want the table's "+
							"not_author", tool, got)
					}
					if len(f.keys) != 0 {
						t.Errorf("a refused %s reached the store", tool)
					}
					return
				}
				if tool == chatlet.DeleteMessageTool &&
					(len(f.moderations) != 1 || f.moderations[0].Moderate != c.moderate) {
					t.Errorf("%s's delete reached the store as %+v, want moderation %v",
						c.name, f.moderations, c.moderate)
				}
			})
		}
	}
}

// A ROOM'S ARCHIVE IS DECIDED ON THE STORED ROOM, and only an archive is.
//
// Who may close a room to everybody in it is its creator, the lead of the unit
// a unit's room is recorded under, or admin — and both halves of that are on
// the row, never in the arguments, so the tool reads the room and asks the
// table's archive verb about it, and the store is handed the decision with the
// unit it was taken on as a precondition. A topic or a purpose is any member's
// and reads nothing.
//
// Mutation: drop the archive ask from update_channel and the colleague's
// archive reaches the store; pass an empty ArchiveUnit and the unit's lead's
// archive carries no precondition.
func TestARoomsArchiveIsDecidedOnTheStoredRoom(t *testing.T) {
	t.Parallel()
	unitRoom := chatlet.Channel{ID: chatRoomID, Name: "sre", Kind: chatlet.KindUnit,
		Unit: "sre", CreatedBy: "agent-ceo"}
	named := chatlet.Channel{ID: chatRoomID, Name: chatRoomName, Kind: chatlet.KindPublic,
		CreatedBy: "jane"}
	for _, c := range []struct {
		name     string
		person   context.Context
		room     chatlet.Channel
		admitted bool
	}{
		{"its creator", personBoundTo("jane", iam.GrantChatlet), named, true},
		{"a colleague", personBoundTo("sam", iam.GrantChatlet), named, false},
		{"its unit's lead", personBoundTo("cto", iam.GrantChatlet), unitRoom, true},
		{"admin", personBoundTo("sam", iam.GrantAdmin), named, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeChat()
			f.room = c.room
			catalogue := operatorChat(t, f, nil, nil)
			got := callOperatorAs(c.person, t, catalogue[chatlet.UpdateChannelTool],
				map[string]any{"channel": chatRoomID, "archived": true})
			if got.Failed == c.admitted {
				t.Fatalf("%s's archive answered %+v, want admitted %v", c.name, got,
					c.admitted)
			}
			if !c.admitted {
				if len(f.patches) != 0 {
					t.Errorf("a refused archive reached the store: %+v", f.patches)
				}
				return
			}
			want := chatlet.ChannelAuthority{Archive: true, ArchiveUnit: c.room.Unit}
			if len(f.authorities) != 1 || f.authorities[0] != want {
				t.Errorf("the archive reached the store under %+v, want %+v",
					f.authorities, want)
			}
		})
	}

	// AND A TOPIC READS NOTHING AND CARRIES NO ARCHIVE.
	f := newFakeChat()
	got := callOperatorAs(personBoundTo("sam", iam.GrantChatlet), t,
		operatorChat(t, f, nil, nil)[chatlet.UpdateChannelTool],
		map[string]any{"channel": chatRoomID, "topic": "the launch"})
	if got.Failed || f.reads != 0 || len(f.authorities) != 1 ||
		f.authorities[0] != (chatlet.ChannelAuthority{}) {
		t.Errorf("a colleague's topic answered %+v after %d reads under %+v", got,
			f.reads, f.authorities)
	}
}
