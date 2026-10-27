package queries_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// stubWork and stubPages record what filter the surface built, which is the
// half a round trip through a real reader would hide: the questions here are
// almost entirely about turning a query string into a Filter, and a test that
// only checked the rows would pass with every filter dropped.
type stubWork struct {
	places        map[string]tracker.TurnPlace
	placesAsked   []string
	turns         tracker.TaskTurns
	turnsRef      string
	turnsCursor   string
	turnsLimit    int
	inbox         tracker.InboxAnswer
	inboxQuery    tracker.InboxQuery
	routing       tracker.RoutingAnswer
	routingQuery  tracker.RoutingQuery
	ranked        []tracker.Ranked
	searchText    string
	searchLimit   int
	searchMode    knowledge.Mode
	searchOutcome knowledge.Outcome
	query         tracker.Query
	answer        tracker.Answer
	detail        tracker.TaskDetail
	views         tracker.ViewQuery
	every         tracker.EveryViewQuery
	listing       tracker.ViewListing
	personQuery   tracker.PersonQuery
	person        tracker.PersonState

	projectQuery  tracker.ProjectQuery
	projects      tracker.ProjectListing
	detailQuery   tracker.ProjectDetailQuery
	project       tracker.ProjectDetail
	workloadQuery tracker.WorkloadQuery
	workload      tracker.WorkloadAnswer

	// The instant and the clock each day-cutting read was handed, so a
	// test can hold a surface to the company's own midnight.
	expandNow, workloadNow, myWorkNow    time.Time
	expandZone, workloadZone, myWorkZone *time.Location

	expandViewer  tracker.Viewer
	activityQuery tracker.ActivityQuery
	activity      tracker.ActivityAnswer
	myWorkQuery   tracker.MyWorkQuery
	myWork        tracker.MyWork

	flowQuery      tracker.FlowQuery
	flow           tracker.FlowAnswer
	feedQuery      tracker.FeedQuery
	feed           tracker.FeedPage
	decisionsQuery tracker.DecisionsQuery
	decisions      tracker.DecisionsAnswer

	catalogueQuery tracker.CatalogueQuery
	taskLevel      statelog.ReadLevel
	taskFresh      statelog.Freshness
	taskWants      tracker.DetailWants

	// expandErr is what expanding a view's parameters fails with.
	expandErr error

	// asks is the thread the asked probe reads: the comment an OPEN ask
	// addressed to each author's handle would be answered as, absent for
	// one nobody asked; threadErr is what that read fails with, and
	// threadAsked every query it was handed.
	asks        map[string]string
	threadErr   error
	threadAsked []tracker.ThreadQuery

	err error
}

// Thread answers the asked probe from [stubWork.asks], as the tracker's own
// inference would: the one open ask addressed to the author, or none.
func (s *stubWork) Thread(_ context.Context, q tracker.ThreadQuery,
	_ statelog.Freshness) (tracker.ResolvedThread, error) {

	s.threadAsked = append(s.threadAsked, q)
	if s.threadErr != nil {
		return tracker.ResolvedThread{}, s.threadErr
	}
	return tracker.ResolvedThread{Answers: s.asks[q.Author]}, nil
}

func (s *stubWork) Projects(_ context.Context, q tracker.ProjectQuery) (
	tracker.ProjectListing, error) {

	s.projectQuery = q
	return s.projects, s.err
}

func (s *stubWork) Project(_ context.Context, q tracker.ProjectDetailQuery) (
	tracker.ProjectDetail, error) {

	s.detailQuery = q
	return s.project, s.err
}

func (s *stubWork) Workload(_ context.Context, q tracker.WorkloadQuery,
	now time.Time, loc *time.Location) (tracker.WorkloadAnswer, error) {

	s.workloadQuery = q
	s.workloadNow, s.workloadZone = now, loc
	return s.workload, s.err
}

func (s *stubWork) Activity(_ context.Context, q tracker.ActivityQuery,
	_ time.Time) (tracker.ActivityAnswer, error) {

	s.activityQuery = q
	return s.activity, s.err
}

func (s *stubWork) MyWork(_ context.Context, q tracker.MyWorkQuery,
	now time.Time, loc *time.Location) (tracker.MyWork, error) {

	s.myWorkQuery = q
	s.myWorkNow, s.myWorkZone = now, loc
	return s.myWork, s.err
}

func (s *stubWork) EveryView(_ context.Context, q tracker.EveryViewQuery) (tracker.ViewListing, error) {
	s.every = q
	return s.listing, s.err
}

func (s *stubWork) Views(_ context.Context, q tracker.ViewQuery) (tracker.ViewListing, error) {
	s.views = q
	return s.listing, s.err
}

func (s *stubWork) ExpandedQuery(_ context.Context, params map[string]any,
	viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {

	// RECORDED, because the viewer is the one input to this call that the
	// parsed query does not carry: it is a property of the surface rather
	// than a filter, so a handler that dropped it would still return rows.
	s.expandViewer = viewer
	s.expandNow, s.expandZone = now, loc
	if s.expandErr != nil {
		return tracker.Query{}, s.expandErr
	}
	return tracker.ParseQuery(tracker.MapParams(params), now, loc)
}

func (s *stubWork) Catalogue(_ context.Context,
	q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {

	s.catalogueQuery = q
	return tracker.CatalogueAnswer{}, nil
}

func (s *stubWork) Person(_ context.Context, q tracker.PersonQuery, _ time.Time) (tracker.PersonState, error) {
	s.personQuery = q
	return s.person, s.err
}

func (s *stubWork) Inbox(_ context.Context, q tracker.InboxQuery, _ time.Time) (tracker.InboxAnswer, error) {
	s.inboxQuery = q
	return s.inbox, s.err
}

func (s *stubWork) Flow(_ context.Context, q tracker.FlowQuery, _ time.Time,
	_ *time.Location) (tracker.FlowAnswer, error) {

	s.flowQuery = q
	return s.flow, s.err
}

func (s *stubWork) CompanyFeed(_ context.Context, q tracker.FeedQuery) (tracker.FeedPage, error) {
	s.feedQuery = q
	// AT THE LEVEL ASKED, as the reader answers, so a case can tell which
	// half of a composed feed set the answer's level.
	page := s.feed
	if page.Level == "" {
		page.Level = q.Level
	}
	return page, s.err
}

func (s *stubWork) Decisions(_ context.Context, q tracker.DecisionsQuery, _ time.Time,
	_ *time.Location) (tracker.DecisionsAnswer, error) {

	s.decisionsQuery = q
	return s.decisions, s.err
}

func (s *stubWork) TurnPlaces(_ context.Context, runs []string,
	_ statelog.Freshness) (map[string]tracker.TurnPlace, error) {

	s.placesAsked = runs
	return s.places, s.err
}

func (s *stubWork) Routing(_ context.Context, q tracker.RoutingQuery, _ time.Time) (
	tracker.RoutingAnswer, error) {

	s.routingQuery = q
	return s.routing, s.err
}

// Search is the stub's half of the SEPARATE search seam — see
// [queries.WorkSearcher]. It is on this type for the harness's convenience
// only; the surface takes the two independently, and a case that wants a node
// with a board and no index leaves `WorkSearch` nil.
func (s *stubWork) Search(_ context.Context, q tracker.SearchQuery) (tracker.SearchAnswer, error) {
	s.searchText, s.searchLimit, s.searchMode = q.Text, q.Limit, q.Mode
	return tracker.SearchAnswer{Hits: s.ranked, Outcome: s.searchOutcome}, s.err
}

func (s *stubWork) TurnsOf(_ context.Context, ref, cursor string, limit int,
	fresh statelog.Freshness) (tracker.TaskTurns, error) {

	s.turnsRef, s.turnsCursor, s.turnsLimit, s.taskFresh = ref, cursor, limit, fresh
	return s.turns, s.err
}

func (s *stubWork) Tasks(_ context.Context, q tracker.Query, _ time.Time) (tracker.Answer, error) {
	s.query = q
	return s.answer, s.err
}

func (s *stubWork) Task(_ context.Context, _ string, want tracker.DetailWants,
	fresh statelog.Freshness) (tracker.TaskDetail, error) {

	s.taskLevel, s.taskFresh = fresh.Level, fresh
	s.taskWants = want
	return s.detail, s.err
}

type stubPages struct {
	filter pages.Filter
	list   []pages.Summary
	total  int
	after  string
	err    error

	// level is what the surface asked for, so a route that stopped naming
	// one is visible: an unset level is what made every page read on this
	// surface a label rather than a guarantee.
	level statelog.ReadLevel

	// fresh is the whole ask, so a route that carried the level and
	// dropped the bounds or the floor beside it is visible too.
	fresh statelog.Freshness

	// activity and revision are what the two newest reads were asked, so a
	// case can prove a filter reached the reader rather than only that
	// rows came back.
	activity     pages.PageActivityQuery
	changes      []pages.PageChange
	revision     pages.Revision
	revisionHeld bool
	askedPage    string
	askedVersion int

	// detail is what Get answers.
	detail pages.Detail
}

func (s *stubPages) List(_ context.Context, f pages.Filter,
	fresh statelog.Freshness,
) (pages.Listing, error) {
	s.filter, s.level, s.fresh = f, fresh.Level, fresh
	return pages.Listing{
		Pages: s.list, Total: s.total, After: s.after, Level: fresh.Level, Complete: true,
	}, s.err
}

func (s *stubPages) Get(_ context.Context, _ string,
	fresh statelog.Freshness,
) (pages.Detail, error) {
	s.level, s.fresh = fresh.Level, fresh
	return s.detail, s.err
}

func (s *stubPages) Containers(_ context.Context,
	fresh statelog.Freshness,
) ([]pages.ContainerListing, error) {
	s.level, s.fresh = fresh.Level, fresh
	return nil, s.err
}

func (s *stubPages) Activity(_ context.Context, q pages.PageActivityQuery) (
	pages.PageActivity, error) {

	s.activity = q
	s.level, s.fresh = q.Freshness.Level, q.Freshness
	return pages.PageActivity{
		Changes: s.changes, Level: q.Freshness.Level, Complete: true,
	}, s.err
}

func (s *stubPages) Revision(_ context.Context, pageID string, version int,
	fresh statelog.Freshness,
) (pages.Revision, bool, error) {
	s.askedPage, s.askedVersion = pageID, version
	s.level, s.fresh = fresh.Level, fresh
	return s.revision, s.revisionHeld, s.err
}

// personalQuestions are the five scoped by the caller's own record or seat —
// see Sources.recordHandle and Sources.trailSeat. They refuse a caller who
// names somebody else's record without the relation or grant that reaches it,
// so a sweep that walks every native question has to present a credential for
// these five. Named once rather than per sweep: the set grew from one to five,
// and each sweep that spelled it as `== "work_my_work"` silently stopped
// covering the rest.
var personalQuestions = map[string]bool{
	"work_my_work":  true,
	"work_person":   true,
	"work_inbox":    true,
	"conversations": true,
	"decisions":     true,
}

// askNative runs one question against a registry built from these sources,
// returning the error rather than failing on it: every case here is about a
// refusal, which the shared `ask` helper turns into a Fatalf.
// askNative asks as somebody who USES THE COMPANY: a credential holding every
// app and not admin.
//
// It is that rather than a credential holding everything, deliberately: these
// cases are about the board and the wiki, and asking them with the narrowest
// credential that may use both is what keeps a question quietly becoming
// admin's from going unnoticed here.
func askNative(t *testing.T, s queries.Sources, what string, params map[string]any) (any, error) {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	return r.Answer(asGrants(t, "", iam.Apps...), what, params)
}

// askTranscripts is askNative for the questions that serve what a turn SAID:
// prompts, tool arguments, diary entries, a seat's notes on who it has worked
// with. Every seat's at once is the company-wide trail, which is admin's —
// somebody who uses the company's apps must not reach it (ADR-0033).
func askTranscripts(t *testing.T, s queries.Sources, what string,
	params map[string]any) (any, error) {

	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	return r.Answer(asGrants(t, "", iam.GrantAdmin), what, params)
}

// askAsOperator is askNative for the questions that describe the DEPLOYMENT
// rather than the company's work. Separate rather than a parameter on the one
// above, so no case here can hand itself a wider credential by accident.
func askAsOperator(t *testing.T, s queries.Sources, what string,
	params map[string]any) (any, error) {

	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	return r.Answer(asGrants(t, "ops-1", iam.AllGrants...), what, params)
}

// asSeat is a context carrying a caller BOUND TO A SEAT, which is what every
// personal answer is scoped by now — the viewer is the principal's own seat
// and no longer a parameter anybody types.
func asSeat(t *testing.T, handle string, grants ...iam.Grant) context.Context {
	t.Helper()
	return iam.WithPrincipal(t.Context(), iam.Principal{
		ID: uuid.New(), Login: handle, Kind: iam.KindPerson, Seat: handle,
		Stage: iam.StageActive, Grants: grants,
	})
}

// askAsSeat is [askNative] for a caller who holds one.
func askAsSeat(t *testing.T, s queries.Sources, handle, what string,
	params map[string]any) (any, error) {

	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	return r.Answer(asSeat(t, handle, iam.AllGrants...), what, params)
}

// askHolding is [askAsSeat] for a case about AUTHORITY, which states the
// grants rather than taking every one: [iam.AllGrants] carries admin, which
// every personal class admits before it asks the chart — so a case using it
// against a chart that reports no relation is allowed anyway and asserts
// nothing.
func askHolding(t *testing.T, s queries.Sources, handle, what string,
	params map[string]any, grants ...iam.Grant) (any, error) {

	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	return r.Answer(asSeat(t, handle, grants...), what, params)
}

// asGrants is a context carrying a credential with exactly these grants.
func asGrants(t *testing.T, id string, grants ...iam.Grant) context.Context {
	t.Helper()
	if id == "" {
		id = "reader"
	}
	return iam.WithPrincipal(t.Context(), iam.Principal{
		ID: uuid.New(), Login: "token:" + id, Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: grants,
	})
}

// A QUESTION WITH NO SOURCE IS UNREGISTERED, not registered-and-empty. A
// company on Jira has no native record for this node to have a copy of, and a
// board answering "no work" would say the opposite of what is true.
func TestTheNativeQuestionsAreAbsentWithoutTheirReaders(t *testing.T) {
	for _, what := range []string{"work_items", "work_item", "pages", "page", "containers"} {
		if _, err := askNative(t, queries.Sources{}, what, nil); !errors.Is(err, queries.ErrUnknown) {
			t.Errorf("%s on a node with no native backend answered %v, want unknown", what, err)
		}
	}
}

// A COPY THAT CANNOT ANSWER SAYS SO. Flattening a failure to an empty list
// would tell a person the company has no work — an answer they act on, by
// filing the duplicate or concluding the migration failed.
//
// THE HYDRATION SENTINEL IS GONE with the projection that raised it: both
// native backends are state-log domains now and both say how far behind they
// are through the coverage on the answer itself, which is a POSITION rather
// than a boolean. What still has to hold is that a read which FAILED is not
// rendered as a read that found nothing.
func TestAReadThatFailedIsNotRenderedAsEmpty(t *testing.T) {
	sentinel := errors.New("the store could not be reached")
	w := &stubWork{err: sentinel}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_items", nil); !errors.Is(err, sentinel) {
		t.Errorf("a failed board read answered %v, want the failure", err)
	}
	p := &stubPages{err: sentinel}
	if _, err := askNative(t, queries.Sources{Pages: p}, "pages", nil); !errors.Is(err, sentinel) {
		t.Errorf("a failed page listing answered %v, want the failure", err)
	}
}

// A RECORD THAT IS NOT THERE IS NOT A FAILURE. A mistyped item key must read
// to an operator as a dead link, not as the server being broken.
func TestAMissingRecordIsNotFound(t *testing.T) {
	w := &stubWork{err: tracker.ErrNoTask}
	_, err := askNative(t, queries.Sources{Work: w}, "work_item", map[string]any{"id": "ENG-999"})
	if !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("a missing item answered %v, want not-found", err)
	}
	p := &stubPages{err: pages.ErrNotFound}
	_, err = askNative(t, queries.Sources{Pages: p}, "page", map[string]any{"id": "nope"})
	if !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("a missing page answered %v, want not-found", err)
	}
}

// THE ITEM QUERY ASKS FOR EVERY PART, the custom fields included.
//
// A detail read without them rendered a task filed with a severity as one that
// carried none — confidently, in a properties panel, beside a board that had
// just filtered on that very field. The four are asked for together because
// one screen draws all four and a second read for the fields would be a second
// answer that can disagree with the first.
func TestTheItemQueryAsksForEveryPart(t *testing.T) {
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_item",
		map[string]any{"id": "ENG-1"}); err != nil {
		t.Fatalf("work_item: %v", err)
	}
	got := w.taskWants
	// THE CLOCK IS CHECKED ON ITS OWN, below: a pointer compares by address.
	got.Clock = nil
	want := tracker.DetailWants{Comments: true, History: true, Links: true, Fields: true}
	if got != want {
		t.Errorf("work_item asked the reader for %+v, want %+v", got, want)
	}
}

// AND IT ASKS ON THE COMPANY'S CLOCK, so the rail's due date stands on the
// calendar every board's overdue mark was cut on (ADR-0018). Without a clock
// the answer carries no standing and the task page fell back to the reader's
// own "1d ago" beside a board drawing the same task two days late.
func TestTheItemQueryStandsTheDueDateOnTheCompanyClock(t *testing.T) {
	losAngeles, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2031, time.April, 17, 6, 30, 0, 0, time.UTC)
	w := &stubWork{}
	src := queries.Sources{
		Work:    w,
		Now:     func() time.Time { return now },
		Company: companySource(t, &config.Company{Timezone: "America/Los_Angeles"}),
	}
	if _, err := askNative(t, src, "work_item", map[string]any{"id": "ENG-1"}); err != nil {
		t.Fatalf("work_item: %v", err)
	}
	clock := w.taskWants.Clock
	if clock == nil {
		t.Fatal("work_item asked the reader with no clock, so no due date can be stood")
	}
	if !clock.Now.Equal(now) || clock.Zone.String() != losAngeles.String() {
		t.Fatalf("work_item asked on %v in %v, want %v in %v", clock.Now, clock.Zone, now, losAngeles)
	}
}

// AND IT ASKS WITH THE CHART, so the properties panel reads a team's NAME
// where the row holds its key.
//
// What a task's unit fields hold is the unit's KEY as it was when the row was
// written — an address a person typed, short, and possibly one the chart has
// since dropped. Without the chart this answer carried that key alone, and an item
// page said `eng` beside a board column that said Engineering. An unresolved
// reference is also a FINDING — the team has left the chart — so a surface
// that holds a chart and does not pass it reports every task as orphaned.
func TestTheItemQueryAsksWithTheChart(t *testing.T) {
	w := &stubWork{}
	cfg := company(t)
	if _, err := askNative(t, queries.Sources{
		Work: w, Company: companySource(t, cfg),
	}, "work_item", map[string]any{"id": "ENG-1"}); err != nil {
		t.Fatalf("work_item: %v", err)
	}
	if w.taskWants.Units == nil {
		t.Error("work_item read the item with no chart, so its unit fields " +
			"render as a key nobody reads and as a team the chart has lost")
	}
}

// A TASK'S TURNS ARE ASKED AT THE CALLER'S CURSOR AND PAGE, HELD TO THE CEILING.
//
// The page walks back through a task's turns by the cursor the previous page
// handed out, and a page bigger than the tracker's own ceiling is the
// ceiling rather than a refusal or an unbounded read. A task that does not
// exist is `not_found`, never an empty list a screen would draw as "no agent
// has worked on this".
func TestTheTurnsQuestionFollowsTheCursorAtTheCallersPage(t *testing.T) {
	w := &stubWork{turns: tracker.TaskTurns{
		Task: "t-1", Key: "ENG-1",
		Turns: []tracker.TaskTurn{{TurnID: "run-2", Ordinal: 2, Tokens: 900}},
		Next:  "41",
	}}
	got, err := askNative(t, queries.Sources{Work: w}, "work_item_turns",
		map[string]any{"id": "ENG-1", "cursor": "88", "limit": 500})
	if err != nil {
		t.Fatalf("work_item_turns: %v", err)
	}
	if w.turnsRef != "ENG-1" || w.turnsCursor != "88" || w.turnsLimit != tracker.MaxTaskTurns {
		t.Errorf("asked the reader for (%q, %q, %d), want (ENG-1, 88, %d)",
			w.turnsRef, w.turnsCursor, w.turnsLimit, tracker.MaxTaskTurns)
	}
	page, ok := got.(tracker.TaskTurns)
	if !ok || page.Next != "41" || len(page.Turns) != 1 || page.Turns[0].Ordinal != 2 {
		t.Errorf("answered %#v, want the reader's page with its cursor", got)
	}

	if _, err := askNative(t, queries.Sources{Work: w}, "work_item_turns",
		map[string]any{}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("no id answered %v, want bad_params", err)
	}
	gone := &stubWork{err: tracker.ErrNoTask}
	if _, err := askNative(t, queries.Sources{Work: gone}, "work_item_turns",
		map[string]any{"id": "ENG-404"}); !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("a task that does not exist answered %v, want not_found", err)
	}
}

// THE THREAD QUESTION ASKS FOR THE THREAD ALONE, AT THE CALLER'S CURSOR.
//
// `work_item` returns the newest page and a cursor that nothing read, so every
// comment older than the twentieth was unreachable. This is the read that
// follows the cursor — and it asks for the comments and nothing else, because
// the pane that walks a thread draws none of the history, links or fields a
// detail assembles. The page size is the caller's, held to the tracker's own
// ceiling.
func TestTheThreadQuestionFollowsTheCursorAtTheCallersPage(t *testing.T) {
	w := &stubWork{detail: tracker.TaskDetail{
		Task:           tracker.Task{ID: "t-1", Key: "ENG-1"},
		Comments:       []tracker.Comment{{ID: "c-21", Body: "older"}},
		CommentsCursor: "1700000000:c-20",
	}}
	got, err := askNative(t, queries.Sources{Work: w}, "work_comments",
		map[string]any{"item": "ENG-1", "cursor": "1800000000:c-41", "limit": 500})
	if err != nil {
		t.Fatalf("work_comments: %v", err)
	}
	want := tracker.DetailWants{
		Comments: true, CommentCursor: "1800000000:c-41", CommentLimit: tracker.MaxCommentPage,
	}
	if w.taskWants != want {
		t.Errorf("work_comments asked the reader for %+v, want %+v", w.taskWants, want)
	}
	answer := got.(map[string]any)
	if answer["next_cursor"] != "1700000000:c-20" || answer["key"] != "ENG-1" {
		t.Errorf("the answer lost the next cursor or the key: %v", answer)
	}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_comments",
		map[string]any{}); err == nil {
		t.Error("a thread asked of no item was answered")
	}
	// AN EMPTY THREAD IS A LIST: an absent key reads like an answer that
	// failed to carry one.
	w.detail = tracker.TaskDetail{Task: tracker.Task{ID: "t-2", Key: "ENG-2"}}
	got, _ = askNative(t, queries.Sources{Work: w}, "work_comments",
		map[string]any{"item": "ENG-2"})
	if list, ok := got.(map[string]any)["comments"].([]tracker.Comment); !ok || list == nil {
		t.Errorf("an empty thread answered %v rather than an empty list", got)
	}
	if w.taskWants.CommentLimit != tracker.DetailComments {
		t.Errorf("a caller naming no page got %d, want the detail's own %d",
			w.taskWants.CommentLimit, tracker.DetailComments)
	}
}

// EVERY FILTER REACHES THE READER. A filter honoured on one transport and
// dropped on the other is the exact divergence this package exists to
// prevent, and a board silently ignoring `assignee` looks like a board with
// nothing assigned.
func TestABoardFilterReachesTheReader(t *testing.T) {
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_items", map[string]any{
		"container": "project:eng", "assignee": "swe", "tag": "urgent",
		"status": "todo,in_progress", "q": "deploy", "limit": 10,
	}); err != nil {
		t.Fatalf("work_items: %v", err)
	}
	got := w.query
	// UPPERCASED, because a project key is compared upper everywhere else
	// and a board that only matched the case somebody typed would answer
	// empty for the same project spelled two ways.
	if got.Scope.Project != "ENG" {
		t.Errorf("project reached the reader as %q, want it upper-cased",
			got.Scope.Project)
	}
	if len(got.Assignee) != 1 || got.Assignee[0] != "swe" {
		t.Errorf("assignee reached the reader as %v", got.Assignee)
	}
	if len(got.Tags.Tags) != 1 || got.Tags.Tags[0] != "urgent" || got.Text != "deploy" {
		t.Errorf("a filter was dropped on the way: %+v", got)
	}
	if len(got.Status) != 2 || got.Status[0] != tracker.StatusTodo {
		t.Errorf("statuses reached the reader as %v", got.Status)
	}
	if got.Limit != 10 {
		t.Errorf("paging reached the reader as limit=%d", got.Limit)
	}
}

// AN ABSENT SCOPE IS NEITHER THE WORKSPACE NOR A PROJECT, and the caller
// resolves it from its own surface. Defaulting an omitted container to the
// workspace would make the cheapest thing to type the most expensive query in
// the system — every seat's idle board scanning every project.
func TestAnAbsentContainerIsNeither(t *testing.T) {
	for name, tc := range map[string]struct {
		params    map[string]any
		workspace bool
		project   string
	}{
		"absent":    {params: nil},
		"workspace": {params: map[string]any{"container": "workspace"}, workspace: true},
		"a project": {params: map[string]any{"container": "project:ENG"}, project: "ENG"},
	} {
		t.Run(name, func(t *testing.T) {
			w := &stubWork{}
			if _, err := askNative(t, queries.Sources{Work: w}, "work_items", tc.params); err != nil {
				t.Fatalf("work_items: %v", err)
			}
			if w.query.Scope.Workspace != tc.workspace || w.query.Scope.Project != tc.project {
				t.Errorf("the scope reached the reader as %+v", w.query.Scope)
			}
		})
	}
}

// The same three states for `skills`, and all three are real: only the
// tool-skill pages, everything but them, and everything.
func TestSkillsIsThreeStated(t *testing.T) {
	p := &stubPages{}
	if _, err := askNative(t, queries.Sources{Pages: p}, "pages", nil); err != nil {
		t.Fatalf("pages: %v", err)
	}
	if p.filter.Skills != nil {
		t.Errorf("an absent `skills` reached the reader as %v", *p.filter.Skills)
	}
	if _, err := askNative(t, queries.Sources{Pages: p}, "pages", map[string]any{"skills": false}); err != nil {
		t.Fatalf("pages: %v", err)
	}
	if p.filter.Skills == nil || *p.filter.Skills {
		t.Error("skills=false reached the reader as absent or true")
	}
}

// A PAGE CONTAINER REACHES THE READER IN THE KNOWLEDGE BASE'S OWN SPELLING, on
// both reads that take one.
//
// The rows are keyed upper-case, and the READ'S DEFERRAL SCOPE is composed from
// the value as handed in — so a `container=eng` reaching the reader as typed
// asked whether a record held against `eng` was pending, which no writer ever
// files one under, and answered complete over a container a retained record
// was about. The activity read passed its container on untrimmed and
// un-upper-cased, and the list read kept a private copy of the rule.
func TestAPageContainerReachesTheReaderCanonical(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"eng", " Eng ", "ENG"} {
		p := &stubPages{}
		if _, err := askNative(t, queries.Sources{Pages: p}, "pages",
			map[string]any{"container": raw}); err != nil {
			t.Fatalf("pages: %v", err)
		}
		if p.filter.Container != "ENG" {
			t.Errorf("pages container=%q reached the reader as %q, want ENG",
				raw, p.filter.Container)
		}
		if _, err := askNative(t, queries.Sources{Pages: p}, "page_activity",
			map[string]any{"container": raw}); err != nil {
			t.Fatalf("page_activity: %v", err)
		}
		if p.activity.Container != "ENG" {
			t.Errorf("page_activity container=%q reached the reader as %q, "+
				"want ENG", raw, p.activity.Container)
		}
	}
}

// A TREE LEVEL IS ASKED FOR AND ANSWERED WHOLE: `roots` and `after` reach
// the reader, and the answer carries the listing's TOTAL and the next cursor
// beside the window — without them fifty pages and a container of four hundred
// were one answer.
func TestAPageListingCarriesItsTotalAndCursor(t *testing.T) {
	p := &stubPages{list: []pages.Summary{{ID: "p1", Title: "Runbooks"}}}
	p.total, p.after = 412, "next-window"
	got, err := askNative(t, queries.Sources{Pages: p}, "pages",
		map[string]any{"container": "ENG", "roots": true, "after": "cursor-1", "limit": 500})
	if err != nil {
		t.Fatalf("pages: %v", err)
	}
	if !p.filter.Roots || p.filter.After != "cursor-1" || p.filter.Limit != 500 {
		t.Errorf("the reader was asked %+v, want roots, after=cursor-1, limit 500", p.filter)
	}
	answer := got.(map[string]any)
	if answer["total"] != 412 || answer["after"] != "next-window" {
		t.Errorf("the answer carries total %v and after %v, want 412 and the cursor",
			answer["total"], answer["after"])
	}
	// ROOTS AND A PARENT ARE OPPOSITE QUESTIONS, refused rather than one
	// silently winning.
	_, err = askNative(t, queries.Sources{Pages: p}, "pages",
		map[string]any{"roots": true, "parent": "p1"})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("roots with a parent answered %v, want bad params", err)
	}
	// AND A CURSOR THE READER REFUSES IS THE CALLER'S MISTAKE.
	p.err = fmt.Errorf("wrapped: %w", pages.ErrBadCursor)
	_, err = askNative(t, queries.Sources{Pages: p}, "pages", map[string]any{"after": "junk"})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a refused cursor answered %v, want bad params", err)
	}
}

// A BAD ENUM IS REFUSED NAMING THE CLOSED SET, rather than silently matching
// nothing: a board that answered empty for `status=done` (which is not a
// status here) would send somebody looking for the missing items.
func TestAnUnknownStatusIsRefusedNamingTheSet(t *testing.T) {
	_, err := askNative(t, queries.Sources{Work: &stubWork{}}, "work_items",
		map[string]any{"status": "finished"})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("an unknown status answered %v, want bad params", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "finished") ||
		!strings.Contains(msg, "six") {
		t.Errorf("the refusal does not name the value and the set: %q", msg)
	}
}

// THE BOARD'S TOTAL IS A SEPARATE HINT, never len(items). A header reporting
// the page size as the project's size says "50 items" for every project with
// more than fifty — and an EXACT total over an unbounded set is the one query
// in this grammar that turns a poll into a scan, which is why it says hint.
func TestTheBoardCarriesItsOwnTotalAndReadLevel(t *testing.T) {
	w := &stubWork{answer: tracker.Answer{
		Rows:      []tracker.TaskRow{{ID: "i1", Key: "ENG-1"}},
		TotalHint: 42, Level: statelog.ReadStale, Complete: true,
	}}
	got, err := askNative(t, queries.Sources{Work: w}, "work_items", nil)
	if err != nil {
		t.Fatalf("work_items: %v", err)
	}
	payload, _ := got.(map[string]any)
	if payload["total_hint"] != 42 {
		t.Errorf("the board carried total_hint=%v with one row", payload["total_hint"])
	}
	// AND THE READ LEVEL RIDES WITH IT. A screen that could not say how
	// stale its answer is would render a lagging node identically to a
	// caught-up one — which is the one thing this framework's read levels
	// exist to make impossible.
	if payload["read_level"] != statelog.ReadStale {
		t.Errorf("the board carried read_level=%v", payload["read_level"])
	}
	if payload["complete"] != true {
		t.Errorf("the board carried complete=%v", payload["complete"])
	}
}

// A NODE THAT IS BEHIND ANSWERS "COME BACK", NOT "THE SERVER BROKE".
//
// This is a difference a client acts on: a 503 with a hint refreshes the
// screen in a few seconds, where a 500 tells it to give up on a screen that
// would have worked. The API reference has documented the 503 since the
// surface existed, and nothing produced it — every read refusal reached the
// caller as a plain failure once the tracker moved onto the log.
func TestAReadThisNodeCannotServeYetIsUnavailableRatherThanFailed(t *testing.T) {
	t.Parallel()
	work := &stubWork{err: &statelog.Refused{
		Code: statelog.RefuseBehind, Level: statelog.ReadSession,
		Detail: "this node is 40 000 records behind", RetryAfter: 12 * time.Second,
	}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Work: work})

	_, err := r.Answer(asGrants(t, "", iam.GrantTracker), "work_items", map[string]any{})
	if !errors.Is(err, queries.ErrUnavailable) {
		t.Fatalf("a node that is behind answered %v — a read refusal that "+
			"reaches a client as a plain failure is rendered as a broken "+
			"server on a screen that would have worked in a few seconds", err)
	}
	// AND THE HINT IS THE REFUSAL'S OWN, derived from how far behind this
	// node is over how fast it is draining, and it survives the wrapping:
	// the REST answer reads it through [statelog.RetryAfter]. A flat five
	// seconds is wrong in both directions on one fleet.
	if got := statelog.RetryAfter(err, 5*time.Second); got != 12*time.Second {
		t.Errorf("the retry hint is %s, want the refusal's own 12s", got)
	}
}

// A FLOOR ON ANOTHER LOG IS THE REQUEST'S MISTAKE, AND A REBUILT STREAM IS
// THIS NODE'S.
//
// The reader refuses a `min_position` naming another domain's log before it
// looks at anything about this node, because every node refuses it the same:
// the caller has to change what it asks, so it is [queries.ErrBadParams]. It
// used to arrive as a `wrong_stream` refusal and be answered unavailable — a
// 503 sending a client to another node that refused it identically. The code
// keeps its other meaning, which IS a state of this node (its checkpoint past
// the log's end, or a stream rebuilt under it), and that one stays
// unavailable: the control.
func TestAFloorOnAnotherLogIsBadParamsAndARebuiltStreamIsNot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"a min_position on another domain's log",
			fmt.Errorf("tracker: read: %w: this read floors at CREWLET_PAGES_LOG@1:5",
				statelog.ErrForeignPosition),
			queries.ErrBadParams},
		{"a node whose stream was rebuilt under it",
			&statelog.Refused{Code: statelog.RefuseWrongStream, Level: statelog.ReadStale},
			queries.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := queries.NewRegistry()
			queries.Register(r, queries.Sources{Work: &stubWork{err: tc.err}})
			_, err := r.Answer(asGrants(t, "", iam.GrantTracker), "work_items",
				map[string]any{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("answered %v, want %v", err, tc.want)
			}
			if errors.Is(tc.want, queries.ErrBadParams) && errors.Is(err, queries.ErrUnavailable) {
				t.Errorf("answered %v, which is also unavailable — a request "+
					"every node refuses is not this node's to come back to", err)
			}
		})
	}
}

// A FEED'S REQUEST MISTAKE IS BAD PARAMS, AND A FEED'S FAULT IS NOT.
//
// `work_activity` and `work_inbox` read two positions off the wire — a cursor
// and a `since` — and the reader refuses one it cannot use, before any row, as
// [tracker.ErrBadQuery]: not a position, or a position from another log. That
// is the caller's, so it is bad params. `work_activity` used to answer EVERY
// failure that was not the state log's as bad params, so a store this node
// could not read was told to the caller as their mistake; `work_inbox` answered
// none, so a malformed cursor was a fault. The sentinel decides now, and a
// store's own error stays a fault — the control.
func TestAFeedsRequestMistakeIsBadParamsAndItsFaultIsNot(t *testing.T) {
	t.Parallel()
	mistake := fmt.Errorf("%w: cursor: statelog: a position on another log",
		tracker.ErrBadQuery)
	fault := errors.New("open /var/lib/crewlet/replicated.db: disk I/O error")
	for _, what := range []struct {
		name string
		args map[string]any
	}{
		{"work_activity", map[string]any{"container": "workspace"}},
		{"work_inbox", map[string]any{"handle": "ana"}},
	} {
		t.Run(what.name, func(t *testing.T) {
			t.Parallel()
			_, err := askAsOperator(t, queries.Sources{Work: &stubWork{err: mistake}},
				what.name, what.args)
			if !errors.Is(err, queries.ErrBadParams) {
				t.Errorf("a refused request answered %v, want %v", err, queries.ErrBadParams)
			}
			_, err = askAsOperator(t, queries.Sources{Work: &stubWork{err: fault}},
				what.name, what.args)
			if err == nil || errors.Is(err, queries.ErrBadParams) ||
				errors.Is(err, queries.ErrUnavailable) {
				t.Errorf("a store fault answered %v, want a plain failure — it "+
					"is this node's, not the caller's and not a refusal", err)
			}
		})
	}
}

// AND A REFUSAL WAITING CANNOT CLEAR IS UNAVAILABLE TOO — WITH NO HINT.
//
// A node holding a record it cannot decode will not catch up however long the
// caller waits, and a log at its ceiling or a barrier its broker refused will
// not either. None of them is a fault: they were one, a 500 `query_failed`
// whose remedy went only to the log, and a linearizable read on a full log
// became one the moment its barrier stopped reading as a missed quorum. So
// every state-log refusal is [queries.ErrUnavailable], and what keeps a
// client out of a loop is the hint, which is the state log's own rule and
// ZERO for these — a surface answers it with no Retry-After. A behind node
// is the control: it keeps its derived hint.
func TestARefusalWaitingCannotClearIsUnavailableWithNoHint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		code statelog.ReadRefusal
		want time.Duration
	}{
		{"a record this node cannot decode", statelog.RefuseDeferred, 0},
		{"a log at its byte ceiling", statelog.RefuseLogFull, 0},
		{"a barrier the broker refused", statelog.RefuseBrokerRefused, 0},
		{"a node that is behind, the control", statelog.RefuseBehind, 12 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			refused := &statelog.Refused{Code: tc.code, Level: statelog.ReadLinearizable,
				Detail: "what the refusal is about"}
			if tc.code.Retryable() {
				refused.RetryAfter = 12 * time.Second
			}
			r := queries.NewRegistry()
			queries.Register(r, queries.Sources{Work: &stubWork{err: refused}})

			_, err := r.Answer(asGrants(t, "", iam.GrantTracker), "work_items",
				map[string]any{})
			if !errors.Is(err, queries.ErrUnavailable) {
				t.Fatalf("a read refused %s answered %v, want %v — it is this "+
					"node's refusal, not a fault", tc.code, err, queries.ErrUnavailable)
			}
			if got := statelog.RetryAfter(err, 5*time.Second); got != tc.want {
				t.Errorf("a read refused %s carries a hint of %s, want %s",
					tc.code, got, tc.want)
			}
		})
	}
}

// A REFUSAL ABOUT THE REQUEST STAYS ONE, even when what it wraps is a refusal
// the registry would otherwise answer as unavailable. The caller has to change
// the request; "come back" would send the identical one round a loop.
func TestARefusalAboutTheRequestIsNeverReclassifiedAsUnavailable(t *testing.T) {
	t.Parallel()
	behind := &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadSession,
		RetryAfter: time.Second}
	work := &stubWork{err: fmt.Errorf("%w: %w", tracker.ErrTooBroad, behind)}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Work: work})

	_, err := r.Answer(asGrants(t, "", iam.GrantTracker), "work_items", map[string]any{})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("err = %v, want ErrBadParams", err)
	}
	if errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("a refusal about the request was also reported as %v", queries.ErrUnavailable)
	}
}

// A READ NO COPY COULD ANSWER IS "COME BACK", NEVER "ASK SOMETHING ELSE".
//
// The tracker is read through the estate router, as a seat's tools read it, so
// a copy out of service, a peer restarting or no data node live at all reaches
// this surface as the router's own refusal. The answers that read what is left
// of a failure as a refusal of the request — a view's expansion, the activity
// gate — told a caller to change a question nothing was wrong with, with a
// 400; and the registry answered a 500 where a screen should try again.
//
// AND SO IS EVERY OTHER CONDITION OF THIS NODE the tools' own rule names
// (builtin.Condition): a router that could not even send the request because
// this node's queue is not live, a store held closed while it adopts a peer's
// snapshot, a read that ran out of time. This surface kept a second, shorter
// list of its own, and a view's expansion that met any of those told a caller
// its question was malformed.
//
// Mutation: put the old three-sentinel list back in unavailableHere and the
// queue, store and deadline cases go red; drop the ErrUnserved arm from
// builtin.Condition and the router's do.
func TestAReadNoCopyCouldAnswerIsUnavailable(t *testing.T) {
	t.Parallel()
	unserved := &estate.ErrUnserved{Detail: "data-a: no answer"}
	notLive := fmt.Errorf("estate: tracker.expanded_query: %w", queue.ErrNotLive)
	closed := fmt.Errorf("tracker: read the board: %w", store.ErrNoEstate)
	for _, tc := range []struct {
		name   string
		what   string
		params map[string]any
		work   *stubWork
	}{
		{"the board", "work_items", map[string]any{}, &stubWork{err: unserved}},
		{"a view's expansion", "work_items", map[string]any{},
			&stubWork{expandErr: fmt.Errorf("estate: tracker.expanded_query: %w", unserved)}},
		{"the activity", "work_activity", map[string]any{"container": "workspace"},
			&stubWork{err: unserved}},
		{"a view's expansion this node could not send", "work_items", map[string]any{},
			&stubWork{expandErr: notLive}},
		{"a view's expansion that ran out of time", "work_items", map[string]any{},
			&stubWork{expandErr: fmt.Errorf("estate: tracker.expanded_query: %w",
				context.DeadlineExceeded)}},
		{"the board on a store held closed", "work_items", map[string]any{},
			&stubWork{err: closed}},
	} {
		_, err := askNative(t, queries.Sources{Work: tc.work}, tc.what, tc.params)
		if !errors.Is(err, queries.ErrUnavailable) || errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s: a read no copy could answer = %v, want %v and never %v", tc.name,
				err, queries.ErrUnavailable, queries.ErrBadParams)
		}
	}
}

// A VIEW STRIP'S CONTAINER IS THE QUERY GRAMMAR'S OWN SPELLING.
//
// A screen reaches the strip and then the tasks in it; two spellings of one
// container would make the tab it lands on belong to a different project from
// the rows beneath it.
func TestAViewStripTakesTheContainerTheBoardTakes(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		kind, id string
	}{
		{"workspace", tracker.ContainerWorkspace, ""},
		{"project:eng", tracker.ContainerProject, "ENG"},
		{"project:ENG", tracker.ContainerProject, "ENG"},
		{"unit:engineering", tracker.ContainerUnit, "engineering"},
		{"person:ana", tracker.ContainerPerson, "ana"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			w := &stubWork{}
			// AS ANA, because the strip is personalised by the
			// CALLER's own seat now: a `viewer=` parameter was a
			// free choice of whose pins to read.
			if _, err := askAsSeat(t, queries.Sources{Work: w}, "ana",
				"work_views", map[string]any{"container": tc.raw}); err != nil {
				t.Fatalf("work_views: %v", err)
			}
			if w.views.Container.Kind != tc.kind || w.views.Container.ID != tc.id {
				t.Fatalf("%q reached the reader as %s %q, want %s %q", tc.raw,
					w.views.Container.Kind, w.views.Container.ID, tc.kind, tc.id)
			}
			if w.views.Viewer != "ana" {
				t.Fatalf("the viewer reached the reader as %q", w.views.Viewer)
			}
		})
	}

	// AND A CONTAINER THE TRACKER COULD NOT HOLD IS A BAD PARAMETER, not
	// an empty strip: a screen rendering nothing cannot tell a container
	// with no views from one that does not exist.
	for _, raw := range []string{"", "team:eng", "project:", "ENG"} {
		w := &stubWork{}
		_, err := askNative(t, queries.Sources{Work: w}, "work_views",
			map[string]any{"container": raw})
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("container=%q answered %v, want a bad-parameter refusal", raw, err)
		}
	}
}

// THE PROJECTS LISTING'S TWO CLOSED SETS REACH THE READER AS THEMSELVES.
//
// `archived=` SELECTS a set and `sort=` orders the WHOLE of it before the
// engine's own cap takes a page. Both were the screen's own work once — the
// route sent a widening boolean and the directory narrowed and re-sorted what
// came back — so past the cap the Archived segment reported a company with
// dozens of retired projects as having none, and `sort=-open` ranked the most
// open work among the projects whose keys sort first.
func TestTheProjectsListingCarriesItsArchivalSetAndOrdering(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		archived   string
		sort       string
		wantSet    tracker.ArchivedMode
		wantSort   tracker.ProjectSort
		descending bool
	}{
		{"", "", tracker.ArchivedExclude, "", false},
		{"false", "key", tracker.ArchivedExclude, tracker.ProjectSortKey, false},
		{"only", "-active", tracker.ArchivedOnly, tracker.ProjectSortActive, true},
		{"true", "last_change", tracker.ArchivedInclude, tracker.ProjectSortLastChange, false},
	} {
		w := &stubWork{}
		args := map[string]any{}
		if tc.archived != "" {
			args["archived"] = tc.archived
		}
		if tc.sort != "" {
			args["sort"] = tc.sort
		}
		if _, err := askNative(t, queries.Sources{Work: w}, "work_projects",
			args); err != nil {
			t.Fatalf("work_projects%+v: %v", args, err)
		}
		if w.projectQuery.Archived != tc.wantSet {
			t.Errorf("archived=%q reached the reader as %q, want %q",
				tc.archived, w.projectQuery.Archived, tc.wantSet)
		}
		if w.projectQuery.Sort != tc.wantSort ||
			w.projectQuery.Descending != tc.descending {
			t.Errorf("sort=%q reached the reader as %q/%v, want %q/%v",
				tc.sort, w.projectQuery.Sort, w.projectQuery.Descending,
				tc.wantSort, tc.descending)
		}
	}

	// AND A VALUE THAT IS NEITHER SET'S IS A 400 NAMING THE PARAMETER, not
	// a silent fall back: a screen handed the default for a filter it
	// asked for draws a set nobody chose, and the reader has no way to
	// tell. `bad_params` is also the one code the dashboard renders as the
	// SCREEN's fault, so retrying is not offered.
	for _, tc := range []struct{ key, value, want string }{
		{"archived", "yes", "archived"},
		// `active` READS LIKE A VALUE and is not one — the live set is
		// `false`. Accepting it as the default would be exactly the
		// silent narrowing this refusal exists to stop.
		{"archived", "active", "archived"},
		{"sort", "lead", "sort"},
		{"sort", "-progress", "sort"},
	} {
		w := &stubWork{}
		_, err := askNative(t, queries.Sources{Work: w}, "work_projects",
			map[string]any{tc.key: tc.value})
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s=%s answered %v, want a bad-parameter refusal",
				tc.key, tc.value, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) ||
			!strings.Contains(err.Error(), tc.value) {
			t.Errorf("%s=%s is refused with %q, want the parameter and the "+
				"value named", tc.key, tc.value, err)
		}
		// AND THE READ IS NEVER MADE. A refusal that had already asked
		// the store would be a 400 over an answer somebody paid for.
		if w.projectQuery.Level != "" {
			t.Errorf("%s=%s reached the reader as %+v, want no read at all",
				tc.key, tc.value, w.projectQuery)
		}
	}
}

// A PROJECT THAT IS THERE IS NEVER ANSWERED AS MISSING. A `for_type` the
// company does not file was refused wrapped in tracker.ErrNoProject, so the
// route answered 404 about a project that exists — a dead link to a person,
// for an argument they could fix. The key that names nothing is still 404.
func TestAProjectDetailClassesWhatIsMissing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"no project", fmt.Errorf("tracker: no project \"ENGG\": %w", tracker.ErrNoProject),
			queries.ErrNotFound},
		{"no type", fmt.Errorf("tracker: no type \"epic\": %w", tracker.ErrNoType),
			queries.ErrBadParams},
	} {
		_, err := askNative(t, queries.Sources{Work: &stubWork{err: tc.err}},
			"work_project", map[string]any{"key": "ENG", "for_type": "epic"})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: answered %v, want %v", tc.name, err, tc.want)
		}
		if errors.Is(tc.want, queries.ErrBadParams) && errors.Is(err, queries.ErrNotFound) {
			t.Errorf("%s: an unknown type is also classed not-found", tc.name)
		}
	}
}

// A STRIP IS PERSONALISED BY THE CALLER AND BY NOBODY ELSE.
//
// `viewer=` used to select WHOSE pins and personal views order the strip, and
// nothing checked it: on a node with anonymous reads a caller could take the
// handles out of `org` and page through every seat's pinned views. The
// parameter is gone — the strip is ordered by the caller's own record — so
// there is no handle to check and nothing to refuse.
//
// A CALLER WITH NO SEAT GETS THEIR OWN STRIP TOO, under the name their record
// is kept under — the login `set_pins` and `save_work_view` write with. It was
// read under no name at all, so an unbound caller's assistant pinned views
// their strip never showed. And a strip is about a CONTAINER, so a caller the
// engine can name nothing for still gets the shared one, never a refusal.
func TestAStripIsPersonalisedByTheCallerAlone(t *testing.T) {
	t.Parallel()

	// THE CALLER'S OWN, never a handle in the bag.
	w := &stubWork{}
	if _, err := askAsSeat(t, queries.Sources{Work: w}, "ada-okonkwo",
		"work_views", map[string]any{"container": "workspace"}); err != nil {

		t.Fatalf("work_views: %v", err)
	}
	if w.views.Viewer != "ada-okonkwo" {
		t.Errorf("the strip was personalised by %q, want the caller's own seat",
			w.views.Viewer)
	}

	// AND A HANDLE IN THE BAG IS IGNORED rather than honoured: the
	// parameter does not exist, so there is no way to ask for somebody
	// else's pins at all.
	w = &stubWork{}
	if _, err := askAsSeat(t, queries.Sources{Work: w}, "ada-okonkwo",
		"work_views", map[string]any{
			"container": "workspace", "viewer": "somebody.else",
		}); err != nil {

		t.Fatalf("work_views: %v", err)
	}
	if w.views.Viewer != "ada-okonkwo" {
		t.Errorf("a `viewer` in the bag reached the reader as %q", w.views.Viewer)
	}

	// AND AN UNBOUND CALLER'S STRIP IS THEIRS, under their login.
	w = &stubWork{}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Work: w})
	if _, err := r.Answer(everyGrant(t), "work_views",
		map[string]any{"container": "workspace"}); err != nil {

		t.Fatalf("the strip, asked by an unbound caller: %v", err)
	}
	if w.views.Viewer != "token:test" {
		t.Errorf("an unbound token's strip was personalised by %q, want its "+
			"login — the name its own pins are written under", w.views.Viewer)
	}
}

// PINNED COUNTS ARE ASKED FOR, AND ONLY FOR A RECORD.
//
// `counts=true` reaches the reader with the company's own clock, because a
// pinned view's relative dates are cut on it exactly as the board it opens
// cuts them. The counts are of the CALLER's own pins — their record, never a
// name they typed — so for a caller the engine can name no record for it is
// refused by name: answering it with none would read as nothing pinned.
func TestPinnedCountsReachTheReaderOnlyForARecord(t *testing.T) {
	t.Parallel()
	w := &stubWork{}
	if _, err := askAsSeat(t, queries.Sources{Work: w}, "ada-okonkwo", "work_views",
		map[string]any{"container": "workspace", "counts": "true"}); err != nil {
		t.Fatalf("counts for the caller: %v", err)
	}
	if !w.views.Counts || w.views.Viewer != "ada-okonkwo" || w.views.Now.IsZero() ||
		w.views.Zone == nil {
		t.Errorf("the reader was asked %+v — want the caller's pins counted, with "+
			"an instant and the company's clock", w.views)
	}

	w = &stubWork{}
	nobody := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantTracker}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Work: w})
	if _, err := r.Answer(iam.WithPrincipal(t.Context(), nobody), "work_views",
		map[string]any{"container": "workspace", "counts": "true"}); !errors.Is(
		err, queries.ErrBadParams) {
		t.Errorf("counts for a caller with no record answered %v, want a "+
			"bad-params refusal", err)
	}
	if w.views.Counts {
		t.Error("the refused counts reached the reader")
	}

	w = &stubWork{}
	if _, err := askAsSeat(t, queries.Sources{Work: w}, "ada-okonkwo", "work_views",
		map[string]any{"container": "workspace"}); err != nil {
		t.Fatalf("a strip without counts: %v", err)
	}
	if w.views.Counts {
		t.Error("a strip that did not ask for counts was counted — every " +
			"poll would pay a query per pin")
	}
}

// A GROUPED ANSWER REACHES THE CALLER.
//
// The payload was built by hand from `items` alone, and a grouped answer has
// NO flat rows by construction — so a populated board arrived as `items: []`
// and a screen rendered a company with no work.
func TestAGroupedAnswerReachesTheCaller(t *testing.T) {
	w := &stubWork{answer: tracker.Answer{
		Groups: []tracker.Group{{
			Key: "todo", Count: 12,
			Rows: []tracker.TaskRow{{ID: "t-1", Key: "ENG-1"}},
		}},
		GroupsDropped: 3, GroupsOverlap: true,
		Totals:   []tracker.Total{{Key: "points:sum", Column: "points", Op: "sum"}},
		Complete: true,
	}}
	got, err := askNative(t, queries.Sources{Work: w}, "work_items",
		map[string]any{"container": "project:eng", "group_by": "status"})
	if err != nil {
		t.Fatalf("work_items: %v", err)
	}
	payload, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("the answer is %T", got)
	}
	for _, key := range []string{"groups", "groups_dropped", "groups_overlap",
		"totals"} {
		if _, held := payload[key]; !held {
			t.Errorf("the payload carries no %q, so a board built from it "+
				"renders a company with no work", key)
		}
	}
}

// A CALLER THAT ASKED WHERE A TASK SITS IS ANSWERED, NULL INCLUDED.
//
// `around=` is a task page asking "3 of what": a task that has moved off the
// board answers `null`, and the key has to be ON the payload for a page to
// tell that from a server that never understood the question — while a caller
// that did not ask gets no key at all, so every other board's payload is
// unchanged.
func TestAroundReachesTheCallerAndIsNullWhenAbsent(t *testing.T) {
	position := 3
	next := "ENG-4"
	for _, tc := range []struct {
		name   string
		params map[string]any
		answer tracker.Answer
		held   bool
		want   any
	}{
		{"placed", map[string]any{"container": "project:eng", "around": "ENG-3"},
			tracker.Answer{Complete: true, Around: &tracker.Around{
				Position: &position, Next: &next, TotalHint: 18}}, true, nil},
		{"absent", map[string]any{"container": "project:eng", "around": "ENG-9"},
			tracker.Answer{Complete: true}, true, (*tracker.Around)(nil)},
		{"not asked", map[string]any{"container": "project:eng"},
			tracker.Answer{Complete: true}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := askNative(t, queries.Sources{Work: &stubWork{
				answer: tc.answer}}, "work_items", tc.params)
			if err != nil {
				t.Fatalf("work_items: %v", err)
			}
			payload, _ := got.(map[string]any)
			value, held := payload["around"]
			if held != tc.held {
				t.Fatalf("the payload carries around=%v (held %v), want held %v",
					value, held, tc.held)
			}
			switch {
			case !held:
			case tc.answer.Around != nil:
				if value != tc.answer.Around {
					t.Errorf("around is %v, want the reader's own answer", value)
				}
			default:
				if value != tc.want {
					t.Errorf("around is %#v, want a typed null", value)
				}
			}
		})
	}
}

// sessionQuestion is one question this surface serves at a caller's floor:
// the domains whose logs it reads — one, or each of the several an answer
// composed from more than one log reads, each at its own floor — the
// arguments it needs to answer at all, and where the stubs record the level it
// was asked at and the staleness bound each of its readers was handed.
type sessionQuestion struct {
	what    string
	domains []string
	args    map[string]any
	level   func(stubs) statelog.ReadLevel
	// bounds are the staleness bounds the readers this question reads were
	// handed, as the stubs recorded them: ONE PER READER, in the order of
	// domains, so an answer composed of halves names each half's, and a half
	// that dropped the caller's bound is seen as the half it is.
	bounds func(stubs) []bound
	// ready prepares stubs a question needs to answer rather than refuse
	// (a revision has to be held to be read); nil for none.
	ready func(stubs)
}

// bound is a staleness bound as one reader was handed it, in both of its
// units.
type bound struct {
	maxLag    time.Duration
	maxLagSeq uint64
}

// stubs are the readers one ask of a [sessionQuestion] is answered from — the
// tracker's, the knowledge base's and the code host's — each recording what
// it was asked.
type stubs struct {
	work  *stubWork
	pages *stubPages
	code  *stubCode
}

// newStubs are stubs nothing has asked yet, readied for q.
func newStubs(q sessionQuestion) stubs {
	s := stubs{work: &stubWork{}, pages: &stubPages{}, code: &stubCode{}}
	if q.ready != nil {
		q.ready(s)
	}
	return s
}

// sources are s as a registry is wired with them.
func (s stubs) sources() queries.Sources {
	return queries.Sources{Work: s.work, Pages: s.pages, Code: s.code}
}

// sessionQuestions are EVERY question on this surface that reads a
// freshness — and therefore takes a write's position as its floor. One
// table, walked by [TestEveryNativeQuestionResolvesTheCallersOwnLevel] for
// the level, by [TestTheStalenessBoundsReachEveryQuestionThatCanHoldThem] for
// the bound and by [TestEverySessionQueryTakesAFreshnessFloor] against the
// dashboard's own list, so the three cannot come to describe different sets.
var sessionQuestions = []sessionQuestion{
	{"work_items", []string{"tracker"}, map[string]any{},
		func(s stubs) statelog.ReadLevel { return s.work.query.Level },
		func(s stubs) []bound { return handed(s.work.query.MaxLag, s.work.query.MaxLagSeq) }, nil},
	{"work_item", []string{"tracker"}, map[string]any{"id": "ENG-1"},
		func(s stubs) statelog.ReadLevel { return s.work.taskLevel },
		func(s stubs) []bound { return handedWhole(s.work.taskFresh) }, nil},
	{"work_comments", []string{"tracker"}, map[string]any{"item": "ENG-1"},
		func(s stubs) statelog.ReadLevel { return s.work.taskLevel },
		func(s stubs) []bound { return handedWhole(s.work.taskFresh) }, nil},
	{"work_item_turns", []string{"tracker"}, map[string]any{"id": "ENG-1"},
		func(s stubs) statelog.ReadLevel { return s.work.taskFresh.Level },
		func(s stubs) []bound { return handedWhole(s.work.taskFresh) }, nil},
	{"work_views", []string{"tracker"}, map[string]any{"container": "workspace"},
		func(s stubs) statelog.ReadLevel { return s.work.views.Level },
		func(s stubs) []bound { return handed(s.work.views.MaxLag, s.work.views.MaxLagSeq) }, nil},
	{"work_saved_views", []string{"tracker"}, map[string]any{},
		func(s stubs) statelog.ReadLevel { return s.work.every.Level },
		func(s stubs) []bound { return handed(s.work.every.MaxLag, s.work.every.MaxLagSeq) }, nil},
	{"work_catalogue", []string{"tracker"}, map[string]any{},
		func(s stubs) statelog.ReadLevel { return s.work.catalogueQuery.Level },
		func(s stubs) []bound {
			return handed(s.work.catalogueQuery.MaxLag, s.work.catalogueQuery.MaxLagSeq)
		}, nil},
	{"work_person", []string{"tracker"}, map[string]any{"handle": "ana"},
		func(s stubs) statelog.ReadLevel { return s.work.personQuery.Level },
		func(s stubs) []bound { return handed(s.work.personQuery.MaxLag, s.work.personQuery.MaxLagSeq) }, nil},
	{"work_projects", []string{"tracker"}, map[string]any{},
		func(s stubs) statelog.ReadLevel { return s.work.projectQuery.Level },
		func(s stubs) []bound {
			return handed(s.work.projectQuery.MaxLag, s.work.projectQuery.MaxLagSeq)
		}, nil},
	{"work_project", []string{"tracker"}, map[string]any{"key": "ENG"},
		func(s stubs) statelog.ReadLevel { return s.work.detailQuery.Level },
		func(s stubs) []bound { return handed(s.work.detailQuery.MaxLag, s.work.detailQuery.MaxLagSeq) }, nil},
	{"work_workload", []string{"tracker"}, map[string]any{},
		func(s stubs) statelog.ReadLevel { return s.work.workloadQuery.Level },
		func(s stubs) []bound {
			return handed(s.work.workloadQuery.MaxLag, s.work.workloadQuery.MaxLagSeq)
		}, nil},
	{"work_activity", []string{"tracker"}, map[string]any{"container": "workspace"},
		func(s stubs) statelog.ReadLevel { return s.work.activityQuery.Level },
		func(s stubs) []bound {
			return handed(s.work.activityQuery.MaxLag, s.work.activityQuery.MaxLagSeq)
		}, nil},
	// OPERATOR-ONLY, and it is in this walk precisely because it
	// is: `my_work` is somebody's whole day, and an operator
	// reading it at a level nobody chose is the same defect with a
	// credential in front of it.
	{"work_my_work", []string{"tracker"}, map[string]any{"handle": "ana"},
		func(s stubs) statelog.ReadLevel { return s.work.myWorkQuery.Level },
		func(s stubs) []bound { return handed(s.work.myWorkQuery.MaxLag, s.work.myWorkQuery.MaxLagSeq) }, nil},
	{"work_inbox", []string{"tracker"}, map[string]any{"handle": "ana"},
		func(s stubs) statelog.ReadLevel { return s.work.inboxQuery.Level },
		func(s stubs) []bound { return handed(s.work.inboxQuery.MaxLag, s.work.inboxQuery.MaxLagSeq) }, nil},
	{"work_routing", []string{"tracker"}, map[string]any{"record_id": "r-1"},
		func(s stubs) statelog.ReadLevel { return s.work.routingQuery.Level },
		func(s stubs) []bound {
			return handed(s.work.routingQuery.MaxLag, s.work.routingQuery.MaxLagSeq)
		}, nil},
	{"work_flow", []string{"tracker"}, map[string]any{},
		func(s stubs) statelog.ReadLevel { return s.work.flowQuery.Level },
		func(s stubs) []bound { return handed(s.work.flowQuery.MaxLag, s.work.flowQuery.MaxLagSeq) }, nil},
	// TWO LOGS: the work is the tracker's and the pages the knowledge
	// base's, each half at its own floor
	// ([TestTheCompanyFeedWaitsOnTheFloorOfEachLogItReads]). Asked for a
	// kind of each, so every walk reaches both halves, and each half's
	// bound is its own ([statelog.Freshness.On] hands each a copy of the
	// ask).
	{"company_feed", []string{"tracker", "pages"}, map[string]any{"kinds": "completed,page"},
		func(s stubs) statelog.ReadLevel { return s.work.feedQuery.Level },
		func(s stubs) []bound {
			return append(handed(s.work.feedQuery.MaxLag, s.work.feedQuery.MaxLagSeq),
				handedWhole(s.pages.fresh)...)
		}, nil},
	{"decisions", []string{"tracker"}, map[string]any{"handle": "ana"},
		func(s stubs) statelog.ReadLevel { return s.work.decisionsQuery.Level },
		func(s stubs) []bound {
			return handed(s.work.decisionsQuery.MaxLag, s.work.decisionsQuery.MaxLagSeq)
		}, nil},
	{"pages", []string{"pages"}, map[string]any{},
		func(s stubs) statelog.ReadLevel { return s.pages.level },
		func(s stubs) []bound { return handedWhole(s.pages.fresh) }, nil},
	{"page", []string{"pages"}, map[string]any{"id": "p1"},
		func(s stubs) statelog.ReadLevel { return s.pages.level },
		func(s stubs) []bound { return handedWhole(s.pages.fresh) }, nil},
	{"containers", []string{"pages"}, map[string]any{},
		func(s stubs) statelog.ReadLevel { return s.pages.level },
		func(s stubs) []bound { return handedWhole(s.pages.fresh) }, nil},
	{"page_activity", []string{"pages"}, map[string]any{},
		func(s stubs) statelog.ReadLevel { return s.pages.level },
		func(s stubs) []bound { return handedWhole(s.pages.fresh) }, nil},
	{"page_revision", []string{"pages"}, map[string]any{"page": "p1", "version": 2},
		func(s stubs) statelog.ReadLevel { return s.pages.level },
		func(s stubs) []bound { return handedWhole(s.pages.fresh) },
		func(s stubs) { s.pages.revisionHeld = true }},
	// THE CODE HOST'S ROWS, each read back at the code log's floor once a
	// write lands there and answers with where (code.go).
	{"code_repos", []string{"code"}, map[string]any{},
		func(s stubs) statelog.ReadLevel { return s.code.last().Fresh.Level },
		func(s stubs) []bound { return handedWhole(s.code.last().Fresh) }, nil},
	{"code_repo", []string{"code"}, map[string]any{"name": "web"},
		func(s stubs) statelog.ReadLevel { return s.code.last().Fresh.Level },
		func(s stubs) []bound { return handedWhole(s.code.last().Fresh) }, nil},
	{"code_pushes", []string{"code"}, map[string]any{"name": "web"},
		func(s stubs) statelog.ReadLevel { return s.code.last().Fresh.Level },
		func(s stubs) []bound { return handedWhole(s.code.last().Fresh) }, nil},
}

// handed is the bound of a reader of one log, as its stub recorded the two
// units off the reader's own query.
func handed(maxLag time.Duration, maxLagSeq uint64) []bound {
	return []bound{{maxLag: maxLag, maxLagSeq: maxLagSeq}}
}

// handedWhole is [handed] for a reader handed the ask whole.
func handedWhole(f statelog.Freshness) []bound { return handed(f.MaxLag, f.MaxLagSeq) }

// EVERY QUESTION ON THIS SURFACE RESOLVES THE CALLER'S OWN FRESHNESS, and the
// one that did not was twelve of the thirteen.
//
// `work_items` read `read_level` because it is the question that goes through
// the query grammar. Every other one wrote a hardcoded `stale` into its query
// and never looked at the key at all — so a caller asking a project listing
// for a linearizable answer was served this node's rows and told, in the
// answer's own `read_level` field, that it came back linearizable. A level
// that never downgrades silently is the whole contract; a surface that
// silently ignored the ask broke it through the other door.
//
// The case is written as a WALK over the questions rather than one assertion
// per reader, because the defect was never in one of them: it was that adding
// the fourteenth question would inherit whatever the thirteenth typed.
func TestEveryNativeQuestionResolvesTheCallersOwnLevel(t *testing.T) {
	t.Parallel()
	for _, tc := range sessionQuestions {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			ask := askNative
			if personalQuestions[tc.what] {
				ask = askAsOperator
			}
			// asked asks tc with params beside its own arguments, of stubs
			// nothing has asked yet, which it answers with what they
			// recorded.
			asked := func(params map[string]any) (stubs, error) {
				s := newStubs(tc)
				all := map[string]any{}
				for k, v := range params {
					all[k] = v
				}
				for k, v := range tc.args {
					all[k] = v
				}
				_, err := ask(t, s.sources(), tc.what, all)
				return s, err
			}

			// THE DEFAULT FIRST: a caller who says nothing gets this
			// surface's own level, not whatever the reader defaults to.
			s, err := asked(nil)
			if err != nil {
				t.Fatalf("%s: %v", tc.what, err)
			}
			want := statelog.DefaultReadLevel(statelog.SurfaceDashboard)
			if got := tc.level(s); got != want {
				t.Errorf("%s read at %q with no ask, want this surface's "+
					"default %q", tc.what, got, want)
			}

			// AND THE ASK IS HONOURED, which is the half twelve of these
			// questions dropped on the floor.
			if s, err = asked(map[string]any{"read_level": "linearizable"}); err != nil {
				t.Fatalf("%s asking linearizable: %v", tc.what, err)
			}
			if got := tc.level(s); got != statelog.ReadLinearizable {
				t.Errorf("%s asked for linearizable and read at %q — a level "+
					"the answer then reports back as the one asked for is a "+
					"stale answer wearing a stronger name", tc.what, got)
			}

			// AND A BARE `session` IS REFUSED RATHER THAN QUIETLY
			// SERVED: it waits for a position, and none was named.
			if _, err := asked(map[string]any{"read_level": "session"}); !errors.Is(err, queries.ErrBadParams) {
				t.Errorf("%s accepted a bare read_level=session, answering %v",
					tc.what, err)
			}

			// WITH THE POSITION IT IS HONOURED, which is the whole of
			// read-your-writes over this wire: a write answered with
			// where it landed and the caller hands that back.
			if s, err = asked(map[string]any{
				"read_level": "session", "min_position": "CREWLET_TRACKER_LOG@1:4711",
			}); err != nil {
				t.Fatalf("%s asking session with a floor: %v", tc.what, err)
			}
			if got := tc.level(s); got != statelog.ReadSession {
				t.Errorf("%s asked for session with a floor and read at %q",
					tc.what, got)
			}
		})
	}
}

// AND THE STALENESS BOUND REACHES THE READER, in both of its units.
//
// A bound refused where it is inconsistent and dropped where it is not is a
// bound that never bounded anything — the defect `max_lag_seconds` was fixed
// for on the board, and `max_lag_seq` was left in beside it, and which every
// other question here had in both units because none of them read either key.
// A tile declaring "at most twenty seconds behind" and rendering whatever came
// back is a live screen showing an hour-old answer.
//
// THE POINT READS AND THE PAGE READS TOO. They handed over the level alone,
// on the claim that a single row has no set for a bound to be enforced
// against — but the bound is about this node's LAG, and a row is exactly as
// far behind as a board.
//
// A WALK OVER [sessionQuestions], the one table of the questions this surface
// serves at a caller's floor, rather than a list of its own: this walk kept
// one, which stopped growing while the surface did, and `company_feed` — the
// first answer read from two logs, each half handed an ask of its own
// ([statelog.Freshness.On]) — was one it never asked, so a half that dropped
// the bound passed this walk and every other case in the package. And EVERY
// READER a question reads is held to it, one per log it reads: both halves of
// the feed, each where its own stub recorded it.
//
// Mutation: build the feed's halves as `Freshness{Level: f.Level}` in
// [statelog.Freshness.On] and `company_feed` goes red here, for both halves.
func TestTheStalenessBoundsReachEveryQuestionThatCanHoldThem(t *testing.T) {
	t.Parallel()
	args := map[string]any{"max_lag_seconds": "30", "max_lag_seq": "250"}
	for _, tc := range sessionQuestions {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			ask := askNative
			if personalQuestions[tc.what] {
				ask = askAsOperator
			}
			all := map[string]any{}
			for k, v := range args {
				all[k] = v
			}
			for k, v := range tc.args {
				all[k] = v
			}
			s := newStubs(tc)
			if _, err := ask(t, s.sources(), tc.what, all); err != nil {
				t.Fatalf("%s: %v", tc.what, err)
			}
			got := tc.bounds(s)
			if want := len(tc.domains); len(got) != want {
				t.Fatalf("%s names the bounds of %d reader(s) and reads %d log(s) — "+
					"name one per reader, or a half goes unchecked", tc.what, len(got), want)
			}
			for i, b := range got {
				reader := fmt.Sprintf("%s's %s reader", tc.what, tc.domains[i])
				if b.maxLag != 30*time.Second {
					t.Errorf("%s carried max_lag_seconds as %s", reader, b.maxLag)
				}
				if b.maxLagSeq != 250 {
					t.Errorf("%s carried max_lag_seq as %d — the record count is "+
						"the reading the broker actually answers", reader, b.maxLagSeq)
				}
			}

			// AND A BOUND AT A LEVEL THAT IS NOT STALE IS REFUSED
			// rather than carried and ignored: it is not a level of its
			// own and it narrows nothing at the other two, so passing
			// it there is a caller who believes they asked for
			// something they did not.
			all["read_level"] = "linearizable"
			if _, err := ask(t, newStubs(tc).sources(), tc.what, all); !errors.Is(err, queries.ErrBadParams) {
				t.Errorf("%s took a staleness bound at linearizable, answering %v",
					tc.what, err)
			}
		})
	}
}

// AND THE FLOOR REACHES EVERY QUESTION, at every level it may be asked at.
//
// `min_position` is what a write's answer carries, handed back: a caller that
// created a task through the operator MCP and redraws the board over this
// surface names the position and is served nothing from before it. A question
// that parsed the key and dropped it would answer identically — same rows,
// same `read_level` — which is the shape the staleness bounds already shipped
// in, so the walk is over every question rather than the one that goes
// through the grammar.
//
// AND EVERY FLOOR REACHES IT, not the first: `min_position` repeats once per
// log, and a question that read one log handed its reader only the floor it
// found first would drop a second the caller named — a floor on another log,
// which the reader refuses as the request's mistake
// ([TestASingleLogReadRefusesAnotherLogsFloor]), and which a reader never
// handed it cannot refuse.
func TestTheCallersFloorReachesEveryNativeQuestion(t *testing.T) {
	t.Parallel()
	want := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 4711}
	other := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 99}
	for _, tc := range []struct {
		what   string
		args   map[string]any
		floors func(*stubWork, *stubPages) []statelog.Position
	}{
		{"work_items", nil, func(w *stubWork, _ *stubPages) []statelog.Position { return w.query.Floors }},
		{"work_item", map[string]any{"id": "ENG-1"},
			func(w *stubWork, _ *stubPages) []statelog.Position { return w.taskFresh.Floors }},
		{"work_comments", map[string]any{"item": "ENG-1"},
			func(w *stubWork, _ *stubPages) []statelog.Position { return w.taskFresh.Floors }},
		{"work_item_turns", map[string]any{"id": "ENG-1"},
			func(w *stubWork, _ *stubPages) []statelog.Position { return w.taskFresh.Floors }},
		{"work_views", map[string]any{"container": "workspace"},
			func(w *stubWork, _ *stubPages) []statelog.Position { return w.views.Floors }},
		{"work_catalogue", nil, func(w *stubWork, _ *stubPages) []statelog.Position { return w.catalogueQuery.Floors }},
		{"work_person", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) []statelog.Position { return w.personQuery.Floors }},
		{"work_projects", nil, func(w *stubWork, _ *stubPages) []statelog.Position { return w.projectQuery.Floors }},
		{"work_project", map[string]any{"key": "ENG"},
			func(w *stubWork, _ *stubPages) []statelog.Position { return w.detailQuery.Floors }},
		{"work_activity", map[string]any{"container": "workspace"},
			func(w *stubWork, _ *stubPages) []statelog.Position { return w.activityQuery.Floors }},
		{"work_my_work", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) []statelog.Position { return w.myWorkQuery.Floors }},
		{"work_inbox", map[string]any{"handle": "ana"},
			func(w *stubWork, _ *stubPages) []statelog.Position { return w.inboxQuery.Floors }},
		{"work_routing", map[string]any{"record_id": "r-1"},
			func(w *stubWork, _ *stubPages) []statelog.Position { return w.routingQuery.Floors }},
		{"pages", nil, func(_ *stubWork, p *stubPages) []statelog.Position { return p.fresh.Floors }},
		{"page", map[string]any{"id": "p1"},
			func(_ *stubWork, p *stubPages) []statelog.Position { return p.fresh.Floors }},
		{"containers", nil, func(_ *stubWork, p *stubPages) []statelog.Position { return p.fresh.Floors }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			ask := askNative
			if personalQuestions[tc.what] {
				ask = askAsOperator
			}
			for _, level := range []string{"", "linearizable", "session", "stale", "consistent_prefix"} {
				for _, named := range []struct {
					value any
					want  []statelog.Position
				}{
					{want.String(), []statelog.Position{want}},
					{[]any{want.String(), other.String()}, []statelog.Position{want, other}},
				} {
					work, pages := &stubWork{}, &stubPages{}
					src := queries.Sources{Work: work, Pages: pages}
					args := map[string]any{"min_position": named.value}
					if level != "" {
						args["read_level"] = level
					}
					for k, v := range tc.args {
						args[k] = v
					}
					if _, err := ask(t, src, tc.what, args); err != nil {
						t.Fatalf("%s at read_level=%q with floors %v: %v", tc.what, level,
							named.value, err)
					}
					if got := tc.floors(work, pages); !slices.Equal(got, named.want) {
						t.Errorf("%s at read_level=%q carried the floors as %v, want %v "+
							"— a floor the reader never sees is one nothing waits for",
							tc.what, level, got, named.want)
					}
				}
			}
			// AND A MALFORMED ONE IS REFUSED AS BAD PARAMETERS, naming
			// the key, rather than silently read as no floor.
			work, pages := &stubWork{}, &stubPages{}
			src := queries.Sources{Work: work, Pages: pages}
			args := map[string]any{"min_position": "4711"}
			for k, v := range tc.args {
				args[k] = v
			}
			if _, err := ask(t, src, tc.what, args); !errors.Is(err, queries.ErrBadParams) ||
				!strings.Contains(err.Error(), "min_position") {
				t.Errorf("%s took min_position=4711, answering %v", tc.what, err)
			}
		})
	}
}

// THE VIEWER IS THE CALLER, AND NOT A PARAMETER AT ALL.
//
// It was one, and `workItems` read it out of the bag and deleted the key again
// before handing the rest to `ExpandedQuery` — because `viewer` is not in
// `tracker.QueryKeys` and `checkKeys` refuses a key nothing parses. Which left
// exactly one honest value for the parameter and made every other one a free
// choice of whose queue to render, on a surface where `preset=my_queue`,
// `preset=priorities` and `f.<slug>=me` all resolve against it.
//
// It is the principal's own seat now. What this case keeps is the half that
// still has to hold: the value reaches the EXPANSION as a viewer, because a
// fix that dropped the parameter without threading the caller's seat would
// make `me` and every personal preset resolve to nobody — a quieter wrong
// answer than the refusal it replaced.
func TestTheViewerIsTheCallersOwnSeat(t *testing.T) {
	w := &stubWork{}
	if _, err := askAsSeat(t, queries.Sources{Work: w}, "ada", "work_items",
		map[string]any{"assignee": "ada"}); err != nil {

		t.Fatalf("work_items: %v", err)
	}
	if got := w.query.Assignee; len(got) != 1 || got[0] != "ada" {
		t.Errorf("assignee = %v, want [ada]", got)
	}
	if w.expandViewer.Handle != "ada" {
		t.Errorf("the viewer reached the expansion as %q, want the caller's "+
			"own seat — `me` and every personal preset resolve from it",
			w.expandViewer.Handle)
	}

	// AND A CALLER WHO NAMES ONE IS REFUSED, which is the parameter being
	// gone rather than ignored: a key nothing parses widens an answer
	// silently, so the grammar refuses it.
	if _, err := askAsSeat(t, queries.Sources{Work: &stubWork{}}, "ada",
		"work_items", map[string]any{"viewer": "someone.else"}); err == nil {

		t.Error("a caller named a viewer and was answered")
	}
}

// A CHANGE KIND OFF THE WIRE IS CHECKED THE SAME WAY.
//
// It was trusted — [tracker.ChangeKind] over whatever string arrived — on the
// reasoning that an empty feed was survivable. It is the same quiet wrong
// answer: the history screen takes its kind from a URL a person can bookmark,
// so a kind this build does not have drew an empty history that reads exactly
// like a project nothing happened to, where the page activity beside it
// refuses the same mistake by name.
func TestAnUnknownChangeKindIsRefusedRatherThanFilteringToNothing(t *testing.T) {
	t.Parallel()
	w := &stubWork{}
	_, err := askNative(t, queries.Sources{Work: w}, "work_activity", map[string]any{
		"container": "workspace", "kinds": "status,promoted",
	})
	if !errors.Is(err, queries.ErrBadParams) || !strings.Contains(err.Error(), "promoted") {
		t.Fatalf("kinds=status,promoted answered %v, want a bad-parameter refusal "+
			"naming the kind", err)
	}
	if w.activityQuery.Kinds != nil {
		t.Errorf("the refused query still reached the reader as %v",
			w.activityQuery.Kinds)
	}

	// THE CONTROL: every kind this build has goes through as itself.
	w = &stubWork{}
	all := make([]string, 0, len(tracker.ChangeKinds))
	for _, kind := range tracker.ChangeKinds {
		all = append(all, string(kind))
	}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_activity", map[string]any{
		"container": "workspace", "kinds": strings.Join(all, ","),
	}); err != nil {
		t.Fatalf("every change kind this build has was refused: %v", err)
	}
	if !slices.Equal(w.activityQuery.Kinds, tracker.ChangeKinds) {
		t.Errorf("the kinds reached the reader as %v, want %v",
			w.activityQuery.Kinds, tracker.ChangeKinds)
	}
}

// AN ACTOR KIND OFF THE WIRE IS CHECKED AT THIS SURFACE, and it is the only
// place that can check it.
//
// An unknown kind simply matches nothing, which is a filter that answers
// empty — and the ACTOR kind is the filter the audit screen is made of: an
// empty audit reads as a company nobody has touched, and a filter quietly
// ignored reads as one where everybody is an operator. The reader takes typed
// values and cannot tell a kind the caller invented from one this build was
// compiled without, so the refusal belongs here, at the edge where the string
// still exists.
func TestAnUnknownActorKindIsRefusedRatherThanFilteringToNothing(t *testing.T) {
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_activity", map[string]any{
		"container": "workspace", "actor_kinds": "root",
	}); err == nil {
		t.Fatal("work_activity accepted actor_kinds=root — nothing is written " +
			"under it, so the feed answers empty and the screen reads as a " +
			"company nobody has touched")
	}
	if got := w.activityQuery.ActorKinds; len(got) != 0 {
		t.Errorf("the refused query still reached the reader as %v", got)
	}

	// AND THE KINDS THE ENGINE DOES WRITE UNDER ALL ARRIVE, in the order
	// they were named — a gate that refused everything would pass the case
	// above and take the screen with it.
	if _, err := askNative(t, queries.Sources{Work: w}, "work_activity", map[string]any{
		"container": "workspace", "actor_kinds": "operator, human",
	}); err != nil {
		t.Fatalf("work_activity naming two real actor kinds: %v", err)
	}
	want := []tracker.AuthorKind{tracker.AuthorOperator, tracker.AuthorHuman}
	if got := w.activityQuery.ActorKinds; !slices.Equal(got, want) {
		t.Errorf("the reader was asked for %v, want %v", got, want)
	}
}

// EVERY SAVED VIEW, ACROSS THE CONTAINERS, takes the strip's rules: whose
// views and pins is the CALLER's own record, never a name they type, and a
// count needs a record to hold pins — so it reaches the reader with the
// company's clock for a caller with one and is refused by name for a caller
// the engine can name no record for.
func TestEverySavedViewIsTheCallersAndCountedOnlyForARecord(t *testing.T) {
	t.Parallel()
	w := &stubWork{}
	if _, err := askAsSeat(t, queries.Sources{Work: w}, "ada-okonkwo",
		"work_saved_views", map[string]any{"counts": "true"}); err != nil {
		t.Fatalf("every view, counted, for the caller: %v", err)
	}
	if w.every.Viewer != "ada-okonkwo" || !w.every.Counts || w.every.Now.IsZero() ||
		w.every.Zone == nil {
		t.Errorf("the reader was asked %+v — want the caller's own record, counts "+
			"on, an instant and the company's clock", w.every)
	}

	// `viewer=` IS NOT A PARAMETER: whose views is never a typed name.
	w = &stubWork{}
	if _, err := askAsSeat(t, queries.Sources{Work: w}, "ada-okonkwo",
		"work_saved_views", map[string]any{"viewer": "somebody-else"}); err != nil {
		t.Fatalf("every view with a stray viewer=: %v", err)
	}
	if w.every.Viewer != "ada-okonkwo" {
		t.Errorf("a typed viewer= chose whose views to read: %q", w.every.Viewer)
	}

	w = &stubWork{}
	nobody := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantTracker}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Work: w})
	if _, err := r.Answer(iam.WithPrincipal(t.Context(), nobody), "work_saved_views",
		map[string]any{"counts": "true"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("counts for a caller with no record answered %v, want a "+
			"bad-params refusal", err)
	}
	if w.every.Counts {
		t.Error("the refused counts reached the reader")
	}
}

// AN ASKED PERSON READS THE ITEM WITHOUT THE TRACKER, and nobody else does
// (ADR-0033, `get_work_item`'s asked arm). A person kept off the tracker may
// read the one item an OPEN ask on it names them on — so they can read what
// they were asked and answer it — and only the item says whether one does: it
// is read, the ask resolved through the same thread read the answer they
// would post is resolved by, and the item decided again. A tracker holder is
// decided before anything is read, and never asks the thread.
//
// A MISSING ITEM IS THE SAME REFUSAL as one they were not asked on, to
// somebody without the tracker, so the refusal is no oracle for which keys
// exist; and a thread this node cannot read is unavailable, never "you were not
// asked".
//
// Mutation: answer the detail on the first ErrUnresolved without the probe, and
// bo reads ana's item; return ErrNotFound for a missing item before deciding,
// and the two refusals differ.
func TestAnAskedPersonReadsTheItemWithoutTheTracker(t *testing.T) {
	t.Parallel()
	item := tracker.TaskDetail{Task: tracker.Task{ID: "task-1", Key: "ENG-1", Project: "ENG"}}
	ask := func(work *stubWork, ctx context.Context) (any, error) {
		t.Helper()
		r := queries.NewRegistry()
		queries.Register(r, queries.Sources{Work: work, Chart: flatChart{}})
		return r.Answer(ctx, "work_item", map[string]any{"id": "ENG-1"})
	}
	refusalOf := func(err error) queries.Refusal {
		t.Helper()
		var refusal *queries.Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("answered %v, want a refusal", err)
		}
		return *refusal
	}

	// A TRACKER HOLDER, decided before the item is read.
	holder := &stubWork{detail: item, asks: map[string]string{}}
	if _, err := ask(holder, asSeat(t, "bo", iam.GrantTracker)); err != nil {
		t.Fatalf("a tracker holder was refused: %v", err)
	}
	if len(holder.threadAsked) != 0 {
		t.Errorf("a tracker holder's read asked the thread %v", holder.threadAsked)
	}

	// THE PERSON THE OPEN ASK NAMES, holding nothing.
	asked := &stubWork{detail: item, asks: map[string]string{"ana": "c-7"}}
	got, err := ask(asked, asSeat(t, "ana"))
	if err != nil {
		t.Fatalf("the person asked was refused the item: %v", err)
	}
	if detail, _ := got.(tracker.TaskDetail); detail.Task.Key != "ENG-1" {
		t.Errorf("the person asked was answered %v, want the item", got)
	}
	if len(asked.threadAsked) != 1 || asked.threadAsked[0].Task != "task-1" ||
		asked.threadAsked[0].Author != "ana" {
		t.Errorf("the thread was asked %v, want task-1 by ana", asked.threadAsked)
	}

	// SOMEBODY ELSE, holding nothing, and a missing item: one refusal.
	notAsked := refusalOf(func() error {
		_, err := ask(&stubWork{detail: item, asks: map[string]string{"ana": "c-7"}},
			asSeat(t, "bo"))
		return err
	}())
	missing := refusalOf(func() error {
		_, err := ask(&stubWork{err: tracker.ErrNoTask}, asSeat(t, "bo"))
		return err
	}())
	if !slices.Equal(notAsked.Grants, []iam.Grant{iam.GrantTracker}) {
		t.Errorf("a person not asked was refused naming %v, want [tracker]", notAsked.Grants)
	}
	if missing.What != notAsked.What || missing.Reason != notAsked.Reason ||
		!slices.Equal(missing.Grants, notAsked.Grants) {
		t.Errorf("a missing item was refused %+v and an item not asked on %+v — "+
			"the difference says which keys exist", missing, notAsked)
	}
	if _, err := ask(&stubWork{err: tracker.ErrNoTask}, asSeat(t, "bo", iam.GrantTracker)); !errors.Is(
		err, queries.ErrNotFound) {
		t.Errorf("a missing item asked by a tracker holder = %v, want not found", err)
	}

	// A THREAD THIS NODE CANNOT READ says nothing about who was asked.
	blind := &stubWork{detail: item, threadErr: errors.New("the tracker's applier is behind")}
	if _, err := ask(blind, asSeat(t, "ana")); !errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("an unreadable thread = %v, want unavailable", err)
	}
}
