package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet chatlet`, and the things about it nothing else would catch.
//
// The verbs are thin over routes the API already serves, so what is worth a
// test here is not that a GET reaches a handler. It is the places where this
// CLI has to KNOW something about chat that no compiler checks: that a post is
// the dashboard's own tool over the act transport and a destruction the
// purge's shape, that no verb may name an author, that what a destruction
// confirms is counted and compared the way its route counts and compares it,
// that an unmeasured badge is not a zero — and that the verbs, the switch and
// the usage text are three copies of one set. How an open outcome is finished
// is every keyed write's, held by [TestEveryKeyedWriteFinishesAnOpenOutcomeUnderItsOwnOperation].

// fakeChatletNode is a node that answers whatever a case told it to, and
// remembers what it was asked — behind a lock, for [fakePurgeNode]'s reason.
type fakeChatletNode struct {
	server *httptest.Server

	mu      sync.Mutex
	status  int
	body    string
	method  string
	path    string
	query   url.Values
	sent    json.RawMessage
	auth    string
	key     string
	calls   int
	headers http.Header
}

func newFakeChatletNode(t *testing.T, status int, body string) *fakeChatletNode {
	t.Helper()
	n := &fakeChatletNode{status: status, body: body}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := new(bytes.Buffer)
		if _, err := raw.ReadFrom(r.Body); err != nil {
			t.Errorf("reading the request body: %v", err)
		}
		n.mu.Lock()
		n.calls++
		n.method, n.path, n.query = r.Method, r.URL.Path, r.URL.Query()
		n.auth, n.key, n.headers = r.Header.Get("Authorization"), r.Header.Get(opkey.Header),
			r.Header.Clone()
		n.sent = nil
		if raw.Len() > 0 {
			n.sent = json.RawMessage(raw.Bytes())
		}
		status, body := n.status, n.body
		n.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(n.server.Close)
	return n
}

// asked is a copy of what the node was last sent.
func (n *fakeChatletNode) asked() fakeChatletNode {
	n.mu.Lock()
	defer n.mu.Unlock()
	return fakeChatletNode{method: n.method, path: n.path, query: n.query, sent: n.sent,
		auth: n.auth, key: n.key, calls: n.calls, headers: n.headers}
}

// envelopeOf is the refusal body a node answers for code and detail, as the
// node assembles it ([httpjson.Envelope]) rather than as a case would spell it:
// a body typed here carried a `detail` the node no longer writes for these
// refusals, and none of the `message` it always does.
func envelopeOf(t *testing.T, code httpjson.Code, detail httpjson.Detail) string {
	t.Helper()
	raw, err := json.Marshal(httpjson.Envelope(code, detail))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// chatletCLI runs one `crewlet chatlet` invocation against a fake node, holding the
// fixture credential.
func chatletCLI(t *testing.T, base string, args ...string) (string, string, error) {
	t.Helper()
	return cli(t, append(append([]string{"chatlet"}, args...), "-api", base)...)
}

// EVERY `chatlet` VERB IS DISPATCHED AND ADVERTISED, in both directions.
//
// Nothing connects runChatlet's switch to chatletUsage, so a verb can ship reachable
// and undocumented or documented and unreachable. So the switch is read out of
// the source, as run()'s is, and held against the usage text's lines. And a
// misspelled verb carrying another verb's flag is reported as an unknown
// COMMAND: the switch refuses it before any flag set is built, so the operator
// is sent to the word they typed wrong rather than to a flag that was never
// the problem. Mutation: drop the `retention` line from the usage, or its case
// from the switch, and one half fails.
func TestEveryChatletVerbIsDispatchedAndAdvertised(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	if err := runChatlet(noEnviron, nil, &out, &errs); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("`crewlet chatlet` with no verb gave %v, want the help", err)
	}
	dispatched := switchCases(t, "chatlet.go", "runChatlet", "sub")
	advertised := advertisedWords(t, out.String(), "crewlet chatlet ", "\nEvery command")
	for _, verb := range dispatched {
		if !slices.Contains(advertised, verb) {
			t.Errorf("runChatlet dispatches %q but the usage never names `crewlet chatlet "+
				"%s`, so it can only be found by reading the source", verb, verb)
		}
	}
	for _, verb := range advertised {
		if !slices.Contains(dispatched, verb) {
			t.Errorf("the usage names `crewlet chatlet %s` and runChatlet dispatches no "+
				"such verb, so it answers `unknown chatlet command`", verb)
		}
	}

	node := newFakeChatletNode(t, http.StatusOK, `{}`)
	for _, typo := range [][]string{{"nonesuch"}, {"reed", "c-eng", "-limit", "5"}} {
		if _, _, err := chatletCLI(t, node.server.URL, typo...); err == nil ||
			!strings.Contains(err.Error(), "unknown chatlet command") {
			t.Errorf("`chatlet %s` gave %v, want the unknown-command refusal",
				strings.Join(typo, " "), err)
		}
	}
	if node.asked().calls != 0 {
		t.Error("an unknown verb reached the node")
	}
}

// A POST IS THE COMPOSER'S TOOL, OVER THE ROUTE THE DASHBOARD WRITES THROUGH.
//
// `post_message` through `POST /operator/act/{tool}` (ADR-0024): the tool's own
// arguments as `{"args": …}` and nothing beside them — the transport refuses an
// unknown field — the operation in the `Idempotency-Key` it requires, and the
// credential from the environment. So a line said from a terminal and one said
// from a screen are one gesture with one record, attributed to the seat the
// credential acts as. And no argument names an author: the server resolves the
// credential to a seat, and a body carrying a handle would be a second answer
// to the only question that decides what a transcript may show.
func TestAChatletPostIsTheComposersToolOverTheActTransport(t *testing.T) {
	t.Parallel()
	node := newFakeChatletNode(t, http.StatusOK, `{"tool":"post_message",
		"outcome":"applied","position":"CREWLET_CHAT_LOG@1:7",
		"receipt":{"message_id":"m-1","channel":"c-eng","outcome":"applied",
		"position":"CREWLET_CHAT_LOG@1:7","mentioned":["cto"],
		"addressed_whole_room":true}}`)
	out, _, err := chatletCLI(t, node.server.URL, "post", "#eng",
		"-body", "hello @cto and @channel", "-link", "https://example.com/x")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	asked := node.asked()
	if asked.method != http.MethodPost || asked.path != "/operator/act/post_message" {
		t.Errorf("the post went to %s %s, want the act transport's post_message",
			asked.method, asked.path)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(asked.sent, &sent); err != nil || len(sent) != 1 ||
		sent["args"] == nil {
		t.Fatalf("the body %s is not exactly {\"args\": …} (%v)", asked.sent, err)
	}
	var args map[string]any
	if err := json.Unmarshal(sent["args"], &args); err != nil {
		t.Fatal(err)
	}
	if args["channel"] != "#eng" || args["body"] != "hello @cto and @channel" ||
		!reflect.DeepEqual(args["links"], []any{"https://example.com/x"}) {
		t.Errorf("args = %v, want the room as typed, the body and the link", args)
	}
	for _, forbidden := range []string{"author", "author_kind", "handle", "as", "op_id"} {
		if _, named := args[forbidden]; named {
			t.Errorf("the post named %q: %s", forbidden, asked.sent)
		}
	}
	if _, minted := statelog.OpMintedAt(asked.key); !minted {
		t.Errorf("the post carried the key %q, which no ledger could vouch for", asked.key)
	}
	if asked.auth != "Bearer "+cliFixtureToken {
		t.Errorf("the post sent %q rather than the credential in %s", asked.auth, apiTokenEnv)
	}
	for _, said := range []string{
		"post m-1 in #eng: applied at CREWLET_CHAT_LOG@1:7 (operation " + asked.key + ")",
		"@channel: this addresses the whole room", "mentions: @cto",
	} {
		if !strings.Contains(out, said) {
			t.Errorf("the post printed:\n%s\nwant %q", out, said)
		}
	}
}

// A POST'S OTHER OUTCOMES ARE SAID FOR WHAT THEY ARE: `pending` is a published
// message the node has not applied, which reads as nothing having happened if
// the operator then reads the room — so it says not to post again — and an
// unknown the transport answered 200 FAILS, naming the operation that finishes
// it: exited zero, a script read a message nobody could vouch for as said.
func TestAChatletPostSaysAPendingAndAnUnknownOutcomeForWhatTheyAre(t *testing.T) {
	t.Parallel()
	pending := newFakeChatletNode(t, http.StatusAccepted, `{"outcome":"pending",
		"position":"CREWLET_CHAT_LOG@1:7","receipt":{"message_id":"m-1"}}`)
	out, _, err := chatletCLI(t, pending.server.URL, "post", "c-eng", "-body", "hello")
	if err != nil {
		t.Fatalf("a pending post came back as a failure: %v", err)
	}
	if !strings.Contains(out, "pending") || !strings.Contains(out, "Do not run this again") {
		t.Errorf("a pending post printed:\n%s", out)
	}

	unknown := newFakeChatletNode(t, http.StatusOK, `{"outcome":"unknown","receipt":{}}`)
	_, _, err = chatletCLI(t, unknown.server.URL, "post", "c-eng", "-body", "hello")
	if key := unknown.asked().key; err == nil || !strings.Contains(err.Error(),
		"-op-id "+key) {
		t.Errorf("an unknown post answered %v, want it to fail naming the "+
			"operation it was sent under (%s)", err, key)
	}
}

// A CREDENTIAL ACTING AS ITSELF IS TOLD WHICH CREDENTIAL OR BINDING WOULD DO.
//
// Every chat verb a seat makes is SEATED, so a Tier A token or an unbound
// service account is refused `unauthorized` with reason `unseated` and no
// grant named — none would admit it. Rendered as the bare reason, it sent an
// operator holding admin looking for a permission they already had.
func TestAChatletVerbUnderACredentialActingAsItselfNamesTheBinding(t *testing.T) {
	t.Parallel()
	node := newFakeChatletNode(t, http.StatusForbidden, envelopeOf(t,
		httpjson.CodeUnauthorized, httpjson.Detail{
			authz.DetailReason: string(authz.ReasonUnseated), authz.DetailGrants: []string{}}))
	for _, verb := range [][]string{
		{"post", "c-eng", "-body", "hello"}, {"channels"}, {"read", "c-eng"},
		{"search", "launch"},
	} {
		_, _, err := chatletCLI(t, node.server.URL, verb...)
		if err == nil {
			t.Fatalf("%s: a refusal was read as an answer", verb[0])
		}
		if !strings.Contains(err.Error(), "crewlet iam bind") ||
			!strings.Contains(err.Error(), "crewlet iam token -login") {
			t.Errorf("%s under a credential acting as itself reads %q, want the "+
				"binding and the person's own token named", verb[0], err)
		}
	}
}

// A POST'S -link SAYS WHAT A LINK IS. The room refuses a link that is not an
// absolute address in one of [chatlet.LinkSchemes], and a post refused for one
// is answered at the floor, in its code's own words (below), so the flag's help
// is where a person posting from a terminal learns the rule before they send.
// Mutation: drop mailto from the flag's help, and this fails.
func TestAChatletPostsLinkFlagSaysWhatALinkIs(t *testing.T) {
	t.Parallel()
	_, errs, err := cliIn(t, noEnviron, "chatlet", "post", "launch", "-h")
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("chatlet post -h = %v, want the help", err)
	}
	_, help, found := strings.Cut(errs, "  -link value\n")
	help, _, _ = strings.Cut(help, "\n  -")
	if !found {
		t.Fatalf("chatlet post -h names no -link:\n%s", errs)
	}
	words := strings.FieldsFunc(help, func(r rune) bool { return r < 'a' || r > 'z' })
	for _, scheme := range chatlet.LinkSchemes() {
		if !slices.Contains(words, scheme) {
			t.Errorf("-link's help %q does not say a link may be %s", help, scheme)
		}
	}
}

// A POST REFUSED AT ADR-0060'S FLOOR SAYS ITS CODE'S OWN SENTENCE.
//
// Chat's refusals are worded for nobody yet, so the act route answers them
// with no `detail` and the code's `message` the whole of what they say: a post
// into an archived room is `409 conflict` in the conflict code's words. Printed
// as the bare code, it told the operator neither what happened nor what to do
// next. The body is the act route's own, written by [operator.Fail] for the
// class and the cause the post meets, rather than a sentence typed here.
// Mutation: drop the message from [refusalWords.said] and the post reads "the
// node answered 409: conflict" and nothing more.
func TestAChatletPostRefusedAtTheFloorSaysTheCodesSentence(t *testing.T) {
	t.Parallel()
	answered := httptest.NewRecorder()
	operator.Fail(answered, crewletmcp.Classify(crewletmcp.RefusalConflict, chatlet.ErrArchived),
		caveat.Refusal{}, "", operator.About("post_message", map[string]any{"channel": "launch"}))
	node := newFakeChatletNode(t, answered.Code, answered.Body.String())
	_, _, err := chatletCLI(t, node.server.URL, "post", "launch", "-body", "hi")
	if err == nil || !strings.Contains(err.Error(), "409") ||
		!strings.Contains(err.Error(), httpjson.CodeConflict.Message()) {
		t.Errorf("a post refused at the floor reads %v, want the status and the "+
			"conflict code's own sentence", err)
	}
}

// A NODE THAT SERVES NO CHAT IS TOLD WHY, whichever of chat's surfaces asked —
// and a node with no company YET is not.
//
// Each refusal is right in its own words — a question nobody registered, a tool
// the catalogue does not carry, a route whose half is not up — and none names
// the one thing that explains all three: chat exists only where `chat.backend`
// is `chatlet`, its writes only on a node that may publish, its routes only on a
// node with the ingress role. A node that has not been handed a company says
// THAT, which the axis sentence would bury.
func TestAChatletVerbOnANodeServingNoChatNamesTheAxis(t *testing.T) {
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
		{"retention", "no_route", []string{"retention", "c-eng", "-days", "30", "-confirm", "30"}},
		{"read", "unknown_query", []string{"read", "c-eng"}},
		{"channels", "unknown_query", []string{"channels"}},
		{"search", "unknown_query", []string{"search", "launch"}},
	} {
		t.Run(verb.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeChatletNode(t, http.StatusNotFound,
				envelopeOf(t, httpjson.Code(verb.code), nil))
			_, _, err := chatletCLI(t, node.server.URL, verb.args...)
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

	none := newFakeChatletNode(t, http.StatusServiceUnavailable,
		`{"error":"no_active_revision","detail":"this node has not been handed a company yet"}`)
	_, _, err := chatletCLI(t, none.server.URL, "channels")
	if err == nil || strings.Contains(err.Error(), "chat.backend") ||
		!strings.Contains(err.Error(), "not been handed a company") {
		t.Errorf("a node with no company yet reads %v, want its own words and not "+
			"the chat axis", err)
	}
}

// AN ERASE NAMES ITS ROWS, AND COUNTS THEM AS ITS ROUTE DOES.
//
// The case it is for — a credential pasted into one message — is one a prune
// answers by destroying everything the room said before an instant. An erase
// sends the ids and the reason as the route's JSON body and the COUNT as its
// confirmation, both over the set the route reads ([chatlet.EraseSet]): a message
// named twice is one message, and confirmed as two the route refused an erase
// the operator had typed correctly. And it prints what a destruction does not
// reach.
func TestAChatletEraseNamesItsRowsAndCountsThemAsItsRouteDoes(t *testing.T) {
	t.Parallel()
	node := newFakeChatletNode(t, http.StatusOK, `{"channel_id":"c-eng","messages":2,
		"outcome":"applied","op_id":"the-scoped-key","position":"CREWLET_CHAT_LOG@1:9"}`)
	out, _, err := chatletCLI(t, node.server.URL, "erase", "c-eng", "-message", "m-2",
		"-message", "m-1", "-message", "m-2", "-reason", "a pasted key", "-confirm", "2")
	if err != nil {
		t.Fatalf("erase: %v", err)
	}
	asked := node.asked()
	if asked.method != http.MethodPost || asked.path != "/chatlet/channels/c-eng/erase" {
		t.Errorf("the erase went to %s %s", asked.method, asked.path)
	}
	var sent struct {
		MessageIDs []string `json:"message_ids"`
		Reason     string   `json:"reason"`
	}
	if err := json.Unmarshal(asked.sent, &sent); err != nil {
		t.Fatalf("the body sent was not JSON: %v (%s)", err, asked.sent)
	}
	if !reflect.DeepEqual(sent.MessageIDs, []string{"m-1", "m-2"}) ||
		sent.Reason != "a pasted key" {
		t.Errorf("the erase sent %s, want the set of ids and the reason", asked.sent)
	}
	if asked.query.Get("confirm") != "2" || asked.query.Has("op_id") {
		t.Errorf("the erase asked ?%s, want the count and no op_id beside the header",
			asked.query.Encode())
	}
	if _, minted := statelog.OpMintedAt(asked.key); !minted {
		t.Errorf("the erase carried the key %q, which carries no instant", asked.key)
	}
	if !strings.Contains(out, "applied at CREWLET_CHAT_LOG@1:9 (operation the-scoped-key)") ||
		!strings.Contains(out, "offline or evicted") {
		t.Errorf("the erase printed:\n%s", out)
	}
}

// AN ERASE IS REFUSED BEFORE IT IS SENT when its count, its reason or its
// messages are missing. The rows go on every node with no inverse, so a typo
// costs nothing: no request reaches the node at all.
func TestAChatletEraseRefusesWhatTheRouteWouldBeforeItIsSent(t *testing.T) {
	t.Parallel()
	node := newFakeChatletNode(t, http.StatusOK, `{"outcome":"applied"}`)
	for name, args := range map[string][]string{
		"no messages": {"erase", "c-eng", "-reason", "why", "-confirm", "0"},
		"no reason":   {"erase", "c-eng", "-message", "m-1", "-confirm", "1"},
		"no count":    {"erase", "c-eng", "-message", "m-1", "-reason", "why"},
		"the wrong count": {"erase", "c-eng", "-message", "m-1", "-message", "m-2",
			"-reason", "why", "-confirm", "1"},
		"a repeat counted twice": {"erase", "c-eng", "-message", "m-1", "-message", "m-1",
			"-reason", "why", "-confirm", "2"},
		"no room": {"erase", "-message", "m-1", "-reason", "why", "-confirm", "1"},
	} {
		if _, _, err := chatletCLI(t, node.server.URL, args...); err == nil {
			t.Errorf("an erase with %s was accepted", name)
		}
	}
	if calls := node.asked().calls; calls != 0 {
		t.Errorf("a refused erase still reached the node %d time(s)", calls)
	}
}

// THE CUTOFF IS TYPED TWICE, and compared as the route compares it.
//
// Everything the room said before the instant is destroyed on every node with
// no inverse, so the value that decides how much of a conversation disappears
// is not one to inherit from a shell history. Refused here, a typo costs
// nothing: no request is sent. Compared as INSTANTS, so the same moment
// written in two zones is the confirmation it is — the route reads it so, and
// a CLI comparing the strings refused what the node would have taken.
func TestAChatletPruneConfirmsItsCutoffAsTheRouteDoes(t *testing.T) {
	t.Parallel()
	node := newFakeChatletNode(t, http.StatusOK, `{"channel_id":"c-eng",
		"cutoff":"2031-03-24T04:35:00Z","outcome":"applied","op_id":"k",
		"position":"CREWLET_CHAT_LOG@1:9"}`)
	for name, args := range map[string][]string{
		"two instants": {"prune", "c-eng", "-cutoff", "2031-03-24T04:35:00Z",
			"-confirm", "2031-03-25T04:35:00Z"},
		"no confirmation": {"prune", "c-eng", "-cutoff", "2031-03-24T04:35:00Z"},
		"not an instant": {"prune", "c-eng", "-cutoff", "last tuesday",
			"-confirm", "last tuesday"},
		"no room": {"prune", "-cutoff", "2031-03-24T04:35:00Z",
			"-confirm", "2031-03-24T04:35:00Z"},
	} {
		if _, _, err := chatletCLI(t, node.server.URL, args...); err == nil {
			t.Errorf("a prune with %s was accepted", name)
		}
	}
	if calls := node.asked().calls; calls != 0 {
		t.Errorf("a refused prune still reached the node %d time(s)", calls)
	}

	out, _, err := chatletCLI(t, node.server.URL, "prune", "c-eng",
		"-cutoff", "2031-03-24T04:35:00Z", "-confirm", "2031-03-24T05:35:00+01:00")
	if err != nil {
		t.Fatalf("one instant written in two zones was refused: %v", err)
	}
	asked := node.asked()
	if asked.method != http.MethodPost || asked.path != "/chatlet/channels/c-eng/prune" ||
		asked.query.Get("cutoff") != "2031-03-24T04:35:00Z" ||
		asked.query.Get("confirm") != "2031-03-24T04:35:00Z" || asked.sent != nil {
		t.Errorf("the prune asked %s %s?%s with %s, want the cutoff twice and no body",
			asked.method, asked.path, asked.query.Encode(), asked.sent)
	}
	if !strings.Contains(out, "prune c-eng before 2031-03-24T04:35:00Z: applied") ||
		!strings.Contains(out, "offline or evicted") {
		t.Errorf("the prune printed:\n%s", out)
	}
}

// A ROOM'S HORIZON IS EXACTLY ONE OF A NUMBER OF DAYS AND THE COMPANY'S.
//
// The zero is a setting — `-days 0` keeps the room for ever — so a flag left
// off cannot also mean zero, and "the company's own horizon" is a choice of
// its own, sent as null. A command naming neither, or both, is refused before
// anything is sent: the guess that read an absent value as the company's would
// destroy the past of a room an administrator had set to keep for ever.
func TestAChatletRetentionSaysExactlyOneHorizon(t *testing.T) {
	t.Parallel()
	node := newFakeChatletNode(t, http.StatusOK, `{"channel_id":"c-eng",
		"outcome":"applied","op_id":"k","position":"CREWLET_CHAT_LOG@1:9"}`)
	for name, args := range map[string][]string{
		"neither":     {"retention", "c-eng", "-confirm", "30"},
		"both":        {"retention", "c-eng", "-days", "30", "-company", "-confirm", "30"},
		"not a whole": {"retention", "c-eng", "-days", "1.5", "-confirm", "1.5"},
		"no room":     {"retention", "-days", "30", "-confirm", "30"},
		// THE HORIZON TYPED TWICE: a shorter one is a prune on a delay.
		"no confirmation":                     {"retention", "c-eng", "-days", "30"},
		"another number confirmed":            {"retention", "c-eng", "-days", "30", "-confirm", "3"},
		"the company's confirmed as days":     {"retention", "c-eng", "-company", "-confirm", "0"},
		"for ever confirmed as the company's": {"retention", "c-eng", "-days", "0", "-confirm", "company"},
	} {
		if _, _, err := chatletCLI(t, node.server.URL, args...); err == nil {
			t.Errorf("a retention with %s was accepted", name)
		}
	}
	if calls := node.asked().calls; calls != 0 {
		t.Errorf("a refused retention still reached the node %d time(s)", calls)
	}
	for _, tc := range []struct {
		args    []string
		want    string
		confirm string
		says    string
	}{
		{[]string{"-days", "30", "-confirm", "30"}, `{"retention_days":30}`, "30", "30 day(s)"},
		{[]string{"-days", "0", "-confirm", "0"}, `{"retention_days":0}`, "0", "kept for ever"},
		{[]string{"-company", "-confirm", "company"}, `{"retention_days":null}`, "company",
			"the company's horizon"},
	} {
		out, _, err := chatletCLI(t, node.server.URL,
			append([]string{"retention", "c-eng"}, tc.args...)...)
		if err != nil {
			t.Fatalf("retention %v: %v", tc.args, err)
		}
		asked := node.asked()
		if asked.method != http.MethodPut || asked.path != "/chatlet/channels/c-eng/retention" ||
			string(asked.sent) != tc.want || asked.query.Get("confirm") != tc.confirm {
			t.Errorf("retention %v sent %s %s?%s %s, want PUT %s confirming %q", tc.args,
				asked.method, asked.path, asked.query.Encode(), asked.sent, tc.want,
				tc.confirm)
		}
		if _, minted := statelog.OpMintedAt(asked.key); !minted {
			t.Errorf("retention %v carried the key %q", tc.args, asked.key)
		}
		if !strings.Contains(out, "retention of c-eng: "+tc.says+", applied") {
			t.Errorf("retention %v printed:\n%s", tc.args, out)
		}
	}
}

// A TRANSCRIPT IS PRINTED AS IT IS READ, AND SAYS WHAT IT COULD NOT PRESENT.
//
// Oldest first, because a conversation read bottom-up is not one; every
// message beside its id, which is what `chatlet erase` names and what a reply's
// mark points at; a thread's reply marked with the message it hangs off, a
// tombstone as one taken back rather than as a line with nothing in it, and a
// message of several lines — or carrying links, which are part of what it says
// — kept under its own heading. And a read is served from the rows the
// answering copy has applied, so "there is nothing here" and "that copy has not
// caught up" are different facts — the second is said, with how far behind it
// is. Mutation: drop the id from the line, or the links, and this fails.
func TestAChatletTranscriptIsPrintedAsItIsReadAndSaysWhatItLacked(t *testing.T) {
	t.Parallel()
	node := newFakeChatletNode(t, http.StatusOK, `{
		"read_level":"stale","complete":false,"log_lag":12,
		"position":{"stream":"CREWLET_CHAT_LOG","generation":1,"seq":9},
		"channel_id":"c-eng","next_cursor":"40","messages":[
		{"message":{"id":"m-3","author":"cto","body":"first line\nsecond line",
		  "thread_root":"m-1","created_at":"2031-03-24T04:37:00Z"},"channel_seq":43},
		{"message":{"id":"m-2","author":"ceo","body":"",
		  "deleted_at":"2031-03-24T04:40:00Z","created_at":"2031-03-24T04:36:00Z"},"channel_seq":42},
		{"message":{"id":"m-1","author":"ceo","body":"what ships?",
		  "links":["https://example.com/plan"],
		  "created_at":"2031-03-24T04:35:00Z"},"channel_seq":41}]}`)
	out, _, err := chatletCLI(t, node.server.URL, "read", "c-eng")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	first, second, third := strings.Index(out, "what ships?"), strings.Index(out, "(deleted)"),
		strings.Index(out, "↳m-1 first line")
	if first < 0 || second < first || third < second {
		t.Errorf("the transcript is not oldest first with its reply marked:\n%s", out)
	}
	if !strings.Contains(out, "first line\n        second line") {
		t.Errorf("a message's second line is not kept under it:\n%s", out)
	}
	for id, said := range map[string]string{"m-1": "what ships?", "m-2": "(deleted)",
		"m-3": "first line"} {
		if row := lineWith(out, said); !strings.Contains(row, "  "+id+"  ") {
			t.Errorf("the line %q does not name its message %s, which `chatlet erase` "+
				"takes and a reply's mark points at", row, id)
		}
	}
	if !strings.Contains(out, "what ships?\n        link: https://example.com/plan") {
		t.Errorf("a message's link is not printed under it:\n%s", out)
	}
	for _, said := range []string{"older: -before 40", "incomplete",
		"stale at CREWLET_CHAT_LOG@1:9, 12 record(s) behind the log"} {
		if !strings.Contains(out, said) {
			t.Errorf("the transcript printed:\n%s\nwant %q", out, said)
		}
	}
}

// A READ'S PAGE IS THE DOMAIN'S UNLESS THE OPERATOR NAMES ONE.
//
// A number the CLI sent by default would be a second copy of the domain's own
// page size, drifting silently the day that moved — so an unnamed limit sends
// nothing and the node's own default applies, and a named one is sent as
// typed beside the cursor that pages back.
func TestAChatletReadLeavesThePageSizeToTheNodeUnlessNamed(t *testing.T) {
	t.Parallel()
	node := newFakeChatletNode(t, http.StatusOK, `{"read_level":"stale",
		"complete":true,"position":{"stream":"CREWLET_CHAT_LOG","seq":9},
		"messages":[]}`)
	out, _, err := chatletCLI(t, node.server.URL, "read", "c-eng")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// AN EMPTY PAGE IS SAID FOR WHAT IT IS: a room nothing is left in, or —
	// paging back — the start of the room, which are different facts.
	if !strings.Contains(out, "No messages") {
		t.Errorf("an empty room printed:\n%s", out)
	}
	if asked := node.asked(); asked.path != "/chatlet/channels/c-eng/messages" ||
		len(asked.query) != 0 {
		t.Errorf("an unnamed page asked %s?%s, want no limit at all", asked.path,
			asked.query.Encode())
	}
	out, _, err = chatletCLI(t, node.server.URL, "read", "c-eng", "-limit", "5", "-before", "40")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "No older messages") {
		t.Errorf("a page past the start of the room printed:\n%s", out)
	}
	if asked := node.asked(); asked.query.Get("limit") != "5" ||
		asked.query.Get("cursor") != "40" {
		t.Errorf("-limit 5 -before 40 asked ?%s", asked.query.Encode())
	}
}

// AN UNREAD COUNT NOBODY COULD MEASURE IS NOT ZERO.
//
// The rail answers `read_state: false` when the seat's coordination record could
// not be read, and zeroes every count on purpose — so a client that printed
// the number told an operator they were caught up in every room over a store
// that did not answer. The rooms are still the log's and still print; only the
// column becomes unknown, and the output says why. A direct conversation has
// no name and is named by who is in it, and a muted room says so.
func TestAChatletRailWhoseReadStateNobodyReadClaimsNoCount(t *testing.T) {
	t.Parallel()
	rail := func(measured bool) string {
		return `{"read_level":"stale","complete":true,
			"position":{"stream":"CREWLET_CHAT_LOG","seq":9},
			"read_state":` + strconv.FormatBool(measured) + `,
			"channels":[
			{"channel":{"id":"c-eng","kind":"public","name":"eng"},"muted":true,
			 "unread":` + map[bool]string{true: "4", false: "0"}[measured] + `,
			 "last":{"author":"cto","excerpt":"on\tit\nnow"}},
			{"channel":{"id":"c-dm","kind":"dm"},"participants":["ceo","cto"],
			 "unread":0}]}`
	}

	unmeasured := newFakeChatletNode(t, http.StatusOK, rail(false))
	out, _, err := chatletCLI(t, unmeasured.server.URL, "channels")
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	if row := lineWith(out, "c-eng"); !strings.Contains(row, " ? ") {
		t.Errorf("an unmeasured rail printed the row %q, which claims a count", row)
	}
	if !strings.Contains(out, "unread: unknown") {
		t.Errorf("an unmeasured rail never said its counts are unknown:\n%s", out)
	}

	measured := newFakeChatletNode(t, http.StatusOK, rail(true))
	out, _, err = chatletCLI(t, measured.server.URL, "channels")
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	row := lineWith(out, "c-eng")
	if !strings.Contains(row, " 4 ") || strings.Contains(out, "unread: unknown") {
		t.Errorf("a measured rail printed:\n%s\nwant its count and no doubt", out)
	}
	if !strings.Contains(row, "eng (muted)") || !strings.Contains(row, "cto: on it now") {
		t.Errorf("the room's row is %q, want it muted and its last line on one line", row)
	}
	if dm := lineWith(out, "c-dm"); !strings.Contains(dm, "ceo, cto") {
		t.Errorf("a direct conversation is listed as %q, want who is in it", dm)
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

// A SEARCH SAYS WHAT IT RAN OVER, AND WHEN ITS INDEX IS BEHIND.
//
// "Nothing matched" and "this seat may read nothing" look identical without
// the number of rooms searched, and only one is about the query; and an index
// that has not caught up with its transcript draws its hits from part of it,
// which a reader must be told before they read "nobody said that". The query
// is the operator's text, a room and an author — the `@` a handle is typed
// with is not part of it. And a hit names its message, because a search is how
// the one message a credential was pasted into is found, and `chatlet erase` takes
// its id.
func TestAChatletSearchSaysWhatItRanOverAndWhenItsIndexIsBehind(t *testing.T) {
	t.Parallel()
	node := newFakeChatletNode(t, http.StatusOK, `{"hits":[],"searched":3,
		"building":true,"read_level":"stale"}`)
	out, _, err := chatletCLI(t, node.server.URL, "search", "launch plan",
		"-channel", "c-eng", "-author", "@cto", "-limit", "5")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	asked := node.asked()
	if asked.path != "/chatlet/search" || asked.query.Get("q") != "launch plan" ||
		asked.query.Get("channel_id") != "c-eng" || asked.query.Get("author") != "cto" ||
		asked.query.Get("limit") != "5" {
		t.Errorf("the search asked %s?%s", asked.path, asked.query.Encode())
	}
	if !strings.Contains(out, "No hits across 3 room(s)") ||
		!strings.Contains(out, "still building") {
		t.Errorf("an empty search over a building index printed:\n%s", out)
	}

	found := newFakeChatletNode(t, http.StatusOK, `{"hits":[{"message_id":"m-1",
		"channel_id":"c-eng","author":"cto","snippet":"the launch\nplan",
		"at":"2031-03-24T04:35:00Z","score":1.5}],"searched":3,"read_level":"stale"}`)
	out, _, err = chatletCLI(t, found.server.URL, "search", "launch")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if row := lineWith(out, "c-eng"); !strings.Contains(row, "the launch plan") ||
		!strings.Contains(row, "m-1") || strings.Contains(out, "still building") {
		t.Errorf("a search printed:\n%s", out)
	}
	if found.asked().query.Has("limit") {
		t.Error("an unnamed limit was sent, a second copy of the index's own")
	}
}

// THE CREDENTIAL IS NEVER A FLAG, here as on every command that talks to a
// node: a token typed as an argument is in shell history and `ps`.
func TestChatletTakesNoTokenFlag(t *testing.T) {
	t.Parallel()
	node := newFakeChatletNode(t, http.StatusOK, `{}`)
	_, _, err := chatletCLI(t, node.server.URL, "channels", "-token", "x")
	if err == nil || !strings.Contains(err.Error(), "-token") {
		t.Errorf("`chatlet channels -token` gave %v, want the flag refused", err)
	}
	if node.asked().calls != 0 {
		t.Error("a refused flag still reached the node")
	}
}
