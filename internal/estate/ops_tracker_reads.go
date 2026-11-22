package estate

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- the tracker's reads ------------------------------------------------ //

type tasksArgs struct {
	Query worklet.Query
	Now   time.Time
}

var opTasks = define("tracker.tasks", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a tasksArgs) (worklet.Answer, error) {
		if b.Tracker == nil {
			return worklet.Answer{}, errNoHalf
		}
		a.Query.Units = b.Units
		return b.Tracker.Tasks(ctx, a.Query, a.Now)
	}).rowsTo(statelog.AnswerBytes)

type taskArgs struct {
	IDOrKey string
	Want    worklet.DetailWants
	Fresh   statelog.Freshness

	// Zone is the name of the zone the asker's day clock is on
	// ([worklet.DayClock.Zone], [zoneArg]).
	Zone string
}

var opTask = define("tracker.task", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a taskArgs) (worklet.TaskDetail, error) {
		if b.Tracker == nil {
			return worklet.TaskDetail{}, errNoHalf
		}
		if a.Want.Clock != nil {
			loc, err := zoneOf(a.Zone)
			if err != nil {
				return worklet.TaskDetail{}, err
			}
			clock := *a.Want.Clock
			clock.Zone = loc
			a.Want.Clock = &clock
		}
		a.Want.Units = b.Units
		return b.Tracker.Task(ctx, a.IDOrKey, a.Want, a.Fresh)
	}).oneAnswer()

// viewsArgs is a views query as it crosses, with the zone its pinned counts
// cut relative dates on carried by name ([zoneArg]).
type viewsArgs struct {
	Query worklet.ViewQuery
	Zone  string
	After string
}

// ONE CONTAINER'S STRIP OF VIEWS, as a screen shows it — the dashboard's tabs,
// the API's listing: read leniently.
var opViews = define("tracker.views", opRead, trackerDomain, false, views).rowsTo(statelog.AnswerBytes)

// THE STRIP A VIEW IS SAVED AGAIN FROM, ANSWERED WHOLE ([op.answeredWhole]):
// save_work_view replaces a view whole, so a call changing one thing re-sends
// the rest as list_work_views showed it (internal/agent/builtin), and a newer
// build's field this build would drop is refused here rather than saved away.
// Its own operation, so a screen that only shows the strip is not refused one
// ([opArtifactVersionCarried] says why).
//
// Not a save that keeps what it does not name: a field left at its zero
// crosses exactly as one an older build never sent ([define]), so a merge
// could not tell cleared from unknown ([worklet.Writer.WriteView]).
var opViewsCarried = define("tracker.views_carried", opRead, trackerDomain, false,
	views).rowsTo(statelog.AnswerBytes).answeredWhole()

// viewsPage is a page of a container's strip and the cursor of the page
// after it, empty on the last.
type viewsPage struct {
	Listing worklet.ViewListing
	Next    string
}

// views is a page of one container's strip of views, read from this node's
// copy.
func views(ctx context.Context, b Backend, _ *Actor, a viewsArgs) (viewsPage, error) {
	if b.Tracker == nil {
		return viewsPage{}, errNoHalf
	}
	loc, err := zoneOf(a.Zone)
	if err != nil {
		return viewsPage{}, err
	}
	q := a.Query
	q.Units, q.Zone = b.Units, loc
	listing, next, err := b.Tracker.ViewsPage(ctx, q, a.After)
	return viewsPage{Listing: listing, Next: next}, err
}

// zoneArg is a location as it crosses: its NAME, because a *time.Location is a
// table of transitions a JSON encoder writes as nothing at all. Empty is a nil
// location.
func zoneArg(loc *time.Location) string {
	if loc == nil {
		return ""
	}
	return loc.String()
}

// zoneOf is the location a request named, loaded through the one loader the
// company's clock is read with ([period.LoadZone], ADR-0018) — which refuses
// `Local`, since the serving node's own clock is not the asker's — or nil for
// a request that named none.
func zoneOf(name string) (*time.Location, error) {
	if name == "" {
		return nil, nil
	}
	loc, err := period.LoadZone(name)
	if err != nil {
		return nil, fmt.Errorf("estate: the asking node's zone is not one this "+
			"node can load: %w", err)
	}
	return loc, nil
}

type expandArgs struct {
	Params map[string]any
	Viewer worklet.Viewer
	Now    time.Time

	// Zone is the location's name ([zoneArg]).
	Zone string
}

// A LIST'S PARAMETERS EXPANDED INTO THE QUERY ITS READ ASKS, ANSWERED WHOLE
// ([op.answeredWhole]): every caller hands the answer to tracker.tasks as that
// read's query — a seat's list_work_items, a screen's list of work items — so
// a filter a newer data node parsed and this build would drop is refused rather
// than asked without, which would answer more than the caller asked for.
var opExpandedQuery = define("tracker.expanded_query", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a expandArgs) (worklet.Query, error) {
		if b.Tracker == nil {
			return worklet.Query{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.Query{}, err
		}
		return b.Tracker.ExpandedQuery(ctx, a.Params, a.Viewer, a.Now, loc)
	}).oneAnswer().answeredWhole()

// THE WORKSPACE CATALOGUE, as a screen shows it: read leniently.
var opCatalogue = define("tracker.catalogue", opRead, trackerDomain, false, catalogue).oneAnswer()

// THE CATALOGUE A CALLER SENDS BACK, ANSWERED WHOLE ([op.answeredWhole]): its
// writes replace every declaration whole, and write_work_catalogue tells a
// model to send back what get_work_catalogue gave (internal/agent/builtin), so
// a newer build's field this build would drop is refused rather than written
// away. Its own operation, so a screen that only shows the catalogue is not
// refused one ([opArtifactVersionCarried] says why).
var opCatalogueCarried = define("tracker.catalogue_carried", opRead, trackerDomain, false,
	catalogue).oneAnswer().answeredWhole()

// catalogue is the workspace catalogue, read from this node's copy.
func catalogue(ctx context.Context, b Backend, _ *Actor, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
	if b.Tracker == nil {
		return worklet.CatalogueAnswer{}, errNoHalf
	}
	return b.Tracker.Catalogue(ctx, q)
}

type personArgs struct {
	Query worklet.PersonQuery
	Now   time.Time
}

var opPerson = define("tracker.person", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a personArgs) (worklet.PersonState, error) {
		if b.Tracker == nil {
			return worklet.PersonState{}, errNoHalf
		}
		return b.Tracker.Person(ctx, a.Query, a.Now)
	}).oneAnswer()

type threadArgs struct {
	Query worklet.ThreadQuery
	Fresh statelog.Freshness
}

var opThread = define("tracker.thread", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a threadArgs) (worklet.ResolvedThread, error) {
		if b.Tracker == nil {
			return worklet.ResolvedThread{}, errNoHalf
		}
		return b.Tracker.Thread(ctx, a.Query, a.Fresh)
	}).oneAnswer()

var opWorkPatterns = define("tracker.work_patterns", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.PatternsQuery) (worklet.WorkPatterns, error) {
		if b.Tracker == nil {
			return worklet.WorkPatterns{}, errNoHalf
		}
		return b.Tracker.WorkPatterns(ctx, q)
	}).oneAnswer()

var opFlowHours = define("tracker.flow_hours", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.FlowHoursQuery) ([]worklet.FlowHours, error) {
		if b.Tracker == nil {
			return nil, errNoHalf
		}
		return b.Tracker.FlowHours(ctx, q)
	}).rowsTo(statelog.AnswerBytes)

var opFlowPickups = define("tracker.flow_pickups", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.FlowHoursQuery) ([]time.Duration, error) {
		if b.Tracker == nil {
			return nil, errNoHalf
		}
		return b.Tracker.FlowPickups(ctx, q)
	}).rowsTo(statelog.AnswerBytes)

var opFlowItems = define("tracker.flow_items", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.FlowHoursQuery) (map[string]int, error) {
		if b.Tracker == nil {
			return nil, errNoHalf
		}
		return b.Tracker.FlowItems(ctx, q)
	}).rowsTo(statelog.AnswerBytes)

var opTurnsMoved = define("tracker.turns_moved", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.TurnsMovedQuery) (map[string]bool, error) {
		if b.Tracker == nil {
			return nil, errNoHalf
		}
		return b.Tracker.TurnsMoved(ctx, q)
	}).perAsked()

type activityArgs struct {
	Query worklet.ActivityQuery
	Now   time.Time
}

var opActivity = define("tracker.activity", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a activityArgs) (worklet.ActivityAnswer, error) {
		if b.Tracker == nil {
			return worklet.ActivityAnswer{}, errNoHalf
		}
		return b.Tracker.Activity(ctx, a.Query, a.Now)
	}).rowsTo(statelog.AnswerBytes)

type myWorkArgs struct {
	Query worklet.MyWorkQuery
	Now   time.Time

	// Zone is the company clock the asker cut "today" on ([zoneArg]).
	Zone string
}

var opMyWork = define("tracker.my_work", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a myWorkArgs) (worklet.MyWork, error) {
		if b.Tracker == nil {
			return worklet.MyWork{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.MyWork{}, err
		}
		return b.Tracker.MyWork(ctx, a.Query, a.Now, loc)
	}).oneAnswer()

type readyWorkArgs struct {
	Query worklet.ReadyWorkQuery
	Now   time.Time
}

var opReadyWork = define("tracker.ready_work", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a readyWorkArgs) (worklet.ReadyWork, error) {
		if b.Tracker == nil {
			return worklet.ReadyWork{}, errNoHalf
		}
		return b.Tracker.ReadyWork(ctx, a.Query, a.Now)
	}).oneAnswer()

var opPickupAnswer = define("tracker.pickup_answer", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.PickupAnswerQuery) (types.PickupAnswer, error) {
		if b.Tracker == nil {
			return "", errNoHalf
		}
		return b.Tracker.PickupAnswer(ctx, q)
	}).oneAnswer()

// ownAsksArgs is [opOwnAsks]' input.
type ownAsksArgs struct {
	Query worklet.OwnAsksQuery
	Now   time.Time
}

var opOwnAsks = define("tracker.own_asks", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a ownAsksArgs) (worklet.OwnAsks, error) {
		if b.Tracker == nil {
			return worklet.OwnAsks{}, errNoHalf
		}
		return b.Tracker.OwnAsks(ctx, a.Query, a.Now)
	}).oneAnswer()

var opEntryMarks = define("tracker.entry_marks", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, level statelog.ReadLevel) (worklet.EntryMarks, error) {
		if b.Tracker == nil {
			return worklet.EntryMarks{}, errNoHalf
		}
		return b.Tracker.EntryMarks(ctx, level)
	}).rowsTo(statelog.AnswerBytes)

// entryWorkArgs is [opEntryWork]'s input.
type entryWorkArgs struct {
	Query worklet.EntryWorkQuery
	Now   time.Time
}

var opEntryWork = define("tracker.entry_work", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a entryWorkArgs) (worklet.EntryWork, error) {
		if b.Tracker == nil {
			return worklet.EntryWork{}, errNoHalf
		}
		return b.Tracker.EntryWork(ctx, a.Query, a.Now)
	}).oneAnswer()

var opProjects = define("tracker.projects", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.ProjectQuery) (worklet.ProjectListing, error) {
		if b.Tracker == nil {
			return worklet.ProjectListing{}, errNoHalf
		}
		q.Units = b.Units
		return b.Tracker.Projects(ctx, q)
	}).rowsTo(statelog.AnswerBytes)

// ONE PROJECT, as a screen shows it: read leniently.
var opProject = define("tracker.project", opRead, trackerDomain, false, project).oneAnswer()

// ONE PROJECT A CALLER SENDS BACK, ANSWERED WHOLE ([op.answeredWhole]):
// write_project replaces the field declarations whole, sent as describe_project
// read them (internal/agent/builtin), so a newer build's field this build would
// drop is refused rather than written away. Its own operation, so a screen
// that only shows the project is not refused one ([opArtifactVersionCarried]
// says why).
var opProjectCarried = define("tracker.project_carried", opRead, trackerDomain, false,
	project).oneAnswer().answeredWhole()

// project is one project, read from this node's copy.
func project(ctx context.Context, b Backend, _ *Actor, q worklet.ProjectDetailQuery) (worklet.ProjectDetail, error) {
	if b.Tracker == nil {
		return worklet.ProjectDetail{}, errNoHalf
	}
	q.Units = b.Units
	return b.Tracker.Project(ctx, q)
}

type workSearchArgs struct {
	Text  string
	Limit int

	// Mode is how to rank. Empty is hybrid — what a caller that names no
	// mode (a seat's search_work tool) sends.
	Mode knowledge.Mode
}

// THE RANKED SEARCH READS THE INDEX OVER THE TRACKER'S ROWS, so it runs only on
// a copy whose tracker is in service, and CARRIES NO FLOOR: each node's own
// walk maintains the index behind its applier, so a floor would promise a
// visibility the index does not give.
var opWorkSearch = define("tracker.search", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a workSearchArgs) (worklet.SearchAnswer, error) {
		if b.WorkSearch == nil {
			return worklet.SearchAnswer{}, errNoHalf
		}
		return b.WorkSearch.Search(ctx, worklet.SearchQuery{Text: a.Text, Limit: a.Limit, Mode: a.Mode})
	}).oneAnswer().floorless()

type workloadArgs struct {
	Query worklet.WorkloadQuery
	Now   time.Time

	// Zone is the company clock the asker cut its days on ([zoneArg]).
	Zone string
}

var opWorkload = define("tracker.workload", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a workloadArgs) (worklet.WorkloadAnswer, error) {
		if b.Tracker == nil {
			return worklet.WorkloadAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.WorkloadAnswer{}, err
		}
		a.Query.Units = b.Units
		return b.Tracker.Workload(ctx, a.Query, a.Now, loc)
	}).rowsTo(statelog.AnswerBytes)

type inboxArgs struct {
	Query worklet.InboxQuery
	Now   time.Time
}

// opInbox is named `read_inbox`, beside `write_inbox`, never `tracker.inbox`:
// an operation's name is not a subject, but that one is spelled like a seat's
// inbox subject, and the guard that keeps every subject in internal/queue/topics
// rightly reads any literal ending `.inbox` as one written by hand.
var opInbox = define("tracker.read_inbox", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a inboxArgs) (worklet.InboxAnswer, error) {
		if b.Tracker == nil {
			return worklet.InboxAnswer{}, errNoHalf
		}
		return b.Tracker.Inbox(ctx, a.Query, a.Now)
	}).rowsTo(statelog.AnswerBytes)

type routingArgs struct {
	Query worklet.RoutingQuery
	Now   time.Time
}

var opRouting = define("tracker.routing", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a routingArgs) (worklet.RoutingAnswer, error) {
		if b.Tracker == nil {
			return worklet.RoutingAnswer{}, errNoHalf
		}
		return b.Tracker.Routing(ctx, a.Query, a.Now)
	}).rowsTo(statelog.AnswerBytes)

type turnsOfArgs struct {
	IDOrKey string
	Cursor  string
	Limit   int
	Fresh   statelog.Freshness
}

// A TASK'S TURNS are the tracker's: the turn records are charged to the task
// (ADR-0022) and live beside its rows.
var opTurnsOf = define("tracker.turns_of", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a turnsOfArgs) (worklet.TaskTurns, error) {
		if b.Tracker == nil {
			return worklet.TaskTurns{}, errNoHalf
		}
		return b.Tracker.TurnsOf(ctx, a.IDOrKey, a.Cursor, a.Limit, a.Fresh)
	}).rowsTo(statelog.AnswerBytes)

// everyViewArgs is [viewsArgs] for every saved view.
type everyViewArgs struct {
	Query worklet.EveryViewQuery
	Zone  string
	After string
}

// everyViewPage is a page of every view a person sees, in the containers'
// order, and the cursor of the page after it, empty on the last.
type everyViewPage struct {
	Listing worklet.ViewListing
	Next    string
}

var opEveryView = define("tracker.every_view", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a everyViewArgs) (everyViewPage, error) {
		if b.Tracker == nil {
			return everyViewPage{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return everyViewPage{}, err
		}
		q := a.Query
		q.Units, q.Zone = b.Units, loc
		listing, next, err := b.Tracker.EveryViewPage(ctx, q, a.After)
		return everyViewPage{Listing: listing, Next: next}, err
	}).rowsTo(statelog.AnswerBytes)

type flowArgs struct {
	Query worklet.FlowQuery
	Now   time.Time

	// Zone is the company clock the asker cut its days on ([zoneArg]).
	Zone string
}

var opFlow = define("tracker.flow", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a flowArgs) (worklet.FlowAnswer, error) {
		if b.Tracker == nil {
			return worklet.FlowAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.FlowAnswer{}, err
		}
		return b.Tracker.Flow(ctx, a.Query, a.Now, loc)
	}).oneAnswer()

var opCompanyFeed = define("tracker.company_feed", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.FeedQuery) (worklet.FeedPage, error) {
		if b.Tracker == nil {
			return worklet.FeedPage{}, errNoHalf
		}
		return b.Tracker.CompanyFeed(ctx, q)
	}).rowsTo(statelog.AnswerBytes)

type decisionsArgs struct {
	Query worklet.DecisionsQuery
	Now   time.Time

	// Zone is the company clock the asker cut "overdue" on ([zoneArg]).
	Zone string
}

var opDecisions = define("tracker.decisions", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a decisionsArgs) (worklet.DecisionsAnswer, error) {
		if b.Tracker == nil {
			return worklet.DecisionsAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.DecisionsAnswer{}, err
		}
		return b.Tracker.Decisions(ctx, a.Query, a.Now, loc)
	}).rowsTo(statelog.AnswerBytes)

// opAnsweredAsks takes [decisionsArgs]: the same person, freshness and clock,
// asked for the asks closed rather than the ones still open.
var opAnsweredAsks = define("tracker.answered_asks", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a decisionsArgs) (worklet.AnsweredAnswer, error) {
		if b.Tracker == nil {
			return worklet.AnsweredAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.AnsweredAnswer{}, err
		}
		return b.Tracker.AnsweredAsks(ctx, a.Query, a.Now, loc)
	}).rowsTo(statelog.AnswerBytes)

type turnPlacesArgs struct {
	Runs  []string
	Fresh statelog.Freshness
}

var opTurnPlaces = define("tracker.turn_places", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a turnPlacesArgs) (map[string]worklet.TurnPlace, error) {
		if b.Tracker == nil {
			return nil, errNoHalf
		}
		return b.Tracker.TurnPlaces(ctx, a.Runs, a.Fresh)
	}).perAsked()
