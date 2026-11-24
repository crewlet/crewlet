package queries_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/research"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// memorySandbox is a pending-run store with nothing in it: this sweep is
// about which names exist, not what they answer.
type memorySandbox struct{}

func (memorySandbox) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return nil, nil
}

func (memorySandbox) BridgeLog(context.Context, sandbox.PendingRun) (sandbox.BridgeLog, error) {
	return sandbox.BridgeLog{}, nil
}

func (memorySandbox) Get(context.Context, string) (sandbox.PendingRun, bool, error) {
	return sandbox.PendingRun{}, false, nil
}

// roomQueries is every query kind a room asks for, mapped to the files that
// ask, found in the rooms' own source: a hand-maintained list drifts towards
// claiming the server answers more than it does.
//
// Every call shape counts: `useQuery`, `useSearchQuery`, `useCodeQuery` (REST,
// never the socket's in-flight slots) and the direct `query` a pager or an
// action uses. [clientsource.Calls] reads them by syntax; each hit is keyed by
// path, not base name.
//
// A kind not given as a string literal is invisible here, which is safe only
// because the dashboard's `app/source.test.ts` ("every read names its
// question") requires a literal at every such call.
func roomQueries(t *testing.T) map[string][]string {
	t.Helper()
	calls, err := clientsource.Calls(clientsource.Tree(t), "useQuery", "useSearchQuery", "useCodeQuery",
		"query")
	if err != nil {
		// FAILS rather than skips: the dashboard source is always in a
		// checkout, and a skip is indistinguishable from a pass.
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Fatal("the sweep found no query calls at all, so it certifies nothing")
	}
	return calls
}

// emptyAdvisor is an advisor log holding nothing.
type emptyAdvisor struct{}

func (emptyAdvisor) Findings(context.Context, advisordomain.Filter, statelog.Freshness) ([]advisordomain.FindingRow, string, error) {
	return nil, "", nil
}
func (emptyAdvisor) NonClosed(context.Context, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return nil, nil
}
func (emptyAdvisor) Finding(context.Context, string, statelog.Freshness) (advisordomain.FindingRow, []advisordomain.HistoryRow, error) {
	return advisordomain.FindingRow{}, nil, advisordomain.ErrNotFound
}
func (emptyAdvisor) Episodes(context.Context, string, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return nil, nil
}
func (emptyAdvisor) LiveProgress(context.Context, statelog.Freshness) (string, []advisordomain.ProgressLine, bool, error) {
	return "", nil, false, nil
}
func (emptyAdvisor) Checkpoint(context.Context, string, statelog.Freshness) (json.RawMessage, bool, error) {
	return nil, false, nil
}

func (emptyAdvisor) RunProgress(context.Context, string, statelog.Freshness) ([]advisordomain.ProgressLine, error) {
	return nil, nil
}
func (emptyAdvisor) LatestRun(context.Context, statelog.Freshness) (advisordomain.RunRow, bool, error) {
	return advisordomain.RunRow{}, false, nil
}
func (emptyAdvisor) PendingRequests(context.Context, statelog.Freshness) ([]advisordomain.RunRow, error) {
	return nil, nil
}
func (emptyAdvisor) LastRunOfClasses(context.Context, statelog.Freshness) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}
func (emptyAdvisor) AlarmEpisodes(context.Context, time.Time, statelog.Freshness) ([]advisordomain.AlarmEpisodeRow, error) {
	return nil, nil
}
func (emptyAdvisor) Reports(context.Context, string, int, statelog.Freshness) ([]advisordomain.ReportRow, string, error) {
	return nil, "", nil
}
func (emptyAdvisor) Report(context.Context, string, statelog.Freshness) (advisordomain.ReportRow, bool, error) {
	return advisordomain.ReportRow{}, false, nil
}

// everySeam is a Sources with every seam present, which is what makes the
// registry list everything this build can answer.
//
// Register only tests the seams for nil, so zero values are enough: this
// sweep is about WHICH NAMES exist, not what they answer.
func everySeam(t *testing.T) queries.Sources {
	t.Helper()
	surface, _ := configSurface(t, companyDoc)
	cfg := company(t)
	return queries.Sources{
		State:    &livestate.LiveState{},
		Events:   eventfan.Solo("node-a", &store.EventLog{}),
		Usage:    usage.Reader{Estate: &store.DB{}},
		Company:  companySource(t, cfg),
		Coord:    coordmemory.New(),
		Plane:    coordmemory.NewFleet(),
		Memory:   &memread.Reader{},
		Channels: fakeChannels{},
		Budget:   coordmemory.NewFleet(),
		Sandbox:  memorySandbox{},
		Research: noResearch{},
		Searcher: noResearch{},
		// EACH NODE'S STATUS RECORD, counted against Coord's presence.
		NodeStatuses: coordmemory.NewFleet(),
		// A RUNNING RUN'S LIVE OUTPUT: the real reader over an empty
		// fleet record, which answers every run `not_running`.
		ExecutorTail: &sandbox.TailReader{Pending: sandbox.NewCoordStore(coordmemory.NewFleet()),
			Condition: func(err error) bool { _, ok := builtin.Condition(err); return ok }},
		// A SEAT'S EXECUTOR SESSIONS: the rows, and the store their sealed
		// objects open through — two seams, because a node with no object
		// store still lists the rows.
		ExecutorSessions: &stubSessions{},
		SessionObjects:   failingObjects{},
		Config:           surface,
		Work:             emptyWork{},
		Pages:            emptyPages{},
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

		IssueReports: issuereport.New(issuereport.Config{Store: coordmemory.NewFleet()}),
		AgentAccess:  func(context.Context) ([]queries.AgentAccessRow, error) { return nil, nil },
		// WHAT DELEGATES DECIDED, read off an access log holding nothing.
		Delegated: emptyVerdicts{},
		// AND WHAT A RULE CAN NAME FROM THE COMPANY'S GITHUB.
		DelegatedGitHub: ruleGitHub{},
		// WHAT AGENTS CHANGED ON THE GROWTH ACCOUNTS, off the same empty log.
		Growth: emptyGrowth{},
		// THE ATLAS, read off a node that runs no company yet.
		Atlas:    builtin.AtlasDeps{},
		Insights: coordmemory.NewFleet(),
		// THE ADVISOR'S LOG, holding nothing.
		Advisor: emptyAdvisor{},
		// THE FLEET'S BACKUP REGISTER, which every node opens.
		Backups: register{},
		// THE ARTIFACTS AND THEIR DATASTORES, each its own seam because a
		// node is wired with them only where the feature is deployed.
		Artifacts:    &queries.ArtifactsSource{},
		ArtifactData: &queries.ArtifactDataSource{},
		// THIS NODE'S SIGN-IN LIMITS, which internal/api fills from the
		// Bootstrap on every node that serves the API.
		SignIn: &config.APIAuth{},
		// GITLET'S ROWS, which internal/api fills from the code
		// surface on every node that serves the API.
		Code: &stubCode{},
		// THE COMPANY'S OWN CONVERSATION. Three seams rather than one:
		// a search is asked for a viewer rather than over rooms, and the
		// badges are coordination's rather than the log's — so a surface
		// with the rooms and neither of the others is a real shape, and
		// `chat_search` is gated on its own seam.
		Chat:       emptyChat{},
		ChatSearch: emptyChat{},
		ChatDraft:  emptyChat{},
		ChatReads:  coordmemory.NewFleet(),
		// A PERSON'S HOME BOARD, the coordination twin's.
		HomeBoards: coordmemory.NewFleet(),
		// THE MODEL QUESTIONS, each its own seam: what this node resolved
		// for each model, the fleet's device sign-ins, and the catalogue a
		// model step suggests from. Every engine that serves the API wires
		// all three, and Settings › Models & Fleets reads each. The fakes are
		// models_test.go's, empty: this sweep is about which names exist.
		Models:        fakeModels{},
		DeviceSignIns: fakeDevices{},
		ModelChecks:   fakeModelChecks{},
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

func (emptyChat) Reach(_ context.Context, _ chatlet.Actor, channelID, _ string,
	_ chatlet.NewMessage) (chatlet.Draft, error) {
	return chatlet.Draft{ChannelID: channelID}, nil
}

// emptyWork and emptyPages are the native readers with nothing in them, on
// fakeChannels' terms: this sweep is about which NAMES exist.
type emptyWork struct{}

func (emptyWork) FlowHours(context.Context, worklet.FlowHoursQuery) ([]worklet.FlowHours, error) {
	return nil, nil
}

func (emptyWork) FlowItems(context.Context, worklet.FlowHoursQuery) (map[string]int, error) {
	return map[string]int{}, nil
}

func (emptyWork) FlowPickups(context.Context, worklet.FlowHoursQuery) ([]time.Duration, error) {
	return []time.Duration{}, nil
}

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

func (emptyWork) EntryMarks(context.Context, statelog.ReadLevel) (worklet.EntryMarks, error) {
	return worklet.EntryMarks{Open: map[string]int{}}, nil
}

func (emptyWork) EntryWork(context.Context, worklet.EntryWorkQuery, time.Time) (worklet.EntryWork, error) {
	return worklet.EntryWork{Open: []worklet.TaskRow{}, Recent: []worklet.TaskRow{}}, nil
}

func (emptyWork) WorkPatterns(context.Context, worklet.PatternsQuery) (worklet.WorkPatterns, error) {
	return worklet.WorkPatterns{}, nil
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
// Each side's tests pass against its own idea of the other, so a name no
// answer is registered under comes back unknown_query and renders as an empty
// room with no error anywhere. This is the cheap half of the gate
// internal/e2e gives the push protocol: it checks NAMES, not fields, so it
// needs no node.
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
// this is the complete surface, and nothing is invoked.
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
// The direction that goes quiet rather than breaking: an answer nobody calls
// is code with tests and no way to notice it stopped being right.
//
// NO SILENT EXCEPTIONS. A kind with no room reader is a decision made in the
// open, in [readOutsideTheDashboard], held both ways so none outlives its
// reason: a kind it names that a room reads is red until its line goes, and so
// is one this build no longer answers.
func TestEveryQueryThisServerAnswersHasAReader(t *testing.T) {
	t.Parallel()
	asked := roomQueries(t)
	answered := registeredKinds(t)
	for _, kind := range answered {
		rooms, roomReads := asked[kind]
		command, outside := readOutsideTheDashboard[kind]
		switch {
		case roomReads && outside:
			t.Errorf("%s asks for %q now, so readOutsideTheDashboard's line for it "+
				"(%s) excuses nothing — delete it", strings.Join(rooms, " and "), kind, command)
		case !roomReads && !outside:
			t.Errorf("this build answers %q and no room asks for it — either a "+
				"reader was lost, or the answer should go with whatever used to "+
				"call it", kind)
		}
	}
	for kind, command := range readOutsideTheDashboard {
		if !slices.Contains(answered, kind) {
			t.Errorf("readOutsideTheDashboard names %q (%s), which this build does "+
				"not answer — delete its line", kind, command)
		}
	}
}

// readOutsideTheDashboard are the questions no room reads and something
// outside the dashboard does, over their REST paths, each beside what reads
// it: `code_repo_readers` is an admin's script (docs/guides/gitlet.md, "Who
// read a unit's repository"), and an assistant asks the operator tool of the
// same name (builtin.ReadersOf). Held both ways by
// [TestEveryQueryThisServerAnswersHasAReader]: the first room that reads one
// of these deletes its line.
var readOutsideTheDashboard = map[string]string{
	"research_page":     "Ask Crewlet's fetch_web_page, over GET /research/page (ADR-0151)",
	"research_search":   "Ask Crewlet's search_web, over GET /research/search (ADR-0151)",
	"code_repo_readers": "an admin's script: GET /gitlet/repos/{name}/readers, as docs/guides/gitlet.md prints it",
	"advisor_capacity":  "the Advisor's deep run and an admin's script: GET /advisor/capacity (docs/guides/advisor.md)",
	"llm_fleets":        "a script or the operator: GET /llm/fleets (docs/reference/api-endpoints.md), until the Models page reads it",
}

// AND THE SWEEP ITSELF CANNOT GO QUIET.
//
// Both gates above are only as wide as [everySeam]: a seam the helper does not
// set registers nothing, and its names vanish from `registeredKinds` without
// either direction noticing.
//
// So the gating conditions are READ FROM Register ITSELF rather than restated
// here: a hand-kept list of seams is a second copy that drifts.
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
// BOUNDED AT THE NEXT TOP-LEVEL DECLARATION: the answers below it test their
// seams for a different reason (a nil `Events` behind a seat's phase history is
// an empty list, not an absent question), and reading those as gates would
// demand seams this sweep has no use for.
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
// The applier records, per change and per recipient, the ONE reason of
// eighteen under which that person heard about it. It reaches a screen through
// `work_inbox`, and a reason the client has no phrase for renders as its own
// snake_case value on the surface a person reads first.
//
// The client's table is read from ITS OWN SOURCE rather than restated, so the
// gate cannot drift towards claiming the pair agree. A phrase for a reason
// nothing writes is checked too: a renamed reason leaves a dead entry behind
// and a live one missing.
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
// `token_series` refuses an unknown `group`, so a control offering a seventh
// value is a chart that never loads rather than one drawn on the wrong
// dimension. The screen's list and [tokens.Groups] are one closed set written
// twice, and this gate says so, in both directions (a renamed group otherwise
// leaves a dead control behind).
//
// The client's table is read from ITS OWN SOURCE rather than restated here:
// a gate carrying its own copy drifts towards claiming the pair agree.
func TestEveryCostDimensionTheScreenOffersIsOneTheEngineAccepts(t *testing.T) {
	t.Parallel()
	// THE `GROUPS` TABLE ITSELF, not every `{value, label}` pair on the
	// screen: the compare control is the same shape, and its "previous" would
	// read as a seventh dimension. FOUND RATHER THAN ADDRESSED by file path:
	// the constant is the subject, not the file it happens to sit in.
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
// silent: an entry the engine folds elsewhere is an executor drawn as Review
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

// emptyVerdicts is an access log holding no verdict.
type emptyVerdicts struct{}

func (emptyVerdicts) Verdicts(context.Context, accessdomain.VerdictQuery, statelog.Freshness) (accessdomain.VerdictPage, error) {
	return accessdomain.VerdictPage{Complete: true}, nil
}

func (emptyVerdicts) OpenVerdict(accessdomain.VerdictRow) (string, string, []string, error) {
	return "", "", nil, nil
}

type emptyGrowth struct{}

func (emptyGrowth) Changes(context.Context, accessdomain.GrowthQuery, statelog.Freshness) (accessdomain.GrowthPage, error) {
	return accessdomain.GrowthPage{Complete: true}, nil
}

// noResearch reads no page, for the sweep that only needs the seat filled.
type noResearch struct{}

func (noResearch) Read(context.Context, string, int) (research.Page, error) {
	return research.Page{}, nil
}

func (noResearch) ResearchSearch(context.Context, string) (research.Results, error) {
	return research.Results{}, nil
}
