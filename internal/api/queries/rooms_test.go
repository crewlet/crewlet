package queries_test

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/worklet"
)

// memorySandbox is a pending-run store with nothing in it: this sweep is
// about which names exist, not what they answer.
type memorySandbox struct{}

func (memorySandbox) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return nil, nil
}

func (memorySandbox) Get(context.Context, string) (sandbox.PendingRun, bool, error) {
	return sandbox.PendingRun{}, false, nil
}

// roomQueries is every query kind a room asks for, mapped to the files that
// ask — found in the rooms' own source, never a list kept here: a
// hand-maintained one is exactly what drifts, and it would drift towards
// claiming the server answers more than it does.
//
// EVERY CALL SHAPE: the `useQuery` hook a screen renders from, the
// `useSearchQuery` hook every ranked search is asked through (it applies the
// search bound before `useQuery`), and the direct `socket.query` a pager or an
// action uses. Read by [clientsource.Calls], so a
// call a formatter wrapped across lines, one with type arguments and one
// inside markup are the calls they are. The sweep this replaced was a regular
// expression whose name class once could not match `a2a_channels` and whose
// paren once could not be followed by a line break — a sweep whose whole job
// is to notice a missing name, twice silently narrowed by the shape of a
// pattern — and it keyed each hit on the file's BASE name, so two rooms named
// alike collapsed into one.
//
// A kind handed over in anything but a string literal is invisible here —
// [clientsource.Calls] can read nothing else — and that is safe only because
// the dashboard refuses one: `app/source.test.ts`'s "every read names its
// question" requires a literal at every `useQuery(`, `useSearchQuery(` and
// `query(` call, bare or as a method, which is every call Calls is asked for
// below.
func roomQueries(t *testing.T) map[string][]string {
	t.Helper()
	calls, err := clientsource.Calls(clientsource.Tree(t), "useQuery", "useSearchQuery", "query")
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
		Usage:    &store.DB{},
		Company:  companySource(t, cfg),
		Coord:    coordmemory.New(),
		Plane:    coordmemory.NewFleet(),
		Runs:     &fakeRuns{},
		Memory:   &memread.Reader{},
		Channels: fakeChannels{},
		Budget:   coordmemory.NewFleet(),
		Sandbox:  memorySandbox{},
		// A RUNNING RUN'S LIVE OUTPUT: the real reader over an empty
		// fleet record, which answers every run `not_running`.
		ExecutorTail: &sandbox.TailReader{Pending: sandbox.NewCoordStore(coordmemory.NewFleet()),
			Condition: func(err error) bool { _, ok := builtin.Condition(err); return ok }},
		Config: surface,
		Work:   emptyWork{},
		Pages:  emptyPages{},
		// THE SEARCH INDEX IS ITS OWN SEAM, so a node with a board and
		// no index is a real shape this sweep can describe.
		WorkSearch: emptyWork{},
		// A PROJECT'S FILES, their own seam for the same reason.
		Files: emptyFiles{},
		// THE RETENTION DOCUMENT, which Settings › Nodes' replication
		// panels read. A pass-through on the real surface, so the seam is
		// a function rather than a reader — and this sweep is about which
		// names exist, so what it answers is nothing.
		Retention: func(context.Context) any { return nil },
		// THE FLEET BROKER'S MEMBERSHIP, which every engine serves.
		FleetBroker: emptyBroker{},
		// THE FLEET'S BACKUP REGISTER, which every node opens.
		Backups: register{},
		// THE ARTIFACTS AND THEIR DATASTORES, each its own seam because a
		// node is wired with them only where the feature is deployed.
		Artifacts:    &queries.ArtifactsSource{},
		ArtifactData: &queries.ArtifactDataSource{},
		// THIS NODE'S SIGN-IN POSTURE, which internal/api fills from the
		// Bootstrap on every node that serves the API.
		SignIn: &queries.SignInPolicy{},
		// THE COMPANY'S OWN CONVERSATION. Three seams rather than one:
		// a search is asked for a viewer rather than over rooms, and the
		// badges are coordination's rather than the log's — so a surface
		// with the rooms and neither of the others is a real shape, and
		// `chat_search` is gated on its own seam.
		Chat:       emptyChat{},
		ChatSearch: emptyChat{},
		ChatReads:  coordmemory.NewFleet(),
		// THE MODEL QUESTIONS, each its own seam: what this node resolved
		// for each model, the fleet's device sign-ins, and the catalogue a
		// model step suggests from. Every engine that serves the API wires
		// all three, and Settings › Models reads each. The fakes are
		// models_test.go's, empty: this sweep is about which names exist.
		Models:        fakeModels{},
		DeviceSignIns: fakeDevices{},
		Catalogue:     fakeCatalogue{},
	}
}

// emptyBroker is a fleet broker with nothing to list, on emptyWork's terms.
type emptyBroker struct{}

func (emptyBroker) List(context.Context) (engine.BrokerView, error) {
	return engine.BrokerView{}, nil
}

// emptyFiles is a project with no files.
type emptyFiles struct{}

func (emptyFiles) Files(context.Context, worklet.FileQuery) (worklet.FileListing, error) {
	return worklet.FileListing{}, nil
}

// emptyChat is the chat reader and its keyword index with nothing in them, on
// emptyWork's terms: this sweep is about which NAMES exist.
type emptyChat struct{ noReaderViews }

func (emptyChat) Channels(context.Context, string, chatlet.ChannelsQuery,
	statelog.Freshness) (chatlet.ChannelListing, error) {

	return chatlet.ChannelListing{}, nil
}

func (emptyChat) Channel(context.Context, string, string,
	statelog.Freshness) (chatlet.ChannelDetail, error) {

	return chatlet.ChannelDetail{}, nil
}

func (emptyChat) ChannelNamed(context.Context, string, string,
	statelog.Freshness) (chatlet.ChannelDetail, error) {

	return chatlet.ChannelDetail{}, nil
}

func (emptyChat) Messages(context.Context, string, chatlet.TranscriptQuery,
	statelog.Freshness) (chatlet.Transcript, error) {

	return chatlet.Transcript{}, nil
}

func (emptyChat) Thread(context.Context, string, chatlet.ThreadQuery,
	statelog.Freshness) (chatlet.Thread, error) {

	return chatlet.Thread{}, nil
}

func (emptyChat) RoomNames(context.Context, string,
	statelog.Freshness) (chatlet.RoomNames, error) {

	return chatlet.RoomNames{}, nil
}

func (emptyChat) Directory(context.Context, string,
	statelog.Freshness) (chatlet.Directory, error) {

	return chatlet.Directory{}, nil
}

func (emptyChat) Search(context.Context, estate.ChatQuery) (estate.ChatSearchAnswer, error) {
	return estate.ChatSearchAnswer{Hits: []search.ChatHit{}}, nil
}

// emptyWork and emptyPages are the native readers with nothing in them, on
// fakeChannels' terms: this sweep is about which NAMES exist.
type emptyWork struct{}

func (emptyWork) Tasks(context.Context, worklet.Query, time.Time) (worklet.Answer, error) {
	return worklet.Answer{}, nil
}

func (emptyWork) Task(context.Context, string, worklet.DetailWants,
	statelog.Freshness) (worklet.TaskDetail, error) {

	return worklet.TaskDetail{}, nil
}

func (emptyWork) TurnsOf(context.Context, string, string, int,
	statelog.Freshness) (worklet.TaskTurns, error) {

	return worklet.TaskTurns{Turns: []worklet.TaskTurn{}}, nil
}

func (emptyWork) EveryView(context.Context, worklet.EveryViewQuery) (worklet.ViewListing, error) {
	return worklet.ViewListing{}, nil
}

func (emptyWork) Views(context.Context, worklet.ViewQuery) (worklet.ViewListing, error) {
	return worklet.ViewListing{}, nil
}

func (emptyWork) ExpandedQuery(_ context.Context, params map[string]any,
	_ worklet.Viewer, now time.Time, loc *time.Location) (worklet.Query, error) {

	return worklet.ParseQuery(worklet.MapParams(params), now, loc)
}

func (emptyWork) Catalogue(context.Context, worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
	return worklet.CatalogueAnswer{}, nil
}

func (emptyWork) Projects(context.Context, worklet.ProjectQuery) (
	worklet.ProjectListing, error) {

	return worklet.ProjectListing{}, nil
}

func (emptyWork) Project(context.Context, worklet.ProjectDetailQuery) (
	worklet.ProjectDetail, error) {

	return worklet.ProjectDetail{}, nil
}

func (emptyWork) Workload(context.Context, worklet.WorkloadQuery, time.Time,
	*time.Location) (worklet.WorkloadAnswer, error) {

	return worklet.WorkloadAnswer{}, nil
}

func (emptyWork) Activity(context.Context, worklet.ActivityQuery, time.Time) (
	worklet.ActivityAnswer, error) {

	return worklet.ActivityAnswer{}, nil
}

func (emptyWork) MyWork(context.Context, worklet.MyWorkQuery, time.Time,
	*time.Location) (worklet.MyWork, error) {

	return worklet.MyWork{}, nil
}

func (emptyWork) Person(context.Context, worklet.PersonQuery, time.Time) (worklet.PersonState, error) {
	return worklet.PersonState{}, nil
}

func (emptyWork) Inbox(context.Context, worklet.InboxQuery, time.Time) (
	worklet.InboxAnswer, error) {
	return worklet.InboxAnswer{}, nil
}

func (emptyWork) Routing(context.Context, worklet.RoutingQuery, time.Time) (
	worklet.RoutingAnswer, error) {
	return worklet.RoutingAnswer{}, nil
}

func (emptyWork) Flow(context.Context, worklet.FlowQuery, time.Time, *time.Location) (
	worklet.FlowAnswer, error) {
	return worklet.FlowAnswer{Points: []worklet.FlowPoint{}}, nil
}

func (emptyWork) CompanyFeed(context.Context, worklet.FeedQuery) (worklet.FeedPage, error) {
	return worklet.FeedPage{Rows: []worklet.FeedRow{}}, nil
}

func (emptyWork) Decisions(context.Context, worklet.DecisionsQuery, time.Time, *time.Location) (
	worklet.DecisionsAnswer, error) {
	return worklet.DecisionsAnswer{Asks: []worklet.AskRow{}}, nil
}

func (emptyWork) AnsweredAsks(context.Context, worklet.DecisionsQuery, time.Time, *time.Location) (
	worklet.AnsweredAnswer, error) {
	return worklet.AnsweredAnswer{Asks: []worklet.AnsweredAsk{}}, nil
}

func (emptyWork) TurnPlaces(context.Context, []string, statelog.Freshness) (
	map[string]worklet.TurnPlace, error) {
	return map[string]worklet.TurnPlace{}, nil
}

func (emptyWork) Search(context.Context, worklet.SearchQuery) (worklet.SearchAnswer, error) {
	return worklet.SearchAnswer{}, nil
}

func (emptyWork) Thread(context.Context, worklet.ThreadQuery, statelog.Freshness) (
	worklet.ResolvedThread, error) {
	return worklet.ResolvedThread{}, nil
}

type emptyPages struct{}

func (emptyPages) List(context.Context, doclet.Filter, statelog.Freshness) (doclet.Listing, error) {
	return doclet.Listing{}, nil
}

func (emptyPages) Get(context.Context, string, statelog.Freshness) (doclet.Detail, error) {
	return doclet.Detail{}, nil
}

func (emptyPages) Activity(context.Context, doclet.PageActivityQuery) (doclet.PageActivity, error) {
	return doclet.PageActivity{}, nil
}

func (emptyPages) Revision(context.Context, string, int, statelog.Freshness) (
	doclet.Revision, bool, error) {
	return doclet.Revision{}, false, nil
}

func (emptyPages) Containers(context.Context, statelog.Freshness) ([]doclet.ContainerListing, error) {
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

// AND THE SWEEP ITSELF CANNOT GO QUIET.
//
// Both gates above are only as wide as [everySeam]: Register tests each seam
// for nil, so a seam the helper does not set registers nothing, and every name
// behind it disappears from `registeredKinds` without either direction saying
// a word. That is not hypothetical — it is how the chat family arrived, gated
// on three seams this helper did not set, and it was caught only because six
// rooms happened to ask for those names. An operator-only family, or one read
// over REST from the docs, would have gone in silently and this sweep would
// have reported a pass over a surface it could not see.
//
// So the gating conditions are READ FROM Register ITSELF rather than restated
// here, on the reasoning the reader gate above takes when it reads the
// dashboard's own calls through clientsource: a list of seams kept by hand
// beside the one that matters is the second copy that drifts.
func TestEverySeamThatGatesAQuestionIsInTheSweep(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("answers.go")
	if err != nil {
		t.Fatalf("Register's own source could not be read, so this gate "+
			"certifies nothing: %v", err)
	}
	gates := regexp.MustCompile(`s\.([A-Z][A-Za-z0-9]*) != nil`).
		FindAllStringSubmatch(registerBody(t, string(source)), -1)
	if len(gates) == 0 {
		t.Fatal("Register gates nothing on a seam at all, which it has always " +
			"done — so this gate certifies nothing")
	}
	sweep := reflect.ValueOf(everySeam(t))
	seen := map[string]bool{}
	for _, gate := range gates {
		name := gate[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		field := sweep.FieldByName(name)
		if !field.IsValid() {
			t.Errorf("Register gates on Sources.%s and no such field exists — "+
				"this gate is reading a Register it does not understand", name)
			continue
		}
		if field.IsNil() {
			t.Errorf("Register gates every name behind Sources.%s on it being "+
				"non-nil and everySeam leaves it nil, so those names are absent "+
				"from this sweep and BOTH directions of it go quiet about them",
				name)
		}
	}
}

// registerBody is Register's own body, which is where a gate on a seam lives.
//
// BOUNDED AT THE NEXT TOP-LEVEL DECLARATION rather than read to the end of the
// file, because the answers below it test their seams for a different reason:
// a nil `Events` behind a seat's phase history is an empty list rather than an
// absent question, and reading those as gates would demand seams this sweep
// has no use for.
func registerBody(t *testing.T, source string) string {
	t.Helper()
	const decl = "\nfunc Register(r *Registry, s Sources) {"
	from := strings.Index(source, decl)
	if from < 0 {
		t.Fatal("Register's declaration was not found, so this gate certifies " +
			"nothing — it has been renamed or reshaped")
	}
	body := source[from+len(decl):]
	if end := strings.Index(body, "\nfunc "); end >= 0 {
		return body[:end]
	}
	return body
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
	table, err := clientsource.Literal(clientsource.Tree(t), "PHRASES")
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
	for _, reason := range worklet.Reasons {
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
	block, err := clientsource.Literal(clientsource.Tree(t), "GROUPS")
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

// THE SPLITS THE SCREEN KEEPS FROM A READER WHO IS NOT AN ADMIN ARE EXACTLY THE
// ONES THE ENGINE REFUSES THEM (tokens.AdminGroups). One the engine refuses and
// the screen still offers is a control that draws a refusal; one the screen
// hides and the engine answers is a dimension a reader cannot reach.
//
// Mutation: add a group to either list alone, and this fails.
func TestTheScreenOffersAnAdminsSplitsToAnAdminAlone(t *testing.T) {
	t.Parallel()
	block, err := clientsource.Literal(clientsource.Tree(t), "ADMIN_GROUPS")
	if err != nil {
		t.Fatal(err)
	}
	var engine []string
	for _, g := range tokens.AdminGroups {
		engine = append(engine, string(g))
	}
	if got := clientsource.Strings(block); !slices.Equal(got, engine) {
		t.Errorf("the screen keeps %v to an admin, the engine refuses %v to everybody else", got, engine)
	}
}

// AND THE CHART DRAWS EXACTLY THE ENGINE'S PHASE BANDS, IN ITS ORDER.
//
// `group=phase` answers four bands the engine folds every phase into ONCE
// (tokens.PhaseBand), and the dashboard's `BANDS` gives each its hue and its
// place in the stack. A band the engine answers and the table lacks is drawn in
// no colour; a band the table carries and the engine dropped is a legend entry
// for spend that can never appear; and a table in another ORDER stacks the
// columns differently from the engine's own legend.
func TestTheSpendChartDrawsExactlyTheEnginesPhaseBands(t *testing.T) {
	t.Parallel()
	block, err := clientsource.Literal(clientsource.Tree(t), "BANDS")
	if err != nil {
		t.Fatal(err)
	}
	drawn := clientsource.Field(block, "value")
	if len(drawn) == 0 {
		t.Fatal("no bands were found at all, so this gate certifies nothing")
	}
	var folded []string
	for _, band := range tokens.Bands {
		folded = append(folded, string(band))
	}
	if !slices.Equal(drawn, folded) {
		t.Errorf("the dashboard draws the bands %v and the engine folds into %v — "+
			"one set, in one stacking order", drawn, folded)
	}
}

// AND A PHASE IS DRAWN IN THE BAND THE ENGINE FOLDS IT INTO.
//
// The design system has no phase hues: inside a figure a phase is a series,
// and the dashboard draws it in its BAND's (`PHASE_BANDS`, read by
// `phaseColor`), so a phase is one colour on every chart. That table is
// tokens.PhaseBand written out by phase, and it can drift two ways, both
// silent: an entry the engine folds elsewhere is a coding run drawn as Review
// in one chart and counted as Execute in the next, and a phase the table does
// not name falls to Auxiliary's hue whatever the engine does with it.
func TestEveryPhaseIsDrawnInTheBandTheEngineFoldsItInto(t *testing.T) {
	t.Parallel()
	block, err := clientsource.Literal(clientsource.Tree(t), "PHASE_BANDS")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("no phases were found at all, so this gate certifies nothing")
	}
	for _, key := range keys {
		drawn := clientsource.Field(block, key)
		if want := string(tokens.PhaseBand(key)); len(drawn) != 1 || drawn[0] != want {
			t.Errorf("the dashboard draws phase %q in band %v and the engine folds it into %q",
				key, drawn, want)
		}
	}
	for _, p := range phase.All {
		if !slices.Contains(keys, string(p)) {
			t.Errorf("phase %q is not in PHASE_BANDS, so it is drawn in Auxiliary's hue "+
				"whatever band the engine counts it in", p)
		}
	}
}
