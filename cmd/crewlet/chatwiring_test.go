package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CHAT SURFACE A CHATLET COMPANY'S NODE SERVES, AND NOTHING ELSE:
// questions to read, act tools to write, and three of the human write
// surface's routes that destroy.
//
// # Why this exists
//
// Every chat seam the API takes is resolved PER QUESTION, and correctly so: a
// company on `chat.backend: vendor` or `none` has no Chatlet, and its node
// answers the questions `unknown_query`, the tools `unknown_tool` and the
// destructions `no_route` rather than serving a surface with nothing behind
// it. That tolerance has one failure mode, and it shipped once: a process that
// never wired the seams serves the same answers to a Chatlet company — an
// absent surface indistinguishable from a company that opted out, every unit
// test green and the dashboard talking to nothing. Nil is a legal value for
// every seam, so no compile check catches it; the assertion has to be made
// against a SERVED surface, which is what this does — the node built the way
// `crewlet run` builds it ([serveAPI]), asked whether each half is there.
//
// # And that it is ONE surface per gesture
//
// Every ordinary chat write is a tool the dashboard calls through
// `/operator/act` (ADR-0024). A REST route for one beside it would be a second
// path to the same write, decided and audited apart, so the paths those
// gestures once had must answer the router's own refusal on the served node —
// not merely in the api package's suite, which cannot see what this process
// mounts.
//
// Every probe goes through [chatProbe], which says why each is authenticated.
func TestAChatletCompanyServesItsChatSurface(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	if _, onChatlet := engine.OperatorChat(e); !onChatlet {
		t.Fatal("the test company runs no Chatlet, so this asserts nothing " +
			"— check companyYAML still leaves chat.backend to derive")
	}
	serve := chatProbe(t, servedChat(t, bootstrapFor(t, reservePort(t)), e), cliFixtureToken)
	absent := func(code string) bool {
		switch httpjson.Code(code) {
		case httpjson.CodeUnknownQuery, httpjson.CodeUnknownTool, httpjson.CodeNoRoute,
			httpjson.CodeMethodNotAllowed:
			return true
		}
		return false
	}

	// THE READS: every chat question, by its named route and by the generic
	// form, chat_search included.
	for _, path := range chatQuestionPaths {
		if status, code := serve(http.MethodGet, path, ""); absent(code) {
			t.Errorf("GET %s answered %d %s although this company runs Chatlet "+
				"— the read side is unwired", path, status, code)
		}
	}

	// THE WRITES: every chat tool the operator catalogue serves, the reads
	// among them answering that they are reads.
	for _, tool := range slices.Concat(chatlet.Tools(), chatlet.OperatorOnlyTools()) {
		if status, code := serve(http.MethodPost, "/operator/act/"+tool,
			`{"args":{}}`); absent(code) {
			t.Errorf("POST /operator/act/%s answered %d %s although this company "+
				"runs Chatlet — the operator surface was built without it",
				tool, status, code)
		}
	}

	// THE THREE THAT DESTROY, mounted beside the purge.
	for _, route := range chatDestructionRoutes {
		if status, code := serve(route.method, route.path, "{}"); absent(code) {
			t.Errorf("%s %s answered %d %s — not served, although this company "+
				"runs Chatlet", route.method, route.path, status, code)
		}
	}

	// AND NO ORDINARY GESTURE IS A ROUTE. `method_not_allowed` counts as
	// absent: several of these paths carry a GET read, so an unmounted POST
	// resolves to the pattern that is there and is refused for its method.
	for _, path := range []string{
		"/chatlet/channels", "/chatlet/dms", "/chatlet/read", "/chatlet/channels/c1",
		"/chatlet/channels/c1/messages", "/chatlet/channels/c1/join",
		"/chatlet/channels/c1/messages/m1/edit",
	} {
		if status, code := serve(http.MethodPost, path, "{}"); !absent(code) {
			t.Errorf("POST %s answered %d %q — a chat gesture is served beside the "+
				"act transport, as a second write path", path, status, code)
		}
	}
}

// A COMPANY OFF CHATLET IS SERVED NONE OF IT, IN THE THREE WORDS `crewlet
// chat` READS AS THAT.
//
// The other half of the case above. A company on `chat.backend: none` has no
// rooms, so its node answers every chat question `unknown_query`, every chat
// tool `unknown_tool` and every destruction `no_route` — exactly the three
// codes [chatletUnserved] names the axis for, and the only account this CLI gives
// of why a chat verb was refused. A surface that answered anything else — a
// question read against rooms nobody runs, a tool reaching an absent store —
// would reach an operator as the node's bare refusal, with nothing to say the
// company runs no Chatlet at all. Mutation: serve a company off Chatlet
// the native read sources, or answer its destructions anything but
// `no_route`, and this fails.
func TestACompanyOffChatletIsServedNoneOfIt(t *testing.T) {
	t.Parallel()
	port := reservePort(t)
	boot := bootstrapFor(t, port)
	boot.API.Auth.TOTP = iam.SecondFactorOptional
	company, err := config.ParseCompany([]byte(peopleCompanyYAML + "chat:\n  backend: none\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if _, onChatlet := engine.OperatorChat(e); onChatlet {
		t.Fatal("the premise: a company on chat.backend none runs Chatlet here")
	}
	surface := servedChat(t, boot, e)
	// A PERSON'S OWN TOKEN, holding admin: a question decides its seated
	// verb on the authority table before it asks whether the company runs
	// any rooms, so a credential acting as itself would be told `unseated`
	// here and the answer this holds would never be reached.
	serve := chatProbe(t, surface, founderToken(t, httpxtest.Pool(t),
		"http://127.0.0.1:"+strconv.Itoa(port)))
	for _, path := range chatQuestionPaths {
		if status, code := serve(http.MethodGet, path, ""); code !=
			string(httpjson.CodeUnknownQuery) {
			t.Errorf("GET %s answered %d %q on a company off Chatlet, want %s",
				path, status, code, httpjson.CodeUnknownQuery)
		}
	}
	for _, tool := range slices.Concat(chatlet.Tools(), chatlet.OperatorOnlyTools()) {
		if status, code := serve(http.MethodPost, "/operator/act/"+tool,
			`{"args":{}}`); code != string(httpjson.CodeUnknownTool) {
			t.Errorf("POST /operator/act/%s answered %d %q on a company off Chatlet"+
				", want %s", tool, status, code, httpjson.CodeUnknownTool)
		}
	}
	for _, route := range chatDestructionRoutes {
		if status, code := serve(route.method, route.path, "{}"); code !=
			string(httpjson.CodeNoRoute) {
			t.Errorf("%s %s answered %d %q on a company off Chatlet, want %s",
				route.method, route.path, status, code, httpjson.CodeNoRoute)
		}
	}
}

// chatQuestionPaths is every chat question, by its named route and by the
// generic `/query/` form, chat_search included.
var chatQuestionPaths = []string{
	"/chatlet/channels", "/chatlet/channels/c1", "/chatlet/channels/c1/messages",
	"/chatlet/channels/c1/threads/m1", "/chatlet/activity", "/chatlet/search?q=launch",
	"/chatlet/room-names", "/chatlet/rooms",
	"/query/chat_channels", "/query/chat_channel?channel_id=c1",
	"/query/chat_messages?channel_id=c1",
	"/query/chat_thread?channel_id=c1&root_id=m1", "/query/chat_activity",
	"/query/chat_search?q=launch", "/query/chat_room_names", "/query/chat_rooms",
}

// chatDestructionRoutes are the human write surface's three chat routes that
// destroy, now or on a delay.
var chatDestructionRoutes = []struct{ method, path string }{
	{http.MethodPost, "/chatlet/channels/c1/erase"},
	{http.MethodPost, "/chatlet/channels/c1/prune"},
	{http.MethodPut, "/chatlet/channels/c1/retention"},
}

// servedChat is the HTTP surface `crewlet run` serves for e ([serveNode]),
// taken down when the case ends.
func servedChat(t *testing.T, boot *config.Bootstrap, e *engine.Engine) *httpSurface {
	t.Helper()
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveNode: %v", err)
	}
	if surface == nil || surface.app == nil {
		t.Fatal("no HTTP surface")
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	return surface
}

// chatProbe asks surface one request under token, answering its status and
// the engine's code.
//
// IT ASSERTS MOUNTING, NOT BEHAVIOUR. Every probe is authenticated, because
// the guard runs BEFORE the mux resolves a path: an unauthenticated probe is
// 401 whether the route exists or not, and a 404 check without a credential
// cannot fail. With the fixture's Tier A token, which holds admin and acts as
// no seat, a served chat question or tool answers the seated rows' refusal,
// which is itself proof it was reached.
func chatProbe(t *testing.T, surface *httpSurface, token string) func(
	method, path, body string) (int, string) {

	t.Helper()
	return func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(opkey.Header, statelog.NewOpID(time.Now(), "probe"))
		rec := httptest.NewRecorder()
		surface.app.ServeHTTP(rec, req)
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(rec.Result().Body).Decode(&answer)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s refused the probe's token, so this case is asserting "+
				"nothing", method, path)
		}
		return rec.Code, answer.Error
	}
}

// A NODE IN A MAINTENANCE MODE STILL TAKES A PERSON'S READ STATE, AND NO CHAT
// WRITE.
//
// The mode forbids a node to publish, and every chat write is a record on the
// log — so the tools that write the rooms are not served there, nor the
// destructions. A person's read state is neither: it is the fleet's
// coordination record, and with every chat gesture the dashboard makes going
// through the act transport (ADR-0024), withholding `mark_chat_read` on such a
// node would leave a person's screen there no way to clear a badge at all. The
// read TOOLS go with the writes, on the knowledge base's terms, while the
// questions the dashboard asks still answer from the copy as it is. Mutation:
// hand the operator surface no read state on a node that may not publish, or
// its writer anyway, and this fails.
func TestAMaintenanceModeNodeTakesAPersonsReadStateAndNoChatWrite(t *testing.T) {
	t.Parallel()
	e := testEngineWith(t, engine.Options{Mode: statelog.ModeMaintenance})
	surface, err := serveNode(t, bootstrapFor(t, reservePort(t)), e)
	if err != nil {
		t.Fatalf("serveNode: %v", err)
	}
	if surface == nil || surface.app == nil {
		t.Fatal("no HTTP surface")
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	answers := func(method, path string) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(`{"args":{}}`))
		req.Header.Set("Authorization", "Bearer "+cliFixtureToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(opkey.Header, statelog.NewOpID(time.Now(), "probe"))
		rec := httptest.NewRecorder()
		surface.app.ServeHTTP(rec, req)
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(rec.Result().Body).Decode(&answer)
		return answer.Error
	}
	if code := answers(http.MethodPost, "/operator/act/"+chatlet.MarkChatReadTool); code ==
		string(httpjson.CodeUnknownTool) {
		t.Errorf("a maintenance-mode node does not serve %s", chatlet.MarkChatReadTool)
	}
	for _, write := range []string{chatlet.PostMessageTool, chatlet.CreateChannelTool,
		chatlet.ReactToMessageTool, chatlet.ListChannelsTool, chatlet.SearchMessagesTool} {
		if code := answers(http.MethodPost, "/operator/act/"+write); code !=
			string(httpjson.CodeUnknownTool) {
			t.Errorf("a maintenance-mode node serves %s (answered %q)", write, code)
		}
	}
	if code := answers(http.MethodPost, "/chatlet/channels/c1/erase"); code !=
		string(httpjson.CodeNoRoute) {
		t.Errorf("a maintenance-mode node serves the erase (answered %q)", code)
	}
	if code := answers(http.MethodGet, "/chatlet/channels"); code ==
		string(httpjson.CodeUnknownQuery) {
		t.Error("a maintenance-mode node does not answer the room list")
	}
}

// A NODE SERVING NO API STILL SHOWS ITS SEATS' WORKING INDICATORS.
//
// The node running a seat's turn is rarely the node a browser is attached to,
// so an indicator raised on it reaches a room as the seat's member of that
// room's presence, on this node's presence hub, which tells the node the room
// is open on — and when the hub that raised it was the API's, a node with no
// ingress role or `api.port: 0` raised every indicator into nothing: the seat
// working, every room showing nothing, and nothing anywhere to say so. So this
// builds the hub the way `crewlet run` builds it ([api.NewEngineChatLive],
// beside the engine and before any listener), binds NO listener, raises an
// indicator as the engine's status driver does, and asks the engine's own
// presence hub who is in the room, as its peers learn it. Red if the hub never
// learned the engine's presence hub.
func TestANodeServingNoAPIShowsItsSeatsIndicators(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	chatLive := chatNode(t.Context(), t, e)
	room := uuid.New()
	if !chatLive.Hub().SetStatus(t.Context(), notify.StatusPost{Handle: "ceo",
		Channel: room.String(), Turn: "run-1", Text: "reading the room"}) {
		t.Fatal("the hub refused the indicator")
	}
	if got := workingIn(t, e, room, func(got string) bool {
		return got == "ceo run-1 reading the room"
	}); got != "ceo run-1 reading the room" {
		t.Fatalf("the engine's presence hub shows %q working in the room, want the "+
			"seat the hub raised — the hub never learned the engine", got)
	}
}

// THE HUB'S INDICATORS GO UP AFTER THE CONTEXT THAT BUILT IT ENDS, AND COME
// DOWN WITH ITS OWNER'S STOP.
//
// `crewlet run` builds the hub under the signal context, which is cancelled
// at the START of the drain — and the drain is when the turns already running
// post their replies and raise and lower their indicators, which a room on
// another node learns of through the presence hub. So a turn raising one
// through the engine's own status driver after that context ended must still
// show in the room, and [api.EngineChatLive.Stop] must take it down: the
// driver built over the hub goes with it, and lowers every line it held,
// rather than leaving the room showing a seat at work for the presence hub's
// whole expiry.
func TestTheChatHubsIndicatorsOutliveItsContextAndEndWithItsStop(t *testing.T) {
	t.Parallel()
	built, cancel := context.WithCancel(t.Context())
	e := testEngine(t)
	chatLive := chatNode(built, t, e)
	cancel()

	room := uuid.New()
	session := e.Status().Begin(t.Context(), "ceo", "run-1", "execute", map[string]string{
		notify.TransportField: chatlet.Source, notify.ChannelField: room.String(),
		notify.ChannelTypeField: string(chatlet.KindPublic), notify.MessageIDField: "m-1",
		notify.ThreadAnchorField: "m-1", notify.FollowReasonField: string(chatlet.ReasonMention),
		notify.UserField: "founder", notify.ActorField: "founder",
		chatlet.MetaAuthorKind: string(chatlet.AuthorHuman),
	})
	if session == nil {
		t.Fatal("the engine's driver raised nothing for a mention in a Chatlet room")
	}
	working := func(got string) bool { return strings.HasPrefix(got, "ceo run-1 ") }
	if got := workingIn(t, e, room, working); !working(got) {
		t.Fatalf("the room shows %q working after the building context ended, want ceo", got)
	}

	chatLive.Stop(context.WithoutCancel(t.Context()))
	nobody := func(got string) bool { return got == "" }
	if got := workingIn(t, e, room, nobody); got != "" {
		t.Fatalf("the room still shows %q working after the hub's owner stopped it", got)
	}
	// AND STAYS DOWN for a second more: nothing the stopped hub held raises
	// it again.
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		if got := workingNow(t, e, room); got != "" {
			t.Fatalf("the room shows %q working again after the hub's owner stopped it", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// chatNode builds a node's engine and its chat hub the way `crewlet run`
// does, the hub under ctx and no listener at all, and takes both down in
// [shutdown]'s order: the hub before the engine whose presence hub it raises
// indicators on — cleanups run last-registered first, and [testEngine]
// registered the engine's.
func chatNode(ctx context.Context, t *testing.T, e *engine.Engine) *api.EngineChatLive {
	t.Helper()
	if _, onChatlet := engine.OperatorChat(e); !onChatlet {
		t.Fatal("the test company runs no Chatlet, so this asserts nothing")
	}
	chatLive := api.NewEngineChatLive(ctx, e)
	t.Cleanup(func() { chatLive.Stop(context.WithoutCancel(ctx)) })
	return chatLive
}

// workingIn is [workingNow] once want holds of it, asked again for up to two
// seconds, because the driver raises on its own goroutine and the room is
// shown a change on its next fan-out; the last answer is returned either way.
func workingIn(t *testing.T, e *engine.Engine, room uuid.UUID, want func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := workingNow(t, e, room)
		if want(got) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// workingNow is who the engine's presence hub shows working in a chat room, as
// "seat turn status", or "" for nobody: what a socket joining the room is
// handed, which is the room as the node's sockets were last shown it.
func workingNow(t *testing.T, e *engine.Engine, room uuid.UUID) string {
	t.Helper()
	var roster presence.Roster
	session, err := e.Presence().Join(presence.ChatRoom(room),
		presence.Local{Seat: "watcher", Kind: iam.ActorHuman},
		func(_ string, r presence.Roster) bool {
			roster = r
			return true
		})
	if err != nil {
		t.Fatalf("join the room: %v", err)
	}
	e.Presence().Leave(presence.ChatRoom(room), session)
	for _, m := range roster.Members {
		if m.Kind == iam.ActorAgent {
			return m.Seat + " " + m.Turn + " " + m.Status
		}
	}
	return ""
}

// `crewlet run` BUILDS THE HUB ON EVERY NODE, whatever it serves.
//
// The two cases above hold the hub the constructor builds; this holds that
// [runEngine] builds it on every boot path past the engine — which no test
// can drive end to end, since the process is signal-driven — by reading the
// source: the call is what a statement of runEngine's own body assigns, not
// one nested in a branch on api.port or a role — or in any other statement a
// path can skip — and not one serveAPI makes, which returns before any of its
// surface on a node that binds nothing. Mutation: move the call under `if
// boot.API.Port != 0`, into a switch or a closure, or into serveAPI, and this
// fails.
func TestRunBuildsTheChatHubOnEveryNode(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	builds := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewEngineChatLive" {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "api"
	}
	calls := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			found = found || builds(n)
			return !found
		})
		return found
	}
	var run, served *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			switch fn.Name.Name {
			case "runEngine":
				run = fn
			case "serveAPI":
				served = fn
			}
		}
	}
	if run == nil || served == nil {
		t.Fatal("main.go has no runEngine or serveAPI, so this gate reads nothing")
	}
	if calls(served) {
		t.Error("serveAPI builds the chat hub, which a node serving no API never reaches")
	}
	// THE CALL ITSELF, as what a statement of runEngine's own body assigns:
	// one anywhere inside a statement — an if, a switch, a loop, a closure —
	// is one a path through it can skip.
	top := false
	for _, stmt := range run.Body.List {
		if assign, ok := stmt.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 &&
			builds(assign.Rhs[0]) {
			top = true
		}
	}
	if !top {
		t.Error("runEngine does not build the chat hub as a statement of its own " +
			"body, so a node that serves no API — or one a branch skips — answers " +
			"shows no room the indicators its seats raise")
	}
}
