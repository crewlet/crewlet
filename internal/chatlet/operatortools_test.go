package chatlet_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
)

// A PERSON'S CHAT GESTURES LAND AS THE STORE RULES THEM — end to end, from the
// operator catalogue's tool to the rows.
//
// The dashboard makes every change through these tools (ADR-0024), and each
// one is a translation: arguments into the store's shape, a request into the
// call's key, the table's decision into the authority the store is handed,
// and a store refusal into a class a person's screen acts on. Nothing here is
// mocked but the transport — which is one line: it puts the request's key on
// the actor — so a field the tool drops, a key it carries wrongly, an
// authority it hands over that the table did not decide, or a rule it states
// that the store does not keep shows up as rows that disagree with the answer.
func TestAPersonsChatGesturesLandAsTheStoreRulesThem(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob", "ana"))
	// JANE AND ANA HOLD CHAT AND NOTHING ELSE, so what either does here is
	// theirs only by the table's author path; BOB HOLDS ADMIN too, so his
	// take-down of somebody's words is moderation.
	jane := newOperatorTools(t, r, "jane", iam.GrantChatlet)
	ana := newOperatorTools(t, r, "ana", iam.GrantChatlet)
	bob := newOperatorTools(t, r, "bob", iam.GrantChatlet, iam.GrantAdmin)

	// A PRIVATE ROOM, MADE ONCE however often the request is sent.
	made := jane.do(chatlet.CreateChannelTool, "req-create", map[string]any{
		"channel": "launch", "kind": "private", "members": []any{"bob", "ana"},
	})
	room := made["channel"].(string)
	again := jane.do(chatlet.CreateChannelTool, "req-create", map[string]any{
		"channel": "launch", "kind": "private", "members": []any{"bob", "ana"},
	})
	if again["channel"] != room {
		t.Fatalf("the same create request answered room %v and then %v", room, again["channel"])
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE name = 'launch'`); got != 1 {
		t.Fatalf("one create request sent twice left %d rooms", got)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_members WHERE channel_id = ?`, room); got != 3 {
		t.Fatalf("the room holds %d members, want its maker, bob and ana", got)
	}

	// SAID, REWRITTEN BY ITS AUTHOR, AND REFUSED TO ANYBODY ELSE — admin
	// included, whom the table admits and the store does not: an edit puts
	// words in somebody's mouth, so there is no moderator's edit.
	said := jane.do(chatlet.PostMessageTool, "req-post", map[string]any{
		"channel": room, "body": "we ship on friday",
	})["message_id"].(string)
	jane.do(chatlet.EditMessageTool, "req-edit", map[string]any{
		"channel": room, "message_id": said, "body": "we ship on monday",
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND body = ?`,
		said, "we ship on monday"); got != 1 {
		t.Fatal("the author's edit did not land")
	}
	if got := bob.refused(chatlet.EditMessageTool, "req-bob-edit", map[string]any{
		"channel": room, "message_id": said, "body": "we ship never",
	}); tools.RefusalOf(got) != tools.RefusalForbidden {
		t.Errorf("somebody else's edit answered %s (%s), want forbidden", tools.RefusalOf(got), got.Output)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND body = ?`,
		said, "we ship on monday"); got != 1 {
		t.Fatal("a refused edit rewrote the message")
	}

	// A MEMBER WHO IS NEITHER ITS AUTHOR NOR ADMIN is refused a take-down,
	// decided on whose words they are once the message is read.
	if got := ana.refused(chatlet.DeleteMessageTool, "req-ana-delete", map[string]any{
		"channel": room, "message_id": said,
	}); tools.RefusalOf(got) != tools.RefusalForbidden {
		t.Errorf("a take-down by a member who did not write it answered %s (%s), "+
			"want forbidden", tools.RefusalOf(got), got.Output)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ?
		AND deleted_at IS NOT NULL`, said); got != 0 {
		t.Fatal("a refused take-down took the message down")
	}

	// THE MEMBERSHIP STATED WHOLE: bob and ana are taken out by being left
	// out.
	jane.do(chatlet.SetChannelMembersTool, "req-members", map[string]any{
		"channel": room, "members": []any{"jane"},
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_members WHERE channel_id = ?
		AND handle IN ('bob', 'ana')`, room); got != 0 {
		t.Fatal("a membership stated without bob and ana left them in the room")
	}

	// AND MODERATION IS NOT BOUNDED BY MEMBERSHIP: bob, out of this private
	// room now, still takes a message in it down. It is the one gesture
	// that reaches past a private room's membership — decided on the
	// message nobody has read, so the moderator is never shown it — and the
	// row stays, so a thread hung off it keeps its root.
	bob.do(chatlet.DeleteMessageTool, "req-delete", map[string]any{
		"channel": room, "message_id": said,
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ?
		AND deleted_at IS NOT NULL`, said); got != 1 {
		t.Fatal("an admin outside a private room could not take a message in it " +
			"down, or its row did not survive")
	}

	// ARCHIVED BY ITS CREATOR, and then a room that takes nothing more — the
	// argument's to fix, by saying it somewhere else.
	jane.do(chatlet.UpdateChannelTool, "req-archive", map[string]any{
		"channel": room, "archived": true,
	})
	if got := jane.refused(chatlet.PostMessageTool, "req-late", map[string]any{
		"channel": room, "body": "anybody there?",
	}); tools.RefusalOf(got) != tools.RefusalInvalid {
		t.Errorf("a post into an archived room answered %s (%s), want invalid", tools.RefusalOf(got), got.Output)
	}
}

// operatorTools is the operator catalogue's chat over a real store, acting as
// one person bound to the human seat named.
type operatorTools struct {
	t      *testing.T
	r      *writeRound
	handle string
	caller context.Context
	tools  map[string]tools.Callable
}

// requestOn is the context key a request's key travels under here, from the
// call to the actor — the one thing the act transport adds.
type requestOn struct{}

func newOperatorTools(t *testing.T, r *writeRound, handle string,
	grants ...iam.Grant) operatorTools {

	t.Helper()
	out := operatorTools{
		t: t, r: r, handle: handle, tools: map[string]tools.Callable{},
		caller: iam.WithPrincipal(t.Context(), iam.Principal{
			ID:   uuid.NewSHA1(uuid.NameSpaceOID, []byte(handle)),
			Kind: iam.KindPerson, Login: handle + ".person", Seat: handle,
			Stage: iam.StageActive, Grants: grants,
		}),
	}
	actor := func(ctx context.Context, _ *turnctx.Turn) (chatlet.Actor, error) {
		key, _ := ctx.Value(requestOn{}).(string)
		return chatlet.Actor{Handle: handle, Kind: chatlet.AuthorHuman,
			OperatorID: "pat:" + handle, OpKey: key}, nil
	}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Chat: builtin.ChatDeps{
			Writer: r.store, Reader: settledReads{reader(t, r)}, Actor: actor,
		},
		Authorize: builtin.Decide(authz.NoChart{}),
	}) {
		out.tools[tool.Name()] = tool
	}
	return out
}

// call makes one call under a request, as the act transport does, and applies
// what it wrote.
func (o operatorTools) call(tool, request string, args map[string]any) tools.Result {
	o.t.Helper()
	callable := o.tools[tool]
	if callable == nil {
		o.t.Fatalf("the operator catalogue serves no %s", tool)
	}
	ctx := context.WithValue(o.caller, requestOn{}, requested(o.handle+"/"+request))
	got, err := callable.Call(ctx, args)
	if err != nil {
		o.t.Fatalf("%s: %v", tool, err)
	}
	o.r.drain()
	return got
}

// do makes a call that must land, and answers its receipt.
func (o operatorTools) do(tool, request string, args map[string]any) map[string]any {
	o.t.Helper()
	got := o.call(tool, request, args)
	if got.Failed {
		o.t.Fatalf("%s %v: %s", tool, args, got.Output)
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(got.Output), &receipt); err != nil {
		o.t.Fatalf("%s answered %q: %v", tool, got.Output, err)
	}
	return receipt
}

// refused makes a call that must be refused, and answers the refusal.
func (o operatorTools) refused(tool, request string, args map[string]any) tools.Result {
	o.t.Helper()
	got := o.call(tool, request, args)
	if !got.Failed {
		o.t.Fatalf("%s %v landed: %s", tool, args, got.Output)
	}
	return got
}
