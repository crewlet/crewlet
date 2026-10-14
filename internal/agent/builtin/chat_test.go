package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
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
)

// posted is one message the fake took, with the key that decides its id.
type postedMessage struct {
	channelID string
	replyTo   string
	actor     chat.Actor
	in        chat.NewMessage
}

type fakeChat struct {
	posts     []postedMessage
	reactions []string
	joined    []string
	left      []string

	// keys is the key every gesture other than a post arrived with, in
	// order — a post's is on its message.
	keys []statelog.CallKey

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

	searched []builtin.ChatSearch
	viewers  []string

	// listed is every listing query, so a case can see which cursors the
	// unread counts were taken against.
	listed []chat.ChannelsQuery

	// What the person's gestures handed the store, in order.
	created    []chat.NewChannel
	patches    []chat.ChannelPatch
	memberSets [][]chat.Member
	edits      []chat.EditMessage
	deleted    []string
	actors     []chat.Actor
}

func newFakeChat() *fakeChat { return &fakeChat{} }

func (f *fakeChat) Channels(_ context.Context, viewer string, q chat.ChannelsQuery,
	fresh statelog.Freshness) (chat.ChannelListing, error) {

	f.levels = append(f.levels, fresh.Level)
	f.viewers = append(f.viewers, viewer)
	f.listed = append(f.listed, q)
	f.reads++
	if f.readErr != nil {
		return chat.ChannelListing{}, f.readErr
	}
	// COUNTED ABOVE THE CURSOR IT WAS HANDED, as the store counts: the room
	// holds seven messages at positions one to seven, so a reader who has
	// read through four has three new and one who has opened nothing has
	// seven. A count that ignored the cursor would hold no case here to it.
	return chat.ChannelListing{
		Channels: []chat.ChannelSummary{{
			Channel: chat.Channel{ID: chatRoomID, Name: chatRoomName, Kind: chat.KindPublic},
			Unread:  int(max(chatRoomMessages-q.Cursors[chatRoomID], 0)),
		}},
	}, nil
}

// chatRoomMessages is how many messages the fake's one room holds.
const chatRoomMessages = 7

// ChannelNamed resolves the one room this fake holds by its name, for any
// viewer, as the store resolves a public room's address.
func (f *fakeChat) ChannelNamed(_ context.Context, viewer, name string,
	fresh statelog.Freshness) (chat.ChannelDetail, error) {

	f.levels = append(f.levels, fresh.Level)
	f.viewers = append(f.viewers, viewer)
	f.reads++
	if f.readErr != nil {
		return chat.ChannelDetail{}, f.readErr
	}
	if chat.NormalizeName(name) != chatRoomName {
		return chat.ChannelDetail{}, fmt.Errorf("%w: no room is called %q",
			chat.ErrNotFound, name)
	}
	return chat.ChannelDetail{Channel: chat.Channel{
		ID: chatRoomID, Name: chatRoomName, Kind: chat.KindPublic,
	}}, nil
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
	if f.postErr != nil {
		return chat.Written{}, f.postErr
	}
	f.posts = append(f.posts, postedMessage{channelID: channelID, actor: actor, in: in})
	return f.wrote(20 + uint64(len(f.posts))), nil
}

func (f *fakeChat) Reply(_ context.Context, actor chat.Actor, channelID, replyTo string,
	in chat.NewMessage) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	if f.postErr != nil {
		return chat.Written{}, f.postErr
	}
	f.posts = append(f.posts, postedMessage{
		channelID: channelID, replyTo: replyTo, actor: actor, in: in,
	})
	return f.wrote(20 + uint64(len(f.posts))), nil
}

func (f *fakeChat) OpenDirect(_ context.Context, _ chat.Actor, participants []string,
	key statelog.CallKey) (chat.Written, bool, error) {

	if f.writeErr != nil {
		return chat.Written{}, false, f.writeErr
	}
	f.keys = append(f.keys, key)
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
	if f.opened != nil {
		room.Outcome = *f.opened
		if f.opened.Unvouched {
			return chat.Written{ChangeID: f.opened.OpID, Outcome: *f.opened}, false, nil
		}
	}
	return room, true, nil
}

func (f *fakeChat) React(_ context.Context, _ chat.Actor, _, messageID, emoji string,
	key statelog.CallKey) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.keys = append(f.keys, key)
	f.reactions = append(f.reactions, messageID+emoji)
	return f.wrote(31), nil
}

func (f *fakeChat) Unreact(_ context.Context, _ chat.Actor, _, messageID, emoji string,
	key statelog.CallKey) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.keys = append(f.keys, key)
	f.reactions = append(f.reactions, "-"+messageID+emoji)
	return f.wrote(32), nil
}

func (f *fakeChat) Join(_ context.Context, _ chat.Actor, channelID string,
	key statelog.CallKey) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.keys = append(f.keys, key)
	f.joined = append(f.joined, channelID)
	return f.wrote(33), nil
}

func (f *fakeChat) Leave(_ context.Context, _ chat.Actor, channelID string,
	key statelog.CallKey) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.keys = append(f.keys, key)
	f.left = append(f.left, channelID)
	return f.wrote(34), nil
}

func (f *fakeChat) CreateChannel(_ context.Context, actor chat.Actor,
	in chat.NewChannel) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.keys = append(f.keys, in.CallKey)
	f.created = append(f.created, in)
	f.actors = append(f.actors, actor)
	out := f.wrote(41)
	out.Channel = chat.Channel{ID: chatRoomID, Name: in.Name, Kind: chat.KindPublic}
	if f.outcome != nil && f.outcome.Unvouched {
		out.Channel = chat.Channel{}
	}
	return out, nil
}

func (f *fakeChat) PatchChannel(_ context.Context, actor chat.Actor, _ string,
	patch chat.ChannelPatch, key statelog.CallKey) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.keys = append(f.keys, key)
	f.patches = append(f.patches, patch)
	f.actors = append(f.actors, actor)
	return f.wrote(42), nil
}

func (f *fakeChat) SetMembers(_ context.Context, actor chat.Actor, _ string,
	members []chat.Member, key statelog.CallKey) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.keys = append(f.keys, key)
	f.memberSets = append(f.memberSets, members)
	f.actors = append(f.actors, actor)
	return f.wrote(43), nil
}

func (f *fakeChat) Edit(_ context.Context, actor chat.Actor, _, _ string,
	in chat.EditMessage) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.keys = append(f.keys, in.CallKey)
	f.edits = append(f.edits, in)
	f.actors = append(f.actors, actor)
	return f.wrote(44), nil
}

func (f *fakeChat) Delete(_ context.Context, actor chat.Actor, _, messageID string,
	key statelog.CallKey) (chat.Written, error) {

	if f.writeErr != nil {
		return chat.Written{}, f.writeErr
	}
	f.keys = append(f.keys, key)
	f.deleted = append(f.deleted, messageID)
	f.actors = append(f.actors, actor)
	return f.wrote(45), nil
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
	if f.outcome != nil {
		if f.outcome.Unvouched {
			return chat.Written{ChangeID: f.outcome.OpID, Outcome: *f.outcome}
		}
		return chat.Written{
			Channel:  chat.Channel{ID: chatRoomID, Name: chatRoomName},
			ChangeID: f.outcome.OpID, Outcome: *f.outcome,
		}
	}
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
// The whole of native chat's idempotency rests on the key every post hands
// the store: its SEED is the turn's unit of work rather than its run, so a
// redelivered trigger — which is ordinary — derives the same messages instead
// of a second copy of the conversation; and its REPEAT is the run's own call
// log's count, kept by the surface every call goes through, so a turn's second
// remark is never its first one's retry. That count was once read from a
// record nothing in production ever filled, so every post of a turn was call
// zero and a seat's second message in one room was dropped as a duplicate of
// its first. These calls go through a real surface for that reason.
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
	run := func(runID string) []statelog.CallKey {
		f := newFakeChat()
		surface := chatSurface(t, f, runID)
		for _, c := range calls {
			executeWork(t, surface, c.tool, c.args)
		}
		keys := make([]statelog.CallKey, 0, len(f.posts))
		for _, p := range f.posts {
			keys = append(keys, p.in.CallKey)
		}
		return keys
	}
	first := run("run-1")
	if len(first) != len(calls) {
		t.Fatalf("%d calls reached the store with %d keys", len(calls), len(first))
	}
	// THE COUNTS THE RUN'S LOG HANDS EACH CALL: how many earlier calls to
	// the same tool asked for something else. Two different posts are
	// already two messages by their words; what the count adds is that the
	// SAME words after a different post (call 3) are not call 0's retry,
	// while the same call asked again with nothing between (call 4) is
	// call 3's.
	//
	// AND THE ARGUMENTS THE COUNT COMPARED, as a digest: the count tells
	// calls apart by what the model typed, so the identity has to as well,
	// or two calls typed differently that name one room could share both.
	repeats := []int{0, 1, 0, 1, 1}
	for i, key := range first {
		if key.Seed != "wk-1" || key.Tool != calls[i].tool || key.Repeat != repeats[i] ||
			key.Args != turnctx.ArgsDigest(calls[i].args) {
			t.Fatalf("call %d carried key %#v, want the turn's work key, the "+
				"tool %s, repeat %d and its arguments' digest — every key was %#v",
				i, key, calls[i].tool, repeats[i], first)
		}
	}
	if first[0] == first[3] {
		t.Errorf("\"on it\" said again after a different post carried the first "+
			"one's key %#v — it would be answered as that post's retry and never "+
			"said", first[0])
	}
	if first[3] != first[4] {
		t.Errorf("the same post asked twice in a row carried %#v and %#v — an "+
			"executor that asks twice would say it twice", first[3], first[4])
	}

	// THE RE-RUN: the same unit of work, a NEW run id, a fresh call log.
	if rerun := run("run-2"); !slices.Equal(rerun, first) {
		t.Errorf("the re-run carried %#v, the first run %#v — a redelivered "+
			"trigger would post the whole conversation again", rerun, first)
	}
}

// JOINING AGAIN AFTER LEAVING IS A NEW OPERATION, not the first join's retry.
//
// join_channel and leave_channel write one membership, so a turn counts them
// as one family (chat.CallFamily): the leave between two joins raises the
// second one's count, where a count kept per tool saw nothing different and
// handed the second join the first one's key — and with it the first one's
// success, while the seat stayed outside the room. A call outside the family
// counts for nothing, and the same join asked again with nothing between is
// the same operation.
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
	// join, leave — each counting the family's calls before it that were
	// not itself: the two joins for the first leave, the leave for the
	// second join, and both joins and that leave for the last.
	repeats := []int{0, 0, 2, 1, 3}
	if len(f.keys) != len(repeats) {
		t.Fatalf("%d membership writes reached the store, want %d", len(f.keys), len(repeats))
	}
	for i, key := range f.keys {
		if key.Repeat != repeats[i] {
			t.Fatalf("membership write %d carried repeat %d, want %d — every key "+
				"was %#v", i, key.Repeat, repeats[i], f.keys)
		}
	}
	if f.keys[0] == f.keys[3] {
		t.Errorf("the join after a leave carried the first join's key %#v — it "+
			"would be answered with that join's success and change nothing", f.keys[0])
	}
}

// chatSurface is a tool surface over the chat tools, bound to a turn with a
// call log of its own — the frame a real turn's calls go through.
func chatSurface(t *testing.T, f *fakeChat, runID string) *tools.Surface {
	t.Helper()
	reg := chatRegistry(t, chatDeps(f))
	turn := turnFor(t, "agent-cto")
	turn.RunID = runID
	turn.Calls = turnctx.NewCallLog()
	return tools.NewSurface("execute", reg.Snapshot(), chat.Tools()).ForTurn(turn)
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
			for _, c := range []struct {
				tool string
				args map[string]any
			}{
				{builtin.PostMessageTool, map[string]any{"channel": chatRoomID, "body": "a"}},
				{builtin.ReplyInThreadTool,
					map[string]any{"channel": chatRoomID, "reply_to": "m1", "body": "b"}},
				{builtin.SendDMTool, map[string]any{"to": []any{"agent-ceo"}, "body": "c"}},
				{builtin.ReactToMessageTool,
					map[string]any{"channel": chatRoomID, "message": "m1", "emoji": ":+1:"}},
				{builtin.JoinChannelTool, map[string]any{"channel": chatRoomID}},
				{builtin.LeaveChannelTool, map[string]any{"channel": chatRoomID}},
			} {
				if got := callChat(t, reg, c.tool, turn, c.args); got.Failed {
					t.Fatalf("%s failed: %s", c.tool, got.Output)
				}
			}
			keys := slices.Clone(f.keys)
			for _, p := range f.posts {
				keys = append(keys, p.in.CallKey)
			}
			// A post, a reply, a direct message's open and its post, a
			// reaction, a join and a leave.
			if len(keys) != 7 {
				t.Fatalf("the writes reached the store with %d keys, want 7", len(keys))
			}
			for i, key := range keys {
				if key.Seed != turn.WorkKey || !key.Since.Equal(want) {
					t.Errorf("chat write %d carries key %#v, want the work key %q "+
						"at %s", i, key, turn.WorkKey, want)
				}
			}
		})
	}
}

// A PERSON'S RETRIED REQUEST IS ONE MESSAGE, and a request's operation writes
// only the call it was derived for.
//
// Outside a turn the operator's act transport names the operation from the
// request a person's client sends again, unchanged, on a retry
// ([builtin.RequestOperation]); every chat write the call makes is keyed on it
// and carries its instant, so the retry is the message the first attempt
// posted. Under an operation the transport derived for ANOTHER call it is
// refused before anything is written. And a caller that names no operation —
// an operator's assistant over MCP — writes under a fresh one every time.
func TestAnOperatorsChatWriteIsKeyedOnItsRequest(t *testing.T) {
	t.Parallel()
	const request = "0192a1b2-c3d4-7e5f-8a6b-000000000042"
	args := map[string]any{"channel": chatRoomID, "body": "standup moved to ten"}
	op, err := builtin.RequestOperation("ops", request, builtin.PostMessageTool, args)
	if err != nil {
		t.Fatalf("RequestOperation: %v", err)
	}
	opOf := func(ctx context.Context) string {
		got, _ := ctx.Value(operationCtx{}).(string)
		return got
	}
	f := newFakeChat()
	deps := chatDeps(f)
	deps.Actor = func(context.Context, *turnctx.Turn) (chat.Actor, error) {
		return chat.Actor{Handle: "jane", Kind: chat.AuthorOperator, OperatorID: "ops"}, nil
	}
	deps.Operation = opOf
	tool := registered(t, builtin.Deps{Chat: deps}, builtin.PostMessageTool)

	for range 2 {
		if got, err := tool.Call(withOperation(t.Context(), op), args); err != nil || got.Failed {
			t.Fatalf("post under the request: %v %s", err, got.Output)
		}
	}
	minted, _ := statelog.OpMintedAt(op)
	if len(f.posts) != 2 || f.posts[0].in.CallKey != f.posts[1].in.CallKey {
		t.Fatalf("one request sent twice reached the store as %+v", f.posts)
	}
	// NO REPEAT COUNT AND NO DIGEST: the operation already names this call,
	// its arguments included, and nothing counted it.
	if key := f.posts[0].in.CallKey; key.Seed != op || !key.Since.Equal(minted) ||
		key.Tool != builtin.PostMessageTool || key.Repeat != 0 || key.Args != "" {
		t.Fatalf("the request's post carried key %#v, want the operation %s at "+
			"its own instant %s and nothing else", key, op, minted)
	}

	other := maps.Clone(args)
	other["body"] = "standup moved to eleven"
	got, err := tool.Call(withOperation(t.Context(), op), other)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !got.Failed || !strings.Contains(got.Output, "derived for another call") {
		t.Errorf("a post under another call's operation answered %+v", got)
	}
	if len(f.posts) != 2 {
		t.Errorf("a post under another call's operation reached the store: %+v",
			f.posts[2:])
	}

	if got, err := tool.Call(t.Context(), args); err != nil || got.Failed {
		t.Fatalf("post with no operation: %v %s", err, got.Output)
	}
	if key := f.posts[2].in.CallKey; key != (statelog.CallKey{}) {
		t.Errorf("a post that named no operation carried key %#v — a stable key "+
			"would collapse every later post into the first", key)
	}
}

// EVERY CHAT WRITE IS REFUSED UNDER AN OPERATION DERIVED FOR ANOTHER CALL, and
// writes nothing.
//
// The operation a transport derives from a request names ONE call — the tool
// and its arguments — and every id the write makes is derived from it: a post
// under another call's operation is a post under the message id that call
// derives, and a join under one is that call's membership change. A check held
// by one tool and not its neighbours is the tool a transport that derived the
// wrong operation writes through, so all six are held here, each under an
// operation derived for the same tool with one more argument than it is given.
func TestEveryChatWriteIsRefusedUnderAnotherCallsOperation(t *testing.T) {
	t.Parallel()
	const request = "0192a1b2-c3d4-7e5f-8a6b-000000000099"
	cases := map[string]map[string]any{
		builtin.PostMessageTool:   {"channel": chatRoomID, "body": "standup moved"},
		builtin.ReplyInThreadTool: {"channel": chatRoomID, "reply_to": "m1", "body": "ok"},
		builtin.SendDMTool:        {"to": []any{"agent-ceo"}, "body": "a word"},
		builtin.ReactToMessageTool: {
			"channel": chatRoomID, "message": "m1", "emoji": ":+1:",
		},
		builtin.JoinChannelTool:  {"channel": chatRoomID},
		builtin.LeaveChannelTool: {"channel": chatRoomID},
	}
	if got := len(cases); got != len(chat.Tools())-3 {
		t.Fatalf("%d chat writes are held here and the catalogue has %d tools "+
			"beside its three reads — a write added to it is held here too",
			got, len(chat.Tools()))
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			derivedFor := maps.Clone(args)
			derivedFor["note"] = "one more argument than this call has"
			op, err := builtin.RequestOperation("ops", request, name, derivedFor)
			if err != nil {
				t.Fatalf("RequestOperation: %v", err)
			}
			f := newFakeChat()
			deps := chatDeps(f)
			deps.Actor = func(context.Context, *turnctx.Turn) (chat.Actor, error) {
				return chat.Actor{Handle: "jane", Kind: chat.AuthorOperator,
					OperatorID: "ops"}, nil
			}
			deps.Operation = operationOf
			tool := registered(t, builtin.Deps{Chat: deps}, name)

			got, err := tool.Call(withOperation(t.Context(), op), args)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if !got.Failed || !strings.Contains(got.Output, "derived for another call") {
				t.Errorf("%s under another call's operation answered %+v", name, got)
			}
			if len(f.posts)+len(f.keys)+len(f.reactions)+len(f.joined)+len(f.left) != 0 {
				t.Errorf("%s under another call's operation reached the store: "+
					"posts %v, keys %v, reactions %v, joined %v, left %v", name,
					f.posts, f.keys, f.reactions, f.joined, f.left)
			}
		})
	}
}

// A DIRECT MESSAGE WHOSE CONVERSATION'S OPEN CAME BACK UNKNOWN SAYS NOTHING,
// and says so.
//
// The open derives its operation from the work's start, so it can answer
// `unknown` — a lost acknowledgement, or a node whose ledger cannot vouch for
// an operation minted before the rows it lost, whose answer names no room at
// all. Posting on regardless sent the message into the room id "" — refused as
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
			if !got.Failed || !got.Unknown || got.Refusal != tools.RefusalUnavailable {
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
// Every receipt here used to be built whatever the write answered, so an
// `unknown` post came back with a `message_id` and a seat reported a message
// it may never have said — while an operation this node's ledger cannot
// vouch for is answered `unknown` WITHOUT being published, and its answer
// names no room, so a join reported `joined` into a room called nothing. Each
// write now answers the unknown result: a post with how to repeat it as the
// same message and what a reworded one costs, a statement — a reaction, a
// join, a leave — with its repeat offered, because stating the same thing
// again changes nothing.
func TestAChatWriteWhoseOutcomeIsUnknownIsNeitherDoneNorFailed(t *testing.T) {
	t.Parallel()
	const op = "01a11a61-ff0c-77c1-8cb1-7b9e5497550e"
	writes := seatChatWrites()
	for name, outcome := range map[string]statelog.Result{
		"unvouched":            {Outcome: statelog.OutcomeUnknown, OpID: op, Unvouched: true},
		"lost acknowledgement": {Outcome: statelog.OutcomeUnknown, OpID: op},
	} {
		for _, w := range writes {
			t.Run(name+"/"+w.tool, func(t *testing.T) {
				t.Parallel()
				f := newFakeChat()
				f.outcome = &outcome
				reg := chatRegistry(t, chatDeps(f))
				got := callChat(t, reg, w.tool, turnFor(t, "agent-cto"), w.args)
				if !got.Failed || !got.Unknown || got.Refusal != tools.RefusalUnavailable {
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
			map[string]any{"channel": chatRoomID, "message": "m1", "emoji": ":+1:"},
			"harmless"},
		{builtin.JoinChannelTool, map[string]any{"channel": chatRoomID}, "harmless"},
		{builtin.LeaveChannelTool, map[string]any{"channel": chatRoomID}, "harmless"},
	}
}

// A WRITE NOBODY ANSWERED SAYS HOW THIS CALLER REPEATS IT.
//
// The router answers [estate.ErrOutcomeUnknown] for two classes of write. One
// carrying no key of its caller's — an operator's assistant over MCP — is
// reported at the first data node that stayed silent, and its repeat is a
// second write. A KEYED one — a turn's call, a person's request — is asked of
// every other data node first and reported only when none of them answered,
// and the same call IS its safe retry. On a node without data every chat
// write is remote, so this is the common path, not an edge. The answer used to
// tell every caller that a second call is made twice, which sent a seat
// looking where it could simply have repeated itself — and the answer a
// person's act transport rendered contradicted the outbox's own retry under
// the same request id.
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
			if !got.Failed || !got.Unknown {
				t.Fatalf("a %s nobody answered answered %+v, want unknown", w.tool, got)
			}
			if !strings.Contains(got.Output, w.next) {
				t.Errorf("a turn's %s nobody answered does not offer %q: %s",
					w.tool, w.next, got.Output)
			}
			if strings.Contains(got.Output, "made twice") ||
				strings.Contains(got.Output, "a second call is a new operation") {
				t.Errorf("a turn's %s nobody answered warns that its repeat "+
					"duplicates, which for a turn it does not: %s", w.tool, got.Output)
			}
		})
	}

	// AN UNKEYED CALLER IS WARNED: the operator's assistant names no
	// operation for these tools, so its repeat is a second message.
	f := newFakeChat()
	f.writeErr = silent
	person := operatorChat(t, f, nil)
	got := callOperator(t, person[builtin.PostMessageTool],
		map[string]any{"channel": chatRoomID, "body": "on it"})
	if !got.Failed || !got.Unknown {
		t.Fatalf("an operator's post nobody answered answered %+v, want unknown", got)
	}
	if !strings.Contains(got.Output, "a second call is a new operation") ||
		strings.Contains(got.Output, "exactly the same arguments") {
		t.Errorf("an operator's post nobody answered does not warn that its "+
			"repeat is a second message: %s", got.Output)
	}
	// AND A STATEMENT IS STILL OFFERED AS HARMLESS, whoever made it.
	got = callOperator(t, person[builtin.ReactToMessageTool],
		map[string]any{"channel": chatRoomID, "message": "m1", "emoji": ":+1:"})
	if !got.Unknown || !strings.Contains(got.Output, "harmless") {
		t.Errorf("an operator's reaction nobody answered is not offered as "+
			"harmless: %s", got.Output)
	}
}

// NO POSTING TOOL IS COUNTED AMONG A FAMILY, which is what lets a post's
// "make the same call again" name its own tool and no other.
//
// A turn's repeat count for a tool in a family (chat.CallFamily) is raised by
// EVERY call to another tool of that family, whatever its arguments, so the
// sentence a post's unknown answer gives — "before calling it with any
// others" — would be wrong for one: it would let the other tool be called
// between, and that call makes the repeat a new operation. The posting tools
// are the only ones that answer with the sentence (membership and reactions
// state rather than add, and offer their repeat instead), and a post is
// additive, so none of them belongs to a family. A posting tool given one has
// to take a sentence that names the rest of its family unconditionally.
func TestNoPostingToolIsCountedAmongAFamily(t *testing.T) {
	t.Parallel()
	// AND A ROOM'S CREATE, which answers an unknown with the same sentence.
	for _, tool := range append(chat.WriteTools(), chat.CreateChannelTool) {
		if family := chat.CallFamily(tool); len(family) != 0 {
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
	// AND THE TURN ID IS THE RUN, which is provenance — while the KEY'S
	// SEED is the work key: the run id is minted fresh on every attempt,
	// so an id derived from it makes every redelivery a second copy of what
	// the turn said.
	if actor.TurnID != "run-1" {
		t.Errorf("the message names run %q, want the run that said it", actor.TurnID)
	}
	if key := f.posts[0].in.CallKey; key.Seed != "wk-1" {
		t.Errorf("the message id is seeded from %q, want the turn's work key — "+
			"a re-run would post everything twice", key.Seed)
	}
}

// EVERY WRITE WAITS FOR ITS OWN POSITION, AND NO READ WAITS AT ALL.
//
// A write lands on the fleet's log and a read is answered from whichever
// copy's rows serve it, so a seat that posts and then reads the room in the
// same turn must be made to see its own message — a model that cannot find
// what it just said says it again. The position is handed to the seam
// ([builtin.ChatDeps.Await]) — a seat's node session floor, which every later
// read carries to whichever copy answers it, or the operator surface's own
// applier — and it must be THIS write's, or the next read can miss it. Waiting
// before a READ would buy nothing: there is no position to wait for that the
// caller did not already produce. The gestures only a person makes are held
// to the same rule, through the catalogue that serves them.
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

	person := operatorChat(t, f, nil)
	for name, want := range map[string]uint64{
		chat.CreateChannelTool: 41, chat.OpenDMTool: 11, chat.UpdateChannelTool: 42,
		chat.SetChannelMembersTool: 43, chat.EditMessageTool: 44,
		chat.DeleteMessageTool: 45,
	} {
		before := len(f.awaited)
		if got := callOperator(t, person[name], operatorChatWrites[name]); got.Failed {
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
		builtin.ReactToMessageTool: {
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		},
		builtin.JoinChannelTool:  own,
		builtin.LeaveChannelTool: own,

		// THE PERSON'S OWN GESTURES, classified by the same switch: the
		// dashboard's act transport refuses a proven read, and an assistant
		// is told which of these destroys what somebody else wrote.
		chat.CreateChannelTool: shared,
		chat.OpenDMTool: {
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		},
		chat.UpdateChannelTool:     restated,
		chat.SetChannelMembersTool: restated,
		chat.EditMessageTool:       restated,
		chat.DeleteMessageTool:     restated,
		chat.MarkChatReadTool:      own,
	}
	for _, name := range append(chat.Tools(), chat.OperatorOnlyTools()...) {
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
// had not just listed the rooms — and a name held only against the rooms a
// seat is in would leave a unit's room it was never in, but owes a post to,
// writable by an id nothing ever handed it. That half runs end to end over
// the real store in internal/chat's TestASeatOutsideAUnitSpeaksInItsRoomByName;
// this fake resolves its one public room for every viewer, as the store does.
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

	// AND A NAME NOTHING THIS SEAT MAY READ HOLDS SAYS WHERE TO LOOK. It is
	// the argument's to fix rather than a room that does not exist, because
	// a private room the seat is not in answers exactly as a name nobody
	// holds — and no address, id or name, reaches that one.
	got := callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": "marketing", "body": "hello",
	})
	if !got.Failed || got.Refusal != tools.RefusalInvalid {
		t.Fatalf("a post to a name nothing readable holds answered %+v, want "+
			"the argument's class", got)
	}
	if !strings.Contains(got.Output, builtin.ListChannelsTool) {
		t.Errorf("the refusal does not say how to find the room: %s", got.Output)
	}

	// AND A ROOM THE STORE DOES NOT HOLD NAMES BOTH ADDRESSES, so the seat
	// reading it is never sent after an id it did not need: a name reaches
	// every room it may read as surely as an id does.
	f.writeErr = fmt.Errorf("%w: room %s", chat.ErrNotFound, chatRoomID)
	got = callChat(t, reg, builtin.PostMessageTool, turn, map[string]any{
		"channel": chatRoomID, "body": "hello",
	})
	if !got.Failed || got.Refusal != tools.RefusalNotFound ||
		!strings.Contains(got.Output, builtin.ListChannelsTool) ||
		!strings.Contains(got.Output, "its id or to its name") {

		t.Errorf("a post to a room the store does not hold answered %+v, want "+
			"not-found naming where a room is looked up by either address", got)
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
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Chat: deps}) {
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
// for is the one the call acts as, never the token's name, which is not who
// `mark_chat_read` wrote it under.
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
		t.Errorf("the read state was asked for %v, want the person the call acts "+
			"as (jane), never the token (ops)", *asked)
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
// every row uses for a number.
func TestAnUnreadableReadStateCostsTheCountsAndNotTheListing(t *testing.T) {
	t.Parallel()
	tool, _ := readStateChat(t, newFakeChat(), coord.ChatReadState{},
		errors.New("coordination store unreachable"))
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
		!strings.Contains(said, "coordination store unreachable") {
		t.Errorf("the listing says %q of its read state, want unknown, and why", said)
	}
	if _, said := top["unread"]; said {
		t.Errorf("the answer puts a sentence under `unread`, which every row uses "+
			"for a number: %v", top)
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
		// FORBIDDEN, the class a person's surface prints as written —
		// which is what carries the binding's name to the person who has
		// to add it. Invalid would tell them to change an argument they
		// never sent.
		if got.Refusal != tools.RefusalForbidden {
			t.Errorf("%s refused a caller with no seat as %q, want %q", c.name,
				got.Refusal, tools.RefusalForbidden)
		}
	}
	if f.reads != 0 || len(f.posts) != 0 {
		t.Errorf("a caller with no seat reached the backend: %d reads, %d posts",
			f.reads, len(f.posts))
	}

	// AND EVERY TOOL THE OPERATOR CATALOGUE SERVES, which is where an
	// unbound token actually arrives: the dashboard's act transport and a
	// person's assistant both reach chat through it, the read-state write
	// included.
	var flushed []flush
	deps.Cursors = fakeCursors{flushed: &flushed}
	catalogue := map[string]tools.Callable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Chat: deps}) {
		catalogue[tool.Name()] = tool
	}
	args := map[string]map[string]any{
		builtin.ListChannelsTool:   nil,
		builtin.ReadChannelTool:    {"channel": chatRoomID},
		builtin.SearchMessagesTool: {"text": "anything"},
		builtin.PostMessageTool:    {"channel": chatRoomID, "body": "hi"},
		builtin.ReplyInThreadTool: {
			"channel": chatRoomID, "reply_to": "m1", "body": "hi",
		},
		builtin.SendDMTool: {"to": []any{"agent-ceo"}, "body": "hi"},
		builtin.ReactToMessageTool: {
			"channel": chatRoomID, "message": "m1", "emoji": ":+1:",
		},
		builtin.JoinChannelTool:  {"channel": chatRoomID},
		builtin.LeaveChannelTool: {"channel": chatRoomID},
		chat.MarkChatReadTool:    {"mute": []any{chatRoomID}},
	}
	for name, a := range operatorChatWrites {
		args[name] = a
	}
	for _, name := range append(chat.Tools(), chat.OperatorOnlyTools()...) {
		tool, served := catalogue[name]
		if !served {
			t.Errorf("the operator catalogue does not serve %s, so this case "+
				"cannot hold it", name)
			continue
		}
		got := callOperator(t, tool, args[name])
		if !got.Failed || got.Refusal != tools.RefusalForbidden ||
			!strings.Contains(got.Output, "contact.crewlet_operator_id") {

			t.Errorf("%s answered a caller with no seat %+v, want forbidden "+
				"carrying the remedy", name, got)
		}
	}
	if f.reads != 0 || len(f.posts) != 0 || len(f.keys) != 0 || len(flushed) != 0 {
		t.Errorf("a caller with no seat reached the backend: %d reads, %d posts, "+
			"%d other writes, %d read-state flushes", f.reads, len(f.posts),
			len(f.keys), len(flushed))
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

// EVERY CHAT TOOL ANSWERS A REFUSAL WITH ITS CLASS, never with the argument
// class a bare failure carries.
//
// The class is what a person's surface acts on — whether it prints the tool's
// sentence, which status the act transport answers, whether a retry could ever
// help — and every chat failure used to be the argument class, so a node that
// could not read its rows, a lost race and a name somebody else holds all told
// a person to fix their input. Each refusal here is shaped the way the store
// builds it and reaches the tool through its writer or its reader, so a call
// site that bypasses the classifier is caught at the call site.
func TestEveryChatToolAnswersARefusalWithItsClass(t *testing.T) {
	t.Parallel()
	writes := map[string]map[string]any{
		builtin.PostMessageTool:   {"channel": chatRoomID, "body": "standup moved"},
		builtin.ReplyInThreadTool: {"channel": chatRoomID, "reply_to": "m1", "body": "ok"},
		builtin.SendDMTool:        {"to": []any{"agent-ceo"}, "body": "a word"},
		builtin.ReactToMessageTool: {
			"channel": chatRoomID, "message": "m1", "emoji": ":+1:",
		},
		builtin.JoinChannelTool:  {"channel": chatRoomID},
		builtin.LeaveChannelTool: {"channel": chatRoomID},
	}
	for name, args := range writes {
		for _, c := range chatWriteRefusals {
			want, err := c.want, c.err
			t.Run(name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				f := newFakeChat()
				f.writeErr = err
				got := callChat(t, chatRegistry(t, chatDeps(f)), name,
					turnFor(t, "agent-cto"), args)
				if !got.Failed || got.Refusal != want {
					t.Errorf("%s refused by %v answered class %q, want %q: %s",
						name, err, got.Refusal, want, got.Output)
				}
			})
		}
	}

	// AND THE GESTURES ONLY A PERSON MAKES, through the operator catalogue
	// that serves them to the dashboard: the same store, the same sentinels,
	// and a person's screen acting on the class exactly as a model does.
	for name, args := range operatorChatWrites {
		for _, c := range chatWriteRefusals {
			want, err := c.want, c.err
			t.Run("operator/"+name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				f := newFakeChat()
				f.writeErr = err
				got := callOperator(t, operatorChat(t, f, nil)[name], args)
				if !got.Failed || got.Refusal != want {
					t.Errorf("%s refused by %v answered class %q, want %q: %s",
						name, err, got.Refusal, want, got.Output)
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
			tools.RefusalUnavailable: errors.New("the store would not answer"),
			tools.RefusalNotFound:    fmt.Errorf("%w: room room-1", chat.ErrNotFound),
			// AN ARGUMENT THE READER REFUSED — a cursor that is no
			// position — is the caller's to fix: "try again" is a retry
			// refused the same way every time.
			tools.RefusalInvalid: chat.ChannelPatch{}.Validate(),
			// A READER WITH NO SEAT BEHIND IT is refused by the store
			// naming the binding it lacks, which is no retry either.
			tools.RefusalForbidden: fmt.Errorf("%w: viewer: this chat read "+
				"resolved to no seat", chat.ErrForbidden),
		} {
			t.Run(name+"/"+string(want), func(t *testing.T) {
				t.Parallel()
				f := newFakeChat()
				f.readErr = err
				got := callChat(t, chatRegistry(t, chatDeps(f)), name,
					turnFor(t, "agent-cto"), args)
				if !got.Failed || got.Refusal != want {
					t.Errorf("%s refused by %v answered class %q, want %q: %s",
						name, err, got.Refusal, want, got.Output)
				}
			})
		}
	}
}

// operatorChatWrites are the gestures only a person makes that append to the
// log, each with arguments that reach its writer — every one the operator
// catalogue serves but mark_chat_read, which writes the coordination store.
var operatorChatWrites = map[string]map[string]any{
	chat.CreateChannelTool: {"name": "launch"},
	chat.OpenDMTool:        {"to": []any{"bob"}},
	chat.UpdateChannelTool: {"channel": chatRoomID, "topic": "the launch"},
	chat.SetChannelMembersTool: {
		"channel": chatRoomID, "members": []any{"jane"},
	},
	chat.EditMessageTool:   {"channel": chatRoomID, "message": "m1", "body": "fixed"},
	chat.DeleteMessageTool: {"channel": chatRoomID, "message": "m1"},
}

// chatWriteRefusals are the chat store's write refusals, each with the class
// every chat write tool answers it with.
var chatWriteRefusals = []struct {
	name string
	want tools.Refusal
	err  error
}{
	{"invalid", tools.RefusalInvalid,
		fmt.Errorf("%w: body: a message is 40960 bytes", chat.ErrInvalid)},
	{"archived", tools.RefusalInvalid,
		fmt.Errorf("%w: room-1 was archived", chat.ErrArchived)},
	{"forbidden", tools.RefusalForbidden,
		fmt.Errorf("%w: room-1 is a private room", chat.ErrForbidden)},
	{"name taken", tools.RefusalExists, fmt.Errorf("%w: launch", chat.ErrNameTaken)},
	{"room exists", tools.RefusalExists, fmt.Errorf("%w: dm-1", chat.ErrRoomExists)},
	{"conflict", tools.RefusalConflict, fmt.Errorf("%w: room-1", chat.ErrConflict)},
	{"not found", tools.RefusalNotFound, fmt.Errorf("%w: room room-1", chat.ErrNotFound)},
	// A ROOM A NEWER PEER MADE IS THIS NODE'S, NOT A PERMISSION.
	{"unknown kind", tools.RefusalUnavailable,
		fmt.Errorf("%w: room-1 is a \"guild\" room", chat.ErrUnknownKind)},
	{"log refused", tools.RefusalUnavailable,
		&statelog.Unavailable{Reason: statelog.ReasonDeferred}},
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
	if !got.Failed || got.Refusal != tools.RefusalInvalid {
		t.Errorf("a name nothing matches answered %+v, want the argument class", got)
	}

	f = newFakeChat()
	f.readErr = errors.New("the store would not answer")
	got = callChat(t, chatRegistry(t, chatDeps(f)), builtin.PostMessageTool,
		turnFor(t, "agent-cto"), map[string]any{"channel": "eng", "body": "hi"})
	if !got.Failed || got.Refusal != tools.RefusalUnavailable {
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
	if !got.Failed || got.Refusal != tools.RefusalInvalid ||
		!strings.Contains(got.Output, "by its id") {
		t.Errorf("a name on a surface with no reader answered %+v", got)
	}
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
	f := newFakeChat()
	f.opened = &statelog.Result{Outcome: statelog.OutcomePending,
		Position: statelog.Position{Stream: "CREWLET_CHAT_LOG", Seq: 11}}
	got := callChat(t, chatRegistry(t, chatDeps(f)), builtin.SendDMTool,
		turnFor(t, "agent-cto"), map[string]any{"to": []any{"agent-ceo"}, "body": "a word"})
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

	f = newFakeChat()
	// THE STORE'S OWN FIELD REFUSAL, so the sentence a person is shown is
	// the store's without its Go prefix.
	f.postErr = chat.ChannelPatch{}.Validate()
	got = callChat(t, chatRegistry(t, chatDeps(f)), builtin.SendDMTool,
		turnFor(t, "agent-cto"), map[string]any{"to": []any{"agent-ceo"}, "body": "a word"})
	if !got.Failed || got.Refusal != tools.RefusalInvalid {
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
	f = newFakeChat()
	f.postErr = errors.New("statelog: read the operation ledger: disk I/O")
	got = callChat(t, chatRegistry(t, chatDeps(f)), builtin.SendDMTool,
		turnFor(t, "agent-cto"), map[string]any{"to": []any{"agent-ceo"}, "body": "a word"})
	if !got.Failed || got.Refusal != tools.RefusalUnavailable {
		t.Fatalf("a post the node could not make answered %+v, want the node's class", got)
	}
	for _, want := range []string{"is open", "dm-agent-ceo", "was NOT sent", "disk I/O"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the answer does not say %q: %s", want, got.Output)
		}
	}
	if strings.Contains(got.Output, "nothing was written") {
		t.Errorf("the answer says the conversation is open and that nothing was "+
			"written: %s", got.Output)
	}

	// A POST NOBODY ANSWERED AFTER THE OPEN is unknown, never "NOT sent":
	// the router does not repeat a post carrying no key of its caller's, so
	// one a data node never answered may have been said — and the node's
	// class the answer above rewrites is the class an unknown is said in.
	f = newFakeChat()
	f.postErr = fmt.Errorf("%w (chat.post, asked of data-1)", estate.ErrOutcomeUnknown)
	got = callChat(t, chatRegistry(t, chatDeps(f)), builtin.SendDMTool,
		turnFor(t, "agent-cto"), map[string]any{"to": []any{"agent-ceo"}, "body": "a word"})
	if !got.Failed || !got.Unknown || got.Refusal != tools.RefusalUnavailable {
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
	got = callChat(t, chatRegistry(t, chatDeps(f)), builtin.SendDMTool,
		turnFor(t, "agent-cto"), map[string]any{"to": []any{"agent-ceo"}, "body": "a word"})
	if !got.Failed || !got.Unknown {
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
	got = callChat(t, chatRegistry(t, chatDeps(f)), builtin.SendDMTool,
		turnFor(t, "agent-cto"), map[string]any{
			"to": []any{"agent-ceo"}, "body": strings.Repeat("x", chat.MaxBody+1),
		})
	if !got.Failed || got.Refusal != tools.RefusalInvalid {
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
