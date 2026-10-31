package chatlet_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// A SEAT'S TURN SAYS EVERYTHING IT SAID, ONCE — end to end, from the model's
// tool call to the rows.
//
// Nothing between the two is mocked: the seat's own chat tools behind the
// authority gate, the tool surface a real turn's calls go through — which
// keeps the run's call log — and the shipped store over a real broker. Every
// half of the property lives in a different package, which is exactly why it
// is asserted here rather than in any one of them: the store derives a
// message's id from the key the tool hands it, the tool derives that key from
// the turn and reads its repeat count from the log, and the surface is what
// writes the log. A count read from anything the surface does not fill makes
// every post of a turn its first, and a seat's second remark in one room is
// then dropped as a duplicate of the first — a failure each package's own
// tests pass straight over.
func TestASeatsTurnSaysEverythingItSaidOnce(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	room := r.room(person("jane"), "launch", "eng").Channel.ID
	seat := newSeatTools(t, r)

	run := func(runID string) {
		say := seat.run(runID)
		first := say.message(chatlet.PostMessageTool,
			map[string]any{"channel": room, "body": "on it"})
		say.message(chatlet.PostMessageTool,
			map[string]any{"channel": room, "body": "opened ENG-14"})
		say.message(chatlet.ReplyInThreadTool,
			map[string]any{"channel": room, "reply_to": first, "body": "it was the cache"})
		// THE SAME WORDS AFTER A DIFFERENT POST: a remark of its own.
		say.message(chatlet.PostMessageTool,
			map[string]any{"channel": room, "body": "on it"})
		// AND ASKED AGAIN WITH NOTHING BETWEEN, as an executor that repeats
		// itself does: the same message.
		say.message(chatlet.PostMessageTool,
			map[string]any{"channel": room, "body": "on it"})
	}
	messages := func() int {
		t.Helper()
		return r.count(`SELECT COUNT(*) FROM chat_messages WHERE channel_id = ? AND event = ''`, room)
	}

	run("run-1")
	if got := messages(); got != 4 {
		t.Fatalf("a turn that said four different things — two posts, a reply, "+
			"and its first words again after the second — left %d messages; the "+
			"fifth call repeated the fourth and is one with it", got)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages
		WHERE channel_id = ? AND body = ?`, room, "on it"); got != 2 {
		t.Fatalf("\"on it\" was said before and after another post and the room "+
			"holds it %d times, want 2", got)
	}

	// THE RE-RUN: a redelivered trigger runs the turn again, under a new run
	// id with an empty call log, and makes the same calls.
	end := seat.end()
	run("run-2")
	if got := messages(); got != 4 {
		t.Fatalf("a re-run of the turn left %d messages, want the first run's "+
			"4 — a redelivery would post the whole conversation again", got)
	}
	if got := seat.end(); got != end {
		t.Errorf("a re-run of the turn published %d records, want none", got-end)
	}
}

// A SEAT THAT SAYS ONE THING IN TWO PLACES SAYS IT IN BOTH, as often as it
// says it — end to end, through the counts a real call log hands the calls.
//
// The count can hand two different calls one number: "on it" in #launch,
// then in #ops, then in #launch again counts the second and third 1 each, and
// those two differ only by the room. A reply under one message, another, then
// the first again is the same shape by the message answered; and a reaction
// taken back and put on again after another emoji counts 2, like that emoji
// did. Each third call is a gesture of its own, and a seat whose third call
// was answered as the second's retry would be told `applied` for something
// nobody ever sees.
func TestASeatSaysOneThingInTwoPlacesInBoth(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	launch := r.room(person("jane"), "launch", "eng").Channel.ID
	ops := r.room(person("jane"), "ops", "eng").Channel.ID
	question := r.say(person("jane"), launch, "why is it slow?", "k-q").Message.ID
	other := r.say(person("jane"), launch, "and the cache?", "k-c").Message.ID
	call := newSeatTools(t, r).run("run-1")
	receipt := func(tool string, args map[string]any) (id, channel, root string) {
		t.Helper()
		var got struct {
			MessageID  string `json:"message_id"`
			Channel    string `json:"channel"`
			ThreadRoot string `json:"thread_root"`
		}
		if err := json.Unmarshal([]byte(call.do(tool, args)), &got); err != nil {
			t.Fatalf("%s answered a receipt that does not decode: %v", tool, err)
		}
		return got.MessageID, got.Channel, got.ThreadRoot
	}

	// THE ROOM.
	post := func(room string) (string, string) {
		id, channel, _ := receipt(chatlet.PostMessageTool,
			map[string]any{"channel": room, "body": "on it"})
		return id, channel
	}
	first, _ := post(launch)
	second, _ := post(ops)
	third, in := post(launch)
	if third == first || third == second || in != launch {
		t.Fatalf("\"on it\" in #launch after #ops answered %s in %s — the "+
			"earlier ones are %s and %s", third, in, first, second)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages
		WHERE channel_id = ? AND body = ?`, launch, "on it"); got != 2 {
		t.Fatalf("#launch holds \"on it\" %d times, want 2", got)
	}

	// THE MESSAGE A REPLY ANSWERS.
	reply := func(to string) (string, string) {
		id, _, root := receipt(chatlet.ReplyInThreadTool,
			map[string]any{"channel": launch, "reply_to": to, "body": "ok"})
		return id, root
	}
	underQuestion, _ := reply(question)
	underOther, _ := reply(other)
	again, root := reply(question)
	if again == underQuestion || again == underOther || root != question {
		t.Fatalf("\"ok\" under the question after the other thread answered %s "+
			"under %q — the earlier replies are %s and %s", again, root,
			underQuestion, underOther)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages
		WHERE thread_root = ? AND body = ?`, question, "ok"); got != 2 {
		t.Fatalf("the question's thread holds \"ok\" %d times, want 2", got)
	}

	// THE EMOJI.
	react := func(emoji string, remove bool) {
		receipt(chatlet.ReactToMessageTool, map[string]any{
			"channel": launch, "message_id": question, "emoji": emoji, "remove": remove,
		})
	}
	react(":+1:", false)
	react(":+1:", true)
	react(":eyes:", false)
	react(":+1:", false)
	if got := r.count(`SELECT COUNT(*) FROM chat_reactions WHERE message_id = ?`,
		question); got != 2 {
		t.Fatalf("the question carries %d reactions after :+1: was taken back, "+
			":eyes: put on and :+1: put back, want both", got)
	}
}

// A SEAT THAT LEAVES A ROOM AND JOINS IT AGAIN IS IN IT — and one that joins
// and leaves again is out of it — end to end, for the reason the cases above
// are: the halves live in three packages.
//
// join_channel and leave_channel are two tools writing ONE membership, and a
// repeat count kept per tool cannot see the other one: the second join of
// join, leave, join is its tool's first call after nothing different, so it
// would derive the first join's operation, the ledger would answer it as that
// join's retry — `joined`, at the first join's revision, with nothing
// published — and the seat would stay outside the room. The pair is counted as
// one family ([chatlet.CallFamily]), so the leave raises the second join's count.
//
// A RE-RUN of the same calls still publishes nothing, because the family's
// counts are a function of the calls and a re-run makes the same ones.
func TestASeatThatRejoinsARoomIsInIt(t *testing.T) {
	t.Parallel()
	join, leave := chatlet.JoinChannelTool, chatlet.LeaveChannelTool
	cases := []struct {
		name    string
		founded []string
		calls   []string
		member  bool
	}{
		{"join, leave, join", nil, []string{join, leave, join}, true},
		{"leave, join, leave", []string{"eng"}, []string{leave, join, leave}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newWriteRound(t, agents())
			room := r.room(person("jane"), "ops", c.founded...).Channel.ID
			seat := newSeatTools(t, r)
			member := func() bool {
				t.Helper()
				return r.count(`SELECT COUNT(*) FROM chat_members
					WHERE channel_id = ? AND handle = ?`, room, "eng") == 1
			}
			run := func(runID string) {
				call := seat.run(runID)
				for _, tool := range c.calls {
					call.do(tool, map[string]any{"channel": room})
				}
			}

			run("run-1")
			if got := member(); got != c.member {
				t.Fatalf("after %s the seat's membership is %v, want %v — the "+
					"third call was answered as the first one's retry and "+
					"published nothing", c.name, got, c.member)
			}
			end := seat.end()
			run("run-2")
			if got := seat.end(); got != end {
				t.Errorf("a re-run of %s published %d records — a redelivered "+
					"turn would make every membership change again", c.name, got-end)
			}
			if got := member(); got != c.member {
				t.Errorf("after the re-run the seat's membership is %v, want %v",
					got, c.member)
			}
		})
	}
}

// A ROOM NAMED TWO WAYS IS STILL ONE ROOM, AND A CALL TYPED TWO WAYS IS STILL
// TWO CALLS.
//
// The repeat count tells calls apart by their arguments as the model typed
// them, and the store derives an id from what they RESOLVED to — the room's
// id, not "#ops" or the uuid. Without the call's own arguments in the identity
// two calls typed differently can share both a count and an identity. Here the
// last leave, by id, follows the join, the leave by name and the join before it
// — three different calls — and the leave by name followed the two leaves by
// id and the first join: three as well. So the last leave would derive the
// leave by name's id and be answered with that leave's success, and the seat
// would still be in the room the join before it had put it back in.
func TestACallTypedAnotherWayIsAnotherCall(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	room := r.room(person("jane"), "ops", "eng").Channel.ID
	call := newSeatTools(t, r).run("run-1")
	join, leave := chatlet.JoinChannelTool, chatlet.LeaveChannelTool
	byID, byName := map[string]any{"channel": room}, map[string]any{"channel": "#ops"}

	call.do(leave, byID)
	call.do(leave, byID) // asked again: the same leave
	call.do(join, byID)
	call.do(leave, byName) // the room by its address, as written
	call.do(join, byID)
	call.do(leave, byID)
	if got := r.count(`SELECT COUNT(*) FROM chat_members
		WHERE channel_id = ? AND handle = ?`, room, "eng"); got != 0 {
		t.Fatalf("the seat's last call left the room and it is still in it — " +
			"the leave by id was answered as the leave by name before it")
	}
}

// A SEAT OUTSIDE A UNIT SPEAKS IN THAT UNIT'S ROOM BY ITS NAME — end to end,
// from the tool call to the row.
//
// A decision may promise its outcome to any unit's room, and the engine then
// holds the asker's turn until the post is made there. A unit's room is one a
// seat cannot join — its membership is the org chart's — so a name that
// resolved only among the rooms the caller was in would leave an asker outside
// the unit naming the room it owed a post to and never reaching it: refused
// "no room called", with no tool that would hand it the id, its turn sent back
// until its cap. The name is the room's address, and it resolves for anybody
// who may read the room.
func TestASeatOutsideAUnitSpeaksInItsRoomByName(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents())
	room := r.unitRoom("platform", "platform", "ops").Channel.ID
	if got := r.count(`SELECT COUNT(*) FROM chat_members WHERE channel_id = ? AND handle = ?`,
		room, "eng"); got != 0 {
		t.Fatalf("the seat is in the unit's room, so this case certifies nothing")
	}
	call := newSeatTools(t, r).run("run-1")
	call.do(chatlet.PostMessageTool, map[string]any{
		"channel": "#platform", "body": "decided: we ship on friday",
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_messages
		WHERE channel_id = ? AND author_handle = ? AND body = ?`,
		room, "eng", "decided: we ship on friday"); got != 1 {
		t.Fatalf("the seat's post to #platform left %d messages in the unit's room", got)
	}
}

// A SEAT'S LINK TO NO ADDRESS IS REFUSED IN THE STORE'S WORDS, naming the
// entry, and nothing is written: end to end, from the model's call to the log.
//
// What a link is, is the store's rule ([chatlet.NewMessage.Validate]), and the
// seat's three posting tools reach it on paths of their own: post_message and
// reply_in_thread through the post, send_dm before it opens the conversation.
// Each answers the model with the store's own sentence, which names the entry
// to change, classes the refusal as an argument to change, and publishes
// nothing: no message, and for send_dm no conversation opened by a refusal.
// Mutation: admit every link in the store's check, and each call is said.
func TestASeatsLinkToNoAddressIsRefusedInTheStoresWords(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "eng").Channel.ID
	question := r.say(person("jane"), room, "where is the plan?", "k-q").Message.ID
	links := []string{"https://example.com/plan", "#/worklet/ENG-1"}
	refused := chatlet.NewMessage{Body: "here", Links: links}.Validate()
	if refused == nil {
		t.Fatalf("the store takes %q as a link, so no tool is refused it", links[1])
	}
	want := chatlet.Sentence(refused)
	if !strings.Contains(want, "`links[1]`") {
		t.Fatalf("the store refuses the second link with %q, which does not name it", want)
	}
	sent := []any{links[0], links[1]}
	seat := newSeatTools(t, r)
	end := seat.end()
	call := seat.run("run-1")
	for tool, args := range map[string]map[string]any{
		chatlet.PostMessageTool: {"channel": room, "body": "here", "links": sent},
		chatlet.ReplyInThreadTool: {"channel": room, "reply_to": question, "body": "here",
			"links": sent},
		chatlet.SendDMTool: {"to": []any{"jane"}, "body": "here", "links": sent},
	} {
		res, err := call.surface.Execute(t.Context(), llm.ToolCall{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if !res.Failed || res.Refusal() != tools.RefusalInvalid ||
			!strings.Contains(res.Output, want) {
			t.Errorf("%s answered %q (failed %v, class %q), want the store's %q, "+
				"classed %s", tool, res.Output, res.Failed, res.Refusal(), want,
				tools.RefusalInvalid)
		}
	}
	r.drain()
	if got := seat.end(); got != end {
		t.Errorf("the refused calls published %d records", got-end)
	}
}

// A MESSAGE'S LINKS ARE A SET: a link named twice is kept once, in the order
// it was first written, so it counts once against [chatlet.MaxLinks] and is
// previewed once. The tool surface reads every list argument that way
// (argval.Strings, which trims, drops empties and keeps the first of a
// repeat), so a person's composer and a seat's call land the same set.
// Mutation: read `links` without argval's cleaning, and the stored links
// carry the repeat.
func TestAMessagesLinksAreASet(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch", "eng").Channel.ID
	seat := newSeatTools(t, r)
	call := seat.run("run-1")
	sent := []any{"https://example.com/b", "https://example.com/a", "https://example.com/b"}
	res, err := call.surface.Execute(t.Context(), llm.ToolCall{Name: chatlet.PostMessageTool,
		Arguments: map[string]any{"channel": room, "body": "both", "links": sent}})
	if err != nil || res.Failed {
		t.Fatalf("post_message: %v %s", err, res.Output)
	}
	r.drain()
	if n := r.count(`SELECT COUNT(*) FROM chat_messages WHERE links = ?`,
		`["https://example.com/b","https://example.com/a"]`); n != 1 {
		t.Errorf("%d messages carry the links once each in the order written, want 1", n)
	}
}

// seatTools is the seat `eng`'s own chat tools over a round's store, as a real
// turn reaches them: behind the authority gate, through the tool surface a
// turn's calls go through, which keeps the run's call log.
type seatTools struct {
	t   *testing.T
	r   *writeRound
	reg *tools.Registry
	org *org.Organization
}

func newSeatTools(t *testing.T, r *writeRound) seatTools {
	t.Helper()
	company := &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{{Name: "Engineer", DeclaredHandle: "eng"}},
	}
	company.Normalize()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Chat:      builtin.ChatDeps{Writer: r.store, Reader: settledReads{reader(t, r)}},
		Authorize: builtin.Decide(authz.NoChart{}),
	}); err != nil {
		t.Fatalf("register the chat tools: %v", err)
	}
	return seatTools{t: t, r: r, reg: reg, org: company}
}

// reader is the read side over a round's rows, because a caller names a room
// by its name and the tool resolves that name — and decides an edit or a
// delete on the message's author — through it.
func reader(t *testing.T, r *writeRound) *chatlet.Reader {
	t.Helper()
	authority, err := statelogtest.LocalReaderOver(chatlet.Domain{}, r.db, r.waiter)
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	read, err := chatlet.NewReader(chatlet.ReaderOptions{
		Log: authority, Committed: r.waiter.Committed,
	})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	return read
}

// settledReads serves a caller's reads from this node's rows as they stand,
// whatever level the tool asks for.
//
// A seat reads LINEARIZABLY, which appends a barrier to the log and waits for
// this node's applier to reach it — and this harness applies by hand, after a
// call returns, so a barrier would wait for an applier that runs only once the
// call it is blocking has finished. Every call here is drained before the next
// one is made, so the rows a read finds are already the log's; the level is
// not what these cases are about.
type settledReads struct{ *chatlet.Reader }

var stale = statelog.Freshness{Level: statelog.ReadStale}

func (r settledReads) Channels(ctx context.Context, viewer string, q chatlet.ChannelsQuery,
	_ statelog.Freshness) (chatlet.ChannelListing, error) {
	return r.Reader.Channels(ctx, viewer, q, stale)
}

func (r settledReads) Channel(ctx context.Context, viewer, channelID string,
	_ statelog.Freshness) (chatlet.ChannelDetail, error) {
	return r.Reader.Channel(ctx, viewer, channelID, stale)
}

func (r settledReads) ChannelNamed(ctx context.Context, viewer, name string,
	_ statelog.Freshness) (chatlet.ChannelDetail, error) {
	return r.Reader.ChannelNamed(ctx, viewer, name, stale)
}

func (r settledReads) Messages(ctx context.Context, viewer string, q chatlet.TranscriptQuery,
	_ statelog.Freshness) (chatlet.Transcript, error) {
	return r.Reader.Messages(ctx, viewer, q, stale)
}

func (r settledReads) Thread(ctx context.Context, viewer string, q chatlet.ThreadQuery,
	_ statelog.Freshness) (chatlet.Thread, error) {
	return r.Reader.Thread(ctx, viewer, q, stale)
}

// seatRun is one execution of a turn, making calls.
type seatRun struct {
	tools   seatTools
	surface *tools.Surface
}

// run is one execution of the turn — a fresh run id and an empty call log,
// over one unit of work begun at [requestedAt].
func (s seatTools) run(runID string) seatRun {
	turn := &turnctx.Turn{
		RunID: runID, WorkKey: "wk-launch", WorkSince: requestedAt,
		Seat: s.org.Roles[0], Org: s.org, Calls: turnctx.NewCallLog(),
	}
	return seatRun{tools: s, surface: tools.NewSurface("execute",
		s.reg.Snapshot(), chatlet.Tools()).ForTurn(turn)}
}

// do makes one call, which must not fail, and applies what it wrote.
func (c seatRun) do(tool string, args map[string]any) string {
	t := c.tools.t
	t.Helper()
	res, err := c.surface.Execute(t.Context(), llm.ToolCall{Name: tool, Arguments: args})
	if err != nil || res.Failed {
		t.Fatalf("%s %v: %v %s", tool, args, err, res.Output)
	}
	c.tools.r.drain()
	return res.Output
}

// message makes one posting call and answers the message it names.
func (c seatRun) message(tool string, args map[string]any) string {
	t := c.tools.t
	t.Helper()
	out := c.do(tool, args)
	var receipt struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal([]byte(out), &receipt); err != nil || receipt.MessageID == "" {
		t.Fatalf("%s answered %q, which names no message", tool, out)
	}
	return receipt.MessageID
}

// end is the log's last sequence, which a call that published nothing leaves
// where it was.
func (s seatTools) end() uint64 {
	s.t.Helper()
	last, err := s.r.log.End(s.t.Context())
	if err != nil {
		s.t.Fatalf("read the log's end: %v", err)
	}
	return last
}
