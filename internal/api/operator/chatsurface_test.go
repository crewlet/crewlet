package operator_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
)

// chatCalls is one well-formed call of EVERY chat tool the operator surface
// serves — the reads a person's assistant makes and every gesture the
// dashboard does — so a case that walks it cannot be satisfied by a tool
// refusing its arguments before it ever asks who is calling.
func chatCalls() map[string]map[string]any {
	return map[string]map[string]any{
		builtin.ListChannelsTool:   {},
		builtin.ReadChannelTool:    {"channel": readRoom},
		builtin.SearchMessagesTool: {"text": "standup"},
		builtin.PostMessageTool:    {"channel": readRoom, "body": "standup moved"},
		builtin.ReplyInThreadTool:  {"channel": readRoom, "reply_to": "m1", "body": "ok"},
		builtin.SendDMTool:         {"to": []any{"ops"}, "body": "a word"},
		builtin.ReactToMessageTool: {"channel": readRoom, "message": "m1", "emoji": ":+1:"},
		builtin.JoinChannelTool:    {"channel": readRoom},
		builtin.LeaveChannelTool:   {"channel": readRoom},
		chat.CreateChannelTool:     {"name": "launch", "kind": "private"},
		chat.OpenDMTool:            {"to": []any{"ops"}},
		chat.UpdateChannelTool:     {"channel": readRoom, "topic": "the launch"},
		chat.SetChannelMembersTool: {"channel": readRoom, "members": []any{"ops"}},
		chat.EditMessageTool:       {"channel": readRoom, "message": "m1", "body": "by noon"},
		chat.DeleteMessageTool:     {"channel": readRoom, "message": "m1"},
		chat.MarkChatReadTool:      {"cursors": map[string]any{readRoom: 12}},
	}
}

// chatReads are the chat tools that change nothing: never audited, and refused
// by the act transport, which only writes.
var chatReads = []string{
	builtin.ListChannelsTool, builtin.ReadChannelTool, builtin.SearchMessagesTool,
}

// touchedChat is every chat backend seam at once — rooms, writes, search and
// read state — counting each call that reached it, and answering each the way
// a store with nothing in it would.
type touchedChat struct {
	stubChatReader
	stubChatWriter
	mu    sync.Mutex
	calls []string
}

func (c *touchedChat) touch(what string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, what)
}

func (c *touchedChat) touched() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

func (c *touchedChat) Channels(ctx context.Context, viewer string, q chat.ChannelsQuery,
	fresh statelog.Freshness) (chat.ChannelListing, error) {
	c.touch("Channels")
	return c.stubChatReader.Channels(ctx, viewer, q, fresh)
}

func (c *touchedChat) Messages(ctx context.Context, viewer string, q chat.TranscriptQuery,
	fresh statelog.Freshness) (chat.Transcript, error) {
	c.touch("Messages")
	return c.stubChatReader.Messages(ctx, viewer, q, fresh)
}

func (c *touchedChat) Thread(ctx context.Context, viewer string, q chat.ThreadQuery,
	fresh statelog.Freshness) (chat.Thread, error) {
	c.touch("Thread")
	return c.stubChatReader.Thread(ctx, viewer, q, fresh)
}

func (c *touchedChat) ChannelNamed(ctx context.Context, viewer, name string,
	fresh statelog.Freshness) (chat.ChannelDetail, error) {
	c.touch("ChannelNamed")
	return c.stubChatReader.ChannelNamed(ctx, viewer, name, fresh)
}

func (c *touchedChat) Post(ctx context.Context, actor chat.Actor, channel string,
	in chat.NewMessage) (chat.Written, error) {
	c.touch("Post")
	return c.stubChatWriter.Post(ctx, actor, channel, in)
}

func (c *touchedChat) Reply(ctx context.Context, actor chat.Actor, channel, replyTo string,
	in chat.NewMessage) (chat.Written, error) {
	c.touch("Reply")
	return c.stubChatWriter.Reply(ctx, actor, channel, replyTo, in)
}

func (c *touchedChat) OpenDirect(ctx context.Context, actor chat.Actor, to []string,
	key statelog.CallKey) (chat.Written, bool, error) {
	c.touch("OpenDirect")
	return c.stubChatWriter.OpenDirect(ctx, actor, to, key)
}

func (c *touchedChat) React(ctx context.Context, actor chat.Actor, channel, message,
	emoji string, key statelog.CallKey) (chat.Written, error) {
	c.touch("React")
	return c.stubChatWriter.React(ctx, actor, channel, message, emoji, key)
}

func (c *touchedChat) Unreact(ctx context.Context, actor chat.Actor, channel, message,
	emoji string, key statelog.CallKey) (chat.Written, error) {
	c.touch("Unreact")
	return c.stubChatWriter.Unreact(ctx, actor, channel, message, emoji, key)
}

func (c *touchedChat) Join(ctx context.Context, actor chat.Actor, channel string,
	key statelog.CallKey) (chat.Written, error) {
	c.touch("Join")
	return c.stubChatWriter.Join(ctx, actor, channel, key)
}

func (c *touchedChat) Leave(ctx context.Context, actor chat.Actor, channel string,
	key statelog.CallKey) (chat.Written, error) {
	c.touch("Leave")
	return c.stubChatWriter.Leave(ctx, actor, channel, key)
}

func (c *touchedChat) CreateChannel(ctx context.Context, actor chat.Actor,
	in chat.NewChannel) (chat.Written, error) {
	c.touch("CreateChannel")
	return c.stubChatWriter.CreateChannel(ctx, actor, in)
}

func (c *touchedChat) PatchChannel(ctx context.Context, actor chat.Actor, channel string,
	patch chat.ChannelPatch, key statelog.CallKey) (chat.Written, error) {
	c.touch("PatchChannel")
	return c.stubChatWriter.PatchChannel(ctx, actor, channel, patch, key)
}

func (c *touchedChat) SetMembers(ctx context.Context, actor chat.Actor, channel string,
	members []chat.Member, key statelog.CallKey) (chat.Written, error) {
	c.touch("SetMembers")
	return c.stubChatWriter.SetMembers(ctx, actor, channel, members, key)
}

func (c *touchedChat) Edit(ctx context.Context, actor chat.Actor, channel, message string,
	in chat.EditMessage) (chat.Written, error) {
	c.touch("Edit")
	return c.stubChatWriter.Edit(ctx, actor, channel, message, in)
}

func (c *touchedChat) Delete(ctx context.Context, actor chat.Actor, channel, message string,
	key statelog.CallKey) (chat.Written, error) {
	c.touch("Delete")
	return c.stubChatWriter.Delete(ctx, actor, channel, message, key)
}

func (c *touchedChat) SearchMessages(context.Context, string, builtin.ChatSearch) (
	[]search.ChatHit, error) {
	c.touch("SearchMessages")
	return nil, nil
}

func (c *touchedChat) AdvanceChatRead(context.Context, string, coord.ChatReadDelta) (
	coord.ChatReadState, error) {
	c.touch("AdvanceChatRead")
	return coord.ChatReadState{}, nil
}

func (c *touchedChat) readState(context.Context, string) (coord.ChatReadState, error) {
	c.touch("ReadState")
	return coord.ChatReadState{}, nil
}

// chatSurface is the operator surface serving every chat tool over one
// [touchedChat], bound by [boundChart] and audited into a log the case reads.
func chatSurface(t *testing.T) (*operator.Server, *touchedChat, *auditLog) {
	t.Helper()
	backend := &touchedChat{}
	audit := &auditLog{}
	s := newSurface(t, operator.Options{
		Chat: builtin.ChatDeps{
			Reader: backend, Writer: backend, Search: backend, Cursors: backend,
			ReadState: backend.readState,
			Actor:     operator.ChatActor(boundChart, nil),
		},
		Org:   boundChart,
		Audit: audit,
	})
	// THE TABLE IS HELD IN BOTH DIRECTIONS. Every name in [chatCalls] must
	// be served — or a case walking it is satisfied by a tool that does
	// not exist — and every tool the chat deps add must be in [chatCalls],
	// or a chat gesture added to the catalogue escapes every case that
	// walks it, these being the only audit guard the chat screen's
	// gestures have. What the chat deps add is the surface built with them
	// less the surface built without them.
	without := newSurface(t, operator.Options{Org: boundChart, Audit: &auditLog{}})
	var added []string
	for _, name := range s.Tools() {
		if !slices.Contains(without.Tools(), name) {
			added = append(added, name)
		}
	}
	slices.Sort(added)
	if want := slices.Sorted(maps.Keys(chatCalls())); !slices.Equal(added, want) {
		t.Fatalf("the chat deps add %v to the surface and chatCalls holds %v — a "+
			"chat tool missing from the table is one no case here audits", added, want)
	}
	return s, backend, audit
}

// actChat posts one chat call through the act transport as a token.
func actChat(t *testing.T, s *operator.Server, token, tool string,
	args map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"request_id": requestA, "args": args})
	if err != nil {
		t.Fatal(err)
	}
	return act(t, guarded(s, false), token, tool, "application/json", string(raw))
}

// EVERY CHAT GESTURE IS AUDITED, ON BOTH TRANSPORTS, AND NO CHAT READ IS.
//
// The chat screen makes every change through this catalogue (ADR-0024), so a
// person's whole chat life — what they said, edited, took back, who they let
// into a room, what they muted — is recorded here or nowhere: unlike a tracker
// or page write there is no history row a message write would otherwise leave
// about the CALL, a refused one least of all. Each record names the person the
// call was admitted as. A read is never recorded, on either transport: it
// changes nothing, and a row per question an assistant asked would bury the
// writes an audit is read for.
func TestEveryChatGestureIsAuditedAndNoChatReadIs(t *testing.T) {
	t.Parallel()
	calls := chatCalls()
	for _, transport := range []string{types.TransportAct, types.TransportMCP} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			s, _, audit := chatSurface(t)
			for name, args := range calls {
				switch transport {
				case types.TransportAct:
					status, answer := actChat(t, s, "founder-secret", name, args)
					if slices.Contains(chatReads, name) {
						if status != http.StatusBadRequest ||
							answer["error"] != string(operator.CodeReadOnlyTool) {
							t.Errorf("the read %s answered %d %v on the act transport, "+
								"want 400 %s", name, status, answer, operator.CodeReadOnlyTool)
						}
						continue
					}
					// SERVED, so the calls above are well formed: a
					// gesture refused for its arguments is audited too,
					// and would prove nothing about the ones that land.
					if status != http.StatusOK {
						t.Errorf("%s answered %d %v, want it served", name, status, answer)
					}
				case types.TransportMCP:
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					res, err := dialOperator(t, s, "founder").CallTool(ctx,
						&mcp.CallToolParams{Name: name, Arguments: args})
					cancel()
					if err != nil {
						t.Fatalf("%s over MCP: %v", name, err)
					}
					// SERVED HERE TOO: a refused call is audited
					// as well, so an audit count alone cannot tell
					// a transport that refuses every gesture from
					// one that carries them.
					if res.IsError {
						t.Errorf("%s over MCP was refused: %s", name, textOf(res))
					}
				}
			}
			var audited []string
			for _, ev := range audit.published() {
				p := payloadOf(t, ev)
				audited = append(audited, p.Tool)
				if p.Transport != transport || p.ActorSeat != "jane-founder" ||
					p.OperatorID != "founder" {
					t.Errorf("the record of %s is %+v, want the founder's token, "+
						"their seat and %s", p.Tool, p, transport)
				}
				if p.Outcome != types.AuditApplied {
					t.Errorf("the record of %s over %s says %q, want %q — the "+
						"gesture did not land", p.Tool, transport, p.Outcome,
						types.AuditApplied)
				}
			}
			for name := range calls {
				want := 1
				if slices.Contains(chatReads, name) {
					want = 0
				}
				if got := countOf(audited, name); got != want {
					t.Errorf("%s left %d audit records over %s, want %d",
						name, got, transport, want)
				}
			}
		})
	}
}

func countOf(names []string, name string) int {
	n := 0
	for _, got := range names {
		if got == name {
			n++
		}
	}
	return n
}

// AN UNBOUND TOKEN REACHES NO ROOM, OVER EITHER TRANSPORT, READS INCLUDED.
//
// The act transport admits only a person, so it refuses the token `unbound`
// before any tool runs. MCP admits any token — an assistant connected with a
// CI credential is that credential acting as itself — so there it is the chat
// actor that refuses, on every chat tool, naming the line of configuration that
// would make the caller somebody: a private room's contents are decided by its
// membership, and a caller with no seat has none to serve. Either way nothing
// reaches the rooms, the search or the read state. Over MCP each refused
// gesture is still audited, as `refused` and `forbidden`, under no seat.
func TestAnUnboundTokenReachesNoRoomOverEitherTransport(t *testing.T) {
	t.Parallel()
	calls := chatCalls()
	t.Run(types.TransportAct, func(t *testing.T) {
		t.Parallel()
		s, backend, audit := chatSurface(t)
		for name, args := range calls {
			if slices.Contains(chatReads, name) {
				continue
			}
			status, answer := actChat(t, s, "ci-secret", name, args)
			if status != http.StatusForbidden || answer["error"] != string(operator.CodeUnbound) {
				t.Errorf("an unbound token's %s answered %d %v, want 403 %s",
					name, status, answer, operator.CodeUnbound)
			}
		}
		if got := backend.touched(); len(got) != 0 {
			t.Errorf("an unbound token's acts reached the chat backend: %v", got)
		}
		if got := audit.published(); len(got) != 0 {
			t.Errorf("acts the transport refused before any tool ran were audited: %d", len(got))
		}
	})
	t.Run(types.TransportMCP, func(t *testing.T) {
		t.Parallel()
		s, backend, audit := chatSurface(t)
		sess := dialOperator(t, s, "ci")
		for name, args := range calls {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			cancel()
			if err != nil {
				t.Fatalf("%s over MCP: %v", name, err)
			}
			if !res.IsError || !strings.Contains(textOf(res), "contact.crewlet_operator_id: ci") {
				t.Errorf("an unbound token's %s was answered %q, want a refusal naming "+
					"the binding", name, textOf(res))
			}
		}
		if got := backend.touched(); len(got) != 0 {
			t.Errorf("an unbound token's calls reached the chat backend: %v", got)
		}
		recorded := audit.published()
		if len(recorded) != len(calls)-len(chatReads) {
			t.Errorf("%d refused gestures left %d audit records, want one each",
				len(calls)-len(chatReads), len(recorded))
		}
		for _, ev := range recorded {
			p := payloadOf(t, ev)
			if p.Outcome != types.AuditRefused ||
				p.Refusal != string(crewletmcp.RefusalForbidden) || p.ActorSeat != "" {
				t.Errorf("the refused %s is recorded as %+v, want refused, forbidden "+
					"and no seat", p.Tool, p)
			}
		}
	})
}

// A PERSON'S LISTING IS COUNTED AGAINST THE READ STATE OF THE SEAT THEIR TOKEN
// IS BOUND TO — never the token's own name. `mark_chat_read` writes the record
// under the seat the call was admitted as, and the reconcile keeps or forgets it
// by the org chart's handles, so a listing that asked for the token's name
// would read a record nobody writes and call every room unread.
func TestAListingReadsTheReadStateOfTheSeatTheTokenIsBoundTo(t *testing.T) {
	t.Parallel()
	var (
		mu    sync.Mutex
		asked []string
	)
	s := newSurface(t, operator.Options{
		Chat: builtin.ChatDeps{
			Reader: stubChatReader{},
			ReadState: func(_ context.Context, handle string) (coord.ChatReadState, error) {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, handle)
				return coord.ChatReadState{}, nil
			},
			Actor: operator.ChatActor(boundChart, nil),
		},
		Org: boundChart,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := dialOperator(t, s, "founder").CallTool(ctx,
		&mcp.CallToolParams{Name: builtin.ListChannelsTool, Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_channels over MCP: %v %q", err, textOf(res))
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(asked, []string{"jane-founder"}) {
		t.Errorf("the read state was asked for %v, want the seat the token `founder` "+
			"is bound to (jane-founder)", asked)
	}
}
