package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A company where one person's token is bound to their seat and a second
// person's is not bound at all.
const chatCompany = `
name: Nimbus
providers:
  llm:
    p:
      type: anthropic
      model: m
      api_keys: ["${K}"]
roles:
  - name: Agent CEO
    handle: agent-ceo
    llm: p
  - name: Founder
    handle: founder
    kind: human
    contact:
      crewlet_operator_id: founder-token
  - name: Head of Ops
    handle: ops
    kind: human
    contact:
      slack_user_id: U0OPS
`

func chatOrg(t *testing.T) func() *org.Organization {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(chatCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	return func() *org.Organization { return o }
}

// A PERSON IN CHAT SPEAKS UNDER THEIR SEAT, and the write is still made
// through the credential: the bound seat's handle is the message's author, the
// kind is `operator` and the token's id is recorded beside it (ADR-0024).
//
// The author is the seat rather than the token — the one field in which chat
// differs from a tracker write — because a room renders a speaker by it: a room
// where `ci` or `ops-bot` can appear as a speaker is one where "who said this"
// has two vocabularies. The kind and the token are the operator's, as on every
// write this surface makes, so a dashboard write and the same person's
// assistant's read identically in an audit — and an audit can still ask what
// one token did.
func TestAnOperatorPostsAsTheSeatTheirTokenIsBoundTo(t *testing.T) {
	t.Parallel()
	actor, err := operator.ChatActor(chatOrg(t), nil)(
		auth.WithOperator(t.Context(), "founder-token"), nil)
	if err != nil {
		t.Fatalf("ChatActor: %v", err)
	}
	if actor.Handle != "founder" {
		t.Errorf("the message is authored by %q, want the bound seat", actor.Handle)
	}
	if actor.Kind != chat.AuthorOperator {
		t.Errorf("a person's message through a token is attributed as %q, want %q",
			actor.Kind, chat.AuthorOperator)
	}
	if actor.OperatorID != "founder-token" {
		t.Errorf("the credential is recorded as %q", actor.OperatorID)
	}
	// AND THE RENDERED NAME IS THE HANDLE, which is what a room shows: a
	// speaker labelled `operator:founder-token` beside the same person's
	// own messages is two people to whoever is reading the room.
	if actor.Name() != "founder" {
		t.Errorf("the room would show the speaker as %q", actor.Name())
	}
}

// A PERSON BOUND BY A `${VAR}` THE NODE WAS HANDED SPEAKS AS THEMSELF where no
// pin names their seat. The REST erase and prune and the live socket's viewer
// resolve the caller with nothing pinned, and the variable here is set in no
// process — only in the lookup the surface is handed, which is the node's own
// chain. Read from the process environment instead, the actor refused that
// person as bound to no seat while the act transport admitted every press they
// made.
func TestAnUnpinnedChatActorResolvesABindingThroughItsLookup(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_OPERATOR_TEST_HANDED_CHAT_BINDING"
	if _, set := os.LookupEnv(variable); set {
		t.Fatalf("the premise: %s is set in no process", variable)
	}
	chart := func() *org.Organization {
		o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			{Name: "Jane Founder", Kind: org.KindHuman,
				Contact: &org.HumanContact{CrewletOperatorID: "${" + variable + "}"}},
		}}
		o.Normalize()
		return o
	}
	lookup := func(name string) (string, bool) {
		if name == variable {
			return "founder", true
		}
		return "", false
	}
	actor, err := operator.ChatActor(chart, lookup)(
		auth.WithOperator(t.Context(), "founder"), nil)
	if err != nil {
		t.Fatalf("a person the surface's lookup binds was refused: %v", err)
	}
	if actor.Handle != "jane-founder" || actor.OperatorID != "founder" {
		t.Errorf("the bound person speaks as %+v, want jane-founder through the "+
			"founder credential", actor)
	}
}

// AN UNBOUND TOKEN GETS NOTHING, READS INCLUDED — and the refusal names the
// field that fixes it.
//
// The remedy is a line of company configuration rather than a different
// credential, so a refusal that said "unauthorized" would send a person
// looking for a token they already hold. Refusing the READS is the half worth
// stating: a private room's contents are decided by its membership, and a
// caller the engine cannot resolve to a seat has no membership — serving it
// "everything public" would invent a viewer the company never declared, and
// letting the caller name a seat to read as would hand whoever holds the token
// every private conversation in the company.
func TestAnUnboundTokenIsRefusedNamingTheBinding(t *testing.T) {
	t.Parallel()
	actorFor := operator.ChatActor(chatOrg(t), nil)
	for name, ctx := range map[string]context.Context{
		"a token bound to no seat":   auth.WithOperator(context.Background(), "nobody"),
		"no operator on the context": context.Background(),
		"an empty operator id":       auth.WithOperator(context.Background(), ""),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := actorFor(ctx, nil)
			if err == nil {
				t.Fatal("an unresolvable caller was given an identity in chat")
			}
			// TYPED AS THE ACT TRANSPORT'S OWN REFUSAL, so a route answers
			// it under the code a client already handles there.
			var refused *operator.ChatActorError
			if !errors.As(err, &refused) || refused.Refusal != operator.ChatUnbound ||
				refused.Refusal.Code() != operator.CodeUnbound {
				t.Errorf("an unbound caller was refused as %#v, want %q under %q",
					err, operator.ChatUnbound, operator.CodeUnbound)
			}
		})
	}

	_, err := actorFor(auth.WithOperator(t.Context(), "nobody"), nil)
	if err == nil {
		t.Fatal("an unbound token was given an identity")
	}
	for _, want := range []string{"contact.crewlet_operator_id", "kind: human"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q, so nobody can act on it: %v",
				want, err)
		}
	}
}

// A TOKEN BOUND TO AN AGENT SEAT IS REFUSED. The binding exists so a person's
// own credential is recognised as theirs; aimed at an agent it would let
// whoever holds the token speak in that agent's voice, and every colleague in
// the room would read the message as the agent's own words.
//
// THE CHART IS BUILT BY HAND HERE because the config layer refuses this shape
// outright — `contact` on a seat that is not `kind: human` is a parse error —
// and that is exactly why the guard is worth keeping: it is the second lock on
// a door the first one holds shut, and a chart reaches this function from
// wherever the engine last resolved one rather than from the parser.
func TestATokenBoundToAnAgentSeatCannotSpeakAsIt(t *testing.T) {
	t.Parallel()
	agent := &org.Role{
		Name:    "Release Bot",
		Contact: &org.HumanContact{CrewletOperatorID: "ci"},
	}
	chart := &org.Organization{Name: "Nimbus", Roles: []*org.Role{agent}}

	_, err := operator.ChatActor(func() *org.Organization { return chart }, nil)(
		auth.WithOperator(t.Context(), "ci"), nil)
	if err == nil {
		t.Fatal("a token bound to an agent seat was allowed to post as it")
	}
	if !strings.Contains(err.Error(), agent.Handle()) {
		t.Errorf("the refusal does not name the seat it was aimed at: %v", err)
	}
	var refused *operator.ChatActorError
	if !errors.As(err, &refused) || refused.Refusal != operator.ChatAgentSeat ||
		refused.Refusal.Code() == operator.CodeUnbound {
		t.Errorf("an agent seat's token was refused as %#v, want %q under a code "+
			"of its own — it is not the unbound caller's rule", err, operator.ChatAgentSeat)
	}
}

// THE SEAT THE CALL WAS ADMITTED AS IS THE SEAT IT SPEAKS AS. The admission
// pins it, and the chart is read afterwards only to learn what that seat is —
// so a token the chart has since bound to somebody else still speaks as the
// person it was admitted as, a pin of nobody is refused as unbound, and a
// pinned seat that is an agent, or that an apply removed, is refused rather
// than looked up again from the token.
func TestTheChatActorSpeaksAsTheSeatTheCallWasAdmittedAs(t *testing.T) {
	t.Parallel()
	agent := &org.Role{Name: "Release Bot"}
	chart := &org.Organization{Name: "Nimbus", Roles: []*org.Role{
		{Name: "Founder", Kind: org.KindHuman,
			Contact: &org.HumanContact{CrewletOperatorID: "founder-token"}},
		{Name: "Pat Successor", Kind: org.KindHuman},
		agent,
	}}
	chart.Normalize()
	actorFor := operator.ChatActor(func() *org.Organization { return chart }, nil)
	token := auth.WithOperator(t.Context(), "founder-token")

	got, err := actorFor(operator.WithSeat(token, "founder-token", "pat-successor"), nil)
	if err != nil || got.Handle != "pat-successor" {
		t.Errorf("a call admitted as pat-successor spoke as %+v (%v) — the "+
			"chart's binding was read again", got, err)
	}
	for pin, want := range map[string]struct {
		says    string
		refusal operator.ChatRefusal
	}{
		"":                  {"contact.crewlet_operator_id", operator.ChatUnbound},
		agent.Handle():      {"agent seat", operator.ChatAgentSeat},
		"somebody-who-left": {"no longer has", operator.ChatSeatRemoved},
	} {
		_, err := actorFor(operator.WithSeat(token, "founder-token", pin), nil)
		var refused *operator.ChatActorError
		switch {
		case err == nil:
			t.Errorf("a call admitted as %q was given an identity in chat", pin)
		case !strings.Contains(err.Error(), want.says):
			t.Errorf("the refusal of a call admitted as %q does not say %q: %v",
				pin, want.says, err)
		case !errors.As(err, &refused) || refused.Refusal != want.refusal:
			t.Errorf("a call admitted as %q was refused as %#v, want %q", pin, err,
				want.refusal)
		}
	}
}

// A PIN IS ITS OWN CALLER'S, and a context re-keyed to another credential
// does not inherit it.
//
// The dispatch pins the seat it admitted a call as on the call's context, and
// every actor reads that pin before the chart. A context is inherited by
// everything derived from it, so one re-keyed to another operator with
// [auth.WithOperator] still carried the pin — and every resolution made under
// it answered the FIRST caller's seat for the second credential, which is how
// the live channel once resolved every socket on a node to the person whose
// read flush it was pushing. So a pin made for one operator is no pin at all
// to another: the chat actor and the work actor both resolve the second
// credential to its own seat, as a caller nobody pinned.
func TestASeatPinnedForOneCallerIsNotAnothersSeat(t *testing.T) {
	t.Parallel()
	chart := &org.Organization{Name: "Nimbus", Roles: []*org.Role{
		{Name: "Founder", Kind: org.KindHuman,
			Contact: &org.HumanContact{CrewletOperatorID: "founder-token"}},
		{Name: "Ops", Kind: org.KindHuman,
			Contact: &org.HumanContact{CrewletOperatorID: "ops-token"}},
	}}
	chart.Normalize()
	company := func() *org.Organization { return chart }
	pinned := operator.WithSeat(auth.WithOperator(t.Context(), "founder-token"),
		"founder-token", "founder")
	rekeyed := auth.WithOperator(pinned, "ops-token")

	spoke, err := operator.ChatActor(company, nil)(rekeyed, nil)
	if err != nil || spoke.Handle != "ops" || spoke.OperatorID != "ops-token" {
		t.Errorf("a context pinned for the founder and re-keyed to ops spoke as "+
			"%+v (%v), want ops's own seat", spoke, err)
	}
	wrote, err := operator.WorkActor(company, nil)(rekeyed, nil)
	if err != nil || wrote.Seat != "ops" || wrote.OperatorID != "ops-token" {
		t.Errorf("a context pinned for the founder and re-keyed to ops wrote as "+
			"%+v (%v), want ops's own seat", wrote, err)
	}

	// AND THE PIN STILL HOLDS FOR THE CALLER IT WAS MADE FOR, the chart's
	// later binding notwithstanding — which is the whole of what it is for.
	elsewhere := operator.WithSeat(auth.WithOperator(t.Context(), "founder-token"),
		"founder-token", "ops")
	if spoke, err := operator.ChatActor(company, nil)(elsewhere, nil); err != nil ||
		spoke.Handle != "ops" {

		t.Errorf("a call admitted as ops spoke as %+v (%v) — its own pin was "+
			"not read", spoke, err)
	}
}

// A SOCKET IS RESOLVED EXACTLY AS AN UNPINNED CALLER IS, and from nothing but
// its credential.
//
// [operator.ChatViewer] is how the live channel decides whose a socket is, on
// every pass its hub makes; [operator.ChatActor] is how a write is attributed.
// The two must be one answer — a person who may post in a room is exactly the
// person whose socket may be shown it — so for every caller the dispatch did
// not pin they agree, value and refusal alike. And the viewer takes no
// context, so the seat one call was pinned to has no way into it: resolved
// through the actor under a person's own pinned request, every socket on the
// node once became that person.
func TestASocketIsResolvedAsAnUnpinnedCallerIs(t *testing.T) {
	t.Parallel()
	agent := &org.Role{Name: "Release Bot",
		Contact: &org.HumanContact{CrewletOperatorID: "ci"}}
	person := &org.Role{Name: "Founder", Kind: org.KindHuman,
		Contact: &org.HumanContact{CrewletOperatorID: "founder-token"}}
	chart := &org.Organization{Name: "Nimbus", Roles: []*org.Role{person, agent}}
	chart.Normalize()
	company := func() *org.Organization { return chart }
	actor := operator.ChatActor(company, nil)
	viewer := operator.ChatViewer(company, nil)

	for id, refusal := range map[string]operator.ChatRefusal{
		"founder-token": "",
		"ci":            operator.ChatAgentSeat,
		"nobody":        operator.ChatUnbound,
		"":              operator.ChatUnbound,
	} {
		want, wantErr := actor(auth.WithOperator(t.Context(), id), nil)
		got, gotErr := viewer(id)
		var refused *operator.ChatActorError
		if errors.As(gotErr, &refused) != (refusal != "") ||
			(refused != nil && refused.Refusal != refusal) {

			t.Errorf("%q: a socket resolved to %+v (%v), want refusal %q", id, got,
				gotErr, refusal)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: a socket resolved to %+v, a write to %+v", id, got, want)
		}
		var wantRefused, gotRefused *operator.ChatActorError
		errors.As(wantErr, &wantRefused)
		errors.As(gotErr, &gotRefused)
		switch {
		case (wantRefused == nil) != (gotRefused == nil):
			t.Errorf("%q: a socket was refused as %v, a write as %v", id, gotErr, wantErr)
		case wantRefused != nil && *gotRefused != *wantRefused:
			t.Errorf("%q: a socket was refused as %#v, a write as %#v", id,
				gotRefused, wantRefused)
		}
	}
	if got, err := viewer("founder-token"); err != nil || got.Handle != person.Handle() {
		t.Errorf("the founder's socket resolved to %+v (%v), want their seat", got, err)
	}
}

// EVERY REFUSAL HAS A CODE, AND ONLY THE UNBOUND CALLER SHARES ONE.
//
// The three are fixed in three places, so a route answers each under a code
// of its own — except the caller the act transport already refuses as
// `unbound`, which is the same rule and keeps the same code, so a client
// that handles it there handles it on every chat route.
func TestEveryChatRefusalHasItsOwnCode(t *testing.T) {
	t.Parallel()
	seen := map[httpjson.Code]operator.ChatRefusal{}
	for _, r := range []operator.ChatRefusal{
		operator.ChatUnbound, operator.ChatAgentSeat, operator.ChatSeatRemoved,
	} {
		if !r.Valid() {
			t.Errorf("%q is not Valid", r)
		}
		code := r.Code()
		if other, taken := seen[code]; taken {
			t.Errorf("%q and %q share the code %q", r, other, code)
		}
		seen[code] = r
	}
	if operator.ChatRefusal("nonesuch").Valid() {
		t.Error("an unknown refusal is Valid")
	}
}

// THE CALLER MAY NEVER NAME A SEAT. One deps field decides who a write is
// attributed to, exactly as the work and page actors already do — so there is
// no argument, on any tool, that moves the author.
func TestTheChatActorIgnoresEverythingButTheCredential(t *testing.T) {
	t.Parallel()
	actorFor := operator.ChatActor(chatOrg(t), nil)
	founder, err := actorFor(auth.WithOperator(t.Context(), "founder-token"), nil)
	if err != nil {
		t.Fatalf("ChatActor: %v", err)
	}
	// The same function, a different credential, no author at all: the
	// identity travels with the request and with nothing else, so a caller
	// cannot inherit the last one's seat.
	if _, err := actorFor(auth.WithOperator(t.Context(), "ops-bot"), nil); err == nil {
		t.Fatal("a second credential resolved to the first one's seat")
	}
	if founder.Handle != "founder" {
		t.Errorf("the author is %q", founder.Handle)
	}
}

// THE SURFACE SERVES EVERY CHAT GESTURE A PERSON MAKES.
//
// The dashboard makes every change it makes through this catalogue
// (ADR-0024), so a gesture missing here is a control on the chat screen with
// nothing behind it: the nine a seat holds — reading, finding, saying,
// reacting, joining and leaving — and the ones only a person makes, which no
// seat is given. The read-state write is served only where its store is
// wired, on [builtin.Register]'s rule that a tool with no backing is omitted.
func TestTheOperatorSurfaceServesEveryChatGestureAPersonMakes(t *testing.T) {
	t.Parallel()
	deps := builtin.ChatDeps{
		Reader: stubChatReader{}, Writer: stubChatWriter{},
		Search: stubChatSearcher{}, Actor: operator.ChatActor(chatOrg(t), nil),
	}
	srv := newSurface(t, operator.Options{Chat: deps})
	if srv == nil {
		t.Fatal("a company running native chat got no operator surface")
	}
	names := srv.Tools()
	for _, want := range append(chat.Tools(), chat.OperatorOnlyTools()...) {
		if want == chat.MarkChatReadTool {
			continue
		}
		if !slices.Contains(names, want) {
			t.Errorf("the surface serves %v, without %s", names, want)
		}
	}
	if slices.Contains(names, chat.MarkChatReadTool) {
		t.Errorf("a surface with no read-state store offered %s", chat.MarkChatReadTool)
	}

	deps.Cursors = stubChatCursors{}
	if names := newSurface(t, operator.Options{Chat: deps}).Tools(); !slices.Contains(
		names, chat.MarkChatReadTool) {
		t.Errorf("a surface with a read-state store serves %v, without %s",
			names, chat.MarkChatReadTool)
	}
}

// A PERSON'S RETRIED POST IS ONE MESSAGE.
//
// The act transport names a call's operation from the request id a retry
// repeats, and the chat tools read it through their own seam
// ([builtin.ChatDeps.Operation]) — which this surface wires. Left unwired, the
// tools see no operation, key the post on nothing, and the store mints a
// fresh one each time: a person whose post answered `unknown` and who pressed
// retry would say it twice. A second gesture, under a second request id, is a
// second message.
func TestARetriedChatActIsOneMessage(t *testing.T) {
	t.Parallel()
	writer := &recordingChat{}
	h := guarded(newSurface(t, operator.Options{
		Chat: builtin.ChatDeps{
			Reader: stubChatReader{}, Writer: writer,
			Actor: operator.ChatActor(boundChart, nil),
		},
		Org: boundChart,
	}), false)
	args := postArgs()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{requestA, requestA, requestB} {
		if status, answer := act(t, h, "founder-secret", builtin.PostMessageTool,
			"application/json", `{"request_id":"`+id+`","args":`+string(raw)+`}`); status != http.StatusOK {
			t.Fatalf("post_message answered %d %v", status, answer)
		}
	}
	keys := writer.keys()
	if len(keys) != 3 {
		t.Fatalf("three acts reached the store as %d posts", len(keys))
	}
	op := requestOperation(t, "founder", requestA, builtin.PostMessageTool, args)
	minted, _ := statelog.OpMintedAt(op)
	switch {
	case keys[0] != keys[1]:
		t.Errorf("one request sent twice posted under %#v and then %#v — a "+
			"retry the store cannot collapse", keys[0], keys[1])
	case keys[0].Seed != op || !keys[0].Since.Equal(minted):
		t.Errorf("the request posted under %#v, want the operation %s its id "+
			"names, at the request's own instant %s", keys[0], op, minted)
	case keys[2].Seed == keys[0].Seed:
		t.Errorf("a second request posted under the first one's operation %s — "+
			"it would be answered as that post's retry and never said", op)
	}
}

// postArgs is the one post_message call these cases make, into a room named
// by its id so no listing is read to resolve it.
func postArgs() map[string]any {
	return map[string]any{
		"channel": "6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f",
		"body":    "standup moved to ten",
	}
}

// A READ CURSOR PAST THE FLOAT RANGE REACHES THE STORE EXACTLY, over both
// transports.
//
// A cursor is a packed log position with the generation above bit 40, so on a
// log past its first eight thousand generations it is an integer a float64
// cannot hold. Both transports decoded a call's arguments with the default
// decoder, which rounds every number through a float before a tool sees it —
// and the person's badge was then recorded at a place they had never read to.
func TestAReadCursorPastTheFloatRangeReachesTheStoreExactly(t *testing.T) {
	t.Parallel()
	const room = "6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f"
	const cursor = int64(1)<<53 + 1
	for _, transport := range []string{"act", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			flushes := &recordingCursors{}
			s := newSurface(t, operator.Options{
				Chat: builtin.ChatDeps{
					Reader: stubChatReader{}, Writer: stubChatWriter{},
					Cursors: flushes, Actor: operator.ChatActor(boundChart, nil),
				},
				Org: boundChart,
			})
			switch transport {
			case "act":
				// THE WIRE'S OWN DIGITS, never a Go float marshalled back.
				body := `{"request_id":"` + requestA + `","args":{"cursors":{"` +
					room + `":` + strconv.FormatInt(cursor, 10) + `}}}`
				if status, answer := act(t, guarded(s, false), "founder-secret",
					chat.MarkChatReadTool, "application/json", body); status != http.StatusOK {
					t.Fatalf("mark_chat_read answered %d %v", status, answer)
				}
			case "mcp":
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				got, err := dialOperator(t, s, "founder").CallTool(ctx, &mcp.CallToolParams{
					Name: chat.MarkChatReadTool,
					Arguments: map[string]any{"cursors": map[string]any{
						room: json.Number(strconv.FormatInt(cursor, 10)),
					}},
				})
				if err != nil || got.IsError {
					t.Fatalf("mark_chat_read over MCP: %v %v", err, got)
				}
			}
			held := flushes.deltas()
			if len(held) != 1 {
				t.Fatalf("%d flushes reached the store", len(held))
			}
			if at := held[0].Cursors[room]; at != cursor {
				t.Errorf("the cursor reached the store as %d, want %d — rounded "+
					"through a float on the way in", at, cursor)
			}
		})
	}
}

// recordingCursors is a read-state store recording every flush it is handed.
type recordingCursors struct {
	mu   sync.Mutex
	seen []coord.ChatReadDelta
}

func (c *recordingCursors) AdvanceChatRead(_ context.Context, _ string,
	delta coord.ChatReadDelta) (coord.ChatReadState, error) {

	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, delta)
	return coord.ChatReadState{Cursors: delta.Cursors}, nil
}

func (c *recordingCursors) deltas() []coord.ChatReadDelta {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.seen)
}

// recordingChat is a chat writer recording who every post was made as and the
// key it arrives with.
type recordingChat struct {
	stubChatWriter
	mu      sync.Mutex
	seen    []statelog.CallKey
	authors []chat.Actor
}

func (c *recordingChat) Post(_ context.Context, actor chat.Actor, _ string,
	in chat.NewMessage) (chat.Written, error) {

	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, in.CallKey)
	c.authors = append(c.authors, actor)
	at := statelog.Position{Stream: "CREWLET_CHAT_LOG", Seq: uint64(len(c.seen))}
	return chat.Written{
		ChangeID: "m" + strconv.Itoa(len(c.seen)), Revision: uint64(at.Packed()),
		Outcome: statelog.Result{Outcome: statelog.OutcomeApplied, Position: at},
	}, nil
}

func (c *recordingChat) keys() []statelog.CallKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.seen)
}

func (c *recordingChat) actors() []chat.Actor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.authors)
}

// ---- the stubs --------------------------------------------------------- //
//
// They answer nothing: every case above is about which tools the surface
// serves and who a call is attributed to, neither of which reaches a backend.

type stubChatReader struct{}

func (stubChatReader) Channels(context.Context, string, chat.ChannelsQuery,
	statelog.Freshness) (chat.ChannelListing, error) {
	return chat.ChannelListing{}, nil
}

func (stubChatReader) Messages(context.Context, string, chat.TranscriptQuery,
	statelog.Freshness) (chat.Transcript, error) {
	return chat.Transcript{}, nil
}

func (stubChatReader) Thread(context.Context, string, chat.ThreadQuery,
	statelog.Freshness) (chat.Thread, error) {
	return chat.Thread{}, nil
}

func (stubChatReader) ChannelNamed(context.Context, string, string,
	statelog.Freshness) (chat.ChannelDetail, error) {
	return chat.ChannelDetail{}, nil
}

type stubChatWriter struct{}

func (stubChatWriter) Post(context.Context, chat.Actor, string, chat.NewMessage) (
	chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) Reply(context.Context, chat.Actor, string, string,
	chat.NewMessage) (chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) OpenDirect(context.Context, chat.Actor, []string, statelog.CallKey) (
	chat.Written, bool, error) {
	return chat.Written{}, false, nil
}

func (stubChatWriter) React(context.Context, chat.Actor, string, string, string,
	statelog.CallKey) (chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) Unreact(context.Context, chat.Actor, string, string, string,
	statelog.CallKey) (chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) Join(context.Context, chat.Actor, string, statelog.CallKey) (
	chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) Leave(context.Context, chat.Actor, string, statelog.CallKey) (
	chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) CreateChannel(context.Context, chat.Actor, chat.NewChannel) (
	chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) PatchChannel(context.Context, chat.Actor, string, chat.ChannelPatch,
	statelog.CallKey) (chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) SetMembers(context.Context, chat.Actor, string, []chat.Member,
	statelog.CallKey) (chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) Edit(context.Context, chat.Actor, string, string,
	chat.EditMessage) (chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) Delete(context.Context, chat.Actor, string, string,
	statelog.CallKey) (chat.Written, error) {
	return chat.Written{}, nil
}

type stubChatCursors struct{}

func (stubChatCursors) AdvanceChatRead(context.Context, string, coord.ChatReadDelta) (
	coord.ChatReadState, error) {
	return coord.ChatReadState{}, nil
}

type stubChatSearcher struct{}

func (stubChatSearcher) SearchMessages(context.Context, string, builtin.ChatSearch) (
	[]search.ChatHit, error) {
	return nil, nil
}
