package queries_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/tracker"
)

// memorySandbox is a pending-run store with nothing in it: this sweep is
// about which names exist, not what they answer.
type memorySandbox struct{}

func (memorySandbox) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return nil, nil
}

// roomQueries is every query kind a room asks for, mapped to the files that
// ask — found in the rooms' own source, never a list kept here: a
// hand-maintained one is exactly what drifts, and it would drift towards
// claiming the server answers more than it does.
//
// BOTH CALL SHAPES: the `useQuery` hook a screen renders from, and the direct
// `socket.query` a pager or an action uses. Read by [clientsource.Calls], so a
// call a formatter wrapped across lines, one with type arguments and one
// inside markup are the calls they are. The sweep this replaced was a regular
// expression whose name class once could not match `a2a_channels` and whose
// paren once could not be followed by a line break — a sweep whose whole job
// is to notice a missing name, twice silently narrowed by the shape of a
// pattern — and it keyed each hit on the file's BASE name, so two rooms named
// alike collapsed into one. `internal/api/queries` is one level deeper than
// the directory [clientsource.Tree] is written against, so it joins the extra
// step itself.
//
// A kind handed over in anything but a string literal is invisible here —
// [clientsource.Calls] can read nothing else — and that is safe only because
// the dashboard refuses one: `app/source.test.ts`'s "every read names its
// question" requires a literal at every `useQuery(` and `query(` call, bare or
// as a method, which is every call Calls is asked for below.
func roomQueries(t *testing.T) map[string][]string {
	t.Helper()
	calls, err := clientsource.Calls("../"+clientsource.Tree, "useQuery", "query")
	if err != nil {
		// FAILS rather than skips. The dashboard source is committed, so it
		// is always in a checkout — and a skip here is indistinguishable
		// from a pass, which is how this gate went quiet the last time the
		// tree moved: it pointed at `static/dashboard/js`, the hand-written
		// bundle the React rewrite deleted, and certified nothing for the
		// whole of that rewrite.
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Fatal("the sweep found no query calls at all, so it certifies nothing")
	}
	return calls
}

// everySeam is a Sources with every seam present, which is what makes the
// registry list everything this build can answer.
//
// The seams are only tested for nil by Register, so zero values are enough
// and nothing here is called — this sweep is about WHICH NAMES exist, not
// what they answer. The per-kind tests in this package cover the answers.
func everySeam(t *testing.T) queries.Sources {
	t.Helper()
	surface, _ := configSurface(t, companyDoc)
	cfg := company(t)
	return queries.Sources{
		State:    &livestate.LiveState{},
		Events:   eventfan.Solo("node-a", &store.EventLog{}),
		Spend:    &store.EventLog{},
		Health:   func(context.Context) any { return nil },
		Company:  func() *config.Company { return cfg },
		Coord:    coordmemory.New(),
		Plane:    coordmemory.NewFleet(),
		Runs:     &fakeRuns{},
		Diary:    &learning.Diary{},
		Episodes: &learning.Episodes{},
		Skills:   &learning.Skills{},
		Channels: fakeChannels{},
		Budget:   coordmemory.NewFleet(),
		Sandbox:  memorySandbox{},
		Config:   surface,
		Work:     emptyWork{},
		Pages:    emptyPages{},
		// THE SEARCH INDEX IS ITS OWN SEAM, so a node with a board and
		// no index is a real shape this sweep can describe.
		WorkSearch:     emptyWork{},
		Conversations:  emptyConversations{},
		Counterparties: emptyCounterparties{},
		// THE RETENTION DOCUMENT, which the Fleet screen's replication
		// panels read. A pass-through on the real surface, so the seam is
		// a function rather than a reader — and this sweep is about which
		// names exist, so what it answers is nothing.
		Retention: func(context.Context) any { return nil },
	}
}

// emptyWork and emptyPages are the native readers with nothing in them, on
// fakeChannels' terms: this sweep is about which NAMES exist.
type emptyWork struct{}

func (emptyWork) Tasks(context.Context, tracker.Query, time.Time) (tracker.Answer, error) {
	return tracker.Answer{}, nil
}

func (emptyWork) Task(context.Context, string, tracker.DetailWants,
	statelog.Freshness) (tracker.TaskDetail, error) {

	return tracker.TaskDetail{}, nil
}

func (emptyWork) Views(context.Context, tracker.ViewQuery) (tracker.ViewListing, error) {
	return tracker.ViewListing{}, nil
}

func (emptyWork) ExpandedQuery(_ context.Context, params map[string]any,
	_ tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {

	return tracker.ParseQuery(tracker.MapParams(params), now, loc)
}

func (emptyWork) Catalogue(context.Context, tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
	return tracker.CatalogueAnswer{}, nil
}

func (emptyWork) Projects(context.Context, tracker.ProjectQuery) (
	tracker.ProjectListing, error) {

	return tracker.ProjectListing{}, nil
}

func (emptyWork) Project(context.Context, tracker.ProjectDetailQuery) (
	tracker.ProjectDetail, error) {

	return tracker.ProjectDetail{}, nil
}

func (emptyWork) Workload(context.Context, tracker.WorkloadQuery, time.Time,
	*time.Location) (tracker.WorkloadAnswer, error) {

	return tracker.WorkloadAnswer{}, nil
}

func (emptyWork) Activity(context.Context, tracker.ActivityQuery, time.Time) (
	tracker.ActivityAnswer, error) {

	return tracker.ActivityAnswer{}, nil
}

func (emptyWork) MyWork(context.Context, tracker.MyWorkQuery, time.Time,
	*time.Location) (tracker.MyWork, error) {

	return tracker.MyWork{}, nil
}

func (emptyWork) Person(context.Context, tracker.PersonQuery, time.Time) (tracker.PersonState, error) {
	return tracker.PersonState{}, nil
}

func (emptyWork) Inbox(context.Context, tracker.InboxQuery, time.Time) (
	tracker.InboxAnswer, error) {
	return tracker.InboxAnswer{}, nil
}

func (emptyWork) Routing(context.Context, tracker.RoutingQuery, time.Time) (
	tracker.RoutingAnswer, error) {
	return tracker.RoutingAnswer{}, nil
}

func (emptyWork) Search(context.Context, string, int) ([]tracker.Ranked, error) {
	return nil, nil
}

// emptyConversations and emptyCounterparties are the two per-seat stores with
// nothing in them, on emptyWork's terms.
type emptyConversations struct{}

func (emptyConversations) Threads(context.Context, string, int) ([]ledgerstore.Thread, error) {
	return nil, nil
}

func (emptyConversations) History(context.Context, string, string, int) ([]ledger.Session, error) {
	return nil, nil
}

type emptyCounterparties struct{}

func (emptyCounterparties) List(context.Context, string) ([]learning.Profile, error) {
	return nil, nil
}

type emptyPages struct{}

func (emptyPages) List(context.Context, pages.Filter, statelog.Freshness) (pages.Listing, error) {
	return pages.Listing{}, nil
}

func (emptyPages) Get(context.Context, string, statelog.Freshness) (pages.Detail, error) {
	return pages.Detail{}, nil
}

func (emptyPages) Activity(context.Context, pages.PageActivityQuery) (pages.PageActivity, error) {
	return pages.PageActivity{}, nil
}

func (emptyPages) Revision(context.Context, string, int, statelog.Freshness) (
	pages.Revision, bool, error) {
	return pages.Revision{}, false, nil
}

func (emptyPages) Containers(context.Context, statelog.Freshness) ([]pages.ContainerListing, error) {
	return nil, nil
}

// fakeChannels is an A2A channel reader with nothing in it: this sweep is
// about which names exist, not what they answer.
type fakeChannels struct{}

func (fakeChannels) OpenChannels(context.Context) ([]coord.Channel, error) { return nil, nil }
func (fakeChannels) AllChannels(context.Context) ([]coord.Channel, error)  { return nil, nil }

// EVERY QUERY A ROOM MAKES IS A QUERY THIS SERVER ANSWERS.
//
// Nothing linked the two, and the cost was a whole feature: the Config
// room's entity editor listed a collection with query("config_entities"),
// opened one with {kind, id}, and no answer was ever registered under that
// name — so every list came back unknown_query and the editor was dead from
// the day it shipped. Both sides' tests passed, because each was written
// against its own idea of the other.
//
// This is the cheap half of the gate internal/e2e gives the push protocol,
// and it is deliberately about NAMES rather than fields: a name is checkable
// without standing up a node, and a name that nothing answers is the failure
// that renders as an empty room with no error anywhere.
func TestEveryQueryARoomMakesIsAnswered(t *testing.T) {
	t.Parallel()
	answered := registeredKinds(t)
	for kind, rooms := range roomQueries(t) {
		if !slices.Contains(answered, kind) {
			t.Errorf("%s asks for %q and nothing answers it, so the room renders "+
				"empty with no error anywhere", strings.Join(rooms, " and "), kind)
		}
	}
}

// registeredKinds is every name this build answers, from a Sources with
// every seam present.
//
// The NAMES only: Register gates each kind on its seam being non-nil, so
// this is the complete surface — and nothing here is invoked, because the
// question is which names exist rather than what they answer. The per-kind
// tests in this package cover the answers.
func registeredKinds(t *testing.T) []string {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, everySeam(t))
	names := r.Names()
	if len(names) == 0 {
		t.Fatal("a Sources with every seam registered nothing, so this sweep " +
			"certifies nothing")
	}
	return names
}

// AND EVERY QUERY THIS SERVER ANSWERS IS ONE SOMETHING ASKS FOR.
//
// The other direction, and the one that goes quiet rather than breaking: an
// answer nobody calls is code with tests, no readers, and no way to notice
// it stopped being right.
//
// NO EXCEPTIONS. This carried a map of kinds "read by name from somewhere that
// is not a room" — `stream` through a header poll in a file the React rewrite
// deleted, `config_entities` through a guide — and both had long since gained
// a room that reads them, so the map was consulted for nothing and excused
// nothing. An exemption nobody reaches is how the next unread answer gets
// waved through with a reason that stopped being true; a kind that genuinely
// has no room reader is a decision this gate should make someone take in the
// open.
func TestEveryQueryThisServerAnswersHasAReader(t *testing.T) {
	t.Parallel()
	asked := roomQueries(t)
	for _, kind := range registeredKinds(t) {
		if _, ok := asked[kind]; !ok {
			t.Errorf("this build answers %q and no room asks for it — either a "+
				"reader was lost, or the answer should go with whatever used to "+
				"call it", kind)
		}
	}
}

// AND EVERY WAKE REASON HAS ENGLISH ON THE OTHER SIDE.
//
// The applier records, per change and per recipient, the ONE reason of eighteen
// under which that person heard about it — the fact no commercial tracker
// keeps. It reaches a screen through `work_inbox`, and a reason the client has
// no phrase for renders as its own snake_case value: a log line where a
// sentence belongs, on the surface a person reads first.
//
// This is the `rooms` idiom one level down: the client's table is read from
// ITS OWN SOURCE rather than restated here, so the gate cannot drift towards
// claiming the pair agree. A phrase the client carries for a reason nothing
// writes is checked too — that is how a renamed reason leaves a dead entry
// behind and a live one missing.
func TestEveryWakeReasonReadsAsEnglishOnTheClient(t *testing.T) {
	t.Parallel()
	// FOUND RATHER THAN ADDRESSED, and read by its syntax: the table's KEYS
	// are the reasons it phrases, whatever the layout. The pattern this
	// replaced wanted each key at exactly two spaces of indent, so a key a
	// formatter moved, or one that had to be quoted, was a reason read as
	// unphrased; and the private walk it went through matched once per FILE,
	// so a second PHRASES table beside the first — the one a screen might
	// actually render — passed unseen.
	table, err := clientsource.Literal("../"+clientsource.Tree, "PHRASES")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(table)
	if err != nil {
		t.Fatal(err)
	}
	phrased := map[string]bool{}
	for _, key := range keys {
		phrased[key] = true
	}
	if len(phrased) == 0 {
		t.Fatal("no phrases were found at all, so this gate certifies nothing")
	}
	for _, reason := range tracker.Reasons {
		if !phrased[string(reason)] {
			t.Errorf("the engine writes %q and the client has no phrase for it, "+
				"so it renders as its own snake_case value on the one screen a "+
				"person reads first", reason)
		}
		delete(phrased, string(reason))
	}
	for leftover := range phrased {
		t.Errorf("the client phrases %q and nothing writes it — a renamed reason "+
			"leaves exactly this behind", leftover)
	}
}

// AND EVERY DIMENSION THE COST AXIS OFFERS IS ONE THE ENGINE ACCEPTS.
//
// `token_series` refuses an unknown `group` naming what it takes, which is the
// right refusal — and it turns a control offering a seventh value into a chart
// that never loads rather than one drawn on the wrong dimension. The screen's
// list and [tokens.Groups] are therefore one closed set written twice, and
// this is the gate that says so.
//
// The client's table is read from ITS OWN SOURCE rather than restated here,
// the `rooms` idiom: a gate carrying its own copy of the list drifts towards
// claiming the pair agree. A value the client offers and the engine dropped is
// checked too — that is how a renamed group leaves a dead control behind.
func TestEveryCostDimensionTheScreenOffersIsOneTheEngineAccepts(t *testing.T) {
	t.Parallel()
	// THE `GROUPS` TABLE ITSELF, not every `{value, label}` pair on the
	// screen: the compare control is the same shape one line away, and a
	// sweep over the whole file read its "previous" as a seventh dimension.
	//
	// FOUND RATHER THAN ADDRESSED. This read the screen at
	// `routes/Spend.tsx` and went red the day the screens were grouped by
	// workspace, reporting a drift between two lists that had not changed.
	// A gate over a constant is a gate over the constant, and the file it
	// happens to sit in is not the subject.
	block, err := clientsource.Literal("../"+clientsource.Tree, "GROUPS")
	if err != nil {
		t.Fatal(err)
	}
	offered := map[string]bool{}
	for _, value := range clientsource.Field(block, "value") {
		offered[value] = true
	}
	if len(offered) == 0 {
		t.Fatal("no dimensions were found at all, so this gate certifies nothing")
	}
	for _, group := range tokens.Groups {
		if !offered[string(group)] {
			t.Errorf("the engine buckets by %q and the screen does not offer it, "+
				"so a dimension the company can be read on is unreachable", group)
		}
		delete(offered, string(group))
	}
	for leftover := range offered {
		t.Errorf("the screen offers %q and the engine refuses it, so picking it "+
			"draws no chart at all — a renamed group leaves exactly this behind",
			leftover)
	}
}
