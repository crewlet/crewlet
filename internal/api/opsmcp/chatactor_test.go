package opsmcp_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
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

// A PERSON IN CHAT IS A SEAT, so an operator writes as the seat their token is
// bound to — never as the token.
//
// The tracker and the wiki record a credential as an author of its own kind,
// because a token genuinely is not a colleague there. Chat has no such
// reading: the name beside the words is the whole of what a reader has, and a
// room where `ci` or `ops-bot` can appear as a speaker is one where "who said
// this" has two vocabularies. The credential is still on the record, beside
// the author rather than instead of it, so an audit can ask what one token did.
func TestAnOperatorPostsAsTheSeatTheirTokenIsBoundTo(t *testing.T) {
	t.Parallel()
	actor, err := opsmcp.ChatActor(chatOrg(t))(
		auth.WithOperator(t.Context(), "founder-token"), nil)
	if err != nil {
		t.Fatalf("ChatActor: %v", err)
	}
	if actor.Handle != "founder" {
		t.Errorf("the message is authored by %q, want the bound seat", actor.Handle)
	}
	if actor.Kind != chat.AuthorHuman {
		t.Errorf("a person's message is attributed as %q, want %q",
			actor.Kind, chat.AuthorHuman)
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
	actorFor := opsmcp.ChatActor(chatOrg(t))
	for name, ctx := range map[string]context.Context{
		"a token bound to no seat":   auth.WithOperator(context.Background(), "nobody"),
		"no operator on the context": context.Background(),
		"an empty operator id":       auth.WithOperator(context.Background(), ""),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := actorFor(ctx, nil); err == nil {
				t.Fatal("an unresolvable caller was given an identity in chat")
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

	_, err := opsmcp.ChatActor(func() *org.Organization { return chart })(
		auth.WithOperator(t.Context(), "ci"), nil)
	if err == nil {
		t.Fatal("a token bound to an agent seat was allowed to post as it")
	}
	if !strings.Contains(err.Error(), agent.Handle()) {
		t.Errorf("the refusal does not name the seat it was aimed at: %v", err)
	}
}

// THE CALLER MAY NEVER NAME A SEAT. One deps field decides who a write is
// attributed to, exactly as the work and page actors already do — so there is
// no argument, on any tool, that moves the author.
func TestTheChatActorIgnoresEverythingButTheCredential(t *testing.T) {
	t.Parallel()
	actorFor := opsmcp.ChatActor(chatOrg(t))
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

// THE SURFACE SERVES THE SIX AN ASSISTANT NEEDS, and not the furniture.
//
// Reading the rooms, finding what was said and saying something are what a
// person's assistant is for. A reaction and a room's membership are what
// somebody arranges in their own client while they are looking at it — the
// same reasoning that keeps the saved-view tools out of a seat's registry,
// applied from the other side.
func TestTheOperatorSurfaceServesTheChatToolsAPersonNeeds(t *testing.T) {
	t.Parallel()
	srv := opsmcp.New(opsmcp.Options{
		Chat: builtin.ChatDeps{
			Reader: stubChatReader{}, Writer: stubChatWriter{},
			Search: stubChatSearcher{}, Actor: opsmcp.ChatActor(chatOrg(t)),
		},
	})
	if srv == nil {
		t.Fatal("a company running native chat got no operator surface")
	}
	names := srv.Tools()
	for _, want := range []string{
		chat.ListChannelsTool, chat.ReadChannelTool, chat.SearchMessagesTool,
		chat.PostMessageTool, chat.ReplyInThreadTool, chat.SendDMTool,
	} {
		if !slices.Contains(names, want) {
			t.Errorf("the surface serves %v, without %s", names, want)
		}
	}
	for _, unwanted := range []string{
		chat.ReactToMessageTool, chat.JoinChannelTool, chat.LeaveChannelTool,
	} {
		if slices.Contains(names, unwanted) {
			t.Errorf("an operator's assistant was offered %s", unwanted)
		}
	}
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

type stubChatWriter struct{}

func (stubChatWriter) Post(context.Context, chat.Actor, string, chat.NewMessage) (
	chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) Reply(context.Context, chat.Actor, string, string,
	chat.NewMessage) (chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) OpenDirect(context.Context, chat.Actor, []string) (
	chat.Written, bool, error) {
	return chat.Written{}, false, nil
}

func (stubChatWriter) React(context.Context, chat.Actor, string, string, string) (
	chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) Unreact(context.Context, chat.Actor, string, string, string) (
	chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) Join(context.Context, chat.Actor, string) (chat.Written, error) {
	return chat.Written{}, nil
}

func (stubChatWriter) Leave(context.Context, chat.Actor, string) (chat.Written, error) {
	return chat.Written{}, nil
}

type stubChatSearcher struct{}

func (stubChatSearcher) SearchMessages(context.Context, string, builtin.ChatSearch) (
	[]search.ChatHit, error) {
	return nil, nil
}
