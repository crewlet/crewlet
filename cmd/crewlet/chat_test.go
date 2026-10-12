package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet chat`, and the four things about it that nothing else would catch.
//
// The verbs are thin over routes the API already serves, so what is worth a
// test here is not that a GET reaches a handler. It is the places where this
// CLI has to KNOW something about chat that no compiler checks: that a post is
// the dashboard's own tool over the act transport and a prune the purge's
// shape, that an outcome nobody can vouch for names what finishes it, that no
// verb may name an author, that a cutoff is typed twice, and that the list of
// verbs, the switch and the usage text are three copies of one set.

// chatRouteFake is a node that answers whatever a test told it to, and
// remembers what it was asked.
type chatRouteFake struct {
	server *httptest.Server

	status int
	body   string

	// hangUp drops the connection without an answer, as a node that died
	// mid-request or a proxy that gave up does, after handing what it was
	// sent to hungUp: a dropped connection carries nothing a reader of the
	// fields below could synchronise on. Its value is the request id a
	// post carried, or the query a prune did.
	hangUp bool
	hungUp chan string

	method string
	path   string
	query  string
	sent   json.RawMessage
	token  string
	calls  int
}

func newChatRouteFake(t *testing.T, status int, body string) *chatRouteFake {
	t.Helper()
	fake := &chatRouteFake{status: status, body: body, hungUp: make(chan string, 1)}
	fake.server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			fake.calls++
			fake.method, fake.path = r.Method, r.URL.Path
			fake.query = r.URL.RawQuery
			fake.token = r.Header.Get("Authorization")
			raw := new(bytes.Buffer)
			if _, err := raw.ReadFrom(r.Body); err != nil {
				t.Errorf("reading the request body: %v", err)
			}
			if raw.Len() > 0 {
				fake.sent = json.RawMessage(raw.Bytes())
			}
			if fake.hangUp {
				fake.hungUp <- firstNonEmpty(fake.requestIDOf(), fake.query)
				if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fake.status)
			_, _ = w.Write([]byte(fake.body))
		}))
	t.Cleanup(fake.server.Close)
	return fake
}

// requestIDOf is the act request id the last body carried, or "".
func (f *chatRouteFake) requestIDOf() string {
	var sent struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(f.sent, &sent)
	return sent.RequestID
}

// requestID is the act request id the node was sent, failing the case if it
// was sent none.
func (f *chatRouteFake) requestID(t *testing.T) string {
	t.Helper()
	id := f.requestIDOf()
	if id == "" {
		t.Fatalf("the node was sent no request_id: %s", f.sent)
	}
	return id
}

// chatCLI runs one `crewlet chat` invocation against a fake node.
func chatCLI(t *testing.T, base string, args ...string) (string, string, error) {
	t.Helper()
	full := append([]string{"chat"}, args...)
	full = append(full, "-url", base, "-token", "t0ken")
	return cli(t, full...)
}

// EVERY VERB IS IN THE LIST, IN THE SWITCH AND IN THE USAGE TEXT.
//
// Nothing connects the three. A name in the list that no case handles reaches
// an "is listed but not dispatched" refusal; a name the switch handles that
// the list omits is refused before it is ever dispatched, because the guard
// runs first; and either one missing from the usage text is a command an
// operator is only ever told about by reading the source. This is the `chat`
// twin of TestUsageAdvertisesEveryDispatchedCommand, which covers the other
// direction — that `crewlet chat` itself is advertised by the binary's usage.
func TestEveryChatVerbIsDispatchedAndDocumented(t *testing.T) {
	t.Parallel()

	// The node answers a refusal to everything: what is under test is
	// which code path the name reached, never whether the call worked.
	fake := newChatRouteFake(t, http.StatusServiceUnavailable, `{"error":"no_chat"}`)
	for _, verb := range chatSubcommands {
		if !strings.Contains(chatUsage, "crewlet chat "+verb+" ") {
			t.Errorf("chatUsage never names %q, so an operator is only told "+
				"about it by reading the source", verb)
		}
		_, _, err := chatCLI(t, fake.server.URL, verb)
		if err != nil && strings.Contains(err.Error(), "unknown chat command") {
			t.Errorf("%q is listed but the switch does not dispatch it", verb)
		}
		if err != nil && strings.Contains(err.Error(), "is listed but not dispatched") {
			t.Errorf("%q reached the switch's unreachable arm", verb)
		}
	}

	_, _, err := chatCLI(t, fake.server.URL, "nonesuch")
	if err == nil || !strings.Contains(err.Error(), "unknown chat command") {
		t.Errorf("an unknown verb gave %v, want the unknown-command refusal", err)
	}
	// AND A MISSPELLED VERB CARRYING ANOTHER VERB'S FLAG is still reported
	// as an unknown COMMAND. The guard runs before any flag set is built
	// precisely so the operator is sent to the word they typed wrong
	// rather than to a flag that was never the problem.
	_, _, err = chatCLI(t, fake.server.URL, "reed", "-limit", "5")
	if err == nil || !strings.Contains(err.Error(), "unknown chat command") {
		t.Errorf("`chat reed -limit 5` gave %v, want the unknown-command refusal", err)
	}

	var help bytes.Buffer
	usage(&help)
	if !strings.Contains(help.String(), "crewlet chat ") {
		t.Errorf("the binary's usage never names `crewlet chat`:\n%s", help.String())
	}
}

// A POST IS THE COMPOSER'S TOOL, OVER THE ROUTE THE DASHBOARD WRITES THROUGH.
//
// `post_message` through `POST /operator/act/{tool}` (ADR-0024), with a JSON
// body carrying a UUIDv7 request id and the tool's own arguments — so a line
// said from a terminal and one said from a screen are one gesture with one
// record, attributed to the person the token is bound to. And no argument names
// an author: the server resolves the credential to a seat, and a body carrying
// a handle would be a second answer to the only question that decides what a
// transcript may show.
func TestAChatPostIsTheComposersToolOverTheActTransport(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusOK, `{"tool":"post_message",
		"outcome":"applied","position":"CREWLET_CHAT_LOG@1:7",
		"receipt":{"message_id":"m-1","channel":"c-eng","outcome":"applied",
		"position":"CREWLET_CHAT_LOG@1:7"}}`)
	out, _, err := chatCLI(t, fake.server.URL, "post", "#eng",
		"-body", "hello", "-link", "https://example.com/x")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if fake.method != http.MethodPost || fake.path != "/operator/act/post_message" {
		t.Errorf("the post went to %s %s, want the act transport's post_message",
			fake.method, fake.path)
	}
	var sent struct {
		RequestID string         `json:"request_id"`
		Args      map[string]any `json:"args"`
	}
	if err := json.Unmarshal(fake.sent, &sent); err != nil {
		t.Fatalf("the body sent was not JSON: %v (%s)", err, fake.sent)
	}
	if id, err := uuid.Parse(sent.RequestID); err != nil || id.Version() != 7 {
		t.Errorf("request_id %q is not a UUIDv7: the act transport refuses it",
			sent.RequestID)
	}
	if sent.Args["channel"] != "#eng" || sent.Args["body"] != "hello" ||
		!reflect.DeepEqual(sent.Args["links"], []any{"https://example.com/x"}) {
		t.Errorf("args = %v, want the room as typed, the body and the link", sent.Args)
	}
	for _, forbidden := range []string{"author", "author_kind", "handle", "as",
		"operation_id"} {
		if _, named := sent.Args[forbidden]; named {
			t.Errorf("the post named %q: %s", forbidden, fake.sent)
		}
	}
	if fake.token != "Bearer t0ken" {
		t.Errorf("the post sent %q rather than the credential the server "+
			"resolves to a person", fake.token)
	}
	if !strings.Contains(out, "post m-1 in #eng: applied at CREWLET_CHAT_LOG@1:7") {
		t.Errorf("the post printed:\n%s", out)
	}
}

// EVERY OUTCOME IS SAID FOR WHAT IT IS, and the two nobody can vouch for name
// the request that finishes them.
//
// `pending` is a published message the node has not applied, which reads as
// nothing having happened if the operator then reads the room — so it says
// not to post again. `unknown` arrives as the act transport's 503 carrying the
// outcome, whose class alone is a refusal's; and a request nobody answered may
// have posted all the same. In both the request id is the only handle on the
// message, and it was minted inside this command.
func TestAChatPostSaysEveryOutcomeAndHowToFinishAnUnknownOne(t *testing.T) {
	t.Parallel()
	t.Run("pending", func(t *testing.T) {
		t.Parallel()
		fake := newChatRouteFake(t, http.StatusOK, `{"outcome":"pending",
			"position":"CREWLET_CHAT_LOG@1:7","receipt":{"message_id":"m-1"}}`)
		out, _, err := chatCLI(t, fake.server.URL, "post", "c-eng", "-body", "hello")
		if err != nil {
			t.Fatalf("a pending post came back as a failure: %v", err)
		}
		if !strings.Contains(out, "pending") || !strings.Contains(out, "Do not run this again") {
			t.Errorf("a pending post printed:\n%s", out)
		}
	})
	t.Run("answered unknown", func(t *testing.T) {
		t.Parallel()
		fake := newChatRouteFake(t, http.StatusServiceUnavailable,
			`{"error":"unavailable","tool":"post_message","outcome":"unknown",
			"detail":"Whether the message landed is unknown"}`)
		out, _, err := chatCLI(t, fake.server.URL, "post", "c-eng", "-body", "hello")
		if err != nil {
			t.Fatalf("an unknown post came back as a failure: %v", err)
		}
		if !strings.Contains(out, "-request-id "+fake.requestID(t)) ||
			!strings.Contains(out, "Whether the message landed is unknown") {
			t.Errorf("an unknown post does not name the request to retry and the "+
				"node's reason:\n%s", out)
		}
	})
	t.Run("not answered", func(t *testing.T) {
		t.Parallel()
		fake := newChatRouteFake(t, http.StatusOK, "")
		fake.hangUp = true
		_, stderr, err := chatCLI(t, fake.server.URL, "post", "c-eng", "-body", "hello")
		if err == nil {
			t.Fatal("a post nobody answered was reported as said")
		}
		if sent := <-fake.hungUp; !strings.Contains(stderr, "-request-id "+sent) {
			t.Errorf("a post nobody answered does not name the request to retry:\n%s",
				stderr)
		}
	})
}

// THE SAME REQUEST ID IS WHAT MAKES A RETRY ONE MESSAGE. The transport derives
// the post's operation from it, so the retry has to carry the id the first
// attempt printed — a fresh one would be a second remark.
func TestAChatPostRetriesUnderTheRequestIDItIsGiven(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusOK,
		`{"outcome":"applied","receipt":{"message_id":"m-1"}}`)
	first := uuid.Must(uuid.NewV7()).String()
	if _, _, err := chatCLI(t, fake.server.URL, "post", "c-eng",
		"-body", "hello", "-request-id", first); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got := fake.requestID(t); got != first {
		t.Errorf("-request-id %s sent %q", first, got)
	}
}

// A REFUSAL IS STILL A REFUSAL, carrying the node's remedy: a token that names
// nobody is told which seat to bind it on, not that it is wrong.
func TestAChatPostUnderAnUnboundTokenCarriesTheRemedy(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusForbidden,
		`{"error":"unbound","tool":"post_message",`+
			`"detail":"the token \"ci\" is bound to no seat",`+
			`"hint":"give a human seat contact.crewlet_operator_id: ci"}`)
	_, _, err := chatCLI(t, fake.server.URL, "post", "c-eng", "-body", "hello")
	if err == nil {
		t.Fatal("a refusal was read as a successful write")
	}
	if !strings.Contains(err.Error(), "contact.crewlet_operator_id: ci") ||
		strings.Contains(err.Error(), "check it against the") {
		t.Errorf("the refusal reads %q, want the node's remedy", err)
	}
}

// A DESTRUCTION UNDER AN UNBOUND TOKEN READS AS THE ACT TRANSPORT'S REFUSAL.
//
// The destructive routes refuse a token no seat binds under the act
// transport's own code, `unbound`, so the sentence this CLI keeps for that
// refusal — the token is fine and names nobody — is the one an operator reads
// on a prune too, rather than a generic "the node answered 403".
func TestAChatPruneUnderAnUnboundTokenReadsAsTheActTransportsRefusal(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusForbidden,
		`{"error":"unbound","detail":"the token \"ci\" is bound to no seat, and a `+
			`message's author is a seat — set contact.crewlet_operator_id: ci"}`)
	_, _, err := chatCLI(t, fake.server.URL, "prune", "c-eng",
		"-cutoff", "2031-03-24T04:35:00Z", "-confirm", "2031-03-24T04:35:00Z")
	if err == nil {
		t.Fatal("a refused prune was reported as done")
	}
	if !strings.Contains(err.Error(), "bound to no person") ||
		!strings.Contains(err.Error(), "contact.crewlet_operator_id: ci") {
		t.Errorf("an unbound prune reads %q, want the unbound sentence and the "+
			"node's remedy", err)
	}
}

// A NODE THAT SERVES NO CHAT IS TOLD WHY, whichever of chat's surfaces asked.
//
// Each refusal is right in its own words — a question nobody registered, a
// route nobody mounted, a tool the catalogue does not carry — and none of them
// names the one thing that explains all three: chat exists only where
// `chat.backend` is `native`, and its writes only on a node that may publish.
func TestAChatVerbOnANodeServingNoChatNamesTheAxis(t *testing.T) {
	t.Parallel()
	for _, verb := range []struct {
		name string
		code string
		args []string
	}{
		{"post", "unknown_tool", []string{"post", "c-eng", "-body", "hello"}},
		{"prune", "no_route", []string{"prune", "c-eng",
			"-cutoff", "2031-03-24T04:35:00Z", "-confirm", "2031-03-24T04:35:00Z"}},
		{"erase", "no_route", []string{"erase", "c-eng", "-message", "m-1",
			"-reason", "a pasted key", "-confirm", "1"}},
		{"read", "unknown_query", []string{"read", "c-eng"}},
		{"channels", "unknown_query", []string{"channels"}},
		{"search", "unknown_query", []string{"search", "launch"}},
	} {
		t.Run(verb.name, func(t *testing.T) {
			t.Parallel()
			fake := newChatRouteFake(t, http.StatusNotFound,
				`{"error":"`+verb.code+`","detail":"not here"}`)
			_, _, err := chatCLI(t, fake.server.URL, verb.args...)
			if err == nil {
				t.Fatal("a 404 was read as an answer")
			}
			if !strings.Contains(err.Error(), "chat.backend") ||
				!strings.Contains(err.Error(), verb.code) {
				t.Errorf("%s on a node serving no chat reads %q, which never names "+
					"the axis, or loses the node's own code", verb.name, err)
			}
		})
	}
}

// A PRUNE IS IN THE PURGE'S SHAPE: it carries its own operation, and reads
// every outcome from a 200.
//
// The id is minted HERE, before the request, as the route would mint it, so a
// prune whose answer was lost is finished by sending that id — minted by the
// node, it would have been lost with the answer. `unknown` prints no position,
// which is the whole content of unknown, and names the id to retry with.
func TestAChatPruneCarriesItsOwnOperationAndSaysEveryOutcome(t *testing.T) {
	t.Parallel()
	confirmed := []string{"prune", "c-eng",
		"-cutoff", "2031-03-24T04:35:00Z", "-confirm", "2031-03-24T04:35:00Z"}

	fake := newChatRouteFake(t, http.StatusOK, `{"channel_id":"c-eng",
		"outcome":"unknown","op_id":"`+opIDMinted+`"}`)
	out, _, err := chatCLI(t, fake.server.URL, confirmed...)
	if err != nil {
		t.Fatalf("an unknown prune came back as a failure: %v", err)
	}
	sent, err := url.ParseQuery(fake.query)
	if err != nil {
		t.Fatal(err)
	}
	if _, minted := statelog.OpMintedAt(sent.Get("op_id")); !minted {
		t.Errorf("the prune sent op_id %q, which carries no instant: a node "+
			"whose ledger lost a row could never vouch for its retry",
			sent.Get("op_id"))
	}
	// NAMED FOR THIS PRUNE, as the route names its own: the route holds a
	// brought-back id to that name, so one minted under any other would be
	// refused on the retry it exists for.
	prune := chat.PruneOpName("c-eng", time.Date(2031, 3, 24, 4, 35, 0, 0, time.UTC))
	if !statelog.OpNamed(sent.Get("op_id"), prune) {
		t.Errorf("the prune sent op_id %q, not one named for it (%q)",
			sent.Get("op_id"), prune)
	}
	if !strings.Contains(out, "-op-id "+opIDMinted) || strings.Contains(out, " at ") {
		t.Errorf("an unknown prune printed:\n%s\nwant the id to retry with and "+
			"no position", out)
	}

	retry := newChatRouteFake(t, http.StatusOK, `{"channel_id":"c-eng",
		"outcome":"applied","op_id":"`+opIDMinted+`",
		"position":{"stream":"CREWLET_CHAT_LOG","seq":9}}`)
	out, _, err = chatCLI(t, retry.server.URL, append(confirmed, "-op-id", opIDMinted)...)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again, _ := url.ParseQuery(retry.query); again.Get("op_id") != opIDMinted {
		t.Errorf("-op-id %s sent %q", opIDMinted, again.Get("op_id"))
	}
	if !strings.Contains(out, "applied at CREWLET_CHAT_LOG 9") {
		t.Errorf("the retried prune printed:\n%s", out)
	}

	// A REFUSAL NAMES THE OPERATION TOO. The route answers a prune no data
	// node would run as 503 carrying its op_id; a retry under it is safe
	// whatever happened, and a fresh one is a second prune if anything did.
	refused := newChatRouteFake(t, http.StatusServiceUnavailable,
		`{"error":"unavailable","detail":"no data node serves the estate",`+
			`"op_id":"`+opIDMinted+`"}`)
	_, stderr, err := chatCLI(t, refused.server.URL, confirmed...)
	if err == nil {
		t.Fatal("a refused prune was reported as done")
	}
	if !strings.Contains(stderr, "-op-id "+opIDMinted) {
		t.Errorf("a refused prune does not name the operation it carried:\n%s", stderr)
	}

	// EXCEPT THE ONE REFUSAL NO RETRY UNDER IT CAN FINISH: an id that
	// already names a record on something else. Offered as a retry, the
	// operator would be refused the same way for ever.
	reused := newChatRouteFake(t, http.StatusConflict,
		`{"error":"op_reused","detail":"the op_id already names a record on something else"}`)
	_, stderr, err = chatCLI(t, reused.server.URL, append(confirmed, "-op-id", opIDMinted)...)
	if err == nil {
		t.Fatal("a prune refused op_reused was reported as done")
	}
	if !strings.Contains(stderr, "without -op-id") || strings.Contains(stderr, "with -op-id") {
		t.Errorf("a prune whose operation names another record was offered a retry "+
			"under it:\n%s", stderr)
	}

	lost := newChatRouteFake(t, http.StatusOK, "")
	lost.hangUp = true
	_, stderr, err = chatCLI(t, lost.server.URL, confirmed...)
	if err == nil {
		t.Fatal("a prune nobody answered was reported as done")
	}
	if dropped, _ := url.ParseQuery(<-lost.hungUp); dropped.Get("op_id") == "" ||
		!strings.Contains(stderr, "-op-id "+dropped.Get("op_id")) {
		t.Errorf("a prune nobody answered does not name its operation:\n%s", stderr)
	}
}

// AN ERASE IS THE PRUNE'S SHAPE, AIMED AT THE ROWS NAMED.
//
// The case it is for — a credential pasted into one message — is one a prune
// answers by destroying everything the room said before an instant. An erase
// sends the ids and the reason as the route's JSON body, the COUNT as its
// confirmation, and an operation minted here, before the request; every
// outcome reads as a prune's, and a request nobody answered names the id that
// finishes it.
func TestAChatEraseNamesItsRowsAndCarriesItsOwnOperation(t *testing.T) {
	t.Parallel()
	confirmed := []string{"erase", "c-eng", "-message", "m-1", "-message", "m-2",
		"-reason", "a pasted key", "-confirm", "2"}

	fake := newChatRouteFake(t, http.StatusOK, `{"channel_id":"c-eng","messages":2,
		"outcome":"applied","op_id":"`+eraseMinted+`",
		"position":{"stream":"CREWLET_CHAT_LOG","seq":9}}`)
	out, _, err := chatCLI(t, fake.server.URL, confirmed...)
	if err != nil {
		t.Fatalf("erase: %v", err)
	}
	if fake.method != http.MethodPost || fake.path != "/chat/channels/c-eng/erase" {
		t.Errorf("the erase went to %s %s", fake.method, fake.path)
	}
	var sent struct {
		MessageIDs []string `json:"message_ids"`
		Reason     string   `json:"reason"`
	}
	if err := json.Unmarshal(fake.sent, &sent); err != nil {
		t.Fatalf("the body sent was not JSON: %v (%s)", err, fake.sent)
	}
	if !reflect.DeepEqual(sent.MessageIDs, []string{"m-1", "m-2"}) ||
		sent.Reason != "a pasted key" {
		t.Errorf("the erase sent %s, want the ids and the reason", fake.sent)
	}
	query, err := url.ParseQuery(fake.query)
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("confirm") != "2" {
		t.Errorf("the erase confirmed %q, want the count", query.Get("confirm"))
	}
	if _, minted := statelog.OpMintedAt(query.Get("op_id")); !minted {
		t.Errorf("the erase sent op_id %q, which carries no instant", query.Get("op_id"))
	}
	if erase := chat.EraseOpName("c-eng", []string{"m-1", "m-2"}); !statelog.OpNamed(
		query.Get("op_id"), erase) {
		t.Errorf("the erase sent op_id %q, not one named for it (%q)",
			query.Get("op_id"), erase)
	}
	if !strings.Contains(out, "applied at CREWLET_CHAT_LOG 9") ||
		!strings.Contains(out, "offline or evicted") {
		t.Errorf("the erase printed:\n%s", out)
	}

	unknown := newChatRouteFake(t, http.StatusOK, `{"channel_id":"c-eng",
		"outcome":"unknown","op_id":"`+eraseMinted+`"}`)
	out, _, err = chatCLI(t, unknown.server.URL, append(confirmed, "-op-id", eraseMinted)...)
	if err != nil {
		t.Fatalf("an unknown erase came back as a failure: %v", err)
	}
	if again, _ := url.ParseQuery(unknown.query); again.Get("op_id") != eraseMinted {
		t.Errorf("-op-id %s sent %q", eraseMinted, again.Get("op_id"))
	}
	if !strings.Contains(out, "-op-id "+eraseMinted) || strings.Contains(out, " at ") {
		t.Errorf("an unknown erase printed:\n%s\nwant the id and no position", out)
	}

	lost := newChatRouteFake(t, http.StatusOK, "")
	lost.hangUp = true
	_, stderr, err := chatCLI(t, lost.server.URL, confirmed...)
	if err == nil {
		t.Fatal("an erase nobody answered was reported as done")
	}
	if dropped, _ := url.ParseQuery(<-lost.hungUp); dropped.Get("op_id") == "" ||
		!strings.Contains(stderr, "-op-id "+dropped.Get("op_id")) {
		t.Errorf("an erase nobody answered does not name its operation:\n%s", stderr)
	}
}

// AN ERASE IS REFUSED BEFORE IT IS SENT when its count, its reason or its
// messages are missing. The rows go on every node with no inverse, so a typo
// costs nothing: no request reaches the node at all.
func TestAChatEraseRefusesWhatTheRouteWouldBeforeItIsSent(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusOK, `{"outcome":"applied"}`)
	for name, args := range map[string][]string{
		"no messages": {"erase", "c-eng", "-reason", "why", "-confirm", "0"},
		"no reason":   {"erase", "c-eng", "-message", "m-1", "-confirm", "1"},
		"no count":    {"erase", "c-eng", "-message", "m-1", "-reason", "why"},
		"the wrong count": {"erase", "c-eng", "-message", "m-1", "-message", "m-2",
			"-reason", "why", "-confirm", "1"},
		"no room": {"erase", "-message", "m-1", "-reason", "why", "-confirm", "1"},
	} {
		if _, _, err := chatCLI(t, fake.server.URL, args...); err == nil {
			t.Errorf("an erase with %s was accepted", name)
		}
	}
	if fake.calls != 0 {
		t.Errorf("a refused erase still reached the node %d time(s)", fake.calls)
	}
}

// eraseMinted is an erase's operation id in the engine's grammar.
var eraseMinted = statelog.NewOpID(time.Date(2031, 3, 24, 4, 35, 0, 0, time.UTC),
	"chat-erase-c-eng")

// opIDMinted is an operation id in the engine's grammar, as a route answers one.
var opIDMinted = statelog.NewOpID(time.Date(2031, 3, 24, 4, 35, 0, 0, time.UTC),
	"chat-prune-c-eng")

// THE CUTOFF IS TYPED TWICE, and the second one is checked here rather than
// only at the route.
//
// Everything the room said before the instant is destroyed on every node with
// no inverse, so the value that decides how much of a conversation disappears
// is not one to inherit from a shell history. Refusing here also means a typo
// costs nothing: no request is sent at all.
func TestAChatPruneRefusesACutoffThatIsNotTypedTwice(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusOK,
		`{"outcome":"applied","op_id":"`+opIDMinted+`","cutoff":"2031-03-24T04:35:00Z",`+
			`"position":{"stream":"CREWLET_CHAT_LOG","seq":9}}`)

	_, _, err := chatCLI(t, fake.server.URL, "prune", "c-eng",
		"-cutoff", "2031-03-24T04:35:00Z", "-confirm", "2031-03-25T04:35:00Z")
	if err == nil {
		t.Fatal("two different instants were accepted as a confirmation")
	}
	if fake.calls != 0 {
		t.Errorf("a mistyped confirmation still reached the node %d time(s)",
			fake.calls)
	}

	// AND A CUTOFF THAT IS NOT AN INSTANT AT ALL is refused before the
	// round trip, naming the shape rather than repeating the node's words.
	_, _, err = chatCLI(t, fake.server.URL, "prune", "c-eng",
		"-cutoff", "last tuesday", "-confirm", "last tuesday")
	if err == nil || !strings.Contains(err.Error(), "RFC 3339") {
		t.Errorf("a cutoff that is not an instant gave %v", err)
	}
	if fake.calls != 0 {
		t.Errorf("an unparseable cutoff reached the node %d time(s)", fake.calls)
	}

	out, _, err := chatCLI(t, fake.server.URL, "prune", "c-eng",
		"-cutoff", "2031-03-24T04:35:00Z", "-confirm", "2031-03-24T04:35:00Z")
	if err != nil {
		t.Fatalf("a matching pair was refused: %v", err)
	}
	if !strings.Contains(fake.query, "cutoff=") ||
		!strings.Contains(fake.query, "confirm=") {
		t.Errorf("the prune sent %q, which the route refuses", fake.query)
	}
	if !strings.Contains(out, "offline or evicted") {
		t.Errorf("the prune never said what it does not reach:\n%s", out)
	}
}

// AN INCOMPLETE ANSWER SAYS SO. A chat read is served from the rows this node
// has applied, so "there is nothing here" and "this node has not caught up"
// are different facts — and only one of them is about the company.
func TestAChatReadSaysWhenThisNodeCouldNotPresentEverything(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusOK, `{
		"read_level":"stale","complete":false,"log_lag":12,
		"position":{"stream":"CREWLET_CHAT_LOG","seq":9},
		"channel_id":"c-eng","messages":[]}`)
	out, _, err := chatCLI(t, fake.server.URL, "read", "c-eng")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "incomplete") {
		t.Errorf("an incomplete answer printed as a complete one:\n%s", out)
	}
	if !strings.Contains(out, "12 record(s) behind") {
		t.Errorf("the lag was not reported:\n%s", out)
	}
}

// AN UNREAD COUNT NOBODY COULD MEASURE IS NOT ZERO.
//
// The rail answers `read_state: false` when the person's coordination record
// could not be read, and zeroes every count on purpose — so a client that
// printed the number told an operator they were caught up in every room over
// a store that did not answer. The rooms are still the log's and still print;
// only the column becomes unknown, and the output says why.
func TestAChatRailWhoseReadStateNobodyReadClaimsNoCount(t *testing.T) {
	t.Parallel()
	rail := func(measured bool) string {
		return `{"read_level":"stale","complete":true,
			"position":{"stream":"CREWLET_CHAT_LOG","seq":9},
			"read_state":` + strconv.FormatBool(measured) + `,
			"channels":[{"channel":{"id":"c-eng","kind":"public","name":"eng"},
			"unread":` + map[bool]string{true: "4", false: "0"}[measured] + `}]}`
	}

	fake := newChatRouteFake(t, http.StatusOK, rail(false))
	out, _, err := chatCLI(t, fake.server.URL, "channels")
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	row := lineWith(out, "c-eng")
	if strings.Fields(row)[3] != "?" {
		t.Errorf("an unmeasured rail printed the row %q, which claims a count", row)
	}
	if !strings.Contains(out, "unread: unknown") {
		t.Errorf("an unmeasured rail never said its counts are unknown:\n%s", out)
	}

	measured := newChatRouteFake(t, http.StatusOK, rail(true))
	out, _, err = chatCLI(t, measured.server.URL, "channels")
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	if row := lineWith(out, "c-eng"); strings.Fields(row)[3] != "4" ||
		strings.Contains(out, "unread: unknown") {
		t.Errorf("a measured rail printed:\n%s\nwant its count and no doubt", out)
	}
}

// lineWith is the first line of out that contains needle, or "".
func lineWith(out, needle string) string {
	for line := range strings.Lines(out) {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// A READ'S PAGE IS THE DOMAIN'S UNLESS THE OPERATOR NAMES ONE.
//
// A number the CLI sent by default would be a second copy of the domain's own
// page size, drifting silently the day that moved — so an unnamed limit sends
// nothing and the node's own default applies, and a named one is sent as
// typed beside the cursor that pages back.
func TestAChatReadLeavesThePageSizeToTheNodeUnlessNamed(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusOK, `{"read_level":"stale",
		"complete":true,"position":{"stream":"CREWLET_CHAT_LOG","seq":9},
		"messages":[]}`)
	if _, _, err := chatCLI(t, fake.server.URL, "read", "c-eng"); err != nil {
		t.Fatalf("read: %v", err)
	}
	if fake.path != "/chat/channels/c-eng/messages" || fake.query != "" {
		t.Errorf("an unnamed page asked %s?%s, want no limit at all",
			fake.path, fake.query)
	}
	if _, _, err := chatCLI(t, fake.server.URL, "read", "c-eng",
		"-limit", "5", "-before", "cur 1"); err != nil {
		t.Fatalf("read: %v", err)
	}
	asked, err := url.ParseQuery(fake.query)
	if err != nil {
		t.Fatal(err)
	}
	if asked.Get("limit") != "5" || asked.Get("cursor") != "cur 1" {
		t.Errorf("-limit 5 -before %q asked %q", "cur 1", fake.query)
	}
}
