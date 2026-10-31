package builtin_test

import (
	"context"
	"encoding/json"
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
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// The chat gestures only a person makes, as the operator surface serves them —
// to the dashboard's act transport and to a person's own assistant. Their
// receipts' outcome and position and the same operations on a retry are held
// with every other operator write in actanswer_test.go; these cases hold what
// each one hands the store, and what each refuses before it reaches it.

// operatorChatActor is what the operator surface's chat actor builds for a
// token bound to a person: the human seat as the handle every message carries,
// and the credential beside it ([builtin.ChatActorOf]).
func operatorChatActor(context.Context, *turnctx.Turn) (chatlet.Actor, error) {
	return chatlet.Actor{Handle: "jane", Kind: chatlet.AuthorHuman, OperatorID: "pat:1"}, nil
}

// chatChart is the org chart these cases decide on: `cto` leads the unit
// `sre`, nobody leads anything else, and no seat sits in any unit.
type chatChart struct{}

func (chatChart) Leads(context.Context, string, string) (bool, error)        { return false, nil }
func (chatChart) LeadsProject(context.Context, string, string) (bool, error) { return false, nil }
func (chatChart) LeadsUnit(_ context.Context, actor, unit string) (bool, error) {
	return actor == "cto" && unit == "sre", nil
}
func (chatChart) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
}
func (chatChart) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	return actor == "cto", nil
}
func (chatChart) MemberOf(context.Context, string, string) (bool, error) {
	return false, nil
}

// personBoundTo is a context carrying a PERSON bound to the human seat named,
// holding the grants named — the principal the operator surface acts for.
func personBoundTo(seat string, grants ...iam.Grant) context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.NewSHA1(uuid.NameSpaceOID, []byte(seat)),
		Kind: iam.KindPerson, Login: seat + ".person", Seat: seat,
		Stage: iam.StageActive, Grants: grants,
	})
}

// operatorChat is the operator catalogue's chat over f, a read-state store and
// a mention resolver that names whoever the words name — acting as actor, or
// as the bound person jane when actor is nil.
func operatorChat(t *testing.T, f *fakeChat, cursors builtin.ChatCursors,
	actor func(context.Context, *turnctx.Turn) (chatlet.Actor, error)) map[string]tools.Callable {

	t.Helper()
	deps := chatDeps(f)
	deps.Actor = operatorChatActor
	if actor != nil {
		deps.Actor = actor
	}
	deps.Cursors = cursors
	deps.Mentions = fakeMentions{"bob"}
	deps.Now = func() time.Time { return time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC) }
	out := map[string]tools.Callable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Chat: deps, Authorize: builtin.Decide(chatChart{}),
	}) {
		out[tool.Name()] = tool
	}
	return out
}

// callOperator makes one call the way the operator surface does — no turn, as
// the person jane holding every grant.
func callOperator(t *testing.T, tool tools.Callable, args map[string]any) tools.Result {
	t.Helper()
	return callOperatorAs(personBoundTo("jane", iam.AllGrants...), t, tool, args)
}

// callOperatorAs is [callOperator] as the principal ctx carries.
func callOperatorAs(ctx context.Context, t *testing.T, tool tools.Callable,
	args map[string]any) tools.Result {

	t.Helper()
	if tool == nil {
		t.Fatal("the operator catalogue does not serve this tool")
	}
	got, err := tool.Call(ctx, args)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return got
}

// invokeOperator makes one call as the operator surface's transports do —
// through tools.Invoke, which holds it to the tool's schema before the tool
// runs (ADR-0059) — as the person jane holding every grant, and fails t when
// that check refused it, so what comes back is the tool's own answer to a
// call the check admitted.
func invokeOperator(t *testing.T, tool tools.Callable, raw map[string]any) tools.Result {
	t.Helper()
	got, refused, err := tools.Invoke(personBoundTo("jane", iam.AllGrants...),
		tools.Invocation{Name: tool.Name(), Tool: tool,
			Args: tools.ArgsOf(tool, false, raw), Where: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if refused {
		t.Fatalf("the check refused %v before the tool ran: %s", raw, got.Output)
	}
	return got
}

// EVERY GESTURE HANDS THE STORE WHAT THE PERSON ASKED, AS THE PERSON.
//
// A field dropped between the call and the store is a setting a person
// believes they made: a member missing from a private room's founding set is
// somebody who cannot read it, and an empty topic read as "no change" is a
// topic that will not clear. And the actor is the operator surface's, every
// time — the author field is never something a call can name.
func TestAPersonsChatGesturesReachTheStoreAsAsked(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	surface := operatorChat(t, f, nil, nil)

	if got := callOperator(t, surface[chatlet.CreateChannelTool], map[string]any{
		"channel": "#launch", "kind": "private", "topic": "the launch",
		"purpose": "ship it",
		"members": []any{"bob", map[string]any{"handle": "ana", "follow_all": true}},
	}); got.Failed {
		t.Fatalf("create_channel failed: %s", got.Output)
	}
	made := f.created[0]
	if made.Name != "launch" || made.Kind != chatlet.KindPrivate ||
		made.Topic != "the launch" || made.Purpose != "ship it" || made.Unit != "" {
		t.Errorf("create_channel handed the store %+v", made)
	}
	if want := []chatlet.Member{{Handle: "bob"}, {Handle: "ana", FollowAll: true}}; !slices.Equal(made.Members, want) {
		t.Errorf("the founding members were %+v, want %+v", made.Members, want)
	}

	// A NAMED FIELD IS SET, AN EMPTY ONE CLEARS, AN ABSENT ONE IS LEFT ALONE.
	if got := callOperator(t, surface[chatlet.UpdateChannelTool], map[string]any{
		"channel": chatRoomID, "topic": "", "archived": true,
	}); got.Failed {
		t.Fatalf("update_channel failed: %s", got.Output)
	}
	patch := f.patches[0]
	if patch.Topic == nil || *patch.Topic != "" {
		t.Errorf("an empty topic was not handed over as a topic cleared: %+v", patch.Topic)
	}
	if patch.Archived == nil || !*patch.Archived {
		t.Errorf("the archive was not handed over: %+v", patch.Archived)
	}
	if patch.Purpose != nil {
		t.Errorf("a setting the call did not name was changed: %+v", patch)
	}

	// AN EMPTY MEMBERSHIP IS A GESTURE, never nothing.
	if got := callOperator(t, surface[chatlet.SetChannelMembersTool], map[string]any{
		"channel": chatRoomID, "members": []any{},
	}); got.Failed {
		t.Fatalf("set_channel_members failed: %s", got.Output)
	}
	if set := f.memberSets[0]; set == nil || len(set) != 0 {
		t.Errorf("an emptied room was handed over as %#v, want an empty set", set)
	}

	// THE MENTIONS ARE THE NEW WORDS', re-read rather than carried.
	if got := callOperator(t, surface[chatlet.EditMessageTool], map[string]any{
		"channel": chatRoomID, "message_id": "m1", "body": "@bob it is friday",
	}); got.Failed {
		t.Fatalf("edit_message failed: %s", got.Output)
	}
	if edit := f.edits[0]; edit.Body != "@bob it is friday" || !slices.Equal(edit.Mentions, []string{"bob"}) {
		t.Errorf("edit_message handed the store %+v", edit)
	}
	// AND THE LINKS CHANGE ONLY WHEN NAMED: an edit that leaves them out
	// keeps them — the typo fix that used to remove every one — while a
	// named list, an empty one included, replaces them.
	if edit := f.edits[0]; edit.Links != nil {
		t.Errorf("an edit naming no links handed the store %v, which replaces them",
			*edit.Links)
	}
	for i, links := range [][]any{{}, {"https://example.com/plan"}} {
		if got := callOperator(t, surface[chatlet.EditMessageTool], map[string]any{
			"channel": chatRoomID, "message_id": "m1", "body": "see the plan",
			"links": links,
		}); got.Failed {
			t.Fatalf("edit_message with links failed: %s", got.Output)
		}
		edit := f.edits[1+i]
		if edit.Links == nil || len(*edit.Links) != len(links) {
			t.Errorf("an edit naming the links %v handed the store %+v", links, edit.Links)
		}
	}

	if got := callOperator(t, surface[chatlet.DeleteMessageTool], map[string]any{
		"channel": chatRoomID, "message_id": "m1",
	}); got.Failed || !slices.Equal(f.deleted, []string{"m1"}) {
		t.Fatalf("delete_message answered %+v and took back %v", got, f.deleted)
	}

	got := callOperator(t, surface[chatlet.OpenDMTool], map[string]any{"to": []any{"bob"}})
	if got.Failed || !strings.Contains(got.Output, `"created": true`) {
		t.Errorf("open_dm answered %+v, want the conversation and whether it was made", got)
	}

	for i, actor := range f.actors {
		if actor.Handle != "jane" || actor.Kind != chatlet.AuthorHuman || actor.OperatorID != "pat:1" {
			t.Errorf("gesture %d was made as %+v, not the operator surface's actor", i, actor)
		}
	}
}

// WHAT A GESTURE CANNOT READ IS REFUSED NAMING IT, AND REACHES NOTHING.
//
// Each of these used to be the shape that is silently read as something else:
// a list with an entry nobody could read is a member left out of a private
// room, an `archived: "yes"` is no archive at all, and an update naming no
// field is a record on the log for a change nobody made. A null is nothing
// sent, as the check at the surface reads it (ADR-0059): a membership sent as
// null emptied the room, and an archive sent as null names no change.
func TestAPersonsChatGestureThatCannotBeReadIsRefusedNamingIt(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		tool string
		args map[string]any
		says string
	}{
		"a member with no handle": {chatlet.CreateChannelTool, map[string]any{
			"channel": "launch", "members": []any{map[string]any{"follow_all": true}}},
			"entry 0 names no handle"},
		"a follow that is not a flag": {chatlet.SetChannelMembersTool, map[string]any{
			"channel": chatRoomID, "members": []any{map[string]any{"handle": "bob",
				"follow_all": "yes"}}}, "true or false"},
		"members not a list": {chatlet.SetChannelMembersTool, map[string]any{
			"channel": chatRoomID, "members": map[string]any{"handle": "bob"}},
			"a list of members"},
		"no membership at all": {chatlet.SetChannelMembersTool, map[string]any{
			"channel": chatRoomID}, "needs `members`"},
		"a membership sent as null": {chatlet.SetChannelMembersTool, map[string]any{
			"channel": chatRoomID, "members": nil}, "needs `members`"},
		"an archive that is not a flag": {chatlet.UpdateChannelTool, map[string]any{
			"channel": chatRoomID, "archived": "yes"}, "`archived` is true or false"},
		"an update naming nothing": {chatlet.UpdateChannelTool, map[string]any{
			"channel": chatRoomID}, "names no change"},
		"an archive sent as null": {chatlet.UpdateChannelTool, map[string]any{
			"channel": chatRoomID, "archived": nil}, "names no change"},
		"a create with no room": {chatlet.CreateChannelTool, map[string]any{
			"kind": "public"}, "needs a `channel`"},
		"an edit with no words": {chatlet.EditMessageTool, map[string]any{
			"channel": chatRoomID, "message_id": "m1"}, "delete_message"},
		"a delete naming no message": {chatlet.DeleteMessageTool, map[string]any{
			"channel": chatRoomID}, "needs a `message_id`"},
		"a conversation with nobody": {chatlet.OpenDMTool, map[string]any{},
			"needs a `to`"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeChat()
			got := callOperator(t, operatorChat(t, f, nil, nil)[c.tool], c.args)
			if tools.RefusalOf(got) != tools.RefusalInvalid {
				t.Fatalf("%s answered %+v, want the argument refusal", c.tool, got)
			}
			if !strings.Contains(got.Output, c.says) {
				t.Errorf("the refusal does not say %q: %s", c.says, got.Output)
			}
			if len(f.keys) != 0 || f.reads != 0 {
				t.Errorf("a refused %s reached the store: %d writes, %d reads",
					c.tool, len(f.keys), f.reads)
			}
		})
	}
}

// A MEMBERSHIP IS READ AS THE CHECK AT THE SURFACE ADMITS IT (ADR-0059).
//
// Every call reaches these tools through tools.Invoke, which holds it to the
// tool's schema before the tool runs. An entry the schema declared an object
// alone was a bare handle refused there as a misread, though the tool read it,
// its description offered it and the dashboard's new-room dialog sends
// nothing else. So each spelling a model or a screen writes is admitted by the
// check AND read by the tool as the same members: bare handles, objects, the
// two together, the one string a model sends where a list belongs, and a
// list carrying a null, which is a member not named. A value that names
// nobody at all is the one the tool refuses after the check admitted it, and
// only where it would empty a room (TestOnlyTheEmptyListEmptiesARoom).
//
// Mutation: declare an entry an object alone and the bare handles are refused
// as misread; read the list by a rule of its own that takes no single string,
// and "bob, ana" is refused after the check admitted it; read a null entry as
// a handle, and it is refused as an entry naming none.
func TestAMembershipIsReadAsTheCheckAdmitsIt(t *testing.T) {
	t.Parallel()
	bob, ana := chatlet.Member{Handle: "bob"}, chatlet.Member{Handle: "ana"}
	for name, c := range map[string]struct {
		members any
		want    []chatlet.Member
	}{
		"bare handles": {[]any{"bob", "ana"}, []chatlet.Member{bob, ana}},
		"objects": {[]any{map[string]any{"handle": "bob", "follow_all": true}},
			[]chatlet.Member{{Handle: "bob", FollowAll: true}}},
		"both": {[]any{"bob", map[string]any{"handle": "ana", "follow_all": true}},
			[]chatlet.Member{bob, {Handle: "ana", FollowAll: true}}},
		"one string standing for the list": {"bob, ana", []chatlet.Member{bob, ana}},
		"a follow sent as null": {[]any{map[string]any{"handle": "bob", "follow_all": nil}},
			[]chatlet.Member{bob}},
		"a null beside a handle": {[]any{nil, "bob"}, []chatlet.Member{bob}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeChat()
			tool := operatorChat(t, f, nil, nil)[chatlet.SetChannelMembersTool]
			got := invokeOperator(t, tool, map[string]any{"channel": chatRoomID,
				"members": c.members})
			if got.Failed {
				t.Fatalf("the tool refused %v, which the check admitted: %s", c.members,
					got.Output)
			}
			if len(f.memberSets) != 1 || !slices.Equal(f.memberSets[0], c.want) {
				t.Errorf("the check admitted %v and the tool read %+v, want %+v",
					c.members, f.memberSets, c.want)
			}
		})
	}
}

// ONLY THE EMPTY LIST EMPTIES A ROOM.
//
// set_channel_members states the whole membership, so a value naming nobody
// takes everybody out, and a private room emptied has nobody left who can
// read it or set its members again. The check at the surface admits a string
// where the list belongs (ADR-0059), and one naming no handle reads as an
// empty list, as a list of nulls does; each is likelier a caller that meant
// to send nothing than one that meant to empty the room. So the tool empties
// a room for `[]` alone, and refuses every other value naming nobody with
// nothing written, saying how to empty it.
//
// Mutation: drop the tool's empty-list test, and "", " , " and [null] each
// empty the room, answered `applied`.
func TestOnlyTheEmptyListEmptiesARoom(t *testing.T) {
	t.Parallel()
	for name, members := range map[string]any{
		"a blank string":    "",
		"commas and spaces": " , ",
		"a list of nulls":   []any{nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeChat()
			tool := operatorChat(t, f, nil, nil)[chatlet.SetChannelMembersTool]
			got := invokeOperator(t, tool, map[string]any{"channel": chatRoomID,
				"members": members})
			if tools.RefusalOf(got) != tools.RefusalInvalid ||
				!strings.Contains(got.Output, "names nobody") || !strings.Contains(got.Output, "[]") {

				t.Errorf("members %#v answered %+v, want it refused as naming nobody, "+
					"saying how to empty the room", members, got)
			}
			if len(f.keys) != 0 || f.reads != 0 {
				t.Errorf("a membership naming nobody reached the store: %d writes, %d reads",
					len(f.keys), f.reads)
			}
		})
	}

	f := newFakeChat()
	tool := operatorChat(t, f, nil, nil)[chatlet.SetChannelMembersTool]
	if got := invokeOperator(t, tool, map[string]any{"channel": chatRoomID,
		"members": []any{}}); got.Failed {
		t.Fatalf("the empty list was refused: %s", got.Output)
	}
	if len(f.memberSets) != 1 || f.memberSets[0] == nil || len(f.memberSets[0]) != 0 {
		t.Errorf("the empty list was handed over as %#v, want the room emptied", f.memberSets)
	}
}

// A NULL IS NOTHING SENT, to a gesture as to the check at the surface
// (ADR-0059), so it changes nothing it names: the check admits it as absent,
// and read as named it cleared a room's topic, removed every link of a
// message and ended a do-not-disturb. The empty value is still the one that
// clears, which TestAPersonsChatGesturesReachTheStoreAsAsked and
// TestAPersonsReadStateIsWrittenAsStated hold. A null position in a flush's
// cursors is a room not named, so the rest of the flush is recorded, where
// it was refused whole after the check had admitted it.
//
// Mutation: read any of them as named whenever its key is present, and the
// change it makes is named here; read the cursors without dropping a null
// position, and the flush naming one is refused.
func TestANullIsNothingSent(t *testing.T) {
	t.Parallel()
	f := newFakeChat()
	var flushed []flush
	surface := operatorChat(t, f, fakeCursors{flushed: &flushed}, nil)

	if got := callOperator(t, surface[chatlet.UpdateChannelTool], map[string]any{
		"channel": chatRoomID, "topic": nil, "purpose": "ship it",
	}); got.Failed {
		t.Fatalf("update_channel failed: %s", got.Output)
	}
	if patch := f.patches[0]; patch.Topic != nil || patch.Purpose == nil {
		t.Errorf("a null topic beside a purpose was handed over as %+v, want the "+
			"purpose alone", patch)
	}

	if got := callOperator(t, surface[chatlet.EditMessageTool], map[string]any{
		"channel": chatRoomID, "message_id": "m1", "body": "fixed the typo",
		"links": nil,
	}); got.Failed {
		t.Fatalf("edit_message failed: %s", got.Output)
	}
	if edit := f.edits[0]; edit.Links != nil {
		t.Errorf("a null list of links was handed over as %v, which replaces them",
			*edit.Links)
	}

	if got := callOperator(t, surface[chatlet.MarkChatReadTool], map[string]any{
		"cursors": nil, "mute": []any{chatRoomID}, "dnd_until": nil,
	}); got.Failed {
		t.Fatalf("mark_chat_read failed: %s", got.Output)
	}
	if len(flushed) != 1 {
		t.Fatalf("%d flushes reached the store", len(flushed))
	}
	if d := flushed[0].delta; d.Cursors != nil || d.DNDUntil != nil {
		t.Errorf("a null cursor set and do-not-disturb were handed over as %+v", d)
	}

	// THROUGH THE CHECK, as the act route sends a flush: the check admits a
	// null position as a room not named, and so does the tool.
	flushed = nil
	if got := invokeOperator(t, surface[chatlet.MarkChatReadTool], map[string]any{
		"cursors": map[string]any{chatRoomID: 4096, "room-b": nil},
		"mute":    []any{"room-c"},
	}); got.Failed {
		t.Fatalf("a flush naming a null position was refused: %s", got.Output)
	}
	if len(flushed) != 1 {
		t.Fatalf("%d flushes reached the store", len(flushed))
	}
	if d := flushed[0].delta; len(d.Cursors) != 1 || d.Cursors[chatRoomID] != 4096 ||
		!slices.Equal(d.Mute, []string{"room-c"}) {

		t.Errorf("a null position beside a cursor and a mute was handed over as %+v, "+
			"want that cursor and that mute alone", d)
	}
}

// A GESTURE WHOSE OUTCOME IS UNKNOWN IS NEITHER DONE NOR FAILED.
//
// The settings, the members, the words and the tombstone are each STATED, so
// their repeat is offered as harmless. A room's create ADDS, so its answer
// says to look first — and why a second create is safe if the first landed.
func TestAPersonsChatGestureWhoseOutcomeIsUnknownSaysSo(t *testing.T) {
	t.Parallel()
	const op = "01a11a61-ff0c-77c1-8cb1-7b9e5497550e"
	for _, c := range []struct {
		tool string
		args map[string]any
		next string
	}{
		{chatlet.CreateChannelTool, map[string]any{"channel": "launch"},
			"the second is refused because the name is taken"},
		{chatlet.OpenDMTool, map[string]any{"to": []any{"bob"}}, "harmless"},
		{chatlet.UpdateChannelTool, map[string]any{"channel": chatRoomID, "topic": "t"}, "harmless"},
		{chatlet.SetChannelMembersTool, map[string]any{"channel": chatRoomID,
			"members": []any{"bob"}}, "harmless"},
		{chatlet.EditMessageTool, map[string]any{"channel": chatRoomID, "message_id": "m1",
			"body": "again"}, "harmless"},
		{chatlet.DeleteMessageTool, map[string]any{"channel": chatRoomID, "message_id": "m1"},
			"harmless"},
	} {
		t.Run(c.tool, func(t *testing.T) {
			t.Parallel()
			f := newFakeChat()
			outcome := statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: op}
			f.outcome, f.opened = &outcome, &outcome
			got := callOperator(t, operatorChat(t, f, nil, nil)[c.tool], c.args)
			if !tools.UnknownOf(got) || tools.RefusalOf(got) != tools.RefusalUnavailable {
				t.Fatalf("an unknown %s answered %+v, want the unknown answer", c.tool, got)
			}
			for _, want := range []string{"is unknown", op, c.next} {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the answer does not say %q: %s", want, got.Output)
				}
			}
		})
	}
}

// ---- mark_chat_read ---------------------------------------------------- //

// fakeCursors is a person's read-state store, recording every flush.
type fakeCursors struct {
	flushed *[]flush
	err     error
	state   coord.ChatReadState
}

type flush struct {
	handle string
	delta  coord.ChatReadDelta
}

func (f fakeCursors) AdvanceChatRead(_ context.Context, handle string,
	delta coord.ChatReadDelta) (coord.ChatReadState, error) {

	if f.flushed != nil {
		*f.flushed = append(*f.flushed, flush{handle: handle, delta: delta})
	}
	return f.state, f.err
}

// A PERSON'S READ STATE IS WRITTEN AS THEY STATED IT, AND ANSWERED AS THEIRS.
//
// Three facts travel and each has its own "absent": a cursor only moves
// forward, a mute and an unmute each name the ROOMS whose state changes — so a
// room neither names keeps its own, and muting one needs nothing read first —
// and an empty do-not-disturb ends it. A cursor is a packed log position whose
// generation sits above bit 40, so it is read exactly rather than through a
// float. The record is the caller's OWN, kept under the seat the call acts as,
// and the answer is that state, named by its person, applied at no position,
// and never the reconcile's own stamp.
func TestAPersonsReadStateIsWrittenAsStated(t *testing.T) {
	t.Parallel()
	var flushed []flush
	absent := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cursors := fakeCursors{flushed: &flushed, state: coord.ChatReadState{
		Cursors: map[string]int64{chatRoomID: 9007199254740993},
		Muted:   []string{chatRoomID, "room-noisy"}, AbsentSince: absent, Revision: 41,
	}}
	tool := operatorChat(t, newFakeChat(), cursors, nil)[chatlet.MarkChatReadTool]
	// THE WIRE'S OWN NUMBER, as the act transport decodes it: 2^53 + 1,
	// which a float cannot hold.
	got := callOperator(t, tool, map[string]any{
		"cursors":   map[string]any{chatRoomID: json.Number("9007199254740993")},
		"mute":      []any{chatRoomID},
		"unmute":    []any{"room-quiet"},
		"dnd_until": "",
	})
	if got.Failed {
		t.Fatalf("mark_chat_read failed: %s", got.Output)
	}
	if len(flushed) != 1 {
		t.Fatalf("%d flushes reached the store", len(flushed))
	}
	written := flushed[0]
	if written.handle != "jane" {
		t.Errorf("the read state was written for %q, want the bound person's seat", written.handle)
	}
	if at := written.delta.Cursors[chatRoomID]; at != 9007199254740993 {
		t.Errorf("the cursor arrived as %d — a packed position read through a float", at)
	}
	if !slices.Equal(written.delta.Mute, []string{chatRoomID}) ||
		!slices.Equal(written.delta.Unmute, []string{"room-quiet"}) {
		t.Errorf("the rooms arrived as mute %v and unmute %v, want [%s] and "+
			"[room-quiet] — exactly the rooms named, and no list", written.delta.Mute,
			written.delta.Unmute, chatRoomID)
	}
	if written.delta.DNDUntil == nil || !written.delta.DNDUntil.IsZero() {
		t.Errorf("an ended do-not-disturb arrived as %v", written.delta.DNDUntil)
	}
	if !written.delta.At.Equal(time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("the flush is stamped %s, not the surface's clock", written.delta.At)
	}
	var answer map[string]any
	decoder := json.NewDecoder(strings.NewReader(got.Output))
	decoder.UseNumber()
	if err := decoder.Decode(&answer); err != nil {
		t.Fatalf("the answer is not JSON: %v: %s", err, got.Output)
	}
	if answer["outcome"] != string(statelog.OutcomeApplied) || answer["position"] != nil {
		t.Errorf("a written read state answered outcome %v at %v, want applied at "+
			"no position — it is on no log", answer["outcome"], answer["position"])
	}
	read, ok := answer["read"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no read state: %s", got.Output)
	}
	// NAMED BY ITS PERSON, as the push and the rail's `read` are: the screen
	// that flushed applies this, and a state that did not say whose it is
	// could not be told from the one a tab held for whoever its credential
	// named before.
	if read["handle"] != "jane" {
		t.Errorf("the answer's read state names %v, want the bound person: %s",
			read["handle"], got.Output)
	}
	held, _ := read["cursors"].(map[string]any)
	if at, _ := held[chatRoomID].(json.Number); at.String() != "9007199254740993" {
		t.Errorf("the answer's cursor is %v: %s", held[chatRoomID], got.Output)
	}
	// THE WHOLE SET the record now holds, which the person named one room
	// of: the answer is the state, not an echo of the call.
	if muted, ok := read["muted"].([]any); !ok || len(muted) != 2 ||
		muted[0] != chatRoomID || muted[1] != "room-noisy" {

		t.Errorf("the answer's muted set is %#v: %s", read["muted"], got.Output)
	}
	// AND THE REVISION THE RECORD STANDS AT, which is how the screen that
	// flushed orders this answer against the push the same flush raises.
	if revision, _ := read["revision"].(json.Number); revision.String() != "41" {
		t.Errorf("the answer's revision is %v, want the record's: %s",
			read["revision"], got.Output)
	}
	if strings.Contains(got.Output, "absent") {
		t.Errorf("the answer carries the reconcile's absence stamp: %s", got.Output)
	}

	// AND WHAT IS NOT NAMED IS LEFT ALONE.
	flushed = nil
	if got := callOperator(t, tool, map[string]any{
		"cursors": map[string]any{chatRoomID: 12.0},
	}); got.Failed {
		t.Fatalf("a cursor alone failed: %s", got.Output)
	}
	if d := flushed[0].delta; d.Mute != nil || d.Unmute != nil || d.DNDUntil != nil {
		t.Errorf("a flush naming only a cursor changed the rest: %+v", d)
	}
}

// A MUTE NAMES ONE ROOM AND KEEPS EVERY OTHER, through the real merge.
//
// The screen's toggle sends the one room it is on. A tool that sent the list —
// or a merge that took what it was sent as the list — would have a person who
// muted one room find every other room they had muted unmuted, and a fresh tab
// that has seen no state could not mute anything without reading the list
// first.
func TestAMuteNamesOneRoomAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	tool := operatorChat(t, newFakeChat(), coordmem.NewFleet(), nil)[chatlet.MarkChatReadTool]
	mutedAfter := func(args map[string]any) []any {
		t.Helper()
		got := callOperator(t, tool, args)
		if got.Failed {
			t.Fatalf("mark_chat_read %v failed: %s", args, got.Output)
		}
		var answer struct {
			Read struct {
				Muted []any `json:"muted"`
			} `json:"read"`
		}
		if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
			t.Fatalf("the answer is not JSON: %v: %s", err, got.Output)
		}
		return answer.Read.Muted
	}
	mutedAfter(map[string]any{"mute": []any{"room-a"}})
	if got := mutedAfter(map[string]any{"mute": []any{"room-b"}}); len(got) != 2 {
		t.Errorf("muting room-b after room-a leaves %v, want both", got)
	}
	got := mutedAfter(map[string]any{"unmute": []any{"room-a"}})
	if len(got) != 1 || got[0] != "room-b" {
		t.Errorf("unmuting room-a leaves %v, want room-b alone", got)
	}
	// AND A CALL ABOUT SOMETHING ELSE LEAVES THE SET AS IT IS.
	if got := mutedAfter(map[string]any{"cursors": map[string]any{"room-a": 3.0}}); len(got) != 1 {
		t.Errorf("a cursor flush left the mutes as %v, want room-b still muted", got)
	}
}

// THE READER'S MARKS RIDE THE SAME RECORD, each named alone, through the real
// merge: a room's level, a star, a save, a thread follow and a thread's own
// place all land in one call and come back in the answer the screen applies,
// and a later call about one of them leaves the rest as they were.
//
// Mutation: drop any of the new arguments from the delta and its field comes
// back empty.
func TestTheReadersMarksAreWrittenAsStated(t *testing.T) {
	t.Parallel()
	tool := operatorChat(t, newFakeChat(), coordmem.NewFleet(), nil)[chatlet.MarkChatReadTool]
	type read struct {
		ThreadCursors map[string]float64 `json:"thread_cursors"`
		Every         []string           `json:"every"`
		Starred       []string           `json:"starred"`
		Saved         []struct {
			ChannelID string `json:"channel_id"`
			MessageID string `json:"message_id"`
		} `json:"saved"`
		Threads []struct {
			RootID    string `json:"root_id"`
			Following bool   `json:"following"`
		} `json:"threads"`
	}
	after := func(args map[string]any) read {
		t.Helper()
		got := callOperator(t, tool, args)
		if got.Failed {
			t.Fatalf("mark_chat_read %v failed: %s", args, got.Output)
		}
		var answer struct {
			Read read `json:"read"`
		}
		if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
			t.Fatalf("the answer is not JSON: %v: %s", err, got.Output)
		}
		return answer.Read
	}
	got := after(map[string]any{
		"notify":         map[string]any{"room-a": "all"},
		"star":           []any{"room-b"},
		"save":           []any{map[string]any{"channel_id": "room-a", "message_id": "m-1"}},
		"unfollow":       []any{map[string]any{"channel_id": "room-a", "root_id": "r-1"}},
		"thread_cursors": map[string]any{"r-2": 40.0},
	})
	switch {
	case len(got.Every) != 1 || got.Every[0] != "room-a":
		t.Errorf("notify all left every %v", got.Every)
	case len(got.Starred) != 1 || got.Starred[0] != "room-b":
		t.Errorf("a star left %v", got.Starred)
	case len(got.Saved) != 1 || got.Saved[0].MessageID != "m-1":
		t.Errorf("a save left %+v", got.Saved)
	case len(got.Threads) != 1 || got.Threads[0].RootID != "r-1" || got.Threads[0].Following:
		t.Errorf("an unfollow left %+v", got.Threads)
	case got.ThreadCursors["r-2"] != 40:
		t.Errorf("a thread cursor left %v", got.ThreadCursors)
	}
	got = after(map[string]any{"unstar": []any{"room-b"}})
	if len(got.Starred) != 0 || len(got.Saved) != 1 || len(got.Every) != 1 || len(got.Threads) != 1 {
		t.Errorf("an unstar disturbed the other marks: %+v", got)
	}
}

// A READ STATE THAT CANNOT BE READ IS REFUSED, and reaches nothing — and a
// store that could not be written is the node's, never the person's input.
func TestAReadStateThatCannotBeWrittenIsRefusedWithItsClass(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		args map[string]any
		says string
	}{
		"a cursor that is not a position": {map[string]any{
			"cursors": map[string]any{chatRoomID: "yesterday"}},
			"whole, non-negative log position"},
		"a cursor below zero": {map[string]any{
			"cursors": map[string]any{chatRoomID: -1.0}},
			"whole, non-negative log position"},
		"cursors that are not an object": {map[string]any{"cursors": []any{1}},
			"`cursors` is an object of id"},
		"a level this build does not know": {map[string]any{
			"notify": map[string]any{chatRoomID: "loud"}}, "`all`, `mentions` or `muted`"},
		"a save that names no message": {map[string]any{
			"save": []any{map[string]any{"channel_id": chatRoomID}}},
			"`save` entry 0 needs a `channel_id` and a `message_id`"},
		"a follow that is no list": {map[string]any{"follow": 3.0},
			"`follow` is a list"},
		"a thread cursor below zero": {map[string]any{
			"thread_cursors": map[string]any{"root-1": -4.0}},
			"`thread_cursors` entry"},
		"cursors naming no room": {map[string]any{"cursors": map[string]any{}},
			"names nothing"},
		"cursors whose every position is null": {map[string]any{
			"cursors": map[string]any{chatRoomID: nil}}, "names nothing"},
		"a do-not-disturb that is no instant": {map[string]any{"dnd_until": "tonight"},
			"RFC 3339"},
		"nothing named": {map[string]any{}, "names nothing"},
		"only blank rooms named": {map[string]any{"mute": []any{" "}, "unmute": []any{""}},
			"names nothing"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var flushed []flush
			got := callOperator(t, operatorChat(t, newFakeChat(),
				fakeCursors{flushed: &flushed}, nil)[chatlet.MarkChatReadTool], c.args)
			if tools.RefusalOf(got) != tools.RefusalInvalid || !strings.Contains(got.Output, c.says) {
				t.Errorf("answered %+v, want invalid saying %q", got, c.says)
			}
			if len(flushed) != 0 {
				t.Errorf("a refused flush reached the store: %+v", flushed)
			}
		})
	}

	got := callOperator(t, operatorChat(t, newFakeChat(), fakeCursors{
		err: fmt.Errorf("%w: lost every compare-and-swap", coord.ErrUnavailable),
	}, nil)[chatlet.MarkChatReadTool], map[string]any{"mute": []any{chatRoomID}})
	if tools.RefusalOf(got) != tools.RefusalUnavailable || tools.UnknownOf(got) ||
		!strings.Contains(got.Output, "nothing was recorded") {

		t.Errorf("a store that took no write answered %+v, want the node's class "+
			"saying nothing was recorded", got)
	}

	// A FLUSH THE MERGE REFUSED is the caller's to change, the same way
	// every time it is sent — so it is invalid, not the store's condition,
	// and told in the merge's own words rather than behind the Go package's
	// prefix, which says "refused" a second time inside a refusal.
	//
	// Mutation: drop the ErrInvalidReadState arm and it reads as the store
	// that could not take it, `unavailable`.
	merge := fmt.Errorf("%w: room %s is both muted and unmuted in one flush",
		coord.ErrInvalidReadState, chatRoomID)
	got = callOperator(t, operatorChat(t, newFakeChat(), fakeCursors{err: merge},
		nil)[chatlet.MarkChatReadTool], map[string]any{"mute": []any{chatRoomID}})
	switch {
	case tools.RefusalOf(got) != tools.RefusalInvalid || tools.UnknownOf(got):
		t.Errorf("a flush the merge refused answered %+v, want invalid", got)
	case !strings.Contains(got.Output, coord.ReadStateSentence(merge)),
		!strings.Contains(got.Output, "Nothing was recorded"):
		t.Errorf("a flush the merge refused answered %q, want the merge's own "+
			"words and that nothing was recorded", got.Output)
	case strings.Contains(got.Output, coord.ErrInvalidReadState.Error()):
		t.Errorf("a flush the merge refused answered %q, which carries the Go "+
			"package's prefix", got.Output)
	}
}

// A FLUSH WHOSE ANSWER WAS LOST MAY HAVE LANDED, AND IS NEVER REPORTED AS ONE
// THAT DID NOT.
//
// The read state is a compare-and-set in the coordination store, and a put
// whose reply never came is [coord.ErrWriteUnknown]: answering it "nothing
// was recorded" tells a person whose badge cleared that it did not, and the
// act transport renders that as a refusal the dashboard retries as one. It is
// `unknown` — and it says the repeat is safe, because the merge is idempotent.
//
// Mutation: drop the ErrWriteUnknown arm and it reads as "nothing was
// recorded".
func TestAFlushWhoseAnswerWasLostIsUnknown(t *testing.T) {
	t.Parallel()
	got := callOperator(t, operatorChat(t, newFakeChat(), fakeCursors{
		err: fmt.Errorf("%w: %w: advance the chat read state for jane: nats: timeout",
			coord.ErrUnavailable, coord.ErrWriteUnknown),
	}, nil)[chatlet.MarkChatReadTool], map[string]any{"cursors": map[string]any{chatRoomID: 12.0}})
	if !tools.UnknownOf(got) || tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Fatalf("a flush whose answer was lost answered %+v, want unknown", got)
	}
	if strings.Contains(got.Output, "nothing was recorded") {
		t.Errorf("a flush that may have landed was told it was not recorded: %s", got.Output)
	}
	for _, want := range []string{"may have been recorded", "again is safe"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the answer does not say %q: %s", want, got.Output)
		}
	}

	// AND A FLUSH THE STORE'S OWN MERGE REFUSES IS THE CALLER'S TO CHANGE.
	// One room past the mute cap is refused the same way however often it
	// is sent, so "try again" — the node's class — is advice nobody can
	// follow. Through the real merge, over the coordination store's twin.
	muted := make([]any, coord.MaxReadCursors+1)
	for i := range muted {
		muted[i] = fmt.Sprintf("room-%d", i)
	}
	got = callOperator(t, operatorChat(t, newFakeChat(), coordmem.NewFleet(), nil)[chatlet.MarkChatReadTool],
		map[string]any{"mute": muted})
	if tools.RefusalOf(got) != tools.RefusalInvalid ||
		!strings.Contains(got.Output, fmt.Sprint(coord.MaxReadCursors)) {

		t.Errorf("a mute past the cap answered %+v, want invalid naming the cap", got)
	}
	both := callOperator(t, operatorChat(t, newFakeChat(), coordmem.NewFleet(), nil)[chatlet.MarkChatReadTool],
		map[string]any{"mute": []any{chatRoomID}, "unmute": []any{chatRoomID}})
	if tools.RefusalOf(both) != tools.RefusalInvalid ||
		!strings.Contains(both.Output, "both muted and unmuted") {

		t.Errorf("a room both muted and unmuted answered %+v, want invalid saying so", both)
	}
	if strings.Contains(got.Output, "try again") {
		t.Errorf("a refused flush told its caller to try again: %s", got.Output)
	}
	// AND IN THE MERGE'S OWN WORDS: this answer is read by the model that
	// made the call for the person who muted those rooms, and a Go
	// package's prefix — with a second "refused" inside the first — is not
	// an instruction anybody can follow.
	if strings.Contains(got.Output, "coord:") ||
		strings.Count(got.Output, "refused") != 1 {

		t.Errorf("a refused flush reads to its sender as: %s", got.Output)
	}
}

// A CURSOR IS A PERSON'S. An agent is woken rather than reading, and a
// surface that handed an agent's identity to this tool would be recording
// attention nobody paid — refused by the table first (the verb is a person's)
// and by the tool itself for a surface wired without the table, because the
// coordination store keys a read state by whatever handle it is given.
//
// Mutation: drop the tool's IsHuman check and the agent's read state is
// written behind an allow-all decision.
func TestAnAgentHasNoReadStateToRecord(t *testing.T) {
	t.Parallel()
	var flushed []flush
	deps := chatDeps(newFakeChat())
	deps.Cursors = fakeCursors{flushed: &flushed}
	deps.Actor = func(context.Context, *turnctx.Turn) (chatlet.Actor, error) {
		return chatlet.Actor{Handle: "agent-cto", Kind: chatlet.AuthorAgent}, nil
	}
	allowAll := func(context.Context, authz.Action, authz.Object) error { return nil }
	var tool tools.Callable
	for _, candidate := range builtin.OperatorTools(builtin.OperatorDeps{
		Chat: deps, Authorize: allowAll,
	}) {
		if candidate.Name() == chatlet.MarkChatReadTool {
			tool = candidate
		}
	}
	got := callOperator(t, tool, map[string]any{"mute": []any{chatRoomID}})
	if tools.RefusalOf(got) != tools.RefusalForbidden {
		t.Errorf("an agent's read state answered %+v, want forbidden", got)
	}
	if len(flushed) != 0 {
		t.Errorf("an agent's read state was written: %+v", flushed)
	}
}
