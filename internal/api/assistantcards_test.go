package api_test

import (
	"context"
	"maps"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/worklet"
)

// TestEveryReadCardReadsItsRoutesOwnAnswer: EVERY PATH A READ'S CARD READS IS
// A KEY OF THE ANSWER ITS ROUTE GIVES.
//
// The model reads a tool's result from its card (cards.Summary), so a card
// that reads its rows from a key the answer does not hold is a tool that
// answers "nothing found" whatever the company holds: list_artifacts read
// `items` from a listing whose rows are `contents`, and told people they had
// no artifacts. A card spec is a set of strings the compiler cannot hold to
// anything, and many answers are maps a handler builds by hand, which no type
// walk could see. So every read tool's every card variant is ASKED, through
// the App's own handler as an administrator, over a company whose sources
// answer a value with every field set (assistanttest.Filled), and each path
// its spec reads must be in the real answer (assistanttest.HoldCard).
//
// What this fixture cannot fill (the spend and turn estates, which only a
// running fleet writes, and what only a live seat reports) is held to the Go
// type the handler encodes there instead ([answerTypes]): only a value's type
// is declared, every key above it is the answer's. A declaration the walk no
// longer needs fails, so the list only shrinks. GET /iam/seats, a surface
// this App does not stand up, is held by internal/api/iamapi's own suite
// (TestTheSeatsCardReadsTheSeatsAnswer). Mutation: point list_artifacts' rows
// back at `items`, or list_decisions' task at `ask.task_key`, and this
// names the tool, the variant and the path.
func TestEveryReadCardReadsItsRoutesOwnAnswer(t *testing.T) {
	t.Parallel()
	world := filledWorld(t)
	fx := newAssistAppWith(t, []iam.Grant{iam.GrantAdmin}, world.mount)
	held, used := 0, map[string]bool{}
	for _, tool := range readTools() {
		variants := assistanttest.CardVariants(t, tool)
		for variant, spec := range tool.Cards {
			name := tool.Name + "/" + variant
			if heldElsewhere[name] {
				continue
			}
			args, reached := variants[variant]
			if !reached {
				t.Errorf("%s: no example arguments reach this card", name)
				continue
			}
			maps.Copy(args, argsFor[tool.Name])
			answer := askAsAdmin(t, fx, tool.Name, name, args)
			if answer == nil {
				continue
			}
			n, consulted := assistanttest.HoldCard(t, name, spec, answer, answerTypes[name])
			held += n
			for _, at := range consulted {
				used[name+" "+at] = true
			}
			// A NOTE IS WHAT A SEARCH THAT COULD NOT RUN SAYS, so it is held
			// to the answer the search gives while its index is building.
			if spec.Note != "" {
				world.building.Store(true)
				building := askAsAdmin(t, fx, tool.Name, name+" (building)", args)
				world.building.Store(false)
				if building != nil && !assistanttest.Present(building, spec.Note) {
					t.Errorf("%s reads its note from %q, which a search that could not run "+
						"does not answer", name, spec.Note)
				}
			}
		}
	}
	for name, declared := range answerTypes {
		for at := range declared {
			if !used[name+" "+at] {
				t.Errorf("answerTypes declares %s's %q, which the walk never needed: the "+
					"fixture answers it now, so remove the declaration", name, at)
			}
		}
	}
	if held < 200 {
		t.Errorf("only %d paths were held to an answer; the walk is not reading the catalogue",
			held)
	}
}

// askAsAdmin is a read tool's request with args, answered by the App's own
// handler to the fixture's administrator, decoded; nil, with the failure
// reported, for anything but a 200.
func askAsAdmin(t *testing.T, fx *assistApp, tool, name string, args map[string]any) any {
	t.Helper()
	read, ok := catalog.Lookup(tool)
	if !ok || read.Kind != catalog.KindRead {
		t.Fatalf("%s is not a read tool", tool)
	}
	req, err := read.Request(args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	status, body := fx.asPerson(t, req)
	if status != http.StatusOK {
		t.Errorf("%s answered %d %s, so its card cannot be held to its answer", name, status,
			body)
		return nil
	}
	answer, err := cards.Decode(body)
	if err != nil {
		t.Fatalf("%s answered no JSON: %v", name, err)
	}
	return answer
}

// argsFor are the arguments a tool is asked with beyond its examples: the seat
// this fixture's live state knows, where the example names one it does not.
var argsFor = map[string]map[string]any{
	"get_agent": {"handle": "ceo"},
}

// heldElsewhere are the card variants another suite holds to their route's
// answer, because this App does not stand their surface up.
var heldElsewhere = map[string]bool{
	"get_org_chart/seats": true, // internal/api/iamapi: TestTheSeatsCardReadsTheSeatsAnswer
}

// answerTypes are, by card variant, the Go types the handler encodes where
// this fixture's answer leaves a value out or null (see
// assistanttest.ValueTypes): the estates only a running fleet writes (spend,
// turns, a seat's activity and its live turn), and the members a handler
// writes with `omitempty` that this company leaves zero.
var answerTypes = map[string]assistanttest.ValueTypes{
	"list_agents/": {
		"row.turn":       reflect.TypeFor[livestate.LiveTurn](),
		"row.last_turn":  reflect.TypeFor[livestate.LastTurn](),
		"row.paused":     reflect.TypeFor[livestate.Paused](),
		"row.last_error": reflect.TypeFor[livestate.ErrorInfo](),
	},
	"get_agent/detail": {
		"record.live.last_turn":  reflect.TypeFor[livestate.LastTurn](),
		"record.live.last_error": reflect.TypeFor[livestate.ErrorInfo](),
	},
	// A ROW OF THE FEED IS ONE SOURCE'S, and a schedule's is the usage
	// estate's, which only a running fleet writes.
	"get_company_feed/":         {"row": reflect.TypeFor[queries.FeedEntry]()},
	"get_agent/activity":        {"row": reflect.TypeFor[queries.SeatActivity]()},
	"list_turns/":               {"row": reflect.TypeFor[store.Turn]()},
	"get_spend/agent":           {"row": reflect.TypeFor[tokens.AgentRow]()},
	"get_spend/model":           {"row": reflect.TypeFor[tokens.ModelRow]()},
	"get_spend/phase":           {"row": reflect.TypeFor[tokens.PhaseRow]()},
	"get_spend_series/by_group": {"row": reflect.TypeFor[tokens.GroupRow]()},
	"get_fleet_health/backups":  {"row": reflect.TypeFor[queries.BackupRunRow]()},
}

// ---- a company whose every source answers ------------------------------ //

// cardsCompany is the parity company with what the reads list beyond its
// seats: a unit with its lead, budgets for the company and a seat, and an
// integration.
const cardsCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
token_budget:
  day: 1000000
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    token_budget:
      day: 100000
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
units:
  - name: Engineering
    lead: cto
    roles:
      - name: CTO
        handle: cto
        llm: zulu
integrations:
  mattermost: {enabled: true, url: https://mm.example.com, team: acme}
`

// cardsWorld is the filled company's sources, and the switch that puts its
// work and chat indexes in the state of ones still building.
type cardsWorld struct {
	paritySources
	building *atomic.Bool
}

// filledWorld is [cardsCompany] with its tracker, work index, executors,
// pages, knowledge base, budgets, backups and artifacts answering a value with
// every field set, and one node of the fleet reporting, so each read answers
// rows to hold its card to.
func filledWorld(t *testing.T) cardsWorld {
	t.Helper()
	s := parityWorldOf(t, cardsCompany)
	building := &atomic.Bool{}
	work := filledWork{building: building}
	s.Work, s.WorkSearch, s.Sandbox, s.Pages = work, work, filledRuns{}, filledPages{}
	talk := filledChat{building: building}
	s.Chat, s.ChatSearch = talk, talk
	s.Knowledge = func() knowledge.Searcher { return filledKnowledge{} }
	s.Budget, s.Backups = filledBudget{}, filledBackups{}
	// THE MATTERMOST INTEGRATION'S LAST RECONCILE, filled, as the fleet's
	// reconcile loop records it.
	s.Reconciles = func(context.Context) []integration.State {
		state := filled[integration.State]()
		state.Kind, state.Disconnecting = integration.KindMattermost, false
		return []integration.State{state}
	}
	estate := filledArtifacts{}
	s.Artifacts = &queries.ArtifactsSource{Gate: artifactsapi.Gate{Heads: estate,
		Chart: authz.NoChart{}, Marks: estate, Roster: estate,
		Limits: func() artifacts.Limits { return artifacts.Limits{CompanyBytes: 1, Artifacts: 1} }},
		Reads: estate}
	status := filled[coord.NodeStatus]()
	if _, _, err := s.Coord.TryAcquire(t.Context(), coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a/1", TTL: time.Hour,
			Meta: map[string]any{coord.StatusKey: status.Meta()}}); err != nil {
		t.Fatalf("report node-a to the fleet: %v", err)
	}
	return cardsWorld{paritySources: s, building: building}
}

// filled is assistanttest.Filled on this suite's clock.
func filled[T any]() T { return assistanttest.Filled[T](clock) }

// filledWork is a tracker and a work index answering every read filled; the
// index answers as one still building while building is set.
type filledWork struct{ building *atomic.Bool }

func (filledWork) Tasks(context.Context, worklet.Query, time.Time) (worklet.Answer, error) {
	return filled[worklet.Answer](), nil
}

func (filledWork) Task(context.Context, string, worklet.DetailWants, statelog.Freshness) (
	worklet.TaskDetail, error) {
	return filled[worklet.TaskDetail](), nil
}

func (filledWork) TurnsOf(context.Context, string, string, int, statelog.Freshness) (
	worklet.TaskTurns, error) {
	return filled[worklet.TaskTurns](), nil
}

func (filledWork) Views(context.Context, worklet.ViewQuery) (worklet.ViewListing, error) {
	return filled[worklet.ViewListing](), nil
}

func (filledWork) EveryView(context.Context, worklet.EveryViewQuery) (worklet.ViewListing, error) {
	return filled[worklet.ViewListing](), nil
}

// ExpandedQuery answers the empty query, as stubWorkReader does: the tracker
// asked it answers filled whatever it was asked.
func (filledWork) ExpandedQuery(context.Context, map[string]any, worklet.Viewer, time.Time,
	*time.Location) (worklet.Query, error) {
	return worklet.Query{}, nil
}

func (filledWork) Catalogue(context.Context, worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
	return filled[worklet.CatalogueAnswer](), nil
}

func (filledWork) Projects(context.Context, worklet.ProjectQuery) (worklet.ProjectListing, error) {
	return filled[worklet.ProjectListing](), nil
}

func (filledWork) Project(context.Context, worklet.ProjectDetailQuery) (worklet.ProjectDetail, error) {
	return filled[worklet.ProjectDetail](), nil
}

func (filledWork) Workload(context.Context, worklet.WorkloadQuery, time.Time, *time.Location) (
	worklet.WorkloadAnswer, error) {
	return filled[worklet.WorkloadAnswer](), nil
}

func (filledWork) Activity(context.Context, worklet.ActivityQuery, time.Time) (
	worklet.ActivityAnswer, error) {
	return filled[worklet.ActivityAnswer](), nil
}

func (filledWork) MyWork(context.Context, worklet.MyWorkQuery, time.Time, *time.Location) (
	worklet.MyWork, error) {
	return filled[worklet.MyWork](), nil
}

func (filledWork) Person(context.Context, worklet.PersonQuery, time.Time) (worklet.PersonState, error) {
	return filled[worklet.PersonState](), nil
}

func (filledWork) Inbox(context.Context, worklet.InboxQuery, time.Time) (worklet.InboxAnswer, error) {
	return filled[worklet.InboxAnswer](), nil
}

func (filledWork) Routing(context.Context, worklet.RoutingQuery, time.Time) (
	worklet.RoutingAnswer, error) {
	return filled[worklet.RoutingAnswer](), nil
}

func (filledWork) Flow(context.Context, worklet.FlowQuery, time.Time, *time.Location) (
	worklet.FlowAnswer, error) {
	return filled[worklet.FlowAnswer](), nil
}

// CompanyFeed answers one filled row at the end of the feed: a cursor is the
// tracker's own encoding, which no filled string is.
func (filledWork) CompanyFeed(context.Context, worklet.FeedQuery) (worklet.FeedPage, error) {
	page := filled[worklet.FeedPage]()
	for i := range page.Rows {
		page.Rows[i].Cursor = ""
	}
	page.More = false
	return page, nil
}

func (filledWork) Decisions(context.Context, worklet.DecisionsQuery, time.Time, *time.Location) (
	worklet.DecisionsAnswer, error) {
	return filled[worklet.DecisionsAnswer](), nil
}

func (filledWork) AnsweredAsks(context.Context, worklet.DecisionsQuery, time.Time,
	*time.Location) (worklet.AnsweredAnswer, error) {
	return filled[worklet.AnsweredAnswer](), nil
}

func (filledWork) TurnPlaces(context.Context, []string, statelog.Freshness) (
	map[string]worklet.TurnPlace, error) {
	return filled[map[string]worklet.TurnPlace](), nil
}

func (filledWork) Thread(context.Context, worklet.ThreadQuery, statelog.Freshness) (
	worklet.ResolvedThread, error) {
	return filled[worklet.ResolvedThread](), nil
}

func (w filledWork) Search(context.Context, worklet.SearchQuery) (worklet.SearchAnswer, error) {
	if w.building.Load() {
		return worklet.SearchAnswer{}, worklet.ErrIndexBuilding
	}
	return filled[worklet.SearchAnswer](), nil
}

// filledChat is the company's own chat answering every read filled, and its
// index as one still building while building is set.
type filledChat struct {
	noReaderViews
	building *atomic.Bool
}

func (filledChat) Channels(context.Context, string, chatlet.ChannelsQuery, statelog.Freshness) (
	chatlet.ChannelListing, error) {
	return filled[chatlet.ChannelListing](), nil
}

func (filledChat) Channel(context.Context, string, string, statelog.Freshness) (
	chatlet.ChannelDetail, error) {
	return filled[chatlet.ChannelDetail](), nil
}

func (filledChat) ChannelNamed(context.Context, string, string, statelog.Freshness) (
	chatlet.ChannelDetail, error) {
	return filled[chatlet.ChannelDetail](), nil
}

func (filledChat) Messages(context.Context, string, chatlet.TranscriptQuery, statelog.Freshness) (
	chatlet.Transcript, error) {
	return filled[chatlet.Transcript](), nil
}

func (filledChat) Thread(context.Context, string, chatlet.ThreadQuery, statelog.Freshness) (
	chatlet.Thread, error) {
	return filled[chatlet.Thread](), nil
}

func (filledChat) RoomNames(context.Context, string, statelog.Freshness) (
	chatlet.RoomNames, error) {
	return filled[chatlet.RoomNames](), nil
}

// Activity answers filled, which is what the assistant reads where a person
// was named.
func (filledChat) Activity(context.Context, string, chatlet.ActivityQuery, statelog.Freshness) (
	chatlet.Activity, error) {
	return filled[chatlet.Activity](), nil
}

func (filledChat) Directory(context.Context, string, statelog.Freshness) (
	chatlet.Directory, error) {
	return filled[chatlet.Directory](), nil
}

// Search answers filled hits, each said in a room the answer names, as the
// serving node names every room its hits were said in.
func (c filledChat) Search(context.Context, estate.ChatQuery) (estate.ChatSearchAnswer, error) {
	answer := filled[estate.ChatSearchAnswer]()
	answer.Rooms = map[string]chatlet.ReadableRoom{}
	for _, hit := range answer.Hits {
		room := filled[chatlet.ReadableRoom]()
		room.ID = hit.ChannelID
		answer.Rooms[hit.ChannelID] = room
	}
	answer.Building = c.building.Load()
	return answer, nil
}

// filledRuns is a node running one executor, filled, parked on a question
// put to the fixture's person, so it is also what waits on them.
type filledRuns struct{}

func (r filledRuns) ListActive(ctx context.Context) ([]sandbox.PendingRun, error) {
	run, _, err := r.Get(ctx, "")
	return []sandbox.PendingRun{run}, err
}

func (filledRuns) Get(context.Context, string) (sandbox.PendingRun, bool, error) {
	run := filled[sandbox.PendingRun]()
	run.Status, run.AudienceHandles = sandbox.StatusAwaiting, []string{assistSeat}
	return run, true, nil
}

// filledPages is a knowledge base whose every read is filled.
type filledPages struct{}

func (filledPages) List(context.Context, doclet.Filter, statelog.Freshness) (doclet.Listing, error) {
	return filled[doclet.Listing](), nil
}

func (filledPages) Get(context.Context, string, statelog.Freshness) (doclet.Detail, error) {
	return filled[doclet.Detail](), nil
}

func (filledPages) Containers(context.Context, statelog.Freshness) ([]doclet.ContainerListing, error) {
	return filled[[]doclet.ContainerListing](), nil
}

// Activity answers one filled page change at the end of the activity, for the
// same reason [filledWork.CompanyFeed] does.
func (filledPages) Activity(context.Context, doclet.PageActivityQuery) (doclet.PageActivity, error) {
	activity := filled[doclet.PageActivity]()
	activity.NextCursor = ""
	return activity, nil
}

func (filledPages) Revision(context.Context, string, int, statelog.Freshness) (
	doclet.Revision, bool, error) {
	return filled[doclet.Revision](), true, nil
}

// filledKnowledge is the company's own knowledge base, finding a filled hit.
type filledKnowledge struct{}

func (filledKnowledge) Backend() string                             { return doclet.Backend }
func (filledKnowledge) CanSearch(*org.Role, *org.Organization) bool { return true }
func (filledKnowledge) Search(context.Context, knowledge.Query) knowledge.Result {
	return filled[knowledge.Result]()
}

// filledBudget is a budget ledger nobody has spent against yet: every window
// reads as unspent.
type filledBudget struct{}

func (filledBudget) Usage(context.Context, coord.Windows) ([]coord.Usage, error) { return nil, nil }

// filledBackups is a fleet holding one filled backup point.
type filledBackups struct{}

func (filledBackups) BackupPoints(context.Context) ([]coord.BackupPoint, error) {
	return filled[[]coord.BackupPoint](), nil
}

// filledArtifacts is the artifacts estate holding one filled artifact, owned
// by the fixture person's seat, so the listing's authority shows it to them.
type filledArtifacts struct{ apiArtifacts }

// filledHead is the artifact, live, of a kind this build serves, owned by the
// fixture person's seat and filed in no folder.
func filledHead(id artifacts.ID) artifactsdomain.Head {
	head := filled[artifactsdomain.Head]()
	head.ID, head.Kind, head.Status = id, artifacts.KindHTML, artifactsdomain.StatusLive
	head.Owner, head.OwnerKind = assistSeat, iam.ActorHuman
	head.Folder, head.Shares = "", nil
	return head
}

func (filledArtifacts) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	return filledHead(id), statelog.Position{}, nil
}

func (filledArtifacts) List(context.Context, artifactsdomain.Viewer, artifactsdomain.ListQuery) (
	artifactsdomain.ListPage, error) {
	item := filled[artifactsdomain.ListItem]()
	item.Head, item.FolderPath = filledHead(apiPage), ""
	return artifactsdomain.ListPage{Contents: []artifactsdomain.ListItem{item}, Total: 1}, nil
}
