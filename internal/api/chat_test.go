package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A company where the founder's token is bound to their seat and the release
// pipeline's is bound to nothing.
//
// THE UNBOUND ONE IS THE POINT OF THE FIXTURE. A Tier A token with no
// `contact.crewlet_operator_id` behind it is the ordinary shape of a CI
// credential, it is accepted everywhere else on this API, and chat is where it
// has to get nothing at all.
const chatCompany = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: Founder
    handle: founder
    kind: human
    contact:
      crewlet_operator_id: founder
`

// chatPosture is a closed posture with two credentials: one bound to a seat
// and one bound to nothing.
func chatPosture() config.Bootstrap {
	b := config.DefaultBootstrap()
	b.API.Auth.AllowAnonymousRead = false
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "founder", Token: "secret"},
		{ID: "release-bot", Token: "pipeline"},
	}
	return b
}

// recordingChat remembers what the routes asked it to write.
//
// It answers [statelog.OutcomeApplied] unless a case says otherwise, because
// the outcome is the one thing several of these cases are about and a fixture
// that could only report success would make two of the three unreachable.
type recordingChat struct {
	actor   chat.Actor
	channel string
	message chat.NewMessage
	members []chat.Member
	erased  []string
	reason  string
	cutoff  time.Time
	calls   int

	outcome statelog.Outcome
	err     error
}

func (c *recordingChat) written(id string) (chat.Written, error) {
	c.calls++
	if c.err != nil {
		return chat.Written{}, c.err
	}
	outcome := c.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	at := statelog.Position{Stream: "CREWLET_CHAT_LOG", Generation: 1, Seq: 41}
	if outcome == statelog.OutcomeUnknown {
		at = statelog.Position{}
	}
	return chat.Written{
		Channel:  chat.Channel{ID: c.channel, Kind: chat.KindPublic},
		Message:  chat.Message{ID: id, Author: c.actor.Handle},
		Revision: 41,
		ChangeID: "op-1",
		Outcome:  statelog.Result{Outcome: outcome, Position: at, OpID: "op-1"},
	}, nil
}

func (c *recordingChat) CreateChannel(_ context.Context, actor chat.Actor,
	in chat.NewChannel) (chat.Written, error) {
	c.actor, c.channel = actor, in.Name
	return c.written("")
}

func (c *recordingChat) OpenDirect(_ context.Context, actor chat.Actor,
	participants []string) (chat.Written, bool, error) {
	c.actor = actor
	c.members = nil
	for _, handle := range participants {
		c.members = append(c.members, chat.Member{Handle: handle})
	}
	written, err := c.written("")
	return written, true, err
}

func (c *recordingChat) PatchChannel(_ context.Context, actor chat.Actor,
	channelID string, _ chat.ChannelPatch) (chat.Written, error) {
	c.actor, c.channel = actor, channelID
	return c.written("")
}

func (c *recordingChat) SetMembers(_ context.Context, actor chat.Actor,
	channelID string, members []chat.Member) (chat.Written, error) {
	c.actor, c.channel, c.members = actor, channelID, members
	return c.written("")
}

func (c *recordingChat) Join(_ context.Context, actor chat.Actor, channelID string) (
	chat.Written, error) {
	c.actor, c.channel = actor, channelID
	return c.written("")
}

func (c *recordingChat) Leave(_ context.Context, actor chat.Actor, channelID string) (
	chat.Written, error) {
	c.actor, c.channel = actor, channelID
	return c.written("")
}

func (c *recordingChat) Erase(_ context.Context, actor chat.Actor, channelID string,
	messageIDs []string, reason string) (chat.Written, error) {
	c.actor, c.channel, c.erased, c.reason = actor, channelID, messageIDs, reason
	return c.written("")
}

func (c *recordingChat) Prune(_ context.Context, actor chat.Actor, channelID string,
	cutoff time.Time) (chat.Written, error) {
	c.actor, c.channel, c.cutoff = actor, channelID, cutoff
	return c.written("")
}

func (c *recordingChat) Post(_ context.Context, actor chat.Actor, channelID string,
	in chat.NewMessage) (chat.Written, error) {
	c.actor, c.channel, c.message = actor, channelID, in
	return c.written("m-1")
}

func (c *recordingChat) Reply(_ context.Context, actor chat.Actor, channelID,
	replyTo string, in chat.NewMessage) (chat.Written, error) {
	c.actor, c.channel, c.message = actor, channelID, in
	return c.written(replyTo + "-reply")
}

func (c *recordingChat) Edit(_ context.Context, actor chat.Actor, channelID,
	messageID string, _ chat.EditMessage) (chat.Written, error) {
	c.actor, c.channel = actor, channelID
	return c.written(messageID)
}

func (c *recordingChat) Delete(_ context.Context, actor chat.Actor, channelID,
	messageID string) (chat.Written, error) {
	c.actor, c.channel = actor, channelID
	return c.written(messageID)
}

func (c *recordingChat) React(_ context.Context, actor chat.Actor, channelID,
	messageID, _ string) (chat.Written, error) {
	c.actor, c.channel = actor, channelID
	return c.written(messageID)
}

func (c *recordingChat) Unreact(_ context.Context, actor chat.Actor, channelID,
	messageID, _ string) (chat.Written, error) {
	c.actor, c.channel = actor, channelID
	return c.written(messageID)
}

// recordingCursors remembers one person's read-state flush.
type recordingCursors struct {
	handle string
	delta  coord.ChatReadDelta
}

func (c *recordingCursors) AdvanceChatRead(_ context.Context, handle string,
	delta coord.ChatReadDelta) (coord.ChatReadState, error) {
	c.handle, c.delta = handle, delta
	return coord.ChatReadState{Cursors: delta.Cursors}, nil
}

func chatApp(t *testing.T, writer api.ChatWriter, cursors api.ChatCursors) *api.App {
	t.Helper()
	c, err := config.ParseCompany([]byte(chatCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b := chatPosture()
	return newApp(t, api.Options{
		Bootstrap: &b,
		Sources:   queries.Sources{Company: func() *config.Company { return c }},
		Chat:      writer,
		Cursors:   cursors,
	})
}

// chatPost runs one authenticated request and returns the status and body.
func chatPost(t *testing.T, a *api.App, token, path, body string) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(http.MethodPost, path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.NewDecoder(rec.Result().Body).Decode(&decoded)
	return rec.Code, decoded
}

// A CREDENTIAL BOUND TO NO SEAT WRITES NOTHING, and the refusal names the
// binding.
//
// It is the strictest rule on this API and it is deliberate: a transcript is
// the most sensitive thing a deployment holds, and a pipeline's credential is
// not a person. The remedy is a line of the company document rather than a
// different token, which is why this is a 403 naming the field and not a 401.
func TestAChatWriteRefusesACredentialBoundToNoSeat(t *testing.T) {
	t.Parallel()
	writer := &recordingChat{}
	a := chatApp(t, writer, nil)

	code, body := chatPost(t, a, "pipeline", "/chat/channels/room-1/messages",
		`{"body":"ship it","operation_id":"k-1"}`)
	if code != http.StatusForbidden {
		t.Fatalf("an unbound credential posted and got %d: %v", code, body)
	}
	if writer.calls != 0 {
		t.Error("the write path was reached by a caller with no seat")
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "contact.crewlet_operator_id") {
		t.Errorf("the refusal does not name the binding, so nobody can act on "+
			"it: %q", detail)
	}
}

// THE AUTHOR IS THE RESOLVED SEAT, AND A BODY THAT NAMES ONE IS REFUSED.
//
// Two halves of one rule. The seat comes from the credential, which is what
// makes impersonation impossible; and a body that tries anyway is told so,
// because ignoring it would let a caller believe it had posted as somebody
// else while the message landed under its own name with nothing to say the
// attempt was made.
func TestTheAuthorIsTheResolvedSeatAndNeverTheBodys(t *testing.T) {
	t.Parallel()
	writer := &recordingChat{}
	a := chatApp(t, writer, nil)

	code, body := chatPost(t, a, "secret", "/chat/channels/room-1/messages",
		`{"body":"on it","operation_id":"k-1"}`)
	if code != http.StatusOK {
		t.Fatalf("the post answered %d: %v", code, body)
	}
	if writer.actor.Handle != "founder" {
		t.Errorf("the message was authored by %q rather than the seat the "+
			"token is bound to", writer.actor.Handle)
	}
	if writer.actor.Kind != chat.AuthorHuman {
		t.Errorf("a person's message is attributed as %q, want %q",
			writer.actor.Kind, chat.AuthorHuman)
	}
	if writer.actor.OperatorID != "founder" {
		t.Errorf("the credential was not recorded beside the seat: %q",
			writer.actor.OperatorID)
	}

	// AND THE BODY CANNOT MOVE IT.
	writer.calls = 0
	code, body = chatPost(t, a, "secret", "/chat/channels/room-1/messages",
		`{"author":"ceo","body":"ship it","operation_id":"k-2"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("a body naming an author answered %d: %v", code, body)
	}
	if writer.calls != 0 {
		t.Error("a body naming an author reached the write path at all")
	}
	if got, _ := body["error"].(string); got != "author_not_yours" {
		t.Errorf("the refusal is coded %q", got)
	}
}

// EVERY WRITE ROUTE RESOLVES THE VIEWER, not just the one above.
//
// A rule enforced at whichever route somebody remembered is a rule the next
// route does not have, so this walks the whole surface with the unbound
// credential and asserts that none of them reaches the write path.
func TestEveryChatWriteRouteResolvesTheViewer(t *testing.T) {
	t.Parallel()
	writer := &recordingChat{}
	a := chatApp(t, writer, &recordingCursors{})

	for _, route := range []struct{ path, body string }{
		{"/chat/channels", `{"name":"launch","kind":"public"}`},
		{"/chat/dms", `{"participants":["ceo"]}`},
		{"/chat/channels/room-1/patch", `{"topic":"t"}`},
		{"/chat/channels/room-1/members", `{"members":[{"handle":"ceo"}]}`},
		{"/chat/channels/room-1/join", ""},
		{"/chat/channels/room-1/leave", ""},
		{"/chat/channels/room-1/messages", `{"body":"hi","operation_id":"k"}`},
		{"/chat/channels/room-1/messages/m-1/replies", `{"body":"hi","operation_id":"k"}`},
		{"/chat/channels/room-1/messages/m-1/edit", `{"body":"hi"}`},
		{"/chat/channels/room-1/messages/m-1/delete", ""},
		{"/chat/channels/room-1/messages/m-1/react", `{"emoji":"+1"}`},
		{"/chat/channels/room-1/messages/m-1/unreact", `{"emoji":"+1"}`},
		{"/chat/channels/room-1/erase?confirm=1", `{"message_ids":["m-1"],"reason":"r"}`},
		{"/chat/channels/room-1/prune?cutoff=2025-01-01T00:00:00Z&confirm=2025-01-01T00:00:00Z", ""},
		{"/chat/read", `{"cursors":{"room-1":1}}`},
	} {
		code, body := chatPost(t, a, "pipeline", route.path, route.body)
		if code != http.StatusForbidden {
			t.Errorf("POST %s answered %d for a credential bound to no seat: %v",
				route.path, code, body)
		}
	}
	if writer.calls != 0 {
		t.Errorf("%d writes were made by a caller with no seat", writer.calls)
	}
}

// A PENDING WRITE SAYS PENDING, and an unknown one says unknown.
//
// The framework answers three different facts about a write and a 200/500 pair
// collapses two of them. `pending` is a record that IS durable and that this
// node has not applied — a caller told 200 reads the room here, does not find
// its message and says it again — and `unknown` is a record that may or may
// not exist, whose only correct handling is a retry under the SAME op id.
func TestAChatWriteAnswersItsOwnThreeValuedOutcome(t *testing.T) {
	t.Parallel()
	for name, expect := range map[string]struct {
		outcome statelog.Outcome
		status  int
	}{
		"applied": {statelog.OutcomeApplied, http.StatusOK},
		"pending": {statelog.OutcomePending, http.StatusAccepted},
		"unknown": {statelog.OutcomeUnknown, http.StatusGatewayTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			writer := &recordingChat{outcome: expect.outcome}
			code, body := chatPost(t, chatApp(t, writer, nil), "secret",
				"/chat/channels/room-1/messages", `{"body":"hi","operation_id":"k"}`)
			if code != expect.status {
				t.Errorf("a %s write answered %d, want %d: %v",
					name, code, expect.status, body)
			}
			if got, _ := body["outcome"].(string); got != string(expect.outcome) {
				t.Errorf("the body reports the outcome as %q", got)
			}
			if got, _ := body["op_id"].(string); got != "op-1" {
				t.Errorf("the answer carries op id %q — retrying under the same "+
					"one is the only safe retry, so it travels with every "+
					"outcome", got)
			}
		})
	}
}

// THE TWO THAT DESTROY CARRY A CONFIRMATION AND A REASON.
//
// An erase removes message rows on every node with no inverse. The
// confirmation is the COUNT because repeating the room id would confirm
// nothing — it is already in the URL that carries the request — and the reason
// is the only account of the rows that survives them.
func TestTheEraseRouteRefusesWithoutItsConfirmationAndItsReason(t *testing.T) {
	t.Parallel()
	writer := &recordingChat{}
	a := chatApp(t, writer, nil)

	for name, request := range map[string]struct{ path, body string }{
		"no confirmation": {"/chat/channels/room-1/erase",
			`{"message_ids":["m-1","m-2"],"reason":"a legal request"}`},
		"the wrong count": {"/chat/channels/room-1/erase?confirm=1",
			`{"message_ids":["m-1","m-2"],"reason":"a legal request"}`},
		"no reason": {"/chat/channels/room-1/erase?confirm=2",
			`{"message_ids":["m-1","m-2"]}`},
	} {
		code, body := chatPost(t, a, "secret", request.path, request.body)
		if code != http.StatusBadRequest {
			t.Errorf("an erase with %s answered %d: %v", name, code, body)
		}
	}
	if writer.calls != 0 {
		t.Fatalf("%d erases got through without a confirmation", writer.calls)
	}

	code, body := chatPost(t, a, "secret", "/chat/channels/room-1/erase?confirm=2",
		`{"message_ids":["m-1","m-2"],"reason":"a legal request"}`)
	if code != http.StatusOK {
		t.Fatalf("a confirmed erase answered %d: %v", code, body)
	}
	if !slices.Equal(writer.erased, []string{"m-1", "m-2"}) {
		t.Errorf("the erase removed %v", writer.erased)
	}
	// AND IT IS RECORDED AS AN OPERATOR'S ACT UNDER THE PERSON'S OWN SEAT.
	// The write path demands the kind in its own decide — an author that
	// could erase its own messages could erase the evidence of what it did
	// — and the handle is what says WHO did it.
	if writer.actor.Kind != chat.AuthorOperator {
		t.Errorf("the erase was recorded as a %q act", writer.actor.Kind)
	}
	if writer.actor.Handle != "founder" || writer.actor.OperatorID != "founder" {
		t.Errorf("the erase was attributed to %q under credential %q",
			writer.actor.Handle, writer.actor.OperatorID)
	}
}

// A PRUNE ECHOES ITS CUTOFF, because that instant decides how much of a room
// disappears and it is not a value to inherit from a shell history.
func TestThePruneRouteRefusesWithoutItsCutoffEchoed(t *testing.T) {
	t.Parallel()
	writer := &recordingChat{}
	a := chatApp(t, writer, nil)

	for name, path := range map[string]string{
		"no cutoff":       "/chat/channels/room-1/prune",
		"no confirmation": "/chat/channels/room-1/prune?cutoff=2025-01-01T00:00:00Z",
		"a mismatch": "/chat/channels/room-1/prune?cutoff=2025-01-01T00:00:00Z" +
			"&confirm=2025-06-01T00:00:00Z",
		"not an instant": "/chat/channels/room-1/prune?cutoff=yesterday&confirm=yesterday",
	} {
		code, body := chatPost(t, a, "secret", path, "")
		if code != http.StatusBadRequest {
			t.Errorf("a prune with %s answered %d: %v", name, code, body)
		}
	}
	if writer.calls != 0 {
		t.Fatalf("%d prunes got through unconfirmed", writer.calls)
	}

	code, _ := chatPost(t, a, "secret", "/chat/channels/room-1/prune"+
		"?cutoff=2025-01-01T00:00:00Z&confirm=2025-01-01T00:00:00Z", "")
	if code != http.StatusOK {
		t.Fatalf("a confirmed prune answered %d", code)
	}
	if !writer.cutoff.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the prune was published with cutoff %s", writer.cutoff)
	}
}

// A READ CURSOR IS FLUSHED FOR THE RESOLVED SEAT, never for a named one.
//
// The cursor is a fact about one reader's attention. A caller that could name
// whose it was would be able to mark somebody else's rooms read, which clears
// a badge that is the only in-app notice this product has.
func TestAReadCursorIsFlushedForTheResolvedSeat(t *testing.T) {
	t.Parallel()
	cursors := &recordingCursors{}
	a := chatApp(t, &recordingChat{}, cursors)

	code, body := chatPost(t, a, "secret", "/chat/read",
		`{"cursors":{"room-1":4096},"muted":["room-2"]}`)
	if code != http.StatusOK {
		t.Fatalf("the flush answered %d: %v", code, body)
	}
	if cursors.handle != "founder" {
		t.Errorf("the cursor was filed under %q", cursors.handle)
	}
	if cursors.delta.Cursors["room-1"] != 4096 {
		t.Errorf("the cursor moved to %d", cursors.delta.Cursors["room-1"])
	}
	if !slices.Equal(cursors.delta.Muted, []string{"room-2"}) {
		t.Errorf("the mute list is %v", cursors.delta.Muted)
	}
	if cursors.delta.At.IsZero() {
		t.Error("the flush carries no instant, so nothing can say since when")
	}
}

// AN OMITTED MUTE LIST LEAVES THE STORED ONE ALONE, and an empty one clears
// it.
//
// Absent means unchanged is [coord.ChatReadDelta]'s own contract — it is what
// lets two tabs flush different facts about one person without either erasing
// the other's — and a decoder that could not tell a missing key from an empty
// array would silently un-mute every room whenever a tab flushed a cursor.
func TestAnOmittedMuteListIsNotAnEmptyOne(t *testing.T) {
	t.Parallel()
	cursors := &recordingCursors{}
	a := chatApp(t, &recordingChat{}, cursors)

	chatPost(t, a, "secret", "/chat/read", `{"cursors":{"room-1":1}}`)
	if cursors.delta.Muted != nil {
		t.Errorf("a flush that said nothing about mutes carried %v, which "+
			"replaces the whole list", cursors.delta.Muted)
	}
	chatPost(t, a, "secret", "/chat/read", `{"muted":[]}`)
	if cursors.delta.Muted == nil {
		t.Error("an explicitly empty mute list read as `unchanged`, so nothing " +
			"can ever un-mute a room")
	}
}

// THE CHAT WRITE ROUTES ARE ABSENT ON A COMPANY THAT RUNS NO NATIVE CHAT.
//
// Not present and refusing: an endpoint that exists and answers 503 to
// everything reads as broken, while one that is not there matches what
// `chat.backend` says. The mux's own 404 is text/plain, which is how a caller
// tells it from the query layer's.
//
// A WRITE-ONLY PATH IS WHAT THIS ASKS ABOUT. The READS are mounted on every
// node — a question with no source answers the query layer's own
// `unknown_query`, which is the tracker's and the wiki's arrangement — so a
// POST to a read's path is a method that path does not take, which is a
// different and equally honest answer.
func TestTheChatWriteRoutesAreAbsentWithoutANativeChat(t *testing.T) {
	t.Parallel()
	b := chatPosture()
	a := newApp(t, api.Options{Bootstrap: &b})

	req := httptest.NewRequest(http.MethodPost, "/chat/channels/room-1/join", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a company with no native chat answered %d", rec.Code)
	}
	if ct := rec.Result().Header.Get("Content-Type"); strings.Contains(ct, "json") {
		t.Errorf("content-type %q: the route is registered and refusing rather "+
			"than absent", ct)
	}
}

// THE REST READ AND THE SOCKET QUERY ANSWER IDENTICALLY.
//
// The socket is the dashboard's only data channel and the REST surface is what
// a script and the end-to-end suite use, so the two have to be one
// implementation: a named route resolves its path values and hands them to the
// SAME registry entry the query channel reaches. This calls both and compares
// the bytes, because "they call the same function" is a claim about code and
// this is a claim about answers.
//
// The question is registered by the test rather than by the chat sources,
// which keeps the case about the adapter — the thing this file owns — rather
// than about what a chat listing contains.
func TestTheRESTReadAndTheSocketQueryAnswerIdentically(t *testing.T) {
	t.Parallel()
	a := chatApp(t, &recordingChat{}, nil)
	a.Queries().Register("chat_messages", func(_ context.Context, p queries.Params) (any, error) {
		// The PARAMETERS are the answer, so a route that captured a path
		// value and never passed it on fails here rather than looking
		// identical to one that did.
		return map[string]any{
			"channel_id": p.String("channel_id"),
			"cursor":     p.String("cursor"),
			"limit":      p.Int("limit", 0),
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet,
		"/chat/channels/room-1/messages?cursor=41&limit=25", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET answered %d: %s", rec.Code, rec.Body.String())
	}

	// The socket's own entry point, with the frame's params as a JSON
	// object — which is the other half of what these two transports do
	// differently.
	overSocket, err := a.Queries().Answer(t.Context(), "chat_messages",
		map[string]any{"channel_id": "room-1", "cursor": "41", "limit": float64(25)},
		"founder")
	if err != nil {
		t.Fatalf("the socket query failed: %v", err)
	}
	raw, err := json.Marshal(overSocket)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != string(raw) {
		t.Errorf("the two transports answered differently:\n  REST:   %s\n  socket: %s",
			got, raw)
	}
}

// EVERY CHAT READ ROUTE REACHES THE REGISTRY.
//
// A route that is not registered answers the mux's own 404 — text/plain, no
// code — while one whose QUESTION has no source answers the query layer's,
// which carries `unknown_query` as JSON. The difference is the whole of what
// this asserts: these paths exist, and what a node without chat sources is
// missing is the source rather than the route.
func TestEveryChatReadRouteReachesTheQueryLayer(t *testing.T) {
	t.Parallel()
	a := chatApp(t, &recordingChat{}, nil)
	for _, path := range []string{
		"/chat/channels",
		"/chat/channels/room-1",
		"/chat/channels/room-1/messages",
		"/chat/channels/room-1/threads/m-1",
		"/chat/mentions",
		"/chat/search",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if ct := rec.Result().Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s answered %q; a bare mux 404 means the route is not "+
				"registered at all", path, ct)
		}
	}
}

// publicRoom is a read side with one public room in it, for the live case
// below.
type publicRoom struct{}

func (publicRoom) Channel(_ context.Context, _, channelID string,
	_ statelog.Freshness) (chat.ChannelDetail, error) {
	return chat.ChannelDetail{
		Channel: chat.Channel{ID: channelID, Kind: chat.KindPublic},
	}, nil
}

// A COMMITTED RECORD REACHES A BOUND SOCKET AND NOT AN UNBOUND ONE, through
// the real credential chain.
//
// The filter itself is exercised in the stream package's own suite with an
// injected viewer; what this asserts is the WIRING — that the resolution a
// socket is identified by is the one a write is attributed by, walked through
// Tier A's tokens and the company's own org chart. Two resolutions is two
// answers to who somebody is, and this is the only place both are in play.
func TestACommittedRecordReachesTheSocketOfTheSeatItIsVisibleTo(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(chatCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b := chatPosture()
	live := api.NewChatLive(api.ChatLiveOptions{
		Company: func() *config.Company { return c },
		Rooms:   func() stream.ChatRooms { return publicRoom{} },
		NodeID:  "node-a",
	})
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Sources:   queries.Sources{Company: func() *config.Company { return c }},
		Chat:      &recordingChat{},
		ChatLive:  live,
	})
	a.Start(t.Context())

	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"

	bound := dialChat(t, base+"?token=secret")
	unbound := dialChat(t, base+"?token=pipeline")

	live.Applied([]chat.Applied{{
		Position:   statelog.Position{Stream: "CREWLET_CHAT_LOG", Generation: 1, Seq: 9},
		Subject:    chat.MessageSubject("room-1"),
		Op:         chat.OpPost,
		OpID:       "op-9",
		ChannelID:  "room-1",
		MessageID:  "m-9",
		ChannelSeq: 3,
		Actor:      "ceo",
		ActorKind:  chat.AuthorAgent,
		At:         time.Unix(1_700_000_000, 0).UTC(),
	}})

	if kind := nextKind(t, bound, "chat_posted"); kind != stream.KindChatPosted {
		t.Fatalf("the bound socket was sent %q", kind)
	}
	// AND THE UNBOUND ONE IS SENT NOTHING. It is not a watcher at all, so
	// this is a read that must time out rather than a frame that must be
	// filtered — the seconds here are only to tell "not sent" from "not
	// sent yet", and the send it would race with is an in-memory queue
	// write on a goroutine that has already run for its neighbour.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for {
		_, raw, readErr := unbound.Read(ctx)
		if readErr != nil {
			return // timed out: nothing was sent, which is the assertion
		}
		var env map[string]any
		_ = json.Unmarshal(raw, &env)
		if kind, _ := env["kind"].(string); strings.HasPrefix(kind, "chat_") {
			t.Fatalf("a credential bound to no seat was sent %q", kind)
		}
	}
}

// dialChat opens a socket and swallows its opening snapshot.
func dialChat(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	if kind := nextKind(t, conn, stream.KindSnapshot); kind != stream.KindSnapshot {
		t.Fatalf("the first frame was %q", kind)
	}
	return conn
}

// nextKind reads frames until one is not a health tick, which the shared timer
// emits on its own schedule and which is nobody's business here.
func nextKind(t *testing.T, conn *websocket.Conn, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v", want, err)
		}
		var env map[string]any
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		kind, _ := env["kind"].(string)
		if kind == stream.KindHealth {
			continue
		}
		return kind
	}
}

// NO PUSH KIND IS ALSO A QUESTION.
//
// An envelope carries a `kind` and nothing in it says which direction the
// frame was travelling, so a name in both namespaces is a frame a reader
// cannot interpret without knowing who sent it.
func TestNoChatPushKindIsAlsoARegisteredQuestion(t *testing.T) {
	t.Parallel()
	a := chatApp(t, &recordingChat{}, nil)
	// The chat questions this build's sources may register. Named here
	// because the fixture wires none of them, and the point is the
	// NAMESPACE rather than which of them a given node can answer.
	for _, what := range []string{
		"chat_channels", "chat_channel", "chat_messages", "chat_thread",
		"chat_mentions", "chat_search",
	} {
		a.Queries().Register(what, func(context.Context, queries.Params) (any, error) {
			return nil, nil
		})
	}
	names := a.Queries().Names()
	for _, kind := range stream.ChatPushKinds() {
		if slices.Contains(names, kind) {
			t.Errorf("%q is both a push kind and a question", kind)
		}
	}
}
