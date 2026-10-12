package chat_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// A PERSON'S CHAT GESTURES LAND AS THE STORE RULES THEM — end to end, from the
// operator catalogue's tool to the rows.
//
// The dashboard makes every change through these tools (ADR-0024), and each
// one is a translation: arguments into the store's shape, a request into the
// call's key, a store refusal into a class a person's screen acts on. Nothing
// here is mocked but the act transport — which is one line: it derives the
// call's operation from the request id — so a field the tool drops, a key it
// derives wrongly or a rule it states that the store does not keep shows up as
// rows that disagree with the answer.
func TestAPersonsChatGesturesLandAsTheStoreRulesThem(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane", "bob"))
	jane := newOperatorTools(t, r, operatorFor("jane"))

	// A PRIVATE ROOM, MADE ONCE however often the request is sent.
	made := jane.do(chat.CreateChannelTool, "req-create", map[string]any{
		"name": "launch", "kind": "private", "members": []any{"bob"},
	})
	room := made["channel"].(string)
	again := jane.do(chat.CreateChannelTool, "req-create", map[string]any{
		"name": "launch", "kind": "private", "members": []any{"bob"},
	})
	if again["channel"] != room {
		t.Fatalf("the same create request answered room %v and then %v", room, again["channel"])
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_channels WHERE name = 'launch'`); got != 1 {
		t.Fatalf("one create request sent twice left %d rooms", got)
	}
	if got := r.count(`SELECT COUNT(*) FROM chat_members WHERE channel_id = ?`, room); got != 2 {
		t.Fatalf("the room holds %d members, want its maker and bob", got)
	}

	// A UNIT'S ROOM IS THE CHART'S, and the tool says so in the class a
	// person's screen prints as written.
	if got := jane.refused(chat.CreateChannelTool, "req-unit", map[string]any{
		"name": "engineering", "kind": "unit",
	}); got.Refusal != tools.RefusalForbidden {
		t.Errorf("a hand-made unit room answered %+v, want forbidden", got)
	}

	// SAID, REWRITTEN BY ITS AUTHOR, AND REFUSED TO ANYBODY ELSE.
	said := jane.do(chat.PostMessageTool, "req-post", map[string]any{
		"channel": room, "body": "we ship on friday",
	})["message_id"].(string)
	jane.do(chat.EditMessageTool, "req-edit", map[string]any{
		"channel": room, "message": said, "body": "we ship on monday",
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ? AND body = ?`,
		said, "we ship on monday"); got != 1 {
		t.Fatal("the author's edit did not land")
	}
	bob := newOperatorTools(t, r, operatorFor("bob"))
	if got := bob.refused(chat.EditMessageTool, "req-bob-edit", map[string]any{
		"channel": room, "message": said, "body": "we ship never",
	}); got.Refusal != tools.RefusalForbidden {
		t.Errorf("somebody else's edit answered %+v, want forbidden", got)
	}

	// TAKEN BACK, by an operator who did not write it — the store's rule
	// for moderation — and the row stays.
	bob.do(chat.DeleteMessageTool, "req-delete", map[string]any{
		"channel": room, "message": said,
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ?
		AND deleted_at IS NOT NULL`, said); got != 1 {
		t.Fatal("the message was not taken back, or its row did not survive")
	}

	// THE MEMBERSHIP STATED WHOLE: bob is taken out by being left out.
	jane.do(chat.SetChannelMembersTool, "req-members", map[string]any{
		"channel": room, "members": []any{"jane"},
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_members WHERE channel_id = ?
		AND handle = 'bob'`, room); got != 0 {
		t.Fatal("a membership stated without bob left bob in the room")
	}

	// AND MODERATION IS NOT BOUNDED BY MEMBERSHIP: bob, out of this private
	// room now, still takes a message in it down. It is the one gesture
	// that reaches past a private room's membership — the authority an
	// operator credential holds over the company's history, as an erase
	// and a prune do — so the store's reachable rule must not gate it.
	later := jane.do(chat.PostMessageTool, "req-post-later", map[string]any{
		"channel": room, "body": "the launch date stays between us",
	})["message_id"].(string)
	bob.do(chat.DeleteMessageTool, "req-delete-later", map[string]any{
		"channel": room, "message": later,
	})
	if got := r.count(`SELECT COUNT(*) FROM chat_messages WHERE id = ?
		AND deleted_at IS NOT NULL`, later); got != 1 {
		t.Fatal("an operator outside a private room could not take a message in it down")
	}

	// ARCHIVED, and then a room that takes nothing more — the argument's to
	// fix, by saying it somewhere else.
	jane.do(chat.UpdateChannelTool, "req-archive", map[string]any{
		"channel": room, "archived": true,
	})
	if got := jane.refused(chat.PostMessageTool, "req-late", map[string]any{
		"channel": room, "body": "anybody there?",
	}); got.Refusal != tools.RefusalInvalid {
		t.Errorf("a post into an archived room answered %+v, want invalid", got)
	}
}

// operatorTools is the operator catalogue's chat over a real store, acting as
// one person.
type operatorTools struct {
	t     *testing.T
	r     *writeRound
	actor chat.Actor
	tools map[string]tools.Callable
}

// operationOn is the context key the act transport's operation travels under
// here — the shape operator.WithOperation gives it.
type operationOn struct{}

func newOperatorTools(t *testing.T, r *writeRound, actor chat.Actor) operatorTools {
	t.Helper()
	authority, err := statelogtest.LocalReaderOver(chat.Domain{}, r.db, r.waiter)
	if err != nil {
		t.Fatalf("build the read authority: %v", err)
	}
	reader, err := chat.NewReader(chat.ReaderOptions{
		Log: authority, Committed: r.waiter.Committed,
	})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	out := operatorTools{t: t, r: r, actor: actor, tools: map[string]tools.Callable{}}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Chat: builtin.ChatDeps{
		Writer: r.store, Reader: settledReads{reader},
		Actor: func(context.Context, *turnctx.Turn) (chat.Actor, error) { return actor, nil },
		Operation: func(ctx context.Context) string {
			op, _ := ctx.Value(operationOn{}).(string)
			return op
		},
	}}) {
		out.tools[tool.Name()] = tool
	}
	return out
}

// call makes one call under a request, as the act transport does, and applies
// what it wrote.
func (o operatorTools) call(tool, request string, args map[string]any) tools.Result {
	o.t.Helper()
	op, err := builtin.RequestOperation(o.actor.OperatorID,
		fmt.Sprintf("0192a1b2-c3d4-7e5f-8a6b-%012x", requestNumber(request)), tool, args)
	if err != nil {
		o.t.Fatalf("RequestOperation: %v", err)
	}
	got, err := o.tools[tool].Call(context.WithValue(o.t.Context(), operationOn{}, op), args)
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

// requestNumber is a stable number for a request's name, so every request a
// case names is one UUIDv7 and the same name is the same request.
func requestNumber(name string) uint64 {
	var n uint64
	for _, c := range name {
		n = n*131 + uint64(c)
	}
	return n % (1 << 48)
}
