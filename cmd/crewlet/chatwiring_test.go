package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// chatWiringToken is this case's own operator credential; the guard runs
// ahead of the mux, so the surface has to admit the request before a route's
// presence is observable at all.
const chatWiringToken = "chat-wiring-test-token"

// THE CHAT SURFACE IS REACHABLE ON A COMPANY THAT RUNS NATIVE CHAT, AND IT IS
// THE SURFACE MAIN SERVES: questions to read, act tools to write, and two
// operator routes that destroy.
//
// # Why this exists
//
// Every chat dependency [api.Options] and the operator surface take is
// OPTIONAL, and correctly so: a company on `chat.backend: vendor` or `none`
// has no native chat, and the API leaves the questions, the tools and the
// routes unregistered rather than serving a surface with nothing behind it.
//
// That tolerance has one failure mode, and it shipped: if the process simply
// never fills them, a native-chat company gets the same unregistered surface —
// and an absent route is indistinguishable from a company that opted out. The
// whole feature was unreachable in the shipped binary, every unit test passed,
// and the dashboard talked to nothing. A compile check could not catch it —
// nil is a legal value for every seam — so the assertion has to be made
// against a SERVED surface, which is what this does: build the node the way
// `crewlet run` builds it, and ask whether each half is there.
//
// # And that it is ONE surface per gesture
//
// Every ordinary chat write is a tool the dashboard calls through
// `/operator/act` (ADR-0024). A REST route for one beside it would be a second
// identity and an unaudited write path, so the paths those gestures once had
// must answer the router's own refusal on the served node — not merely in the
// api package's own suite, which cannot see what this process mounts.
//
// IT ASSERTS MOUNTING, NOT BEHAVIOUR. Every probe is authenticated, because
// the guard runs BEFORE the mux resolves a path: an unauthenticated probe is
// 401 whether the route exists or not, and a 404 check without a token is a
// check that cannot fail.
func TestANativeChatCompanyServesItsChatSurface(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	if _, native := engine.OperatorChat(e); !native {
		t.Fatal("the test company runs no native chat, so this asserts nothing " +
			"— check companyYAML still leaves chat.backend to derive")
	}
	boot := bootstrapFor(t, freePort(t))
	boot.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: chatWiringToken}}
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveNode: %v", err)
	}
	if surface == nil {
		t.Fatal("no HTTP surface")
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })

	serve := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+chatWiringToken)
		rec := httptest.NewRecorder()
		surface.app.ServeHTTP(rec, req)
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(rec.Result().Body).Decode(&answer)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s still refused the test token, so this case is "+
				"asserting nothing", method, path)
		}
		return rec.Code, answer.Error
	}

	// THE READS: every question, chat_search included. Anything but
	// "unknown query" proves it is registered.
	for _, what := range []string{
		"chat_channels", "chat_channel", "chat_messages",
		"chat_thread", "chat_mentions", "chat_search",
	} {
		if _, code := serve(http.MethodGet, "/query/"+what, ""); code == "unknown_query" {
			t.Errorf("the %s query is unregistered although this company runs "+
				"native chat — the read side is unwired", what)
		}
	}

	// THE WRITES: every chat tool the operator catalogue serves, the reads
	// among them answering that they are reads. `unknown_tool` is the one
	// answer that says the catalogue was built without chat.
	tools := append(chat.Tools(), chat.OperatorOnlyTools()...)
	slices.Sort(tools)
	for _, tool := range slices.Compact(tools) {
		if _, code := serve(http.MethodPost, "/operator/act/"+tool, "{}"); code == "unknown_tool" {
			t.Errorf("POST /operator/act/%s is not a tool this node serves, although "+
				"the company runs native chat — the operator surface was built "+
				"without it", tool)
		}
	}

	// THE TWO THAT DESTROY, mounted beside the purge.
	for _, path := range []string{
		"/chat/channels/c1/erase", "/chat/channels/c1/prune",
	} {
		if status, code := serve(http.MethodPost, path, "{}"); code == "no_route" ||
			code == "method_not_allowed" {
			t.Errorf("POST %s answered %d %s — not mounted, although this company "+
				"runs native chat", path, status, code)
		}
	}

	// AND NO ORDINARY GESTURE IS A ROUTE. 405 counts as absent: several of
	// these paths carry a GET read, so an unmounted POST resolves to the
	// pattern that is there and is refused for its method.
	for _, path := range []string{
		"/chat/channels", "/chat/dms", "/chat/read",
		"/chat/channels/c1/messages", "/chat/channels/c1/join",
		"/chat/channels/c1/messages/m1/edit",
	} {
		if status, code := serve(http.MethodPost, path, "{}"); code != "no_route" &&
			code != "method_not_allowed" {
			t.Errorf("POST %s answered %d %q — a chat gesture is served beside the "+
				"act transport, as a second write path nothing audits", path,
				status, code)
		}
	}
}

// A NODE SERVING NO API STILL ANSWERS FOR ITS SEATS' WORKING INDICATORS.
//
// The node running a seat's turn is rarely the node a browser is attached to,
// so an indicator raised on it reaches a room through that node's answer to a
// peer's presence probe — and the hub that answers used to be started by the
// API. A node with no ingress role, or `api.port: 0`, therefore recorded every
// indicator its seats raised in a map no probe read: the seat working, every
// room showing nothing, and nothing anywhere to say so.
//
// So this builds the node the way `crewlet run` does
// ([api.NewEngineWithChat]) and binds NO listener at all, raises an indicator
// as the engine's status driver does, and asks the fleet's own subject as a
// peer would. Red if the hub is not running, and red if it never learned the
// engine: its answerer rides the engine's broker, and it decides who may be in
// a room through the engine's router.
func TestANodeServingNoAPIAnswersForItsSeatsIndicators(t *testing.T) {
	t.Parallel()
	e, chatLive := chatNode(t.Context(), t)
	chatLive.Hub().SetStatus(t.Context(), "ceo", "room-1", "", "reading the room")
	if !answersFor(t, e, "ceo", 3*stream.ChatPresenceInterval+5*time.Second) {
		t.Fatal("a node serving no API never answered a presence probe for the " +
			"indicator its own seat raised — the hub is not running, or never " +
			"learned the engine")
	}
}

// THE HUB OUTLIVES THE CONTEXT THAT BUILT IT, AND ITS OWNER'S STOP ENDS IT.
//
// `crewlet run` builds the node under the signal context, which is cancelled
// at the START of the drain — and the drain is when the turns already running
// post their replies and lower their indicators, which a peer's room learns of
// through this hub. A hub that ended with that context went dark for exactly
// the stretch an operator watching a rolling restart most wants it. So the
// context's cancellation must not end it, and [api.EngineChatLive.Stop] must.
func TestTheChatHubOutlivesTheContextThatBuiltIt(t *testing.T) {
	t.Parallel()
	built, cancel := context.WithCancel(t.Context())
	e, chatLive := chatNode(built, t)
	chatLive.Hub().SetStatus(t.Context(), "ceo", "room-1", "", "reading the room")
	if !answersFor(t, e, "ceo", 3*stream.ChatPresenceInterval+5*time.Second) {
		t.Fatal("the hub never answered at all, so this asserts nothing")
	}

	cancel()
	// PAST THE TEARDOWN a cancelled hub would make: its loops return on the
	// context at once, and the answerer is unsubscribed on the way out.
	time.Sleep(200 * time.Millisecond)
	if !answersFor(t, e, "ceo", 3*stream.ChatPresenceInterval) {
		t.Fatal("the hub stopped answering when the context that built the " +
			"node was cancelled — which is the start of the drain, when the " +
			"turns that are finishing still raise and lower indicators")
	}

	chatLive.Stop()
	if answersFor(t, e, "ceo", 2*stream.ChatPresenceAsk) {
		t.Fatal("the hub still answered after its owner stopped it")
	}
}

// chatNode builds a node the way `crewlet run` does, under ctx, binding no
// listener, and tears it down in [shutdown]'s order.
func chatNode(ctx context.Context, t *testing.T) (*engine.Engine, *api.EngineChatLive) {
	t.Helper()
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	boot := bootstrapFor(t, 0)
	seedEngineStore(t, boot)
	e, chatLive, err := api.NewEngineWithChat(ctx, engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		t.Fatalf("api.NewEngineWithChat: %v", err)
	}
	// IN shutdown's ORDER: the hub before the engine whose broker its
	// answerer is subscribed on. Cleanups run last-registered first.
	t.Cleanup(func() { e.Stop(context.WithoutCancel(ctx)) })
	t.Cleanup(chatLive.Stop)
	if _, native := engine.OperatorChat(e); !native {
		t.Fatal("the test company runs no native chat, so this asserts nothing")
	}
	return e, chatLive
}

// answersFor reports whether a presence probe on room-1 is answered naming
// handle as working within wait — the question a peer's hub asks.
func answersFor(t *testing.T, e *engine.Engine, handle string, wait time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), stream.ChatPresenceAsk)
		replies, err := e.Backends().Queue.Ask(ctx, topics.ChatPresence,
			[]byte(`{"version":1,"channels":["room-1"]}`), 0)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("probe: %v", err)
		}
		for _, reply := range replies {
			if strings.Contains(string(reply), `"`+handle+`"`) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
