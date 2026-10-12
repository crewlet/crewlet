package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/org"
	qmemory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A company where the founder's token is bound to their seat and the release
// pipeline's is bound to nothing.
//
// THE UNBOUND ONE IS THE POINT OF THE FIXTURE. A Tier A token with no
// `contact.crewlet_operator_id` behind it is the ordinary shape of a CI
// credential, it is accepted everywhere else on this API, and chat is where it
// reads and says nothing at all — while a destruction, which is said in
// nobody's voice, takes it as the purge does.
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
      crewlet_operator_id: founder-token
`

// chatPosture is a closed posture with two credentials: one bound to a person
// and one bound to nothing.
func chatPosture() config.Bootstrap {
	b := config.DefaultBootstrap()
	b.API.Auth.AllowAnonymousRead = false
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "founder-token", Token: "secret"},
		{ID: "release-bot", Token: "pipeline"},
	}
	return b
}

// recordingPurger remembers what the two destructive routes asked it to do.
//
// It answers [statelog.OutcomeApplied] unless a case says otherwise, because
// the outcome is the one thing several of these cases are about and a fixture
// that could only report success would make two of the three unreachable.
type recordingPurger struct {
	actor   chat.Actor
	opID    string
	channel string
	erased  []string
	reason  string
	cutoff  time.Time
	calls   int

	outcome statelog.Outcome
	err     error
}

func (p *recordingPurger) written() (chat.Written, error) {
	p.calls++
	if p.err != nil {
		return chat.Written{}, p.err
	}
	outcome := p.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	at := statelog.Position{Stream: "CREWLET_CHAT_LOG", Generation: 1, Seq: 41}
	if outcome == statelog.OutcomeUnknown {
		at = statelog.Position{}
	}
	return chat.Written{
		Channel:  chat.Channel{ID: p.channel, Kind: chat.KindPublic},
		Revision: 41,
		ChangeID: p.opID,
		Outcome:  statelog.Result{Outcome: outcome, Position: at, OpID: p.opID},
	}, nil
}

func (p *recordingPurger) Erase(_ context.Context, actor chat.Actor, opID, channelID string,
	messageIDs []string, reason string) (chat.Written, error) {
	p.actor, p.opID, p.channel, p.erased, p.reason = actor, opID, channelID, messageIDs, reason
	return p.written()
}

func (p *recordingPurger) Prune(_ context.Context, actor chat.Actor, opID, channelID string,
	cutoff time.Time) (chat.Written, error) {
	p.actor, p.opID, p.channel, p.cutoff = actor, opID, channelID, cutoff
	return p.written()
}

func chatApp(t *testing.T, purger api.ChatPurger) *api.App {
	t.Helper()
	return chatAppWith(t, purger, nil)
}

// chatAppWith is [chatApp] under a posture the case adjusts first.
func chatAppWith(t *testing.T, purger api.ChatPurger, posture func(*config.Bootstrap)) *api.App {
	t.Helper()
	c, err := config.ParseCompany([]byte(chatCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b := chatPosture()
	if posture != nil {
		posture(&b)
	}
	return newApp(t, api.Options{
		Bootstrap:  &b,
		Sources:    queries.Sources{Company: func() *config.Company { return c }},
		ChatPurger: purger,
	})
}

// chatPost runs one authenticated request and returns the status and body.
func chatPost(t *testing.T, a *api.App, token, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.NewDecoder(rec.Result().Body).Decode(&decoded)
	return rec.Code, decoded
}

// The two destructive requests, confirmed, as every case below sends them.
const (
	confirmedErase = "/chat/channels/room-1/erase?confirm=2"
	eraseBody      = `{"message_ids":["m-1","m-2"],"reason":"a legal request"}`
	confirmedPrune = "/chat/channels/room-1/prune" +
		"?cutoff=2025-01-01T00:00:00Z&confirm=2025-01-01T00:00:00Z"
)

// A DESTRUCTION IS CREDENTIAL-SCOPED, as the purge and the node gate are: the
// credential is the authority, and its binding decides only how the act is
// named.
//
// A token bound to a person is recorded as that person, the token beside the
// seat. Every other credential — one no seat binds (a pipeline's, the ordinary
// shape of a CI token), a disabled guard's anonymous caller — is recorded
// under its own id with kind `operator`,
// exactly as a purge of a task records it. Refusing them was a narrower
// authority than the rest of the operator's destructive surface: the same
// pipeline could purge a task and evict a node from the chat log but could not
// erase a credential pasted into a room. Both routes, because a rule enforced
// at whichever route somebody remembered is a rule the next route does not
// have.
func TestAChatDestructionIsScopedToTheCredential(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		token, handle, operatorID string
		disabled                  bool
	}{
		"a token bound to a person": {token: "secret", handle: "founder", operatorID: "founder-token"},
		"a token bound to no seat":  {token: "pipeline", handle: "release-bot", operatorID: "release-bot"},
		"a disabled guard's caller": {token: "anything", disabled: true,
			handle: auth.AnonymousOperator, operatorID: auth.AnonymousOperator},
	} {
		for _, route := range []struct{ path, body string }{
			{confirmedErase, eraseBody},
			{confirmedPrune, ""},
		} {
			t.Run(name+" "+route.path, func(t *testing.T) {
				t.Parallel()
				purger := &recordingPurger{}
				a := chatApp(t, purger)
				if tc.disabled {
					a = chatAppWith(t, purger, func(b *config.Bootstrap) {
						b.API.Auth.Disabled = true
					})
				}
				code, body := chatPost(t, a, tc.token, route.path, route.body)
				if code != http.StatusOK {
					t.Fatalf("POST %s answered %d: %v", route.path, code, body)
				}
				want := chat.Actor{Handle: tc.handle, Kind: chat.AuthorOperator,
					OperatorID: tc.operatorID}
				if purger.actor.Handle != want.Handle || purger.actor.Kind != want.Kind ||
					purger.actor.OperatorID != want.OperatorID {
					t.Errorf("the destruction was recorded as %+v, want %+v",
						purger.actor, want)
				}
			})
		}
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
	purger := &recordingPurger{}
	a := chatApp(t, purger)

	for name, request := range map[string]struct{ path, body string }{
		"no confirmation": {"/chat/channels/room-1/erase", eraseBody},
		"the wrong count": {"/chat/channels/room-1/erase?confirm=1", eraseBody},
		"no reason":       {confirmedErase, `{"message_ids":["m-1","m-2"]}`},
		"no body":         {confirmedErase, ""},
		// A WORD THIS BUILD DOES NOT KNOW is refused rather than
		// ignored: on a gesture with no inverse, a misspelt field is a
		// different set of rows destroyed than the caller meant.
		"a misspelt field": {confirmedErase,
			`{"message_ids":["m-1","m-2"],"reasons":"a legal request"}`},
	} {
		code, body := chatPost(t, a, "secret", request.path, request.body)
		if code != http.StatusBadRequest {
			t.Errorf("an erase with %s answered %d: %v", name, code, body)
		}
	}
	if purger.calls != 0 {
		t.Fatalf("%d erases got through without a confirmation", purger.calls)
	}

	code, body := chatPost(t, a, "secret", confirmedErase, eraseBody)
	if code != http.StatusOK {
		t.Fatalf("a confirmed erase answered %d: %v", code, body)
	}
	if !slices.Equal(purger.erased, []string{"m-1", "m-2"}) || purger.channel != "room-1" {
		t.Errorf("the erase removed %v from %q", purger.erased, purger.channel)
	}
	if purger.reason != "a legal request" {
		t.Errorf("the erase carried reason %q", purger.reason)
	}
	// AND IT IS RECORDED AS AN OPERATOR'S ACT UNDER THE PERSON'S OWN SEAT.
	// The write path demands the kind in its own decide — an author that
	// could erase its own messages could erase the evidence of what it did
	// — and the handle is what says WHO did it.
	if purger.actor.Kind != chat.AuthorOperator {
		t.Errorf("the erase was recorded as a %q act", purger.actor.Kind)
	}
	if purger.actor.Handle != "founder" || purger.actor.OperatorID != "founder-token" {
		t.Errorf("the erase was attributed to %q under credential %q",
			purger.actor.Handle, purger.actor.OperatorID)
	}
}

// A PRUNE ECHOES ITS CUTOFF, because that instant decides how much of a room
// disappears and it is not a value to inherit from a shell history.
func TestThePruneRouteRefusesWithoutItsCutoffEchoed(t *testing.T) {
	t.Parallel()
	purger := &recordingPurger{}
	a := chatApp(t, purger)

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
	if purger.calls != 0 {
		t.Fatalf("%d prunes got through unconfirmed", purger.calls)
	}

	code, _ := chatPost(t, a, "secret", confirmedPrune, "")
	if code != http.StatusOK {
		t.Fatalf("a confirmed prune answered %d", code)
	}
	if !purger.cutoff.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the prune was published with cutoff %s", purger.cutoff)
	}
	if purger.actor.Kind != chat.AuthorOperator || purger.actor.Handle != "founder" {
		t.Errorf("the prune was attributed to %q as a %q act", purger.actor.Handle,
			purger.actor.Kind)
	}
}

// A DESTRUCTION ANSWERS AS A PURGE DOES: 200 for every outcome, the outcome
// and the operation beside the gesture's own echo, and NO POSITION FOR
// UNKNOWN.
//
// One convention among destructive operator routes, so a client — `crewlet
// chat prune`, a script — reads a chat destruction with the reader it reads a
// purge with. `pending` is durable and `unknown` is the one to retry; neither
// is a failure, and a zero position on `unknown` would read as a record at the
// log's origin when the gesture may never have reached the log at all.
func TestAChatDestructionAnswersAsAPurgeDoes(t *testing.T) {
	t.Parallel()
	for _, outcome := range []statelog.Outcome{
		statelog.OutcomeApplied, statelog.OutcomePending, statelog.OutcomeUnknown,
	} {
		for _, route := range []struct{ path, body, echo string }{
			{confirmedErase, eraseBody, "messages"},
			{confirmedPrune, "", "cutoff"},
		} {
			t.Run(string(outcome)+" "+route.echo, func(t *testing.T) {
				purger := &recordingPurger{outcome: outcome}
				code, body := chatPost(t, chatApp(t, purger), "secret", route.path, route.body)
				if code != http.StatusOK {
					t.Fatalf("a %s destruction answered %d: %v", outcome, code, body)
				}
				if got, _ := body["outcome"].(string); got != string(outcome) {
					t.Errorf("the body reports the outcome as %q", got)
				}
				if got, _ := body["op_id"].(string); got == "" || got != purger.opID {
					t.Errorf("the answer names op %q and the write was made under %q "+
						"— a retry is safe only under the operation that was sent",
						got, purger.opID)
				}
				if body["channel_id"] != "room-1" || body[route.echo] == nil {
					t.Errorf("the answer does not echo its gesture: %v", body)
				}
				_, positioned := body["position"]
				if positioned == (outcome == statelog.OutcomeUnknown) {
					t.Errorf("a %s answer carries a position: %t", outcome, positioned)
				}
			})
		}
	}
}

// THE OPERATION IS THE CALLER'S WHERE IT BROUGHT ONE, AND CARRIES ITS INSTANT
// AND ITS GESTURE WHERE IT DID NOT.
//
// A destruction that answered `unknown` is finished by sending the id it
// answered with: one sent with a fresh id is a second gesture. An id the route
// mints is [statelog.NewOpID]'s, whose instant is what the ledger judges a
// retry by — one carrying none reads as minted before every loss the ledger
// has had, and is answered `unknown` without ever being published — and it is
// named for the gesture, which is what a brought-back id is held to.
func TestAChatDestructionRunsUnderTheCallersOperation(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, route := range []struct{ name, path, body, gesture string }{
		{"erase", confirmedErase, eraseBody,
			chat.EraseOpName("room-1", []string{"m-1", "m-2"})},
		{"prune", confirmedPrune, "", chat.PruneOpName("room-1", cutoff)},
	} {
		t.Run(route.name, func(t *testing.T) {
			purger := &recordingPurger{}
			a := chatApp(t, purger)

			if code, body := chatPost(t, a, "secret", route.path, route.body); code != http.StatusOK {
				t.Fatalf("answered %d: %v", code, body)
			}
			if err := statelog.CheckCallerOpID(purger.opID); err != nil {
				t.Errorf("the route minted %q, which a retry could not be sent "+
					"under: %v", purger.opID, err)
			}
			if !statelog.OpNamed(purger.opID, route.gesture) {
				t.Errorf("the route minted %q, which is not named for its gesture %q "+
					"— its own retry would be refused", purger.opID, route.gesture)
			}

			brought := statelog.NewOpID(time.Now(), route.gesture)
			if code, body := chatPost(t, a, "secret", route.path+"&op_id="+brought,
				route.body); code != http.StatusOK {
				t.Fatalf("a retry answered %d: %v", code, body)
			}
			if purger.opID != brought {
				t.Errorf("a retry ran under %q, not the %q it brought", purger.opID, brought)
			}

			calls := purger.calls
			code, body := chatPost(t, a, "secret", route.path+"&op_id=not-an-op-id", route.body)
			if code != http.StatusBadRequest || body["error"] != "op_id_invalid" {
				t.Errorf("an id with no instant answered %d: %v", code, body)
			}
			if purger.calls != calls {
				t.Error("an id the ledger cannot judge was written under")
			}
		})
	}
}

// A BROUGHT-BACK OPERATION FINISHES ONLY THE GESTURE IT WAS MINTED FOR.
//
// An erase and a prune arbitrate on the ROOM, and a retry is collapsed onto
// the record its operation already holds there whatever that record says. So
// an erase's id carried to other messages in the same room — or a prune's to
// a later cutoff — was answered `applied` from the first gesture while nothing
// it named was destroyed: a compliance erasure reported done that did not
// happen. Each is refused before anything is sent, and the same gesture
// spelled another way is not.
func TestAChatDestructionHoldsABroughtOperationToItsGesture(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		path, body, opName string
		held               bool
	}{
		"an erase of other messages": {confirmedErase, eraseBody,
			chat.EraseOpName("room-1", []string{"m-1", "m-3"}), false},
		"an erase in another room": {confirmedErase, eraseBody,
			chat.EraseOpName("room-2", []string{"m-1", "m-2"}), false},
		"a prune's id on an erase": {confirmedErase, eraseBody,
			chat.PruneOpName("room-1", cutoff), false},
		"a prune to another cutoff": {confirmedPrune, "",
			chat.PruneOpName("room-1", cutoff.Add(24*time.Hour)), false},
		"the erase's own messages in another order": {confirmedErase, eraseBody,
			chat.EraseOpName("room-1", []string{"m-2", "m-1"}), true},
		"the prune's own cutoff": {confirmedPrune, "",
			chat.PruneOpName("room-1", cutoff), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			purger := &recordingPurger{}
			brought := statelog.NewOpID(now, tc.opName)
			code, body := chatPost(t, chatApp(t, purger), "secret",
				tc.path+"&op_id="+brought, tc.body)
			if tc.held {
				if code != http.StatusOK || purger.opID != brought {
					t.Errorf("the gesture's own id answered %d under %q: %v",
						code, purger.opID, body)
				}
				return
			}
			if code != http.StatusBadRequest || body["error"] != "op_id_invalid" {
				t.Errorf("an id minted for another gesture answered %d: %v", code, body)
			}
			if purger.calls != 0 {
				t.Errorf("an id minted for another gesture was written under %d "+
					"time(s) — it would be answered from that gesture's record", purger.calls)
			}
		})
	}
}

// AN OPERATION THAT ALREADY NAMES ANOTHER RECORD IS NOT A RETRY TO OFFER.
//
// Every other refusal a destruction meets is finished by the same id sent
// again, which is why each names it. This one is refused the same way under
// that id every time — the id is the problem — so it is `409 op_reused`,
// names no operation, and says to start afresh. Answered as this node's own
// failure it was a 503 carrying the id, and the command line told the operator
// to run it again with that same id for ever.
func TestAChatDestructionWhoseOperationNamesAnotherRecordIsNotOfferedARetry(t *testing.T) {
	t.Parallel()
	reused := &statelog.Unavailable{Reason: statelog.ReasonOpReused,
		Detail: "the operation already names a record on another room"}
	for _, route := range []struct{ path, body string }{
		{confirmedErase, eraseBody},
		{confirmedPrune, ""},
	} {
		purger := &recordingPurger{err: fmt.Errorf("estate: chat: %w", reused)}
		code, body := chatPost(t, chatApp(t, purger), "secret", route.path, route.body)
		if code != http.StatusConflict || body["error"] != string(statelog.ReasonOpReused) {
			t.Errorf("POST %s answered %d: %v", route.path, code, body)
		}
		if _, named := body["op_id"]; named {
			t.Errorf("POST %s offered the operation it can never finish under: %v",
				route.path, body)
		}
		if detail, _ := body["detail"].(string); !strings.Contains(detail, "no op_id") {
			t.Errorf("POST %s does not say to start afresh: %q", route.path, detail)
		}
	}
}

// A REFUSAL SAYS WHAT TO CHANGE, IN THE DOMAIN'S OWN WORDS — and a destruction
// nobody answered is UNKNOWN, never a failure.
//
// Each refusal class is a different thing for the caller to do, and its detail
// is [chat.Sentence]: the writer's words without the package's prefix, beside
// the operation it was made under. What is not a refusal is the node's own: a router that found nobody to run the
// gesture is 503 naming the operation, and one whose write went unanswered may
// have landed — answered as the write path's own `unknown`, so a caller
// retries under the same id rather than sending a second gesture.
func TestAChatDestructionIsRefusedByWhatItAsksOfItsCaller(t *testing.T) {
	t.Parallel()
	for name, expect := range map[string]struct {
		err     error
		status  int
		code    string
		outcome statelog.Outcome
	}{
		"a refused value": {
			err:    fmt.Errorf("chat: erase: %w", &chat.InvalidError{Field: "message_ids", Why: "too many"}),
			status: http.StatusBadRequest, code: "invalid"},
		"a room that is not there": {
			err: fmt.Errorf("%w: room-1", chat.ErrNotFound), status: http.StatusNotFound,
			code: "not_found"},
		"a permission": {
			err: fmt.Errorf("%w: not yours", chat.ErrForbidden), status: http.StatusForbidden,
			code: "forbidden"},
		"a room that kept changing": {
			err: fmt.Errorf("%w: busy", chat.ErrConflict), status: http.StatusConflict,
			code: "conflict"},
		// AN ARCHIVED ROOM IS NOT A CONFLICT and does not wrap one, so
		// without its own arm it read as this node's failure.
		"an archived room": {
			err: fmt.Errorf("%w: room-1", chat.ErrArchived), status: http.StatusConflict,
			code: "archived"},
		"nobody to run it": {
			err:    &estate.ErrUnserved{Detail: "node-a: out of service"},
			status: http.StatusServiceUnavailable, code: "unavailable"},
		"nobody answered": {
			err:    fmt.Errorf("%w (chat.prune, asked of node-a)", estate.ErrOutcomeUnknown),
			status: http.StatusOK, outcome: statelog.OutcomeUnknown},
		// A WALK THE REQUEST'S OWN DEADLINE CUT OFF was sent and may have
		// landed as surely as one nobody answered.
		"the walk ran out of time": {
			err:    fmt.Errorf("estate: chat.prune: %w", context.DeadlineExceeded),
			status: http.StatusOK, outcome: statelog.OutcomeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			purger := &recordingPurger{err: expect.err}
			code, body := chatPost(t, chatApp(t, purger), "secret", confirmedPrune, "")
			if code != expect.status {
				t.Fatalf("answered %d, want %d: %v", code, expect.status, body)
			}
			if expect.outcome != "" {
				if body["outcome"] != string(expect.outcome) || body["op_id"] != purger.opID {
					t.Errorf("an unanswered destruction is not answered as unknown "+
						"under its operation: %v", body)
				}
				if _, positioned := body["position"]; positioned {
					t.Error("an unknown destruction claims a position")
				}
				return
			}
			if body["error"] != expect.code {
				t.Errorf("refused as %v, want %q", body["error"], expect.code)
			}
			detail, _ := body["detail"].(string)
			if detail != chat.Sentence(expect.err) {
				t.Errorf("the detail is %q, want the domain's own sentence %q",
					detail, chat.Sentence(expect.err))
			}
			// EVERY REFUSAL NAMES THE OPERATION, as the purge's does: a
			// client keeps one rule for every destruction, and one the
			// route minted the id for would otherwise hold none.
			if purger.opID == "" || body["op_id"] != purger.opID {
				t.Errorf("a %s refusal does not name the operation it was made "+
					"under (%q): %v", expect.code, purger.opID, body)
			}
		})
	}
}

// NO ORDINARY CHAT GESTURE IS A ROUTE.
//
// Every one of them is a tool the dashboard calls through `/operator/act`
// (ADR-0024). A second path for one gesture would be a second identity, an
// unaudited write — the act transport leaves an `operator_acted` row and a
// route here would leave none — and a second answer to what happened. So with
// chat's destructive half wired, every path the branch once served answers the
// router's own refusal, and nothing reaches the write path.
func TestNoOrdinaryChatGestureIsARoute(t *testing.T) {
	t.Parallel()
	purger := &recordingPurger{}
	a := chatApp(t, purger)
	for _, path := range []string{
		"/chat/channels",
		"/chat/dms",
		"/chat/read",
		"/chat/channels/room-1/patch",
		"/chat/channels/room-1/members",
		"/chat/channels/room-1/join",
		"/chat/channels/room-1/leave",
		"/chat/channels/room-1/messages",
		"/chat/channels/room-1/messages/m-1/replies",
		"/chat/channels/room-1/messages/m-1/edit",
		"/chat/channels/room-1/messages/m-1/delete",
		"/chat/channels/room-1/messages/m-1/react",
		"/chat/channels/room-1/messages/m-1/unreact",
	} {
		code, body := chatPost(t, a, "secret", path, `{"body":"hi"}`)
		// 405 IS NOT A MOUNTED WRITE: several of these paths carry a GET
		// read, and a POST to one answers the mux's method refusal.
		if body["error"] != "no_route" && body["error"] != "method_not_allowed" {
			t.Errorf("POST %s answered %d %v — a chat gesture is served beside "+
				"the act transport", path, code, body)
		}
	}
	if purger.calls != 0 {
		t.Errorf("%d writes were made through a gesture route", purger.calls)
	}
}

// THE DESTRUCTIVE ROUTES ARE ABSENT ON A COMPANY THAT RUNS NO NATIVE CHAT.
//
// Not present and refusing: an endpoint that exists and answers 503 to
// everything reads as broken, while one that is not there matches what
// `chat.backend` says. The router's own `no_route` is the mark of a path
// nothing mounted — a mounted handler refusing writes a code of its own.
func TestTheChatDestructionsAreAbsentWithoutANativeChat(t *testing.T) {
	t.Parallel()
	a := chatApp(t, nil)
	for _, route := range []struct{ path, body string }{
		{confirmedErase, eraseBody},
		{confirmedPrune, ""},
	} {
		code, body := chatPost(t, a, "secret", route.path, route.body)
		if code != http.StatusNotFound || body["error"] != "no_route" {
			t.Errorf("POST %s on a company with no native chat answered %d %v",
				route.path, code, body)
		}
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
	a := chatApp(t, nil)
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
		"founder-token")
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
// A path nothing mounted answers the router's own `no_route`, while a route
// whose QUESTION has no source answers the query layer's `unknown_query`. The
// difference is the whole of what this asserts: these paths exist, and what a
// node without chat sources is missing is the source rather than the route.
func TestEveryChatReadRouteReachesTheQueryLayer(t *testing.T) {
	t.Parallel()
	a := chatApp(t, nil)
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
		var body map[string]any
		_ = json.NewDecoder(rec.Result().Body).Decode(&body)
		if body["error"] != "unknown_query" {
			t.Errorf("GET %s answered %d %v; a route that reached the registry "+
				"with no source answers unknown_query", path, rec.Code, body)
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

func (r publicRoom) ChannelHere(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chat.ChannelDetail, error) {
	return r.Channel(ctx, viewer, channelID, fresh)
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
		ChatLive:  live,
	})
	// THE HUB IS ITS OWNER'S TO RUN, never the App's — see
	// [TestTheAppNeitherStartsNorStopsTheChatHub].
	live.Start(t.Context())
	t.Cleanup(live.Stop)
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

	if kind := nextChatKind(t, bound, 10*time.Second); kind != stream.KindChatPosted {
		t.Fatalf("the bound socket was sent %q", kind)
	}
	// AND THE UNBOUND ONE IS SENT NOTHING. It is not a watcher at all, so
	// this is a read that must time out rather than a frame that must be
	// filtered — the seconds here are only to tell "not sent" from "not
	// sent yet", and the send it would race with is an in-memory queue
	// write on a goroutine that has already run for its neighbour.
	if kind := nextChatKind(t, unbound, 2*time.Second); kind != "" {
		t.Fatalf("a credential bound to no seat was sent %q", kind)
	}
}

// AN APPLY THAT UNBINDS A TOKEN CLOSES THE SOCKET IT OPENED, through the real
// credential chain.
//
// The hub resolves every socket's credential again on each frame it decides;
// the stream suite holds that with an injected viewer. What this asserts is
// the WIRING: the resolution reads the live epoch — a new company document,
// as an apply installs one, not an edit of the old — so a founder whose
// binding the new document drops is no longer shown the room, and their tab
// is closed to reconnect as whoever the token names now.
func TestAnApplyThatUnbindsATokenClosesItsSocket(t *testing.T) {
	t.Parallel()
	bound, err := config.ParseCompany([]byte(chatCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// THE SAME PERSON, NAMED BY ANOTHER IDENTITY: a human seat keeps one,
	// and the token is no longer it.
	unbound, err := config.ParseCompany([]byte(strings.Replace(chatCompany,
		"crewlet_operator_id: founder-token", "slack_user_id: U0FOUNDER", 1)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var live atomic.Pointer[config.Company]
	live.Store(bound)
	company := func() *config.Company { return live.Load() }
	b := chatPosture()
	hub := api.NewChatLive(api.ChatLiveOptions{
		Company: company,
		Rooms:   func() stream.ChatRooms { return publicRoom{} },
		NodeID:  "node-a",
	})
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Sources:   queries.Sources{Company: company},
		ChatLive:  hub,
	})
	hub.Start(t.Context())
	t.Cleanup(hub.Stop)
	a.Start(t.Context())
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	conn := dialChat(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/stream?token=secret")

	post := func(seq int64) {
		hub.Applied([]chat.Applied{{
			Position:   statelog.Position{Stream: "CREWLET_CHAT_LOG", Generation: 1, Seq: uint64(seq)},
			Subject:    chat.MessageSubject("room-1"),
			Op:         chat.OpPost,
			OpID:       "op-" + strconv.FormatInt(seq, 10),
			ChannelID:  "room-1",
			MessageID:  "m-" + strconv.FormatInt(seq, 10),
			ChannelSeq: seq,
			Actor:      "ceo",
			ActorKind:  chat.AuthorAgent,
			At:         time.Unix(1_700_000_000, 0).UTC(),
		}})
	}
	post(1)
	if kind := nextChatKind(t, conn, 10*time.Second); kind != stream.KindChatPosted {
		t.Fatalf("the bound founder was sent %q", kind)
	}

	live.Store(unbound)
	post(2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatal("a socket whose token the apply unbound was left open")
			}
			return
		}
		var env struct {
			Kind stream.Kind `json:"kind"`
		}
		if json.Unmarshal(raw, &env) == nil && strings.HasPrefix(string(env.Kind), "chat_") {
			t.Fatalf("a socket whose token the apply unbound was sent %q", env.Kind)
		}
	}
}

// ONE PERSON'S READ FLUSH IS NOBODY ELSE'S BUSINESS, through the real act
// transport and the read-state wiring a node runs ([api.NewEngineOperator]).
//
// A flush is pushed to its person's own sockets from inside the act request,
// and that request carries the seat the dispatch pinned for its caller. The
// push once re-resolved every socket on the node under it, so every
// credential resolved to the caller: each other person's socket was closed as
// a binding that moved, with a log line saying so falsely, on every read
// flush, mute and do-not-disturb anybody made — every fifteen seconds while a
// screen was open. So two bound people each hold a socket here, one of them
// flushes, and the other's socket must still be open and still be sent the
// next message.
func TestOnePersonsReadFlushLeavesEveryOtherPersonsSocketOpen(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(chatCompany + `  - name: Ops
    handle: ops
    kind: human
    contact:
      crewlet_operator_id: ops
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company := func() *config.Company { return c }
	chart := func() *org.Organization {
		o, err := c.Organization()
		if err != nil {
			t.Errorf("the test company does not derive: %v", err)
		}
		return o
	}
	b := chatPosture()
	b.API.Auth.Tokens = append(b.API.Auth.Tokens,
		config.APIToken{ID: "ops", Token: "ops-secret"})
	live := api.NewChatLive(api.ChatLiveOptions{
		Company: company,
		Rooms:   func() stream.ChatRooms { return publicRoom{} },
		NodeID:  "node-a",
	})
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Sources:   queries.Sources{Company: company},
		ChatLive:  live,
		Operator: operatorSurface(t, operator.Options{
			Chat: builtin.ChatDeps{
				Cursors: api.ChatReadsOver(coordmemory.NewFleet(), live),
				Actor:   operator.ChatActor(chart, nil),
			},
			Org:   chart,
			Audit: &recordedAudit{},
		}),
	})
	live.Start(t.Context())
	t.Cleanup(live.Stop)
	a.Start(t.Context())
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"
	founder := dialChat(t, base+"?token=secret")
	ops := dialChat(t, base+"?token=ops-secret")

	if status := actRequest(t, a, "secret", chat.MarkChatReadTool,
		`{"cursors":{"room-1":9}}`); status != http.StatusOK {
		t.Fatalf("the founder's flush answered %d", status)
	}
	kind, moved := nextChatPush(t, founder, 10*time.Second)
	if kind != stream.KindChatCursorMoved {
		t.Fatalf("the premise: the founder's own socket was sent %q, want %s",
			kind, stream.KindChatCursorMoved)
	}
	// NAMED BY ITS PERSON: the revision orders two states of one person
	// only, so a tab whose credential has since named somebody else must
	// be able to tell this state is not theirs.
	if moved["handle"] != "founder" {
		t.Errorf("the cursor push names %v, want the founder's own seat: %v",
			moved["handle"], moved)
	}

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
	// nextChatKind fails the case on a socket the server closed — which
	// is what the flush did to this one — and answers "" on one that is
	// open and was sent nothing.
	if kind := nextChatKind(t, ops, 10*time.Second); kind != stream.KindChatPosted {
		t.Fatalf("the other person's socket was sent %q after the founder's "+
			"flush, want the next message on a socket still open", kind)
	}
}

// A PERSON BOUND THROUGH THE NODE'S OWN CHAIN IS THAT PERSON on the two chat
// paths that resolve the caller themselves — a destruction and the socket —
// and not only on the act transport, which pins the seat through the same
// lookup before any chat actor runs. The binding is a `${VAR}` set in no
// process, only in the lookup the node hands its surfaces: resolved through the
// process environment instead, the founder's erase was recorded under their
// bare token rather than as them — once it was refused outright — and their
// socket was shown nothing, while every message they posted was theirs.
func TestAChatDestructionAndASocketResolveABindingThroughTheNodesChain(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_API_TEST_HANDED_CHAT_BINDING"
	if _, set := os.LookupEnv(variable); set {
		t.Fatalf("the premise: %s is set in no process", variable)
	}
	doc := strings.Replace(chatCompany, "crewlet_operator_id: founder-token",
		"crewlet_operator_id: ${"+variable+"}", 1)
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company := func() *config.Company { return c }
	env := func(name string) (string, bool) {
		if name == variable {
			return "founder-token", true
		}
		return "", false
	}
	live := api.NewChatLive(api.ChatLiveOptions{
		Company: company, Env: env,
		Rooms:  func() stream.ChatRooms { return publicRoom{} },
		NodeID: "node-a",
	})
	purger := &recordingPurger{}
	b := chatPosture()
	a := newApp(t, api.Options{
		Bootstrap:  &b,
		Sources:    queries.Sources{Company: company, Env: env},
		ChatPurger: purger,
		ChatLive:   live,
	})
	live.Start(t.Context())
	t.Cleanup(live.Stop)
	a.Start(t.Context())

	code, body := chatPost(t, a, "secret", confirmedErase, eraseBody)
	if code != http.StatusOK || purger.actor.Handle != "founder" {
		t.Errorf("the founder's erase answered %d %v as %q, want it made as the "+
			"founder the node's lookup binds the token to", code, body, purger.actor.Handle)
	}

	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	founder := dialChat(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/stream?token=secret")
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
	if kind := nextChatKind(t, founder, 10*time.Second); kind != stream.KindChatPosted {
		t.Errorf("the founder's socket was sent %q, want the post it may see", kind)
	}
}

// THE APP NEITHER STARTS NOR STOPS THE CHAT HUB.
//
// Half of what the hub runs is the FLEET's rather than this App's sockets: the
// answerer a peer's presence probe reaches is how a working indicator raised on
// one node shows in a room open on another, and the node running a seat's turn
// is rarely the node a browser is attached to. Started by the App, it ran only
// where a listener did — so a node with no ingress role, or `api.port: 0`,
// recorded every indicator its seats raised and answered no probe at all. And
// stopped by the App, it went dark when the listener closed, which on a node
// that does serve one is before the engine's teardown it has to precede.
//
// So this asks the fleet's own subject, as a peer would: nothing answers while
// the App runs and the hub's owner has not started it, the hub answers once it
// has, and it still answers after the App has stopped.
func TestTheAppNeitherStartsNorStopsTheChatHub(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(chatCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	broker := qmemory.NewBroker()
	client := func() *qmemory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start a queue client: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		return q
	}
	node, peer := client(), client()
	live := api.NewChatLive(api.ChatLiveOptions{
		Company: func() *config.Company { return c },
		Rooms:   func() stream.ChatRooms { return publicRoom{} },
		Peers:   func() stream.ChatPeers { return node },
		NodeID:  "node-a",
	})
	// A SEAT WORKING IN A ROOM ON THIS NODE, which is what a peer's probe
	// is asking after.
	live.SetStatus(t.Context(), "ceo", "room-1", "", "reading the room")
	b := chatPosture()
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Sources:   queries.Sources{Company: func() *config.Company { return c }},
		ChatLive:  live,
	})
	probe := func(wait time.Duration) bool {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), wait)
		defer cancel()
		replies, err := peer.Ask(ctx, topics.ChatPresence,
			[]byte(`{"version":1,"channels":["room-1"]}`), 0)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("probe: %v", err)
		}
		for _, reply := range replies {
			if strings.Contains(string(reply), `"ceo"`) {
				return true
			}
		}
		return false
	}
	// ASKED AGAIN AND AGAIN, never once: a scatter nobody serves returns at
	// once, so a single probe right after a hub's loops started would race
	// its subscription and read "not serving" either way. answersWithin
	// keeps asking for the whole window — long enough for a hub somebody
	// started to have subscribed many times over, which is what makes the
	// "nobody answered" half mean something.
	answersWithin := func(window time.Duration) bool {
		t.Helper()
		for deadline := time.Now().Add(window); time.Now().Before(deadline); {
			if probe(100 * time.Millisecond) {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	answers := func() bool { return answersWithin(10 * time.Second) }

	a.Start(t.Context())
	if answersWithin(time.Second) {
		t.Fatal("the hub answered a peer's probe once the App started — the App " +
			"ran a hub whose owner had not, so a node without one runs none")
	}
	live.Start(t.Context())
	t.Cleanup(live.Stop)
	if !answers() {
		t.Fatal("a hub its owner started never answered a peer's probe")
	}
	a.Stop()
	if !answers() {
		t.Error("the hub stopped answering when the App stopped — the listener's " +
			"lifetime, not the node's")
	}
}

// dialChat opens a socket and reads through its opening snapshot.
//
// THE SNAPSHOT IS WHAT SAYS THE SOCKET IS WATCHING: the chat arm registers it
// before the socket's writer starts, so any frame at all proves the
// registration — and reading through the snapshot, past whatever the shared
// fan-out sent ahead of it (the roster, a health tick), is what stops a frame
// announced next being raced against a socket not yet in the registry.
func dialChat(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for the snapshot: %v", err)
		}
		var env struct {
			Kind stream.Kind `json:"kind"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if env.Kind == stream.KindSnapshot {
			return conn
		}
	}
}

// nextChatKind reads frames until one is a chat frame, answering its kind — or
// "" when none arrives inside wait.
//
// EVERY OTHER FRAME IS SKIPPED, whatever it is: the health tick, the roster
// and every push the shared fan-out sends on its own schedule are nobody's
// business here, and a reader that stopped at the first frame that was not a
// health tick failed the day the socket opened with one more of them.
func nextChatKind(t *testing.T, conn *websocket.Conn, wait time.Duration) stream.Kind {
	t.Helper()
	kind, _ := nextChatPush(t, conn, wait)
	return kind
}

// nextChatPush is [nextChatKind] with the frame's data beside its kind.
func nextChatPush(t *testing.T, conn *websocket.Conn,
	wait time.Duration) (stream.Kind, map[string]any) {

	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wait)
	defer cancel()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", nil
			}
			t.Fatalf("reading the socket: %v", err)
		}
		var env struct {
			Kind stream.Kind    `json:"kind"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if strings.HasPrefix(string(env.Kind), "chat_") {
			return env.Kind, env.Data
		}
	}
}

// NO PUSH KIND IS ALSO A QUESTION.
//
// An envelope carries a `kind` and nothing in it says which direction the
// frame was travelling, so a name in both namespaces is a frame a reader
// cannot interpret without knowing who sent it.
//
// HELD AGAINST WHAT THE REGISTRY ACTUALLY REGISTERS, not a list typed here: the
// app is handed a chat reader and a chat searcher, so [queries.Register] adds
// every chat question this build has — and a new one whose name collides with
// a push kind (a `chat_presence` question, say) fails here the day it lands.
func TestNoChatPushKindIsAlsoARegisteredQuestion(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(chatCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b := chatPosture()
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Sources: queries.Sources{
			Company: func() *config.Company { return c },
			Chat:    emptyChatReader{}, ChatSearch: emptyChatReader{},
		},
	})
	names := a.Queries().Names()
	asked := 0
	for _, name := range names {
		if strings.HasPrefix(name, "chat_") {
			asked++
		}
	}
	if asked == 0 {
		t.Fatalf("the registry holds no chat question at all (%v), so this gate "+
			"compares the push kinds against nothing", names)
	}
	for _, kind := range stream.ChatPushKinds() {
		if slices.Contains(names, string(kind)) {
			t.Errorf("%q is both a push kind and a question", kind)
		}
	}
}

// emptyChatReader is chat's read side and its search with nothing in them:
// what [TestNoChatPushKindIsAlsoARegisteredQuestion] hands the registry so it
// registers every chat question, where the answers do not matter.
type emptyChatReader struct{}

func (emptyChatReader) Channels(context.Context, string, chat.ChannelsQuery,
	statelog.Freshness) (chat.ChannelListing, error) {
	return chat.ChannelListing{}, nil
}

func (emptyChatReader) Channel(context.Context, string, string,
	statelog.Freshness) (chat.ChannelDetail, error) {
	return chat.ChannelDetail{}, nil
}

func (emptyChatReader) Messages(context.Context, string, chat.TranscriptQuery,
	statelog.Freshness) (chat.Transcript, error) {
	return chat.Transcript{}, nil
}

func (emptyChatReader) Thread(context.Context, string, chat.ThreadQuery,
	statelog.Freshness) (chat.Thread, error) {
	return chat.Thread{}, nil
}

func (emptyChatReader) Mentions(context.Context, string, chat.MentionQuery,
	statelog.Freshness) (chat.MentionFeed, error) {
	return chat.MentionFeed{}, nil
}

func (emptyChatReader) Search(context.Context, estate.ChatQuery) (estate.ChatSearchAnswer, error) {
	return estate.ChatSearchAnswer{}, nil
}
