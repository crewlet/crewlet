package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// room is the one room these cases speak in, named by its id so a gesture
// reaches the writer without a name to resolve.
const room = "0b8d6a50-9c1e-4c1f-8f43-1b7c2f6c2a11"

// roomPurpose is the purpose somebody gave the room before these cases, which
// a gesture that names none leaves standing.
const roomPurpose = "Ship the launch on time."

// chatDesk is chat as the operator surface reaches it — the router's rooms,
// writer and search, and a person's read state — recording who every gesture
// was made as and every flush it was asked for. Every write answers applied at
// a position, as the real writer does; every read answers an empty room.
type chatDesk struct {
	mu      sync.Mutex
	actors  []chatlet.Actor
	bodies  []string
	flushes []string
}

func (d *chatDesk) wrote(actor chatlet.Actor) (chatlet.Written, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.actors = append(d.actors, actor)
	return chatlet.Written{
		ChangeID: "msg-1", Revision: 3,
		Outcome: statelog.Result{Outcome: statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "CREWLET_CHAT_LOG", Generation: 1, Seq: 12}},
	}, nil
}

// made is every gesture the desk was asked for, in order.
func (d *chatDesk) made() []chatlet.Actor {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.actors)
}

func (d *chatDesk) Channels(context.Context, string, chatlet.ChannelsQuery,
	statelog.Freshness) (chatlet.ChannelListing, error) {
	return chatlet.ChannelListing{}, nil
}

func (d *chatDesk) Channel(context.Context, string, string,
	statelog.Freshness) (chatlet.ChannelDetail, error) {
	return chatlet.ChannelDetail{}, chatlet.ErrNotFound
}

func (d *chatDesk) ChannelNamed(context.Context, string, string,
	statelog.Freshness) (chatlet.ChannelDetail, error) {
	return chatlet.ChannelDetail{}, chatlet.ErrNotFound
}

func (d *chatDesk) Messages(context.Context, string, chatlet.TranscriptQuery,
	statelog.Freshness) (chatlet.Transcript, error) {
	return chatlet.Transcript{}, nil
}

func (d *chatDesk) Thread(context.Context, string, chatlet.ThreadQuery,
	statelog.Freshness) (chatlet.Thread, error) {
	return chatlet.Thread{}, chatlet.ErrNotFound
}

func (d *chatDesk) Post(_ context.Context, actor chatlet.Actor, _ string,
	in chatlet.NewMessage) (chatlet.Written, error) {
	d.mu.Lock()
	d.bodies = append(d.bodies, in.Body)
	d.mu.Unlock()
	return d.wrote(actor)
}

func (d *chatDesk) Reply(_ context.Context, actor chatlet.Actor, _, _ string,
	_ chatlet.NewMessage) (chatlet.Written, error) {
	return d.wrote(actor)
}

func (d *chatDesk) OpenDirect(_ context.Context, actor chatlet.Actor, _ []string) (
	chatlet.Written, bool, error) {
	w, err := d.wrote(actor)
	return w, true, err
}

func (d *chatDesk) React(_ context.Context, actor chatlet.Actor, _, _, _ string) (
	chatlet.Written, error) {
	return d.wrote(actor)
}

func (d *chatDesk) Unreact(_ context.Context, actor chatlet.Actor, _, _, _ string) (
	chatlet.Written, error) {
	return d.wrote(actor)
}

func (d *chatDesk) Join(_ context.Context, actor chatlet.Actor, _ string) (chatlet.Written, error) {
	return d.wrote(actor)
}

func (d *chatDesk) Leave(_ context.Context, actor chatlet.Actor, _ string) (chatlet.Written, error) {
	return d.wrote(actor)
}

func (d *chatDesk) CreateChannel(_ context.Context, actor chatlet.Actor,
	_ chatlet.NewChannel) (chatlet.Written, error) {
	return d.wrote(actor)
}

// PatchChannel answers the room as the patch leaves it: the settings the call
// named, and [roomPurpose] where it named no purpose.
func (d *chatDesk) PatchChannel(_ context.Context, actor chatlet.Actor, id string,
	patch chatlet.ChannelPatch, _ chatlet.ChannelAuthority) (chatlet.Written, error) {
	written, err := d.wrote(actor)
	written.Channel = chatlet.Channel{ID: id, Name: "launch", Kind: chatlet.KindPublic,
		Purpose: roomPurpose}
	if patch.Topic != nil {
		written.Channel.Topic = *patch.Topic
	}
	if patch.Purpose != nil {
		written.Channel.Purpose = *patch.Purpose
	}
	return written, err
}

func (d *chatDesk) SetMembers(_ context.Context, actor chatlet.Actor, _ string,
	_ []chatlet.Member) (chatlet.Written, error) {
	return d.wrote(actor)
}

func (d *chatDesk) AddMembers(_ context.Context, actor chatlet.Actor, _ string,
	handles []string) (chatlet.Written, []string, error) {
	written, err := d.wrote(actor)
	return written, handles, err
}

func (d *chatDesk) Edit(_ context.Context, actor chatlet.Actor, _, _ string,
	_ chatlet.EditMessage) (chatlet.Written, error) {
	return d.wrote(actor)
}

func (d *chatDesk) Delete(_ context.Context, actor chatlet.Actor, _, _ string,
	_ chatlet.MessageAuthority) (chatlet.Written, error) {
	return d.wrote(actor)
}

func (d *chatDesk) Search(context.Context, estate.ChatQuery) (estate.ChatSearchAnswer, error) {
	return estate.ChatSearchAnswer{}, nil
}

func (d *chatDesk) AdvanceChatRead(_ context.Context, handle string,
	_ coord.ChatReadDelta) (coord.ChatReadState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushes = append(d.flushes, handle)
	return coord.ChatReadState{Revision: 1}, nil
}

func (d *chatDesk) readState(context.Context, string) (coord.ChatReadState, error) {
	return coord.ChatReadState{}, nil
}

// half is the desk as the operator's chat half, every seam wired.
func (d *chatDesk) half() builtin.ChatDeps {
	return builtin.ChatDeps{
		Reader: d, Writer: d, Search: d, Cursors: d, ReadState: d.readState,
	}
}

// chatSurface is the operator surface serving chat alone, over desk.
func chatSurface(t *testing.T, desk *chatDesk, audit *auditLog) *operator.Server {
	t.Helper()
	opts := operator.Options{
		Halves: fixed(operator.Halves{Chat: desk.half()}), Org: company,
		Authorize: builtin.Decide(leadsNobody{}),
	}
	if audit != nil {
		opts.Audit = audit
	}
	return newSurface(t, opts)
}

// postBody is one post_message call as the act transport's body.
func postBody(text string) string {
	raw, _ := json.Marshal(map[string]any{"args": map[string]any{
		"channel": room, "body": text,
	}})
	return string(raw)
}

// A CHAT GESTURE IS MADE AS ITS PRINCIPAL, as every other write here is: a
// person bound to a seat speaks AS THAT SEAT, kind human, with the credential
// they came through beside it — the identity directory's binding, which the
// guard resolved, and never a second reading of it — and under the operation
// the request named, which is what the store derives the message's id from.
// The answer names that operation, so a retry can send it back.
//
// A CALL NAMING NO OPERATION — an assistant's, over MCP — carries none, and
// each of its messages is its own.
func TestAChatGestureIsMadeAsItsPrincipal(t *testing.T) {
	t.Parallel()
	desk := &chatDesk{}
	s := chatSurface(t, desk, nil)
	key := newKey()
	status, answer := act(t, guarded(s), "founder", chatlet.PostMessageTool,
		"application/json", postBody("the deploy is green"), key)
	if status != http.StatusOK {
		t.Fatalf("the post answered %d %v", status, answer)
	}
	made := desk.made()
	if len(made) != 1 {
		t.Fatalf("one act made %d gestures", len(made))
	}
	got := made[0]
	if got.Handle != "jane-founder" || got.Kind != chatlet.AuthorHuman ||
		got.OperatorID != "jane.founder" {

		t.Errorf("the message is said as %+v, want jane-founder (human) through "+
			"jane.founder", got)
	}
	op, _ := answer["op_id"].(string)
	if got.OpKey == "" || got.OpKey != op {
		t.Errorf("the message was made under %q and the answer names %q — the "+
			"op_id a retry sends back must be the one it was made under",
			got.OpKey, op)
	}
	if answer["outcome"] != "applied" || answer["position"] != "CREWLET_CHAT_LOG@1:12" {
		t.Errorf("the act answered %v, want applied at the chat log's position", answer)
	}

	ctx := iam.WithPrincipal(t.Context(), founder)
	res, err := s.Dispatch(ctx, operator.Call{
		Transport: types.TransportMCP, Tool: chatlet.PostMessageTool,
		Args: map[string]any{"channel": room, "body": "and so is staging"},
	})
	if err != nil || res.Failed {
		t.Fatalf("the MCP post: err=%v result=%+v", err, res)
	}
	if made := desk.made(); len(made) != 2 || made[1].OpKey != "" {
		t.Errorf("a call naming no operation was made under %q", made[len(made)-1].OpKey)
	}
}

// A RETRIED CHAT ACT IS THE FIRST ATTEMPT'S MESSAGE: sent again under the same
// key, the store is handed the same operation and derives the same message —
// which is what makes a post whose answer never arrived safe to send twice —
// and a new gesture under a new key is a new one.
func TestARetriedChatActCarriesItsOperation(t *testing.T) {
	t.Parallel()
	desk := &chatDesk{}
	h := guarded(chatSurface(t, desk, nil))
	first, second := newKey(), newKey()
	for _, key := range []string{first, first, second} {
		if status, answer := act(t, h, "founder", chatlet.PostMessageTool,
			"application/json", postBody("rolling back"), key); status != http.StatusOK {

			t.Fatalf("the post answered %d %v", status, answer)
		}
	}
	made := desk.made()
	if len(made) != 3 {
		t.Fatalf("three acts made %d gestures", len(made))
	}
	if made[0].OpKey != made[1].OpKey {
		t.Errorf("one request sent twice was made under %q and then %q — the "+
			"store cannot recognise the retry, so it is a second message",
			made[0].OpKey, made[1].OpKey)
	}
	if made[2].OpKey == made[0].OpKey {
		t.Errorf("a new request under a new key was made under the first's "+
			"operation %q, so it collapses into a message already said", made[0].OpKey)
	}
}

// ONLY A PRINCIPAL ACTING AS A SEAT MAY SPEAK OR READ IN CHAT, and the
// authority table is what says so — every chat verb is seated — so the refusal
// is the table's own envelope on every transport, before any tool runs: `403
// unauthorized`, reason `unseated`, and NO grant named, because no permission
// would cure it. An administrator's token acting as itself is refused exactly
// as a pipeline's is: holding every app is not being somebody in a room.
//
// AND IT IS NEVER OFFERED: the act transport's offer is the table's decision
// before any object is named, so a screen built for such a caller enables no
// chat control at all, rather than one refused on every press.
func TestOnlyAPrincipalActingAsASeatMaySpeakInChat(t *testing.T) {
	t.Parallel()
	gestures := map[string]string{
		chatlet.PostMessageTool:   postBody("hello"),
		chatlet.CreateChannelTool: `{"args":{"channel":"deploys"}}`,
		chatlet.MarkChatReadTool:  `{"args":{"cursors":{"` + room + `":1}}}`,
	}
	for name, p := range map[string]iam.Principal{
		"a pipeline's token":        machine("token:chat-bot", iam.GrantChatlet),
		"an administrator's token":  machine("token:root", iam.AllGrants...),
		"a service account unbound": machine("svc-deploy", iam.Apps...),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			desk := &chatDesk{}
			s := chatSurface(t, desk, nil)
			dir := &directory{people: map[string]iam.Principal{"caller": p}}
			mux := http.NewServeMux()
			mux.Handle(operator.ActPattern, s.ActHandler())
			h := dir.middleware(mux)
			for tool, body := range gestures {
				status, answer := act(t, h, "caller", tool, "application/json", body, newKey())
				grants, listed := answer["grants"].([]any)
				if status != http.StatusForbidden ||
					answer["error"] != string(httpjson.CodeUnauthorized) ||
					answer["reason"] != string(authz.ReasonUnseated) || !listed ||
					len(grants) != 0 {

					t.Errorf("%s answered %d %v, want 403 unauthorized, reason "+
						"unseated, no grant", tool, status, answer)
				}
			}
			// THE READS TOO, which the act transport does not serve: over
			// the dispatch an assistant's call takes, refused by the table.
			ctx := iam.WithPrincipal(t.Context(), p)
			for tool, args := range map[string]map[string]any{
				chatlet.ListChannelsTool:   {},
				chatlet.ReadChannelTool:    {"channel": room},
				chatlet.SearchMessagesTool: {"text": "deploy"},
			} {
				res, err := s.Dispatch(ctx, operator.Call{
					Transport: types.TransportMCP, Tool: tool, Args: args,
				})
				var refused *builtin.DecisionRefused
				if err != nil || !res.Failed || !errors.As(res.Cause, &refused) ||
					refused.Decision.Reason != authz.ReasonUnseated {

					t.Errorf("%s answered err=%v result=%+v, want the table's "+
						"unseated refusal", tool, err, res)
				}
			}
			if made := desk.made(); len(made) != 0 || len(desk.flushes) != 0 {
				t.Errorf("a credential acting as itself reached chat: %+v, flushes %v",
					made, desk.flushes)
			}
			acts, err := s.Acts(t.Context(), p)
			if err != nil {
				t.Fatalf("Acts: %v", err)
			}
			for _, tool := range acts {
				if slices.Contains(append(chatlet.Tools(), chatlet.OperatorOnlyTools()...), tool) {
					t.Errorf("the act transport offers %s to a credential acting "+
						"as itself, whose every press is refused", tool)
				}
			}
		})
	}
	// THE CONTROL: the same surface offers a person their room's gestures.
	acts, err := chatSurface(t, &chatDesk{}, nil).Acts(t.Context(), founder)
	if err != nil {
		t.Fatalf("Acts: %v", err)
	}
	for _, tool := range []string{chatlet.PostMessageTool, chatlet.MarkChatReadTool} {
		if !slices.Contains(acts, tool) {
			t.Errorf("a person bound to a seat is not offered %s: %v", tool, acts)
		}
	}
}

// EVERY CHAT GESTURE IS AUDITED AND NO CHAT READ IS, through the one dispatch:
// a write — refused, failed or made — publishes one `operator_acted` record,
// and a proven read publishes none. A person's read cursor is a write here
// like any other: it moves their state, and an audit that left it out would
// say nothing of a badge cleared from another tab.
func TestEveryChatGestureIsAuditedAndNoChatReadIs(t *testing.T) {
	t.Parallel()
	audit := &auditLog{}
	s := chatSurface(t, &chatDesk{}, audit)
	ctx := iam.WithPrincipal(t.Context(), founder)
	served := s.Tools()
	every := append(chatlet.Tools(), chatlet.OperatorOnlyTools()...)
	for _, tool := range every {
		if !slices.Contains(served, tool) {
			t.Fatalf("the chat half serves %v, without %s", served, tool)
		}
		if _, err := s.Dispatch(ctx, operator.Call{
			Transport: types.TransportAct, Key: newKey(), Tool: tool,
			Args: map[string]any{},
		}); err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
	}
	recorded := map[string]int{}
	for _, ev := range audit.published() {
		recorded[payloadOf(t, ev).Tool]++
	}
	for _, tool := range every {
		read := crewletmcp.ReadOnlyProven(s.Annotations(tool))
		switch {
		case read && recorded[tool] != 0:
			t.Errorf("%s is a proven read and was audited %d times", tool, recorded[tool])
		case !read && recorded[tool] != 1:
			t.Errorf("%s writes and was audited %d times, want once", tool, recorded[tool])
		}
	}
}

// CHAT IS A HALF OF ITS OWN, as the tracker and the knowledge base are: a
// company may run the engine's chat beside a vendor's tracker, and a half
// that offered the others' tools whenever it had one would hand an assistant
// tools that fail at the call. And a half holding a person's READ STATE alone
// — a node in a maintenance mode, which publishes no chat record — serves
// `mark_chat_read` alone, since the cursor is the fleet's coordination record.
func TestChatIsAHalfOfItsOwn(t *testing.T) {
	t.Parallel()
	every := append(chatlet.Tools(), chatlet.OperatorOnlyTools()...)
	whole := chatSurface(t, &chatDesk{}, nil).Tools()
	for _, tool := range every {
		if !slices.Contains(whole, tool) {
			t.Errorf("the chat half serves %v, without %s", whole, tool)
		}
	}
	for _, tool := range whole {
		if slices.Contains(worklet.Tools(), tool) || slices.Contains(builtin.PageTools(), tool) {
			t.Errorf("a company whose only native half is chat was offered %q", tool)
		}
	}

	desk := &chatDesk{}
	cursors := newSurface(t, operator.Options{Halves: fixed(operator.Halves{
		Chat: builtin.ChatDeps{Cursors: desk, ReadState: desk.readState},
	})}).Tools()
	for _, tool := range every {
		if offered := slices.Contains(cursors, tool); offered != (tool == chatlet.MarkChatReadTool) {
			t.Errorf("a half holding the read state alone offers %s: %v", tool, offered)
		}
	}
}
