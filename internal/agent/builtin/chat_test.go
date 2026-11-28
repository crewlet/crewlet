package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

// The room every case here addresses: a uuid, which the tools take without a
// lookup, and a name for the cases that exercise name resolution.
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
	// order, so a write that waited for the wrong position, or a read that
	// waited at all, is visible.
	awaited []uint64

	// levels is every read level these tools asked for, so a tool that
	// stopped naming one is visible.
	levels []statelog.ReadLevel

	// reads counts every call that reached the read side, which is what
	// "no read awaits" and "an id costs no lookup" are asserted against.
	reads int

	writeErr error
	readErr  error

	// postErr, where set, refuses only a post or a reply: the half of a
	// direct message after its conversation opened.
	postErr error

	// opened, where set, is the outcome a direct conversation's open
	// answers; an unvouched one answers no room, as the store's does.
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

	// room is the stored row [fakeChat.Channel] answers (its creator and
	// unit decide a room's archive); author wrote the message a Thread read
	// asks about.
	room   chatlet.Channel
	author string

	// threads is every thread read, so the read an edit or a delete makes
	// to learn whose words they are is visible.
	threads []chatlet.ThreadQuery

	// What a read answers beyond the bare room: a room read's transcript, a
	// thread read's root and replies, and a listing's last message.
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
	expected    []*[]chatlet.Member
	added       [][]string
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
	// Counted above the cursor handed, as the store counts: seven messages
	// at positions 1-7, so a reader through four has three unread and one
	// who opened nothing seven. Ignoring the cursor would hold no case to it.
	return chatlet.ChannelListing{
		Served: chatlet.Served{Complete: true},
		Channels: []chatlet.ChannelSummary{{
			Channel: f.room,
			Unread:  int(max(chatRoomMessages-q.Marks.Cursors[chatRoomID], 0)),
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
	members []chatlet.Member, expected *[]chatlet.Member) (chatlet.Written, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.memberSets = append(f.memberSets, members)
	f.expected = append(f.expected, expected)
	f.actors = append(f.actors, actor)
	return f.wrote(43), nil
}

func (f *fakeChat) AddMembers(_ context.Context, actor chatlet.Actor, _ string,
	handles []string) (chatlet.Written, []string, error) {

	if f.writeErr != nil {
		return chatlet.Written{}, nil, f.writeErr
	}
	f.keys = append(f.keys, actor.OpKey)
	f.added = append(f.added, handles)
	f.actors = append(f.actors, actor)
	return f.wrote(44), handles, nil
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
		// The room's settings as they stand: a settings write answers them whole.
		Channel: chatlet.Channel{ID: chatRoomID, Name: chatRoomName,
			Topic: f.room.Topic, Purpose: f.room.Purpose},
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

// callChat runs one chat tool for a turn the caller pins, since the cases turn
// on WHICH turn called, acting as the turn's principal: a seat holding every
// app.
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

// ONLY SAYING SOMETHING IS THE ANSWER. A turn woken because somebody spoke to
// it answers with a post in the thread, the room or a DM, and the gate must
// count that or correct the turn for doing what it was woken to do. Were a
// reaction or a join to count, a thumb would discharge a question nobody
// answered.
//
// Mutation: drop the chat arm of Register's delivery switch and the three go
// red; put the reaction in it and the reaction does.
func TestOnlyTheThreePostingToolsCountAsChatDeliveries(t *testing.T) {
	t.Parallel()
	reg := chatRegistry(t, chatDeps(newFakeChat()))
	deliveries := reg.Deliveries()

	for _, name := range builtin.ChatWrites() {
		// On chat's own source: the gate compares a delivery's surface with
		// the inbound wake's source, so a second spelling makes every
		// Chatlet obligation unreachable and the check silently falls back
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

// A TURN MAY SAY TWO THINGS, and a re-run of it must say each once.
//
// Chatlet's idempotency rests on each post's key: derived from the turn's unit
// of work, not its run, so a redelivered trigger derives the same messages; and
// from the call log's repeat count, kept by the surface, so a second remark is
// never the first one's retry. Hence the real surface here.
//
// Mutation: drop the repeat count from callOpKey's identity and the third
// post's key equals the first's.
func TestATurnThatPostsTwiceWritesTwoMessagesAndARerunWritesOne(t *testing.T) {
	t.Parallel()
	calls := []struct {
		tool string
		args map[string]any
	}{
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "asks": false, "body": "on it"}},
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "asks": false, "body": "opened ENG-14"}},
		{builtin.ReplyInThreadTool, map[string]any{
			"channel": chatRoomID, "reply_to": "m1", "asks": false, "body": "and here is why",
		}},
		// THE SAME WORDS AGAIN, AFTER A DIFFERENT POST: a third message.
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "asks": false, "body": "on it"}},
		// AND AGAIN WITH NOTHING BETWEEN: the same call twice, one message.
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "asks": false, "body": "on it"}},
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
// second one's count. Counted per tool, the second join would carry the first
// one's key and get its success while the seat stayed outside. A call outside
// the family counts for nothing; the same join with nothing between is the
// same operation.
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
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "asks": false, "body": "hi"}},
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
	// The membership writes: join, the same join, leave, join, leave.
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
// Every id the store derives from the key carries its instant, and the state
// log reads it to decide whether this node's ledger can vouch for a retry: a
// key stamped with the call's clock is published again by a re-run after the
// ledger's sweep. A turn whose work began before the ledger remembers is
// rebased (turnctx.Turn.RebasedTo), and that must reach these ids too, or its
// every message is answered `unknown` and lost.
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
			// A post, a reply, a DM's open and post, a reaction, a join, a leave.
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
// the transport carried on the actor (the act transport's request key). A
// caller that names none (an operator's assistant over MCP) writes under a
// fresh operation each time: nothing redelivers its call, and a stable key
// would collapse every later post into the first.
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
// An open can answer `unknown`: a lost acknowledgement, or a node whose ledger
// cannot vouch for it, naming no room. Posting on would hit room "" or a room
// this node has not applied, refused as "not found" for a conversation that
// may exist. So the tool stops: no post, the unknown answer, saying the
// message was not sent and to make the same call again.
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
				map[string]any{"to": []any{"agent-ceo"}, "asks": false, "body": "a word"})
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
// A receipt regardless would hand an `unknown` post a `message_id` a seat then
// reports as said; and an operation the ledger cannot vouch for is answered
// `unknown` unpublished and names no room, so a join would report `joined`
// into nothing. Each write answers the unknown result: a post with how to
// repeat it as the same message and what a reworded one costs; a statement (a
// reaction, a join, a leave) offers its repeat as harmless.
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
		map[string]any{"channel": chatRoomID, "asks": false, "body": "on it"})
	if got.Failed || !strings.Contains(got.Output, `"outcome": "applied"`) ||
		!strings.Contains(got.Output, `"position": "CREWLET_CHAT_LOG@`) {
		t.Errorf("a post that landed answered %+v, want its outcome and position", got)
	}
}

// seatChatWrites are a seat's chat writes, each with the sentence its unknown
// answer offers a turn as the repeat.
//
// A post's repeat is the same call, before its tool is called with anything
// else, naming no other tool since no posting tool is in a family
// (TestNoPostingToolIsCountedAmongAFamily). A statement's is harmless.
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
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "asks": false, "body": "on it"},
			sameCall(builtin.PostMessageTool)},
		{builtin.ReplyInThreadTool,
			map[string]any{"channel": chatRoomID, "reply_to": "m1", "asks": false, "body": "ok"},
			sameCall(builtin.ReplyInThreadTool)},
		{builtin.SendDMTool, map[string]any{"to": []any{"agent-ceo"}, "asks": false, "body": "a word"},
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
// The router answers [estate.ErrOutcomeUnknown] for a write no data node
// answered. A keyed one (a turn's call, a person's request) is asked of every
// data node first, and the same call is its safe retry; an unkeyed one (an
// operator's assistant over MCP) is reported at the first silent node, and its
// repeat is a second write. On a node without data every chat write is remote,
// so this is the common path.
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
		map[string]any{"channel": chatRoomID, "asks": false, "body": "on it"})
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
// A family's repeat count (chatlet.CallFamily) is raised by every call to
// another tool of it, whatever its arguments, so were a post in one, its
// "before calling it with any others" would let that tool be called between,
// making the repeat a new operation.
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
// The arms say opposite things to a model: "refused that" it can fix and call
// again; the fall-through says the write may have landed and must not be
// reported done. The sentence is the store's own, without the Go package's
// prefix.
func TestABodyOverTheCapIsRefusedNamingTheField(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	f.writeErr = chatlet.NewMessage{Body: strings.Repeat("x", chatlet.MaxBody+1)}.Validate()
	if f.writeErr == nil {
		t.Fatal("a body over the cap validates, so this case holds nothing")
	}
	got := callChat(t, chatRegistry(t, chatDeps(f)), builtin.PostMessageTool,
		turnFor(t, "agent-cto"), map[string]any{"channel": chatRoomID, "asks": false, "body": "too long"})
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
// refuses more than [chatlet.MaxLinks], and the parameter's description is the
// one place a model learns that before meeting it as a refusal.
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

// EVERY CHAT TOOL THAT TAKES LINKS TELLS A MODEL WHAT A LINK IS: an absolute
// address in one of [chatlet.LinkSchemes], which the store refuses otherwise.
// Every tool a person's surface serves is walked, so a tool that takes links
// later is held to it too.
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

// THE AUTHOR IS THE TURN'S SEAT AND NEVER AN ARGUMENT: a model naming its own
// author could speak as anybody, and the name beside the words is all a reader
// has. An argument that tries is refused, since no chat tool declares one.
func TestAPostIsAttributedToTheTurnsSeatAndNotToAnArgument(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")
	for _, arg := range []string{"author", "handle", "actor", "author_kind"} {
		got := callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
			"channel": chatRoomID, "asks": false, "body": "shipping tomorrow", arg: "agent-ceo",
		})
		if !got.Failed || len(f.posts) != 0 {
			t.Fatalf("a post naming its own %s answered %+v and reached the "+
				"store %d times", arg, got, len(f.posts))
		}
	}
	callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": chatRoomID, "asks": false, "body": "shipping tomorrow",
	})
	if len(f.posts) != 1 {
		t.Fatalf("%d messages reached the store", len(f.posts))
	}
	actor := f.posts[0].actor
	if actor.Handle != "agent-cto" || actor.Kind != chatlet.AuthorAgent {
		t.Errorf("the message was written as %q (%s): an argument moved the author",
			actor.Handle, actor.Kind)
	}
	// THE TURN ID IS THE RUN (provenance); the KEY is the work's, since a
	// run id is fresh per attempt and would make a redelivery a second copy.
	if actor.TurnID != "run-1" {
		t.Errorf("the message names run %q, want the run that said it", actor.TurnID)
	}
	if actor.OpKey == "" {
		t.Error("a seat's message carries no key, so a re-run would post it twice")
	}
}

// EVERY WRITE WAITS FOR ITS OWN POSITION, AND NO READ WAITS AT ALL.
//
// A read is answered from whichever copy serves it, so a seat that posts then
// reads the room must see its own message, or it says it again. The position
// handed to [builtin.ChatDeps.Await] must be THIS write's, or the next read can
// miss it. A read, a gesture's deciding reads included, has no position of its
// own to wait for.
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
		{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "asks": false, "body": "a"}, 21},
		{builtin.ReplyInThreadTool,
			map[string]any{"channel": chatRoomID, "reply_to": "m1", "asks": false, "body": "b"}, 22},
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

	// A DIRECT MESSAGE WAITS TWICE: the post decides on whichever copy takes
	// it, so one sent before the conversation's create applied there finds
	// no room.
	before := len(f.awaited)
	if got := callChat(t, reg, builtin.SendDMTool, turn, map[string]any{
		"to": []any{"agent-ceo"}, "asks": false, "body": "between us",
	}); got.Failed {
		t.Fatalf("send_dm failed: %s", got.Output)
	}
	if waits := f.awaited[before:]; len(waits) != 2 || waits[0] != 11 {
		t.Errorf("send_dm waited %v, want the conversation's create (11) and "+
			"then its own message", waits)
	}

	// AND A PERSON'S GESTURES, those decided after a read included.
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

// EVERY READ ASKS FOR THE SEAT'S OWN LEVEL: a seat cannot notice a stale
// answer, and one reading a room without its own last message posts it again.
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
// The sub-agent guard asks [mcp.WritesToSharedSurface]: is a second party
// written FOR, not does a row change. A worker posting under its parent's name
// would answer in somebody else's voice unseen; a join changes only where its
// parent listens.
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
// That arm fails closed (a public write), so a tool falling through is safe
// but wrong unseen: the gate counts a read as a possible delivery, and a
// private-state write is withheld from a worker its parent granted it. The
// table is the assertion; a dropped arm changes its values.
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

		// THE PERSON'S GESTURES, by the same switch: the act transport
		// refuses a proven read, and an assistant is told which destroys
		// what somebody else wrote.
		chatlet.CreateChannelTool:     shared,
		chatlet.OpenDMTool:            stated,
		chatlet.UpdateChannelTool:     restated,
		chatlet.SetChannelMembersTool: restated,
		chatlet.AddChannelMembersTool: stated,
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
// read, joined or not: a model types a room's name, never its uuid, so an
// id-only tool would refuse every turn that had not just listed the rooms.
func TestAChannelNameResolvesAsTheRoomsAddress(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")

	for _, ref := range []string{chatRoomName, "#" + chatRoomName, "ENG"} {
		before := len(f.posts)
		if got := callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
			"channel": ref, "asks": false, "body": "hello",
		}); got.Failed {
			t.Fatalf("a post to %q was refused: %s", ref, got.Output)
		}
		if f.posts[before].channelID != chatRoomID {
			t.Errorf("%q resolved to %q, want %q", ref, f.posts[before].channelID, chatRoomID)
		}
	}
	// AS THE SEAT: a private room's name resolves only for its members, so a
	// lookup as anybody else names rooms the seat cannot read.
	for _, viewer := range f.viewers {
		if viewer != "agent-cto" {
			t.Errorf("a name was resolved for %q, want the seat it is said as", viewer)
		}
	}

	// AND AN ID COSTS NO LOOKUP: the common case is the wake's room id, and
	// resolving it would put a read on the hot path of every answer.
	f.reads = 0
	callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": chatRoomID, "asks": false, "body": "hello",
	})
	if f.reads != 0 {
		t.Errorf("a post addressed by id reached the reader %d times", f.reads)
	}

	// AND A ROOM THE STORE DOES NOT HOLD NAMES BOTH ADDRESSES, so the seat is
	// never sent after an id it did not need.
	f.writeErr = fmt.Errorf("%w: room %s", chatlet.ErrNotFound, chatRoomID)
	got := callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": chatRoomID, "asks": false, "body": "hello",
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
// A private room the caller is not in answers exactly as a name nobody holds,
// so "not found" would claim more than the lookup knows. A failed lookup says
// nothing about the name, and told "invalid" a person would retype a right one.
func TestARoomNameThatResolvesToNothingIsTheArgumentsToFix(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	got := callChat(t, chatRegistry(t, chatDeps(f)), builtin.PostMessageTool,
		turnFor(t, "agent-cto"), map[string]any{"channel": "marketing", "asks": false, "body": "hi"})
	if tools.RefusalOf(got) != tools.RefusalInvalid ||
		!strings.Contains(got.Output, builtin.ListChannelsTool) {
		t.Errorf("a name nothing matches answered %+v, want the argument class "+
			"saying how to find the room", got)
	}

	f = newFakeChat()
	f.readErr = &statelog.Unavailable{Reason: statelog.ReasonDeferred}
	got = callChat(t, chatRegistry(t, chatDeps(f)), builtin.PostMessageTool,
		turnFor(t, "agent-cto"), map[string]any{"channel": "eng", "asks": false, "body": "hi"})
	if tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Errorf("a name lookup that failed answered %+v, want the node's class", got)
	}
	if len(f.posts) != 0 {
		t.Errorf("a post whose room could not be resolved reached the store: %v", f.posts)
	}

	// AND A SURFACE WITH NO READER RESOLVES NO NAME: only an id works there,
	// so it is the argument's class again.
	f = newFakeChat()
	got = callChat(t, chatRegistry(t, builtin.ChatDeps{Writer: f}), builtin.PostMessageTool,
		turnFor(t, "agent-cto"), map[string]any{"channel": "eng", "asks": false, "body": "hi"})
	if tools.RefusalOf(got) != tools.RefusalInvalid ||
		!strings.Contains(got.Output, "by its id") {
		t.Errorf("a name on a surface with no reader answered %+v", got)
	}
}

// ADDRESSING THE WHOLE ROOM IS DERIVED FROM THE WORDS: a woken seat's prompt
// says `@channel` addresses a room, so a model writes that, and one that woke
// nobody reads to the room as though everybody had seen it.
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
			map[string]any{"channel": chatRoomID, "asks": false, "body": c.body})
		if got := f.posts[before].in.Collective; got != c.want {
			t.Errorf("%q addressed the room = %v, want %v", c.body, got, c.want)
		}
	}
}

// AN UNREAD COUNT IS A PERSON'S, and a seat has none: reported anyway it is the
// room's whole tail, and a model goes off to catch up on somebody's backlog.
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

// A PERSON'S COUNTS ARE TAKEN AGAINST THEIR OWN CURSORS: from zero, every room
// reads wholly unread to somebody who read most of it. The read state is asked
// for the seat the call acts as, the one `mark_chat_read` wrote it under.
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
	if got := f.listed[len(f.listed)-1].Marks.Cursors; got[chatRoomID] != 4 {
		t.Errorf("the listing was counted against cursors %v, want the person's", got)
	}
	if !slices.Equal(*asked, []string{"jane"}) {
		t.Errorf("the read state was asked for %v, want the seat the call acts "+
			"as (jane)", *asked)
	}

	// NO CURSORS IS SOMEBODY WHO OPENED NOTHING: every room's tail is new.
	tool, _ = readStateChat(t, newFakeChat(), coord.ChatReadState{}, nil)
	_, rows = listing(t, callOperator(t, tool, nil))
	if unread, _ := rows[0]["unread"].(float64); unread != chatRoomMessages {
		t.Errorf("a person who has opened nothing is told %v are new, want %d",
			rows[0]["unread"], chatRoomMessages)
	}
}

// A ROOM THE PERSON MUTED, AND A DO-NOT-DISTURB THEY ARE IN, ARE SAID AS THE
// RAIL SAYS THEM: a person's assistant reads rooms through this tool, and would
// offer to work through a muted room's backlog the person silenced.
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

	// AND AN UNMUTED ROOM, AFTER DO-NOT-DISTURB ENDED, SAYS NEITHER.
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

// A READ STATE THAT COULD NOT BE READ IS NOT A CURSOR AT ZERO, which would
// call every room wholly unread. The listing is served with no count on any
// row, and says why under `read_state` (not `unread`, every row's number), in
// fixed words, never the store's error, which names its transport.
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
// shown a tool that always fails distrusts the whole catalogue. A gesture
// decided after a read (whose message, who made a room) needs the reader too.
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

// A CALLER THE ENGINE CANNOT RESOLVE TO A SEAT GETS NOTHING, READS INCLUDED,
// and the refusal carries the remedy.
//
// A caller with no seat has no membership, and serving it "everything public"
// would invent a viewer the company never declared. Two shapes: the actor seam
// answering nobody, and a credential acting as itself (the table refuses it
// first, `unseated`; this arm holds a surface wired without the table). The
// class is forbidden, whose sentence a tool tells as written, carrying the
// remedy.
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

// A READ THAT COULD NOT BE SERVED IS NEVER AN EMPTY ROOM: a seat told nobody
// answered asks again.
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
// A person's surface acts on the class (the act transport's status, the
// sentence, whether a retry helps), so an unreadable node, a lost race and a
// taken name must each say something different. Each refusal is shaped as the
// store builds it and reaches the tool through its writer or reader, so a call
// site that bypasses the classifier is caught.
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

	// AND THE GESTURES ONLY A PERSON MAKES, through the operator catalogue:
	// the same sentinels, and a screen acting on the class as a model does.
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
			// AN UNSERVED READ IS NEVER NOT-FOUND, or the room renders gone.
			tools.RefusalUnavailable: &statelog.Unavailable{Reason: statelog.ReasonDeferred},
			tools.RefusalInternalError: errors.New("open /var/lib/crewlet/replicated.db: " +
				"disk I/O error"),
			tools.RefusalNotFound: fmt.Errorf("%w: room room-1", chatlet.ErrNotFound),
			// AN ARGUMENT THE READER REFUSED (a cursor that is no position)
			// is the caller's to fix: a retry is refused every time.
			tools.RefusalInvalid: chatlet.ChannelPatch{}.Validate(),
			// A READER WITH NO SEAT is refused naming the binding it lacks.
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

// operatorChatWrites are the person-only gestures that append to the log, with
// arguments that reach the writer: all but mark_chat_read, which writes the
// coordination store.
var operatorChatWrites = map[string]map[string]any{
	chatlet.CreateChannelTool: {"channel": "launch"},
	chatlet.OpenDMTool:        {"to": []any{"bob"}},
	chatlet.UpdateChannelTool: {"channel": chatRoomID, "topic": "the launch"},
	chatlet.SetChannelMembersTool: {
		"channel": chatRoomID, "members": []any{"jane"},
	},
	chatlet.AddChannelMembersTool: {
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
// The open and the post are appended in turn, so the call is as certain as
// the LESS certain and a reader waits for the LATER position, as every
// two-append write answers. A post refused after the open landed says the
// conversation is there ("nothing was written" would be false), with the
// post's class.
func TestADirectMessageAnswersForBothItsRecords(t *testing.T) {
	t.Parallel()
	dm := func(f *fakeChat, body string) tools.Result {
		return callChat(t, chatRegistry(t, chatDeps(f)), builtin.SendDMTool,
			turnFor(t, "agent-cto"), map[string]any{"to": []any{"agent-ceo"}, "asks": false, "body": body})
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

	// THE STORE'S OWN FIELD REFUSAL, told without its Go prefix.
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

	// A POST THE NODE COULD NOT MAKE AFTER THE OPEN is the node's class, and
	// still says the conversation is open, never that nothing was written.
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
	// the router does not repeat an unkeyed post, so it may have been said.
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

	// A POST OF UNKNOWN OUTCOME AFTER THE OPEN says the open landed: only
	// the second record is in doubt.
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

	// AND WHAT THE MESSAGE SAYS IS REFUSED BEFORE ANYTHING IS OPENED, or a
	// refused body leaves a room in every participant's list.
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
// never reaching the index, whose failure a model reads as chat unreadable. At
// the bound it is a search.
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
// The viewer decides which rooms are searched, so it is the calling seat,
// never an argument. A `channel` resolves as every tool's; dropping one that
// names nothing would return hits from rooms the caller did not mean.
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
	if got := f.searched[len(f.searched)-1]; !reflect.DeepEqual(got, want) {
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

// A CHAT SEARCH'S HITS ARE KEYED AS EVERY OTHER CHAT ANSWER IS: the index's
// value has no JSON names, and whole it would key `MessageID` beside the
// `message_id` the transcript, receipts and reply_in_thread use.
//
// Mutation: answer `answer.Hits` whole and every key here goes missing.
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

// A SEARCH OF AN INDEX STILL BEING BUILT IS NEVER "NOTHING MATCHED": a seat
// told nobody said it acts on that. An empty answer says the index is catching
// up, and hits say they may not be all.
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

// A FULL PAGE OF CHAT HITS IS ONE TOOL ANSWER, of prose or of markup.
//
// [search.MaxChatSearchLimit] hits of [search.ChatSnippetBytes] must fit in
// [builtin.ToolAnswerBytes], or the search refuses its own largest page. Held
// at a heavy page: snippets cut at the bound with both marks, fenced, each by
// a different longest handle, full of quotes JSON doubles. Markup travels as
// its bytes, not an HTML-safe encoder's six-byte escapes. A page JSON still
// widens (control characters, six bytes each) is refused naming `limit`, never
// cut, holding the limit's doc true both ways.
//
// Mutation: raise MaxChatSearchLimit to 45, restore HTML escaping, or drop the
// narrowing advice, and the matching half goes red.
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
// The tools name a message by id, so the table cannot form the object from
// the arguments: an admin is admitted on the UNREAD message (moderation,
// without showing it), anybody else reads it and is decided again on its
// author. The decision reaches the store as [chatlet.MessageAuthority.Moderate],
// never re-derived there.
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
// A room's creator, its unit's lead, or admin may archive it; both are on the
// row, not the arguments, so the tool reads the room, asks the table's archive
// verb, and hands the store the decision with its unit as a precondition. A
// topic or a purpose is any member's and reads nothing.
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

// AN AGENT STATES WHETHER ITS MESSAGE ASKS (ADR-0113): a post without `asks` is
// refused unwritten, and the statement reaches the message, so a request
// routes as one and a note as news.
//
// Mutation: default a missing `asks` to false, and a delegation written
// without it reaches its colleague as something that asks nothing.
func TestAnAgentMustSayWhetherItsMessageAsks(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	reg := chatRegistry(t, chatDeps(f))
	got := callChat(t, reg, builtin.PostMessageTool, turnFor(t, "agent-cto"),
		map[string]any{"channel": chatRoomID, "body": "@ops please roll back staging"})
	if !got.Failed || !strings.Contains(got.Output, "needs `asks`") || len(f.posts) != 0 {
		t.Fatalf("a post without asks answered %q and wrote %d messages, want it refused",
			got.Output, len(f.posts))
	}
	for _, asks := range []bool{true, false} {
		got = callChat(t, reg, builtin.PostMessageTool, turnFor(t, "agent-cto"),
			map[string]any{"channel": chatRoomID, "body": "@ops please roll back staging", "asks": asks})
		if got.Failed {
			t.Fatalf("a post with asks=%v failed: %s", asks, got.Output)
		}
		if said := f.posts[len(f.posts)-1].in; said.Asks != asks {
			t.Errorf("a post with asks=%v reached the store asking %v", asks, said.Asks)
		}
	}
}
