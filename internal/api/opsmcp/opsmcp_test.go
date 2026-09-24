package opsmcp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fixed is a surface serving these deps for as long as a test runs.
func fixed(deps builtin.OperatorDeps) opsmcp.Options {
	return opsmcp.Options{Surface: func() opsmcp.Surface {
		return opsmcp.Surface{Deps: deps}
	}}
}

// stubWork is the tracker half with every stub a catalogue check needs.
func stubWork() builtin.WorkDeps {
	return builtin.WorkDeps{
		Reader: stubWorkReader{}, Writer: stubWorkWriter,
		Merges: stubWorkMerger, Actor: opsmcp.WorkActor,
	}
}

// stubPages is the knowledge base's half on the same terms.
func stubPages() builtin.PageDeps {
	return builtin.PageDeps{Reader: stubPageReader{}, Writer: stubPageWriter{},
		Actor: opsmcp.PageActor}
}

// NOTHING TO ASK IS NO SURFACE: with no surface to consult there is nothing a
// request could ever be served from, so the route is not mounted at all.
func TestNoSurfaceBuildsNoServer(t *testing.T) {
	t.Parallel()
	if s := opsmcp.New(opsmcp.Options{}); s != nil {
		t.Errorf("a surface with nothing to consult was built, serving %v", s.Tools())
	}
}

// NOTHING SERVED NOW ANSWERS AS AN UNMOUNTED ROUTE DOES, and the same route
// serves the moment a revision serves something — with no restart, because the
// surface is asked on every request.
func TestNothingServedAnswersAsAnAbsentRouteUntilSomethingIs(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	now := opsmcp.Surface{Closed: map[opsmcp.Half]string{
		opsmcp.Work: "the company's tracker is \"jira\" (`tracker.backend`)",
	}}
	s := opsmcp.New(opsmcp.Options{Surface: func() opsmcp.Surface {
		mu.Lock()
		defer mu.Unlock()
		return now
	}})
	if s == nil {
		t.Fatal("a surface that serves nothing yet got no server, so no later " +
			"revision could ever be served")
	}
	post := func() int {
		body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize",` +
			`"params":{"protocolVersion":"2025-06-18","capabilities":{},` +
			`"clientInfo":{"name":"t","version":"1"}}}`)
		req := httptest.NewRequestWithContext(auth.WithOperator(t.Context(), "founder"),
			http.MethodPost, opsmcp.Path, body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if got := post(); got != http.StatusNotFound {
		t.Errorf("a surface serving nothing answered %d, want %d", got, http.StatusNotFound)
	}
	mu.Lock()
	now = opsmcp.Surface{Deps: builtin.OperatorDeps{Work: stubWork()}}
	mu.Unlock()
	if got := post(); got == http.StatusNotFound {
		t.Error("the tracker is served now and the route still answers 404")
	}
}

// THE TRACKER AND THE KNOWLEDGE BASE ARE SEPARATE GRANTS. A company can run
// the native tracker on Confluence, or the native wiki on Jira, and a surface
// that offered both halves whenever it had one would hand an assistant tools
// that fail at the call.
func TestEachHalfIsOfferedOnItsOwn(t *testing.T) {
	t.Parallel()
	only := opsmcp.New(fixed(builtin.OperatorDeps{Work: stubWork()}))
	if only == nil {
		t.Fatal("a company with only the native tracker got no surface")
	}
	names := only.Tools()
	if !slices.Contains(names, "create_work_item") {
		t.Errorf("the tracker half serves %v, without create_work_item", names)
	}
	for _, name := range names {
		if strings.Contains(name, "page") {
			t.Errorf("a company with no native knowledge base was offered %q", name)
		}
	}
}

// A WRITE IS ATTRIBUTED TO THE CREDENTIAL, never to a seat and never to a
// name the caller chose.
//
// The alternative — letting the caller name a seat to act as — was rejected
// because it lets anybody holding the token write as anybody, and a tracker
// whose author field is chosen by the writer is not an audit trail.
func TestAnOperatorWriteCarriesTheTokensOwnLabel(t *testing.T) {
	t.Parallel()
	ctx := auth.WithOperator(t.Context(), "ops-bot")

	actor, err := opsmcp.WorkActor(ctx, nil)
	if err != nil {
		t.Fatalf("WorkActor: %v", err)
	}
	if actor.Kind != tracker.AuthorOperator {
		t.Errorf("an operator write is attributed as %q", actor.Kind)
	}
	if actor.OperatorID != "ops-bot" {
		t.Errorf("the record names the operator %q", actor.OperatorID)
	}
	// THE AUTHOR IS THE TOKEN'S OWN NAME, and the KIND is what says it is
	// not a seat. An empty author is the one thing this surface must never
	// record — a history row nobody can attribute — so the discriminator
	// is the kind, which every renderer and every recipient rule already
	// reads, rather than the emptiness of a string.
	if actor.Handle != "ops-bot" {
		t.Errorf("an operator write is authored by %q, want the token's name",
			actor.Handle)
	}

	// AND THE KNOWLEDGE BASE RECORDS THE SAME OPERATOR THE SAME WAY, which
	// is the claim rather than the coincidence: the two histories are read
	// TOGETHER on the audit screen, and one person under two names there is
	// two people to whoever is reading it.
	//
	// It was not. The handle was left empty here, and `pages.Actor.Name`
	// falls back to `"operator:" + OperatorID` for an actor without one — so
	// a founder's work commit said `founder` and their page commit said
	// `operator:founder`, three rows apart in one feed, and a reader
	// filtering on a name matched half of what they did. The kind is already
	// its own column on both rows, so the prefix was a second encoding of a
	// fact the row carries.
	page, err := opsmcp.PageActor(ctx, nil)
	if err != nil {
		t.Fatalf("PageActor: %v", err)
	}
	if page.Kind != pages.AuthorOperator || page.OperatorID != "ops-bot" {
		t.Errorf("a page write is attributed as %+v", page)
	}
	if page.Handle != actor.Handle {
		t.Errorf("one operator is recorded as %q by the tracker and %q by the "+
			"knowledge base", actor.Handle, page.Handle)
	}
	// THROUGH `Name`, which is what actually lands in the history row: the
	// field agreeing is the mechanism, and the rendered name is the property.
	if got := page.Name(); got != actor.Handle {
		t.Errorf("a page history row records the author as %q where the tracker "+
			"records %q — the audit feed reads both and shows one person twice",
			got, actor.Handle)
	}
}

// A REQUEST WITH NO OPERATOR IS REFUSED, not written as nobody. This surface
// writes to the company, and a write with no writer is the one thing it must
// never record — so the failure is at the actor rather than deeper, where it
// would already have landed.
func TestAWriteWithNoOperatorIsRefused(t *testing.T) {
	t.Parallel()
	for name, ctx := range map[string]context.Context{
		"no operator on the context": context.Background(),
		"an empty operator id":       auth.WithOperator(context.Background(), ""),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := opsmcp.WorkActor(ctx, nil); err == nil {
				t.Error("a write with no operator was attributed rather than refused")
			}
			if _, err := opsmcp.PageActor(ctx, nil); err == nil {
				t.Error("a page write with no operator was attributed rather than refused")
			}
		})
	}
}

// THE SURFACE IS ALWAYS GUARDED, and it is the auth package that says so.
// Mounting it under /mcp/ — which is exempt wholesale so a sandbox box with
// no API token can reach its seat's tools — would have put a writable company
// surface behind no credential at all.
func TestTheOperatorSurfaceIsNeverAnonymous(t *testing.T) {
	t.Parallel()
	if !auth.AlwaysGuarded(opsmcp.Path) {
		t.Fatalf("%s is not on the always-guarded list, so allow_anonymous_read "+
			"opens a surface that files work", opsmcp.Path)
	}
	if strings.HasPrefix(opsmcp.Path, "/mcp/") {
		t.Fatalf("%s is under the sandbox bridge's exempt prefix", opsmcp.Path)
	}
}

// EVERY TOOL AN OPERATOR IS OFFERED IS ONE A SEAT HAS. Not a subset check for
// tidiness: a name here that no seat tool answers would be a second
// implementation, which is what this whole seam exists to avoid.
func TestTheOperatorCatalogueIsDrawnFromTheSeatOne(t *testing.T) {
	t.Parallel()
	s := opsmcp.New(fixed(builtin.OperatorDeps{Work: stubWork(), Pages: stubPages()}))
	if s == nil {
		t.Fatal("a company on both native backends got no surface")
	}
	seat := append(builtin.WorkWrites(), builtin.PageWrites()...)
	for _, name := range seat {
		if !slices.Contains(s.Tools(), name) {
			t.Errorf("a seat can call %q and an operator cannot", name)
		}
	}
	// AND THE TURN-ONLY TOOLS ARE ABSENT. A diary belongs to a seat, a
	// skill is loaded into a phase, and a colleague ask is answered by
	// waking a seat — there is nobody here for any of the three.
	for _, name := range []string{"reflect_and_persist", "use_skill", "a2a_ask", "run_sandbox"} {
		if slices.Contains(s.Tools(), name) {
			t.Errorf("the operator surface offers %q, which only means something inside a turn", name)
		}
	}
}

// The tracker halves this surface needs to EXIST. What is under test here is
// which tools are offered and who a write is attributed to — neither of which
// reaches a store — so the stubs answer the shapes and nothing else.
type stubWorkReader struct{}

func (stubWorkReader) Tasks(context.Context, tracker.Query, time.Time) (tracker.Answer, error) {
	return tracker.Answer{}, nil
}

func (stubWorkReader) Task(context.Context, string, tracker.DetailWants,
	statelog.Freshness) (tracker.TaskDetail, error) {

	return tracker.TaskDetail{}, tracker.ErrNoTask
}

func (stubWorkReader) Views(context.Context, tracker.ViewQuery) (tracker.ViewListing, error) {
	return tracker.ViewListing{}, nil
}

func (stubWorkReader) ExpandedQuery(_ context.Context, params map[string]any,
	_ tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {

	return tracker.ParseQuery(tracker.MapParams(params), now, loc)
}

func (stubWorkReader) Goals(context.Context, tracker.GoalQuery) (tracker.GoalListing, error) {
	return tracker.GoalListing{}, nil
}

func (stubWorkReader) Catalogue(context.Context, tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
	return tracker.CatalogueAnswer{}, nil
}

func (stubWorkReader) Person(context.Context, tracker.PersonQuery, time.Time) (tracker.PersonState, error) {
	return tracker.PersonState{}, nil
}

func (stubWorkReader) Thread(context.Context, tracker.ThreadQuery,
	statelog.Freshness) (tracker.ResolvedThread, error) {
	return tracker.ResolvedThread{}, nil
}

type stubWorkWriterT struct{}

func stubWorkWriter(builtin.Actor) builtin.WorkWriter { return stubWorkWriterT{} }

// stubWorkMerger is the same stub in its second shape, for the fold — which is
// a SEQUENCE and therefore its own seam. See builtin.WorkMerger.
func stubWorkMerger(builtin.Actor) builtin.WorkMerger { return stubWorkWriterT{} }

func (stubWorkWriterT) MergeDuplicates(context.Context, string, string, string,
	bool, *tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

func (stubWorkWriterT) CreateTask(context.Context, string, tracker.Task,
	*tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

func (stubWorkWriterT) UpdateTask(context.Context, string, string, string, uint64,
	tracker.TaskPatch, tracker.ChangeKind, *tracker.Notify) (tracker.WriteResult, error) {

	return tracker.WriteResult{}, nil
}

type stubPageReader struct{}

func (stubPageReader) List(context.Context, pages.Filter,
	statelog.Freshness) (pages.Listing, error) {
	return pages.Listing{}, nil
}

func (stubPageReader) Get(context.Context, string,
	statelog.Freshness) (pages.Detail, error) {
	return pages.Detail{}, pages.ErrNotFound
}

// stubPageWriter is the knowledge base's write surface as this catalogue
// check needs it: present, so the surface is built, and never called.
type stubPageWriter struct{}

func (stubPageWriter) Create(context.Context, pages.Actor, pages.NewPage) (pages.Written, error) {
	return pages.Written{}, nil
}

func (stubPageWriter) SavePage(context.Context, pages.Actor, string, pages.Save) (pages.Written, error) {
	return pages.Written{}, nil
}

func (stubPageWriter) Rename(context.Context, pages.Actor, string, string, bool) (pages.Written, error) {
	return pages.Written{}, nil
}

func (stubPageWriter) Comment(context.Context, pages.Actor, string, pages.NewComment) (pages.Comment, pages.Written, error) {
	return pages.Comment{}, pages.Written{}, nil
}

func (stubPageWriter) EditComment(context.Context, pages.Actor, string, string, string) (pages.Comment, pages.Written, error) {
	return pages.Comment{}, pages.Written{}, nil
}

// TestEveryToolAnOperatorIsOfferedCarriesItsHints is the finding. This surface
// published a name, a description and a schema and NOTHING else, so an
// operator's own AI assistant — the premise of the whole endpoint — saw
// `search_work_items` and `remove_work_item` as identically unannotated. A
// client that asks before a destructive call had nothing to ask on, and a
// client that skips the prompt for a read prompted on every one.
func TestEveryToolAnOperatorIsOfferedCarriesItsHints(t *testing.T) {
	t.Parallel()
	s := opsmcp.New(fixed(builtin.OperatorDeps{Work: stubWork(), Pages: stubPages()}))
	if s == nil {
		t.Fatal("a company on both native backends got no surface")
	}
	for _, name := range s.Tools() {
		if got := s.Annotations(name); got == (tools.Annotations{}) {
			t.Errorf("%q is advertised with no hints at all — a client "+
				"cannot tell it from an irreversible write", name)
		}
	}

	// AND THE TWO ENDS OF THE RANGE ARE WHAT THEY CLAIM, or the check
	// above would pass on a surface that annotated everything the same.
	if got := s.Annotations(tracker.SearchWorkItemsTool); !crewletmcp.ReadOnlyProven(got) {
		t.Errorf("the ranked search advertises %+v, which is not a proven "+
			"read", got)
	}
	if got := s.Annotations(tracker.MergeWorkItemTool); got.Destructive != crewletmcp.Yes {
		t.Errorf("a fold advertises %+v — it closes somebody's item on every "+
			"board and moves its subtasks, which is what the flag asks", got)
	}
}

// A HALF IS A CLOSED SET, and the three a surface names are the three it serves.
func TestEveryHalfIsValidAndNothingElseIs(t *testing.T) {
	t.Parallel()
	for _, half := range []opsmcp.Half{opsmcp.Work, opsmcp.Pages, opsmcp.Knowledge} {
		if !half.Valid() {
			t.Errorf("%q is a half this surface serves and reports itself invalid", half)
		}
	}
	for _, half := range []opsmcp.Half{"", "tracker", "Work"} {
		if half.Valid() {
			t.Errorf("%q reports itself a half", half)
		}
	}
}

// THE CATALOGUE FOLLOWS THE CURRENT REVISION, and a call to a tool whose half
// has closed since a session listed it is REFUSED NAMING THE SETTING. The
// session here is one client for the whole case, exactly as an operator's
// assistant holds one across an apply: a surface decided when the session
// opened would go on filing work into a tracker the company has left.
func TestTheCatalogueFollowsTheCurrentRevision(t *testing.T) {
	t.Parallel()
	const moved = "the company's tracker is \"jira\" (`tracker.backend`), not the engine's own"
	var mu sync.Mutex
	now := opsmcp.Surface{Deps: builtin.OperatorDeps{Work: stubWork(), Pages: stubPages()}}
	s := opsmcp.New(opsmcp.Options{Surface: func() opsmcp.Surface {
		mu.Lock()
		defer mu.Unlock()
		return now
	}})
	// ONE HANDLER, mounted once as the app mounts it: the sessions live in
	// it, so a handler per request would lose the session it just opened.
	handler := s.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// THE APP'S GUARD, reduced to what it leaves on the context.
		handler.ServeHTTP(w, r.WithContext(auth.WithOperator(r.Context(), "founder")))
	}))
	t.Cleanup(server.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "assistant", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// ITS OWN POOL, for [httpxtest]'s reason: left unset the SDK reaches for
	// http.DefaultClient, which every httptest.Server.Close in this binary
	// sweeps.
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: server.URL, HTTPClient: httpxtest.Pool(t),
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	listed := func() []string {
		t.Helper()
		res, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		return names
	}
	if names := listed(); !slices.Contains(names, builtin.CreateWorkItemTool) ||
		!slices.Contains(names, builtin.WritePageTool) {

		t.Fatalf("both halves are served and the catalogue lists %v", names)
	}

	mu.Lock()
	now = opsmcp.Surface{
		Deps:   builtin.OperatorDeps{Pages: stubPages()},
		Closed: map[opsmcp.Half]string{opsmcp.Work: moved},
	}
	mu.Unlock()
	names := listed()
	if slices.Contains(names, builtin.CreateWorkItemTool) {
		t.Errorf("the tracker half closed and the catalogue still lists %s", builtin.CreateWorkItemTool)
	}
	if !slices.Contains(names, builtin.WritePageTool) {
		t.Errorf("the knowledge base stayed and the catalogue dropped %s", builtin.WritePageTool)
	}

	// THE STALE CALL, named as a client that listed before the move would
	// send it: refused as a tool result naming the setting, not a protocol
	// error saying the tool never existed.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: builtin.CreateWorkItemTool, Arguments: map[string]any{"title": "x"},
	})
	if err != nil {
		t.Fatalf("a call to a closed tool failed as a protocol error: %v", err)
	}
	if !res.IsError || !strings.Contains(text(res), "tracker.backend") {
		t.Errorf("a call to a closed tool answered %q (error %v), want a refusal "+
			"naming tracker.backend", text(res), res.IsError)
	}
	// AND A NAME THIS SURFACE NEVER SERVED IS STILL THE SDK'S UNKNOWN TOOL:
	// the refusal is for what changed, not a blanket over every typo.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "no_such_tool"}); err == nil {
		t.Error("a tool this surface never served was answered rather than refused as unknown")
	}

	mu.Lock()
	now = opsmcp.Surface{Deps: builtin.OperatorDeps{Work: stubWork(), Pages: stubPages()}}
	mu.Unlock()
	if names := listed(); !slices.Contains(names, builtin.CreateWorkItemTool) {
		t.Errorf("the tracker half is served again and the catalogue lists %v", names)
	}
}

// text is a tool result's text, joined.
func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
