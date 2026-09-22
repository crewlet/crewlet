package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `crewlet chat`, and the four things about it that nothing else would catch.
//
// The verbs are thin over routes the API already serves, so what is worth a
// test here is not that a GET reaches a handler. It is the places where this
// CLI has to KNOW something about chat that no compiler checks: that a write
// answers three values over three status codes, that no verb may name an
// author, that a cutoff is typed twice, and that the list of verbs, the
// switch and the usage text are three copies of one set.

// chatRouteFake is a node that answers whatever a test told it to, and
// remembers what it was asked.
type chatRouteFake struct {
	server *httptest.Server

	status int
	body   string

	method string
	path   string
	query  string
	sent   json.RawMessage
	token  string
	calls  int
}

func newChatRouteFake(t *testing.T, status int, body string) *chatRouteFake {
	t.Helper()
	fake := &chatRouteFake{status: status, body: body}
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
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fake.status)
			_, _ = w.Write([]byte(fake.body))
		}))
	t.Cleanup(fake.server.Close)
	return fake
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

// A CHAT WRITE IS THREE-VALUED ON THE WIRE, and two of the three are not 200.
//
// `applied` answers 200, `pending` answers 202 and `unknown` answers 504 —
// and the last two are successful publishes: the record is on the log, or may
// be. [nodeClient.do] turns everything but 200 into an error, which is right
// for every other operator route here and would report a published message as
// a failure. So the answer is identified by carrying an `outcome`, whatever
// the status.
//
// The mutation that turns this red is making chatGesture judge the status
// instead: `pending` and `unknown` then come back as errors and an operator
// re-runs a gesture that already landed.
func TestAChatWriteReadsEveryOutcomeWhateverTheStatus(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name    string
		status  int
		outcome string
		says    string
	}{
		{"applied", http.StatusOK, "applied", "applied"},
		{"pending", http.StatusAccepted, "pending", "Do not run this again"},
		{"unknown", http.StatusGatewayTimeout, "unknown", "-op-id op-42"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			fake := newChatRouteFake(t, arm.status, `{"outcome":"`+arm.outcome+
				`","op_id":"op-42","position":{"stream":"CREWLET_CHAT_LOG",`+
				`"seq":7},"message":{"id":"m-1"}}`)
			out, _, err := chatCLI(t, fake.server.URL, "post", "c-eng",
				"-body", "hello")
			if err != nil {
				t.Fatalf("a %s write came back as a failure: %v", arm.outcome, err)
			}
			if !strings.Contains(out, arm.says) {
				t.Errorf("the %s outcome printed %q, which never says %q",
					arm.outcome, out, arm.says)
			}
		})
	}
}

// A REFUSAL IS STILL A REFUSAL. A body with no outcome in it is not this
// surface answering, whatever its status, and it must not be read as one —
// otherwise a 403 from a token bound to no seat would print as a write that
// happened.
func TestAChatWriteWithNoOutcomeIsARefusal(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusForbidden,
		`{"error":"no_seat","detail":"give a kind: human seat a `+
			`contact.crewlet_operator_id"}`)
	_, _, err := chatCLI(t, fake.server.URL, "post", "c-eng", "-body", "hello")
	if err == nil {
		t.Fatal("a 403 with no outcome was read as a successful write")
	}
	if !strings.Contains(err.Error(), "refused the token") {
		t.Errorf("the refusal reads %q", err)
	}
}

// A NODE RUNNING NO NATIVE CHAT ANSWERS A BARE 404, and the axis is the one
// thing that explains it.
//
// Every chat route is mounted only where `chat.backend` is `native`, and
// net/http's mux answers a route it does not have with an empty body — so the
// generic "the node answered 404: " names nothing an operator can act on.
func TestAChatWriteOnANodeWithoutNativeChatNamesTheAxis(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusNotFound, "")
	_, _, err := chatCLI(t, fake.server.URL, "post", "c-eng", "-body", "hello")
	if err == nil {
		t.Fatal("a 404 was read as a successful write")
	}
	if !strings.Contains(err.Error(), "chat.backend") {
		t.Errorf("a missing route reads %q, which never names the axis that "+
			"decides whether these routes exist", err)
	}
}

// NO VERB HERE NAMES AN AUTHOR, and a post carries an idempotency key.
//
// The server resolves the credential to the seat it writes as — that is the
// rule the whole surface is built on — so a body carrying a handle would be a
// second answer to the only question that decides what a transcript may show.
// The operation id is the other half: a post arbitrates nothing at the broker,
// so nothing else could tell a resubmission from a second remark.
func TestAChatPostNamesNoAuthorAndCarriesAnIdempotencyKey(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusOK,
		`{"outcome":"applied","op_id":"op-1","message":{"id":"m-1"}}`)
	if _, _, err := chatCLI(t, fake.server.URL, "post", "c-eng",
		"-body", "hello", "-link", "https://example.com/x"); err != nil {
		t.Fatalf("post: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(fake.sent, &sent); err != nil {
		t.Fatalf("the body sent was not JSON: %v (%s)", err, fake.sent)
	}
	for _, forbidden := range []string{"author", "author_kind", "handle", "as"} {
		if _, named := sent[forbidden]; named {
			t.Errorf("the post named %q, and a caller may never name a seat: %s",
				forbidden, fake.sent)
		}
	}
	if id, _ := sent["operation_id"].(string); id == "" {
		t.Errorf("the post carried no operation_id, so a retry says the same "+
			"thing twice: %s", fake.sent)
	}
	if fake.token != "Bearer t0ken" {
		t.Errorf("the post sent %q rather than the credential the server "+
			"resolves to a seat", fake.token)
	}
}

// THE SAME OPERATION ID IS WHAT MAKES A RETRY ONE MESSAGE. `unknown` is the
// one outcome to retry, and the retry has to carry the id the first attempt
// printed — a fresh one would be a second remark.
func TestAChatPostRetriesUnderTheOperationIDItIsGiven(t *testing.T) {
	t.Parallel()
	fake := newChatRouteFake(t, http.StatusOK,
		`{"outcome":"applied","op_id":"op-1","message":{"id":"m-1"}}`)
	if _, _, err := chatCLI(t, fake.server.URL, "post", "c-eng",
		"-body", "hello", "-op-id", "op-from-the-first-attempt"); err != nil {
		t.Fatalf("post: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(fake.sent, &sent); err != nil {
		t.Fatalf("the body sent was not JSON: %v", err)
	}
	if sent["operation_id"] != "op-from-the-first-attempt" {
		t.Errorf("-op-id was not carried: %s", fake.sent)
	}
}

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
		`{"outcome":"applied","op_id":"op-1","cutoff":"2031-03-24T04:35:00Z"}`)

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
