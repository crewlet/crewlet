package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A COMPANY OFF NATIVE CHAT IS HANDED NO CHAT SEAM, and "no seam" is an untyped
// nil — never the router's facade held in the interface, which the read
// surface and the API would take for a surface and mount, answering a company
// that talks on Slack with an empty room as though one had been read.
func TestACompanyOffNativeChatIsHandedNoChatSeam(t *testing.T) {
	t.Parallel()
	company, err := config.ParseCompany([]byte(companyYAML + "chat:\n  backend: none\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := testEngineWith(t, engine.Options{Company: company})
	if _, native := engine.OperatorChat(e); native {
		t.Fatal("the premise: a company on chat.backend none runs a native chat here")
	}
	var opts api.Options
	api.EngineChat(&opts, e, nil)
	if opts.Sources.Chat != nil || opts.Sources.ChatSearch != nil || opts.ChatPurger != nil {
		t.Errorf("a company off native chat was handed chat seams: reads %T, search %T, "+
			"destructions %T", opts.Sources.Chat, opts.Sources.ChatSearch, opts.ChatPurger)
	}
}

// THE OPERATOR'S SURFACES ARE HANDED THE ESTATE ROUTER'S SEAMS, never this
// node's own copy: the board and every other REST read, a project's file rows,
// the purge, and every seam of the operator's catalogue, chat's included. Read
// straight off the node's copy, a data node whose copy was out of service —
// wrong rather than behind, or its file shut — sent its seats' calls to a
// peer's copy and went on answering its operator from the one it had stopped
// serving. The router's facade is the only type that routes, so a seam of any
// other type here is a surface reading one node's copy whatever its state.
func TestTheOperatorsSurfacesAreHandedTheRouter(t *testing.T) {
	t.Parallel()
	e := testEngine(t)

	if _, ok := nativeWork(e).(estate.Work); !ok {
		t.Errorf("the REST reads are handed %T, want the router's estate.Work", nativeWork(e))
	}
	if _, ok := nativeFiles(e).(estate.Work); !ok {
		t.Errorf("the file rows are handed %T, want the router's estate.Work", nativeFiles(e))
	}
	if _, ok := nativePages(e).(estate.Pages); !ok {
		t.Errorf("the knowledge reads are handed %T, want the router's estate.Pages", nativePages(e))
	}
	if _, ok := nativeWorkSearch(e).(estate.Work); !ok {
		t.Errorf("the ranked search is handed %T, want the router's estate.Work", nativeWorkSearch(e))
	}
	if _, ok := nativePurger(e).(purgeAdapter); !ok {
		t.Errorf("the purge is handed %T, want the router's writer", nativePurger(e))
	}
	// AND CHAT'S: the rooms, the search a person runs over them and the two
	// destructions, as the one constructor `crewlet run` and the end-to-end
	// suite both wire them through fills them. Each signature is one this
	// node's own copy also answers — the reader, the index and the store —
	// so only the type says which copy a screen is drawn from.
	var chatOpts api.Options
	api.EngineChat(&chatOpts, e, nil)
	if _, ok := chatOpts.Sources.Chat.(estate.Chat); !ok {
		t.Errorf("the chat reads are handed %T, want the router's estate.Chat",
			chatOpts.Sources.Chat)
	}
	if _, ok := chatOpts.Sources.ChatSearch.(estate.ChatSearch); !ok {
		t.Errorf("the chat search is handed %T, want the router's estate.ChatSearch",
			chatOpts.Sources.ChatSearch)
	}
	if _, ok := chatOpts.ChatPurger.(estate.Chat); !ok {
		t.Errorf("the chat erase and prune are handed %T, want the router's estate.Chat",
			chatOpts.ChatPurger)
	}
	// AND A PERSON'S READ STATE IS THE FLEET'S, which a cursor follows
	// across nodes.
	if chatOpts.Sources.ChatReads != e.Backends().Fleet {
		t.Errorf("a person's read state is read from %T, want the fleet's",
			chatOpts.Sources.ChatReads)
	}
	// AND THE LIVE HUB'S, which decides who a committed frame and a room's
	// presence may be shown to. Off this node's copy, a node holding no data
	// — exactly the node whose seats run the turns — had no reader at all and
	// refused every presence probe, so its seats' indicators never left it.
	if _, ok := api.EngineChatRooms(e).(estate.Chat); !ok {
		t.Errorf("the live hub decides visibility with %T, want the router's estate.Chat",
			api.EngineChatRooms(e))
	}

	opts := api.EngineOperatorOptions(e, nil)
	work := opts.Work
	if work.Reader == nil || opts.Pages.Reader == nil || opts.Chat.Reader == nil {
		t.Fatalf("the premise: a company on the native tracker, knowledge base and "+
			"chat hands the operator's surface none of them (work %v, pages %v, "+
			"chat %v)", work.Reader, opts.Pages.Reader, opts.Chat.Reader)
	}
	operator := builtin.Actor{Handle: "founder", Kind: tracker.AuthorOperator, OperatorID: "founder"}
	for _, seam := range []struct {
		name string
		got  any
	}{
		{"Reader", work.Reader},
		{"Files", work.Files},
		{"Inbox", work.Inbox},
		{"Writer", work.Writer(operator)},
		{"Dependencies", work.Dependencies(operator)},
		{"Merges", work.Merges(operator)},
		{"Moves", work.Moves(operator)},
		{"FileWriter", work.FileWriter(operator)},
		{"ViewWriter", work.ViewWriter(operator)},
		{"CatalogueWriter", work.CatalogueWriter(operator)},
		{"PersonWriter", work.PersonWriter(operator)},
		{"TrashWriter", work.TrashWriter(operator)},
		{"Placer", work.Placer(operator)},
		{"ProjectWriter", work.ProjectWriter(operator)},
		{"Pages.Reader", opts.Pages.Reader},
		{"Pages.Writer", opts.Pages.Writer},
		// AND CHAT'S, which the dashboard writes every gesture through
		// (ADR-0024): a half handed this node's own copy answered the chat
		// screen from rows the node had stopped serving.
		{"Chat.Reader", opts.Chat.Reader},
		{"Chat.Writer", opts.Chat.Writer},
		// AND THE SEARCH, which is the one seam whose concrete type is
		// the engine's own rather than the router's facade: a searcher
		// built over this node's index would answer the same signature
		// from a copy the node may have stopped serving.
		{"Chat.Search", opts.Chat.Search},
	} {
		switch got := seam.got.(type) {
		case estate.Work, estate.WorkWriter, estate.Pages, estate.Chat:
		case engine.ChatSearcher:
			if got.Search == (estate.ChatSearch{}) {
				t.Errorf("the operator's %s holds no router", seam.name)
			}
		default:
			t.Errorf("the operator's MCP seam %s is %T, want the router's", seam.name, seam.got)
		}
	}

	// AND EVERY WRITE'S WAIT IS THE ROUTER'S SESSION FLOOR, never this
	// node's own applier: a write hands its position over so the next read
	// sees it, and the copy that answers that read is whichever the router
	// picks — a peer's, once this node's is out of service. A wait on this
	// node's applier is the same signature and waits on the wrong copy.
	floor := reflect.ValueOf(e.AwaitEstate).Pointer()
	for name, await := range map[string]func(context.Context, statelog.Position) error{
		"Work.Await":  work.Await,
		"Pages.Await": opts.Pages.Await,
		"Chat.Await":  opts.Chat.Await,
	} {
		if await == nil || reflect.ValueOf(await).Pointer() != floor {
			t.Errorf("the operator's %s is not the router's session floor "+
				"(engine.AwaitEstate)", name)
		}
	}
}

// A NODE IN A MAINTENANCE MODE STILL TAKES A PERSON'S READ STATE, AND NOTHING
// THAT READS OR WRITES THE ROOMS.
//
// The mode forbids a node to publish, and every chat write is a record on the
// log — so the gestures that write the rooms are not served, and on the
// wiki's terms neither are the tools that read them. A person's read state is
// neither: it is the fleet's coordination store, and with every chat gesture
// the dashboard makes going through this catalogue (ADR-0024), withholding
// `mark_chat_read` there would leave a person's screen on such a node no way
// to clear a badge at all.
func TestAMaintenanceModeNodeTakesAPersonsReadStateAndNoChatWrite(t *testing.T) {
	t.Parallel()
	e := testEngineWith(t, engine.Options{Mode: statelog.ModeMaintenance})

	opts := api.EngineOperatorOptions(e, nil)
	if opts.Chat.Cursors == nil || opts.Chat.ReadState == nil || opts.Chat.Actor == nil {
		t.Fatalf("a maintenance-mode node serves no read state (cursors %v, "+
			"actor set %v)", opts.Chat.Cursors, opts.Chat.Actor != nil)
	}
	if opts.Chat.Writer != nil || opts.Chat.Reader != nil || opts.Chat.Search != nil {
		t.Errorf("a maintenance-mode node hands its operator chat's log (reader %T, "+
			"writer %T, search %T)", opts.Chat.Reader, opts.Chat.Writer, opts.Chat.Search)
	}
	s, err := operator.New(opts)
	if err != nil || s == nil {
		t.Fatalf("operator.New: %v", err)
	}
	served := s.Tools()
	if !slices.Contains(served, chat.MarkChatReadTool) {
		t.Errorf("a maintenance-mode node does not serve %s: %v", chat.MarkChatReadTool, served)
	}
	for _, write := range []string{builtin.PostMessageTool, chat.CreateChannelTool,
		builtin.ListChannelsTool, builtin.SearchMessagesTool} {
		if slices.Contains(served, write) {
			t.Errorf("a maintenance-mode node serves %s", write)
		}
	}
	if _, err := opts.Chat.Cursors.AdvanceChatRead(t.Context(), "ceo", coord.ChatReadDelta{
		Cursors: map[string]int64{"6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f": 3},
		At:      time.Now().UTC(),
	}); err != nil {
		t.Errorf("a flush on a maintenance-mode node: %v", err)
	}
}

// A PERSON'S READ STATE IS THE FLEET'S, AND A FLUSH THAT LANDED IS TOLD TO
// THEIR OTHER SCREENS. The operator surface is the only one with a reader who
// has read state, so it is the only one handed both halves: `list_channels`
// counts against what the coordination store holds for that person, and
// `mark_chat_read` writes it there and pushes the result to that person's
// sockets — a cursor is on no log, so nothing else ever announces it.
func TestAPersonsReadStateIsTheFleetsAndReachesTheirOtherScreens(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	pushed := &personPushes{}
	opts := api.EngineOperatorOptions(e, pushed)
	if opts.Chat.Cursors == nil || opts.Chat.ReadState == nil {
		t.Fatalf("the premise: a native-chat company's operator surface writes and "+
			"reads a person's read state (cursors %v)", opts.Chat.Cursors)
	}
	const room = "6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f"
	state, err := opts.Chat.Cursors.AdvanceChatRead(t.Context(), "ceo", coord.ChatReadDelta{
		Cursors: map[string]int64{room: 42}, At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("the flush: %v", err)
	}
	// NAMED BY ITS PERSON ([coord.PersonChatRead]): the revision orders one
	// person's states only, so a screen must be able to tell whose this is.
	want := coord.PersonChatRead{Handle: "ceo", ChatReadState: state}
	if got := pushed.all(); len(got) != 1 || got[0].handle != "ceo" ||
		got[0].kind != stream.KindChatCursorMoved ||
		!reflect.DeepEqual(got[0].data, any(want)) {

		t.Errorf("the flush was pushed as %+v, want one %s to ceo carrying %+v",
			got, stream.KindChatCursorMoved, want)
	}
	read, err := opts.Chat.ReadState(t.Context(), "ceo")
	if err != nil || read.Cursors[room] != 42 {
		t.Errorf("the listing counts against %+v (%v), want the cursor the flush wrote",
			read, err)
	}

	// AND A SURFACE WITH NO LIVE CHANNEL STILL WRITES, telling nobody.
	quiet := api.EngineOperatorOptions(e, nil)
	if _, err := quiet.Chat.Cursors.AdvanceChatRead(t.Context(), "cto", coord.ChatReadDelta{
		Cursors: map[string]int64{room: 7}, At: time.Now().UTC(),
	}); err != nil {
		t.Errorf("a flush on a node with no live channel: %v", err)
	}
}

// A PERSON'S LISTING COUNTS WHAT THEIR FLUSH LEFT UNREAD, THROUGH THE ENGINE'S
// OWN SEAMS. `mark_chat_read` writes the read state into the fleet's
// coordination store and `list_channels` counts each room against it through
// the router's reader — two seams of one surface that have to meet on the same
// record for the same person, or a person's assistant is told every room is
// wholly unread. A room with three messages in it, a flush through the first,
// and the listing says two.
func TestAPersonsListingCountsWhatTheirFlushLeftUnread(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	opts := api.EngineOperatorOptions(e, nil)
	rooms, ok := opts.Chat.Writer.(estate.Chat)
	if !ok || opts.Chat.Cursors == nil || opts.Chat.ReadState == nil {
		t.Fatalf("the premise: a native-chat company's operator surface writes "+
			"rooms and a person's read state (writer %T)", opts.Chat.Writer)
	}
	ctx := t.Context()
	ceo := chat.Actor{Handle: "ceo", Kind: chat.AuthorAgent}
	room, err := rooms.CreateChannel(ctx, ceo, chat.NewChannel{
		Name: "launch", Kind: chat.KindPublic, Members: []chat.Member{{Handle: "cto"}},
	})
	if err != nil {
		t.Fatalf("create #launch: %v", err)
	}
	var said []chat.Written
	for _, body := range []string{"what ships on friday?", "the importer", "and the docs"} {
		written, err := rooms.Post(ctx, ceo, room.Channel.ID, chat.NewMessage{Body: body})
		if err != nil {
			t.Fatalf("post %q: %v", body, err)
		}
		said = append(said, written)
	}
	if _, err := opts.Chat.Cursors.AdvanceChatRead(ctx, "cto", coord.ChatReadDelta{
		Cursors: map[string]int64{room.Channel.ID: said[0].Outcome.Position.Packed()},
		At:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("the flush: %v", err)
	}

	// AS THE PERSON: who the call acts as is [operator.ChatActor]'s, held by
	// the operator package's own cases; here it is fixed, so what is under
	// test is the engine's two seams meeting.
	deps := opts.Chat
	deps.Actor = func(context.Context, *turnctx.Turn) (chat.Actor, error) {
		return chat.Actor{Handle: "cto", Kind: chat.AuthorOperator, OperatorID: "cto-token"}, nil
	}
	var list tools.Callable
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Chat: deps}) {
		if tool.Name() == builtin.ListChannelsTool {
			list = tool
		}
	}
	if list == nil {
		t.Fatal("the engine's operator surface serves no list_channels")
	}
	got, err := list.Call(ctx, map[string]any{})
	if err != nil || got.Failed {
		t.Fatalf("list_channels: %v %s", err, got.Output)
	}
	var answer struct {
		Channels []struct {
			ID     string `json:"id"`
			Unread *int   `json:"unread"`
		} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v: %s", err, got.Output)
	}
	for _, row := range answer.Channels {
		if row.ID != room.Channel.ID {
			continue
		}
		if row.Unread == nil || *row.Unread != 2 {
			t.Errorf("#launch is listed without an unread count of 2 after a flush "+
				"through the first of three messages: %s", got.Output)
		}
		return
	}
	t.Fatalf("#launch is not in cto's listing: %s", got.Output)
}

// personPushes records every push the operator surface addressed to a person.
type personPushes struct {
	mu   sync.Mutex
	seen []personPush
}

type personPush struct {
	handle string
	kind   stream.Kind
	data   any
}

func (p *personPushes) PushTo(handle string, kind stream.Kind, data any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, personPush{handle: handle, kind: kind, data: data})
}

func (p *personPushes) all() []personPush {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

// A PERSON IS BOUND THROUGH THE NODE'S OWN CHAIN ON EVERY SURFACE `crewlet run`
// SERVES. A token's binding to a person may be a `${VAR}`, and the one here is
// set in no process — only in the environment the engine was handed — so a
// surface wired to read the process environment instead leaves the founder
// unbound: the operator surface writes as the bare token and reads their rows
// under one name, and the dashboard answers the token as nobody. Both are wired
// where an engine meets the API — [api.EngineOperatorOptions] and [serveAPI]'s
// sources — so they are held here, where a test boots an engine and serves it.
// One engine, because both read the same chart.
func TestEverySurfaceBindsAPersonThroughTheNodesOwnChain(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_CLI_TEST_BOUND_FOUNDER"
	if _, set := os.LookupEnv(variable); set {
		t.Fatalf("the premise: %s is set in no process", variable)
	}
	company, err := config.ParseCompany([]byte(companyYAML + `  - name: Founder
    kind: human
    contact:
      crewlet_operator_id: ${` + variable + `}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := testEngineWith(t, engine.Options{Company: company,
		Environment: config.MapSource{variable: "ops-founder"}})

	// THE OPERATOR'S SURFACE: a write through the founder's token is the
	// founder's own, and a read about the founder is by both of their names.
	t.Run("the_operator_surface", func(t *testing.T) {
		t.Parallel()
		opts := api.EngineOperatorOptions(e, nil)
		work := opts.Work
		actor, err := work.Actor(auth.WithOperator(t.Context(), "ops-founder"), nil)
		if err != nil {
			t.Fatalf("actor: %v", err)
		}
		if actor.Seat != "founder" {
			t.Errorf("the founder's token writes as seat %q, want the founder the "+
				"handed environment binds it to", actor.Seat)
		}
		if got := work.Party("founder"); got.OperatorID != "ops-founder" {
			t.Errorf("the founder's party is %+v, want the credential the handed "+
				"environment binds to them", got)
		}
		// AND WHAT THE FOUNDER SAYS IN A ROOM IS THE FOUNDER'S OWN: a
		// message's author is the bound seat itself ([operator.ChatActor]),
		// so an unbound token has nothing to speak as at all.
		speaker, err := opts.Chat.Actor(auth.WithOperator(t.Context(), "ops-founder"), nil)
		if err != nil {
			t.Fatalf("chat actor: %v", err)
		}
		if speaker.Handle != "founder" || speaker.OperatorID != "ops-founder" {
			t.Errorf("the founder's token speaks in a room as %+v, want the founder "+
				"the handed environment binds it to", speaker)
		}
	})

	// THE DASHBOARD'S READS, through the API `crewlet run` serves: the viewer
	// is the person the token is bound to.
	t.Run("the_dashboard", func(t *testing.T) {
		t.Parallel()
		boot := bootstrapFor(t, 0)
		boot.API.Port = freePort(t)
		boot.API.Auth.Tokens = []config.APIToken{{ID: "ops-founder", Token: "founder-test-token"}}
		surface, err := serveNode(t, boot, e)
		if err != nil {
			t.Fatalf("serveAPI: %v", err)
		}
		t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/viewer", nil)
		req.Header.Set("Authorization", "Bearer founder-test-token")
		rec := httptest.NewRecorder()
		surface.app.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /viewer = %d: %s", rec.Code, rec.Body)
		}
		var viewer map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &viewer); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if viewer["handle"] != "founder" {
			t.Errorf("the founder's token views the dashboard as %v, want the founder "+
				"the handed environment binds it to", viewer["handle"])
		}

		// AND THE TWO CHAT DESTRUCTIONS on the same surface, which resolve
		// the person themselves rather than reading the act transport's
		// pin: the founder is past the seat check, and what stops this
		// erase is the reason it does not give — never `unbound`.
		erase := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			"/chat/channels/6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f/erase",
			strings.NewReader(`{"message_ids":[]}`))
		erase.Header.Set("Authorization", "Bearer founder-test-token")
		rec = httptest.NewRecorder()
		surface.app.ServeHTTP(rec, erase)
		var refused struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &refused)
		if rec.Code != http.StatusBadRequest || refused.Error != "reason_required" {
			t.Errorf("the founder's token erases as nobody: POST erase = %d %s, "+
				"want 400 reason_required past the seat the handed environment "+
				"binds it to", rec.Code, rec.Body)
		}
	})
}
